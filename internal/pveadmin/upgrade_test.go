package pveadmin

// upgrade_test.go — 日常升级:内核需重启启发式、更新矩阵扇出、串行管线序列、
// readonly/busy 闸门、节点重启输入确认。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSameKernelVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"6.8.12-9-pve", "6.8.12-9", true},
		{"Linux 6.8.12-9-pve (2025-03-11)", "6.8.12-9", true},
		{"6.8.12-9-pve", "6.8.12-10", false},
		{"6.8.12-10-pve", "6.8.12-1", false}, // 边界安全:不误判前缀
		{"6.8.12-9", "6.8.12-9", true},
	}
	for _, c := range cases {
		if got := sameKernelVersion(c.a, c.b); got != c.want {
			t.Errorf("sameKernelVersion(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if !kernelNeedsReboot([]apiAptVersion{{Package: "proxmox-kernel-6.8", Version: "6.8.12-10", Running: "6.8.12-9-pve"}}) {
		t.Error("应判定需重启")
	}
	if kernelNeedsReboot([]apiAptVersion{{Package: "pve-manager", Version: "8.4.1"}}) {
		t.Error("无 Running 字段不应判定需重启")
	}
}

// wireUpdatesFake 接更新矩阵/日常升级响应。
func wireUpdatesFake(fc *fakePVEClient) {
	fc.getFn = func(path string) (any, error) {
		switch {
		case strings.Contains(path, "/cluster/status"):
			return []map[string]any{
				{"type": "cluster", "name": "pve-prod", "quorate": 1},
				{"type": "node", "name": "pve1", "ip": "10.1.0.11", "online": 1},
				{"type": "node", "name": "pve2", "ip": "10.1.0.12", "online": 0},
			}, nil
		case strings.HasSuffix(path, "/version"):
			return apiVersion{Release: "8.4.1"}, nil
		case strings.Contains(path, "/tasks/"):
			return map[string]any{"status": "stopped", "exitstatus": "OK"}, nil
		case strings.HasSuffix(path, "/status"):
			return map[string]any{"kversion": "Linux 6.8.12-9-pve"}, nil
		case strings.Contains(path, "/apt/update"):
			return []map[string]any{{"Package": "pve-manager", "Version": "8.4.2", "OldVersion": "8.4.1"}}, nil
		case strings.Contains(path, "/apt/versions"):
			return []map[string]any{{"Package": "proxmox-kernel-6.8", "Version": "6.8.12-10", "Running": "6.8.12-9-pve"}}, nil
		case strings.Contains(path, "/subscription"):
			return map[string]any{"status": "active"}, nil
		}
		return nil, fmt.Errorf("unexpected GET %s", path)
	}
	fc.postFn = func(path string) (any, error) {
		if strings.Contains(path, "/apt/update") || strings.Contains(path, "/apt/upgrade") || strings.HasSuffix(path, "/status") {
			return "UPID:pve1:0001:1:aptupdate:0:", nil
		}
		return nil, fmt.Errorf("unexpected POST %s", path)
	}
}

func TestListUpdates(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireUpdatesFake(fc)

	ov, err := s.ListUpdates(context.Background(), cl.ID)
	if err != nil {
		t.Fatalf("ListUpdates: %v", err)
	}
	if !ov.Quorate || len(ov.Items) != 2 {
		t.Fatalf("ov = %+v", ov)
	}
	var pve1, pve2 *NodeUpdates
	for i := range ov.Items {
		if ov.Items[i].Node == "pve1" {
			pve1 = &ov.Items[i]
		} else {
			pve2 = &ov.Items[i]
		}
	}
	if pve1 == nil || pve1.Version != "8.4.1" || pve1.Pending != 1 || !pve1.NeedsReboot || pve1.Subscription != "active" {
		t.Fatalf("pve1 = %+v", pve1)
	}
	if pve2 == nil || pve2.Online || pve2.Error == "" {
		t.Fatalf("离线节点应错误隔离: %+v", pve2)
	}
}

func TestUpgradePipelineSequence(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireUpdatesFake(fc)

	if err := s.StartUpgrade(context.Background(), cl.ID, UpgradeInput{}); err != nil {
		t.Fatalf("StartUpgrade: %v", err)
	}
	st := waitUpgradeDone(t, s, cl.ID)
	if st.Error != "" || st.OK != 1 || st.Failed != 0 {
		t.Fatalf("st = %+v", st)
	}
	it := st.Items[0]
	if it.Node != "pve1" || !it.OK || it.Updated != 1 || !it.NeedsReboot {
		t.Fatalf("item = %+v", it)
	}
	// 序列:刷新索引(API,升级前后各一次)→ SSH apt-get dist-upgrade(严格串行,单节点)。
	refreshPosts := 0
	for _, p := range fc.posts {
		if strings.HasSuffix(p, "/apt/update") {
			refreshPosts++
		}
	}
	// 刷新两次:升级前一次 + 升级后重刷(否则矩阵显示升级前的旧待更新数)。
	if refreshPosts < 2 {
		t.Fatalf("刷新索引应升级前后各一次: %d / %v", refreshPosts, fc.posts)
	}
	sawUpgrade := false
	for _, run := range fr.runs {
		joined := strings.Join(run, " ")
		if strings.Contains(joined, "DEBIAN_FRONTEND=noninteractive") &&
			strings.Contains(joined, "--force-confdef") && strings.Contains(joined, "dist-upgrade") {
			sawUpgrade = true
		}
	}
	if !sawUpgrade {
		t.Fatalf("SSH dist-upgrade 未执行: %v", fr.runs)
	}
}

func waitUpgradeDone(t *testing.T, s *service, id string) UpgradeStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.UpgradeStatus(context.Background(), id)
		if err != nil {
			t.Fatalf("UpgradeStatus: %v", err)
		}
		if st.Done {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("升级任务 5s 未完成")
	return UpgradeStatus{}
}

func TestUpgradeReadonlyAndBusy(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireUpdatesFake(fc)
	ctx := context.Background()

	if err := s.SetReadonly(ctx, cl.ID, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := s.StartUpgrade(ctx, cl.ID, UpgradeInput{}); !errors.Is(err, ErrReadonly) {
		t.Fatalf("readonly = %v", err)
	}
	if err := s.SetReadonly(ctx, cl.ID, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	// busy:第一个任务卡在 apt/update GET,第二个启动应 ErrBusy。
	gate := make(chan struct{})
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/apt/update") {
			<-gate
		}
		return wireUpdatesGet(path)
	}
	if err := s.StartUpgrade(ctx, cl.ID, UpgradeInput{}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := s.StartUpgrade(ctx, cl.ID, UpgradeInput{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy = %v", err)
	}
	close(gate)
	waitUpgradeDone(t, s, cl.ID)
}

// wireUpdatesGet 是 wireUpdatesFake 的 GET 路由(供 busy 测试包装)。
func wireUpdatesGet(path string) (any, error) {
	fc := &fakePVEClient{}
	wireUpdatesFake(fc)
	return fc.getFn(path)
}

func TestRebootNode(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireUpdatesFake(fc)
	ctx := context.Background()

	// 输入确认不匹配。
	if _, err := s.RebootNode(ctx, cl.ID, "pve1", "wrong"); !errors.Is(err, ErrConfirmMismatch) {
		t.Fatalf("confirm = %v", err)
	}
	// 只读闸门。
	if err := s.SetReadonly(ctx, cl.ID, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := s.RebootNode(ctx, cl.ID, "pve1", "pve1"); !errors.Is(err, ErrReadonly) {
		t.Fatalf("readonly = %v", err)
	}
	if err := s.SetReadonly(ctx, cl.ID, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	// 成功:command=reboot。
	ref, err := s.RebootNode(ctx, cl.ID, "pve1", "pve1")
	if err != nil || ref.Node != "pve1" || !strings.HasPrefix(ref.UPID, "UPID:") {
		t.Fatalf("reboot = %+v / %v", ref, err)
	}
	found := false
	for i, p := range fc.posts {
		if strings.HasSuffix(p, "/nodes/pve1/status") && fc.postForms[i].Get("command") == "reboot" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reboot 表单不正确: %v / %v", fc.posts, fc.postForms)
	}
}

func TestResolveUpgradeNodes(t *testing.T) {
	cn := &clusterNodeList{Quorate: true, Nodes: []NodeStatus{
		{Name: "pve1", Online: true}, {Name: "pve2", Online: false}, {Name: "pve3", Online: true},
	}}
	all, err := resolveUpgradeNodes(nil, cn, true)
	if err != nil || len(all) != 2 || all[0] != "pve1" || all[1] != "pve3" {
		t.Fatalf("all = %v / %v", all, err)
	}
	if _, err := resolveUpgradeNodes([]string{"pve2"}, cn, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("offline = %v", err)
	}
	if _, err := resolveUpgradeNodes([]string{"nope"}, cn, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown = %v", err)
	}
	sel, err := resolveUpgradeNodes([]string{"pve3", "pve1", "pve3"}, cn, true)
	if err != nil || len(sel) != 2 || sel[0] != "pve3" {
		t.Fatalf("sel = %v / %v(去重保序)", sel, err)
	}
}

// TestUpgradeRefreshReGetFailureStillUpgrades 刷新后重取待更新数失败时绝不误判「已是最新」。
func TestUpgradeRefreshReGetFailureStillUpgrades(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireUpdatesFake(fc)
	baseGet := fc.getFn
	var updateGets int
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/apt/update") {
			updateGets++
			if updateGets == 2 {
				return nil, fmt.Errorf("%w: transient", ErrConnectFailed)
			}
		}
		return baseGet(path)
	}
	if err := s.StartUpgrade(context.Background(), cl.ID, UpgradeInput{}); err != nil {
		t.Fatalf("StartUpgrade: %v", err)
	}
	st := waitUpgradeDone(t, s, cl.ID)
	if st.OK != 1 || st.Failed != 0 {
		t.Fatalf("st = %+v", st)
	}
	sawUpgrade := false
	for _, run := range fr.runs {
		if strings.Contains(strings.Join(run, " "), "dist-upgrade") {
			sawUpgrade = true
		}
	}
	if !sawUpgrade {
		t.Fatalf("重取待更新失败不应跳过升级: %v", fr.runs)
	}
}

// TestUpgradeRejectsStableAliasSuite pvetools 风格 stable-security(2026-11 起指向
// trixie)的节点:dist-upgrade 前被仓库体检拒绝,绝不带病升级(实测会试图卸载
// proxmox-ve,被 pve-apt-hook 拦截)。
func TestUpgradeRejectsStableAliasSuite(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireUpdatesFake(fc)
	wireAptRepoFake(fr, aptDumpOf(map[string]string{"/etc/apt/sources.list": "deb https://mirrors.ustc.edu.cn/debian bookworm main contrib\n" +
		"deb https://mirrors.ustc.edu.cn/debian-security stable-security main contrib\n" +
		"deb https://mirrors.ustc.edu.cn/proxmox/debian/pve bookworm pve-no-subscription\n"}))
	if err := s.StartUpgrade(context.Background(), cl.ID, UpgradeInput{}); err != nil {
		t.Fatalf("StartUpgrade: %v", err)
	}
	st := waitUpgradeDone(t, s, cl.ID)
	if st.OK != 0 || st.Failed != 1 {
		t.Fatalf("st = %+v", st)
	}
	if !strings.Contains(st.Items[0].Err, "仓库体检未过") || !strings.Contains(st.Items[0].Err, "stable") {
		t.Fatalf("err = %q", st.Items[0].Err)
	}
	for _, run := range fr.runs {
		if strings.Contains(strings.Join(run, " "), "dist-upgrade") {
			t.Fatalf("体检未过不应执行 dist-upgrade: %v", fr.runs)
		}
	}
}
