package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// TestHealthBudgetDerivesFromRetries 健康门控预算按 重试×(单次超时+间隔)+一次超时余量 推导;
// 重型配置必须超过 60s 命令预算 execTimeout(门控走独立 ctx,不被切换阶段 execCtx 砍)。
func TestHealthBudgetDerivesFromRetries(t *testing.T) {
	// 重型:5×(10s+10s)+10s = 110s > execTimeout(60s)。
	heavy := &HealthCheck{Type: HealthCheckHTTP, URL: "http://x/", Retries: 5, TimeoutSeconds: 10, IntervalSeconds: 10}
	want := 5*(10*time.Second+10*time.Second) + 10*time.Second
	if got := healthBudget(heavy); got != want {
		t.Fatalf("healthBudget = %v, want %v", got, want)
	}
	if want <= execTimeout {
		t.Fatalf("重型健康配置预算应超过 execTimeout(%v),得 %v", execTimeout, want)
	}
	// 缺省归一:重试 3、单次超时 5s、interval=0(显式无间隔)→ 3×(5s+0)+5s = 20s。
	def := &HealthCheck{Type: HealthCheckHTTP, URL: "http://x/"}
	wantDef := 3*5*time.Second + 5*time.Second
	if got := healthBudget(def); got != wantDef {
		t.Fatalf("默认 healthBudget = %v, want %v", got, wantDef)
	}
	// 负 interval 归一为默认 3s:3×(5s+3s)+5s = 29s。
	defNeg := &HealthCheck{Type: HealthCheckHTTP, URL: "http://x/", IntervalSeconds: -1}
	if got := healthBudget(defNeg); got != 3*(5*time.Second+3*time.Second)+5*time.Second {
		t.Fatalf("负 interval 默认 3s 的 healthBudget = %v", got)
	}
}

// TestRetargetExecContainer healthExec 探测命令的容器名替换(实例轮转预热打新实例用)。
func TestRetargetExecContainer(t *testing.T) {
	cmd := []string{"docker", "exec", "shop", "sh", "-c", "pg_isready"}
	got := retargetExecContainer(cmd, "shop", "shop-r1a2b3c")
	want := []string{"docker", "exec", "shop-r1a2b3c", "sh", "-c", "pg_isready"}
	if len(got) != len(want) {
		t.Fatalf("retarget 结果异常: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("retarget[%d]=%q want %q: %v", i, got[i], want[i], got)
		}
	}
	// 原切片不被篡改。
	if cmd[2] != "shop" {
		t.Fatalf("retarget 不应改原命令切片: %v", cmd)
	}
	// 容器名相同 / 新名为空 → 原样返回。
	if retargetExecContainer(cmd, "shop", "shop")[2] != "shop" {
		t.Fatalf("同名不应变化")
	}
	if retargetExecContainer(cmd, "shop", "")[2] != "shop" {
		t.Fatalf("空新名不应变化")
	}
}

// TestHealthCheckCommandSuccess 验证 command 探测一次通过 → 该机 success,message 含「健康检查通过」。
func TestHealthCheckCommandSuccess(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: func(_ string, _ []string) (*target.ExecResult, error) {
		return &target.ExecResult{ExitCode: 0}, nil
	}}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")

	svc := New(tgt, rsvc)
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		HealthCheck: &HealthCheck{Type: HealthCheckCommand, Command: []string{"true"}},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res[0].Status != run.TargetSuccess {
		t.Fatalf("want success, got %+v", res[0])
	}
	if !strings.Contains(res[0].Message, "健康检查通过") {
		t.Fatalf("message 应含「健康检查通过」: %q", res[0].Message)
	}
	// 健康探测命令(array 化的 ["true"])应出现在调用序列中,且在 current 软链切换之后
	// (dist 走 release 模式;探测后可能再跟 keepReleases 清理,故不强求是末条)。
	probeIdx, lnIdx := -1, -1
	for i, c := range tgt.calls {
		if len(c) == 1 && c[0] == "true" {
			probeIdx = i
		}
		if len(c) > 0 && c[0] == "ln" && lnIdx < 0 {
			lnIdx = i
		}
	}
	if probeIdx < 0 {
		t.Fatalf("应含健康探测命令 [\"true\"], got %v", tgt.calls)
	}
	if lnIdx < 0 || probeIdx < lnIdx {
		t.Fatalf("健康探测应在 current 切换之后跑: lnIdx=%d probeIdx=%d", lnIdx, probeIdx)
	}
}

// TestHealthCheckCommandFailRetriesExhausted 验证 command 探测始终非零 → 重试耗尽 → failed,
// message 人读含「健康检查失败」,且按 retries 次数重试。
func TestHealthCheckCommandFailRetriesExhausted(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	probeCalls := 0
	tgt := &stubTarget{execFn: func(_ string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) == 1 && cmd[0] == "false" {
			probeCalls++
			return &target.ExecResult{ExitCode: 1, Stderr: "boom"}, nil
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")

	svc := New(tgt, rsvc)
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		HealthCheck: &HealthCheck{
			Type: HealthCheckCommand, Command: []string{"false"},
			Retries: 3, IntervalSeconds: 0,
		},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res[0].Status != run.TargetFailed {
		t.Fatalf("want failed, got %+v", res[0])
	}
	if !strings.Contains(res[0].Message, "健康检查失败") {
		t.Fatalf("message 应含「健康检查失败」: %q", res[0].Message)
	}
	// 统一滚动:同一配置先做滚动前预检(3 次)再做部署后健康门控(3 次)→ 共 6 次。
	if probeCalls != 6 {
		t.Fatalf("探测应共 6 次(预检 3 + 门控 3), got %d", probeCalls)
	}
	// run 终态据健康结果置 failed。
	rn, _ := rsvc.Get(context.Background(), runID)
	if rn.Status != run.StatusFailed {
		t.Fatalf("run 终态 = %q, want failed", rn.Status)
	}
}

// TestHealthCheckHTTPBuildsCurlArray 验证 http 探测构造 curl -fsS --max-time T url（array 化,不拼 shell）。
func TestHealthCheckHTTPBuildsCurlArray(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	var probeCmd []string
	tgt := &stubTarget{execFn: func(_ string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) > 0 && cmd[0] == "curl" {
			probeCmd = cmd
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")

	svc := New(tgt, rsvc)
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		HealthCheck: &HealthCheck{
			Type: HealthCheckHTTP, URL: "http://localhost:8080/healthz", TimeoutSeconds: 7,
		},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res[0].Status != run.TargetSuccess {
		t.Fatalf("want success, got %+v", res[0])
	}
	want := []string{"curl", "-fsS", "--max-time", "7", "http://localhost:8080/healthz"}
	if len(probeCmd) != len(want) {
		t.Fatalf("curl 命令 = %v, want %v", probeCmd, want)
	}
	for i := range want {
		if probeCmd[i] != want[i] {
			t.Fatalf("curl 命令[%d] = %q, want %q (完整 %v)", i, probeCmd[i], want[i], probeCmd)
		}
	}
}

// TestHealthCheckNoneSkipped 验证 type=none / nil → 跳过健康检查(向后兼容 4-2:命令成功即 success,
// 不追加「健康检查通过」)。
func TestHealthCheckNoneSkipped(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")

	svc := New(tgt, rsvc)
	// nil HealthCheck。
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res[0].Status != run.TargetSuccess {
		t.Fatalf("want success, got %+v", res[0])
	}
	if strings.Contains(res[0].Message, "健康检查") {
		t.Fatalf("无健康检查时 message 不应提健康检查: %q", res[0].Message)
	}

	// type=none 显式 → 同样跳过。
	res2, _ := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		HealthCheck: &HealthCheck{Type: HealthCheckNone},
	})
	if strings.Contains(res2[0].Message, "健康检查") {
		t.Fatalf("type=none 时不应提健康检查: %q", res2[0].Message)
	}
}

// TestHealthCheckDefaultsAndClamps 验证默认值与上限(retries 默认 3 / ≤20;timeout 默认 5s / ≤60s)。
func TestHealthCheckDefaultsAndClamps(t *testing.T) {
	cases := []struct {
		hc          HealthCheck
		wantRetries int
		wantTimeout time.Duration
	}{
		{HealthCheck{}, defaultHealthRetries, defaultHealthTimeout},
		{HealthCheck{Retries: -1, TimeoutSeconds: -5}, defaultHealthRetries, defaultHealthTimeout},
		{HealthCheck{Retries: 100, TimeoutSeconds: 9999}, maxHealthRetries, maxHealthTimeout},
		{HealthCheck{Retries: 5, TimeoutSeconds: 10}, 5, 10 * time.Second},
	}
	for i, c := range cases {
		if got := c.hc.retries(); got != c.wantRetries {
			t.Fatalf("case %d retries = %d, want %d", i, got, c.wantRetries)
		}
		if got := c.hc.timeout(); got != c.wantTimeout {
			t.Fatalf("case %d timeout = %v, want %v", i, got, c.wantTimeout)
		}
	}
}

// TestHealthCheckMissingConfigFails 验证 http 缺 url / command 缺命令 → 该机 failed(人读,不 panic)。
func TestHealthCheckMissingConfigFails(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{}
	srv := seedServer(t, tgt, "web-1")
	svc := New(tgt, rsvc)

	// 独立 run/产物(各子用例健康失败会把 run 置 failed,不能复用同一 run)。
	runID1, artID1 := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")
	res, _ := svc.Deploy(context.Background(), DeployInput{
		RunID: runID1, ArtifactID: artID1, ServerIDs: []string{srv.ID},
		HealthCheck: &HealthCheck{Type: HealthCheckHTTP},
	})
	if len(res) != 1 || res[0].Status != run.TargetFailed {
		t.Fatalf("http 缺 url 应 failed, got %+v", res)
	}

	runID2, artID2 := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")
	res2, _ := svc.Deploy(context.Background(), DeployInput{
		RunID: runID2, ArtifactID: artID2, ServerIDs: []string{srv.ID},
		HealthCheck: &HealthCheck{Type: HealthCheckCommand},
	})
	if len(res2) != 1 || res2[0].Status != run.TargetFailed {
		t.Fatalf("command 缺命令应 failed, got %+v", res2)
	}
}
