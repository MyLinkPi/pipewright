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
  hasSecret: boolean
}

export interface MeshACL {
  visibleTo: string[]
  pushAllowed: string[]
  triggerAllowed: string[]
  forwardAllowed: string[]
  /** 0066 cluster-shared build machines: peers allowed to submit shared build jobs. */
  buildAllowed: string[]
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

export interface EasyTierStatus {
  ok: boolean
  err: string
  selfIp: string
  hostname: string
  version: string
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
