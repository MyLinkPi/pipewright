/**
 * nodeUpstream — 「状态来源节点」(通知节点 config.statusFrom)的上游候选计算。
 *
 * 内联 notify 节点执行时整个运行还没结束,后端 run.Status 恒为 running —— 所以 {{status}}
 * 只有在配了 statusFrom(引用一个**上游节点**)时才有意义。本模块按与后端保存期校验
 * (internal/pipeline/notify_status.go)完全一致的规则筛出候选,保证下拉里出现的节点保存必过:
 *
 *   - 跨阶段:任一阶段声明了 needs → 按 needs 传递闭包(上游阶段的全部节点);全部无 needs
 *     → 按声明序线性(与后端 BuildGraph 的退化规则一致,存量直线流水线即成此形态);
 *   - 同阶段:按阶段内 job needs 传递闭包 —— 多 job 阶段一律走 job 级 DAG,未声明 needs 的
 *     平铺邻居是**并行**执行,不在此列;
 *   - 名字引用:同名节点(矩阵把一份逻辑节点展开成多个 cell,名字不变)会一并列出,
 *     后端要求同名节点全部为上游。
 *
 * 后端仍会复校(这里是 UX guardrail,不是 source of truth)。
 */

import type { PipelineStage } from '../../api/pipeline'

/** 按名引用节点的下拉选项(值 = 节点名,与部署节点 artifactJob 同一惯例)。 */
export interface NodeRefOption {
  name: string
  type: string
  stageName: string
}

/** 收集 needs 传递闭包(自 id 的 needs 边出发,含终点)。 */
function needsClosure(needs: ReadonlyArray<string> | undefined, byId: Map<string, ReadonlyArray<string> | undefined>): Set<string> {
  const seen = new Set<string>()
  const stack = [...(needs ?? [])]
  while (stack.length) {
    const id = stack.pop() as string
    if (seen.has(id)) continue
    seen.add(id)
    stack.push(...(byId.get(id) ?? []))
  }
  return seen
}

/**
 * `jobId` 的上游节点候选(声明序)。找不到该 job → 空数组。
 * 返回值里 stageName 为原始阶段名(展示时由调用方按需 localizeName)。
 */
export function upstreamNodeOptions(
  stages: ReadonlyArray<PipelineStage>,
  jobId: string,
): NodeRefOption[] {
  const si = stages.findIndex((s) => s.jobs.some((j) => j.id === jobId))
  if (si < 0) return []
  const owner = stages[si]
  const job = owner.jobs.find((j) => j.id === jobId)
  if (!job) return []

  // 上游**阶段**集合:有 needs → 闭包;全无 needs → 线性声明序。
  const anyNeeds = stages.some((s) => (s.needs?.length ?? 0) > 0)
  const stageNeeds = new Map(stages.map((s) => [s.id, s.needs]))
  const upstreamStageIds = anyNeeds
    ? needsClosure(owner.needs, stageNeeds)
    : new Set(stages.slice(0, si).map((s) => s.id))

  // 同阶段上游 job 集合:job needs 闭包(平铺邻居并行,不算上游)。
  const jobNeeds = new Map(owner.jobs.map((j) => [j.id, j.needs]))
  const sameStageUpstream = needsClosure(job.needs, jobNeeds)

  const out: NodeRefOption[] = []
  stages.forEach((s, i) => {
    const stageIsUpstream = i !== si && upstreamStageIds.has(s.id)
    for (const j of s.jobs) {
      if (stageIsUpstream || (i === si && sameStageUpstream.has(j.id))) {
        out.push({ name: j.name, type: j.type, stageName: s.name })
      }
    }
  })
  return out
}