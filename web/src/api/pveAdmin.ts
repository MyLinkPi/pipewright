/**
 * PVE 集群管理 API client（设置 · PVE Token 配置 + 顶级「PVE」页纳管管理）。
 *
 * 一个 `PVECluster` 由**服务器登记自动探测生成**（登记 PVE 物理机 → pveversion/corosync
 * 探测 → 自动建 pve-<集群名> 服务器池并登记全部节点），本页只做 Token 配置与管理操作。
 * API Token 经后端保险库密文存储，**绝不回显**——列表只以 `hasToken` 告知是否已配置。
 * 集群默认只读：电源操作前须显式解锁（`setPVEReadonly`，留审计）。
 *
 * PVE 物理机（kind=pve_node）不允许作为构建/部署/应用部署目标（域层拦截）。
 *
 * GET    /api/pve/clusters                              → { items: PVECluster[] }
 * GET    /api/pve/clusters/{id}                         → PVECluster
 * PUT    /api/pve/clusters/{id}/token                   → { ok: true }        (needs CSRF)
 * POST   /api/pve/clusters/{id}/test                    → { ok: true }        (needs CSRF)
 * PUT    /api/pve/clusters/{id}/readonly                → { ok: true }        (needs CSRF)
 * DELETE /api/pve/clusters/{id}                         → { ok: true }        (needs CSRF)
 * GET    /api/pve/clusters/{id}/overview                → PVEOverview
 * GET    /api/pve/clusters/{id}/vms                     → { items: PVEVM[] }
 * POST   /api/pve/clusters/{id}/vms/{node}/{vmid}/status → PVETaskRef (202)   (needs CSRF)
 * GET    /api/pve/clusters/{id}/tasks?limit=            → { items: PVETask[] }
 * GET    /api/pve/clusters/{id}/task?node=&upid=        → PVETaskStatus
 * GET    /api/server-pools                              → { items: ServerPool[] }
 */

import { http } from './http'

/** 一个受管 PVE 集群（探测自动生成）。Token 永不返回。 */
export interface PVECluster {
  id: string
  name: string
  apiUrl: string
  tokenName: string
  /** 服务端派生：是否配置了 Token（未配置 = 仅库存，管理面 409）。 */
  hasToken: boolean
  /** verify | insecure | fingerprint。 */
  tlsMode: 'verify' | 'insecure' | 'fingerprint'
  tlsFingerprint: string
  /** created（未配 Token）| ready（测连通过）。 */
  state: 'created' | 'ready'
  /** 只读锁：true 时电源操作被服务端拒绝。 */
  readonly: boolean
  poolId: string
  poolName: string
  /** 池内机器数（探测登记的物理节点）。 */
  nodeCount: number
  createdAt: string
  updatedAt: string
}

/** Token 配置入参（保存后立即测连；成功 → ready）。 */
export interface SetPVETokenInput {
  apiUrl: string
  tokenName: string
  tokenSecret: string
  tlsMode: 'verify' | 'insecure' | 'fingerprint'
  tlsFingerprint?: string
}

/** 集群总览节点条目。 */
export interface PVENodeStatus {
  name: string
  ip: string
  online: boolean
}

/** 集群资源汇总（全集群合计）。 */
export interface PVEResourceSummary {
  cpuUsed: number
  cpuTotal: number
  memUsed: number
  memTotal: number
  storageUsed: number
  storageTotal: number
}

/** 集群总览。 */
export interface PVEOverview {
  version: string
  quorate: boolean
  nodes: PVENodeStatus[] | null
  resources: PVEResourceSummary
}

/** 一个虚拟机/容器（qemu | lxc）。 */
export interface PVEVM {
  vmid: number
  name: string
  node: string
  type: 'qemu' | 'lxc'
  /** running | stopped | 其他 PVE 状态。 */
  status: string
  /** 0-1（占用比例 × 核数）。 */
  cpu: number
  maxCpu: number
  mem: number
  maxMem: number
  uptime: number
}

/** 电源操作受理结果（异步任务句柄，前端轮询至完成）。 */
export interface PVETaskRef {
  node: string
  upid: string
}

/** 单任务状态（done 且 ok = 成功；done 且 !ok = 失败,exitStatus 人读）。 */
export interface PVETaskStatus {
  upid: string
  node: string
  /** running | stopped。 */
  status: string
  exitStatus: string
  done: boolean
  ok: boolean
}

/** 任务中心条目。 */
export interface PVETask {
  upid: string
  node: string
  type: string
  /** OK | ERROR | ''（运行中）。 */
  status: string
  user: string
  startTime: number
  endTime: number
}

/** 服务器池（系统管理:default 标准池 + PVE 探测自动建）。 */
export interface ServerPool {
  id: string
  name: string
  /** standard | pve。 */
  kind: 'standard' | 'pve'
  clusterId: string
  serverCount: number
  createdAt: string
}

/** 列出全部受管集群（自动探测生成;永不包含 Token）。 */
export async function listPVEClusters(): Promise<PVECluster[]> {
  const res = await http.get<{ items: PVECluster[] }>('/api/pve/clusters')
  return res.items ?? []
}

/** 单集群视图。 */
export async function getPVECluster(id: string): Promise<PVECluster> {
  return http.get<PVECluster>(`/api/pve/clusters/${encodeURIComponent(id)}`)
}

/** 配置/更换 API Token（密文存储;保存后立即测连,失败时 Token 已保存但 state 保持 created）。 */
export async function setPVEToken(id: string, input: SetPVETokenInput): Promise<{ ok: boolean }> {
  return http.put<{ ok: boolean }>(`/api/pve/clusters/${encodeURIComponent(id)}/token`, input)
}

/** 实连测试（GET /version;成功置 ready）。 */
export async function testPVECluster(id: string): Promise<{ ok: boolean }> {
  return http.post<{ ok: boolean }>(`/api/pve/clusters/${encodeURIComponent(id)}/test`, {})
}

/** 解锁/上锁电源操作（false = 解锁,是显式审计动作）。 */
export async function setPVEReadonly(id: string, readonly: boolean): Promise<{ ok: boolean }> {
  return http.put<{ ok: boolean }>(`/api/pve/clusters/${encodeURIComponent(id)}/readonly`, { readonly })
}

/** 删除集群（池内机器归位标准池,Token 密文随行删除）。 */
export async function deletePVECluster(id: string): Promise<{ ok: boolean }> {
  return http.delete<{ ok: boolean }>(`/api/pve/clusters/${encodeURIComponent(id)}`)
}

/** 集群总览（版本/quorum/节点/资源）。 */
export async function getPVEOverview(id: string): Promise<PVEOverview> {
  return http.get<PVEOverview>(`/api/pve/clusters/${encodeURIComponent(id)}/overview`)
}

/** 列全部虚拟机与容器。 */
export async function listPVEVMs(id: string): Promise<PVEVM[]> {
  const res = await http.get<{ items: PVEVM[] }>(`/api/pve/clusters/${encodeURIComponent(id)}/vms`)
  return res.items ?? []
}

/** 电源操作（start|shutdown|stop|reboot|reset|suspend|resume;kind 为 qemu|lxc;须已解锁）。 */
export async function pveVMAction(
  id: string,
  node: string,
  vmid: number,
  kind: 'qemu' | 'lxc',
  action: string,
): Promise<PVETaskRef> {
  return http.post<PVETaskRef>(
    `/api/pve/clusters/${encodeURIComponent(id)}/vms/${encodeURIComponent(node)}/${vmid}/status`,
    { action, kind },
  )
}

/** 最近集群任务。 */
export async function listPVETasks(id: string, limit = 50): Promise<PVETask[]> {
  const res = await http.get<{ items: PVETask[] }>(
    `/api/pve/clusters/${encodeURIComponent(id)}/tasks?limit=${limit}`,
  )
  return res.items ?? []
}

/** 单任务状态（电源操作后轮询至 done）。 */
export async function getPVETaskStatus(id: string, node: string, upid: string): Promise<PVETaskStatus> {
  const qs = new URLSearchParams({ node, upid })
  return http.get<PVETaskStatus>(`/api/pve/clusters/${encodeURIComponent(id)}/task?${qs.toString()}`)
}

/** 服务器池只读列表（供服务器列表分组展示与目标选择器过滤 pve_node）。 */
export async function listServerPools(): Promise<ServerPool[]> {
  const res = await http.get<{ items: ServerPool[] }>('/api/server-pools')
  return res.items ?? []
}
