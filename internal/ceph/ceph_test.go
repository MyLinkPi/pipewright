// ceph_test.go — 域层单测:fake target.Service + fake Runner(不触网)。
// 覆盖:创建校验 / 部署管线命令拼装与状态机 / 管理操作命令拼装 / JSON 解析器。
package ceph

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// fakeTarget 是 target.Service 的测试桩(仅 Get/List/Exec 有语义)。
type fakeTarget struct {
	mu      sync.Mutex
	servers map[string]*target.Server
	calls   [][]string
}

func newFakeTarget() *fakeTarget {
	return &fakeTarget{servers: map[string]*target.Server{}}
}

func (f *fakeTarget) add(id, name, host string) {
	f.servers[id] = &target.Server{ID: id, Name: name, Host: host, User: "root", Port: 22}
}

func (f *fakeTarget) Get(_ context.Context, id string) (*target.Server, error) {
	if sv, ok := f.servers[id]; ok {
		return sv, nil
	}
	return nil, target.ErrNotFound
}
func (f *fakeTarget) List(_ context.Context) ([]*target.Server, error) {
	out := make([]*target.Server, 0, len(f.servers))
	for _, sv := range f.servers {
		out = append(out, sv)
	}
	return out, nil
}
func (f *fakeTarget) Create(context.Context, target.CreateInput) (*target.Server, error) {
	return nil, nil
}
func (f *fakeTarget) Update(context.Context, string, target.UpdateInput) (*target.Server, error) {
	return nil, nil
}
func (f *fakeTarget) Delete(context.Context, string) error                     { return nil }
func (f *fakeTarget) Test(context.Context, string) (*target.TestResult, error) { return nil, nil }
func (f *fakeTarget) Exec(_ context.Context, id string, cmd []string) (*target.ExecResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()
	if _, ok := f.servers[id]; !ok {
		return nil, target.ErrNotFound
	}
	return &target.ExecResult{ExitCode: 0}, nil
}
func (f *fakeTarget) ExecStream(context.Context, string, []string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *fakeTarget) ExecInteractive(context.Context, string, []string) (target.Session, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *fakeTarget) Upload(context.Context, string, io.Reader, string) error {
	return nil
}
func (f *fakeTarget) ExecWithStdin(_ context.Context, id string, cmd []string, _ io.Reader) (*target.ExecResult, error) {
	return f.Exec(context.Background(), id, cmd)
}
func (f *fakeTarget) DockerLogin(context.Context, string, string, string) error { return nil }

// SetPlacement / ListPools 满足 target.Service 接口(0084 服务器池追加);测试桩不落库。
func (f *fakeTarget) SetPlacement(context.Context, string, string, string) error { return nil }
func (f *fakeTarget) ListPools(context.Context) ([]*target.ServerPool, error)         { return nil, nil }

// fakeRunner 是 Runner 的测试桩:handle 按 cmd 前缀路由输出;全部命令留痕。
type fakeRunner struct {
	mu     sync.Mutex
	calls  [][]string
	handle func(cmd []string) (string, int)
}

func (f *fakeRunner) Run(_ context.Context, _ string, cmd []string) (*target.ExecResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()
	out, code := f.handle(cmd)
	if code != 0 {
		return &target.ExecResult{ExitCode: code, Stderr: out}, fmt.Errorf("%w: %s 退出码 %d\n%s", ErrExec, strings.Join(cmd, " "), code, out)
	}
	return &target.ExecResult{ExitCode: 0, Stdout: out}, nil
}

func (f *fakeRunner) saw(pred func(cmd []string) bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if pred(c) {
			return true
		}
	}
	return false
}

func newTestService(t *testing.T, st *store.Store, ft *fakeTarget, fr *fakeRunner) Service {
	t.Helper()
	return New(st.DB, ft, nil, WithRunner(fr))
}

// ── 创建校验 ─────────────────────────────────────────────────────────────────

func TestCreateClusterValidation(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ft := newFakeTarget()
		ft.add("srv1", "node1", "10.0.0.11")
		ft.add("srv2", "node2", "10.0.0.12")
		svc := newTestService(t, st, ft, &fakeRunner{handle: func([]string) (string, int) { return "", 0 }})
		ctx := context.Background()

		bad := []CreateInput{
			{Name: "bad name!", BootstrapServerID: "srv1", Hosts: []HostSpec{{ServerID: "srv1"}}},
			{Name: "c1", Hosts: []HostSpec{{ServerID: "srv1"}}},                            // 无 bootstrap
			{Name: "c1", BootstrapServerID: "srv1"},                                        // 无主机
			{Name: "c1", BootstrapServerID: "srv9", Hosts: []HostSpec{{ServerID: "srv1"}}}, // bootstrap 不在列表
			{Name: "c1", BootstrapServerID: "srv1", Hosts: []HostSpec{{ServerID: "srv1", Roles: []string{"admin"}}}},
			{Name: "c1", BootstrapServerID: "srv1", Hosts: []HostSpec{{ServerID: "srv1", Devices: []string{"sdb"}}}},
			{Name: "c1", BootstrapServerID: "srv1", Hosts: []HostSpec{{ServerID: ""}}},
			{Name: "c1", BootstrapServerID: "srv1", PublicNetwork: "not-cidr", Hosts: []HostSpec{{ServerID: "srv1"}}},
			{Name: "c1", BootstrapServerID: "srv1", Hosts: []HostSpec{{ServerID: "ghost"}}}, // 未登记服务器
		}
		for i := range bad {
			if _, err := svc.CreateCluster(ctx, bad[i]); err == nil {
				t.Fatalf("case %d 应报错", i)
			} else if !strings.Contains(err.Error(), "invalid input") && !strings.Contains(err.Error(), ErrInvalidInput.Error()) {
				// 错误信息可读即可,统一 ErrInvalidInput 包装。
				t.Fatalf("case %d 错误类型不符: %v", i, err)
			}
		}

		cl, err := svc.CreateCluster(ctx, CreateInput{
			Name: "prod", BootstrapServerID: "srv1",
			Hosts: []HostSpec{
				{ServerID: "srv1", Roles: []string{"mon", "mgr"}, Devices: []string{"/dev/sdb"}},
				{ServerID: "srv2", Roles: []string{"osd"}, Devices: []string{"/dev/sdb", "/dev/sdc"}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if cl.State != StateCreated || cl.Image != DefaultImage || cl.DashboardUser != "admin" {
			t.Fatalf("默认值不符: %+v", cl)
		}
		if _, err := svc.CreateCluster(ctx, CreateInput{Name: "prod", BootstrapServerID: "srv1", Hosts: []HostSpec{{ServerID: "srv1"}}}); err != ErrNameTaken {
			t.Fatalf("重名应 ErrNameTaken,得 %v", err)
		}
		hosts, err := svc.ListHosts(ctx, cl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(hosts) != 2 || hosts[0].ServerName != "node1" {
			t.Fatalf("主机行不符: %+v", hosts)
		}
	})
}

// ── 部署管线(fake runner 全链路)────────────────────────────────────────────

const testFSID = "3b84f4a1-1111-2222-3333-abcdefabcdef"

// deployHandle 覆盖管线会触达的全部命令形态。
func deployHandle(cmd []string) (string, int) {
	joined := strings.Join(cmd, " ")
	switch {
	case joined == "id -u":
		return "0", 0
	case cmd[0] == "hostname":
		return "mon-host", 0
	case joined == "docker --version":
		return "Docker version 24.0.0", 0
	case joined == "systemctl --version", joined == "python3 --version", joined == "lvs --version":
		return "ok", 0
	case strings.HasPrefix(joined, "timedatectl show"):
		return "1", 0
	case strings.HasPrefix(joined, "lsblk "):
		return `{"blockdevices":[{"path":"/dev/sdb","type":"disk","fstype":"","mountpoint":null},{"path":"/dev/sdc","type":"disk","fstype":"","mountpoint":null}]}`, 0
	case joined == "apt-get --version":
		return "apt 2.4", 0
	case strings.HasPrefix(joined, "apt-get "):
		return "ok", 0
	case joined == "cephadm --version":
		return "cephadm version 19.2.1", 0
	case cmd[0] == "cephadm":
		return "Cluster fsid: " + testFSID + "\nDashboard: https://10.0.0.11:8443/", 0
	case joined == "awk /^fsid/{print $3} /etc/ceph/ceph.conf":
		return testFSID, 0
	case joined == "ceph -v":
		return "ceph version 19.2.1", 0
	case strings.HasPrefix(joined, "ceph orch host add"):
		return "Added host 'mon-host'", 0
	case strings.HasPrefix(joined, "ceph orch host ls"):
		return `[{"host":"mon-host","addr":"10.0.0.11","labels":[],"status":""}]`, 0
	case strings.HasPrefix(joined, "ceph orch daemon add osd"):
		return "Created osd.osd.0 on host 'mon-host'", 0
	case joined == "ceph -s -f json":
		return `{"health":{"status":"HEALTH_OK"},"monmap":{"num_mons":1},"osdmap":{"num_osds":1,"num_up_osds":1,"num_in_osds":1},"pgmap":{"bytes_total":1000,"bytes_used":400,"bytes_avail":600,"pgs_by_state":[{"state_name":"active+clean","count":64}]}}`, 0
	case strings.HasPrefix(joined, "ceph mgr services"):
		return `{"dashboard":"https://10.0.0.11:8443/"}`, 0
	case strings.HasPrefix(joined, "ceph health detail"):
		return `{"checks":{}}`, 0
	}
	return "", 1
}

func TestStartDeployPipeline(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ft := newFakeTarget()
		ft.add("srv1", "node1", "10.0.0.11")
		fr := &fakeRunner{handle: deployHandle}
		svc := newTestService(t, st, ft, fr)
		ctx := context.Background()

		cl, err := svc.CreateCluster(ctx, CreateInput{
			Name: "prod", BootstrapServerID: "srv1",
			Hosts: []HostSpec{{ServerID: "srv1", Roles: []string{"mon", "mgr"}, Devices: []string{"/dev/sdb"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.StartDeploy(ctx, cl.ID); err != nil {
			t.Fatal(err)
		}
		// 轮询至完成(fake 即时响应,秒级)。
		deadline := time.Now().Add(10 * time.Second)
		for {
			st2, err := svc.DeployStatus(ctx, cl.ID)
			if err != nil {
				t.Fatal(err)
			}
			if st2.Done {
				if st2.Error != "" {
					t.Fatalf("部署失败: %s\nlog: %v", st2.Error, st2.Log)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("部署超时未完成: %+v", st2)
			}
			time.Sleep(20 * time.Millisecond)
		}
		got, err := svc.GetCluster(ctx, cl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != StateReady || got.FSID != testFSID || got.DashboardURL != "https://10.0.0.11:8443/" {
			t.Fatalf("回填不符: %+v", got)
		}
		// 命令拼装断言:bootstrap 带 mon-ip/dashboard 旗标;OSD 走 host:device。
		if !fr.saw(func(c []string) bool {
			return len(c) > 5 && c[0] == "cephadm" && c[3] == "bootstrap" && c[4] == "--mon-ip" && c[5] == "10.0.0.11"
		}) {
			t.Fatalf("bootstrap 命令拼装不符:\n%v", fr.calls)
		}
		if !fr.saw(func(c []string) bool {
			return strings.Join(c, " ") == "ceph orch daemon add osd mon-host:/dev/sdb"
		}) {
			t.Fatalf("OSD 命令拼装不符")
		}
		// 状态采集走 TTL 缓存路径。
		status, err := svc.ClusterStatus(ctx, cl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if status.Health != "HEALTH_OK" || status.MonCount != 1 || status.OsdUp != 1 || status.TotalBytes != 1000 {
			t.Fatalf("状态解析不符: %+v", status)
		}
	})
}

// ── 管理操作命令拼装 ─────────────────────────────────────────────────────────

// mgmtHandle 覆盖管理面命令(状态/池/RBD/CephFS/OSD)。
func mgmtHandle(cmd []string) (string, int) {
	joined := strings.Join(cmd, " ")
	switch {
	case strings.HasPrefix(joined, "ceph osd pool ls detail"):
		return `[{"pool_name":"rbd","size":3,"pg_num":32,"erasure_code_profile":"","bytes_used":1024,"quota_max_bytes":0,"application_metadata":{"rbd":{}}}]`, 0
	case strings.HasPrefix(joined, "ceph osd pool "):
		return "ok", 0
	case strings.HasPrefix(joined, "rbd ls "):
		return `[{"name":"disk1","size":1073741824,"provisioning":"thin"},{"name":"disk1@snap1","size":1073741824}]`, 0
	case strings.HasPrefix(joined, "rbd "):
		return "ok", 0
	case strings.HasPrefix(joined, "ceph fs ls"):
		return `[{"name":"cephfs","metadata_pool":"cephfs_metadata","data_pools":["cephfs_data"]}]`, 0
	case joined == "ceph fs subvolumels cephfs -f json":
		return `["sv1"]`, 0
	case strings.HasPrefix(joined, "ceph fs subvolume getpath"):
		return "/volumes/cephfs/sv1/abc", 0
	case strings.HasPrefix(joined, "ceph fs subvolume info"):
		return `{"usage":{"bytes_used":2048},"bytes_quota":0}`, 0
	case strings.HasPrefix(joined, "ceph fs "):
		return "ok", 0
	case strings.HasPrefix(joined, "ceph osd df"):
		return `{"nodes":[{"id":0,"status":"up,in","kb":1024,"used_kb":512,"util":50.0}]}`, 0
	case strings.HasPrefix(joined, "ceph osd metadata"):
		return `[{"id":0,"hostname":"mon-host","devices":["sdb"]}]`, 0
	case strings.HasPrefix(joined, "ceph osd "):
		return "OK to destroy", 0
	case strings.HasPrefix(joined, "ceph orch "):
		return "ok", 0
	case joined == "ceph -s -f json":
		return `{"health":{"status":"HEALTH_OK"},"monmap":{"num_mons":1}}`, 0
	case strings.HasPrefix(joined, "ceph health detail"):
		return `{"checks":{}}`, 0
	case joined == "id -u":
		return "0", 0
	case cmd[0] == "hostname":
		return "mon-host", 0
	case joined == "docker --version":
		return "Docker version 24.0.0", 0
	case joined == "ceph mgr services -f json":
		return `{"dashboard":"https://10.0.0.11:8443/"}`, 0
	}
	return "", 1
}

func newReadyCluster(t *testing.T, st *store.Store, ft *fakeTarget) *Cluster {
	t.Helper()
	svcCreate := newTestService(t, st, ft, &fakeRunner{handle: mgmtHandle})
	cl, err := svcCreate.CreateCluster(context.Background(), CreateInput{
		Name: "prod", BootstrapServerID: "srv1",
		Hosts: []HostSpec{{ServerID: "srv1", Roles: []string{"mon"}, Hostname: "mon-host", Addr: "10.0.0.11"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 直接置 ready(部署链路另有专测)。
	s := svcCreate.(*service)
	if err := s.updateClusterState(context.Background(), cl.ID, StateReady); err != nil {
		t.Fatal(err)
	}
	return cl
}

func TestManagementOps(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ft := newFakeTarget()
		ft.add("srv1", "node1", "10.0.0.11")
		cl := newReadyCluster(t, st, ft)
		fr := &fakeRunner{handle: mgmtHandle}
		svc := newTestService(t, st, ft, fr)
		ctx := context.Background()

		// 池:列表/创建/更新。
		pools, err := svc.ListPools(ctx, cl.ID)
		if err != nil || len(pools) != 1 || pools[0].Name != "rbd" || pools[0].Type != "replicated" || pools[0].Size != 3 {
			t.Fatalf("池解析不符: %+v %v", pools, err)
		}
		if err := svc.CreatePool(ctx, cl.ID, PoolCreateInput{Name: "data", PGNum: 128, Size: 3, Apps: []string{"rbd"}}); err != nil {
			t.Fatal(err)
		}
		if !fr.saw(func(c []string) bool { return strings.Join(c, " ") == "ceph osd pool create data 128" }) {
			t.Fatal("建池命令不符")
		}
		quota := uint64(1024)
		if err := svc.UpdatePool(ctx, cl.ID, "data", PoolUpdateInput{QuotaMaxBytes: &quota}); err != nil {
			t.Fatal(err)
		}
		if !fr.saw(func(c []string) bool { return strings.Join(c, " ") == "ceph osd pool set data quota_max_bytes 1024" }) {
			t.Fatal("配额命令不符")
		}
		if err := svc.CreatePool(ctx, cl.ID, PoolCreateInput{Name: "bad pool"}); err == nil {
			t.Fatal("非法池名应报错")
		}

		// RBD:镜像/快照。
		images, err := svc.ListImages(ctx, cl.ID, "rbd")
		if err != nil || len(images) != 1 || images[0].Name != "disk1" || images[0].SizeBytes != 1073741824 {
			t.Fatalf("镜像解析不符: %+v %v", images, err)
		}
		if err := svc.CreateImage(ctx, cl.ID, "rbd", ImageCreateInput{Name: "disk2", SizeGB: 10}); err != nil {
			t.Fatal(err)
		}
		if !fr.saw(func(c []string) bool { return strings.Join(c, " ") == "rbd create disk2 -p rbd --size 10G" }) {
			t.Fatal("建镜像命令不符")
		}
		if err := svc.CreateSnapshot(ctx, cl.ID, "rbd", "disk1", "snap2"); err != nil {
			t.Fatal(err)
		}
		if !fr.saw(func(c []string) bool { return strings.Join(c, " ") == "rbd snap create rbd/disk1@snap2" }) {
			t.Fatal("快照命令不符")
		}
		if err := svc.RollbackSnapshot(ctx, cl.ID, "rbd", "disk1", "snap2"); err != nil {
			t.Fatal(err)
		}
		if !fr.saw(func(c []string) bool { return strings.Join(c, " ") == "rbd snap rollback rbd/disk1@snap2" }) {
			t.Fatal("回滚命令不符")
		}

		// CephFS:列表/子卷。
		fss, err := svc.ListFilesystems(ctx, cl.ID)
		if err != nil || len(fss) != 1 || fss[0].MetadataPool != "cephfs_metadata" {
			t.Fatalf("fs 解析不符: %+v %v", fss, err)
		}
		svs, err := svc.ListSubvolumes(ctx, cl.ID, "cephfs")
		if err != nil || len(svs) != 1 || svs[0].Path != "/volumes/cephfs/sv1/abc" || svs[0].BytesUsed != 2048 {
			t.Fatalf("子卷解析不符: %+v %v", svs, err)
		}
		if err := svc.CreateSubvolume(ctx, cl.ID, "cephfs", SubvolumeCreateInput{Name: "sv2", SizeGB: 1}); err != nil {
			t.Fatal(err)
		}
		if !fr.saw(func(c []string) bool {
			return strings.HasPrefix(strings.Join(c, " "), "ceph fs subvolume create cephfs sv2 --size ")
		}) {
			t.Fatal("建子卷命令不符")
		}

		// OSD:列表合并/单盘追加。
		osds, err := svc.ListOSDs(ctx, cl.ID)
		if err != nil || len(osds) != 1 || osds[0].Host != "mon-host" || osds[0].Device != "/dev/sdb" || !osds[0].Up || !osds[0].In {
			t.Fatalf("OSD 合并解析不符: %+v %v", osds, err)
		}
		if err := svc.AddOSD(ctx, cl.ID, AddOSDInput{Host: "mon-host", Device: "/dev/sdc"}); err != nil {
			t.Fatal(err)
		}
		if !fr.saw(func(c []string) bool { return strings.Join(c, " ") == "ceph orch daemon add osd mon-host:/dev/sdc" }) {
			t.Fatal("加盘命令不符")
		}
		if err := svc.RemoveOSD(ctx, cl.ID, 0); err != nil {
			t.Fatal(err)
		}
		if !fr.saw(func(c []string) bool {
			return strings.Join(c, " ") == "ceph osd purge osd.0 --yes-i-really-really-mean-it"
		}) {
			t.Fatal("purge 命令不符")
		}

		// 未就绪集群的管理操作应 ErrClusterNotReady。
		cl2, err := svc.CreateCluster(ctx, CreateInput{
			Name: "stage", BootstrapServerID: "srv1", Hosts: []HostSpec{{ServerID: "srv1"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ListPools(ctx, cl2.ID); err == nil {
			t.Fatal("未就绪集群应报 ErrClusterNotReady")
		}
	})
}

// ── 解析器 ───────────────────────────────────────────────────────────────────

func TestParsersTolerant(t *testing.T) {
	// 非法/空输入绝不 panic,给零值。
	if st := parseClusterStatus("not-json"); st.Health != "HEALTH_UNKNOWN" {
		t.Fatalf("非法输入应 UNKNOWN: %+v", st)
	}
	if got := parsePools(""); len(got) != 0 {
		t.Fatal("空池输入应空")
	}
	if got := parseOSDDf("{}"); len(got) != 0 {
		t.Fatal("空 osd 输入应空")
	}
	if got := parseImages(`["img1","img2@snap"]`, "p"); len(got) != 1 || got[0].Name != "img1" {
		t.Fatalf("字符串数组形态镜像解析不符: %+v", got)
	}
	// words:逗号与数组两种 status 形态。
	if ws := words("up,in"); !hasWord(ws, "up") || !hasWord(ws, "in") {
		t.Fatal("字符串 status 解析不符")
	}
	if ws := words([]any{"up", "in"}); !hasWord(ws, "up") || !hasWord(ws, "in") {
		t.Fatal("数组 status 解析不符")
	}
	// health detail。
	detail := parseHealthDetail(`{"checks":{"OSD_DOWN":{"severity":"HEALTH_WARN","summary":{"message":"1 osd down"}}}}`)
	if len(detail) != 1 || !strings.Contains(detail[0], "1 osd down") {
		t.Fatalf("health detail 解析不符: %v", detail)
	}
}
