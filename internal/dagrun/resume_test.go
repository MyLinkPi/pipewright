package dagrun

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// ─── NodeLayout / BuildResumePlan(计划侧)────────────────────────────────────

// mkStages 构造两级流水线:s1(job j1,j2)→ s2(needs s1;job j3)。
func mkStages() []pipeline.Stage {
	return []pipeline.Stage{
		{ID: "s1", Name: "构建", Jobs: []pipeline.Job{
			{ID: "j1", Name: "lint", Type: "script"},
			{ID: "j2", Name: "build", Type: "script"},
		}},
		{ID: "s2", Name: "部署", Needs: []string{"s1"}, Jobs: []pipeline.Job{
			{ID: "j3", Name: "deploy", Type: "deploy_container"},
		}},
	}
}

// parentRunWithSteps 构造带步骤(按 ordinal 升序)的父运行。
func parentRunWithSteps(statuses ...string) *run.Run {
	r := &run.Run{ID: "parent"}
	for i, st := range statuses {
		r.Steps = append(r.Steps, run.Step{Ordinal: i, Name: "n", Status: st})
	}
	return r
}

func TestNodeLayoutMatchesPlanOrder(t *testing.T) {
	keys, err := NodeLayout(mkStages())
	if err != nil {
		t.Fatalf("NodeLayout: %v", err)
	}
	want := []NodeKey{
		{StageID: "s1", StageName: "构建", JobID: "j1", JobName: "lint"},
		{StageID: "s1", StageName: "构建", JobID: "j2", JobName: "build"},
		{StageID: "s2", StageName: "部署", JobID: "j3", JobName: "deploy"},
	}
	if len(keys) != len(want) {
		t.Fatalf("layout 长度 %d,期望 %d", len(keys), len(want))
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys[%d] = %+v,期望 %+v", i, keys[i], want[i])
		}
	}
}

func TestNodeLayoutNoJobStagePlaceholder(t *testing.T) {
	keys, err := NodeLayout([]pipeline.Stage{{ID: "a", Name: "只有阶段"}})
	if err != nil {
		t.Fatalf("NodeLayout: %v", err)
	}
	if len(keys) != 1 || keys[0].JobID != "a" || keys[0].JobName != "只有阶段" {
		t.Fatalf("无 job 阶段应为占位节点(键=阶段 id),得到 %+v", keys)
	}
}

func TestBuildResumePlanFull(t *testing.T) {
	cfg := cfgWith(mkStages()...)
	parent := parentRunWithSteps(run.StepSuccess, run.StepFailed, run.StepFailed)
	// 对齐节点名(身份校验需要 stage/name 与父步骤一致)。
	parent.Steps[0].Name, parent.Steps[0].Stage = "lint", "构建"
	parent.Steps[1].Name, parent.Steps[1].Stage = "build", "构建"
	parent.Steps[2].Name, parent.Steps[2].Stage = "deploy", "部署"

	plan, err := BuildResumePlan(cfg, parent, map[int]string{1: run.NodeRetry, 2: run.NodeSkip})
	if err != nil {
		t.Fatalf("BuildResumePlan: %v", err)
	}
	want := []string{run.NodeInherit, run.NodeRetry, run.NodeSkip}
	for i := range want {
		if plan[i] != want[i] {
			t.Fatalf("plan[%d] = %q,期望 %q(全量 %v)", i, plan[i], want[i], plan)
		}
	}
}

func TestBuildResumePlanSpecChanged(t *testing.T) {
	cfg := cfgWith(mkStages()...)
	parent := parentRunWithSteps(run.StepSuccess, run.StepFailed, run.StepFailed)
	parent.Steps[1].Name = "renamed-build" // 与当前 spec 不一致
	_, err := BuildResumePlan(cfg, parent, map[int]string{1: run.NodeRetry, 2: run.NodeSkip})
	if !errors.Is(err, ErrSpecChanged) {
		t.Fatalf("期望 ErrSpecChanged,得到 %v", err)
	}
}

func TestBuildResumePlanStepCountChanged(t *testing.T) {
	cfg := cfgWith(mkStages()...)
	parent := parentRunWithSteps(run.StepFailed) // 步骤数与布局不符
	if _, err := BuildResumePlan(cfg, parent, map[int]string{0: run.NodeRetry}); !errors.Is(err, ErrSpecChanged) {
		t.Fatalf("期望 ErrSpecChanged,得到 %v", err)
	}
}

func TestBuildResumePlanRequiresFailedDecision(t *testing.T) {
	cfg := cfgWith(mkStages()...)
	parent := parentRunWithSteps(run.StepSuccess, run.StepFailed, run.StepFailed)
	parent.Steps[0].Name, parent.Steps[0].Stage = "lint", "构建"
	parent.Steps[1].Name, parent.Steps[1].Stage = "build", "构建"
	parent.Steps[2].Name, parent.Steps[2].Stage = "deploy", "部署"

	if _, err := BuildResumePlan(cfg, parent, map[int]string{}); err == nil {
		t.Fatal("失败节点未指定处置应报错")
	}
	if _, err := BuildResumePlan(cfg, parent, map[int]string{1: "bogus", 2: run.NodeSkip}); !errors.Is(err, run.ErrInvalidResumePlan) {
		t.Fatalf("未知动作应 ErrInvalidResumePlan,得到 %v", err)
	}
}

func TestBuildResumePlanZeroStepParent(t *testing.T) {
	// 父运行零步骤(如 runner 不支持恢复被 worker 拒收、从未 Plan 的派生运行):
	// 应报 ErrNoFailedNodes(无可恢复项),而非 ErrSpecChanged(配置并未变化,409 提示会误导)。
	cfg := cfgWith(mkStages()...)
	parent := &run.Run{ID: "parent"}
	if _, err := BuildResumePlan(cfg, parent, map[int]string{0: run.NodeRetry}); !errors.Is(err, ErrNoFailedNodes) {
		t.Fatalf("零步骤父运行应 ErrNoFailedNodes,得到 %v", err)
	}
}

func TestBuildResumePlanNoFailedNodes(t *testing.T) {
	cfg := cfgWith(mkStages()...)
	parent := parentRunWithSteps(run.StepSuccess, run.StepSuccess, run.StepSkipped)
	parent.Steps[0].Name, parent.Steps[0].Stage = "lint", "构建"
	parent.Steps[1].Name, parent.Steps[1].Stage = "build", "构建"
	parent.Steps[2].Name, parent.Steps[2].Stage = "deploy", "部署"
	if _, err := BuildResumePlan(cfg, parent, map[int]string{0: run.NodeRetry}); !errors.Is(err, ErrNoFailedNodes) {
		t.Fatalf("无失败节点应 ErrNoFailedNodes,得到 %v", err)
	}
}

// ─── Runner 执行侧(恢复计划应用)───────────────────────────────────────────

// recordingExec 记录「哪些阶段被真实执行、各执行了哪些 job」,并把 job 置 success。
type recordingExec struct {
	mu    sync.Mutex
	ran   map[string][]string // stageID → jobs(按执行序)
	order []string            // 阶段执行序
}

func newRecordingExec() *recordingExec { return &recordingExec{ran: map[string][]string{}} }

func (e *recordingExec) exec(ctx context.Context, _ *run.Run, stage pipeline.Stage, rep StageReporter) error {
	e.mu.Lock()
	e.order = append(e.order, stage.ID)
	e.mu.Unlock()
	for _, jb := range stage.Jobs {
		_ = rep.JobRunning(ctx, jb.ID)
		_ = rep.JobReporter(jb.ID).Log(ctx, "stdout", "ran "+jb.ID)
		_ = rep.JobDone(ctx, jb.ID, run.StepSuccess)
	}
	return nil
}

// ranSorted 返回某阶段实际执行的 job 集合(排序后,消除并行序差异)。
func (e *recordingExec) ranJobs(stageID string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := append([]string(nil), e.ran[stageID]...)
	sort.Strings(out)
	return out
}

func (e *recordingExec) ranStages() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.order...)
}

// recordJobs 包一层,在执行前登记本阶段要跑的 job 集合。
func (e *recordingExec) wrap() StageExecutor {
	return func(ctx context.Context, r *run.Run, stage pipeline.Stage, rep StageReporter) error {
		e.mu.Lock()
		for _, jb := range stage.Jobs {
			e.ran[stage.ID] = append(e.ran[stage.ID], jb.ID)
		}
		e.mu.Unlock()
		return e.exec(ctx, r, stage, rep)
	}
}

// parentRun 构造恢复运行的领域对象:计划下标对应 mkStages 布局(j1,j2,j3)。
func resumeRun(actions ...string) *run.Run {
	return &run.Run{
		ID:        "child",
		ProjectID: "p1",
		Trigger:   run.Trigger{Type: run.TriggerManual, Branch: "main"},
		Resume:    &run.ResumePlan{ParentRunID: "parent", Actions: actions},
	}
}

func TestRunnerResumeFiltersAndPassesDownstream(t *testing.T) {
	exec := newRecordingExec()
	loader := &fakeLoader{cfg: cfgWith(mkStages()...)}
	r := New(loader, WithStageExecutor(exec.wrap()))

	// j1 继承、j2 重跑、j3 正常执行:阶段 s1 只执行 j2;s2(依赖 s1)照常执行。
	sink := newFakeSink()
	if err := r.Run(context.Background(), resumeRun(run.NodeInherit, run.NodeRetry, run.NodeRun), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := exec.ranJobs("s1"); len(got) != 1 || got[0] != "j2" {
		t.Fatalf("s1 应只重跑 j2,得到 %v", got)
	}
	if got := exec.ranJobs("s2"); len(got) != 1 || got[0] != "j3" {
		t.Fatalf("下游 s2 应照常执行 j3,得到 %v", got)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.running != nil {
		for _, ord := range sink.running {
			if ord == 0 { // j1 继承:不得进入 running
				t.Fatalf("继承节点 ordinal 0 不应置 running:%v", sink.running)
			}
		}
	}
	if sink.done[0] != "" {
		t.Fatalf("继承节点 ordinal 0 已由 Plan 落终态,执行器不得改写:done=%v", sink.done)
	}
	if sink.done[1] != run.StepSuccess || sink.done[2] != run.StepSuccess {
		t.Fatalf("j2/j3 应 success:done=%v", sink.done)
	}
}

func TestRunnerResumeSkippedNodePassesDownstream(t *testing.T) {
	exec := newRecordingExec()
	loader := &fakeLoader{cfg: cfgWith(mkStages()...)}
	r := New(loader, WithStageExecutor(exec.wrap()))

	// j1 跳过(放行)、j2 重跑:阶段 s1 执行 j2;s2 不被阻断。
	sink := newFakeSink()
	if err := r.Run(context.Background(), resumeRun(run.NodeSkip, run.NodeRetry, run.NodeRun), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := exec.ranJobs("s2"); len(got) != 1 || got[0] != "j3" {
		t.Fatalf("跳过节点的下游应照常执行,得到 %v", exec.ranJobs("s2"))
	}
}

func TestRunnerResumeAllInheritedStageNotExecuted(t *testing.T) {
	exec := newRecordingExec()
	loader := &fakeLoader{cfg: cfgWith(mkStages()...)}
	r := New(loader, WithStageExecutor(exec.wrap()))

	// s1 全部继承:s1 不执行;s2(依赖 s1)因 s1 放行而照常执行。
	sink := newFakeSink()
	if err := r.Run(context.Background(), resumeRun(run.NodeInherit, run.NodeInherit, run.NodeRun), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, st := range exec.ranStages() {
		if st == "s1" {
			t.Fatalf("全部继承的阶段不应被执行:%v", exec.ranStages())
		}
	}
	if got := exec.ranJobs("s2"); len(got) != 1 {
		t.Fatalf("下游应照常执行:%v", got)
	}
}

func TestRunnerResumeInStageNeedsRemappedToRetained(t *testing.T) {
	// 阶段内 job 级 DAG:s1 中 j2 needs j1;j1 继承、j2 重跑 → 过滤后 j2 的 needs 剪除,
	// 仍能被调度执行(执行器内部建图不会因引用被剪除的节点而失败)。
	stages := []pipeline.Stage{
		{ID: "s1", Name: "构建", Jobs: []pipeline.Job{
			{ID: "j1", Name: "lint", Type: "script"},
			{ID: "j2", Name: "build", Type: "script", Needs: []string{"j1"}},
		}},
	}
	exec := newRecordingExec()
	loader := &fakeLoader{cfg: cfgWith(stages...)}
	r := New(loader, WithStageExecutor(exec.wrap()))
	sink := newFakeSink()
	if err := r.Run(context.Background(), resumeRun(run.NodeInherit, run.NodeRetry), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := exec.ranJobs("s1"); len(got) != 1 || got[0] != "j2" {
		t.Fatalf("needs 剪除后 j2 应可执行,得到 %v", got)
	}
}

func TestRunnerResumePlanLengthMismatchFails(t *testing.T) {
	loader := &fakeLoader{cfg: cfgWith(mkStages()...)}
	r := New(loader)
	err := r.Run(context.Background(), resumeRun(run.NodeInherit), newFakeSink()) // 布局 3 节点,计划只有 1
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("计划与布局长度不符应明确失败,得到 %v", err)
	}
}

func TestRunnerSupportsResumeCapability(t *testing.T) {
	if !run.SupportsResume(New(&fakeLoader{cfg: cfgWith(mkStages()...)})) {
		t.Fatal("dagrun.Runner 应声明支持按节点恢复")
	}
}

// mkNoJobStages 构造含无 job 占位阶段的两级流水线:s0(纯检查点,无 job)→ s1(job j1)。
func mkNoJobStages() []pipeline.Stage {
	return []pipeline.Stage{
		{ID: "s0", Name: "检查点"},
		{ID: "s1", Name: "构建", Needs: []string{"s0"}, Jobs: []pipeline.Job{
			{ID: "j1", Name: "lint", Type: "script"},
		}},
	}
}

func execRanStage(exec *recordingExec, stageID string) bool {
	for _, st := range exec.ranStages() {
		if st == stageID {
			return true
		}
	}
	return false
}

// TestRunnerResumeNoJobStageRunExecutesNormally 回归:无 job 占位阶段计划为 run/retry 时
// 必须走正常执行路径(exec 被调用、finishRemaining 收尾 success),不得被当「无待执行节点」
// 放行(旧缺陷:占位 step 悬挂 pending 到 run 收尾被 reconcile 误标)。
func TestRunnerResumeNoJobStageRunExecutesNormally(t *testing.T) {
	exec := newRecordingExec()
	loader := &fakeLoader{cfg: cfgWith(mkNoJobStages()...)}
	r := New(loader, WithStageExecutor(exec.wrap()))
	sink := newFakeSink()
	if err := r.Run(context.Background(), resumeRun(run.NodeRun, run.NodeRun), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !execRanStage(exec, "s0") {
		t.Fatalf("无 job 阶段 plan=run 应正常执行,实际执行阶段:%v", exec.ranStages())
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.done[0] != run.StepSuccess {
		t.Fatalf("占位节点应被正常路径收尾为 success,得到 done=%v", sink.done)
	}
	if sink.done[1] != run.StepSuccess {
		t.Fatalf("下游 j1 应 success,得到 done=%v", sink.done)
	}
}

// TestRunnerResumeNoJobStageWhenStillApplied 回归:无 job 占位阶段计划为 run 时,when 条件
// 仍须重评(旧缺陷:条件检查被绕过,父运行中因 when 跳过的阶段在恢复中无条件放行下游)。
func TestRunnerResumeNoJobStageWhenStillApplied(t *testing.T) {
	stages := mkNoJobStages()
	stages[0].When = pipeline.When{Branches: []string{"release"}} // 触发分支 main → 不满足
	exec := newRecordingExec()
	loader := &fakeLoader{cfg: cfgWith(stages...)}
	r := New(loader, WithStageExecutor(exec.wrap()))
	sink := newFakeSink()
	if err := r.Run(context.Background(), resumeRun(run.NodeRun, run.NodeRun), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if execRanStage(exec, "s0") {
		t.Fatal("when 不满足的占位阶段不应执行")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.done[0] != run.StepSkipped {
		t.Fatalf("when 不满足:占位节点应 skipped,得到 done=%v", sink.done)
	}
}

// TestRunnerResumeNoJobStageGateStillApplied 回归:无 job 占位阶段(纯审批检查点)计划为
// retry 时,审批门仍须阻塞等待批准(旧缺陷:审批门被绕过直接放行下游)。
func TestRunnerResumeNoJobStageGateStillApplied(t *testing.T) {
	stages := mkNoJobStages()
	stages[0].Gate = true
	gateCalls := 0
	exec := newRecordingExec()
	loader := &fakeLoader{cfg: cfgWith(stages...)}
	r := New(loader, WithStageExecutor(exec.wrap()), WithGate(func(_ context.Context, _ *run.Run, _ pipeline.Stage) (bool, error) {
		gateCalls++
		return true, nil
	}))
	sink := newFakeSink()
	if err := r.Run(context.Background(), resumeRun(run.NodeRetry, run.NodeRun), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gateCalls != 1 {
		t.Fatalf("占位阶段的审批门应被触发 1 次,实际 %d 次", gateCalls)
	}
}

// TestRunnerResumeNoJobStageInheritStillSkipped 确认修复不改变继承语义:无 job 阶段继承时
// 仍不执行、由 Plan 落终态(执行器不改写)。
func TestRunnerResumeNoJobStageInheritStillSkipped(t *testing.T) {
	exec := newRecordingExec()
	loader := &fakeLoader{cfg: cfgWith(mkNoJobStages()...)}
	r := New(loader, WithStageExecutor(exec.wrap()))
	sink := newFakeSink()
	if err := r.Run(context.Background(), resumeRun(run.NodeInherit, run.NodeRun), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if execRanStage(exec, "s0") {
		t.Fatal("继承的占位阶段不应执行")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.done[0] != "" {
		t.Fatalf("继承节点已由 Plan 落终态,执行器不得改写:done=%v", sink.done)
	}
}
