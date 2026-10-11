package dagrun

import (
	"context"
	"testing"

	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// node_status_test.go 覆盖运行内节点状态注册表:同名聚合、ctx 注入、执行器中途可读,
// 以及按节点恢复时继承节点的预置状态。这是通知节点 {{status}}(config.statusFrom)的数据来源。

// TestNodeStatusRegistryAggregation 单元:更新立即生效;同名多命中(矩阵多 cell)按最需关注聚合。
func TestNodeStatusRegistryAggregation(t *testing.T) {
	reg := NewNodeStatusRegistry()
	reg.Add("a1", "单元测试", run.StepPending)
	reg.Add("a2", "单元测试", run.StepPending)
	reg.Add("b", "构建", run.StepPending)

	if st, ok := reg.StatusByName("单元测试"); !ok || st != run.StepPending {
		t.Fatalf("预置状态 = %q(ok=%v), want pending", st, ok)
	}
	reg.Update("a1", run.StepSuccess)
	// 另一 cell 仍 pending:最需关注优先(pending > success)。
	if st, _ := reg.StatusByName("单元测试"); st != run.StepPending {
		t.Fatalf("一 cell success、一 cell pending → %q, want pending", st)
	}
	// 任一 cell failed → failed(如实反映矩阵里的失败)。
	reg.Update("a1", run.StepFailed)
	if st, _ := reg.StatusByName("单元测试"); st != run.StepFailed {
		t.Fatalf("一 cell failed → %q, want failed", st)
	}
	// running 优先于 skipped / success,但低于 failed。
	reg.Update("a1", run.StepSkipped)
	reg.Update("a2", run.StepRunning)
	if st, _ := reg.StatusByName("单元测试"); st != run.StepRunning {
		t.Fatalf("skipped + running → %q, want running", st)
	}
	// 未命中 / 空名 / 未知 id 更新:安全。
	if _, ok := reg.StatusByName("不存在"); ok {
		t.Fatal("未知名不应命中")
	}
	if _, ok := reg.StatusByName("  "); ok {
		t.Fatal("空名不应命中")
	}
	reg.Update("unknown", run.StepRunning)
	if _, ok := reg.StatusByName("构建"); !ok {
		t.Fatal("构建应命中")
	}
	// nil 接收者安全(未注入注册表时的防御)。
	var nilReg *NodeStatusRegistry
	if _, ok := nilReg.StatusByName("x"); ok {
		t.Fatal("nil 注册表不应命中")
	}
	nilReg.Update("x", run.StepRunning)
	nilReg.Add("x", "x", run.StepPending)
}

// TestNodeStatusRegistryContext 单元:ctx 注入/读取闭环;未注入 → nil。
func TestNodeStatusRegistryContext(t *testing.T) {
	if NodeStatusFrom(context.Background()) != nil {
		t.Fatal("未注入时应返回 nil")
	}
	reg := NewNodeStatusRegistry()
	ctx := WithNodeStatusRegistry(context.Background(), reg)
	if NodeStatusFrom(ctx) != reg {
		t.Fatal("ctx 应带回同一注册表")
	}
}

// TestRunnerInjectsLiveNodeStatus 集成:执行器在运行中途经 ctx 读到各节点的实时状态
// (下游未执行 → pending;上游已上报 → success/failed)。
func TestRunnerInjectsLiveNodeStatus(t *testing.T) {
	cfg := cfgWith(
		pipeline.Stage{ID: "a", Name: "A", Jobs: []pipeline.Job{
			{ID: "j-a", Name: "节点A", Type: "script"},
		}},
		pipeline.Stage{ID: "b", Name: "B", Needs: []string{"a"}, Jobs: []pipeline.Job{
			{ID: "j-b", Name: "节点B", Type: "notify"},
		}},
	)
	got := map[string]string{}
	r := New(&fakeLoader{cfg: cfg}, WithStageExecutor(
		func(ctx context.Context, _ *run.Run, st pipeline.Stage, rep StageReporter) error {
			reg := NodeStatusFrom(ctx)
			if reg == nil {
				t.Error("执行器应能从 ctx 读到节点状态注册表")
				return nil
			}
			switch st.ID {
			case "a":
				got["a 看下游节点B"] = regStatus(t, reg, "节点B") // 尚未执行 → pending
				_ = rep.JobRunning(ctx, "j-a")
				got["a 自己 running"] = regStatus(t, reg, "节点A")
				_ = rep.JobDone(ctx, "j-a", run.StepSuccess)
				got["a 自己 done"] = regStatus(t, reg, "节点A")
			case "b":
				got["b 看上游节点A"] = regStatus(t, reg, "节点A") // 上游已成功 → success
				got["b 自己未上报"] = regStatus(t, reg, "节点B")
			}
			return nil
		}))
	if err := r.Run(context.Background(), &run.Run{ProjectID: "p"}, newFakeSink()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	want := map[string]string{
		"a 看下游节点B":     run.StepPending,
		"a 自己 running": run.StepRunning,
		"a 自己 done":    run.StepSuccess,
		"b 看上游节点A":     run.StepSuccess,
		"b 自己未上报":      run.StepPending,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestRunnerRegistryPrefillsInheritedNodesOnResume 集成:按节点恢复时继承节点在开跑前即预置
// success(阶段不执行),下游执行器读到的就是终态。
func TestRunnerRegistryPrefillsInheritedNodesOnResume(t *testing.T) {
	cfg := cfgWith(
		pipeline.Stage{ID: "a", Name: "A", Jobs: []pipeline.Job{
			{ID: "j-a", Name: "节点A", Type: "script"},
		}},
		pipeline.Stage{ID: "b", Name: "B", Needs: []string{"a"}, Jobs: []pipeline.Job{
			{ID: "j-b", Name: "节点B", Type: "script"},
		}},
	)
	ranA := false
	gotUpstream := ""
	r := New(&fakeLoader{cfg: cfg}, WithStageExecutor(
		func(ctx context.Context, _ *run.Run, st pipeline.Stage, _ StageReporter) error {
			if st.ID == "a" {
				ranA = true
			}
			if st.ID == "b" {
				gotUpstream = regStatus(t, NodeStatusFrom(ctx), "节点A")
			}
			return nil
		}))
	rn := &run.Run{ProjectID: "p", Resume: &run.ResumePlan{
		ParentRunID: "p0",
		Actions:     []string{run.NodeInherit, run.NodeRun}, // 节点A 继承、节点B 重跑
	}}
	if err := r.Run(context.Background(), rn, newFakeSink()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if ranA {
		t.Error("全继承阶段不应调用执行器")
	}
	if gotUpstream != run.StepSuccess {
		t.Errorf("继承节点预置状态 = %q, want success", gotUpstream)
	}
}

// regStatus 读注册表并要求命中(t.Error 可在调度 goroutine 里安全调用)。
func regStatus(t *testing.T, reg *NodeStatusRegistry, name string) string {
	t.Helper()
	if reg == nil {
		t.Errorf("节点状态注册表为 nil,无法读取 %q", name)
		return ""
	}
	st, ok := reg.StatusByName(name)
	if !ok {
		t.Errorf("节点 %q 应已登记进状态注册表", name)
		return ""
	}
	return st
}
