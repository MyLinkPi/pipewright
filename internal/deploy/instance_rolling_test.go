package deploy

// instance_rolling_test.go:实例级轮转(默认策略)的命令序 / 失败语义 / 回退路径单测。
// 全程 stubTarget(不触网)+ fakeGateway(捕获 Swap)。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// fakeGateway 是注入的假 InstanceGateway(捕获 Swap 调用)。
type fakeGateway struct {
	mu      sync.Mutex
	refs    []InstanceRef
	swapErr error
	swaps   [][2]string // (old, new)
}

func (g *fakeGateway) ResolveInstances(_ context.Context, _, _ string) ([]InstanceRef, error) {
	return g.refs, nil
}

func (g *fakeGateway) SwapInstance(_ context.Context, _ string, oldC, newC string) error {
	g.mu.Lock()
	g.swaps = append(g.swaps, [2]string{oldC, newC})
	g.mu.Unlock()
	return g.swapErr
}

func (g *fakeGateway) swapCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.swaps)
}

// rollExecFn 是实例轮转用例的通用桩:Running 探测回 true,其余默认 exit0。
func rollExecFn(serverID string, cmd []string) (*target.ExecResult, error) {
	_ = serverID
	if len(cmd) >= 5 && cmd[0] == "docker" && cmd[1] == "inspect" && cmd[3] == "{{.State.Running}}" {
		return &target.ExecResult{ExitCode: 0, Stdout: "true"}, nil
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

// hasCmd 复用 stage_image_test.go 的既有定义;startsWithAny 为本文件补充。

// TestInstanceRollingSuccess:默认策略(空串)下,网关托管服务走逐实例轮转 ——
// pull → 每实例(run 新名 → settle 探测 → Swap → rm 旧),两实例两次 Swap,旧容器硬切为零。
func TestInstanceRollingSuccess(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: rollExecFn}
	srv := seedServer(t, tgt, "gw-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry.acme.io/shop:2")

	gw := &fakeGateway{refs: []InstanceRef{
		{ServiceID: "svc-1", ServiceName: "shop.efg.com", InstanceID: "i1", Container: "shop-1", Port: 8080},
		{ServiceID: "svc-1", ServiceName: "shop.efg.com", InstanceID: "i2", Container: "shop-2", Port: 8080},
	}}
	svc := New(tgt, rsvc, WithInstanceGateway(gw))
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		Config: map[string]string{"drainSeconds": "0"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetSuccess {
		t.Fatalf("want success, got %+v", res)
	}
	if !strings.Contains(res[0].Message, "实例轮转完成") || !strings.Contains(res[0].Message, "shop.efg.com") {
		t.Fatalf("message 应说明实例轮转:%s", res[0].Message)
	}
	// 两次 Swap:shop-1、shop-2 各一次,新名形如 shop-r<hex>。
	if gw.swapCount() != 2 {
		t.Fatalf("应 2 次 Swap,得 %d", gw.swapCount())
	}
	for i, sw := range gw.swaps {
		if !strings.HasPrefix(sw[1], "shop-r") || len(sw[1]) != len("shop-r")+6 {
			t.Fatalf("Swap[%d] 新实例名不符:%q", i, sw[1])
		}
	}
	// 命令序:pull 新镜像;起新实例(新名);探测 Running;停旧实例(旧名)。
	if !hasCmd(tgt.calls, "docker", "pull", "registry.acme.io/shop:2") {
		t.Fatalf("缺 pull:\n%v", tgt.calls)
	}
	if !hasCmd(tgt.calls, "docker", "run", "-d", "--name", "shop-r") && !startsWithAny(tgt.calls, "docker run -d --name shop-r") {
		t.Fatalf("缺起新实例命令:\n%v", tgt.calls)
	}
	if !hasCmd(tgt.calls, "docker", "rm", "-f", "shop-1") || !hasCmd(tgt.calls, "docker", "rm", "-f", "shop-2") {
		t.Fatalf("缺停旧实例命令:\n%v", tgt.calls)
	}
	// 绝不出现旧的「先删旧同名容器再起新」硬切序。
	if hasCmd(tgt.calls, "docker", "rm", "-f", "shop") {
		t.Fatalf("不应硬切旧容器 shop:\n%v", tgt.calls)
	}
}

func startsWithAny(calls [][]string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

// TestInstanceRollingHealthFailKeepsOld:新实例预热失败 → 删新容器、不 Swap、旧实例保留,
// 该机 failed 且中止剩余实例。
func TestInstanceRollingHealthFailKeepsOld(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: func(serverID string, cmd []string) (*target.ExecResult, error) {
		// settle 探测恒 false(新实例起不来)。
		if len(cmd) >= 5 && cmd[0] == "docker" && cmd[1] == "inspect" && cmd[3] == "{{.State.Running}}" {
			return &target.ExecResult{ExitCode: 0, Stdout: "false"}, nil
		}
		return rollExecFn(serverID, cmd)
	}}
	srv := seedServer(t, tgt, "gw-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry.acme.io/shop:2")

	gw := &fakeGateway{refs: []InstanceRef{
		{ServiceID: "svc-1", ServiceName: "shop.efg.com", InstanceID: "i1", Container: "shop-1", Port: 8080},
		{ServiceID: "svc-1", ServiceName: "shop.efg.com", InstanceID: "i2", Container: "shop-2", Port: 8080},
	}}
	svc := New(tgt, rsvc, WithInstanceGateway(gw))
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
		Config: map[string]string{"drainSeconds": "0"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetFailed {
		t.Fatalf("want failed, got %+v", res)
	}
	if !strings.Contains(res[0].Message, "保留旧版本") {
		t.Fatalf("message 应说明保留旧版本:%s", res[0].Message)
	}
	if gw.swapCount() != 0 {
		t.Fatalf("预热失败绝不应 Swap:%v", gw.swaps)
	}
	// 旧实例绝不被删;新实例容器被清理(至少一次 rm -f shop-r…)。
	if hasCmd(tgt.calls, "docker", "rm", "-f", "shop-1") || hasCmd(tgt.calls, "docker", "rm", "-f", "shop-2") {
		t.Fatalf("旧实例不应被删:\n%v", tgt.calls)
	}
	if !startsWithAny(tgt.calls, "docker rm -f shop-r") {
		t.Fatalf("新实例应被清理:\n%v", tgt.calls)
	}
}

// TestInstanceRollingFallbackNoMatch:反查不到网关服务 → 回退既有单机 image 滚动
//(pull → rm 旧同名 → run 新同名),行为与显式 rolling 一致。
func TestInstanceRollingFallbackNoMatch(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: rollExecFn}
	srv := seedServer(t, tgt, "plain-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry.acme.io/shop:2")

	gw := &fakeGateway{refs: nil} // 无匹配
	svc := New(tgt, rsvc, WithInstanceGateway(gw))
	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srv.ID},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetSuccess {
		t.Fatalf("want success, got %+v", res)
	}
	if !strings.Contains(res[0].Message, "image 部署完成") || strings.Contains(res[0].Message, "实例轮转") {
		t.Fatalf("应回退旧滚动文案:%s", res[0].Message)
	}
	if gw.swapCount() != 0 {
		t.Fatalf("回退路径不应 Swap")
	}
	if !hasCmd(tgt.calls, "docker", "rm", "-f", "shop") || !hasCmd(tgt.calls, "docker", "run", "-d", "--name", "shop") {
		t.Fatalf("旧滚动应硬切同名容器 shop:\n%v", tgt.calls)
	}
}
