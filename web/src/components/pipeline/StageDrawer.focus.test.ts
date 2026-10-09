import { describe, it, expect } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import StageDrawer from './StageDrawer.vue'
import type { PipelineStage } from '../../api/pipeline'

function stageFixture(id: string, name: string): PipelineStage {
  return { id, name, kind: 'build', jobs: [] }
}

// 抽屉内重型子编辑器与本用例无关,stub 掉(避免无关 API 触达)。
const globalStubs = {
  StagePostEditor: true,
  StageServicesEditor: true,
  LabelSelectorEditor: true,
}

describe('StageDrawer 名称输入框自动聚焦', () => {
  it('首挂载(抽屉打开)即聚焦名称输入 —— watch immediate 回归', async () => {
    const w = mount(StageDrawer, {
      props: { stage: stageFixture('stg_1', 'build'), stageIndex: 0 },
      attachTo: document.body,
      global: { stubs: globalStubs },
    })
    await nextTick()
    await nextTick()

    const nameInput = w.find('input.drawer-input')
    expect(nameInput.exists()).toBe(true)
    expect(document.activeElement).toBe(nameInput.element)
    w.unmount()
  })

  it('切换到另一阶段时再次聚焦', async () => {
    const w = mount(StageDrawer, {
      props: { stage: stageFixture('stg_1', 'build'), stageIndex: 0 },
      attachTo: document.body,
      global: { stubs: globalStubs },
    })
    await nextTick()
    ;(document.activeElement as HTMLElement | null)?.blur()

    await w.setProps({ stage: stageFixture('stg_2', 'deploy'), stageIndex: 1 })
    await nextTick()
    await nextTick()

    expect(document.activeElement).toBe(w.find('input.drawer-input').element)
    w.unmount()
  })
})
