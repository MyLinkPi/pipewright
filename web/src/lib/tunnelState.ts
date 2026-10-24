/**
 * SSH 隧道状态/模式展示与操作的共享元数据(仿 pm2State.ts)。
 * 总览页(计数/筛选)与行渲染(状态徽标/操作按钮)共用,避免重复。
 */
import type { SshTunnel, TunnelMode, TunnelStatus } from '../api/tunnels'
import { t } from '../i18n'

export interface TunnelStateMeta {
  label: string
  tone: 'green' | 'amber' | 'red' | 'faint'
  pulse: boolean
}

// tone/pulse 是稳定的视觉元数据;label 在访问时经 t() 解析,保证随语言切换更新。
const STATE_VISUAL: Record<TunnelStatus, { labelKey: string; tone: TunnelStateMeta['tone']; pulse: boolean }> = {
  active: { labelKey: 'tunnels.stateActive', tone: 'green', pulse: true },
  connecting: { labelKey: 'tunnels.stateConnecting', tone: 'amber', pulse: true },
  retrying: { labelKey: 'tunnels.stateRetrying', tone: 'red', pulse: true },
  stopped: { labelKey: 'tunnels.stateStopped', tone: 'faint', pulse: false },
  disabled: { labelKey: 'tunnels.stateDisabled', tone: 'faint', pulse: false },
}

export function tunnelStateMeta(s: TunnelStatus): TunnelStateMeta {
  const v = STATE_VISUAL[s] ?? STATE_VISUAL.disabled
  return { label: t(v.labelKey), tone: v.tone, pulse: v.pulse }
}

/** 筛选桶:active / connecting+retrying / stopped+disabled(供筛选与计数)。 */
export type TunnelBucket = 'running' | 'pending' | 'off'

export function tunnelBucket(s: TunnelStatus): TunnelBucket {
  if (s === 'active') return 'running'
  if (s === 'connecting' || s === 'retrying') return 'pending'
  return 'off'
}

export interface TunnelModeMeta {
  label: string
  /** ssh 旗子(技术名不进 i18n)。 */
  flag: string
}

const MODE_VISUAL: Record<TunnelMode, { labelKey: string; flag: string }> = {
  local: { labelKey: 'tunnels.modeLocal', flag: '-L' },
  remote: { labelKey: 'tunnels.modeRemote', flag: '-R' },
  dynamic: { labelKey: 'tunnels.modeDynamic', flag: '-D' },
}

export function tunnelModeMeta(m: TunnelMode): TunnelModeMeta {
  const v = MODE_VISUAL[m] ?? MODE_VISUAL.local
  return { label: t(v.labelKey), flag: v.flag }
}

/** 监听端点文案:bind_port=0 时展示 helper 汇报的实际端口。 */
export function tunnelBindLabel(tn: Pick<SshTunnel, 'bindHost' | 'bindPort' | 'actualPort'>): string {
  const port = tn.bindPort > 0 ? tn.bindPort : tn.actualPort
  if (port > 0 && tn.bindPort === 0) return `${tn.bindHost}:${port}`
  return `${tn.bindHost}:${tn.bindPort}`
}

/** 目标端点文案:dynamic 无目标;连接方式展示服务器名或直连 host@user。 */
export function tunnelTargetLabel(tn: Pick<SshTunnel, 'serverId' | 'serverName' | 'host' | 'user'>): string {
  if (tn.serverId) return tn.serverName || tn.serverId
  return `${tn.user}@${tn.host}`
}

/** 已运行时长(startedAt RFC3339 → 相对时间文案;空/零值返回空串)。 */
export function tunnelUptimeLabel(startedAt: string): string {
  if (!startedAt) return ''
  const ms = Date.now() - new Date(startedAt).getTime()
  if (!Number.isFinite(ms) || ms < 0) return ''
  const sec = Math.floor(ms / 1000)
  const m = Math.floor(sec / 60)
  if (m < 1) return t('time.justNow')
  if (m < 60) return t('time.minAgo', { n: m })
  const h = Math.floor(m / 60)
  if (h < 24) return t('time.hourAgo', { n: h })
  return t('time.dayAgo', { n: Math.floor(h / 24) })
}

export type TunnelAction = 'start' | 'stop' | 'restart'

export interface TunnelActionSpec {
  action: TunnelAction
  label: string
  variant: 'default' | 'ghost'
  /** 需二次确认。 */
  danger?: boolean
}

/** 据状态给出可用操作(顺序即展示顺序;编辑/日志/删除固定在行尾)。 */
export function tunnelActionsFor(status: TunnelStatus): TunnelActionSpec[] {
  switch (status) {
    case 'active':
    case 'connecting':
    case 'retrying':
      return [
        { action: 'restart', label: t('tunnels.actionRestart'), variant: 'ghost', danger: true },
        { action: 'stop', label: t('tunnels.actionStop'), variant: 'ghost', danger: true },
      ]
    case 'stopped':
      return [{ action: 'start', label: t('tunnels.actionStart'), variant: 'default' }]
    default: // disabled
      return [{ action: 'start', label: t('tunnels.actionStart'), variant: 'default' }]
  }
}
