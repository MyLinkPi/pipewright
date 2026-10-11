package pipeline

import (
	"fmt"
	"strings"
)

// notify_status.go 校验通知节点的「状态来源节点」(config["statusFrom"])。
//
// 背景:内联 notify 节点执行时整个运行尚未结束,run.Status 恒为 running —— {{status}} 若取
// 运行状态等于没有信息。于是 notify 节点可配 statusFrom(节点**展示名**,与部署节点
// artifactJob 同一命名惯例)指向一个**上游节点**,执行期 {{status}} 改渲染该节点本次运行的
// 当前状态(见 dagrun.NodeStatusRegistry 与 build.runNotifyJob)。
//
// 保存期校验(本文件)保证「上游」成立,避免配了却永远读不到终态:
//   - 名字必须命中至少一个节点,且**所有**同名节点(矩阵展开的一份逻辑节点 = 多个同名 cell)
//     都必须先于该 notify 节点执行完;
//   - 跨阶段:流水线任一阶段声明了 needs → 按 needs 传递闭包;全部无 needs → 按声明序线性
//     (与 dagrun.BuildGraph 的退化规则一致,存量直线流水线即成此形态);
//   - 同阶段:按阶段内 job needs 传递闭包(多 job 阶段一律走 job 级 DAG,无 needs 即并行 ——
//     所以同阶段平铺的邻居不是上游,引用它们会被拒);
//   - 校验只覆盖经 NormalizeSpec 的入口(画布保存 / YAML 导入 / PAC),与本包其它校验一致。
const notifyStatusFromKey = "statusFrom"

// validateNotifyStatusFrom 校验全部 notify 节点的 statusFrom 引用(错误归一为 ErrInvalidJob)。
func validateNotifyStatusFrom(stages []Stage) error {
	anyStageNeeds := false
	for _, st := range stages {
		if len(st.Needs) > 0 {
			anyStageNeeds = true
			break
		}
	}

	for si := range stages {
		for ji := range stages[si].Jobs {
			n := stages[si].Jobs[ji]
			if strings.TrimSpace(n.Type) != KindNotify {
				continue
			}
			raw, ok := n.Config[notifyStatusFromKey]
			if !ok || raw == nil {
				continue
			}
			name, isStr := raw.(string)
			if !isStr {
				return fmt.Errorf("%w: notify job %q: statusFrom must be a string (node name)", ErrInvalidJob, n.Name)
			}
			name = strings.TrimSpace(name)
			if name == "" {
				continue // 留空 = 不做节点级状态覆盖(沿用运行状态)
			}

			matched := 0
			for ti := range stages {
				for tj := range stages[ti].Jobs {
					m := stages[ti].Jobs[tj]
					if strings.TrimSpace(m.Name) != name {
						continue
					}
					matched++
					if !guaranteedBeforeNotify(stages, anyStageNeeds, si, n, ti, m) {
						return fmt.Errorf(
							"%w: notify job %q: statusFrom %q must reference an upstream node (node %q in stage %q is not guaranteed to finish before it)",
							ErrInvalidJob, n.Name, name, m.Name, stages[ti].Name)
					}
				}
			}
			if matched == 0 {
				return fmt.Errorf("%w: notify job %q: statusFrom %q references an unknown node", ErrInvalidJob, n.Name, name)
			}
		}
	}
	return nil
}

// guaranteedBeforeNotify 报告节点 m(在 stages[ti])是否一定先于 notify 任务 n(在 stages[si])
// 执行完(判定规则见文件头注释)。
func guaranteedBeforeNotify(stages []Stage, anyStageNeeds bool, si int, n Job, ti int, m Job) bool {
	if ti == si {
		// 同阶段:job needs 传递闭包(n 自身不在闭包内 —— 引用自己同样被拒)。
		return jobNeedsReach(stages[si].Jobs, n.ID, m.ID)
	}
	if !anyStageNeeds {
		return ti < si // 线性退化:声明序在前即上游
	}
	return stageNeedsReach(stages, stages[si].Needs, stages[ti].ID)
}

// jobNeedsReach 报告 jobs 内 fromID 经 needs 边能否到达 targetID(传递闭包)。
func jobNeedsReach(jobs []Job, fromID, targetID string) bool {
	byID := make(map[string]Job, len(jobs))
	for _, jb := range jobs {
		byID[jb.ID] = jb
	}
	seen := map[string]bool{}
	stack := append([]string(nil), byID[fromID].Needs...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == targetID {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		stack = append(stack, byID[id].Needs...)
	}
	return false
}

// stageNeedsReach 报告 stages 内 needs 起点(notify 所在阶段的 needs)经阶段 needs 边能否到达
// targetStageID(传递闭包)。
func stageNeedsReach(stages []Stage, needs []string, targetStageID string) bool {
	byID := make(map[string]Stage, len(stages))
	for _, st := range stages {
		byID[st.ID] = st
	}
	seen := map[string]bool{}
	stack := append([]string(nil), needs...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == targetStageID {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		stack = append(stack, byID[id].Needs...)
	}
	return false
}
