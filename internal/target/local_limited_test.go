package target

// 本机(控制节点)受限执行(LimitedExecutor)的单元测试。
//
// 覆盖:Capture 合成快照(指纹稳定 / 角色禁用拒绝)、ExecLimited 本机执行(limits 截断、
// 指纹漂移闸门、命令校验、启动失败 127 伪退出码、ctx 取消)与真实 serverID 透传。
// 本地命令沿用 local_test.go 的纪律:只用跨平台必存在的 `go`,不依赖 sh/echo。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeLimitedBase 是「实现了 LimitedExecutor 的 base」:验证真实 serverID 的
// Capture/ExecLimited 逐字透传(main.go 装配把包装实例断言为 LimitedExecutor 注入 opschat)。
type fakeLimitedBase struct {
	fakeBaseService
	captured string
	gotSnap  ConnectionSnapshot
	gotCmd   []string
}

func (f *fakeLimitedBase) Capture(_ context.Context, id string) (ConnectionSnapshot, error) {
	f.captured = id
	return ConnectionSnapshot{Server: Server{ID: id, Name: "web-1"}}, nil
}

func (f *fakeLimitedBase) ExecLimited(_ context.Context, snap ConnectionSnapshot, cmd []string, _ ExecutionLimits) (*LimitedResult, error) {
	f.gotSnap, f.gotCmd = snap, cmd
	return &LimitedResult{Stdout: "ssh-limited", ExitCode: 0}, nil
}

// 编译期断言:本机装饰器实现 LimitedExecutor(cmd/pipewright 把包装实例断言为该接口注入
// opschat,此方法集是装配契约)。
var _ LimitedExecutor = (*localHostService)(nil)

// localLimitedOf 返回包装实例的具体类型(WithLocalHost 的返回类型是 Service 接口,
// Capture/ExecLimited 不在接口上,测试与 main.go 同款用类型断言取用)。
func localLimitedOf(base Service) *localHostService {
	svc, ok := WithLocalHost(base).(*localHostService)
	if !ok {
		panic("WithLocalHost did not return *localHostService")
	}
	return svc
}

// TestLocalLimitedInterfaceAssertion 与 main.go 装配同款断言:包装实例必须能转型为
// LimitedExecutor,否则本机 AI 目标整条链路静默失效。
func TestLocalLimitedInterfaceAssertion(t *testing.T) {
	if _, ok := Service(WithLocalHost(&fakeLimitedBase{})).(LimitedExecutor); !ok {
		t.Fatal("WithLocalHost instance does not satisfy LimitedExecutor")
	}
}

// TestLocalCaptureSyntheticSnapshot Capture 对本机返回合成快照:不查库、指纹跨调用稳定
// (角色开关不进快照,opschat 会话中途不会误触「目标配置漂移」闸门)。
func TestLocalCaptureSyntheticSnapshot(t *testing.T) {
	svc := localLimitedOf(&fakeBaseService{})
	snap, err := svc.Capture(context.Background(), LocalHostID)
	if err != nil {
		t.Fatalf("Capture(local): %v", err)
	}
	if snap.Server.ID != LocalHostID || snap.Server.Name != "本机(控制节点)" {
		t.Fatalf("snapshot server = %+v", snap.Server)
	}
	if snap.Server.CredentialID != "" {
		t.Fatalf("local snapshot must carry no credential, got %q", snap.Server.CredentialID)
	}
	if snap.CredentialVersion != localCredentialVersion {
		t.Fatalf("credential version = %q, want %q", snap.CredentialVersion, localCredentialVersion)
	}
	again, err := svc.Capture(context.Background(), LocalHostID)
	if err != nil {
		t.Fatalf("Capture(local) again: %v", err)
	}
	if snap.Fingerprint() != again.Fingerprint() {
		t.Fatal("local snapshot fingerprint is not stable")
	}
}

// TestLocalCapturePassthrough 真实 serverID 的 Capture 透传 base 的受限采集。
func TestLocalCapturePassthrough(t *testing.T) {
	base := &fakeLimitedBase{}
	svc := localLimitedOf(base)
	if _, err := svc.Capture(context.Background(), "real-1"); err != nil {
		t.Fatalf("Capture(real-1): %v", err)
	}
	if base.captured != "real-1" {
		t.Fatalf("base captured %q, want real-1", base.captured)
	}
}

// TestLocalCaptureUnavailableBase base 未实现 LimitedExecutor 时,真实 serverID 的
// Capture/ExecLimited 明确报 ErrLimitedUnavailable(不静默落库查询)。
func TestLocalCaptureUnavailableBase(t *testing.T) {
	svc := localLimitedOf(&fakeBaseService{})
	if _, err := svc.Capture(context.Background(), "real-1"); !errors.Is(err, ErrLimitedUnavailable) {
		t.Fatalf("Capture(real-1) err = %v, want ErrLimitedUnavailable", err)
	}
	// 手造 real-ID 快照走透传分支:base 未实现 LimitedExecutor → 明确报错(不静默落库查询)。
	approved := ConnectionSnapshot{Server: Server{ID: "real-1", Name: "web-1"}}
	if _, err := svc.ExecLimited(context.Background(), approved, []string{"du"}, ExecutionLimits{}); !errors.Is(err, ErrLimitedUnavailable) {
		t.Fatalf("ExecLimited(real-1) err = %v, want ErrLimitedUnavailable", err)
	}
}

// TestLocalCaptureRoleDisabled 角色开关关闭时本机受限采集显式拒绝(与执行类请求同一底线)。
func TestLocalCaptureRoleDisabled(t *testing.T) {
	svc := WithLocalHostRoles(&fakeLimitedBase{}, func() LocalRoles { return LocalRoles{} }).(*localHostService)
	if _, err := svc.Capture(context.Background(), LocalHostID); !errors.Is(err, ErrLocalRoleDisabled) {
		t.Fatalf("Capture(local) with disabled role err = %v, want ErrLocalRoleDisabled", err)
	}
}

// TestLocalExecLimitedRunsLocally 本机执行不经 SSH:退出码/stdout 语义与 Exec 对齐,
// zero limits 归一化为默认预算。
func TestLocalExecLimitedRunsLocally(t *testing.T) {
	svc := localLimitedOf(&fakeBaseService{})
	snap, err := svc.Capture(context.Background(), LocalHostID)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	res, err := svc.ExecLimited(context.Background(), snap, []string{"go", "version"}, ExecutionLimits{})
	if err != nil {
		t.Fatalf("ExecLimited(go version): %v", err)
	}
	if res.ExitCode != 0 || res.Truncated {
		t.Fatalf("result = %+v, want clean success", res)
	}
	if !strings.Contains(res.Stdout, "go") {
		t.Fatalf("stdout = %q, want go version output", res.Stdout)
	}
}

// TestLocalExecLimitedTruncates 输出经 outputBudget 共享预算截断(与 SSH 版同语义)。
func TestLocalExecLimitedTruncates(t *testing.T) {
	svc := localLimitedOf(&fakeBaseService{})
	snap, err := svc.Capture(context.Background(), LocalHostID)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	res, err := svc.ExecLimited(context.Background(), snap, []string{"go", "version"}, ExecutionLimits{Bytes: 10, Lines: 1000})
	if err != nil {
		t.Fatalf("ExecLimited: %v", err)
	}
	if !res.Truncated || len(res.Stdout) != 10 {
		t.Fatalf("result = %+v, want 10-byte truncated stdout", res)
	}
}

// TestLocalExecLimitedTargetChanged 批准后配置漂移(指纹不一致)拒绝执行。
func TestLocalExecLimitedTargetChanged(t *testing.T) {
	svc := localLimitedOf(&fakeBaseService{})
	snap, err := svc.Capture(context.Background(), LocalHostID)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	snap.Server.Name = "tampered"
	if _, err := svc.ExecLimited(context.Background(), snap, []string{"go", "version"}, ExecutionLimits{}); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("err = %v, want ErrTargetChanged", err)
	}
}

// TestLocalExecLimitedInvalidCommand 命令 array 校验与 SSH 版共用一套(段数/大小/\0)。
func TestLocalExecLimitedInvalidCommand(t *testing.T) {
	svc := localLimitedOf(&fakeBaseService{})
	snap, err := svc.Capture(context.Background(), LocalHostID)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	for _, cmd := range [][]string{
		nil,
		{"go", "version\x00"},
		append([]string{"go"}, make([]string, 65)...),
	} {
		if _, err := svc.ExecLimited(context.Background(), snap, cmd, ExecutionLimits{}); err == nil {
			t.Fatalf("ExecLimited(%q) accepted invalid command", cmd)
		}
	}
}

// TestLocalExecLimitedMissingBinary 启动失败 → 127 伪退出码、无 error(上层走
// 「该命令在本机不可用」的正常降级分支)。
func TestLocalExecLimitedMissingBinary(t *testing.T) {
	svc := localLimitedOf(&fakeBaseService{})
	snap, err := svc.Capture(context.Background(), LocalHostID)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	res, err := svc.ExecLimited(context.Background(), snap, []string{"no-such-binary-xyz-9ca31f"}, ExecutionLimits{})
	if err != nil {
		t.Fatalf("ExecLimited(missing): %v", err)
	}
	if res.ExitCode != localExitNotFound {
		t.Fatalf("exit = %d, want %d", res.ExitCode, localExitNotFound)
	}
	if res.Stderr == "" {
		t.Fatal("missing binary must leave a human-readable stderr")
	}
}

// TestLocalExecLimitedContextCanceled ctx 取消 → 返回 ctx.Err(与 SSH 版 limitedContextErr 语义一致)。
func TestLocalExecLimitedContextCanceled(t *testing.T) {
	svc := localLimitedOf(&fakeBaseService{})
	snap, err := svc.Capture(context.Background(), LocalHostID)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.ExecLimited(ctx, snap, []string{"go", "version"}, ExecutionLimits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestLocalExecLimitedPassthrough 真实 serverID 的 ExecLimited 透传 base。
func TestLocalExecLimitedPassthrough(t *testing.T) {
	base := &fakeLimitedBase{}
	svc := localLimitedOf(base)
	approved := ConnectionSnapshot{Server: Server{ID: "real-1", Name: "web-1"}}
	res, err := svc.ExecLimited(context.Background(), approved, []string{"du"}, ExecutionLimits{})
	if err != nil {
		t.Fatalf("ExecLimited(real-1): %v", err)
	}
	if res.Stdout != "ssh-limited" {
		t.Fatalf("stdout = %q, want ssh-limited (base result)", res.Stdout)
	}
	if base.gotSnap.Server.ID != "real-1" || len(base.gotCmd) != 1 || base.gotCmd[0] != "du" {
		t.Fatalf("base got snap/cmd = %+v / %v", base.gotSnap, base.gotCmd)
	}
}
