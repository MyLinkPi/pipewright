/**
 * Apt Cache API — 平台内置本地 apt 包缓存(apt-cacher-ng)。
 *
 * GET  /api/settings/apt-cache           → AptCacheConfig
 * PUT  /api/settings/apt-cache           → AptCacheConfig   (needs CSRF;只写库,不触碰机器)
 * POST /api/settings/apt-cache/deploy    → AptCacheDeployResult(needs CSRF;部署/更新控制机栈)
 * POST /api/settings/apt-cache/upgrade-image → AptCacheImageUpgradeResult(needs CSRF;pull 升级栈镜像)
 * GET  /api/settings/apt-cache/status    → AptCacheStatus
 * POST /api/settings/apt-cache/clear-cache → AptCacheDeployResult(needs CSRF;清空缓存目录)
 * POST /api/apt-cache/proxy-apply        → AptProxyApplyResponse(needs CSRF;仅显式勾选目标)
 * GET  /api/apt-cache/inspect?serverId=  → AptProxyInspect(local=控制机本机)
 */

import { http } from './http'

export interface AptCacheConfig {
  enabled: boolean
  /** 其他机器可达的控制机地址(host/IP,无协议无端口)。 */
  externalAddr: string
  /** 控制机出网网卡 IP(仅 UI 预填建议)。 */
  suggestedAddr: string
  port: number
  /** 缓存存储目录(配置值;空 = 服务端默认)。 */
  dataDir: string
  /** 服务端解析后的生效目录(占位展示用)。 */
  effectiveDataDir: string
  /** 「证书管理」证书 ID(空 = 明文 HTTP;非空 = TLS 前端接管走 HTTPS)。 */
  tlsCertId: string
  tlsEnabled: boolean
  /** 只读展示:生成代理片段用的完整地址。 */
  addr: string
  updatedAt: string | null
}

export interface SaveAptCacheInput {
  enabled: boolean
  externalAddr: string
  port: number
  dataDir: string
  tlsCertId: string
}

export interface AptCacheDeployResult {
  ok: boolean
  output: string
  error: string
}

/** 栈镜像升级结果(changed=false = 已是最新,未动容器;缓存在宿主卷,重建不丢)。 */
export interface AptCacheImageUpgradeResult {
  ok: boolean
  changed: boolean
  image: string
  /** 升级前/后本地镜像 ID(截短 sha;首次/本地无镜像时 before 为空)。 */
  before: string
  after: string
  output: string
  error: string
}

export interface AptCacheStatus {
  enabled: boolean
  deployed: boolean
  running: boolean
  /** TLS 前端容器 running(仅 tlsEnabled 时有意义)。 */
  tlsRunning: boolean
  /** 配置是否走 TLS 前端。 */
  tlsEnabled: boolean
  reachable: boolean
  cacheBytes: number
  image: string
  error: string
}

export interface AptProxyApplyResult {
  targetId: string
  targetName: string
  isLocal: boolean
  ok: boolean
  backupPath: string
  error: string
  durationMs: number
}

export interface AptProxyApplyResponse {
  results: AptProxyApplyResult[]
}

export interface AptProxyInspect {
  targetId: string
  targetName: string
  isLocal: boolean
  /** apt-config 实际生效的 http 代理值(未配置 = 空串)。 */
  proxyHttp: string
  proxyHttps: string
  /** 片段文件原文(截断)。 */
  confFile: string
  error: string
}

export async function getAptCacheConfig(): Promise<AptCacheConfig> {
  return http.get<AptCacheConfig>('/api/settings/apt-cache')
}

export async function saveAptCacheConfig(input: SaveAptCacheInput): Promise<AptCacheConfig> {
  return http.put<AptCacheConfig>('/api/settings/apt-cache', input)
}

export async function deployAptCacheStack(): Promise<AptCacheDeployResult> {
  return http.post<AptCacheDeployResult>('/api/settings/apt-cache/deploy', {})
}

/** 升级栈镜像(apt-cacher-ng;TLS 态含 nginx 前端):pull 后按镜像 ID 比对,变了才重建容器。 */
export async function upgradeAptCacheImage(): Promise<AptCacheImageUpgradeResult> {
  return http.post<AptCacheImageUpgradeResult>('/api/settings/apt-cache/upgrade-image', {})
}

export async function getAptCacheStatus(): Promise<AptCacheStatus> {
  return http.get<AptCacheStatus>('/api/settings/apt-cache/status')
}

export async function clearAptCache(): Promise<AptCacheDeployResult> {
  return http.post<AptCacheDeployResult>('/api/settings/apt-cache/clear-cache', {})
}

export async function applyAptProxy(serverIds: string[], includeLocal: boolean): Promise<AptProxyApplyResponse> {
  return http.post<AptProxyApplyResponse>('/api/apt-cache/proxy-apply', { serverIds, includeLocal })
}

export async function inspectAptProxy(serverId: string): Promise<AptProxyInspect> {
  return http.get<AptProxyInspect>(`/api/apt-cache/inspect?serverId=${encodeURIComponent(serverId)}`)
}
