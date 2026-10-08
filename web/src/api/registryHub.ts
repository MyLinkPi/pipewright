/**
 * Registry Hub API — 平台内置本地 Docker registry(制品 + pull-through 缓存双服务)。
 *
 * GET  /api/settings/registry           → RegistryHubConfig
 * PUT  /api/settings/registry           → RegistryHubConfig   (needs CSRF;只写库,不触碰机器)
 * POST /api/settings/registry/deploy    → RegistryDeployResult(needs CSRF;部署/更新控制机栈)
 * GET  /api/settings/registry/status    → RegistryStatus
 * POST /api/settings/registry/prune     → RegistryPruneResult (needs CSRF;按保留策略清理)
 * POST /api/registry/daemon-apply       → RegistryDaemonApplyResponse(needs CSRF;仅显式勾选目标)
 * GET  /api/registry/inspect?serverId=  → RegistryDaemonInspect(local=控制机本机)
 */

import { http } from './http'

export interface RegistryHubConfig {
  enabled: boolean
  /** 其他机器可达的控制机地址(host/IP,无协议无端口)。 */
  externalAddr: string
  /** 控制机出网网卡 IP(仅 UI 预填建议)。 */
  suggestedAddr: string
  /** 缓存 registry 上游(registry API 地址;切换只影响缓存容器)。 */
  upstreamUrl: string
  artifactPort: number
  cachePort: number
  /** 制品/缓存存储目录(配置值;空 = 服务端默认)。 */
  artifactDataDir: string
  cacheDataDir: string
  /** 服务端解析后的生效目录(占位展示用)。 */
  effectiveArtifactDataDir: string
  effectiveCacheDataDir: string
  /** 「证书管理」证书 ID(空 = 明文 HTTP;非空 = 双服务挂载该证书走 HTTPS)。 */
  tlsCertId: string
  keepPerProject: number
  maxAgeDays: number
  /** 只读展示:生成 daemon.json / remoteTag 用的完整地址。 */
  artifactAddr: string
  cacheAddr: string
  updatedAt: string | null
}

export interface SaveRegistryHubInput {
  enabled: boolean
  externalAddr: string
  upstreamUrl: string
  artifactPort: number
  cachePort: number
  artifactDataDir: string
  cacheDataDir: string
  tlsCertId: string
  keepPerProject: number
  maxAgeDays: number
}

export interface RegistryDeployResult {
  ok: boolean
  output: string
  error: string
}

export interface RegistryStatus {
  enabled: boolean
  deployed: boolean
  artifactRunning: boolean
  cacheRunning: boolean
  artifactReachable: boolean
  cacheReachable: boolean
  dataDirBytes: number
  cacheDirBytes: number
  image: string
  error: string
}

export interface RegistryPruneResult {
  ok: boolean
  deletedTags: number
  repos: string[]
  gcOutput: string
  error: string
}

export interface DaemonApplyResult {
  targetId: string
  targetName: string
  isLocal: boolean
  ok: boolean
  backupPath: string
  error: string
  durationMs: number
}

export interface RegistryDaemonApplyResponse {
  results: DaemonApplyResult[]
}

export interface RegistryDaemonInspect {
  targetId: string
  targetName: string
  isLocal: boolean
  mirrors: string[]
  daemonJson: string
  error: string
}

export async function getRegistryHubConfig(): Promise<RegistryHubConfig> {
  return http.get<RegistryHubConfig>('/api/settings/registry')
}

export async function saveRegistryHubConfig(input: SaveRegistryHubInput): Promise<RegistryHubConfig> {
  return http.put<RegistryHubConfig>('/api/settings/registry', input)
}

export async function deployRegistryStack(): Promise<RegistryDeployResult> {
  return http.post<RegistryDeployResult>('/api/settings/registry/deploy', {})
}

export async function getRegistryStatus(): Promise<RegistryStatus> {
  return http.get<RegistryStatus>('/api/settings/registry/status')
}

export async function pruneRegistry(): Promise<RegistryPruneResult> {
  return http.post<RegistryPruneResult>('/api/settings/registry/prune', {})
}

export async function applyRegistryDaemon(serverIds: string[], includeLocal: boolean): Promise<RegistryDaemonApplyResponse> {
  return http.post<RegistryDaemonApplyResponse>('/api/registry/daemon-apply', { serverIds, includeLocal })
}

export async function inspectRegistryDaemon(serverId: string): Promise<RegistryDaemonInspect> {
  return http.get<RegistryDaemonInspect>(`/api/registry/inspect?serverId=${encodeURIComponent(serverId)}`)
}
