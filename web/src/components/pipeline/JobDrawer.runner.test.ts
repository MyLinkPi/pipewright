import { describe, it, expect } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import JobDrawer from './JobDrawer.vue'
import type { PipelineJob, PipelineStage } from '../../api/pipeline'

const stage: PipelineStage = { id: 's1', name: '构建', kind: 'build', jobs: [] }

/** templated(工作室)节点:字段清单是内联数组、不含 RUNNER_FIELD(回归修复前 runner 落 extras 的场景)。 */
function templatedJob(config: Record<string, string>): PipelineJob {
  return { id: 'j1', name: '模板构建', type: 'templated', summary: '', config }
}

function runnerModeSelect(wrapper: ReturnType<typeof mount>) {
  return wrapper.find('select[aria-label="构建在哪执行"]')
}

describe('JobDrawer — templated 节点 runner(构建机)键归属回归', () => {
  it('config.runner 回显为钉死模式(不落 extras → resyncRunnerState 读得到)', async () => {
    const w = mount(JobDrawer, {
      props: {
        job: templatedJob({
          image: 'node:20',
          commandTemplate: 'npm run build',
          runner: 'server:srv-1',
        }),
        stage,
      },
    })
    await nextTick()

    const mode = runnerModeSelect(w)
    expect(mode.exists()).toBe(true)
    expect((mode.element as HTMLSelectElement).value).toBe('pinned')
    // 钉死服务器下拉选中原始 id(机器不在池里 → 兜底项)。
    const serverSelect = w.find('select[aria-label="目标服务器"]')
    expect((serverSelect.element as HTMLSelectElement).value).toBe('srv-1')
    // runner 不出现在「原始参数」高级区 KV 行。
    const kvKeys = w.findAll('input.kv-input').filter((i, idx) => idx % 2 === 0)
    expect(kvKeys.map((i) => (i.element as HTMLInputElement).value)).not.toContain('runner')
  })

  it('切「跟随项目默认」后 runner 从 config 清除(旧值不残留)', async () => {
    const w = mount(JobDrawer, {
      props: {
        job: templatedJob({
          image: 'node:20',
          commandTemplate: 'npm run build',
          runner: 'linux,arch=arm64',
        }),
        stage,
      },
    })
    await nextTick()

    const mode = runnerModeSelect(w)
    expect((mode.element as HTMLSelectElement).value).toBe('label')

    await mode.setValue('inherit')
    await nextTick()

    const last = w.emitted('update')!.at(-1)![0] as Partial<PipelineJob>
    expect(last.config).toBeTruthy()
    expect('runner' in last.config!).toBe(false)
    // 其余键不受影响
    expect(last.config!.image).toBe('node:20')
    expect(last.config!.commandTemplate).toBe('npm run build')
  })
})
