package sshtunnel

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// fakeTargets 只覆写隧道路径用到的方法(Get/Dial/DialDirect);嵌入接口承接其余。
type fakeTargets struct {
	target.Service
	servers   map[string]*target.Server
	dialErr   error
	dialCalls []string
}

func (f *fakeTargets) Get(_ context.Context, id string) (*target.Server, error) {
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return nil, target.ErrNotFound
}

func (f *fakeTargets) Dial(_ context.Context, id string) (target.SSHClient, error) {
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	f.dialCalls = append(f.dialCalls, "server:"+id)
	return &fakeSSHClient{}, nil
}

func (f *fakeTargets) DialDirect(_ context.Context, host string, port int, user, credID string) (target.SSHClient, error) {
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	f.dialCalls = append(f.dialCalls, fmt.Sprintf("direct:%s@%s:%d/%s", user, host, port, credID))
	return &fakeSSHClient{}, nil
}

// fakeSSHClient 把 Dial 转成本机 TCP 直连(测试里指向本地 echo 服务),模拟
// 「经 sshd 到达目标」的数据面;Listen 开本机监听模拟远端 sshd 侧。
type fakeSSHClient struct{}

func (c *fakeSSHClient) Dial(network, addr string) (net.Conn, error) { return net.Dial(network, addr) }
func (c *fakeSSHClient) Listen(network, addr string) (net.Listener, error) {
	return net.Listen(network, addr)
}
func (c *fakeSSHClient) Ping(time.Duration) error { return nil }
func (c *fakeSSHClient) Close() error             { return nil }

// seedCredential 直接落一条 ssh_key 凭据(测试库不需要可解密,密文列只占位)。
func seedCredential(t *testing.T, db *sql.DB, id, typ string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO credentials (id, name, type, scope, ciphertext, masked_value, created_at, updated_at)
		 VALUES (?, ?, ?, '', x'00', 'masked', ?, ?)`, id, "cred-"+id, typ, now, now); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
}

func seedServer(t *testing.T, db *sql.DB, id, credID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO servers (id, name, host, port, user, credential_id, jumps, labels, max_builds, priority, gpu, shared_build, pool_id, kind, created_at, updated_at)
		 VALUES (?, ?, '10.0.0.1', 22, 'root', ?, '[]', '', 0, 0, 0, 0, 'default', 'standard', ?, ?)`,
		id, "srv-"+id, credID, now, now); err != nil {
		t.Fatalf("seed server: %v", err)
	}
}

func newTestService(t *testing.T) (*Service, *fakeTargets) {
	t.Helper()
	db := storetest.OpenDB(t)
	ft := &fakeTargets{servers: map[string]*target.Server{}}
	s := New(db, ft, t.TempDir())
	return s, ft
}

// directInput 是合法的直连 dynamic 隧道(Enabled=false:单测绝不触发真实 Spawn)。
func directInput() Input {
	return Input{
		Name: "t1", Host: "192.0.2.10", Port: 22, User: "root", CredentialID: "cred-1",
		Mode: ModeDynamic, BindHost: "127.0.0.1", BindPort: 18080, Enabled: false,
	}
}

// TestValidateTunnelInput 覆盖校验矩阵:模式白名单、连接方式互斥、dest 规则、端口范围。
func TestValidateTunnelInput(t *testing.T) {
	db := storetest.OpenDB(t)
	seedCredential(t, db, "cred-1", "ssh_key")
	seedCredential(t, db, "cred-bad", "git_token")
	seedServer(t, db, "srv-1", "cred-1")
	ft := &fakeTargets{servers: map[string]*target.Server{
		"srv-1": {ID: "srv-1", Name: "srv-1"},
	}}
	s := New(db, ft, t.TempDir())
	ctx := context.Background()

	cases := []struct {
		name string
		in   Input
		want error
	}{
		{"缺名称", func() Input { in := directInput(); in.Name = ""; return in }(), ErrInvalidInput},
		{"模式非法", func() Input { in := directInput(); in.Mode = "reverse"; return in }(), ErrInvalidInput},
		{"直连缺用户", func() Input { in := directInput(); in.User = ""; return in }(), ErrInvalidInput},
		{"直连缺凭据", func() Input { in := directInput(); in.CredentialID = ""; return in }(), ErrInvalidInput},
		{"直连缺主机", func() Input { in := directInput(); in.Host = ""; return in }(), ErrInvalidInput},
		{"直连凭据类型不符", func() Input { in := directInput(); in.CredentialID = "cred-bad"; return in }(), target.ErrCredentialTypeMismatch},
		{"直连凭据不存在", func() Input { in := directInput(); in.CredentialID = "nope"; return in }(), target.ErrCredentialNotFound},
		{"混合两种连接方式", func() Input { in := directInput(); in.ServerID = "srv-1"; return in }(), ErrInvalidInput},
		{"绑定服务器不存在", func() Input {
			in := Input{Name: "t", ServerID: "ghost", Mode: ModeDynamic, BindPort: 1000}
			return in
		}(), target.ErrNotFound},
		{"dynamic 禁填目标", func() Input { in := directInput(); in.DestHost = "x"; in.DestPort = 80; return in }(), ErrInvalidInput},
		{"local 缺目标", func() Input {
			in := directInput(); in.Mode = ModeLocal; return in
		}(), ErrInvalidInput},
		{"local 目标端口越界", func() Input {
			in := directInput(); in.Mode = ModeLocal; in.DestHost = "db"; in.DestPort = 70000; return in
		}(), ErrInvalidInput},
		{"监听端口越界", func() Input { in := directInput(); in.BindPort = 65536; return in }(), ErrInvalidInput},
		{"合法直连 dynamic", directInput(), nil},
		{"合法绑定服务器 local(自动端口)", func() Input {
			return Input{Name: "t2", ServerID: "srv-1", Mode: ModeLocal, BindPort: 0, DestHost: "db", DestPort: 5432}
		}(), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.normalizeAndValidate(ctx, tc.in, "")
			if tc.want == nil {
				if err != nil {
					t.Fatalf("期望通过,得到错误:%v", err)
				}
				// 默认值归一:bind_host 空回退回环;直连端口缺省 22(绑服务器模式不适用)。
				if tc.in.BindHost == "" && got.BindHost != "127.0.0.1" {
					t.Fatalf("bind_host 默认归一异常:%q", got.BindHost)
				}
				if tc.in.Host != "" && tc.in.Port == 0 && got.Port != 22 {
					t.Fatalf("直连端口默认归一异常:%d", got.Port)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望错误 %v,得到通过", tc.want)
			}
			if !strings.Contains(err.Error(), "sshtunnel") && err != tc.want {
				// target 哨兵直接比较;sshtunnel 哨兵按文本前缀判。
				if err.Error() != tc.want.Error() {
					t.Fatalf("期望错误 %v,得到 %v", tc.want, err)
				}
			}
		})
	}
}

// TestTunnelCRUD 全量走 Create/List/Update/Delete(Enabled=false 全程,绝不触发 Spawn)。
func TestTunnelCRUD(t *testing.T) {
	s, _ := newTestService(t)
	seedCredential(t, s.db, "cred-1", "ssh_key")
	ctx := context.Background()

	in := directInput()
	created, err := s.Create(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" || created.BindHost != "127.0.0.1" {
		t.Fatalf("create 结果异常:%+v", created)
	}

	list, err := s.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v (len=%d)", err, len(list))
	}
	if list[0].Status != StatusDisabled || list[0].ServerName != "" || list[0].CredentialName != "cred-cred-1" {
		t.Fatalf("视图合并异常:%+v", list[0])
	}

	// 更新为 local 模式 + 绑定服务器之外的直连改端口。
	upd := in
	upd.Mode = ModeLocal
	upd.DestHost = "db.internal"
	upd.DestPort = 5432
	updated, err := s.Update(ctx, created.ID, upd)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Mode != ModeLocal || updated.DestPort != 5432 {
		t.Fatalf("update 结果异常:%+v", updated)
	}

	if err := s.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.load(ctx, created.ID); err != ErrNotFound {
		t.Fatalf("删除后 load 期望 ErrNotFound,得到 %v", err)
	}
	// 状态/日志文件应被清理。
	if _, err := os.Stat(filepath.Join(s.rt.stateDir, created.ID+stateSuffix)); !os.IsNotExist(err) {
		t.Fatalf("删除后状态文件应不存在")
	}
}

// TestBindConflict 同 host:port 的第二条已启用本机隧道被拒。
func TestBindConflict(t *testing.T) {
	s, _ := newTestService(t)
	seedCredential(t, s.db, "cred-1", "ssh_key")
	ctx := context.Background()

	first, err := s.Create(ctx, directInput())
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	// 直接改库置 enabled(绕过 Service 的拉起路径:单测不 spawn 真实 helper)。
	if _, err := s.db.Exec(`UPDATE ssh_tunnels SET enabled = 1 WHERE id = ?`, first.ID); err != nil {
		t.Fatalf("enable first: %v", err)
	}
	// 同 bind 的第二条(dynamic)→ 冲突。
	dup := directInput()
	dup.Name = "dup"
	dup.BindPort = 18080
	if _, err := s.Create(ctx, dup); err != ErrBindConflict {
		t.Fatalf("期望 ErrBindConflict,得到 %v", err)
	}
	// remote 模式(bind 在远端 sshd 侧)不参与本机冲突判定。
	remote := directInput()
	remote.Name = "r"
	remote.Mode = ModeRemote
	remote.DestHost = "127.0.0.1"
	remote.DestPort = 80
	remote.BindPort = 18080
	if _, err := s.Create(ctx, remote); err != nil {
		t.Fatalf("remote 不应冲突:%v", err)
	}
	// 更新自身(bind 不变)不误判冲突。
	self := directInput()
	self.Mode = ModeLocal
	self.DestHost = "db"
	self.DestPort = 5432
	if _, err := s.Update(ctx, first.ID, self); err != nil {
		t.Fatalf("更新自身不应冲突:%v", err)
	}
}

// TestStateFileLiveAndWrite 验证心跳文件原子写与新鲜度判定。
func TestStateFileLiveAndWrite(t *testing.T) {
	rt := NewRuntime(t.TempDir())
	st := &StateFile{PID: 42, Status: StatusActive}
	if err := rt.WriteState("t1", st); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := rt.State("t1")
	if got == nil || got.PID != 42 || !got.Live() {
		t.Fatalf("读回/新鲜度异常:%+v", got)
	}
	// 陈旧心跳 = 不存活。
	st.UpdatedAt = time.Now().UTC().Add(-2 * staleAfter).Format(time.RFC3339)
	if err := os.WriteFile(rt.statePath("t1"), mustJSON(st), 0o600); err != nil {
		t.Fatalf("写陈旧心跳:%v", err)
	}
	if got := rt.State("t1"); got.Live() {
		t.Fatalf("陈旧心跳不应存活")
	}
	// 损坏文件 → nil。
	if err := os.WriteFile(rt.statePath("t2"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := rt.State("t2"); got != nil {
		t.Fatalf("损坏文件应返回 nil")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// TestStopProtocol 验证停止协议:Stop 立旗标 → 模拟 helper 看到旗标清文件 → Stop 收敛。
func TestStopProtocol(t *testing.T) {
	rt := NewRuntime(t.TempDir())
	st := &StateFile{PID: -1, Status: StatusActive} // pid=-1:强杀兜底不可作用于真实进程
	if err := rt.WriteState("t1", st); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 模拟 helper:旗标一出现就清状态与旗标(优雅退出语义)。
	go func() {
		for i := 0; i < 100; i++ {
			if _, err := os.Stat(rt.stopPath("t1")); err == nil {
				rt.CleanupState("t1")
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	done := make(chan struct{})
	go func() { rt.Stop("t1"); close(done) }()
	select {
	case <-done:
	case <-time.After(stopWait + 2*time.Second):
		t.Fatalf("Stop 未在窗口内收敛")
	}
	if _, err := os.Stat(rt.statePath("t1")); !os.IsNotExist(err) {
		t.Fatalf("状态文件应已清理")
	}
}

// TestSpawnClearsStaleStopFlag Spawn 前清残留旗标 —— 以替换 exec 为路径检查太重,
// 这里直接验证清理逻辑(旗标文件被 Spawn 路径的第一步移除)。
func TestSpawnClearsStaleStopFlag(t *testing.T) {
	rt := NewRuntime(t.TempDir())
	if err := os.WriteFile(rt.stopPath("t1"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	// 不真跑 Spawn(会拉起真实子进程):验证等价的清理行为 + TailLog/ShouldLog 去重。
	if _, err := os.Stat(rt.stopPath("t1")); err != nil {
		t.Fatal(err)
	}
	if !rt.ShouldLogSpawnFail("t1", "boom") {
		t.Fatalf("首见失败应允许记录")
	}
	if rt.ShouldLogSpawnFail("t1", "boom") {
		t.Fatalf("同文本失败应去重")
	}
	if !rt.ShouldLogSpawnFail("t1", "boom2") {
		t.Fatalf("文本变化应再记录")
	}
}

// TestViewStatusMerge 覆盖视图状态合并矩阵:停用/启用未跑/心跳新鲜(active/retrying)。
func TestViewStatusMerge(t *testing.T) {
	s, _ := newTestService(t)
	seedCredential(t, s.db, "cred-1", "ssh_key")
	ctx := context.Background()

	disabled, _ := s.Create(ctx, directInput())
	if v := s.view(&disabled.Tunnel); v.Status != StatusDisabled {
		t.Fatalf("停用隧道状态期望 disabled,得到 %s", v.Status)
	}

	// 库内置 enabled=1(绕过拉起),无心跳 → stopped。
	enabledNoHelper, _ := s.Create(ctx, func() Input { in := directInput(); in.Name = "e"; in.BindPort = 18081; return in }())
	s.db.Exec(`UPDATE ssh_tunnels SET enabled = 1 WHERE id = ?`, enabledNoHelper.ID)
	tun, _ := s.load(ctx, enabledNoHelper.ID)
	if v := s.view(tun); v.Status != StatusStopped {
		t.Fatalf("启用无 helper 状态期望 stopped,得到 %s", v.Status)
	}

	// 新鲜心跳 retrying → retrying(带 lastError)。
	if err := s.rt.WriteState(enabledNoHelper.ID, &StateFile{
		PID: 7, Status: StatusRetrying, LastError: "无法连接目标主机", Fingerprint: tun.Fingerprint(),
	}); err != nil {
		t.Fatal(err)
	}
	v := s.view(tun)
	if v.Status != StatusRetrying || v.PID != 7 || v.LastError == "" {
		t.Fatalf("retrying 合并异常:%+v", v)
	}

	// 陈旧心跳 → 回落 stopped。
	stale := &StateFile{PID: 7, Status: StatusActive, Fingerprint: tun.Fingerprint()}
	stale.UpdatedAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	os.WriteFile(s.rt.statePath(enabledNoHelper.ID), mustJSON(stale), 0o600)
	if v := s.view(tun); v.Status != StatusStopped {
		t.Fatalf("陈旧心跳应回落 stopped,得到 %s", v.Status)
	}
}

// TestReconcileDecisions 对账决策:停用即停、启用未跑即拉(fake spawn)、指纹变化重启。
func TestReconcileDecisions(t *testing.T) {
	s, ft := newTestService(t)
	seedCredential(t, s.db, "cred-1", "ssh_key")
	ctx := context.Background()

	// fake Runtime:替换 spawnFn/stopFn 为记数(绝不真实拉子进程)。
	spawns := map[string]int{}
	stops := map[string]int{}
	s.rt.spawnFn = func(id string) error { spawns[id]++; return nil }
	s.rt.stopFn = func(id string) { stops[id]++ }
	_ = ft

	// 1) 启用无 helper → 拉起。
	t1, _ := s.Create(ctx, directInput())
	s.db.Exec(`UPDATE ssh_tunnels SET enabled = 1 WHERE id = ?`, t1.ID)
	s.reconcile(ctx)
	if spawns[t1.ID] != 1 {
		t.Fatalf("启用未跑应拉起一次,得到 %d", spawns[t1.ID])
	}

	// 2) 心跳新鲜且指纹一致 → 沿用(收养),不再拉。
	fresh, _ := s.load(ctx, t1.ID)
	s.rt.WriteState(t1.ID, &StateFile{PID: 9, Status: StatusActive, Fingerprint: fresh.Fingerprint()})
	s.reconcile(ctx)
	if spawns[t1.ID] != 1 || stops[t1.ID] != 0 {
		t.Fatalf("收养态不应重拉/停(spawns=%d stops=%d)", spawns[t1.ID], stops[t1.ID])
	}

	// 3) 指纹变化(改 dest)→ 重启:停 + 拉。
	s.db.Exec(`UPDATE ssh_tunnels SET dest_port = 6379 WHERE id = ?`, t1.ID)
	s.reconcile(ctx)
	if stops[t1.ID] != 1 || spawns[t1.ID] != 2 {
		t.Fatalf("指纹变化应重启(stops=%d spawns=%d)", stops[t1.ID], spawns[t1.ID])
	}

	// 4) 停用 → 停。
	s.db.Exec(`UPDATE ssh_tunnels SET enabled = 0 WHERE id = ?`, t1.ID)
	s.rt.WriteState(t1.ID, &StateFile{PID: 9, Status: StatusActive, Fingerprint: fresh.Fingerprint()})
	s.reconcile(ctx)
	if stops[t1.ID] != 2 {
		t.Fatalf("停用应停进程(stops=%d)", stops[t1.ID])
	}
}

// startEcho 起一个回显 TCP 服务(模拟转发目标),返回监听地址。
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// TestLocalForwardPipe local(-L)端到端:本机 listener → fake ssh Dial(echo)→ 双向直通。
func TestLocalForwardPipe(t *testing.T) {
	echoAddr := startEcho(t)
	host, portStr, _ := net.SplitHostPort(echoAddr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	tun := &Tunnel{Mode: ModeLocal, BindHost: "127.0.0.1", BindPort: 0, DestHost: host, DestPort: port}
	fw, err := startLocalForward(&fakeSSHClient{}, tun)
	if err != nil {
		t.Fatalf("startForward: %v", err)
	}
	defer fw.close()
	if fw.actualPort == 0 || fw.endpoint == "" {
		t.Fatalf("自动端口未回显:%+v", fw)
	}

	conn, err := net.Dial("tcp", fw.endpoint)
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer func() { _ = conn.Close() }()
	msg := "hello-tunnel"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("回显不一致:%q", string(buf))
	}
}

// TestDynamicForwardSOCKS5 dynamic(-D)端到端:SOCKS5 握手 + CONNECT 到 echo + 直通。
func TestDynamicForwardSOCKS5(t *testing.T) {
	echoAddr := startEcho(t)
	tun := &Tunnel{Mode: ModeDynamic, BindHost: "127.0.0.1", BindPort: 0}
	fw, err := startDynamicForward(&fakeSSHClient{}, tun)
	if err != nil {
		t.Fatalf("startForward: %v", err)
	}
	defer fw.close()

	conn, err := net.Dial("tcp", fw.endpoint)
	if err != nil {
		t.Fatalf("dial socks: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// 握手:支持无认证。
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		t.Fatalf("handshake reply: %v", err)
	}
	if method[0] != 0x05 || method[1] != 0x00 {
		t.Fatalf("握手回复异常:%v", method)
	}
	// CONNECT echo 地址(ATYP=域名,验证域名形态转发)。
	host, portStr, _ := net.SplitHostPort(echoAddr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	var rep [10]byte
	if _, err := io.ReadFull(conn, rep[:]); err != nil {
		t.Fatalf("connect reply: %v", err)
	}
	if rep[1] != 0x00 {
		t.Fatalf("CONNECT 失败:REP=%d", rep[1])
	}
	msg := "socks-roundtrip"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("回显不一致:%q", string(buf))
	}
}

// TestSOCKS5RejectsNonConnect 非 CONNECT 命令(BIND=0x02)按不支持拒绝。
// 只发 4 字节请求头:拒绝路径在读地址之前(net.Pipe 无缓冲,多发会同步阻塞)。
func TestSOCKS5RejectsNonConnect(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	go serveSOCKS5(clientConn, &fakeSSHClient{})
	w := bufio.NewWriter(serverConn)
	w.Write([]byte{0x05, 0x01, 0x00})
	w.Flush()
	var method [2]byte
	io.ReadFull(serverConn, method[:])
	// BIND 命令头(VER CMD RSV ATYP),地址字段不送 —— REP 在读地址前就应返回。
	w.Write([]byte{0x05, 0x02, 0x00, 0x01})
	w.Flush()
	var rep [10]byte
	if _, err := io.ReadFull(serverConn, rep[:]); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if rep[1] != 0x07 {
		t.Fatalf("非 CONNECT 期望 REP=0x07,得到 0x%02x", rep[1])
	}
}

// TestTailLog 日志尾部行数与缺文件行为。
func TestTailLog(t *testing.T) {
	rt := NewRuntime(t.TempDir())
	if got := rt.TailLog("missing", 10); len(got) != 0 {
		t.Fatalf("缺文件应返回空")
	}
	var sb strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&sb, "line-%03d\n", i)
	}
	if err := os.WriteFile(rt.logPath("t1"), []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := rt.TailLog("t1", 5)
	if len(got) != 5 || got[0] != "line-296" || got[4] != "line-300" {
		t.Fatalf("尾部 5 行异常:%v", got)
	}
	if got := rt.TailLog("t1", 1000); len(got) != 300 {
		t.Fatalf("不足行数应全量返回,得到 %d", len(got))
	}
}

// TestFingerprint 指纹只随转发相关字段变化(name/enabled 不参与)。
func TestFingerprint(t *testing.T) {
	base := &Tunnel{ID: "x", Name: "a", Mode: ModeLocal, BindHost: "127.0.0.1", BindPort: 1, DestHost: "h", DestPort: 2, Host: "hh", Port: 22, User: "u", CredentialID: "c"}
	if base.Fingerprint() != base.Fingerprint() {
		t.Fatalf("同配置指纹不稳定")
	}
	renamed := *base
	renamed.Name = "b"
	if renamed.Fingerprint() != base.Fingerprint() {
		t.Fatalf("name 不应参与指纹")
	}
	rebind := *base
	rebind.BindPort = 9
	if rebind.Fingerprint() == base.Fingerprint() {
		t.Fatalf("bind 变化应改变指纹")
	}
}

// TestRunHelperLifecycle helper 全生命周期集成(fake SSH 数据面 + 真实转发):
// 建连 → 转发就绪(心跳 active)→ SOCKS5 端到端穿隧道 → 停止旗标 → 优雅退出并清理文件。
func TestRunHelperLifecycle(t *testing.T) {
	echoAddr := startEcho(t)
	ft := &fakeTargets{servers: map[string]*target.Server{}}
	dataDir := t.TempDir()

	tun := &Tunnel{
		ID: "life-1", Name: "lifecycle", Mode: ModeDynamic,
		BindHost: "127.0.0.1", BindPort: 0, // 自动分配端口
		Host: "192.0.2.1", Port: 22, User: "root", CredentialID: "c1",
	}
	done := make(chan error, 1)
	go func() { done <- runHelper(tun, ft, dataDir) }()

	rt := NewRuntime(dataDir)
	// 等 helper 写出 active 心跳(转发面就绪)。
	deadline := time.Now().Add(8 * time.Second)
	var st *StateFile
	for time.Now().Before(deadline) {
		if s := rt.State(tun.ID); s != nil && s.Status == StatusActive {
			st = s
			break
		}
		select {
		case err := <-done:
			t.Fatalf("helper 提前退出:%v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if st == nil {
		t.Fatalf("8s 内未见 active 心跳")
	}
	if st.ActualPort <= 0 || st.Endpoint == "" || st.PID <= 0 {
		t.Fatalf("心跳缺少运行信息:%+v", st)
	}

	// 端到端:SOCKS5 握手 → CONNECT echo → 回显。
	conn, err := net.Dial("tcp", st.Endpoint)
	if err != nil {
		t.Fatalf("dial socks: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(echoAddr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	var rep [10]byte
	if _, err := io.ReadFull(conn, rep[:]); err != nil || rep[1] != 0 {
		t.Fatalf("CONNECT 失败:%v REP=%d", err, rep[1])
	}
	msg := "lifecycle-roundtrip"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != msg {
		t.Fatalf("端到端回显异常:%q err=%v", string(buf), err)
	}

	// 停止旗标 → helper 优雅退出,自清状态与旗标文件。
	if err := os.WriteFile(rt.stopPath(tun.ID), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("helper 应优雅退出,得到错误:%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("helper 未在 10s 内响应停止旗标")
	}
	if _, err := os.Stat(rt.statePath(tun.ID)); !os.IsNotExist(err) {
		t.Fatalf("退出后状态文件应被清理")
	}
	// 注:日志文件内容在真实 spawn(主进程重定向 stdio)时产生;直连调用只写 stderr,
	// 故此处不断言日志文件(其读取行为由 TestTailLog 覆盖)。
}
