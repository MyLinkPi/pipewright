// 纯函数:把状态快照派生为界面文案/行数据(可单测,不依赖 Vue)。
import type {
  EasyTierLink,
  ProxyRoute,
  StatusSnapshot,
} from '../api/types'
import { latency, orDash } from './format'

export type ConnectionState =
  | 'service-down'
  | 'not-configured'
  | 'connecting'
  | 'connected'
  | 'disconnected'

export function connectionState(
  snap: StatusSnapshot | null,
  offline: boolean,
): ConnectionState {
  if (offline || !snap) return 'service-down'
  if (!snap.configured) return 'not-configured'
  if (snap.supervisor.running && snap.easyTier && snap.easyTier.selfIp !== '') {
    return 'connected'
  }
  if (snap.enabled) return 'connecting'
  return 'disconnected'
}

export interface StateMeta {
  label: string
  tone: 'ok' | 'warn' | 'idle'
  hint: string
}

export function stateMeta(state: ConnectionState, snap: StatusSnapshot | null): StateMeta {
  switch (state) {
    case 'service-down':
      return {
        label: '服务未运行',
        tone: 'warn',
        hint: '后台服务未安装或未启动；安装后组网随系统启动',
      }
    case 'not-configured':
      return {
        label: '未配置',
        tone: 'warn',
        hint: '导入平台导出配置，或在「连接参数」逐项填写',
      }
    case 'connected':
      return {
        label: '已连接',
        tone: 'ok',
        hint: `虚拟 IP ${orDash(snap?.easyTier?.selfIp ?? '')}`,
      }
    case 'connecting':
      return {
        label: '连接中…',
        tone: 'idle',
        hint: firstHint(snap) ?? '正在打洞/连接对端',
      }
    default:
      return {
        label: '已断开',
        tone: 'idle',
        hint: '配置保留；可点「连接」恢复',
      }
  }
}

function firstHint(snap: StatusSnapshot | null): string | undefined {
  if (!snap) return undefined
  if (snap.binaries.corePath === '') return '等待 EasyTier 二进制就位'
  if (snap.easyTier?.err) return snap.easyTier.err
  if (snap.supervisor.lastErr) return snap.supervisor.lastErr
  return undefined
}

export type PrimaryActionKind =
  | 'install-service'
  | 'import'
  | 'connect'
  | 'disconnect'

export interface PrimaryAction {
  kind: PrimaryActionKind
  label: string
}

export function primaryAction(state: ConnectionState): PrimaryAction {
  switch (state) {
    case 'service-down':
      return { kind: 'install-service', label: '安装并启动服务' }
    case 'not-configured':
      return { kind: 'import', label: '导入配置' }
    case 'connected':
      return { kind: 'disconnect', label: '断开' }
    default:
      return { kind: 'connect', label: '连接' }
  }
}

export interface PeerRow {
  key: string
  virtualIp: string
  hostname: string
  latency: string
  kind: string
  version: string
}

export function peerRows(links: EasyTierLink[] | undefined): PeerRow[] {
  return (links ?? []).map((link) => ({
    key: link.peerId || `${link.virtualIp}-${link.hostname}`,
    virtualIp: orDash(link.virtualIp),
    hostname: orDash(link.hostname),
    latency: latency(link.latencyMs),
    kind: link.relay ? `中继${link.relayVia ? ` via ${link.relayVia}` : ''}` : `直连 ${orDash(link.tunnel)}`,
    version: orDash(link.version),
  }))
}

export interface RouteRow {
  cidr: string
  gateway: string
  backups: number
  enabled: boolean
}

export function routeRows(snap: StatusSnapshot | null): RouteRow[] {
  const cidrs = snap?.easyTier?.proxyCidrs ?? []
  const enabled = new Set(snap?.routes ?? [])
  const byCIDR = new Map<string, ProxyRoute>()
  for (const route of snap?.easyTier?.proxyRoutes ?? []) {
    byCIDR.set(route.cidr, route)
  }
  return cidrs.map((cidr) => {
    const route = byCIDR.get(cidr)
    const label = route
      ? `${orDash(route.viaName)}(${orDash(route.viaIp)})`
      : '—'
    return {
      cidr,
      gateway: label,
      backups: route && route.vias.length > 1 ? route.vias.length - 1 : 0,
      enabled: enabled.has(cidr),
    }
  })
}

export function enabledRouteSet(snap: StatusSnapshot | null): Set<string> {
  return new Set(snap?.routes ?? [])
}
