// 草稿与部分更新构造:表单只在用户点「保存」时提交 dirty 字段;轮询绝不回写 draft。
import type {
  AliyunGroupConfig,
  ConfigSetRequest,
  ConfigView,
  ProxyConfig,
  Socks5Config,
  StatusSnapshot,
} from '../api/types'
import { orDash } from './format'

export interface JoinDraft {
  networkName: string
  peers: string[]
  virtualIp: string
  hostname: string
  networkSecret: string
  credential: string
}

export interface EasytierDraft {
  binDir: string
  rpcAddr: string
}

export interface SocksDraft {
  enabled: boolean
  port: number
  exitNodes: string[]
}

export interface AliyunDraft {
  accessKeyId: string
  accessKeySecret: string
  groups: AliyunGroupConfig[]
}

export function joinDraft(view: ConfigView['join']): JoinDraft {
  return {
    networkName: view.networkName,
    peers: [...(view.peers ?? [])],
    virtualIp: view.virtualIp,
    hostname: view.hostname,
    networkSecret: '',
    credential: '',
  }
}

export function easytierDraft(view: ConfigView['easytier']): EasytierDraft {
  return { binDir: view.binDir, rpcAddr: view.rpcAddr }
}

export function socksDraft(view: Socks5Config): SocksDraft {
  return { enabled: view.enabled, port: view.port, exitNodes: [...(view.exitNodes ?? [])] }
}

export function aliyunDraft(view: ConfigView['aliyun']): AliyunDraft {
  return {
    accessKeyId: view.accessKeyId,
    accessKeySecret: '',
    groups: (view.groups ?? []).map((g) => ({ ...g })),
  }
}

function cleaned(items: string[]): string[] {
  return items.map((v) => v.trim()).filter((v) => v !== '')
}

function sameList(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i])
}

function sameGroups(a: AliyunGroupConfig[], b: AliyunGroupConfig[]): boolean {
  if (a.length !== b.length) return false
  return a.every(
    (g, i) =>
      g.regionId.trim() === b[i].regionId.trim() &&
      g.securityGroupId.trim() === b[i].securityGroupId.trim() &&
      g.description.trim() === b[i].description.trim(),
  )
}

function sameProxies(a: ProxyConfig[], b: ProxyConfig[]): boolean {
  if (a.length !== b.length) return false
  return a.every((p, i) => p.port === b[i].port && sameList(cleaned(p.exitNodes), cleaned(b[i].exitNodes)))
}

export function buildJoinPatch(draft: JoinDraft, base: JoinDraft): ConfigSetRequest {
  const patch: ConfigSetRequest = {}
  if (draft.networkName.trim() !== base.networkName.trim()) {
    patch.networkName = draft.networkName.trim()
  }
  const peers = cleaned(draft.peers)
  if (!sameList(peers, cleaned(base.peers))) patch.peers = peers
  if (draft.virtualIp.trim() !== base.virtualIp.trim()) patch.virtualIp = draft.virtualIp.trim()
  if (draft.hostname.trim() !== base.hostname.trim()) patch.hostname = draft.hostname.trim()
  if (draft.networkSecret.trim() !== '') patch.networkSecret = draft.networkSecret.trim()
  if (draft.credential.trim() !== '') patch.credential = draft.credential.trim()
  return patch
}

export function buildEasytierPatch(draft: EasytierDraft, base: EasytierDraft): ConfigSetRequest {
  const patch: ConfigSetRequest = {}
  if (draft.binDir.trim() !== base.binDir.trim()) patch.easytierBinDir = draft.binDir.trim()
  if (draft.rpcAddr.trim() !== base.rpcAddr.trim()) patch.easytierRpc = draft.rpcAddr.trim()
  return patch
}

export function buildRoutesPatch(routes: string[], base: string[]): ConfigSetRequest {
  const cleanedRoutes = cleaned(routes)
  return sameList(cleanedRoutes, cleaned(base)) ? {} : { routes: cleanedRoutes }
}

export function buildSocksPatch(
  draft: SocksDraft,
  proxies: ProxyConfig[],
  base: SocksDraft,
  baseProxies: ProxyConfig[],
): ConfigSetRequest {
  const patch: ConfigSetRequest = {}
  if (draft.enabled !== base.enabled) patch.socks5Enabled = draft.enabled
  if (draft.port !== base.port) patch.socks5Port = draft.port
  const exits = cleaned(draft.exitNodes)
  if (!sameList(exits, cleaned(base.exitNodes))) patch.socks5ExitNodes = exits
  const cleanProxies = proxies.map((p) => ({ port: p.port, exitNodes: cleaned(p.exitNodes) }))
  if (!sameProxies(cleanProxies, baseProxies)) patch.socks5Proxies = cleanProxies
  return patch
}

export function buildAliyunPatch(draft: AliyunDraft, base: AliyunDraft): ConfigSetRequest {
  const patch: ConfigSetRequest = {}
  if (draft.accessKeyId.trim() !== base.accessKeyId.trim()) {
    patch.aliyunAccessKeyId = draft.accessKeyId.trim()
  }
  if (draft.accessKeySecret.trim() !== '') {
    patch.aliyunAccessKeySecret = draft.accessKeySecret.trim()
  }
  const groups = draft.groups.map((g) => ({
    regionId: g.regionId.trim(),
    securityGroupId: g.securityGroupId.trim(),
    description: g.description.trim(),
  }))
  if (!sameGroups(groups, base.groups)) patch.aliyunGroups = groups
  return patch
}

export function isPatchEmpty(patch: ConfigSetRequest): boolean {
  return Object.keys(patch).length === 0
}

export interface ConfigSummaryRow {
  key: string
  label: string
  configured: string
  actual: string
}

// describeConfig 生成「配置总览」行:配置值来自 GET /api/config,实际值来自状态快照
// (两者可能不同,例如虚拟 IP 留空 = DHCP,实际值来自 easytier.selfIp)。
export function describeConfig(
  view: ConfigView | null,
  snap: StatusSnapshot | null,
): ConfigSummaryRow[] {
  if (!view) return []
  const auth = view.join.hasCredential
    ? '临时凭据（Secure Mode）'
    : view.join.hasSecret
      ? '长期密钥（Secure Mode）'
      : '未配置'
  const actualState = !snap
    ? '—'
    : !snap.configured
      ? '未配置'
      : snap.enabled
        ? snap.easytier?.selfIp
          ? `已连接 ${snap.easytier.selfIp}`
          : '连接中…'
        : '已断开'
  const socksState = snap
    ? snap.socks5.enabled
      ? `监听 127.0.0.1:${snap.socks5.port}`
      : '未启用'
    : '—'
  const runningProxies = snap?.proxies.filter((p) => p.running).length ?? 0
  return [
    {
      key: 'enabled',
      label: '连接开关',
      configured: view.enabled ? '已启用' : '已关闭',
      actual: actualState,
    },
    {
      key: 'network',
      label: '网络名',
      configured: orDash(view.join.networkName),
      actual: orDash(view.join.networkName),
    },
    {
      key: 'auth',
      label: '入网认证',
      configured: auth,
      actual: snap ? 'Secure Mode（固定开启）' : '—',
    },
    {
      key: 'virtualIp',
      label: '虚拟 IP',
      configured: view.join.virtualIp === '' ? '自动分配（DHCP）' : view.join.virtualIp,
      actual: orDash(snap?.easytier?.selfIp ?? ''),
    },
    {
      key: 'hostname',
      label: '主机名',
      configured: view.join.hostname === '' ? '计算机名（默认）' : view.join.hostname,
      actual: orDash(snap?.join.hostname ?? ''),
    },
    {
      key: 'peers',
      label: '对端地址',
      configured: view.join.peers.length > 0 ? view.join.peers.join(', ') : '未配置',
      actual: '—',
    },
    {
      key: 'routes',
      label: '网段路由',
      configured: view.routes.length > 0 ? `${view.routes.length} 个网段` : '不路由任何网段',
      actual: snap ? `${snap.routes.length} 个已启用` : '—',
    },
    {
      key: 'socks5',
      label: '本地 SOCKS5',
      configured: view.socks5.enabled
        ? `启用 · 127.0.0.1:${view.socks5.port}`
        : '未启用',
      actual: socksState,
    },
    {
      key: 'proxies',
      label: '多出口代理',
      configured: view.proxies.length > 0 ? `${view.proxies.length} 条` : '未配置',
      actual: snap ? `${runningProxies}/${snap.proxies.length} 运行中` : '—',
    },
    {
      key: 'easytier',
      label: 'EasyTier',
      configured: `目录 ${orDash(view.easytier.binDir)} · RPC ${orDash(view.easytier.rpcAddr)}`,
      actual: snap
        ? `${orDash(snap.binaries.version)} · ${orDash(snap.binaries.corePath)}`
        : '—',
    },
    {
      key: 'aliyun',
      label: '阿里云安全组',
      configured: `${maskKey(view.aliyun.accessKeyId)} · Secret ${
        view.aliyun.hasSecret ? '已配置' : '未配置'
      } · ${view.aliyun.groups.length} 组`,
      actual: snap
        ? snap.securityGroup.configured
          ? `最近同步 ${snap.securityGroup.lastRun === '' ? '尚未同步' : snap.securityGroup.lastRun}`
          : '未生效'
        : '—',
    },
  ]
}

function maskKey(key: string): string {
  const v = key.trim()
  if (v === '') return 'AK 未配置'
  if (v.length <= 6) return `AK ${v.slice(0, 2)}…`
  return `AK ${v.slice(0, 4)}…${v.slice(-2)}`
}
