package pveadmin

// pveadmin_test.go — 探测解析(fixture)、登记钩子流(假 Runner + 真 target.Service)、
// Token 密封/测连/只读闸门(假客户端工厂)、薄客户端(对 httptest 假 PVE)。

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

func testMasterKey() *[32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 17)
	}
	return &k
}

// ---- 假件 ---------------------------------------------------------------------------

// fakeUpload 记录一次流式写入(Upload/ExecWithStdin/Runner.WriteFile 共用断言形态)。
type fakeUpload struct {
	serverID string
	path     string
	content  string
}

// fakeTarget 是 target.Service 桩:servers 内存表 + SetPlacement 落字段;pveversion/
// hostname 经 Exec 按命令路由(探测链 1/2 步)。db 非 nil 时 Create 同步写真 servers
// 行(供 pve_vm_links 等外键断言;凭据须为真实 vault 凭据 ID)。
type fakeTarget struct {
	mu        sync.Mutex
	servers   map[string]*target.Server
	nextID    int
	isPVE     atomic.Bool
	hostname  string
	uploads   []fakeUpload
	stdinCmds []fakeUpload
	db        *sql.DB
}

func newFakeTarget() *fakeTarget { return &fakeTarget{servers: map[string]*target.Server{}} }

func (f *fakeTarget) Get(_ context.Context, id string) (*target.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sv, ok := f.servers[id]; ok {
		cp := *sv
		return &cp, nil
	}
	return nil, target.ErrNotFound
}

func (f *fakeTarget) List(_ context.Context) ([]*target.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*target.Server, 0, len(f.servers))
	for _, sv := range f.servers {
		cp := *sv
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeTarget) Create(_ context.Context, in target.CreateInput) (*target.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("srv-%d", f.nextID)
	sv := &target.Server{ID: id, Name: in.Name, Host: in.Host, Port: in.Port, User: in.User,
		CredentialID: in.CredentialID, Labels: in.Labels, PoolID: in.PoolID, Kind: in.Kind}
	if sv.PoolID == "" {
		sv.PoolID = target.DefaultPoolID
	}
	if sv.Kind == "" {
		sv.Kind = target.KindStandard
	}
	f.servers[id] = sv
	if f.db != nil {
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := f.db.Exec(`INSERT INTO servers (id, name, host, port, user, credential_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, in.Name, in.Host, in.Port, in.User, in.CredentialID, now, now); err != nil {
			return nil, err
		}
	}
	return sv, nil
}

func (f *fakeTarget) SetPlacement(_ context.Context, id, poolID, kind string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sv, ok := f.servers[id]
	if !ok {
		return target.ErrNotFound
	}
	sv.PoolID = poolID
	sv.Kind = kind
	if kind == target.KindPVENode {
		sv.Labels = ""
		sv.MaxBuilds = 0
	}
	return nil
}

// Exec 路由探测链免提权命令:pveversion / hostname -s。
func (f *fakeTarget) Exec(_ context.Context, id string, cmd []string) (*target.ExecResult, error) {
	if _, ok := f.servers[id]; !ok {
		return nil, target.ErrNotFound
	}
	switch cmd[0] {
	case "pveversion":
		if !f.isPVE.Load() {
			return &target.ExecResult{ExitCode: 127, Stderr: "command not found"}, nil
		}
		return &target.ExecResult{ExitCode: 0, Stdout: "pve-manager/8.2.4"}, nil
	case "hostname":
		return &target.ExecResult{ExitCode: 0, Stdout: f.hostname + "\n"}, nil
	case "sudo":
		return &target.ExecResult{ExitCode: 0}, nil
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

// 其余接口方法:探测/管理面用不到。
func (f *fakeTarget) Update(_ context.Context, id string, in target.UpdateInput) (*target.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sv, ok := f.servers[id]
	if !ok {
		return nil, target.ErrNotFound
	}
	if in.Name != nil {
		sv.Name = *in.Name
	}
	if in.Host != nil {
		sv.Host = *in.Host
	}
	if in.Port != nil {
		sv.Port = *in.Port
	}
	if in.User != nil {
		sv.User = *in.User
	}
	if in.CredentialID != nil {
		sv.CredentialID = *in.CredentialID
	}
	if in.Labels != nil {
		sv.Labels = *in.Labels
	}
	cp := *sv
	return &cp, nil
}
func (f *fakeTarget) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.servers, id)
	return nil
}
func (f *fakeTarget) Test(context.Context, string) (*target.TestResult, error) { return nil, nil }
func (f *fakeTarget) ExecStream(context.Context, string, []string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeTarget) ExecInteractive(context.Context, string, []string) (target.Session, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeTarget) Upload(_ context.Context, id string, r io.Reader, remotePath string) error {
	b, _ := io.ReadAll(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, fakeUpload{serverID: id, path: remotePath, content: string(b)})
	return nil
}
func (f *fakeTarget) Download(_ context.Context, _ string, _ string, _ io.Writer) (int64, error) {
	return 0, nil
}
func (f *fakeTarget) ExecWithStdin(ctx context.Context, id string, cmd []string, r io.Reader) (*target.ExecResult, error) {
	var body string
	if r != nil {
		b, _ := io.ReadAll(r)
		body = string(b)
	}
	f.mu.Lock()
	f.stdinCmds = append(f.stdinCmds, fakeUpload{serverID: id, path: strings.Join(cmd, " "), content: body})
	f.mu.Unlock()
	return f.Exec(ctx, id, cmd)
}
func (f *fakeTarget) DockerLogin(context.Context, string, string, string) error { return nil }
func (f *fakeTarget) ListPools(context.Context) ([]*target.ServerPool, error)   { return nil, nil }

// Dial / DialDirect 满足 target.Service 接口(0099 SSH 隧道追加);测试桩不建连。
func (f *fakeTarget) Dial(context.Context, string) (target.SSHClient, error) {
	return nil, nil
}
func (f *fakeTarget) DialDirect(context.Context, string, int, string, string) (target.SSHClient, error) {
	return nil, nil
}

// fakeRunner 是探测链第 3/4 步(cat corosync.conf / pvecm nodes)的假件:按 cmd 路由。
// WriteFile 记录写入(路径/权限/内容);importOut 供 qm importdisk SSH 兜底断言。
type fakeRunner struct {
	mu        sync.Mutex
	confOut   string
	confErr   error
	nodesOut  string
	nodesErr  error
	importOut string
	importErr error
	runs      [][]string
	writes    []fakeWrite
	writeErr  error
	// runFn 通用路由钩子(升级面测试按 serverID+cmd 返回 fixture;优先于固定路由)。
	runFn func(serverID string, cmd []string) (*target.ExecResult, error)
}

// fakeWrite 记录一次 Runner.WriteFile 调用。
type fakeWrite struct {
	serverID string
	path     string
	mode     os.FileMode
	content  string
}

func (r *fakeRunner) Run(_ context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	r.mu.Lock()
	r.runs = append(r.runs, append([]string(nil), cmd...))
	fn := r.runFn
	r.mu.Unlock()
	if fn != nil {
		return fn(serverID, cmd)
	}
	joined := strings.Join(cmd, " ")
	switch {
	case strings.Contains(joined, "corosync.conf"):
		if r.confErr != nil {
			return nil, r.confErr
		}
		return &target.ExecResult{ExitCode: 0, Stdout: r.confOut}, nil
	case strings.HasPrefix(joined, "pvecm"):
		if r.nodesErr != nil {
			return nil, r.nodesErr
		}
		return &target.ExecResult{ExitCode: 0, Stdout: r.nodesOut}, nil
	case strings.Contains(joined, "importdisk"):
		if r.importErr != nil {
			return nil, r.importErr
		}
		return &target.ExecResult{ExitCode: 0, Stdout: r.importOut}, nil
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

func (r *fakeRunner) WriteFile(_ context.Context, serverID, remotePath string, mode os.FileMode, content io.Reader) error {
	b, _ := io.ReadAll(content)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, fakeWrite{serverID: serverID, path: remotePath, mode: mode, content: string(b)})
	return r.writeErr
}

// fakePVEClient 是假 API 客户端:按 path 前缀回 canned 响应,留痕调用与表单。
type fakePVEClient struct {
	mu        sync.Mutex
	gets      []string
	posts     []string
	postForms []url.Values
	puts      []string
	putForms  []url.Values
	uploads   []string
	getFn     func(path string) (any, error)
	postFn    func(path string) (any, error)
	putFn     func(path string, form url.Values) (any, error)
	uploadFn  func(path string, fields url.Values, fileName string) (any, error)
}

func (c *fakePVEClient) Get(_ context.Context, path string, out any) error {
	c.mu.Lock()
	c.gets = append(c.gets, path)
	fn := c.getFn
	c.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("%w: unexpected GET %s", ErrConnectFailed, path)
	}
	v, err := fn(path)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(v)
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (c *fakePVEClient) Post(_ context.Context, path string, form url.Values, out any) error {
	c.mu.Lock()
	c.posts = append(c.posts, path)
	c.postForms = append(c.postForms, form)
	fn := c.postFn
	c.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("%w: unexpected POST %s", ErrConnectFailed, path)
	}
	v, err := fn(path)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(v)
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (c *fakePVEClient) Put(_ context.Context, path string, form url.Values, out any) error {
	c.mu.Lock()
	c.puts = append(c.puts, path)
	c.putForms = append(c.putForms, form)
	fn := c.putFn
	c.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("%w: unexpected PUT %s", ErrConnectFailed, path)
	}
	v, err := fn(path, form)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(v)
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (c *fakePVEClient) Upload(_ context.Context, path string, fields url.Values, _, fileName string, content io.Reader, out any) error {
	_, _ = io.ReadAll(content) // 流式读尽(假件不存大字节)
	c.mu.Lock()
	c.uploads = append(c.uploads, path)
	fn := c.uploadFn
	c.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("%w: unexpected UPLOAD %s", ErrConnectFailed, path)
	}
	v, err := fn(path, fields, fileName)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(v)
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// newTestService 组装被测服务(真实 DB + 假件注入)。
func newTestService(t *testing.T) (*service, *fakeTarget, *fakeRunner, *fakePVEClient) {
	t.Helper()
	// 大版本 dist-upgrade 通过远端完成标记轮询;测试把间隔压到毫秒级。
	prevPoll := majorUpgradePollInterval
	majorUpgradePollInterval = 5 * time.Millisecond
	t.Cleanup(func() { majorUpgradePollInterval = prevPoll })
	db := storetest.OpenDB(t)
	v := vault.New(db, testMasterKey())
	ft := newFakeTarget()
	fr := &fakeRunner{}
	fc := &fakePVEClient{}
	s := &service{db: db, vault: v, servers: ft, runner: fr, clientF: func(_, _, _, _, _ string) (pveClient, error) { return fc, nil }}
	return s, ft, fr, fc
}

const corosyncFixture = `totem {
    version: 2
    cluster_name: pve-prod
    token: 3000
}
nodelist {
    node {
        ring0_addr: 10.1.0.11
        name: pve1
        nodeid: 1
        quorum_votes: 1
    }
    node {
        ring0_addr: 10.1.0.12
        name: pve2
        nodeid: 2
        quorum_votes: 1
    }
}
qdevice { }
`

const pvecmFixture = `- Cluster information -------------------
  Name:             pve-prod
  Config Version:   7
  Transport:        knet

- Membership information -----------------
    Nodeid      Votes    Qdevice Name
0x00000001          1          A 10.1.0.11 (local)
0x00000002          1          A 10.1.0.12
`

// ---- 探测解析 -----------------------------------------------------------------------

func TestParseCorosyncConf(t *testing.T) {
	info := parseCorosyncConf(corosyncFixture)
	if info == nil {
		t.Fatal("parse returned nil")
	}
	if info.ClusterName != "pve-prod" {
		t.Fatalf("clusterName = %q", info.ClusterName)
	}
	if len(info.Nodes) != 2 || info.Nodes[0].Name != "pve1" || info.Nodes[0].Addr != "10.1.0.11" ||
		info.Nodes[1].Name != "pve2" || info.Nodes[1].Addr != "10.1.0.12" {
		t.Fatalf("nodes = %+v", info.Nodes)
	}
	if parseCorosyncConf("totem { version: 2 }") != nil {
		t.Fatal("无 nodelist 应返回 nil")
	}
}

func TestParsePvecmNodes(t *testing.T) {
	info := parsePvecmNodes(pvecmFixture)
	if info == nil {
		t.Fatal("parse returned nil")
	}
	if info.ClusterName != "pve-prod" {
		t.Fatalf("clusterName = %q", info.ClusterName)
	}
	if len(info.Nodes) != 2 || info.Nodes[0].Addr != "10.1.0.11" || info.Nodes[1].Addr != "10.1.0.12" {
		t.Fatalf("nodes = %+v(地址列,含 (local) 剥离)", info.Nodes)
	}
}

// ---- 登记钩子 -----------------------------------------------------------------------

// TestOnServerRegisteredCluster 集群成员:建集群+池,种子归池,既有兄弟归池(清构建池字段),
// 缺失兄弟建占位;二次登记同集群幂等(不重复建集群/池/占位)。
func TestOnServerRegisteredCluster(t *testing.T) {
	s, ft, fr, _ := newTestService(t)
	ctx := context.Background()

	ft.isPVE.Store(true)
	ft.hostname = "pve1"
	fr.confOut = corosyncFixture
	seed := &target.Server{ID: "seed", Name: "pve1", Host: "10.1.0.11", Port: 22, User: "root", CredentialID: "c1"}
	ft.servers["seed"] = seed
	// 兄弟 pve2 已按普通服务器登记过(带构建池标签)。
	existing := &target.Server{ID: "exist", Name: "db-legacy", Host: "10.1.0.12", Port: 22, User: "root", CredentialID: "c2", Labels: "linux", MaxBuilds: 4}
	ft.servers["exist"] = existing

	res, err := s.OnServerRegistered(ctx, "seed")
	if err != nil || res == nil {
		t.Fatalf("OnServerRegistered = %+v / %v", res, err)
	}
	if res.ClusterName != "pve-prod" || res.Standalone {
		t.Fatalf("res = %+v", res)
	}
	if got, err := s.clusterByName(ctx, "pve-prod"); err != nil || got.State != StateCreated {
		t.Fatalf("cluster row: %v / %+v", err, got)
	}
	// 种子归池。
	if sv, _ := ft.Get(ctx, "seed"); !sv.IsPVENode() || sv.PoolID != res.PoolID {
		t.Fatalf("seed placement: %+v", sv)
	}
	// 既有兄弟归池且构建池字段被清。
	if sv, _ := ft.Get(ctx, "exist"); !sv.IsPVENode() || sv.Labels != "" || sv.MaxBuilds != 0 {
		t.Fatalf("existing sibling placement: %+v", sv)
	}
	if res.AttachedCount != 1 {
		t.Fatalf("attached = %d, want 1", res.AttachedCount)
	}

	// 幂等:另一台节点(pve2 自身)再登记 → 复用集群/池,不再新建占位。
	ft.hostname = "pve2"
	seed2 := &target.Server{ID: "seed2", Name: "pve2", Host: "10.1.0.12", Port: 22, User: "root", CredentialID: "c1"}
	ft.servers["seed2"] = seed2
	res2, err := s.OnServerRegistered(ctx, "seed2")
	if err != nil || res2 == nil {
		t.Fatalf("second OnServerRegistered = %+v / %v", res2, err)
	}
	if res2.PoolID != res.PoolID {
		t.Fatalf("second discovery should reuse pool: %q vs %q", res2.PoolID, res.PoolID)
	}
	clusters, _ := s.ListClusters(ctx)
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want 1(幂等)", len(clusters))
	}
}

// TestOnServerRegisteredStandalone 单机 PVE(无 corosync.conf):以主机名建单节点池。
func TestOnServerRegisteredStandalone(t *testing.T) {
	s, ft, fr, _ := newTestService(t)
	ctx := context.Background()
	ft.isPVE.Store(true)
	ft.hostname = "solo-pve"
	ft.servers["solo"] = &target.Server{ID: "solo", Name: "solo", Host: "192.168.1.50", Port: 22, User: "root", CredentialID: "c1"}
	fr.confErr = fmt.Errorf("%w: cat 退出码 1\nstderr: cat: /etc/pve/corosync.conf: No such file or directory", ErrProbe)

	res, err := s.OnServerRegistered(ctx, "solo")
	if err != nil || res == nil {
		t.Fatalf("res = %+v / %v", res, err)
	}
	if !res.Standalone || res.ClusterName != "solo-pve" {
		t.Fatalf("res = %+v", res)
	}
	if len(res.CreatedSiblings) != 0 {
		t.Fatalf("单机不应建兄弟占位: %+v", res.CreatedSiblings)
	}
}

// TestOnServerRegisteredNonPVE 非 PVE:探测不动任何库表。
func TestOnServerRegisteredNonPVE(t *testing.T) {
	s, ft, _, _ := newTestService(t)
	ft.isPVE.Store(false)
	ft.hostname = "web-1"
	ft.servers["web"] = &target.Server{ID: "web", Name: "web-1", Host: "10.2.0.5", Port: 22, User: "deploy", CredentialID: "c1"}

	res, err := s.OnServerRegistered(context.Background(), "web")
	if err != nil || res != nil {
		t.Fatalf("res = %+v / %v, want nil/nil", res, err)
	}
	clusters, _ := s.ListClusters(context.Background())
	if len(clusters) != 0 {
		t.Fatalf("clusters = %d, want 0", len(clusters))
	}
}

// ---- Token / 管理面 -----------------------------------------------------------------

// seedReadyToken 直插 ready 状态的 Token(绕过 SetToken,便于只测某个 API 面)。
func seedReadyToken(t *testing.T, s *service, clusterID string) {
	t.Helper()
	sealed, err := s.vault.SealSecret([]byte("sec"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE pve_clusters SET api_url = ?, token_name = ?, token_sealed = ?, state = ? WHERE id = ?`,
		"https://10.1.0.11:8006", "root@pam!pw", sealed, StateReady, clusterID); err != nil {
		t.Fatalf("seed token: %v", err)
	}
}

func seedCluster(t *testing.T, s *service) *Cluster {
	t.Helper()
	cl, err := s.ensureCluster(context.Background(), "pve-prod")
	if err != nil {
		t.Fatalf("ensureCluster: %v", err)
	}
	return cl
}

// TestSetTokenSealsAndVerifies Token 密封落库(明文绝不入库)、测连成功置 ready、入参校验。
func TestSetTokenSealsAndVerifies(t *testing.T) {
	s, _, _, fc := newTestService(t)
	ctx := context.Background()
	cl := seedCluster(t, s)

	fc.getFn = func(path string) (any, error) {
		if strings.HasSuffix(path, "/version") {
			return apiVersion{Release: "8.2.4", Version: "8.2.4.1"}, nil
		}
		return nil, fmt.Errorf("unexpected %s", path)
	}

	if err := s.SetToken(ctx, cl.ID, "https://10.1.0.11:8006", "root@pam!pipewright", "secret-uuid-1", TLSModeInsecure, ""); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	row, _ := s.clusterByID(ctx, cl.ID)
	if row.State != StateReady {
		t.Fatalf("state = %q, want ready", row.State)
	}
	// 密文入库:sealed BLOB 非空且不含明文。
	var sealed []byte
	_ = s.db.QueryRow(`SELECT token_sealed FROM pve_clusters WHERE id = ?`, cl.ID).Scan(&sealed)
	if len(sealed) == 0 || strings.Contains(string(sealed), "secret-uuid-1") {
		t.Fatalf("token sealing broken: len=%d", len(sealed))
	}

	// 入参校验。
	for name, in := range map[string][5]string{
		"http 地址":  {"http://10.1.0.11:8006", "root@pam!pw", "s", TLSModeInsecure, ""},
		"token 名":  {"https://h:8006", "bad-name", "s", TLSModeInsecure, ""},
		"空 secret": {"https://h:8006", "root@pam!pw", " ", TLSModeInsecure, ""},
		"指纹缺失":     {"https://h:8006", "root@pam!pw", "s", TLSModeFingerprint, ""},
		"未知模式":     {"https://h:8006", "root@pam!pw", "s", "loose", ""},
	} {
		if err := s.SetToken(ctx, cl.ID, in[0], in[1], in[2], in[3], in[4]); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
}

// TestSetTokenKeepsCreatedWhenTestFails 测连失败:Token 已保存但 state 保持 created。
func TestSetTokenKeepsCreatedWhenTestFails(t *testing.T) {
	s, _, _, fc := newTestService(t)
	ctx := context.Background()
	cl := seedCluster(t, s)
	fc.getFn = func(path string) (any, error) {
		return nil, fmt.Errorf("%w: connection refused", ErrConnectFailed)
	}
	err := s.SetToken(ctx, cl.ID, "https://10.1.0.11:8006", "root@pam!pw", "sec", TLSModeInsecure, "")
	if !errors.Is(err, ErrConnectFailed) {
		t.Fatalf("err = %v, want ErrConnectFailed", err)
	}
	row, _ := s.clusterByID(ctx, cl.ID)
	if row.State != StateCreated || len(row.TokenSealed) == 0 {
		t.Fatalf("token should be saved with state=created: %+v", row)
	}
}

// TestManagementRequiresToken 未配 Token → 管理面 ErrClusterNotReady。
func TestManagementRequiresToken(t *testing.T) {
	s, _, _, _ := newTestService(t)
	cl := seedCluster(t, s)
	ctx := context.Background()
	if _, err := s.Overview(ctx, cl.ID); !errors.Is(err, ErrClusterNotReady) {
		t.Fatalf("Overview err = %v", err)
	}
	if _, err := s.VMAction(ctx, cl.ID, "pve1", "qemu", 101, "start"); !errors.Is(err, ErrClusterNotReady) {
		t.Fatalf("VMAction err = %v", err)
	}
}

// TestVMActionReadonlyGate 只读闸门 + 动作白名单 + UPID 返回。
func TestVMActionReadonlyGate(t *testing.T) {
	s, _, _, fc := newTestService(t)
	ctx := context.Background()
	cl := seedCluster(t, s)
	// 直接落 ready 状态的 Token(readonly 保持默认 1)。
	seedReadyToken(t, s, cl.ID)

	// readonly → 拒绝。
	if _, err := s.VMAction(ctx, cl.ID, "pve1", "qemu", 101, "start"); !errors.Is(err, ErrReadonly) {
		t.Fatalf("readonly err = %v", err)
	}
	// 解锁后成功。
	if err := s.SetReadonly(ctx, cl.ID, false); err != nil {
		t.Fatalf("SetReadonly: %v", err)
	}
	fc.postFn = func(path string) (any, error) {
		if !strings.HasSuffix(path, "/status/start") {
			return nil, fmt.Errorf("unexpected %s", path)
		}
		return "UPID:pve1:0000ABC:1234:qmstart:101:", nil
	}
	ref, err := s.VMAction(ctx, cl.ID, "pve1", "qemu", 101, "start")
	if err != nil || ref.UPID == "" {
		t.Fatalf("VMAction = %+v / %v", ref, err)
	}
	// 白名单外动作/类型。
	if _, err := s.VMAction(ctx, cl.ID, "pve1", "qemu", 101, "destroy"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("destroy err = %v", err)
	}
	if _, err := s.VMAction(ctx, cl.ID, "pve1", "vzctl", 101, "start"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("kind err = %v", err)
	}
	// lxc 没有 reset 子端点(PVE 文档 reset 为 qemu 独有)→ 输入层拒绝,避免 501。
	if _, err := s.VMAction(ctx, cl.ID, "pve1", "lxc", 201, "reset"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("lxc reset err = %v, want ErrInvalidInput", err)
	}
	// qemu 的 reset 合法。
	fc.postFn = func(path string) (any, error) {
		if !strings.HasSuffix(path, "/status/reset") {
			return nil, fmt.Errorf("unexpected %s", path)
		}
		return "UPID:pve1:00003A2D:009BEB5C:67416A2F:qmreset:101:root@pam:", nil
	}
	ref, err = s.VMAction(ctx, cl.ID, "pve1", "qemu", 101, "reset")
	if err != nil || ref.UPID == "" {
		t.Fatalf("qemu reset = %+v / %v", ref, err)
	}
}

// TestListVMsParsesQuirks PVE 字符串数值怪癖在 client 层收口。
func TestListVMsParsesQuirks(t *testing.T) {
	s, _, _, fc := newTestService(t)
	ctx := context.Background()
	cl := seedCluster(t, s)
	seedReadyToken(t, s, cl.ID)
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/cluster/resources") {
			// 数值全为字符串、布尔 0/1 —— PVE 实际形态。
			return []map[string]any{
				{"id": "qemu/101", "type": "qemu", "node": "pve1", "name": "web-1", "status": "running",
					"vmid": "101", "cpu": "0.1234", "maxcpu": "8", "mem": "1073741824", "maxmem": "4294967296", "uptime": "3600"},
				{"id": "lxc/201", "type": "lxc", "node": "pve2", "name": "ct-1", "status": "stopped",
					"vmid": "201", "cpu": 0, "maxcpu": "2", "mem": "0", "maxmem": "536870912", "uptime": "0"},
				{"id": "node/pve1", "type": "node", "node": "pve1", "status": "online", "maxcpu": "32", "maxmem": "68719476736", "mem": "12345678901"},
			}, nil
		}
		return nil, fmt.Errorf("unexpected %s", path)
	}
	vms, err := s.ListVMs(ctx, cl.ID)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 2 {
		t.Fatalf("vms = %d(node 条目应被过滤), %+v", len(vms), vms)
	}
	if vms[0].VMID != 101 || vms[0].MaxCPU != 8 || vms[0].CPU != 0.1234 || vms[0].MaxMem != 4294967296 {
		t.Fatalf("quirk parse: %+v", vms[0])
	}
	if vms[1].Type != "lxc" || vms[1].Status != "stopped" {
		t.Fatalf("lxc parse: %+v", vms[1])
	}
}

// TestOverviewAggregatesResources Overview 三源聚合。storage 条目用量/总量在
// disk/maxdisk(storage 字段是存储名字符串,不存在 maxstorage —— 按文档对齐)。
func TestOverviewAggregatesResources(t *testing.T) {
	s, _, _, fc := newTestService(t)
	ctx := context.Background()
	cl := seedCluster(t, s)
	seedReadyToken(t, s, cl.ID)
	fc.getFn = func(path string) (any, error) {
		switch {
		case strings.HasSuffix(path, "/version"):
			return apiVersion{Release: "8.2.4", Version: "8.2.4"}, nil
		case strings.HasSuffix(path, "/cluster/status"):
			// 真实形态:混合扁平数组 —— cluster 条目 + node 条目(Cluster.pm get_status)。
			return []map[string]any{
				{"type": "cluster", "id": "cluster", "name": "pve-prod",
					"nodes": "1", "version": "2", "quorate": 1},
				{"type": "node", "id": "node/pve1", "name": "pve1", "ip": "10.1.0.11",
					"nodeid": "1", "local": 1, "online": 1, "level": ""},
			}, nil
		case strings.HasSuffix(path, "/cluster/resources"):
			// 数值为字符串 —— PVE 实际形态;storage 条目 storage 是名字、用量在 disk/maxdisk。
			return []map[string]any{
				{"id": "node/pve1", "type": "node", "node": "pve1", "status": "online",
					"cpu": "0.5", "maxcpu": "8", "mem": "2", "maxmem": "10"},
				{"id": "storage/pve1/local", "type": "storage", "storage": "local", "node": "pve1",
					"disk": "300", "maxdisk": "1000", "plugintype": "dir", "status": "available"},
			}, nil
		}
		return nil, fmt.Errorf("unexpected %s", path)
	}
	ov, err := s.Overview(ctx, cl.ID)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if ov.Version != "8.2.4" || !ov.Quorate || len(ov.Nodes) != 1 || !ov.Nodes[0].Online {
		t.Fatalf("overview base = %+v", ov)
	}
	if ov.Resources.CPUTotal != 8 || ov.Resources.CPUUsed != 4 { // 0.5 * 8
		t.Fatalf("cpu = %+v", ov.Resources)
	}
	if ov.Resources.MemUsed != 2 || ov.Resources.MemTotal != 10 {
		t.Fatalf("mem = %+v", ov.Resources)
	}
	if ov.Resources.StorageUsed != 300 || ov.Resources.StorageTotal != 1000 {
		t.Fatalf("storage = %+v", ov.Resources)
	}
}

// TestOverviewStandalone 单机形态:/cluster/status 只有 node 条目、无 quorate,
// 视作有仲裁且节点在线。
func TestOverviewStandalone(t *testing.T) {
	s, _, _, fc := newTestService(t)
	ctx := context.Background()
	cl := seedCluster(t, s)
	seedReadyToken(t, s, cl.ID)
	fc.getFn = func(path string) (any, error) {
		switch {
		case strings.HasSuffix(path, "/version"):
			return apiVersion{Release: "9.0.0", Version: "9.0.0"}, nil
		case strings.HasSuffix(path, "/cluster/status"):
			return []map[string]any{
				{"type": "node", "id": "node/pve9", "name": "pve9", "ip": "10.1.0.9",
					"local": 1, "nodeid": "0", "online": 1, "level": ""},
			}, nil
		case strings.HasSuffix(path, "/cluster/resources"):
			return []map[string]any{}, nil
		}
		return nil, fmt.Errorf("unexpected %s", path)
	}
	ov, err := s.Overview(ctx, cl.ID)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if !ov.Quorate || len(ov.Nodes) != 1 || !ov.Nodes[0].Online || ov.Nodes[0].Name != "pve9" {
		t.Fatalf("standalone overview = %+v", ov)
	}
}

// TestTaskStatusAcceptsRealUPID 真实 UPID 的 user 段含 @ 与 !(root@pam、令牌
// 发起任务 root@pam!ci,见 PVE UPID.pm 原样拼装),不得被 upidRe 误拒。
func TestTaskStatusAcceptsRealUPID(t *testing.T) {
	s, _, _, fc := newTestService(t)
	ctx := context.Background()
	cl := seedCluster(t, s)
	seedReadyToken(t, s, cl.ID)
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/tasks/") {
			return map[string]any{"status": "stopped", "exitstatus": "OK"}, nil
		}
		return nil, fmt.Errorf("unexpected %s", path)
	}
	for _, upid := range []string{
		"UPID:pve1:00003A2D:009BEB5C:67416A2F:qmstart:101:root@pam:",
		"UPID:pve1:00003A2D:009BEB5C:67416A2F:vzstart:201:root@pam!ci:",
	} {
		ts, err := s.TaskStatus(ctx, cl.ID, "pve1", upid)
		if err != nil {
			t.Fatalf("TaskStatus %s: %v", upid, err)
		}
		if !ts.Done || !ts.OK {
			t.Fatalf("TaskStatus %s = %+v", upid, ts)
		}
	}
	// 路径注入形态仍须拒绝。
	for _, upid := range []string{"UPID:pve1:1:2:3:4:5:../etc", "UPID:../x:1:2:3:4:5:"} {
		if _, err := s.TaskStatus(ctx, cl.ID, "pve1", upid); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("injection %s err = %v, want ErrInvalidInput", upid, err)
		}
	}
}

// ---- 薄客户端(对 httptest 假 PVE) ----------------------------------------------------

func TestLiveClientAgainstFakePVE(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken=root@pam!pw=sec" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/version"):
			_, _ = w.Write([]byte(`{"data":{"release":"8.2.4"}}`))
		case strings.HasSuffix(r.URL.Path, "/status/start"):
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:00AB:1:qmstart:101:"}`))
		case strings.Contains(r.URL.Path, "/tasks/"):
			_, _ = w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
		default:
			http.Error(w, `{"data":{"message":"not found"}}`, http.StatusNotFound)
		}
	}))
	defer ts.Close()

	c, err := prodClientFactory(ts.URL, "root@pam!pw", "sec", TLSModeInsecure, "")
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ctx := context.Background()
	var ver apiVersion
	if err := c.Get(ctx, "/api2/json/version", &ver); err != nil || ver.Release != "8.2.4" {
		t.Fatalf("Get version = %+v / %v", ver, err)
	}
	var upid string
	if err := c.Post(ctx, "/api2/json/nodes/pve1/qemu/101/status/start", nil, &upid); err != nil || !strings.HasPrefix(upid, "UPID:") {
		t.Fatalf("Post = %q / %v", upid, err)
	}
	// 非 2xx → ErrConnectFailed,错误体含 PVE message、不含 Token。
	err = c.Get(ctx, "/api2/json/nope", nil)
	if err == nil || !errors.Is(err, ErrConnectFailed) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error mapping = %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "sec") {
		t.Fatalf("错误体泄漏 Token: %v", err)
	}
}

// TestLiveClientGetPreservesQuery 查询串必须进入 RawQuery,不能被编码进 URL.Path。
func TestLiveClientGetPreservesQuery(t *testing.T) {
	var gotURI, gotContent string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		gotContent = r.URL.Query().Get("content")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()
	c, err := prodClientFactory(ts.URL, "root@pam!pw", "sec", TLSModeInsecure, "")
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	var entries []apiContentEntry
	if err := c.Get(context.Background(), "/api2/json/nodes/pve1/storage/local/content?content=import", &entries); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotContent != "import" || gotURI != "/api2/json/nodes/pve1/storage/local/content?content=import" {
		t.Fatalf("uri=%q content=%q", gotURI, gotContent)
	}
}

// TestLiveClientPutAndUpload PUT form 形态与 Upload multipart 流式形态(对 httptest 假 PVE)。
func TestLiveClientPutAndUpload(t *testing.T) {
	var gotPutBody, gotUploadCT, gotUploadFile, gotUploadField string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken=root@pam!pw=sec" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			gotPutBody = string(b)
			if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
				http.Error(w, "bad put content-type", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:00AB:1:qmresize:101:"}`))
		case strings.HasSuffix(r.URL.Path, "/upload"):
			gotUploadCT = r.Header.Get("Content-Type")
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			gotUploadField = r.FormValue("content")
			f, hdr, err := r.FormFile("filename")
			if err != nil {
				http.Error(w, "missing file", http.StatusBadRequest)
				return
			}
			defer func() { _ = f.Close() }()
			b, _ := io.ReadAll(f)
			gotUploadFile = string(b) + "|" + hdr.Filename
			_, _ = w.Write([]byte(`{"data":"local:backup/test.vma"}`))
		default:
			http.Error(w, `{"data":{"message":"not found"}}`, http.StatusNotFound)
		}
	}))
	defer ts.Close()

	c, err := prodClientFactory(ts.URL, "root@pam!pw", "sec", TLSModeInsecure, "")
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ctx := context.Background()
	var upid string
	if err := c.Put(ctx, "/api2/json/nodes/pve1/qemu/101/resize", url.Values{"disk": {"scsi0"}, "size": {"+10G"}}, &upid); err != nil || !strings.HasPrefix(upid, "UPID:") {
		t.Fatalf("Put = %q / %v", upid, err)
	}
	if gotPutBody != "disk=scsi0&size=%2B10G" {
		t.Fatalf("put body = %q", gotPutBody)
	}
	var volid string
	err = c.Upload(ctx, "/api2/json/nodes/pve1/storage/local/upload", url.Values{"content": {"backup"}}, "filename", "test.vma", strings.NewReader("VMA-BYTES"), &volid)
	if err != nil || volid != "local:backup/test.vma" {
		t.Fatalf("Upload = %q / %v", volid, err)
	}
	if !strings.HasPrefix(gotUploadCT, "multipart/form-data; boundary=") {
		t.Fatalf("upload content-type = %q", gotUploadCT)
	}
	if gotUploadField != "backup" || gotUploadFile != "VMA-BYTES|test.vma" {
		t.Fatalf("upload field=%q file=%q", gotUploadField, gotUploadFile)
	}
}

type gatedUploadReader struct {
	started chan struct{}
	release chan struct{}
	calls   int
}

func (r *gatedUploadReader) Read(p []byte) (int, error) {
	if r.calls == 0 {
		r.calls++
		close(r.started)
		return copy(p, "x"), nil
	}
	<-r.release
	return 0, io.EOF
}

// TestLiveClientUploadWaitsProducer PVE 提前响应后,Upload 仍须等 content 读取者退出。
func TestLiveClientUploadWaitsProducer(t *testing.T) {
	responded := make(chan struct{})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		br := bufio.NewReader(r.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Errorf("read multipart header: %v", err)
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			if strings.Contains(line, `filename="gated.vma"`) {
				break
			}
		}
		_, _ = w.Write([]byte(`{"data":"local:backup/gated.vma"}`))
		close(responded)
	}))
	defer ts.Close()
	c, err := prodClientFactory(ts.URL, "root@pam!pw", "sec", TLSModeInsecure, "")
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	content := &gatedUploadReader{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	var volid string
	go func() {
		done <- c.Upload(context.Background(), "/api2/json/nodes/pve1/storage/local/content", url.Values{"content": {"backup"}}, "filename", "gated.vma", content, &volid)
	}()
	for name, ch := range map[string]chan struct{}{"content started": content.started, "early response": responded} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("等待 %s 超时", name)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("producer 未退出 Upload 已返回: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(content.release)
	select {
	case err := <-done:
		if err != nil || volid != "local:backup/gated.vma" {
			t.Fatalf("Upload = %q / %v", volid, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("释放 content 后 Upload 未返回")
	}
}

// TestTLSFingerprintPin 指纹模式:对错指纹拒绝、对证书真指纹放行(冒号/大写归一)。
func TestTLSFingerprintPin(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"release":"8.2.4"}}`))
	}))
	defer ts.Close()

	cert := ts.Certificate()
	sum := sha256.Sum256(cert.Raw)
	trueFP := strings.ToUpper(strings.ToLower(hex.EncodeToString(sum[:])))
	// 带冒号大写形态(与 PVE 界面展示一致)。
	colon := make([]string, 32)
	for i := 0; i < 32; i++ {
		colon[i] = strings.ToUpper(hex.EncodeToString(sum[i : i+1]))
	}
	colonFP := strings.Join(colon, ":")

	ctx := context.Background()
	// 错指纹 → 拒绝。
	bad, err := prodClientFactory(ts.URL, "root@pam!pw", "sec", TLSModeFingerprint, strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if gerr := bad.Get(ctx, "/api2/json/version", nil); !errors.Is(gerr, ErrConnectFailed) {
		t.Fatalf("wrong fingerprint should fail: %v", gerr)
	}
	// 真指纹(hex 与冒号两种形态)→ 放行。
	for name, fp := range map[string]string{"hex": trueFP, "colon": colonFP} {
		c, err := prodClientFactory(ts.URL, "root@pam!pw", "sec", TLSModeFingerprint, fp)
		if err != nil {
			t.Fatalf("%s factory: %v", name, err)
		}
		var ver apiVersion
		if err := c.Get(ctx, "/api2/json/version", &ver); err != nil || ver.Release != "8.2.4" {
			t.Fatalf("%s get = %+v / %v", name, ver, err)
		}
	}
}
