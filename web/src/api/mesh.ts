/**
 * Mesh (cluster) API — peer federation over EasyTier.
 *
 * Admin面(session 鉴权,挂 /api/mesh/* 管理组):
 *   GET  /api/mesh/settings   → MeshSettings       PUT → MeshSettings (CSRF)
 *   GET  /api/mesh/acl        → MeshACL            PUT → MeshACL (CSRF)
 *   GET  /api/mesh/peers      → { items: MeshPeer[] }
 *   POST /api/mesh/peers      → { ok } (CSRF, 手工补录)
 *   DELETE /api/mesh/peers/:id → { ok } (CSRF)
 *   GET  /api/mesh/overview   → MeshOverview(集群页聚合:EasyTier 链路 + 节点表)
 *   GET  /api/mesh/peers/:id/servers|projects|runs → 代理对端查询(未授权 → degraded)
 *   GET  /api/projects/:id/pushed-revisions → 被推送源项目的修订列表(运行对话框选择器)
 */

import { http } from './http'

export interface MeshSettings {
  nodeId: string
  name: string
  enabled: boolean
  publicKey: string
  apiPort: number
  easytierMode: 'managed' | 'external'
  easytierRpc: string
  networkName: string
  virtualIp: string
  peers: string[]
  /** Admin-configured subnet proxy CIDRs advertised by this node (client route targets). */
  proxyNetworksCustom: string[]
  /** Explicitly accepted cross-env advertised CIDRs (route whitelist manual items; only
   *  gateway /32s are accepted automatically - see cluster route isolation). */
  acceptNetworksCustom: string[]
  /** Allow this node to act as an exit node for client local proxies (foreign-network NAT, off by default). */
  enableExitNode: boolean
  /** Secure Mode (v2.6+): Noise XX handshake auth + temp-credential joins. Flipped by the
   *  cluster-wide switch protocol; direct PUT is the recovery hatch only. */
  secureMode: boolean
  hasSecret: boolean
}

export interface MeshACL {
  visibleTo: string[]
  pushAllowed: string[]
  triggerAllowed: string[]
  forwardAllowed: string[]
  /** 0066 cluster-shared build machines: peers allowed to submit shared build jobs. */
  buildAllowed: string[]
  /** Peers allowed to deliver/abort Secure Mode cluster-wide switch instructions. */
  switchAllowed: string[]
}

export interface MeshPeer {
  nodeId: string
  name: string
  virtualIp: string
  apiPort: number
  publicKey: string
  version: string
  source: 'auto' | 'manual'
  publicUrl: string
  lastSeen: string
  updatedAt: string
}

export interface MeshOverviewPeer extends MeshPeer {
  online: boolean
  linkType: '' | 'p2p' | 'relay'
  relayVia: string
  latencyMs: number
}

export interface EasyTierLink {
  peerId: string
  virtualIp: string
  hostname: string
  version: string
  latencyMs: number
  lossRate: string
  rxBytes: number
  txBytes: number
  tunnel: string
  relay: boolean
  relayVia: string
}

/** 某网段的一个通告网关(relay=true 时 relayVia 为到达该网关的中继节点)。 */
export interface ProxyRouteVia {
  ip: string
  name: string
  relay: boolean
  relayVia: string
}

/** 网段→网关:多节点通告同一网段互为备份,vias 收录全部网关,viaIp/viaName 为主网关。 */
export interface ProxyRoute {
  cidr: string
  viaIp: string
  viaName: string
  vias: ProxyRouteVia[]
}

export interface EasyTierStatus {
  ok: boolean
  err: string
  selfIp: string
  hostname: string
  version: string
  proxyCidrs: string[]
  proxyRoutes: ProxyRoute[]
}

export interface MeshSupervisorRuntime {
  running: boolean
  lastErr: string
  restarts: number
  startedAt: string
}

export interface EasyTierBinaries {
  corePath: string
  cliPath: string
  /** Installed version tag (empty = unknown / manually placed). */
  version: string
  downloading: boolean
  lastErr: string
}

export interface MeshOverview {
  settings: MeshSettings
  externalMode: boolean
  easytier: EasyTierStatus
  supervisor?: MeshSupervisorRuntime
  binaries?: EasyTierBinaries
  peers: MeshOverviewPeer[]
}

/** 对端代理查询的降级结构(未授权/不可达;集群页渲染占位徽标)。 */
export interface MeshProxyDegraded {
  available: false
  reason: 'forbidden' | 'unreachable'
}

export interface MeshPeerProject {
  id: string
  name: string
  sourceType: string
  hasPipeline: boolean
  params: Array<{ name: string; type: string; required: boolean; default: string }>
}

export interface MeshPeerRun {
  id: string
  status: string
  trigger: string
  actor: string
  createdAt: string
  finishedAt?: string
}

export interface PushedRevision {
  id: string
  label: string
  items: string[]
  originNode: string
  createdAt: string
}

export function getMeshSettings(): Promise<MeshSettings> {
  return http.get('/api/mesh/settings')
}

export function saveMeshSettings(body: Partial<MeshSettings> & { networkSecret?: string }): Promise<MeshSettings> {
  return http.put('/api/mesh/settings', body)
}

/**
 * Reveal the saved network secret (POST: audited, same pattern as credential reveal).
 * The secret is a cluster-shared key the admin must re-enter on every joining node —
 * it has to be viewable, unlike outbound credentials.
 */
export function revealMeshNetworkSecret(): Promise<{ secret: string }> {
  return http.post('/api/mesh/settings/reveal-secret')
}

/**
 * Export the Win11 client (pwclient) join config as a YAML file download
 * (network name + plaintext secret + peers + advertised subnet routes).
 * Raw fetch instead of the JSON helper: success is a file attachment; failure is
 * the canonical {error:{code,message}} envelope.
 */
/**
 * Export the Win11 client (pwclient) join config as YAML text (for the
 * copy-to-clipboard dialog; the pwclient GUI pastes it directly).
 * Raw fetch instead of the JSON helper: success is a YAML attachment; failure is
 * the canonical {error:{code,message}} envelope.
 */
export async function fetchMeshClientConfigText(): Promise<string> {
  const resp = await fetch('/api/mesh/client-config', { credentials: 'same-origin' })
  if (!resp.ok) {
    let message = `HTTP ${resp.status}`
    try {
      const body = (await resp.json()) as { error?: { message?: string } }
      if (body.error?.message) message = body.error.message
    } catch { /* keep HTTP status text */ }
    throw new Error(message)
  }
  return resp.text()
}

export async function downloadMeshClientConfig(): Promise<void> {
  const resp = await fetch('/api/mesh/client-config', { credentials: 'same-origin' })
  if (!resp.ok) {
    let message = `HTTP ${resp.status}`
    try {
      const body = (await resp.json()) as { error?: { message?: string } }
      if (body.error?.message) message = body.error.message
    } catch { /* keep HTTP status text */ }
    throw new Error(message)
  }
  const disposition = resp.headers.get('content-disposition') ?? ''
  const m = disposition.match(/filename="?([^";]+)"?/)
  const blob = await resp.blob()
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = m?.[1] ?? 'pipewright-client.yaml'
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

export function getMeshACL(): Promise<MeshACL> {
  return http.get('/api/mesh/acl')
}

export function saveMeshACL(acl: MeshACL): Promise<MeshACL> {
  return http.put('/api/mesh/acl', acl)
}

export function listMeshPeers(): Promise<{ items: MeshPeer[] }> {
  return http.get('/api/mesh/peers')
}

export function saveMeshPeer(peer: Pick<MeshPeer, 'nodeId' | 'name' | 'virtualIp' | 'apiPort' | 'publicUrl'>): Promise<{ ok: boolean }> {
  return http.post('/api/mesh/peers', peer)
}

export function deleteMeshPeer(nodeId: string): Promise<{ ok: boolean }> {
  return http.delete(`/api/mesh/peers/${encodeURIComponent(nodeId)}`)
}

export function getMeshOverview(): Promise<MeshOverview> {
  return http.get('/api/mesh/overview')
}

/** Manually trigger the EasyTier binary auto-download (async; poll overview for progress). */
export function triggerEasyTierDownload(): Promise<{ ok: boolean }> {
  return http.post('/api/mesh/easytier/download')
}

/** 代理查询对端服务器指标(返回原始 items 结构或 degraded)。 */
export function fetchPeerServers(nodeId: string): Promise<unknown> {
  return http.get(`/api/mesh/peers/${encodeURIComponent(nodeId)}/servers`)
}

export function fetchPeerProjects(nodeId: string): Promise<{ items: MeshPeerProject[] } | MeshProxyDegraded> {
  return http.get(`/api/mesh/peers/${encodeURIComponent(nodeId)}/projects`)
}

export function fetchPeerRuns(nodeId: string, projectId: string): Promise<{ items: MeshPeerRun[] } | MeshProxyDegraded> {
  return http.get(`/api/mesh/peers/${encodeURIComponent(nodeId)}/runs?projectId=${encodeURIComponent(projectId)}`)
}

export function listPushedRevisions(projectId: string): Promise<{ items: PushedRevision[] }> {
  return http.get(`/api/projects/${encodeURIComponent(projectId)}/pushed-revisions`)
}

/**
 * ── Cluster DNS mapping (A record of the specified domain → this machine's virtual IP; DDNS-style automatic following) ──
 * GET    /api/mesh/dns-records        → { items: MeshDnsRecord[] }
 * POST   /api/mesh/dns-records        → MeshDnsRecord             (CSRF)
 * PUT    /api/mesh/dns-records/:id    → MeshDnsRecord             (CSRF)
 * DELETE /api/mesh/dns-records/:id    → { ok }                    (CSRF; local only, cloud records are preserved)
 * POST   /api/mesh/dns-records/sync   → MeshDnsSyncOutcome        (CSRF; sync now)
 */
export interface MeshDnsRecord {
  id: string
  providerId: string
  fqdn: string
  enabled: boolean
  /** The virtual IP most recently successfully written to the A record (empty = never synced successfully). */
  lastIp: string
  lastSyncedAt: string
  /** Human-readable reason for the most recent sync failure (empty = none). */
  lastError: string
  createdAt: string
  updatedAt: string
  /** Server-side joined echo: provider display name / type; providerGone = the provider has been deleted. */
  providerName: string
  providerType: string
  providerGone: boolean
}

export interface MeshDnsSyncResult {
  id: string
  fqdn: string
  ok: boolean
  error: string
}

export interface MeshDnsSyncOutcome {
  /** The machine's current virtual IP (empty = not yet available: cluster not enabled / EasyTier not ready). */
  ip: string
  results: MeshDnsSyncResult[]
}

export function listMeshDnsRecords(): Promise<{ items: MeshDnsRecord[] }> {
  return http.get('/api/mesh/dns-records')
}

export function createMeshDnsRecord(body: {
  providerId: string
  fqdn: string
  enabled?: boolean
}): Promise<MeshDnsRecord> {
  return http.post('/api/mesh/dns-records', body)
}

export function updateMeshDnsRecord(
  id: string,
  body: { fqdn?: string; enabled?: boolean },
): Promise<MeshDnsRecord> {
  return http.put(`/api/mesh/dns-records/${encodeURIComponent(id)}`, body)
}

export function deleteMeshDnsRecord(id: string): Promise<{ ok: boolean }> {
  return http.delete(`/api/mesh/dns-records/${encodeURIComponent(id)}`)
}

/** Sync now: upsert the A records of all enabled mappings in one go as the current virtual IP (executed synchronously, returns results one by one). */
export function syncMeshDnsRecords(): Promise<MeshDnsSyncOutcome> {
  return http.post('/api/mesh/dns-records/sync')
}

/** 手动运行请求扩展:被推送源项目的修订/条目选择(部署时选择)。 */
export function triggerManualRun(projectId: string, body: {
  branch?: string
  commit?: string
  params?: Record<string, string>
  revision?: string
  items?: string[]
}): Promise<unknown> {
  return http.post(`/api/projects/${encodeURIComponent(projectId)}/runs`, body)
}

/**
 * ── 临时凭据(EasyTier Secure Mode)──
 * POST /api/mesh/credentials        → 签发(credentialSecret 仅此一次返回;CSRF)
 * GET  /api/mesh/credentials        → { items } (登记簿 × easytier 活跃态合并)
 * POST /api/mesh/credentials/:id/revoke → { ok } (CSRF;全网 1~3s 踢下线)
 */
export interface MeshCredential {
  credentialId: string
  remark: string
  ttlSeconds: number
  createdAt: string
  expiresAt: string
  revokedAt: string
  createdBy: string
  source: 'admin' | 'token' | 'external'
  /** active=easytier 活跃;expired=已过期;revoked=已撤销;missing=未到期但已不在活跃表(凭据文件丢失)。 */
  status: 'active' | 'expired' | 'revoked' | 'missing'
}

export function issueMeshCredential(body: { ttlSeconds?: number; remark?: string }): Promise<{
  credentialId: string
  credentialSecret: string
  expiresAt: string
  remark: string
}> {
  return http.post('/api/mesh/credentials', body)
}

export function listMeshCredentials(): Promise<{ items: MeshCredential[] }> {
  return http.get('/api/mesh/credentials')
}

export function revokeMeshCredential(credentialId: string): Promise<{ ok: boolean }> {
  return http.post(`/api/mesh/credentials/${encodeURIComponent(credentialId)}/revoke`)
}

/**
 * ── 凭据自动申请 API 令牌(外部系统 Bearer 认证)──
 * 明文令牌 pwmc_* 仅创建响应出现一次;库内只存哈希。
 */
export interface MeshApiToken {
  name: string
  createdAt: string
  lastUsedAt: string
  revokedAt: string
}

export function createMeshApiToken(name: string): Promise<{ name: string; token: string; createdAt: string }> {
  return http.post('/api/mesh/api-tokens', { name })
}

export function listMeshApiTokens(): Promise<{ items: MeshApiToken[] }> {
  return http.get('/api/mesh/api-tokens')
}

export function revokeMeshApiToken(name: string): Promise<{ ok: boolean }> {
  return http.delete(`/api/mesh/api-tokens/${encodeURIComponent(name)}`)
}

/**
 * ── Secure Mode 全集群统一开关 ──
 * POST /api/mesh/secure-mode/switch  → {changeId,effectiveAt,armed} (CSRF;全员确认才 armed)
 * POST /api/mesh/secure-mode/abort   → { ok } (CSRF;生效时刻前)
 * GET  /api/mesh/secure-mode/status  → 各节点 {secureMode, acked, applied} 聚合
 */
export interface MeshSwitchNodeState {
  nodeId: string
  name: string
  online: boolean
  secureMode: boolean
  changeId: string
  action: string
  acked: boolean
  applied: boolean
  isSelf: boolean
}

export interface MeshSwitchStatus {
  secureMode: boolean
  pending: {
    changeId: string
    action: string
    effectiveAt: string
    initiatedBy: string
    ackedAt: string
    /** 空串 = 未应用(服务端输出空串,不用 time.Time 零值)。 */
    appliedAt: string
  } | null
  nodes: MeshSwitchNodeState[]
  /** 无虚拟 IP、无法纳入齐切的节点名(保持旧态,不参与 ack 判定)。 */
  skipped: string[]
}

export function switchMeshSecureMode(body: { action: 'enable' | 'disable'; effectiveInMinutes?: number }): Promise<{
  changeId: string
  action: string
  effectiveAt: string
  armed: boolean
  /** 无虚拟 IP、未纳入本次齐切的节点名。 */
  skippedNodes?: string[] | null
}> {
  return http.post('/api/mesh/secure-mode/switch', body)
}

export function abortMeshSecureMode(changeId: string): Promise<{ ok: boolean }> {
  return http.post('/api/mesh/secure-mode/abort', { changeId })
}

export function getMeshSecureModeStatus(): Promise<MeshSwitchStatus> {
  return http.get('/api/mesh/secure-mode/status')
}
