import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { effectScope } from 'vue'
import { useDrawerWidth, DRAWER_DEFAULT_WIDTH, DRAWER_MIN_WIDTH, DRAWER_MAX_WIDTH } from './useDrawerWidth'

const STORAGE_KEY = 'pipewright_drawer_width'

beforeEach(() => {
  localStorage.clear()
  // 固定视口宽,保证「视口宽-320」的动态上限可预期(1920-320=1600 → 上限取 1080)。
  Object.defineProperty(window, 'innerWidth', { value: 1920, configurable: true })
})

afterEach(() => {
  vi.unstubAllGlobals()
})

/** 在把手上派发 pointer 事件(MouseEvent 携带坐标;jsdom 无 setPointerCapture,composable 内已有降级)。 */
function drag(handle: HTMLElement, fromX: number, moves: number[], upX?: number): void {
  handle.dispatchEvent(new MouseEvent('pointerdown', { clientX: fromX, button: 0, bubbles: true }))
  for (const x of moves) {
    window.dispatchEvent(new MouseEvent('pointermove', { clientX: x, bubbles: true }))
  }
  window.dispatchEvent(new MouseEvent('pointerup', { clientX: upX ?? moves[moves.length - 1] ?? fromX, bubbles: true }))
}

describe('useDrawerWidth', () => {
  it('无存储记录时取默认宽度', () => {
    const { width } = useDrawerWidth()
    expect(width.value).toBe(DRAWER_DEFAULT_WIDTH)
  })

  it('读取已存储的合法宽度', () => {
    localStorage.setItem(STORAGE_KEY, '720')
    const { width } = useDrawerWidth()
    expect(width.value).toBe(720)
  })

  it('存储值非法(非数字/越界)时夹回合法区间', () => {
    localStorage.setItem(STORAGE_KEY, 'wide')
    expect(useDrawerWidth().width.value).toBe(DRAWER_DEFAULT_WIDTH)

    localStorage.setItem(STORAGE_KEY, '100')
    expect(useDrawerWidth().width.value).toBe(DRAWER_MIN_WIDTH)

    localStorage.setItem(STORAGE_KEY, '99999')
    expect(useDrawerWidth().width.value).toBe(DRAWER_MAX_WIDTH)
  })

  it('向左拖变宽,松手后持久化', () => {
    const { width, onHandleDown } = useDrawerWidth()
    const handle = document.createElement('div')
    handle.addEventListener('pointerdown', onHandleDown as EventListener)

    drag(handle, 800, [700, 600])
    expect(width.value).toBe(640) // 440 + (800 - 600)
    expect(localStorage.getItem(STORAGE_KEY)).toBe('640')
  })

  it('向右拖变窄,且不小于最小宽度', () => {
    localStorage.setItem(STORAGE_KEY, '500')
    const { width, onHandleDown } = useDrawerWidth()
    const handle = document.createElement('div')
    handle.addEventListener('pointerdown', onHandleDown as EventListener)

    drag(handle, 800, [1000, 1300])
    expect(width.value).toBe(DRAWER_MIN_WIDTH)
  })

  it('拖拽不超过视口上限,松手才落盘(拖拽中不写)', () => {
    const { width, onHandleDown } = useDrawerWidth()
    const handle = document.createElement('div')
    handle.addEventListener('pointerdown', onHandleDown as EventListener)

    handle.dispatchEvent(new MouseEvent('pointerdown', { clientX: 1200, button: 0, bubbles: true }))
    window.dispatchEvent(new MouseEvent('pointermove', { clientX: -5000 }))
    expect(width.value).toBe(DRAWER_MAX_WIDTH)
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull() // 拖拽中不落盘
    window.dispatchEvent(new MouseEvent('pointerup', { clientX: -5000 }))
    expect(localStorage.getItem(STORAGE_KEY)).toBe('1080')
  })

  it('拖拽期间锁定 body 光标与选区,松手恢复', () => {
    const { onHandleDown } = useDrawerWidth()
    const handle = document.createElement('div')
    handle.addEventListener('pointerdown', onHandleDown as EventListener)

    handle.dispatchEvent(new MouseEvent('pointerdown', { clientX: 800, button: 0, bubbles: true }))
    expect(document.body.style.cursor).toBe('col-resize')
    expect(document.body.style.userSelect).toBe('none')
    window.dispatchEvent(new MouseEvent('pointerup', { clientX: 800 }))
    expect(document.body.style.cursor).toBe('')
    expect(document.body.style.userSelect).toBe('')
  })

  it('非主键按下不进入拖拽', () => {
    const { width, onHandleDown } = useDrawerWidth()
    const handle = document.createElement('div')
    handle.addEventListener('pointerdown', onHandleDown as EventListener)

    handle.dispatchEvent(new MouseEvent('pointerdown', { clientX: 800, button: 2, bubbles: true }))
    window.dispatchEvent(new MouseEvent('pointermove', { clientX: 0 }))
    expect(width.value).toBe(DRAWER_DEFAULT_WIDTH)
  })

  it('方向键微调:←/↑ 变宽、→/↓ 变窄,Shift 大步,Home 复位', () => {
    const { width, onHandleKeydown, reset } = useDrawerWidth()
    const key = (k: string, shift = false) =>
      onHandleKeydown(new KeyboardEvent('keydown', { key: k, shiftKey: shift, bubbles: true }))

    key('ArrowLeft')
    expect(width.value).toBe(DRAWER_DEFAULT_WIDTH + 16)
    key('ArrowUp')
    expect(width.value).toBe(DRAWER_DEFAULT_WIDTH + 32)
    key('ArrowLeft', true)
    expect(width.value).toBe(DRAWER_DEFAULT_WIDTH + 96)
    key('ArrowRight')
    expect(width.value).toBe(DRAWER_DEFAULT_WIDTH + 80)
    key('ArrowDown')
    expect(width.value).toBe(DRAWER_DEFAULT_WIDTH + 64)
    key('Home')
    expect(width.value).toBe(DRAWER_DEFAULT_WIDTH)
    // 每次按键即落盘
    expect(localStorage.getItem(STORAGE_KEY)).toBe(String(DRAWER_DEFAULT_WIDTH))
    // 未监听的键不消费、不落盘
    localStorage.removeItem(STORAGE_KEY)
    key('a')
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
    reset()
  })

  it('reset 恢复默认宽度并落盘', () => {
    localStorage.setItem(STORAGE_KEY, '800')
    const { width, reset } = useDrawerWidth()
    reset()
    expect(width.value).toBe(DRAWER_DEFAULT_WIDTH)
    expect(localStorage.getItem(STORAGE_KEY)).toBe(String(DRAWER_DEFAULT_WIDTH))
  })

  it('作用域销毁(组件卸载)时恢复拖拽期间的 body 样式并移除监听', () => {
    const scope = effectScope()
    const api = scope.run(() => useDrawerWidth())!
    const handle = document.createElement('div')
    handle.addEventListener('pointerdown', api.onHandleDown as EventListener)

    handle.dispatchEvent(new MouseEvent('pointerdown', { clientX: 800, button: 0, bubbles: true }))
    expect(api.dragging.value).toBe(true)
    expect(document.body.style.cursor).toBe('col-resize')

    scope.stop() // 模拟拖拽中抽屉被关闭/卸载

    expect(api.dragging.value).toBe(false)
    expect(document.body.style.cursor).toBe('')
    expect(document.body.style.userSelect).toBe('')
    const before = api.width.value
    window.dispatchEvent(new MouseEvent('pointermove', { clientX: 300 }))
    expect(api.width.value).toBe(before) // 监听已摘除,不再响应拖拽
  })

  it('视口缩窄后按新上限重新夹取宽度', () => {
    localStorage.setItem(STORAGE_KEY, '1000')
    const scope = effectScope()
    const { width } = scope.run(() => useDrawerWidth())!
    expect(width.value).toBe(1000)

    Object.defineProperty(window, 'innerWidth', { value: 700, configurable: true }) // 上限 = 700-320 = 380
    window.dispatchEvent(new Event('resize'))
    expect(width.value).toBe(380)
    scope.stop()
  })

  it('localStorage 不可用时静默降级为内存态', () => {
    vi.stubGlobal('localStorage', {
      getItem: () => {
        throw new Error('denied')
      },
      setItem: () => {
        throw new Error('denied')
      },
    })
    const { width, reset } = useDrawerWidth()
    expect(width.value).toBe(DRAWER_DEFAULT_WIDTH)
    expect(() => reset()).not.toThrow()
    expect(width.value).toBe(DRAWER_DEFAULT_WIDTH)
  })
})
