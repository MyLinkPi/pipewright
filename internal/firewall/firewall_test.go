// firewall_test.go 覆盖两步生效状态机:fakeHost 断言命令序列、确认门、回退与保活。
package firewall

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// --- 测试桩 ---

// cmdRes 是 fakeHost 对单条命令的定制结果(nil = 走默认 Linux root 全绿环境)。
type cmdRes struct {
	stdout string
	stderr string
	code   int
}

// fakeHost 是捕获 Run 的假本机(照 platformhttps_test 模式)。
type fakeHost struct {
	calls     [][]string
	resultFor func(cmd []string) *cmdRes
}

func (f *fakeHost) Run(_ context.Context, name string, args []string) (string, string, int, error) {
	cmd := append([]string{name}, args...)
	f.calls = append(f.calls, cmd)
	if f.resultFor != nil {
		if r := f.resultFor(cmd); r != nil {
			return r.stdout, r.stderr, r.code, nil
		}
	}
	return defaultLinuxRoot(cmd)
}

// defaultLinuxRoot 把 fake 调成「装好 iptables/ip6tables/Docker 的 root Linux 机器」:
// --version 与 id 探测成功;DOCKER-USER 在;-C 挂钩校验一律「不在」(exit 1,触发 -I)。
func defaultLinuxRoot(cmd []string) (string, string, int, error) {
	switch cmd[0] {
	case "id":
		return "0\n", "", 0, nil
	case "iptables", "ip6tables", "/usr/sbin/iptables", "/sbin/iptables":
		if len(cmd) >= 2 && cmd[1] == "--version" {
			return cmd[0] + " v1.8.7\n", "", 0, nil
		}
		if len(cmd) >= 3 && cmd[1] == "-n" && cmd[2] == "-L" {
			return "", "", 0, nil // DOCKER-USER 在
		}
		if len(cmd) >= 2 && cmd[1] == "-C" {
			return "", "Bad rule (does a matching rule exist in that chain?).", 1, nil
		}
		return "", "", 0, nil
	}
	return "", "", 0, nil
}

// hasCmd 报告是否执行过某条命令(全等)。
func hasCmd(f *fakeHost, want []string) bool {
	for _, c := range f.calls {
		if strings.Join(c, " ") == strings.Join(want, " ") {
			return true
		}
	}
	return false
}

// hasCmdAfter 报告 want 是否出现在 after 之后(用于「先应用新版,回退重放旧版」类断言)。
func hasCmdAfter(f *fakeHost, after, want []string) bool {
	a, w := strings.Join(after, " "), strings.Join(want, " ")
	seenAfter := false
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		if seenAfter && j == w {
			return true
		}
		if j == a {
			seenAfter = true
		}
	}
	return false
}

// linuxEnv 把包级环境探针调成 Linux 非容器(测试结束恢复)。
func linuxEnv(t *testing.T) {
	t.Helper()
	oldGOOS, oldIC := hostGOOS, inContainerMarker
	hostGOOS = "linux"
	inContainerMarker = func() bool { return false }
	t.Cleanup(func() { hostGOOS, inContainerMarker = oldGOOS, oldIC })
}

// newSvc 构造接 fakeHost 的服务(storetest 内存库;SelfIP 固定返回 selfIP)。
func newSvc(t *testing.T, h Host, selfIP string) (Service, *sql.DB) {
	t.Helper()
	db := storetest.OpenDB(t)
	return New(db, Options{Host: h, SelfIP: func() string { return selfIP }}), db
}

// --- 状态机 ---

func TestSaveDraftRunsNoCommands(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{}
	svc, _ := newSvc(t, f, "10.126.126.7")
	p := []int{9090, 80}
	st, err := svc.SaveDraft(context.Background(), DraftInput{Ports: &p, ExtraCIDRs: &[]string{"192.168.30.0/24"}})
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("保存草稿不得碰机器,却执行了: %v", f.calls)
	}
	if joinInts(st.Ports) != "80,9090" || len(st.ExtraCIDRs) != 1 {
		t.Fatalf("草稿回读不符: %+v", st)
	}
}

func TestApplyPendingGolden(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{}
	svc, _ := newSvc(t, f, "10.126.126.7")
	st, err := svc.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if st.Phase != PhasePending {
		t.Fatalf("phase = %s, want pending", st.Phase)
	}
	if strings.Join(st.PendingCIDRs, ",") != "10.126.126.0/24" {
		t.Fatalf("pending cidrs = %v", st.PendingCIDRs)
	}
	if joinInts(st.PendingPorts) != "80,443,5000,5001,8080" {
		t.Fatalf("pending ports = %v", st.PendingPorts)
	}
	// 三处挂钩齐备(v4 INPUT、v4 DOCKER-USER、v6 INPUT),链规则带端口匹配。
	for _, want := range [][]string{
		{"iptables", "-N", chainInput},
		{"iptables", "-A", chainInput, "-p", "tcp", "-m", "multiport", "--dports", "80,443,5000,5001,8080", "-s", "10.126.126.0/24", "-j", "ACCEPT"},
		{"iptables", "-I", "INPUT", "1", "-m", "comment", "--comment", ruleComment, "-j", chainInput},
		{"iptables", "-A", chainDocker, "-p", "tcp", "-m", "multiport", "--dports", "80,443,5000,5001,8080", "-j", "DROP"},
		{"iptables", "-I", "DOCKER-USER", "1", "-m", "comment", "--comment", ruleComment, "-j", chainDocker},
		{"ip6tables", "-N", chainInput},
		{"ip6tables", "-I", "INPUT", "1", "-m", "comment", "--comment", ruleComment, "-j", chainInput},
	} {
		if !hasCmd(f, want) {
			t.Fatalf("缺少命令: %v\n全部: %v", want, f.calls)
		}
	}
}

func TestApplyMeshUnreachable(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{}
	svc, _ := newSvc(t, f, "") // 拿不到 SelfIP 且无补充网段
	if _, err := svc.Apply(context.Background()); !errors.Is(err, ErrMeshUnreachable) {
		t.Fatalf("err = %v, want ErrMeshUnreachable", err)
	}
	for _, c := range f.calls { // 探测命令可以有,规则命令一条不许有
		if len(c) > 1 {
			switch c[1] {
			case "-N", "-F", "-A", "-I", "-D":
				t.Fatalf("不应执行任何规则命令: %v", c)
			}
		}
	}
}

func TestApplyExtraCIDRsOnly(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{}
	svc, _ := newSvc(t, f, "")
	if _, err := svc.SaveDraft(context.Background(), DraftInput{ExtraCIDRs: &[]string{"192.168.30.0/24"}}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	st, err := svc.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Join(st.PendingCIDRs, ",") != "192.168.30.0/24" {
		t.Fatalf("cidrs = %v", st.PendingCIDRs)
	}
}

func TestApplyNoDockerSkipsDockerChain(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{resultFor: func(cmd []string) *cmdRes {
		if len(cmd) >= 3 && cmd[0] == "iptables" && cmd[1] == "-n" && cmd[2] == "-L" {
			return &cmdRes{stderr: "No such chain.", code: 1} // DOCKER-USER 不在
		}
		return nil
	}}
	svc, _ := newSvc(t, f, "10.126.126.7")
	st, err := svc.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(st.StatusDetail, "DOCKER-USER") {
		t.Fatalf("detail 应记 DOCKER-USER note: %q", st.StatusDetail)
	}
	for _, c := range f.calls {
		if len(c) > 1 && c[1] == chainDocker {
			t.Fatalf("无 Docker 时不应动 DOCKER-USER 侧链: %v", c)
		}
	}
}

func TestApplyFailureCleansUp(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{resultFor: func(cmd []string) *cmdRes {
		if len(cmd) >= 3 && cmd[1] == "-A" && cmd[2] == chainInput && len(cmd) > 8 && cmd[len(cmd)-1] == "DROP" {
			return &cmdRes{stderr: "iptables: Some error.", code: 2}
		}
		return nil
	}}
	svc, _ := newSvc(t, f, "10.126.126.7")
	_, err := svc.Apply(context.Background())
	if !errors.Is(err, ErrApply) {
		t.Fatalf("err = %v, want ErrApply", err)
	}
	if !hasCmd(f, []string{"iptables", "-D", "INPUT", "-m", "comment", "--comment", ruleComment, "-j", chainInput}) {
		t.Fatal("失败后应清理 INPUT 挂钩")
	}
	if !hasCmd(f, []string{"iptables", "-X", chainInput}) {
		t.Fatal("失败后应删除专用链")
	}
	st, _ := svc.Get(context.Background())
	if st.Phase != PhaseOff {
		t.Fatalf("失败后 phase 应保持 off, got %s", st.Phase)
	}
}

// --- 确认门 ---

func TestConfirmGate(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{}
	svc, _ := newSvc(t, f, "10.126.126.7")
	ctx := context.Background()

	// phase=off 时确认 → ErrNotPending。
	if _, err := svc.Confirm(ctx, "127.0.0.1"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("err = %v, want ErrNotPending", err)
	}
	if _, err := svc.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ok, err := svc.AllowsConfirm(ctx, "10.126.126.3")
	if err != nil || !ok {
		t.Fatalf("pending 期间 AllowsConfirm(mesh) = %v, %v", ok, err)
	}
	if ok, _ := svc.AllowsConfirm(ctx, "8.8.8.8"); ok {
		t.Fatal("外部 IP 不应在放行集")
	}
	// 外部真实 IP(经 nginx 读到的)→ 拒绝;真实 IP 的解析在 httpapi clientIP,
	// 此处验证域内成员判定。
	if _, err := svc.Confirm(ctx, "8.8.8.8"); !errors.Is(err, ErrIPNotAllowed) {
		t.Fatalf("err = %v, want ErrIPNotAllowed", err)
	}
	// mesh 网段内 → 固化。
	st, err := svc.Confirm(ctx, "10.126.126.9")
	if err != nil {
		t.Fatalf("Confirm(mesh): %v", err)
	}
	if st.Phase != PhaseActive || joinInts(st.ConfirmedPorts) != "80,443,5000,5001,8080" {
		t.Fatalf("固化后状态不符: %+v", st)
	}
}

// --- 回退 ---

func TestRollbackRestoresConfirmed(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{}
	svc, _ := newSvc(t, f, "10.126.126.7")
	ctx := context.Background()

	if _, err := svc.Apply(ctx); err != nil {
		t.Fatalf("Apply#1: %v", err)
	}
	if _, err := svc.Confirm(ctx, "127.0.0.1"); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	// 改草稿端口再应用 → pending(新版);回退应重放上一个固化版(旧端口)。
	p := []int{9090}
	if _, err := svc.SaveDraft(ctx, DraftInput{Ports: &p}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := svc.Apply(ctx); err != nil {
		t.Fatalf("Apply#2: %v", err)
	}
	f.calls = nil
	st, err := svc.Rollback(ctx)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if st.Phase != PhaseActive {
		t.Fatalf("回退后 phase = %s, want active(重放固化版)", st.Phase)
	}
	if !hasCmd(f, []string{"iptables", "-A", chainInput, "-p", "tcp", "-m", "multiport", "--dports", "80,443,5000,5001,8080", "-s", "10.126.126.0/24", "-j", "ACCEPT"}) {
		t.Fatalf("回退应重放固化版端口规则, calls: %v", f.calls)
	}
}

func TestRollbackNoConfirmedClears(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{}
	svc, _ := newSvc(t, f, "10.126.126.7")
	ctx := context.Background()
	if _, err := svc.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	f.calls = nil
	st, err := svc.Rollback(ctx)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if st.Phase != PhaseOff {
		t.Fatalf("无固化版回退后 phase = %s, want off", st.Phase)
	}
	if !hasCmd(f, []string{"iptables", "-D", "INPUT", "-m", "comment", "--comment", ruleComment, "-j", chainInput}) {
		t.Fatal("回退应拆除 INPUT 挂钩")
	}
}

func TestDisableIdempotent(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{}
	svc, _ := newSvc(t, f, "10.126.126.7")
	ctx := context.Background()
	if _, err := svc.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for i := 0; i < 2; i++ { // 重复禁用不报错
		st, err := svc.Disable(ctx)
		if err != nil {
			t.Fatalf("Disable#%d: %v", i+1, err)
		}
		if st.Phase != PhaseOff || len(st.ConfirmedPorts) != 0 || len(st.PendingPorts) != 0 {
			t.Fatalf("禁用后状态不符: %+v", st)
		}
	}
	st, _ := svc.Get(ctx)
	if joinInts(st.Ports) == "" {
		t.Fatal("禁用应保留草稿端口")
	}
}

// --- 保活(平台重启拆 pending / 超时回退 / 漂移重放) ---

// asService 取实现句柄(同包测试特权;状态机接口不够表达 reconcileOnce)。
func asService(svc Service) *service { return svc.(*service) }

func TestReconcileTeardownPendingOnStart(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{}
	svc, db := newSvc(t, f, "10.126.126.7")
	ctx := context.Background()
	if _, err := svc.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 平台重启:同一 DB 上构造新实例(livePending=false),保活首拍应把未确认的
	// 临时规则拆掉(无固化版 → off)。
	f.calls = nil
	svc2 := New(db, Options{Host: f, SelfIP: func() string { return "10.126.126.7" }})
	asService(svc2).reconcileOnce(ctx)
	st, _ := svc.Get(ctx)
	if st.Phase != PhaseOff {
		t.Fatalf("重启后 phase = %s, want off(未确认临时规则应回退)", st.Phase)
	}
	if !hasCmd(f, []string{"iptables", "-X", chainInput}) {
		t.Fatal("重启收敛应清理专用链")
	}
}

func TestReconcilePendingTimeoutRollsBack(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{}
	svc, db := newSvc(t, f, "10.126.126.7")
	ctx := context.Background()
	if _, err := svc.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 把 pending 时间拨到 11 分钟前(直改库模拟超时)。
	past := time.Now().UTC().Add(-11 * time.Minute).Format(time.RFC3339)
	if _, err := db.ExecContext(ctx, `UPDATE firewall_guard SET pending_applied_at = ? WHERE id = 'default'`, past); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	asService(svc).reconcileOnce(ctx)
	st, _ := svc.Get(ctx)
	if st.Phase != PhaseOff {
		t.Fatalf("超时后 phase = %s, want off", st.Phase)
	}
	if st.LastResult != resultReverted {
		t.Fatalf("lastResult = %s, want reverted", st.LastResult)
	}
}

func TestReconcileActiveReplaysOnDrift(t *testing.T) {
	linuxEnv(t)
	f := &fakeHost{}
	svc, _ := newSvc(t, f, "10.126.126.7")
	ctx := context.Background()
	if _, err := svc.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := svc.Confirm(ctx, "127.0.0.1"); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	f.calls = nil
	asService(svc).reconcileOnce(ctx) // -C 全部「不在」(fake 默认)→ 漂移 → 重放
	if !hasCmd(f, []string{"iptables", "-A", chainInput, "-p", "tcp", "-m", "multiport", "--dports", "80,443,5000,5001,8080", "-j", "DROP"}) {
		t.Fatalf("漂移后应重放固化规则, calls: %v", f.calls)
	}
	st, _ := svc.Get(ctx)
	if st.Phase != PhaseActive {
		t.Fatalf("phase = %s, want active", st.Phase)
	}
}

// --- 前置检测 ---

func TestPreconditions(t *testing.T) {
	ctx := context.Background()

	t.Run("not-linux", func(t *testing.T) {
		hostGOOS = "windows"
		t.Cleanup(func() { hostGOOS = "linux" })
		svc, _ := newSvc(t, &fakeHost{}, "")
		if _, err := svc.Apply(ctx); !errors.Is(err, ErrNotLinux) {
			t.Fatalf("err = %v, want ErrNotLinux", err)
		}
	})
	t.Run("in-container", func(t *testing.T) {
		linuxEnv(t)
		inContainerMarker = func() bool { return true }
		svc, _ := newSvc(t, &fakeHost{}, "")
		if _, err := svc.Apply(ctx); !errors.Is(err, ErrInContainer) {
			t.Fatalf("err = %v, want ErrInContainer", err)
		}
	})
	t.Run("no-iptables", func(t *testing.T) {
		linuxEnv(t)
		f := &fakeHost{resultFor: func(cmd []string) *cmdRes {
			if len(cmd) >= 2 && cmd[1] == "--version" {
				return &cmdRes{code: 1}
			}
			return nil
		}}
		svc, _ := newSvc(t, f, "")
		if _, err := svc.Apply(ctx); !errors.Is(err, ErrNoIptables) {
			t.Fatalf("err = %v, want ErrNoIptables", err)
		}
	})
	t.Run("no-privilege", func(t *testing.T) {
		linuxEnv(t)
		f := &fakeHost{resultFor: func(cmd []string) *cmdRes {
			if cmd[0] == "id" {
				return &cmdRes{stdout: "1000\n"}
			}
			if cmd[0] == "sudo" {
				return &cmdRes{stderr: "sudo: a password is required", code: 1}
			}
			return nil
		}}
		svc, _ := newSvc(t, f, "")
		if _, err := svc.Apply(ctx); !errors.Is(err, ErrNoPrivilege) {
			t.Fatalf("err = %v, want ErrNoPrivilege", err)
		}
	})
}

func TestDetectReason(t *testing.T) {
	if (&DetectInfo{Linux: true, Iptables: "iptables", IsRoot: true}).Reason() != "" {
		t.Fatal("全绿时 Reason 应为空")
	}
	if (&DetectInfo{}).Reason() == "" {
		t.Fatal("非 Linux 应有人话原因")
	}
}

// joinInts 拼接端口列表(测试断言用)。
func joinInts(in []int) string { return strings.Join(mapToStrings(in), ",") }

// mapToStrings 逐元素转字符串。
func mapToStrings(in []int) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strconv.Itoa(v)
	}
	return out
}
