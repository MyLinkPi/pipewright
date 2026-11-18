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
 * GET    /api/pve/clusters/{id}/templates               → { items: PVETemplate[] }
 * GET    /api/pve/clusters/{id}/storages?node=          → { items: PVEStorage[] }
 * GET    /api/pve/clusters/{id}/bridges?node=           → { items: PVEBridge[] }
 * GET    /api/pve/clusters/{id}/nextid                  → { vmid: number }
 * GET    /api/pve/clusters/{id}/storage-content?node=&storage=&content= → { items: PVEContentItem[] }
 * GET    /api/pve/clusters/{id}/aplinfo?node=           → { items: PVEAplTemplate[] }
 * POST   /api/pve/clusters/{id}/aplinfo/download        → PVETaskRef (202)   (needs CSRF)
 * POST   /api/pve/clusters/{id}/create                  → { ok: true } (202) (needs CSRF)
 * GET    /api/pve/clusters/{id}/create-status           → PVECreateStatus
 * POST   /api/pve/clusters/{id}/upload-vma?node=&storage= → { volid } (multipart) (needs CSRF)
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

// ---- 建机面（模板克隆 / 云镜像 cloud-init / LXC；批量 1-50；建机后自动登记） ----

/** 可克隆模板（qemu VM 模板 / lxc 容器模板）。 */
export interface PVETemplate {
  vmid: number
  name: string
  node: string
  type: 'qemu' | 'lxc'
}

/** 节点存储状态（建机目标存储/镜像存储下拉）。 */
export interface PVEStorage {
  storage: string
  type: string
  active: boolean
  enabled: boolean
  shared: boolean
  content: string[]
  used: number
  total: number
  avail: number
}

/** 节点网桥。 */
export interface PVEBridge {
  iface: string
  type: string
  active: boolean
  address: string
  comments: string
}

/** 存储内容条目（vztmpl/import/images/backup 卷）。 */
export interface PVEContentItem {
  volid: string
  content: string
  format: string
  size: number
  vmid: number
}

/** 在线 LXC 模板目录条目。 */
export interface PVEAplTemplate {
  template: string
  package: string
  version: string
  section: string
  description: string
}

/** 内联 SSH 凭据（代建一条保险库凭据；与 credentialId 二选一）。 */
export interface PVEInlineSecret {
  name?: string
  kind: 'password' | 'key'
  secret: string
}

/** 建机后自动登记参数（凭据二选一:XOR）。 */
export interface PVERegisterInput {
  sshUser: string
  sshPort?: number
  credentialId?: string
  inlineSecret?: PVEInlineSecret
  waitSeconds?: number
  labels?: string
}

/** 任务内 LXC 模板在线下载参数。 */
export interface PVEAplDownloadInput {
  storage: string
  template: string
}

/** 建机入参（默认 true 的开关显式传值;密码/密钥只写不回显）。 */
export interface PVECreateInput {
  mode: 'clone' | 'image' | 'lxc'
  node?: string
  name: string
  count?: number
  vmid?: number
  templateVmid?: number
  backupVolid?: string
  storage?: string
  imageUrl?: string
  imageVolid?: string
  ostemplate?: string
  aplDownload?: PVEAplDownloadInput
  cores?: number
  memory?: number
  diskGb?: number
  dataGb?: number
  dataStorage?: string
  bridge?: string
  ipMode?: 'dhcp' | 'static'
  ip?: string
  gateway?: string
  prefix?: number
  dns?: string
  ciUser?: string
  ciPassword?: string
  sshKeys?: string
  sshPasswordAuth?: boolean
  timezone?: string
  mirror?: string
  installAgent?: boolean
  disableSelinux?: boolean
  disableFirewall?: boolean
  fixStatic?: boolean
  onboot?: boolean
  start?: boolean
  agent?: boolean
  bios?: string
  machine?: string
  unprivileged?: boolean
  nesting?: boolean
  lxcPassword?: string
  register?: PVERegisterInput
}

/** 批量建机单台结果（创建与登记分开计错）。 */
export interface PVECreateItemResult {
  index: number
  vmid: number
  name: string
  ip?: string
  ok: boolean
  err?: string
  serverId?: string
  registerErr?: string
  note?: string
}

/** 建机任务快照（轮询至 done）。 */
export interface PVECreateStatus {
  running: boolean
  mode: string
  step: string
  message: string
  done: boolean
  error?: string
  log: string[] | null
  items: PVECreateItemResult[] | null
  total: number
  ok: number
  failed: number
}

/** 列集群全部模板（克隆源下拉）。 */
export async function listPVETemplates(id: string): Promise<PVETemplate[]> {
  const res = await http.get<{ items: PVETemplate[] }>(`/api/pve/clusters/${encodeURIComponent(id)}/templates`)
  return res.items ?? []
}

/** 列节点存储（目标存储/镜像存储下拉）。 */
export async function listPVEStorages(id: string, node: string): Promise<PVEStorage[]> {
  const qs = new URLSearchParams({ node })
  const res = await http.get<{ items: PVEStorage[] }>(`/api/pve/clusters/${encodeURIComponent(id)}/storages?${qs.toString()}`)
  return res.items ?? []
}

/** 列节点网桥。 */
export async function listPVEBridges(id: string, node: string): Promise<PVEBridge[]> {
  const qs = new URLSearchParams({ node })
  const res = await http.get<{ items: PVEBridge[] }>(`/api/pve/clusters/${encodeURIComponent(id)}/bridges?${qs.toString()}`)
  return res.items ?? []
}

/** 集群下一个空闲 VMID。 */
export async function getPVENextID(id: string): Promise<number> {
  const res = await http.get<{ vmid: number }>(`/api/pve/clusters/${encodeURIComponent(id)}/nextid`)
  return res.vmid
}

/** 列存储内容（content: vztmpl|import|images|backup|iso）。 */
export async function listPVEStorageContent(id: string, node: string, storage: string, content: string): Promise<PVEContentItem[]> {
  const qs = new URLSearchParams({ node, storage, content })
  const res = await http.get<{ items: PVEContentItem[] }>(`/api/pve/clusters/${encodeURIComponent(id)}/storage-content?${qs.toString()}`)
  return res.items ?? []
}

/** 列节点可下载的 LXC 模板目录。 */
export async function listPVEAplInfo(id: string, node: string): Promise<PVEAplTemplate[]> {
  const qs = new URLSearchParams({ node })
  const res = await http.get<{ items: PVEAplTemplate[] }>(`/api/pve/clusters/${encodeURIComponent(id)}/aplinfo?${qs.toString()}`)
  return res.items ?? []
}

/** 在线下载 LXC 模板到存储（须已解锁;返回任务句柄,轮询 getPVETaskStatus）。 */
export async function downloadPVEAplTemplate(id: string, node: string, storage: string, template: string): Promise<PVETaskRef> {
  return http.post<PVETaskRef>(`/api/pve/clusters/${encodeURIComponent(id)}/aplinfo/download`, { node, storage, template })
}

/** 启动建机任务（异步;须已解锁;快照经 getPVECreateStatus 轮询）。 */
export async function startPVECreate(id: string, input: PVECreateInput): Promise<{ ok: boolean }> {
  return http.post<{ ok: boolean }>(`/api/pve/clusters/${encodeURIComponent(id)}/create`, input)
}

/** 建机任务快照。 */
export async function getPVECreateStatus(id: string, signal?: AbortSignal): Promise<PVECreateStatus> {
  return http.get<PVECreateStatus>(`/api/pve/clusters/${encodeURIComponent(id)}/create-status`, { signal })
}

/** 流式上传 .vma 备份（返回恢复源:卷 ID 或绝对路径,供 backupVolid 用）。 */
export async function uploadPVEBackup(id: string, node: string, storage: string, file: File, signal?: AbortSignal): Promise<string> {
  const qs = new URLSearchParams({ node, storage })
  const form = new FormData()
  form.append('filename', file, file.name)
  const res = await http.postForm<{ volid: string }>(`/api/pve/clusters/${encodeURIComponent(id)}/upload-vma?${qs.toString()}`, form, { signal })
  return res.volid
}
