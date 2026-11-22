package pveadmin

// upgrade_major_test.go — 大版本升级:pve8to9 解析、预检阻断、启动闸门、
// 滚动序列(有状态 fake:版本翻转/重启下线/换源)、fail-fast+retry、持久化 interrupted+resume、
// 中止只在步骤边界生效(绝不中断 dist-upgrade)。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

func TestParsePve8to9(t *testing.T) {
	out := `= CHECKING VERSION INFORMATION FOR PACKAGES =
PASS: pve-manager is up to date.
PASS: cluster is quorate.
WARN: custom sysctl settings found.
FAIL: ceph version too old.
SKIP: not a UEFI system.
TOTAL: 2 PASSED, 1 SKIPPED, 1 WARNINGS, 1 FAILURES
`
	r := parsePve8to9(out)
	if r.Passed != 2 || r.Skipped != 1 || r.Warnings != 1 || r.Failures != 1 {
		t.Fatalf("r = %+v", r)
	}
	if len(r.Lines) != 2 {
		t.Fatalf("WARN/FAIL 行 = %v", r.Lines)
	}
	if r.Summary == "" {
		t.Fatal("summary 未解析")
	}
}

func TestParsePveReleaseAndVersion(t *testing.T) {
	if m, n := parsePveRelease("8.4.1"); m != 8 || n != 4 {
		t.Fatalf("release = %d.%d", m, n)
	}
	if m := pveversionMajor("pve-manager/9.0.3/abc123 (running kernel: 6.14.8-1-pve)"); m != 9 {
		t.Fatalf("pveversion major = %d", m)
	}
	if m := pveversionMajor("garbage"); m != 0 {
		t.Fatalf("garbage = %d", m)
	}
}

// ---- 有状态 fake harness ----------------------------------------------------------------

// majorState 模拟集群升级过程:dist-upgrade 翻转版本、重启置下线(下一次 cluster/status
// 消费)、换源落盘翻转 suite;可注入失败/阻塞/ceph/待更新。
type majorState struct {
	mu               sync.Mutex
	upgraded         map[string]bool
	sourcesTrixe     map[string]bool
	downNode         string
	failUpgrade      map[string]bool
	blockUpgrade     chan struct{}
	ceph             bool
	pending          int
	shutdownCalls    []string
	rebootCalls      []string
	rebooted         map[string]bool
	distUpgradeArg   []string
	distUpgradeCalls map[string]int
	dumpExtra        string // 追加到 dump 尾部的额外源行(如 stable 系滚动别名)
}

func newMajorState() *majorState {
	return &majorState{upgraded: map[string]bool{}, sourcesTrixe: map[string]bool{}, failUpgrade: map[string]bool{},
		distUpgradeCalls: map[string]int{}, rebooted: map[string]bool{}}
}

func sidOfNode(node string) string {
	if node == "pve2" {
		return "node2"
	}
	return "node1"
}

func nodeFromPath(path string) string {
	if i := strings.Index(path, "/nodes/"); i >= 0 {
		rest := path[i+len("/nodes/"):]
		if j := strings.IndexByte(rest, '/'); j > 0 {
			return rest[:j]
		}
	}
	return ""
}

const majorSourcesBookworm = `deb http://deb.debian.org/debian bookworm main contrib
deb http://security.debian.org/debian-security bookworm-security main
deb http://download.proxmox.com/debian/pve bookworm pve-no-subscription
`

func (ms *majorState) wire(fc *fakePVEClient, fr *fakeRunner) {
	fc.getFn = func(path string) (any, error) {
		ms.mu.Lock()
		defer ms.mu.Unlock()
		switch {
		case strings.Contains(path, "/cluster/status"):
			pve1Online, pve2Online := 1, 1
			if ms.downNode == "pve1" {
				pve1Online = 0
				ms.downNode = ""
			} else if ms.downNode == "pve2" {
				pve2Online = 0
				ms.downNode = ""
			}
			return []map[string]any{
				{"type": "cluster", "name": "pve-prod", "quorate": 1},
				{"type": "node", "name": "pve1", "ip": "10.1.0.11", "online": pve1Online},
				{"type": "node", "name": "pve2", "ip": "10.1.0.12", "online": pve2Online},
			}, nil
		case strings.Contains(path, "/cluster/resources"):
			return []map[string]any{
				{"id": "qemu/101", "type": "qemu", "node": "pve1", "name": "web", "vmid": "101", "status": "running"},
			}, nil
		case strings.Contains(path, "/cluster/tasks"):
			return []map[string]any{}, nil
		case strings.HasSuffix(path, "/version"):
			if ms.upgraded[sidOfNode(nodeFromPath(path))] {
				return apiVersion{Release: "9.0.3"}, nil
			}
			return apiVersion{Release: "8.4.1"}, nil
		case strings.Contains(path, "/apt/update"):
			out := make([]map[string]any, ms.pending)
			for i := range out {
				out[i] = map[string]any{"Package": fmt.Sprintf("pkg%d", i)}
			}
			return out, nil
		case strings.Contains(path, "/subscription"):
			return map[string]any{"status": "active"}, nil
		case strings.Contains(path, "/tasks/"):
			return map[string]any{"status": "stopped", "exitstatus": "OK"}, nil
		case strings.HasSuffix(path, "/status"):
			// 节点状态:重启后 uptime 重置(供 waitNodeBack 的真实重启校验)。
			uptime := 1000
			if ms.rebooted[nodeFromPath(path)] {
				uptime = 30
			}
			return map[string]any{"kversion": "Linux 6.14.8-1-pve", "uptime": uptime}, nil
		}
		return nil, fmt.Errorf("unexpected GET %s", path)
	}
	fc.postFn = func(path string) (any, error) {
		ms.mu.Lock()
		defer ms.mu.Unlock()
		node := nodeFromPath(path)
		switch {
		case strings.HasSuffix(path, "/nodes/"+node+"/status"):
			ms.rebootCalls = append(ms.rebootCalls, node)
			ms.rebooted[node] = true
			ms.downNode = node
			return "UPID:" + node + ":0001:1:srvreload:0:", nil
		case strings.Contains(path, "/status/shutdown"):
			ms.shutdownCalls = append(ms.shutdownCalls, path)
			return "UPID:" + node + ":0002:1:qmshutdown:101:", nil
		case strings.Contains(path, "/apt/update"), strings.Contains(path, "/apt/upgrade"):
			return "UPID:" + node + ":0003:1:aptupdate:0:", nil
		}
		return nil, fmt.Errorf("unexpected POST %s", path)
	}
	fr.runFn = func(sid string, cmd []string) (*target.ExecResult, error) {
		joined := strings.Join(cmd, " ")
		ms.mu.Lock()
		defer ms.mu.Unlock()
		switch {
		case strings.Contains(joined, "##PWFILE"):
			content := majorSourcesBookworm
			if ms.sourcesTrixe[sid] {
				content = strings.ReplaceAll(content, "bookworm", "trixie")
			}
			content += ms.dumpExtra
			return &target.ExecResult{ExitCode: 0, Stdout: "##PWFILE:/etc/apt/sources.list\n" + content + "\n##PWEND\n"}, nil
		case strings.Contains(joined, "/etc/os-release"):
			return &target.ExecResult{ExitCode: 0, Stdout: "ID=debian\nVERSION_CODENAME=bookworm\n"}, nil
		case strings.HasPrefix(joined, "df "):
			return &target.ExecResult{ExitCode: 0, Stdout: "Avail\n20000000000\n"}, nil
		case strings.Contains(joined, "test -f /etc/pve/ceph.conf"):
			if ms.ceph {
				return &target.ExecResult{ExitCode: 0}, nil
			}
			return &target.ExecResult{ExitCode: 1}, fmt.Errorf("%w: test 退出码 1", ErrProbe)
		case strings.HasPrefix(joined, "pve8to9"):
			return &target.ExecResult{ExitCode: 0, Stdout: "PASS: ok.\nPASS: ok2.\nTOTAL: 2 PASSED, 0 SKIPPED, 0 WARNINGS, 0 FAILURES\n"}, nil
		case joined == "pveversion":
			return &target.ExecResult{ExitCode: 0, Stdout: "pve-manager/9.0.3/abc (running kernel: 6.14.8-1-pve)"}, nil
		case strings.Contains(joined, "dist-upgrade"):
			ms.distUpgradeArg = append([]string(nil), cmd...)
			ms.distUpgradeCalls[sid]++
			if ms.blockUpgrade != nil {
				ch := ms.blockUpgrade
				ms.mu.Unlock()
				<-ch
				ms.mu.Lock()
			}
			if ms.failUpgrade[sid] {
				return &target.ExecResult{ExitCode: 1, Stderr: "E: broken packages"}, fmt.Errorf("%w: apt-get 退出码 1", ErrProbe)
			}
			ms.upgraded[sid] = true
			return &target.ExecResult{ExitCode: 0, Stdout: "dist-upgrade 完成"}, nil
		case strings.HasPrefix(joined, "mv -f"):
			ms.sourcesTrixe[sid] = true
			return &target.ExecResult{ExitCode: 0}, nil
		default:
			return &target.ExecResult{ExitCode: 0}, nil
		}
	}
}

// seedMajorCluster 建 ready 集群 + 两节点登记机器。
func seedMajorCluster(t *testing.T, s *service, ft *fakeTarget) *Cluster {
	t.Helper()
	cl := seedCreateCluster(t, s, ft)
	poolID, _, err := s.ensurePool(context.Background(), cl.ID, cl.Name)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	ft.servers["node2"] = &target.Server{ID: "node2", Name: "pve2", Host: "10.1.0.12", Port: 22, User: "root", PoolID: poolID, Kind: target.KindPVENode}
	return cl
}

func waitMajorDone(t *testing.T, s *service, id string) MajorUpgradeStatus {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.MajorUpgradeStatus(context.Background(), id)
		if err != nil {
			t.Fatalf("MajorUpgradeStatus: %v", err)
		}
		if st.Done {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("大版本任务 8s 未停止: %+v", func() MajorUpgradeStatus { st, _ := s.MajorUpgradeStatus(context.Background(), id); return st }())
	return MajorUpgradeStatus{}
}

func waitMajorNodeState(t *testing.T, s *service, id, node, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.MajorUpgradeStatus(context.Background(), id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		for _, n := range st.Nodes {
			if n.Node == node && n.State == state {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("节点 %s 未进入 %s", node, state)
}

// ---- 预检 -------------------------------------------------------------------------------

func TestMajorPreflightPassAndBlock(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedMajorCluster(t, s, ft)
	ms := newMajorState()
	ms.wire(fc, fr)
	ctx := context.Background()

	rep, err := s.MajorPreflight(ctx, cl.ID, nil)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if rep.Blocked || rep.CephDetected || len(rep.Nodes) != 2 {
		t.Fatalf("rep = %+v", rep)
	}
	// 两节点 + 自托管警告须 ack。
	if len(rep.AcksRequired) < 2 {
		t.Fatalf("acks = %v", rep.AcksRequired)
	}
	for _, np := range rep.Nodes {
		if np.Version != "8.4.1" || np.Codename != "bookworm" || np.DiskFreeGB < 5 || np.Pve8to9 == nil {
			t.Fatalf("node preflight = %+v", np)
		}
	}

	// Ceph → 阻断。
	ms.mu.Lock()
	ms.ceph = true
	ms.mu.Unlock()
	rep, err = s.MajorPreflight(ctx, cl.ID, nil)
	if err != nil || !rep.Blocked || !rep.CephDetected {
		t.Fatalf("ceph rep = %+v / %v", rep, err)
	}

	// 待更新 > 0 → 阻断。
	ms.mu.Lock()
	ms.ceph = false
	ms.pending = 3
	ms.mu.Unlock()
	rep, _ = s.MajorPreflight(ctx, cl.ID, nil)
	if !rep.Blocked {
		t.Fatal("待更新应阻断")
	}
}

// ---- 启动闸门 ---------------------------------------------------------------------------

func TestStartMajorUpgradeGates(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedMajorCluster(t, s, ft)
	ms := newMajorState()
	ms.wire(fc, fr)
	ctx := context.Background()
	acks := []string{"self-hosting", "two-node-quorum"}

	// 无预检。
	err := s.StartMajorUpgrade(ctx, cl.ID, MajorUpgradeInput{Confirm: cl.Name, Acks: acks})
	if !errors.Is(err, ErrPreflightRequired) {
		t.Fatalf("no preflight = %v", err)
	}
	if _, err := s.MajorPreflight(ctx, cl.ID, nil); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	// 确认不匹配。
	err = s.StartMajorUpgrade(ctx, cl.ID, MajorUpgradeInput{Confirm: "wrong", Acks: acks})
	if !errors.Is(err, ErrConfirmMismatch) {
		t.Fatalf("confirm = %v", err)
	}
	// 警告未 ack。
	err = s.StartMajorUpgrade(ctx, cl.ID, MajorUpgradeInput{Confirm: cl.Name})
	if !errors.Is(err, ErrPreflightRequired) {
		t.Fatalf("acks = %v", err)
	}
	// 只读。
	if err := s.SetReadonly(ctx, cl.ID, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	err = s.StartMajorUpgrade(ctx, cl.ID, MajorUpgradeInput{Confirm: cl.Name, Acks: acks})
	if !errors.Is(err, ErrReadonly) {
		t.Fatalf("readonly = %v", err)
	}
}

// ---- 滚动序列 ---------------------------------------------------------------------------

func TestMajorRollingSequence(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedMajorCluster(t, s, ft)
	ms := newMajorState()
	// 额外注入一条 pvetools 风格的 stable-security 滚动别名行:换源须先钉到
	// 当前代号再升 trixie,终态不得残留 stable(实测教训)。
	ms.dumpExtra = "deb http://security.debian.org/debian-security stable-security main contrib\n"
	ms.wire(fc, fr)
	ctx := context.Background()
	if _, err := s.MajorPreflight(ctx, cl.ID, nil); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	// stable 别名与运行代号不符 → 预检按节点产生两条须 ack 的 repo-suite 警告。
	in := MajorUpgradeInput{Confirm: cl.Name,
		Acks:        []string{"self-hosting", "two-node-quorum", "repo-suite:pve1", "repo-suite:pve2"},
		GuestPolicy: GuestPolicyShutdown}
	if err := s.StartMajorUpgrade(ctx, cl.ID, in); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := waitMajorDone(t, s, cl.ID)
	if st.Status != MajorStatusDone || st.Error != "" {
		t.Fatalf("st = %+v", st)
	}
	if len(st.Nodes) != 2 {
		t.Fatalf("nodes = %+v", st.Nodes)
	}
	for _, n := range st.Nodes {
		if n.State != MajorNodeDone {
			t.Fatalf("node = %+v", n)
		}
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if !ms.upgraded["node1"] || !ms.upgraded["node2"] {
		t.Fatalf("upgraded = %v", ms.upgraded)
	}
	if len(ms.rebootCalls) != 2 {
		t.Fatalf("reboot = %v", ms.rebootCalls)
	}
	if len(ms.shutdownCalls) != 1 { // pve1 的运行中 guest 被关停
		t.Fatalf("shutdown = %v", ms.shutdownCalls)
	}
	// dist-upgrade argv:noninteractive + confdef/confold。
	joined := strings.Join(ms.distUpgradeArg, " ")
	if !strings.Contains(joined, "DEBIAN_FRONTEND=noninteractive") ||
		!strings.Contains(joined, "--force-confdef") || !strings.Contains(joined, "--force-confold") {
		t.Fatalf("dist-upgrade argv = %v", ms.distUpgradeArg)
	}
	// 换源落盘(mv)。
	if !ms.sourcesTrixe["node1"] || !ms.sourcesTrixe["node2"] {
		t.Fatalf("sources = %v", ms.sourcesTrixe)
	}
	// stable 别名先钉 bookworm 再随全局升 trixie:终态无 stable 残留、security 行正确。
	var mainFile string
	for _, w := range fr.writes {
		if w.path == "/etc/apt/sources.list.pw-tmp" {
			mainFile = w.content
		}
	}
	if mainFile == "" || strings.Contains(mainFile, "stable") {
		t.Fatalf("stable 别名未钉死:\n%s", mainFile)
	}
	if !strings.Contains(mainFile, "trixie-security") {
		t.Fatalf("security 行未随 trixie 重写:\n%s", mainFile)
	}
}

func TestMajorFailFastAndRetry(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedMajorCluster(t, s, ft)
	ms := newMajorState()
	ms.failUpgrade["node2"] = true
	ms.wire(fc, fr)
	ctx := context.Background()
	if _, err := s.MajorPreflight(ctx, cl.ID, nil); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	acks := []string{"self-hosting", "two-node-quorum"}
	if err := s.StartMajorUpgrade(ctx, cl.ID, MajorUpgradeInput{Confirm: cl.Name, Acks: acks}); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := waitMajorDone(t, s, cl.ID)
	if st.Status != MajorStatusPaused {
		t.Fatalf("应 fail-fast 暂停: %+v", st)
	}
	if st.Nodes[0].State != MajorNodeDone || st.Nodes[1].State != MajorNodeFailed {
		t.Fatalf("nodes = %+v", st.Nodes)
	}
	// 修复后重试失败节点 → 完成。
	ms.mu.Lock()
	ms.failUpgrade["node2"] = false
	ms.mu.Unlock()
	if err := s.RetryMajorNode(ctx, cl.ID, "pve2", cl.Name); err != nil {
		t.Fatalf("retry: %v", err)
	}
	st = waitMajorDone(t, s, cl.ID)
	if st.Status != MajorStatusDone {
		t.Fatalf("retry 后应完成: %+v", st)
	}
}

func TestMajorInterruptedAndResume(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedMajorCluster(t, s, ft)
	ms := newMajorState()
	ms.wire(fc, fr)
	ctx := context.Background()

	// pve1 在中断前已实际升级完成(fake 状态与 DB done 对齐)。
	ms.upgraded["node1"] = true
	ms.sourcesTrixe["node1"] = true
	// 手工落一个 running 任务(模拟控制端重启前)。
	j := &majorJob{
		id: "job-interrupted", clusterID: cl.ID, running: true, status: MajorStatusRunning,
		guestPolicy: GuestPolicyWarn, nodeOrder: []string{"pve1", "pve2"},
		nodes: []MajorNodeStatus{
			{Node: "pve1", Seq: 0, State: MajorNodeDone},
			{Node: "pve2", Seq: 1, State: MajorNodeUpgrading},
		},
	}
	if err := s.insertMajorJob(ctx, j); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 新服务实例(同 DB/假件,空内存表)→ 懒加载标 interrupted。
	s2 := &service{db: s.db, vault: s.vault, servers: ft, runner: fr, clientF: s.clientF}
	st, err := s2.MajorUpgradeStatus(ctx, cl.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Status != MajorStatusInterrupted || !st.Done {
		t.Fatalf("应 interrupted: %+v", st)
	}
	// 人工续跑:跳过已完成 pve1,续跑 pve2。
	if err := s2.ResumeMajorUpgrade(ctx, cl.ID, cl.Name); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st = waitMajorDone(t, s2, cl.ID)
	if st.Status != MajorStatusDone {
		t.Fatalf("resume 后应完成: %+v", st)
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.distUpgradeCalls["node1"] != 0 {
		t.Fatalf("已完成节点不应重跑 dist-upgrade: %v", ms.distUpgradeCalls)
	}
	if ms.distUpgradeCalls["node2"] != 1 {
		t.Fatalf("pve2 应续跑一次: %v", ms.distUpgradeCalls)
	}
}

func TestMajorAbortAtStepBoundary(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedMajorCluster(t, s, ft)
	ms := newMajorState()
	ms.blockUpgrade = make(chan struct{})
	ms.wire(fc, fr)
	ctx := context.Background()
	if _, err := s.MajorPreflight(ctx, cl.ID, nil); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	acks := []string{"self-hosting", "two-node-quorum"}
	if err := s.StartMajorUpgrade(ctx, cl.ID, MajorUpgradeInput{Confirm: cl.Name, Acks: acks}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 卡在 dist-upgrade 时请求中止。
	waitMajorNodeState(t, s, cl.ID, "pve1", MajorNodeUpgrading)
	if err := s.AbortMajorUpgrade(ctx, cl.ID); err != nil {
		t.Fatalf("abort: %v", err)
	}
	close(ms.blockUpgrade)
	st := waitMajorDone(t, s, cl.ID)
	if st.Status != MajorStatusPaused || !st.AbortRequested {
		t.Fatalf("应中止暂停: %+v", st)
	}
	// dist-upgrade 完成后才停:节点停在 upgrading,未重启。
	if st.Nodes[0].State != MajorNodeUpgrading {
		t.Fatalf("node = %+v", st.Nodes[0])
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.rebootCalls) != 0 {
		t.Fatalf("中止后不应重启: %v", ms.rebootCalls)
	}
}

// TestMajorPreflightSkipsAlready9xNode 已 9.x 节点(混合集群)不因 trixie 代号/待更新把预检误判 FAIL。
func TestMajorPreflightSkipsAlready9xNode(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedMajorCluster(t, s, ft)
	ms := newMajorState()
	ms.wire(fc, fr)
	ms.upgraded["node1"] = true
	ms.sourcesTrixe["node1"] = true
	ctx := context.Background()

	rep, err := s.MajorPreflight(ctx, cl.ID, nil)
	if err != nil || rep.Blocked {
		t.Fatalf("混合集群预检不应阻断: %+v / %v", rep, err)
	}
	for _, np := range rep.Nodes {
		if np.Node != "pve1" {
			continue
		}
		if len(np.Checks) != 2 { // online + version
			t.Fatalf("9.x 节点应提前返回: %+v", np.Checks)
		}
		for _, ck := range np.Checks {
			if ck.Level == "fail" {
				t.Fatalf("9.x 节点不应有 fail: %+v", ck)
			}
		}
	}
	cn, err := fetchClusterNodes(ctx, fc)
	if err != nil {
		t.Fatalf("cluster nodes: %v", err)
	}
	nodes, err := s.majorTargetNodes(ctx, fc, cn, nil)
	if err != nil || len(nodes) != 1 || nodes[0] != "pve2" {
		t.Fatalf("目标节点应仅 pve2: %v / %v", nodes, err)
	}
}

// TestMajorTargetNodesRejectsOffline 默认目标含离线节点 → 拒绝开始(避免静默漏升级)。
func TestMajorTargetNodesRejectsOffline(t *testing.T) {
	s, _, _, fc := newTestService(t)
	fc.getFn = func(path string) (any, error) {
		if strings.HasSuffix(path, "/version") {
			return apiVersion{Release: "8.4.1"}, nil
		}
		return nil, fmt.Errorf("unexpected GET %s", path)
	}
	cn := &clusterNodeList{Nodes: []NodeStatus{
		{Name: "pve1", Online: true},
		{Name: "pve2", Online: false},
	}}
	_, err := s.majorTargetNodes(context.Background(), fc, cn, nil)
	if !errors.Is(err, ErrPreflightRequired) {
		t.Fatalf("offline target = %v, want ErrPreflightRequired", err)
	}
}

// TestMajorHostNodeLastAndAwaitingConfirm 宿主节点(用户显式指定):排到最后、guest
// 不自动关停(shutdown 策略下宿主节点仍跳过)、dist-upgrade 完成后 awaiting-reboot
// 暂停;人工确认重启 → rebooting;续跑 → 从 rebooting 收尾到 done。
func TestMajorHostNodeLastAndAwaitingConfirm(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedMajorCluster(t, s, ft)
	ms := newMajorState()
	ms.wire(fc, fr)
	ctx := context.Background()
	if _, err := s.MajorPreflight(ctx, cl.ID, nil); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	// HostNode 不在目标里(未登记/非 8.x)→ 明确报错。
	acks := []string{"self-hosting", "two-node-quorum"}
	err := s.StartMajorUpgrade(ctx, cl.ID, MajorUpgradeInput{Confirm: cl.Name, Acks: acks, HostNode: "pve9"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("host not in target = %v", err)
	}
	in := MajorUpgradeInput{Confirm: cl.Name, Acks: acks, GuestPolicy: GuestPolicyShutdown, HostNode: "pve2"}
	if err := s.StartMajorUpgrade(ctx, cl.ID, in); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := waitMajorDone(t, s, cl.ID)
	if st.Status != MajorStatusPaused {
		t.Fatalf("宿主节点应暂停等人工确认: %+v", st)
	}
	if len(st.Nodes) != 2 || st.Nodes[1].Node != "pve2" || st.Nodes[1].State != MajorNodeAwaitingReboot {
		t.Fatalf("宿主应最后且 awaiting: %+v", st.Nodes)
	}
	if st.Nodes[0].State != MajorNodeDone {
		t.Fatalf("node1 应完成: %+v", st.Nodes[0])
	}
	// guest 处理:shutdown 策略下 node1 的 guest 被关停,宿主节点跳过(不自动关停)。
	ms.mu.Lock()
	shutdownHost := false
	for _, sc := range ms.shutdownCalls {
		if strings.Contains(sc, "/nodes/pve2/") {
			shutdownHost = true
		}
	}
	rebootPve2 := strings.Contains(strings.Join(ms.rebootCalls, ","), "pve2")
	ms.mu.Unlock()
	if shutdownHost {
		t.Fatalf("宿主节点 guest 不应自动关停: %v", ms.shutdownCalls)
	}
	if rebootPve2 {
		t.Fatalf("awaiting 阶段不应发重启: %v", ms.rebootCalls)
	}
	// 人工确认重启(confirm 不匹配 → 拒绝)。
	if err := s.ConfirmMajorReboot(ctx, cl.ID, "wrong"); !errors.Is(err, ErrConfirmMismatch) {
		t.Fatalf("confirm = %v", err)
	}
	if err := s.ConfirmMajorReboot(ctx, cl.ID, cl.Name); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	ms.mu.Lock()
	rebootPve2 = strings.Contains(strings.Join(ms.rebootCalls, ","), "pve2")
	ms.mu.Unlock()
	if !rebootPve2 {
		t.Fatalf("确认后应发重启: %v", ms.rebootCalls)
	}
	// 续跑 → 从 rebooting 收尾到 done(不再关停/不再 dist-upgrade 宿主)。
	distBefore := ms.distUpgradeCalls["node2"]
	if err := s.ResumeMajorUpgrade(ctx, cl.ID, cl.Name); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st = waitMajorDone(t, s, cl.ID)
	if st.Status != MajorStatusDone {
		t.Fatalf("续跑后应完成: %+v", st)
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.distUpgradeCalls["node2"] != distBefore {
		t.Fatal("续跑不应重跑宿主 dist-upgrade")
	}
	if !ms.upgraded["node1"] || !ms.upgraded["node2"] {
		t.Fatalf("upgraded = %v", ms.upgraded)
	}
}

// TestMajorSubsetAckFiltering 只升部分节点:未选节点的节点级警告 ack 不拦本次升级,
// 集群级警告仍须确认;未选节点完全不升级(不换源/不重启)。
func TestMajorSubsetAckFiltering(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedMajorCluster(t, s, ft)
	ms := newMajorState()
	ms.wire(fc, fr)
	ctx := context.Background()
	rep := &MajorPreflightReport{
		ClusterID: cl.ID, At: time.Now().UTC().Format(time.RFC3339), Quorate: true,
		AcksRequired: []string{"self-hosting", "pve8to9-warn:pve2"},
	}
	s.mu.Lock()
	if s.preflights == nil {
		s.preflights = map[string]preflightEntry{}
	}
	s.preflights[cl.ID] = preflightEntry{at: time.Now(), report: rep}
	s.mu.Unlock()
	acks := []string{"self-hosting"}
	// 全集群(不选节点):pve2 的节点级警告未确认 → 拦。
	err := s.StartMajorUpgrade(ctx, cl.ID, MajorUpgradeInput{Confirm: cl.Name, Acks: acks})
	if !errors.Is(err, ErrPreflightRequired) {
		t.Fatalf("full ack filter = %v", err)
	}
	// 只升 pve1:pve2 的节点级警告不拦。
	if err := s.StartMajorUpgrade(ctx, cl.ID, MajorUpgradeInput{Confirm: cl.Name, Acks: acks, Nodes: []string{"pve1"}}); err != nil {
		t.Fatalf("subset start = %v", err)
	}
	st := waitMajorDone(t, s, cl.ID)
	if st.Status != MajorStatusDone || len(st.Nodes) != 1 || st.Nodes[0].Node != "pve1" {
		t.Fatalf("subset st = %+v", st)
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.upgraded["node2"] || len(ms.rebootCalls) != 1 || ms.rebootCalls[0] != "pve1" {
		t.Fatalf("pve2 不应被动: upgraded=%v reboot=%v", ms.upgraded, ms.rebootCalls)
	}
}
