import { describe, it, expect } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import JobDrawer, { type ProducerOption } from './JobDrawer.vue'
import type { PipelineJob, PipelineStage } from '../../api/pipeline'

// push_image 的「产物来源」(artifactJob)是 producer 字段:候选须限定**同阶段**的 build_image 节点
// (镜像推送在来源节点内完成,见 internal/pipeline/push_image.go)。

const stage: PipelineStage = { id: 's-build', name: '构建', kind: 'build', jobs: [] }

/** 待编辑的推送镜像节点(自身不进入候选)。 */
function pushJob(config: Record<string, string> = {}): PipelineJob {
  return { id: 'jp', name: '推送镜像', type: 'push_image', summary: '', config }
}

const candidates: ProducerOption[] = [
  { name: '构建镜像', type: 'build_image', stageName: '构建', stageId: 's-build' }, // 同阶段 ✓
  { name: '别的镜像', type: 'build_image', stageName: '部署', stageId: 's-deploy' }, // 跨阶段 ✗
  { name: '跑测试', type: 'script', stageName: '构建', stageId: 's-build' }, // 非 build_image ✗
  { name: '推送镜像', type: 'push_image', stageName: '构建', stageId: 's-build' }, // 自身 ✗
]

/** 「产物来源」producer 下拉的候选项文本(value = 节点名)。 */
function optionValues(w: ReturnType<typeof mount>): string[] {
  const select = w.find('select[aria-label="产物来源"]')
  return select.findAll('option').map((o) => o.attributes('value') ?? '')
}

describe('JobDrawer — push_image「产物来源」仅列同阶段 build_image', () => {
  it('候选按「同阶段 + build_image + 非自身」过滤', async () => {
    const w = mount(JobDrawer, {
      props: { job: pushJob(), stage, producerOptions: candidates },
    })
    await nextTick()
    // 首项是空占位(未选择),其后只剩同阶段的 build_image。
    expect(optionValues(w)).toEqual(['', '构建镜像'])
  })

  it('选择后写回 artifactJob(节点名)', async () => {
    const w = mount(JobDrawer, {
      props: { job: pushJob(), stage, producerOptions: candidates },
    })
    await nextTick()
    const select = w.find('select[aria-label="产物来源"]')
    await select.setValue('构建镜像')
    const last = w.emitted('update')!.at(-1)![0] as Partial<PipelineJob>
    expect(last.config!.artifactJob).toBe('构建镜像')
  })
})
