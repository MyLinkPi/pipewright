/**
 * useDrawerWidth — 右侧设置抽屉的可拖拽宽度。
 *
 * 画布右侧的 JobDrawer / StageDrawer 左缘各有一个拖拽把手:向左拖变宽、
 * 向右拖变窄,宽度持久化到 localStorage(`pipewright_drawer_width`),
 * 两个抽屉共用同一宽度(同一个「右栏」概念,且二者互斥渲染)。
 *   - 宽度夹在 [DRAWER_MIN_WIDTH, min(DRAWER_MAX_WIDTH, 视口宽-320)] 内,
 *     保证拖到最宽时画布仍留有可操作区域;窗口尺寸变化时按新上限重新夹取;
 *   - 双击把手恢复默认宽度;
 *   - 把手可聚焦,方向键微调(←/↑ 变宽、→/↓ 变窄),Home 复位;
 *   - 组件卸载时清理拖拽期间设置的全局光标/选区与监听(半途关闭抽屉也不残留);
 *   - localStorage 不可用(隐私模式/配额)时静默降级为仅本次会话生效。
 */
import { getCurrentScope, onScopeDispose, ref } from 'vue'

const STORAGE_KEY = 'pipewright_drawer_width'

/** 默认宽度 = 历史固定宽度,老用户无感知。 */
export const DRAWER_DEFAULT_WIDTH = 440
export const DRAWER_MIN_WIDTH = 360
/** 绝对上限;实际还受「视口宽 - 320」动态收紧。 */
export const DRAWER_MAX_WIDTH = 1080
/** 方向键单次微调步长(px)。 */
const KEY_STEP = 16
/** 画布最小保留宽度:最宽只能拖到 innerWidth - CANVAS_RESERVE。 */
const CANVAS_RESERVE = 320

function readStored(): number | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw === null) return null
    const v = Number(raw)
    if (!Number.isFinite(v)) return null
    return Math.round(v)
  } catch {
    return null // 隐私模式/配额满 → 默认宽度
  }
}

function writeStored(width: number): void {
  try {
    localStorage.setItem(STORAGE_KEY, String(width))
  } catch {
    // 静默降级:仅本次会话内存态生效。
  }
}

/** 当前视口允许的最大宽度;极端窄窗口下仍保证不小于最小宽度。 */
function viewportMax(): number {
  return Math.max(DRAWER_MIN_WIDTH, Math.min(DRAWER_MAX_WIDTH, window.innerWidth - CANVAS_RESERVE))
}

function clampWithin(width: number, max: number): number {
  return Math.round(Math.min(max, Math.max(DRAWER_MIN_WIDTH, width)))
}

export function useDrawerWidth() {
  /** 视口允许的上限(resize 时更新,供 aria-valuemax 与夹取共用)。 */
  const maxWidth = ref(viewportMax())
  const width = ref(clampWithin(readStored() ?? DRAWER_DEFAULT_WIDTH, maxWidth.value))
  /** 拖拽进行中(true 时把手高亮,供 :class 绑定)。 */
  const dragging = ref(false)
  /** 拖拽期间的全局副作用清理(pointerup/cancel 与组件卸载共用;幂等)。 */
  let endDrag: (() => void) | null = null

  function clamp(value: number): number {
    return clampWithin(value, maxWidth.value)
  }

  function setWidth(next: number): void {
    width.value = clamp(next)
  }

  function reset(): void {
    width.value = clamp(DRAWER_DEFAULT_WIDTH)
    writeStored(width.value)
  }

  /** 把手 pointerdown:开始拖拽;向左拖(clientX 减小)变宽。 */
  function onHandleDown(e: PointerEvent): void {
    if (e.button !== 0) return
    e.preventDefault()
    const startX = e.clientX
    const startWidth = width.value
    dragging.value = true
    // preventDefault 会连带取消默认聚焦;主动聚焦把手,保证点击后仍可用方向键微调。
    if (e.currentTarget instanceof HTMLElement) e.currentTarget.focus()
    // 拖拽期间锁定光标与选区,避免划选抽屉内文本。
    document.body.style.cursor = 'col-resize'
    document.body.style.userSelect = 'none'
    const el = e.currentTarget
    if (el instanceof HTMLElement && typeof el.setPointerCapture === 'function') {
      el.setPointerCapture(e.pointerId)
    }
    const onMove = (ev: PointerEvent): void => {
      setWidth(startWidth + (startX - ev.clientX))
    }
    const onUp = (): void => {
      endDrag?.()
      writeStored(width.value) // 拖拽结束时落盘一次即可
    }
    endDrag = (): void => {
      dragging.value = false
      document.body.style.cursor = ''
      document.body.style.userSelect = ''
      window.removeEventListener('pointermove', onMove)
      window.removeEventListener('pointerup', onUp)
      window.removeEventListener('pointercancel', onUp)
      endDrag = null
    }
    window.addEventListener('pointermove', onMove)
    window.addEventListener('pointerup', onUp)
    window.addEventListener('pointercancel', onUp)
  }

  /** 把手键盘微调:←/↑ 变宽、→/↓ 变窄(Shift 大步),Home 复位。 */
  function onHandleKeydown(e: KeyboardEvent): void {
    const step = e.shiftKey ? KEY_STEP * 4 : KEY_STEP
    if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') {
      e.preventDefault()
      setWidth(width.value + step)
    } else if (e.key === 'ArrowRight' || e.key === 'ArrowDown') {
      e.preventDefault()
      setWidth(width.value - step)
    } else if (e.key === 'Home') {
      e.preventDefault()
      reset()
    } else {
      return
    }
    writeStored(width.value)
  }

  /** 窗口尺寸变化:重算上限并把当前宽度夹回去,避免抽屉挤压画布。 */
  function onViewportResize(): void {
    maxWidth.value = viewportMax()
    setWidth(width.value)
  }

  // 仅在组件 setup 作用域内挂全局监听;直接调用(单测/工具)时不注册,避免泄漏。
  if (getCurrentScope()) {
    window.addEventListener('resize', onViewportResize)
    onScopeDispose(() => {
      window.removeEventListener('resize', onViewportResize)
      endDrag?.()
    })
  }

  return {
    width,
    dragging,
    /** 宽度下限(供 aria-valuemin)。 */
    minWidth: DRAWER_MIN_WIDTH,
    /** 宽度上限(resize 时随视口收紧;供 aria-valuemax)。 */
    maxWidth,
    onHandleDown,
    onHandleKeydown,
    reset,
  }
}
