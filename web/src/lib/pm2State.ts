/**
 * PM2 状态展示与生命周期操作的共享元数据(仿 containerState.ts)。
 * 总览页(计数/筛选)与单机卡片(渲染/操作)共用,避免重复。
 */
import type { Pm2Status } from '../api/pm2'
import type { ServiceAction } from '../api/servers'
import { ACTION_HINTS } from './containerState'
import { t } from '../i18n'

/** pm2 可用生命周期操作(后端 ValidateServiceParams type=pm2 白名单同款)。 */
export type Pm2Action = Extract<ServiceAction, 'start' | 'stop' | 'restart' | 'reload' | 'delete'>

/** 五态归并三桶,供筛选与计数:online(含 launching)/ stopped(含 stopping)/ errored。 */
export type Pm2Bucket = 'online' | 'stopped' | 'errored'

export function pm2Bucket(s: Pm2Status): Pm2Bucket {
  if (s === 'online' || s === 'launching') return 'online'
  if (s === 'stopping' || s === 'stopped') return 'stopped'
  return 'errored'
}

export interface Pm2StateMeta {
  label: string
  tone: 'green' | 'amber' | 'red' | 'faint'
  pulse: boolean
}

// tone/pulse 是稳定的视觉元数据;label 在访问时经 t() 解析,保证随语言切换更新。
const STATE_VISUAL: Record<Pm2Status, { labelKey: string; tone: Pm2StateMeta['tone']; pulse: boolean }> = {
  online: { labelKey: 'pm2.stateOnline', tone: 'green', pulse: true },
  launching: { labelKey: 'pm2.stateLaunching', tone: 'amber', pulse: true },
  stopping: { labelKey: 'pm2.stateStopping', tone: 'amber', pulse: false },
  stopped: { labelKey: 'pm2.stateStopped', tone: 'faint', pulse: false },
  errored: { labelKey: 'pm2.stateErrored', tone: 'red', pulse: false },
}

export function pm2StateMeta(s: Pm2Status): Pm2StateMeta {
  const v = STATE_VISUAL[s] ?? STATE_VISUAL.stopped
  return { label: t(v.labelKey), tone: v.tone, pulse: v.pulse }
}

export interface Pm2ActionSpec {
  action: Pm2Action
  label: string
  variant: 'default' | 'ghost' | 'danger'
  /** 需二次确认。 */
  danger?: boolean
}

/** 据进程状态给出可用生命周期操作集(顺序即展示顺序)。 */
export function pm2ActionsFor(status: Pm2Status): Pm2ActionSpec[] {
  switch (status) {
    case 'online':
    case 'launching':
      return [
        { action: 'restart', label: t('pm2.actionRestart'), variant: 'default', danger: true },
        { action: 'reload', label: t('pm2.actionReload'), variant: 'ghost' },
        { action: 'stop', label: t('pm2.actionStop'), variant: 'ghost', danger: true },
        { action: 'delete', label: t('pm2.actionDelete'), variant: 'ghost', danger: true },
      ]
    case 'stopping':
      return [{ action: 'stop', label: t('pm2.actionStop'), variant: 'ghost', danger: true }]
    default: // stopped / errored
      return [
        { action: 'start', label: t('pm2.actionStart'), variant: 'default' },
        { action: 'delete', label: t('pm2.actionDelete'), variant: 'ghost', danger: true },
      ]
  }
}

/** 操作按钮 hover 提示:复用 labels 命名空间的通用 hint(含 pm2 专用 reload/delete)。 */
export const PM2_ACTION_HINTS: Partial<Record<Pm2Action, string>> = {
  get start() {
    return ACTION_HINTS.start
  },
  get restart() {
    return ACTION_HINTS.restart
  },
  get stop() {
    return ACTION_HINTS.stop
  },
  get reload() {
    return ACTION_HINTS.reload
  },
  get delete() {
    return ACTION_HINTS.delete
  },
}

/**
 * 破坏性操作的二次确认文案。
 * body / confirmLabel 用 getter,title 是函数 → t() 均在访问时求值,保持随语言切换更新。
 */
export const PM2_DANGER_COPY: Record<
  'restart' | 'stop' | 'delete',
  { title: (n: string) => string; body: string; confirmLabel: string }
> = {
  restart: {
    title: (n) => t('pm2.dangerRestartTitle', { n }),
    get body() {
      return t('pm2.dangerRestartBody')
    },
    get confirmLabel() {
      return t('pm2.dangerRestartConfirm')
    },
  },
  stop: {
    title: (n) => t('pm2.dangerStopTitle', { n }),
    get body() {
      return t('pm2.dangerStopBody')
    },
    get confirmLabel() {
      return t('pm2.dangerStopConfirm')
    },
  },
  delete: {
    title: (n) => t('pm2.dangerDeleteTitle', { n }),
    get body() {
      return t('pm2.dangerDeleteBody')
    },
    get confirmLabel() {
      return t('pm2.dangerDeleteConfirm')
    },
  },
}

/** 行 key:cluster 模式同名多实例,须连同 pm_id 才唯一。 */
export function pm2RowKey(p: { id: number; name: string }): string {
  return `${p.name}#${p.id}`
}

/** 已运行时长(pm_uptime 毫秒时间戳 → 相对时间文案);0/未来值返回空串)。 */
export function pm2UptimeLabel(uptimeMs: number): string {
  if (!uptimeMs) return ''
  const sec = Math.max(0, Math.floor((Date.now() - uptimeMs) / 1000))
  const m = Math.floor(sec / 60)
  if (m < 1) return t('time.justNow')
  if (m < 60) return t('time.minAgo', { n: m })
  const h = Math.floor(m / 60)
  if (h < 24) return t('time.hourAgo', { n: h })
  return t('time.dayAgo', { n: Math.floor(h / 24) })
}

/** 内存字节 → 人读(与 ServerMetricsCard 同款单位)。 */
export function formatPm2Bytes(bytes: number): string {
  if (!bytes || bytes <= 0) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let v = bytes
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${i === 0 ? v : v.toFixed(1)} ${units[i]}`
}
