// resume.go 实现「失败运行按节点恢复(派生重跑)」的**计划侧**:
//
//   - NodeLayout / buildLayout:把(矩阵展开后的)阶段集按拓扑序摊平成节点序列——与 Runner.Run
//     的 Plan 声明同序同长,是「节点身份跨运行对齐」的唯一事实来源。恢复创建时校验器与执行器
//     共用同一布局函数,配置未变 ⇒ ordinal 一一对应;配置变了 ⇒ 逐 ordinal 比对节点名必失败。
//   - BuildResumePlan:依「当前 spec + 父运行步骤 + 用户对失败节点的处置」生成全量动作数组
//     (下标 = ordinal):父成功 → inherit;父失败 → 用户选择的 retry/skip;其余 → run。
//     计划随派生运行持久化(resume_plan_json),执行器(Runner)纯按它应用,不回读父运行。
//
// 执行侧(Runner 如何应用计划)见 dagrun.go 的 resume 应用段。
package dagrun

import (
	"context"
	"errors"
	"fmt"

	"github.com/huangchengsir/pipewright/internal/dag"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// ErrSpecChanged 表示流水线配置自父运行后已变化,节点无法按 ordinal 对齐,恢复被拒。
// HTTP 层映射 409 spec_changed(人读提示「流水线配置已变化,无法按节点恢复」)。
var ErrSpecChanged = errors.New("dagrun: pipeline spec changed since parent run")

// ErrNoFailedNodes 表示父运行没有失败节点,无可恢复项(全部成功或尚无失败终态明细)。
var ErrNoFailedNodes = errors.New("dagrun: parent run has no failed nodes")

// NodeKey 标识流水线中的一个执行节点(阶段 + job),是节点跨运行的稳定身份。
// JobName/StageName 与 run_steps.name/stage 同源比对;JobID 供 Runner 定位阶段内 job。
type NodeKey struct {
	StageID   string
	StageName string
	// JobID 是节点对应 job 的 id;无 job 阶段的占位节点用该阶段 id(与 Runner.Run 的
	// finishRemaining 收尾键一致)。
	JobID   string
	JobName string
}

// nodeLayout 是 Runner 执行与恢复校验共用的布局:节点序列 + Runner 需要的三张索引表。
type nodeLayout struct {
	keys  []NodeKey
	decls []run.StepDecl
	// jobOrdinals[stageID] = 该阶段 jobID → 全局 ordinal;stageFirstOrd 供阶段级日志归位。
	jobOrdinals   map[string]map[string]int
	stageFirstOrd map[string]int
	stageByID     map[string]pipeline.Stage
	graph         *dag.Graph
}

// buildLayout 对**已矩阵展开**的阶段集建图并按拓扑序摊平节点(Plan 声明顺序 = keys 顺序)。
func buildLayout(stages []pipeline.Stage) (*nodeLayout, error) {
	graph, err := BuildGraph(stages)
	if err != nil {
		return nil, err
	}
	stageByID := make(map[string]pipeline.Stage, len(stages))
	for _, st := range stages {
		stageByID[st.ID] = st
	}
	order := graph.TopoOrder()
	lo := &nodeLayout{
		keys:          make([]NodeKey, 0),
		decls:         make([]run.StepDecl, 0),
		jobOrdinals:   make(map[string]map[string]int, len(order)),
		stageFirstOrd: make(map[string]int, len(order)),
		stageByID:     stageByID,
		graph:         graph,
	}
	ord := 0
	for _, id := range order {
		st := stageByID[id]
		lo.stageFirstOrd[id] = ord
		m := make(map[string]int, len(st.Jobs))
		if len(st.Jobs) == 0 {
			// 无 job 阶段:一个以阶段名命名的占位节点(键用 stageID,供 finishRemaining 收尾)。
			lo.decls = append(lo.decls, run.StepDecl{Name: st.Name, Stage: st.Name})
			lo.keys = append(lo.keys, NodeKey{StageID: st.ID, StageName: st.Name, JobID: st.ID, JobName: st.Name})
			m[id] = ord
			ord++
		}
		for _, jb := range st.Jobs {
			m[jb.ID] = ord
			lo.decls = append(lo.decls, run.StepDecl{Name: jb.Name, Stage: st.Name})
			lo.keys = append(lo.keys, NodeKey{StageID: st.ID, StageName: st.Name, JobID: jb.ID, JobName: jb.Name})
			ord++
		}
		lo.jobOrdinals[id] = m
	}
	return lo, nil
}

// NodeLayout 返回(矩阵展开后)按拓扑序的节点布局,与 Runner.Run 的 Plan 声明同序同长。
// 恢复创建侧校验「配置是否与父运行一致」即比对它与父运行步骤的 (stage, name) 序列。
func NodeLayout(stages []pipeline.Stage) ([]NodeKey, error) {
	lo, err := buildLayout(ExpandMatrix(stages))
	if err != nil {
		return nil, err
	}
	return lo.keys, nil
}

// BuildResumePlan 生成恢复计划(全量动作数组,下标 = ordinal;长度 = 当前布局长度):
//
//   - 父运行节点 success → inherit(继承为成功,不重跑);
//   - 父运行节点 failed  → 必须由 actions[ordinal] 指定 retry / skip(缺失或非法 → 报错);
//   - 其余(skipped/pending 等,从未执行过)→ run(正常执行)。
//
// 校验:当前 spec 与父运行步骤的 (stage,name) 序列逐 ordinal 比对,不一致 → ErrSpecChanged;
// 父运行无失败节点 → ErrNoFailedNodes;actions 含未知动作值 → run.ErrInvalidResumePlan。
// 返回的动作数组直接交 run.Service.CreateResume 持久化。
func BuildResumePlan(cfg *pipeline.Config, parent *run.Run, actions map[int]string) ([]string, error) {
	if cfg == nil || parent == nil {
		return nil, run.ErrInvalidResumePlan
	}
	keys, err := NodeLayout(cfg.Spec.Stages)
	if err != nil {
		return nil, fmt.Errorf("dagrun: build resume layout: %w", err)
	}
	// 父运行零步骤(如 runner 不支持恢复被 worker 拒收、从未 Plan 的派生运行):先于长度
	// 比对拦截——报「无可恢复项」而非 spec_changed(配置并未变化,409 提示会误导用户)。
	if len(parent.Steps) == 0 {
		return nil, ErrNoFailedNodes
	}
	if len(keys) != len(parent.Steps) {
		return nil, ErrSpecChanged
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("dagrun: %w: 未指定任何失败节点的处置", run.ErrInvalidResumePlan)
	}
	plan := make([]string, len(keys))
	failed := 0
	for i, k := range keys {
		ps := parent.Steps[i]
		// 节点身份校验:ordinal 位置上的 (阶段名, 节点名) 必须与父运行一致,否则按配置已变拒绝
		// (宁可拒绝恢复,绝不把动作错配到别的节点上)。
		if ps.Name != k.JobName || ps.Stage != k.StageName {
			return nil, ErrSpecChanged
		}
		switch ps.Status {
		case run.StepSuccess:
			plan[i] = run.NodeInherit
		case run.StepFailed:
			failed++
			switch a := actions[i]; a {
			case run.NodeRetry, run.NodeSkip:
				plan[i] = a
			case "":
				return nil, fmt.Errorf("dagrun: %w: 失败节点 %d(阶段「%s」/「%s」)未指定处置", run.ErrInvalidResumePlan, i, k.StageName, k.JobName)
			default:
				return nil, fmt.Errorf("dagrun: %w: 动作 %q 非法(只支持 retry|skip)", run.ErrInvalidResumePlan, a)
			}
		default:
			// skipped / pending / running / waiting_approval:父运行从未真实执行成功过,正常执行。
			plan[i] = run.NodeRun
		}
	}
	if failed == 0 {
		return nil, ErrNoFailedNodes
	}
	return plan, nil
}

// markPreset 预标记节点「已收尾」:恢复计划的继承/人工跳过节点,其 step 已由 Plan 直接落
// 终态,执行器不会再跑它们;finishRemaining 兜底收尾时必须跳过(不得改写终态)。只改内存
// done 集,不触 DB。
func (s *stageReporter) markPreset(jobID string) {
	if s.done == nil {
		s.done = make(map[string]bool, len(s.jobOrd))
	}
	s.done[jobID] = true
}

// resumeActionLabel 把恢复动作翻成人读短语(日志用)。
func resumeActionLabel(a string) string {
	switch a {
	case run.NodeInherit:
		return "继承自上次运行"
	case run.NodeSkip:
		return "人工跳过"
	case run.NodeRetry:
		return "重跑"
	default:
		return "执行"
	}
}

// applyResumeToStage 按恢复计划整理单个阶段(执行侧;Runner 在 when/审批门检查**之前**调用,
// 使纯继承/跳过的阶段既不执行体、也不触发条件跳过或审批门):
//
//   - inherit / skip 节点:markPreset 预收尾 + 逐节点打日志(日志归到该节点自己的 step);
//   - retry / run 节点:保留;needs 重映射到保留节点(被继承/跳过的上游不重跑,依赖边剪除,
//     下游 job 得以直接执行——注意其 $PIPEWRIGHT_ENV 输出不可用,工作区各自独立克隆);
//
// 返回 nil 表示本阶段没有任何要真实执行的节点(全部继承/跳过):调用方直接放行下游。
// 返回值是 stage 的**副本**(仅 Jobs 被过滤),调用方原 stage 不受影响。
func applyResumeToStage(ctx context.Context, plan []string, stageID string, stage pipeline.Stage, jobOrd map[string]int, rep *stageReporter) *pipeline.Stage {
	inherited, skipped := 0, 0
	// 无 job 阶段:单占位节点(键 = stageID)。仅继承/跳过走预收尾 + 放行;run/retry
	// (及防御性无 ordinal 映射)必须原样返回走正常执行路径——否则 when 条件不重评、
	// 审批门被绕过、占位 step 悬挂 pending 到 run 收尾被 reconcile 误标。
	if len(stage.Jobs) == 0 {
		if ord, ok := jobOrd[stageID]; ok {
			switch plan[ord] {
			case run.NodeInherit:
				rep.markPreset(stageID)
				inherited++
			case run.NodeSkip:
				rep.markPreset(stageID)
				skipped++
			default:
				return &stage
			}
		} else {
			return &stage
		}
	}
	retained := make(map[string]bool, len(stage.Jobs))
	for _, jb := range stage.Jobs {
		ord, ok := jobOrd[jb.ID]
		if !ok {
			retained[jb.ID] = true // 防御:无 ordinal 映射的 job 保留原样执行
			continue
		}
		switch plan[ord] {
		case run.NodeRetry, run.NodeRun:
			retained[jb.ID] = true
		case run.NodeInherit:
			rep.markPreset(jb.ID)
			inherited++
		case run.NodeSkip:
			rep.markPreset(jb.ID)
			skipped++
			_ = rep.JobReporter(jb.ID).Log(ctx, "stdout", "⏭ 人工跳过(恢复计划):本节点不执行,放行下游")
		}
	}
	if inherited+skipped > 0 {
		_ = rep.Log(ctx, "stdout", fmt.Sprintf("恢复运行:阶段「%s」继承 %d 个节点、人工跳过 %d 个节点", stage.Name, inherited, skipped))
	}
	if len(retained) == 0 {
		_ = rep.Log(ctx, "stdout", fmt.Sprintf("恢复运行:阶段「%s」无待执行节点,放行下游", stage.Name))
		return nil
	}
	if len(retained) == len(stage.Jobs) {
		return &stage // 全部执行:无需过滤
	}
	filtered := make([]pipeline.Job, 0, len(retained))
	for _, jb := range stage.Jobs {
		if !retained[jb.ID] {
			continue
		}
		if len(jb.Needs) > 0 {
			needs := make([]string, 0, len(jb.Needs))
			for _, n := range jb.Needs {
				if retained[n] {
					needs = append(needs, n)
				}
			}
			jb.Needs = needs
		}
		filtered = append(filtered, jb)
	}
	out := stage
	out.Jobs = filtered
	_ = rep.Log(ctx, "stdout", fmt.Sprintf("恢复运行:阶段「%s」仅执行 %d/%d 个节点(其余继承/人工跳过)", stage.Name, len(filtered), len(stage.Jobs)))
	return &out
}
