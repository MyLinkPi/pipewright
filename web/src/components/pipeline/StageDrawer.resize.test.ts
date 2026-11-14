import { beforeEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'

import StageDrawer from './StageDrawer.vue'
import type { PipelineStage } from '../../api/pipeline'

const STORAGE_KEY = 'pipewright_drawer_width'
const stage: PipelineStage = { id: 's1', name: '构建', kind: 'build', jobs: [] }

// 阶段抽屉内的重型子编辑器与本用例无关,stub 掉(避免无关 API 触达)。
const globalStubs = {
  StagePostEditor: true,
  StageServicesEditor: true,
  LabelSelectorEditor: true,
}

beforeEach(() => {
  localStorage.clear()
  // 钉到桌面宽度,避免 jsdom 默认 1024px 把动态上限压到 704px。
  Object.defineProperty(window, 'innerWidth', { value: 1920, configurable: true })
})

describe('StageDrawer — 抽屉宽度拖拽调节', () => {
  it('把手暴露 ARIA 宽度值,内容在独立滚动容器内', () => {
    const w = mount(StageDrawer, {
      props: { stage, stageIndex: 0 },
      global: { stubs: globalStubs },
    })
    const handle = w.find('.drawer-resize-handle')
    expect(handle.attributes('role')).toBe('separator')
    expect(handle.attributes('aria-valuenow')).toBe('440')
    expect(handle.attributes('aria-valuemin')).toBe('360')
    expect(handle.attributes('aria-valuemax')).toBe('1080')
    // 把手必须在滚动容器之外,长表单滚动后仍可见可拖。
    expect(w.find('aside.job-drawer > .drawer-scroll .drawer-head').exists()).toBe(true)
    expect(w.find('.drawer-scroll .drawer-resize-handle').exists()).toBe(false)
  })

  it('拖拽后宽度变化,并与 JobDrawer 共享 localStorage 记忆', async () => {
    const w = mount(StageDrawer, {
      props: { stage, stageIndex: 0 },
      global: { stubs: globalStubs },
    })
    const handle = w.find('.drawer-resize-handle')

    // 原生 pointerdown(MouseEvent 带坐标;向左拖 100px → 440 + 100)。
    handle.element.dispatchEvent(new MouseEvent('pointerdown', { clientX: 800, button: 0, bubbles: true }))
    window.dispatchEvent(new MouseEvent('pointermove', { clientX: 700, bubbles: true }))
    window.dispatchEvent(new MouseEvent('pointerup', { clientX: 700, bubbles: true }))
    await w.vm.$nextTick()

    expect(w.find('aside.job-drawer').attributes('style')).toContain('width: 540px')
    expect(localStorage.getItem(STORAGE_KEY)).toBe('540')
    w.unmount()
  })
})
