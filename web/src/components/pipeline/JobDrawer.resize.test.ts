import { beforeEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import JobDrawer from './JobDrawer.vue'
import type { PipelineJob, PipelineStage } from '../../api/pipeline'

const STORAGE_KEY = 'pipewright_drawer_width'
const stage: PipelineStage = { id: 's1', name: '构建', kind: 'build', jobs: [] }
const job: PipelineJob = { id: 'j1', name: '容器部署', type: 'templated', summary: '', config: { image: 'node:20' } }

/** 在把手上派发 pointer 序列(MouseEvent 携带坐标;jsdom 无 setPointerCapture,composable 内已降级)。 */
function drag(handle: HTMLElement, fromX: number, moveX: number): void {
  handle.dispatchEvent(new MouseEvent('pointerdown', { clientX: fromX, button: 0, bubbles: true }))
  window.dispatchEvent(new MouseEvent('pointermove', { clientX: moveX, bubbles: true }))
  window.dispatchEvent(new MouseEvent('pointerup', { clientX: moveX, bubbles: true }))
}

beforeEach(() => {
  localStorage.clear()
  // jsdom 默认视口 1024px → 动态上限 704px,会夹掉 720/860 等用例宽度;钉到桌面宽度。
  Object.defineProperty(window, 'innerWidth', { value: 1920, configurable: true })
})

describe('JobDrawer — 抽屉宽度拖拽调节', () => {
  it('渲染左缘拖拽把手,默认内联宽度 440px', () => {
    const w = mount(JobDrawer, { props: { job, stage } })
    const handle = w.find('.drawer-resize-handle')
    expect(handle.exists()).toBe(true)
    expect(handle.attributes('role')).toBe('separator')
    expect(handle.attributes('aria-orientation')).toBe('vertical')
    // 窗口分隔符需暴露当前/边界宽度,屏幕阅读器才能感知拖拽结果。
    expect(handle.attributes('aria-valuenow')).toBe('440')
    expect(handle.attributes('aria-valuemin')).toBe('360')
    expect(handle.attributes('aria-valuemax')).toBe('1080')
    const aside = w.find('aside.job-drawer')
    expect(aside.attributes('style')).toContain('width: 440px')
  })

  it('内容放在独立滚动容器内,把手不随内容滚走', () => {
    const w = mount(JobDrawer, { props: { job, stage } })
    expect(w.find('aside.job-drawer > .drawer-scroll').exists()).toBe(true)
    expect(w.find('.drawer-scroll .drawer-head').exists()).toBe(true)
    expect(w.find('.drawer-scroll .drawer-resize-handle').exists()).toBe(false)
  })

  it('向左拖拽把手 → 抽屉变宽,松手后宽度记忆到 localStorage', async () => {
    const w = mount(JobDrawer, { props: { job, stage } })
    const handle = w.find('.drawer-resize-handle')
    drag(handle.element as HTMLElement, 800, 600) // 左移 200px → 变宽 200px
    await nextTick()

    expect(w.find('aside.job-drawer').attributes('style')).toContain('width: 640px')
    expect(localStorage.getItem(STORAGE_KEY)).toBe('640')
  })

  it('双击把手恢复默认宽度并落盘', async () => {
    localStorage.setItem(STORAGE_KEY, '720')
    const w = mount(JobDrawer, { props: { job, stage } })
    expect(w.find('aside.job-drawer').attributes('style')).toContain('width: 720px')

    await w.find('.drawer-resize-handle').trigger('dblclick')
    await nextTick()

    expect(w.find('aside.job-drawer').attributes('style')).toContain('width: 440px')
    expect(localStorage.getItem(STORAGE_KEY)).toBe('440')
  })

  it('重挂载时从 localStorage 恢复上次宽度', async () => {
    localStorage.setItem(STORAGE_KEY, '860')
    const w = mount(JobDrawer, { props: { job, stage } })
    expect(w.find('aside.job-drawer').attributes('style')).toContain('width: 860px')
  })

  it('把手可聚焦并响应方向键微调', async () => {
    const w = mount(JobDrawer, { props: { job, stage } })
    const handle = w.find('.drawer-resize-handle')
    expect(handle.attributes('tabindex')).toBe('0')

    await handle.trigger('keydown', { key: 'ArrowLeft' }) // ← = 变宽(把手在抽屉左缘)
    await nextTick()
    expect(w.find('aside.job-drawer').attributes('style')).toContain('width: 456px')
  })

  it('点击把手即获得焦点(之后可直接用方向键微调)', async () => {
    const w = mount(JobDrawer, { props: { job, stage }, attachTo: document.body })
    const handle = w.find('.drawer-resize-handle')

    // 原生 pointerdown(MouseEvent 带坐标;VTU 的 trigger 不能让只读的 clientX 赋值)。
    handle.element.dispatchEvent(new MouseEvent('pointerdown', { clientX: 800, button: 0, bubbles: true }))
    expect(document.activeElement).toBe(handle.element)

    await handle.trigger('keydown', { key: 'ArrowLeft' })
    await nextTick()
    expect(w.find('aside.job-drawer').attributes('style')).toContain('width: 456px')

    w.unmount() // 卸载时恢复拖拽期间锁定的 body 样式
    expect(document.body.style.cursor).toBe('')
  })
})
