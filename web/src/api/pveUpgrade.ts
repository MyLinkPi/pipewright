/**
 * PVE 集群升级 API client（顶级「PVE」页 → 升级管理 /pve/upgrade）。
 *
 * 三条功能线:
 *   A 日常 apt 升级(纯 API):更新矩阵 / 刷新索引(UPID) / 串行升级任务 / 节点重启;
 *   B apt 仓库管理(SSH):清单(双格式) / 启停 / 镜像预设 / 订阅预设 / 撤销 / 批量;
 *   C 大版本 8→9(SSH 滚动,持久化):预检 / 启动 / 快照 / 中止 / 重试 / 续跑。
 *
 * 集群默认只读:写操作前须显式解锁(setPVEReadonly,留审计)。危险动作(重启/大版本)
 * 须输入确认(confirm 原样等于节点名/集群名,服务端强校验)。
 *
 * GET    /api/pve/clusters/{id}/updates                       → PVEUpdatesOverview
 * POST   /api/pve/clusters/{id}/updates/refresh               → { items: PVERefreshItem[] } (202, needs CSRF)
 * POST   /api/pve/clusters/{id}/upgrade                       → { ok: true } (202, needs CSRF)
 * GET    /api/pve/clusters/{id}/upgrade-status                → PVEUpgradeStatus
 * POST   /api/pve/clusters/{id}/nodes/{node}/reboot           → PVETaskRef (202, needs CSRF)
 * GET    /api/pve/clusters/{id}/nodes/{node}/apt-repos        → PVEAptRepoInventory
 * GET    /api/pve/clusters/{id}/apt-repos/overview            → PVEAptRepoOverview
 * POST   /api/pve/clusters/{id}/nodes/{node}/apt-repos/toggle → PVEAptRepoWriteResult (needs CSRF)
 * POST   /api/pve/clusters/{id}/nodes/{node}/apt-repos/mirror → PVEAptRepoWriteResult (needs CSRF)
 * POST   /api/pve/clusters/{id}/nodes/{node}/apt-repos/preset → PVEAptRepoWriteResult (needs CSRF)
 * POST   /api/pve/clusters/{id}/nodes/{node}/apt-repos/restore→ PVEAptRepoWriteResult (needs CSRF)
 * POST   /api/pve/clusters/{id}/apt-repos/apply               → { items: PVEAptRepoApplyItem[] } (needs CSRF)
 * GET    /api/pve/clusters/{id}/major-upgrade/preflight       → PVEMajorPreflightReport
 * POST   /api/pve/clusters/{id}/major-upgrade                 → { ok: true } (202, needs CSRF)
 * GET    /api/pve/clusters/{id}/major-upgrade-status          → PVEMajorUpgradeStatus
 * POST   /api/pve/clusters/{id}/major-upgrade/abort           → { ok: true } (needs CSRF)
 * POST   /api/pve/clusters/{id}/major-upgrade/retry           → { ok: true } (202, needs CSRF)
 * POST   /api/pve/clusters/{id}/major-upgrade/resume          → { ok: true } (202, needs CSRF)
 */

import { http } from './http'
import type { PVETaskRef } from './pveAdmin'

// ─── A 日常升级 ────────────────────────────────────────────────────────────────

/** 一个待更新包。 */
export interface PVEAptPackage {
  package: string
  title?: string
  version?: string
  oldVersion?: string
  origin?: string
}

/** 更新矩阵里一个节点(逐节点错误隔离)。 */
export interface PVENodeUpdates {
  node: string
  online: boolean
  version?: string
  kernelRunning?: string
  pending: number
  packages?: PVEAptPackage[]
  subscription?: string
  needsReboot: boolean
  error?: string
}

export interface PVEUpdatesOverview {
  quorate: boolean
  items: PVENodeUpdates[]
}

/** 索引刷新受理结果(UPID 经既有 getPVETaskStatus 轮询)。 */
export interface PVERefreshItem {
  node: string
  upid?: string
  error?: string
}

export interface PVEUpgradeInput {
  nodes?: string[]
}

export interface PVEUpgradeNodeResult {
  node: string
  ok: boolean
  err?: string
  updated: number
  needsReboot: boolean
  note?: string
}

export interface PVEUpgradeStatus {
  running: boolean
  step: string
  message: string
  done: boolean
  error?: string
  log: string[] | null
  items: PVEUpgradeNodeResult[] | null
  total: number
  ok: number
  failed: number
}

export async function getPVEUpdates(id: string, signal?: AbortSignal): Promise<PVEUpdatesOverview> {
  return http.get<PVEUpdatesOverview>(`/api/pve/clusters/${encodeURIComponent(id)}/updates`, { signal })
}

export async function refreshPVEUpdates(id: string, nodes?: string[]): Promise<PVERefreshItem[]> {
  const res = await http.post<{ items: PVERefreshItem[] }>(
    `/api/pve/clusters/${encodeURIComponent(id)}/updates/refresh`, { nodes })
  return res.items ?? []
}

export async function startPVEUpgrade(id: string, input: PVEUpgradeInput): Promise<void> {
  await http.post(`/api/pve/clusters/${encodeURIComponent(id)}/upgrade`, input)
}

export async function getPVEUpgradeStatus(id: string, signal?: AbortSignal): Promise<PVEUpgradeStatus> {
  return http.get<PVEUpgradeStatus>(`/api/pve/clusters/${encodeURIComponent(id)}/upgrade-status`, { signal })
}

export async function rebootPVENode(id: string, node: string, confirm: string): Promise<PVETaskRef> {
  return http.post<PVETaskRef>(
    `/api/pve/clusters/${encodeURIComponent(id)}/nodes/${encodeURIComponent(node)}/reboot`, { confirm })
}

// ─── B apt 仓库管理 ────────────────────────────────────────────────────────────

/** 一个仓库条目(.list 一行 / .sources 一个 stanza)。 */
export interface PVEAptRepoEntry {
  key: string
  enabled: boolean
  /** debian | pve | ceph | other。ceph 只读。 */
  kind: 'debian' | 'pve' | 'ceph' | 'other'
  types?: string
  uris: string[]
  suites: string[]
  components: string[]
  signedBy?: string
  raw?: string
}

export interface PVEAptRepoFile {
  path: string
  format: 'list' | 'sources'
  entries: PVEAptRepoEntry[]
}

export interface PVEAptRepoWarning {
  key: string
  level: 'warn' | 'info'
  message: string
}

export interface PVEAptRepoInventory {
  node: string
  codename?: string
  subscription?: string
  files: PVEAptRepoFile[]
  warnings: PVEAptRepoWarning[]
}

/** 集群仓库总览里一个节点的摘要(派生自清单,不含逐条目)。 */
export interface PVEAptRepoNodeSummary {
  node: string
  online: boolean
  codename?: string
  subscription?: string
  /** enterprise | no-subscription | mixed | none。 */
  pveRepo: string
  /** official | ustc | tsinghua | aliyun | mixed。 */
  mirror: string
  ceph: boolean
  enabledCount: number
  warnings: PVEAptRepoWarning[]
  error?: string
}

export interface PVEAptRepoOverview {
  items: PVEAptRepoNodeSummary[]
}

export interface PVEAptRepoToggleInput {
  path: string
  key: string
  enabled: boolean
}

export interface PVEAptRepoBackup {
  path: string
  backupPath: string
}

export interface PVEAptRepoWriteResult {
  node: string
  ok: boolean
  backups?: PVEAptRepoBackup[]
  created?: string[]
  updateOk: boolean
  updateTail?: string
  note?: string
}

export interface PVEAptRepoApplyInput {
  action: 'mirror' | 'preset'
  preset: string
  nodes?: string[]
}

export interface PVEAptRepoApplyItem {
  node: string
  ok: boolean
  err?: string
  updateOk: boolean
  note?: string
}

export async function getPVEAptRepos(id: string, node: string, signal?: AbortSignal): Promise<PVEAptRepoInventory> {
  return http.get<PVEAptRepoInventory>(
    `/api/pve/clusters/${encodeURIComponent(id)}/nodes/${encodeURIComponent(node)}/apt-repos`, { signal })
}

export async function getPVEAptRepoOverview(id: string, signal?: AbortSignal): Promise<PVEAptRepoOverview> {
  return http.get<PVEAptRepoOverview>(
    `/api/pve/clusters/${encodeURIComponent(id)}/apt-repos/overview`, { signal })
}

export async function togglePVEAptRepo(id: string, node: string, input: PVEAptRepoToggleInput): Promise<PVEAptRepoWriteResult> {
  return http.post<PVEAptRepoWriteResult>(
    `/api/pve/clusters/${encodeURIComponent(id)}/nodes/${encodeURIComponent(node)}/apt-repos/toggle`, input)
}

export async function setPVEAptRepoMirror(id: string, node: string, preset: string): Promise<PVEAptRepoWriteResult> {
  return http.post<PVEAptRepoWriteResult>(
    `/api/pve/clusters/${encodeURIComponent(id)}/nodes/${encodeURIComponent(node)}/apt-repos/mirror`, { preset })
}

export async function setPVEAptRepoPreset(id: string, node: string, preset: string): Promise<PVEAptRepoWriteResult> {
  return http.post<PVEAptRepoWriteResult>(
    `/api/pve/clusters/${encodeURIComponent(id)}/nodes/${encodeURIComponent(node)}/apt-repos/preset`, { preset })
}

export async function restorePVEAptRepo(id: string, node: string, path: string, backupPath: string): Promise<PVEAptRepoWriteResult> {
  return http.post<PVEAptRepoWriteResult>(
    `/api/pve/clusters/${encodeURIComponent(id)}/nodes/${encodeURIComponent(node)}/apt-repos/restore`, { path, backupPath })
}

export async function applyPVEAptRepos(id: string, input: PVEAptRepoApplyInput): Promise<PVEAptRepoApplyItem[]> {
  const res = await http.post<{ items: PVEAptRepoApplyItem[] }>(
    `/api/pve/clusters/${encodeURIComponent(id)}/apt-repos/apply`, input)
  return res.items ?? []
}

// ─── C 大版本升级 ──────────────────────────────────────────────────────────────

export interface PVEMajorCheck {
  key: string
  level: 'pass' | 'warn' | 'fail'
  message: string
}

export interface PVEPve8to9Result {
  passed: number
  skipped: number
  warnings: number
  failures: number
  lines?: string[]
  summary?: string
}

export interface PVEMajorNodePreflight {
  node: string
  online: boolean
  version?: string
  codename?: string
  diskFreeGb: number
  pending: number
  pve8to9?: PVEPve8to9Result
  checks: PVEMajorCheck[]
  error?: string
}

export interface PVEMajorPreflightReport {
  clusterId: string
  at: string
  quorate: boolean
  cephDetected: boolean
  nodes: PVEMajorNodePreflight[]
  clusterChecks: PVEMajorCheck[]
  blocked: boolean
  acksRequired: string[]
}

export interface PVEMajorUpgradeInput {
  confirm: string
  acks?: string[]
  guestPolicy?: 'warn' | 'shutdown'
  nodes?: string[]
}

export interface PVEMajorNodeStatus {
  node: string
  seq: number
  state: string
  error?: string
  startedAt?: string
  finishedAt?: string
}

export interface PVEMajorUpgradeStatus {
  jobId?: string
  running: boolean
  status?: string
  guestPolicy?: string
  step: string
  message: string
  done: boolean
  error?: string
  log: string[] | null
  nodes: PVEMajorNodeStatus[] | null
  abortRequested: boolean
  createdAt?: string
}

export async function getPVEMajorPreflight(id: string, signal?: AbortSignal): Promise<PVEMajorPreflightReport> {
  return http.get<PVEMajorPreflightReport>(
    `/api/pve/clusters/${encodeURIComponent(id)}/major-upgrade/preflight`, { signal })
}

export async function startPVEMajorUpgrade(id: string, input: PVEMajorUpgradeInput): Promise<void> {
  await http.post(`/api/pve/clusters/${encodeURIComponent(id)}/major-upgrade`, input)
}

export async function getPVEMajorUpgradeStatus(id: string, signal?: AbortSignal): Promise<PVEMajorUpgradeStatus> {
  return http.get<PVEMajorUpgradeStatus>(
    `/api/pve/clusters/${encodeURIComponent(id)}/major-upgrade-status`, { signal })
}

export async function abortPVEMajorUpgrade(id: string): Promise<void> {
  await http.post(`/api/pve/clusters/${encodeURIComponent(id)}/major-upgrade/abort`)
}

export async function retryPVEMajorNode(id: string, node: string, confirm: string): Promise<void> {
  await http.post(`/api/pve/clusters/${encodeURIComponent(id)}/major-upgrade/retry`, { node, confirm })
}

export async function resumePVEMajorUpgrade(id: string, confirm: string): Promise<void> {
  await http.post(`/api/pve/clusters/${encodeURIComponent(id)}/major-upgrade/resume`, { confirm })
}