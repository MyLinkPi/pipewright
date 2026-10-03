package deploy

// instance_rolling_test.go 覆盖「网关托管」两条路径:
//   - image 产物的 maxSurge 换实例(deployInstanceRollingOne):命令序 / 预热失败保旧实例 / 反查不到回退;
//   - 文件产物的「摘 → 部署 → 挂回」(deployWithGatewayDetach):成功挂回、失败保持摘除、
//     摘除本身失败时 message 必须与事实一致(绝不谎称「已摘除」)。
//
// 全程 stubTarget(不触网)+ fakeGateway(捕获 Swap / Detach / Attach)。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// fakeGateway 是注入的假 InstanceGateway:捕获 Swap / Detach / Attach 调用,可注入各自错误。
type fakeGateway struct {
	mu        sync.Mutex
	refs      []InstanceRef
	resolveBy string // 非空时只在 container == resolveBy 时返回 refs(模拟「服务名匹配」)
	swapErr   error
	detachErr error
	attachErr error
	swaps     [][2]string // (old, new)
	detached  []string
	attached  []string
}

func (g *fakeGateway) ResolveInstances(_ context.Context, _, container string) ([]InstanceRef, error) {
	if g.resolveBy != "" && container != g.resolveBy {
		return nil, nil
	}
	return g.refs, nil
}

func (g *fakeGateway) SwapInstance(_ context.Context, _ string, oldC, newC string) error {
	g.mu.Lock()
	g.swaps = append(g.swaps, [2]string{oldC, newC})
	g.mu.Unlock()
	return g.swapErr
}

func (g *fakeGateway) DetachInstance(_ context.Context, instanceID string) error {
	g.mu.Lock()
	g.detached = append(g.detached, instanceID)
	g.mu.Unlock()
	return g.detachErr
}

func (g *fakeGateway) AttachInstance(_ context.Context, instanceID string) error {
	g.mu.Lock()
	g.attached = append(g.attached, instanceID)
	g.mu.Unlock()
	return g.attachErr
}

func (g *fakeGateway) swapCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.swaps)
}

func (g *fakeGateway) attachCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.attached)
}

func (g *fakeGateway) detachCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.detached)
}

// twoInstanceRefs 是网关托管服务「shop.efg.com」的两个实例(shop-1 / shop-2)。
func twoInstanceRefs() []InstanceRef {
	return []InstanceRef{
		{ServiceID: "svc-1", ServiceName: "shop.efg.com", InstanceID: "i1", Container: "shop-1", Port: 8080},
		{ServiceID: "svc-1", ServiceName: "shop.efg.com", InstanceID: "i2", Container: "shop-2", Port: 8080},
	}
}

// rollExecFn 是实例轮转用例的通用桩:Running 探测回 true,其余默认 exit0。
func rollExecFn(serverID string, cmd []string) (*target.ExecResult, error) {
	_ = serverID
	if len(cmd) >= 5 && cmd[0] == "docker" && cmd[1] == "inspect" && cmd[3] == "{{.State.Running}}" {
		return &target.ExecResult{ExitCode: 0, Stdout: "true"}, nil
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

// startsWithAny 报告 calls 中是否有任一命令(空格连接)以 prefix 开头。
func startsWithAny(calls [][]string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

// ---- image:maxSurge 换实例 ----------------------------------------------------

// TestInstanceRollingSuccess:网关托管服务走逐实例轮转 ——
// pull → 每实例(run 新名 → settle 探测 → Swap → rm 旧),两实例两次 Swap,旧容器硬切为零。
func TestInstanceRollingSuccess(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: rollExecFn}
	srv := seedServer(t, tgt, "gw-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactImage, "registry.acme.io/shop:2")

	gw := &fakeGateway{refs: twoInstanceRefs()}
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
	gw.mu.Lock()
	swaps := append([][2]string(nil), gw.swaps...)
	gw.mu.Unlock()
	for i, sw := range swaps {
		if !strings.HasPrefix(sw[1], "shop-r") || len(sw[1]) != len("shop-r")+6 {
			t.Fatalf("Swap[%d] 新实例名不符:%q", i, sw[1])
		}
	}
	// 命令序:pull 新镜像;起新实例(新名);探测 Running;停旧实例(旧名)。
	if !hasCmd(tgt.calls, "docker", "pull", "registry.acme.io/shop:2") {
		t.Fatalf("缺 pull:\n%v", tgt.calls)
	}
	if !startsWithAny(tgt.calls, "docker run -d --name shop-r") {
		t.Fatalf("缺起新实例命令:\n%v", tgt.calls)
	}
	if !hasCmd(tgt.calls, "docker", "rm", "-f", "shop-1") || !hasCmd(tgt.calls, "docker", "rm", "-f", "shop-2") {
		t.Fatalf("缺停旧实例命令:\n%v", tgt.calls)
	}
	// 绝不出现「先删旧同名容器再起新」的硬切序。
	if hasCmd(tgt.calls, "docker", "rm", "-f", "shop") {
		t.Fatalf("不应硬切旧容器 shop:\n%v", tgt.calls)
	}
	// image 路径由 maxSurge 自己管流量,绝不走「摘 → 部署 → 挂回」。
	if gw.detachCount() != 0 || gw.attachCount() != 0 {
		t.Fatalf("image 路径不应 Detach/Attach,得 detach=%d attach=%d", gw.detachCount(), gw.attachCount())
	}
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

	gw := &fakeGateway{refs: twoInstanceRefs()}
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
		t.Fatalf("预热失败绝不应 Swap")
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
// (pull → rm 旧同名 → run 新同名)。
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
	if !strings.Contains(res[0].Message, "部署完成") || strings.Contains(res[0].Message, "实例轮转") {
		t.Fatalf("应回退旧滚动文案:%s", res[0].Message)
	}
	if gw.swapCount() != 0 {
		t.Fatalf("回退路径不应 Swap")
	}
	if !hasCmd(tgt.calls, "docker", "rm", "-f", "shop") || !hasCmd(tgt.calls, "docker", "run", "-d", "--name", "shop") {
		t.Fatalf("旧滚动应硬切同名容器 shop:\n%v", tgt.calls)
	}
}

// ---- 文件产物:网关「摘 → 部署 → 挂回」 ----------------------------------------

// gatewayDetachSvc 装配一个「文件产物 + 网关托管」的部署服务(dist 产物,cfg 带 gatewayService)。
func gatewayDetachSvc(t *testing.T, gw *fakeGateway, execFn func(string, []string) (*target.ExecResult, error)) (Service, string, string, string) {
	t.Helper()
	db := testDB(t)
	rsvc := run.New(db)
	tgt := &stubTarget{execFn: execFn}
	srv := seedServer(t, tgt, "gw-file-1")
	runID, artID := seedSuccessRunWithArtifact(t, db, rsvc, run.ArtifactDist, "dist/shop")
	return New(tgt, rsvc, WithInstanceGateway(gw)), runID, artID, srv.ID
}

// TestGatewayDetachAttachOnFileDeploy:文件部署成功后必须把摘除的实例逐个挂回(流量恢复)。
func TestGatewayDetachAttachOnFileDeploy(t *testing.T) {
	gw := &fakeGateway{refs: twoInstanceRefs(), resolveBy: "shop.efg.com"}
	svc, runID, artID, srvID := gatewayDetachSvc(t, gw, func(_ string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) > 0 && cmd[0] == "readlink" {
			return &target.ExecResult{ExitCode: 1}, nil // 无上一发布(首次部署)
		}
		return &target.ExecResult{ExitCode: 0}, nil
	})

	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srvID},
		Config: map[string]string{"gatewayService": "shop.efg.com"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].Status != run.TargetSuccess {
		t.Fatalf("want success, got %+v", res)
	}
	if gw.detachCount() != 2 || gw.attachCount() != 2 {
		t.Fatalf("应摘除 2 个实例并全部挂回,得 detach=%d attach=%d", gw.detachCount(), gw.attachCount())
	}
	// 全链路无告警 → message 不应出现网关联动告警文案。
	if strings.Contains(res[0].Message, "网关联动") {
		t.Fatalf("摘挂全成功不应有告警:%s", res[0].Message)
	}
}

// TestGatewayDetachKeepsDetachedOnFailure:部署失败 → 已摘除实例保持摘除(故障机不回流量),
// 且 message 如实说明摘除数量。
func TestGatewayDetachKeepsDetachedOnFailure(t *testing.T) {
	gw := &fakeGateway{refs: twoInstanceRefs(), resolveBy: "shop.efg.com"}
	svc, runID, artID, srvID := gatewayDetachSvc(t, gw, func(_ string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) > 0 && cmd[0] == "readlink" {
			return &target.ExecResult{ExitCode: 1}, nil
		}
		return &target.ExecResult{ExitCode: 1, Stderr: "boom"}, nil // 部署命令失败
	})

	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srvID},
		Config: map[string]string{"gatewayService": "shop.efg.com"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res) != 1 || res[0].Status == run.TargetSuccess {
		t.Fatalf("部署命令失败不应 success, got %+v", res)
	}
	if gw.attachCount() != 0 {
		t.Fatalf("部署失败绝不应挂回(故障机不回流量),得 attach=%d", gw.attachCount())
	}
	if !strings.Contains(res[0].Message, "2 个网关实例已保持摘除") {
		t.Fatalf("message 应如实说明保持摘除的实例数:%s", res[0].Message)
	}
	if strings.Contains(res[0].Message, "另有实例摘除失败") {
		t.Fatalf("摘除全部成功时不应出现摘除失败提示:%s", res[0].Message)
	}
}

// TestGatewayDetachReportsDetachFailure:摘除本身失败 → 继续部署(网关不可达不该拦死部署),
// 但 message 必须告警且**不得**声称「已保持摘除」(实例其实仍在接流量)。
func TestGatewayDetachReportsDetachFailure(t *testing.T) {
	gw := &fakeGateway{refs: twoInstanceRefs(), resolveBy: "shop.efg.com", detachErr: target.ErrUnreachable}
	svc, runID, artID, srvID := gatewayDetachSvc(t, gw, func(_ string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) > 0 && cmd[0] == "readlink" {
			return &target.ExecResult{ExitCode: 1}, nil
		}
		return &target.ExecResult{ExitCode: 1, Stderr: "boom"}, nil // 部署也失败
	})

	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srvID},
		Config: map[string]string{"gatewayService": "shop.efg.com"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	msg := res[0].Message
	// 两个实例的摘除失败都要出现(累加而非覆盖)。
	if strings.Count(msg, "从网关摘除实例") != 2 {
		t.Fatalf("两个实例的摘除失败告警都应保留:%s", msg)
	}
	if !strings.Contains(msg, "shop-1") || !strings.Contains(msg, "shop-2") {
		t.Fatalf("告警应指名两个实例:%s", msg)
	}
	if strings.Contains(msg, "已保持摘除") {
		t.Fatalf("摘除失败时绝不可声称已摘除:%s", msg)
	}
	if !strings.Contains(msg, "另有实例摘除失败") {
		t.Fatalf("应提示实例可能仍在接流量:%s", msg)
	}
	if gw.attachCount() != 0 {
		t.Fatalf("未成功摘除的实例不应被挂回,得 attach=%d", gw.attachCount())
	}
}

// TestGatewayDetachSkippedWithoutService:未配 gatewayService → 完全不触碰网关(行为不变)。
func TestGatewayDetachSkippedWithoutService(t *testing.T) {
	gw := &fakeGateway{refs: twoInstanceRefs()}
	svc, runID, artID, srvID := gatewayDetachSvc(t, gw, func(_ string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) > 0 && cmd[0] == "readlink" {
			return &target.ExecResult{ExitCode: 1}, nil
		}
		return &target.ExecResult{ExitCode: 0}, nil
	})

	res, err := svc.Deploy(context.Background(), DeployInput{
		RunID: runID, ArtifactID: artID, ServerIDs: []string{srvID},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res[0].Status != run.TargetSuccess {
		t.Fatalf("want success, got %+v", res)
	}
	if gw.detachCount() != 0 || gw.attachCount() != 0 {
		t.Fatalf("未配 gatewayService 不应触碰网关,得 detach=%d attach=%d", gw.detachCount(), gw.attachCount())
	}
}
