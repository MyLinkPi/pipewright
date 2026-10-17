/**
 * Ceph API — 集群部署(cephadm)与管理(admin 面,挂 /api/ceph/* 管理组)。
 *
 *   GET/POST  /api/ceph/clusters                 → { items: CephCluster[] } / CephCluster (CSRF)
 *   GET/DELETE /api/ceph/clusters/:id            → { cluster, deploy } / { ok } (CSRF;删除仅解除纳管)
 *   POST      /api/ceph/clusters/:id/deploy      → 202 { ok }(异步;进度轮询 deploy/status)
 *   GET       /api/ceph/clusters/:id/deploy/status → CephDeployStatus
 *   GET       /api/ceph/clusters/:id/status      → CephClusterStatus(TTL 15s)
 *   GET/POST  /api/ceph/clusters/:id/hosts       ;DELETE .../hosts/:host(drain 移除)
 *   GET/POST  /api/ceph/clusters/:id/osds        ;DELETE .../osds/:osdId(安全移除)
 *   GET/POST  /api/ceph/clusters/:id/pools       ;PATCH/DELETE .../pools/:name
 *   GET/POST  .../pools/:name/images             ;PATCH/DELETE .../images/:image
 *   GET/POST  .../images/:image/snapshots        ;DELETE .../snapshots/:snap;POST .../rollback
 *   GET       .../filesystems                    ;.../filesystems/:fs/subvolumes CRUD
 */

import { http } from './http'

export interface CephCluster {
  id: string
  name: string
  fsid: string
  bootstrapServerId: string
  bootstrapServerName: string
  image: string
  cephadmSource: string
  publicNetwork: string
  clusterNetwork: string
  state: 'created' | 'deploying' | 'ready' | 'failed'
  dashboardUrl: string
  dashboardUser: string
  dashboardCredentialId: string
  createdAt: string
  updatedAt: string
}

export interface CephDeployStatus {
  running: boolean
  step: string
  message?: string
  done: boolean
  error?: string
  log?: string[]
}

export interface CephPGStateCount {
  state: string
  count: number
}

export interface CephClusterStatus {
  health: string
  healthDetail?: string[]
  totalBytes: number
  usedBytes: number
  availBytes: number
  monCount: number
  mgrCount: number
  osdUp: number
  osdIn: number
  pgsByState?: CephPGStateCount[]
}

export interface CephHost {
  serverId: string
  serverName: string
  roles: string[]
  labels: string[]
  devices: string[]
  hostname: string
  addr: string
  inOrch: boolean
  orchStatus: string
}

export interface CephOSD {
  id: number
  host: string
  device: string
  up: boolean
  in: boolean
  sizeBytes: number
  usedBytes: number
  utilization: number
}

export interface CephPool {
  name: string
  type: 'replicated' | 'erasure'
  size: number
  ecProfile: string
  pgNum: number
  bytesUsed: number
  quotaMaxBytes: number
  apps: string[]
}

export interface CephRBDImage {
  pool: string
  name: string
  sizeBytes: number
  provisioning: string
}

export interface CephRBDSnapshot {
  name: string
  sizeBytes: number
  protected: boolean
}

export interface CephFilesystem {
  name: string
  metadataPool: string
  dataPools: string[]
  mdsCount: number
}

export interface CephSubvolume {
  name: string
  path: string
  bytesUsed: number
  quotaBytes: number
}

// ── 集群 ─────────────────────────────────────────────────────────────────────

export async function listCephClusters(): Promise<CephCluster[]> {
  const res = await http.get<{ items: CephCluster[] }>('/api/ceph/clusters')
  return res.items ?? []
}

export interface CephHostInput {
  serverId: string
  roles: string[]
  labels: string[]
  devices: string[]
}

export async function createCephCluster(body: {
  name: string
  bootstrapServerId: string
  image?: string
  cephadmSource?: string
  publicNetwork?: string
  clusterNetwork?: string
  hosts: CephHostInput[]
}): Promise<CephCluster> {
  return http.post<CephCluster>('/api/ceph/clusters', body)
}

export async function getCephCluster(id: string): Promise<{ cluster: CephCluster; deploy: CephDeployStatus }> {
  return http.get<{ cluster: CephCluster; deploy: CephDeployStatus }>(`/api/ceph/clusters/${id}`)
}

export async function deleteCephCluster(id: string): Promise<void> {
  await http.delete<{ ok: boolean }>(`/api/ceph/clusters/${id}`)
}

export async function deployCephCluster(id: string): Promise<void> {
  await http.post<{ ok: boolean }>(`/api/ceph/clusters/${id}/deploy`, {})
}

export async function getCephDeployStatus(id: string): Promise<CephDeployStatus> {
  return http.get<CephDeployStatus>(`/api/ceph/clusters/${id}/deploy/status`)
}

export async function getCephClusterStatus(id: string): Promise<CephClusterStatus> {
  return http.get<CephClusterStatus>(`/api/ceph/clusters/${id}/status`)
}

// ── 主机 / OSD ───────────────────────────────────────────────────────────────

export async function listCephHosts(id: string): Promise<CephHost[]> {
  const res = await http.get<{ items: CephHost[] }>(`/api/ceph/clusters/${id}/hosts`)
  return res.items ?? []
}

export async function addCephHost(id: string, body: { serverId: string; labels: string[]; devices: string[] }): Promise<void> {
  await http.post<{ ok: boolean }>(`/api/ceph/clusters/${id}/hosts`, body)
}

export async function removeCephHost(id: string, host: string): Promise<void> {
  await http.delete<{ ok: boolean }>(`/api/ceph/clusters/${id}/hosts/${encodeURIComponent(host)}`)
}

export async function listCephOSDs(id: string): Promise<CephOSD[]> {
  const res = await http.get<{ items: CephOSD[] }>(`/api/ceph/clusters/${id}/osds`)
  return res.items ?? []
}

export async function addCephOSD(id: string, host: string, device: string): Promise<void> {
  await http.post<{ ok: boolean }>(`/api/ceph/clusters/${id}/osds`, { host, device })
}

export async function removeCephOSD(id: string, osdId: number): Promise<void> {
  await http.delete<{ ok: boolean }>(`/api/ceph/clusters/${id}/osds/${osdId}`)
}

// ── 存储池 ───────────────────────────────────────────────────────────────────

export async function listCephPools(id: string): Promise<CephPool[]> {
  const res = await http.get<{ items: CephPool[] }>(`/api/ceph/clusters/${id}/pools`)
  return res.items ?? []
}

export async function createCephPool(
  id: string,
  body: { name: string; pgNum: number; type: 'replicated' | 'erasure'; size?: number; ecProfile?: string; apps?: string[] },
): Promise<void> {
  await http.post<{ ok: boolean }>(`/api/ceph/clusters/${id}/pools`, body)
}

export async function updateCephPool(
  id: string,
  name: string,
  body: { quotaMaxBytes?: number; size?: number },
): Promise<void> {
  await http.patch<{ ok: boolean }>(`/api/ceph/clusters/${id}/pools/${encodeURIComponent(name)}`, body)
}

export async function deleteCephPool(id: string, name: string): Promise<void> {
  await http.delete<{ ok: boolean }>(`/api/ceph/clusters/${id}/pools/${encodeURIComponent(name)}`)
}

// ── RBD 块设备 ───────────────────────────────────────────────────────────────

export async function listCephImages(id: string, pool: string): Promise<CephRBDImage[]> {
  const res = await http.get<{ items: CephRBDImage[] }>(
    `/api/ceph/clusters/${id}/pools/${encodeURIComponent(pool)}/images`,
  )
  return res.items ?? []
}

export async function createCephImage(id: string, pool: string, name: string, sizeGb: number): Promise<void> {
  await http.post<{ ok: boolean }>(`/api/ceph/clusters/${id}/pools/${encodeURIComponent(pool)}/images`, { name, sizeGb })
}

export async function resizeCephImage(
  id: string,
  pool: string,
  image: string,
  sizeGb: number,
  allowShrink = false,
): Promise<void> {
  await http.patch<{ ok: boolean }>(
    `/api/ceph/clusters/${id}/pools/${encodeURIComponent(pool)}/images/${encodeURIComponent(image)}`,
    { sizeGb, allowShrink },
  )
}

export async function deleteCephImage(id: string, pool: string, image: string): Promise<void> {
  await http.delete<{ ok: boolean }>(
    `/api/ceph/clusters/${id}/pools/${encodeURIComponent(pool)}/images/${encodeURIComponent(image)}`,
  )
}

export async function listCephSnapshots(id: string, pool: string, image: string): Promise<CephRBDSnapshot[]> {
  const res = await http.get<{ items: CephRBDSnapshot[] }>(
    `/api/ceph/clusters/${id}/pools/${encodeURIComponent(pool)}/images/${encodeURIComponent(image)}/snapshots`,
  )
  return res.items ?? []
}

export async function createCephSnapshot(id: string, pool: string, image: string, snap: string): Promise<void> {
  await http.post<{ ok: boolean }>(
    `/api/ceph/clusters/${id}/pools/${encodeURIComponent(pool)}/images/${encodeURIComponent(image)}/snapshots`,
    { name: snap },
  )
}

export async function deleteCephSnapshot(id: string, pool: string, image: string, snap: string): Promise<void> {
  await http.delete<{ ok: boolean }>(
    `/api/ceph/clusters/${id}/pools/${encodeURIComponent(pool)}/images/${encodeURIComponent(image)}/snapshots/${encodeURIComponent(snap)}`,
  )
}

export async function rollbackCephSnapshot(id: string, pool: string, image: string, snap: string): Promise<void> {
  await http.post<{ ok: boolean }>(
    `/api/ceph/clusters/${id}/pools/${encodeURIComponent(pool)}/images/${encodeURIComponent(image)}/snapshots/${encodeURIComponent(snap)}/rollback`,
    {},
  )
}

// ── CephFS ───────────────────────────────────────────────────────────────────

export async function listCephFilesystems(id: string): Promise<CephFilesystem[]> {
  const res = await http.get<{ items: CephFilesystem[] }>(`/api/ceph/clusters/${id}/filesystems`)
  return res.items ?? []
}

export async function listCephSubvolumes(id: string, fs: string): Promise<CephSubvolume[]> {
  const res = await http.get<{ items: CephSubvolume[] }>(
    `/api/ceph/clusters/${id}/filesystems/${encodeURIComponent(fs)}/subvolumes`,
  )
  return res.items ?? []
}

export async function createCephSubvolume(id: string, fs: string, name: string, sizeGb: number): Promise<void> {
  await http.post<{ ok: boolean }>(`/api/ceph/clusters/${id}/filesystems/${encodeURIComponent(fs)}/subvolumes`, {
    name,
    sizeGb,
  })
}

export async function deleteCephSubvolume(id: string, fs: string, name: string): Promise<void> {
  await http.delete<{ ok: boolean }>(
    `/api/ceph/clusters/${id}/filesystems/${encodeURIComponent(fs)}/subvolumes/${encodeURIComponent(name)}`,
  )
}
