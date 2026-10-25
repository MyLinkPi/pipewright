/**
 * DBaaS (数据库即服务) API.
 *
 * POST   /api/dbaas/instances                        → DBaaSInstance (createPassword 一次性)
 * GET    /api/dbaas/instances?refresh=1              → DBaaSInstance[]
 * GET    /api/dbaas/instances/{id}                   → DBaaSInstance
 * POST   /api/dbaas/instances/{id}/start|stop|restart → DBaaSInstance
 * DELETE /api/dbaas/instances/{id}?keepVolumes=1
 * POST   /api/dbaas/instances/{id}/credentials       → { password } (audited)
 * PUT    /api/dbaas/instances/{id}/schedule
 * POST   /api/dbaas/instances/{id}/backups           → DBaaSBackup (sync)
 * GET    /api/dbaas/instances/{id}/backups           → DBaaSBackup[]
 * POST   /api/dbaas/instances/{id}/restore           (JSON backupId | multipart file)
 * GET    /api/dbaas/backups/{bid}/download
 * DELETE /api/dbaas/backups/{bid}
 * POST   /api/dbaas/backups/{bid}/upload-objectstore
 */

import { http } from './http'

export type DBaaSEngine = 'mysql' | 'postgres' | 'redis'

export interface DBaaSInstance {
  id: string
  name: string
  engine: DBaaSEngine
  version: string
  serverId: string
  serverName: string
  host: string
  hostPort: number
  stackName: string
  cpus: string
  memory: string
  status: string
  statusDetail: string
  backupIntervalHours: number
  backupRetention: number
  lastBackupAt: string
  liveStatus: string
  createdAt: string
  updatedAt: string
  /** 仅创建响应一次性携带;列表/详情恒空。 */
  createPassword?: string
}

export interface CreateDBaaSInput {
  name: string
  engine: DBaaSEngine
  version: string
  serverId: string
  port?: number
  adminPassword?: string
  cpus?: string
  memory?: string
  backupIntervalHours?: number
  backupRetention?: number
}

export interface DBaaSBackup {
  id: string
  instanceId: string
  fileName: string
  sizeBytes: number
  sha256: string
  kind: string
  status: string
  error: string
  objectKey: string
  createdAt: string
  finishedAt: string
}

export interface DBaaSRestoreResult {
  ok: boolean
  error?: string
  safetyBackup?: DBaaSBackup
}

export async function listDBaaSInstances(refresh = false): Promise<DBaaSInstance[]> {
  const res = await http.get<{ items: DBaaSInstance[] }>(`/api/dbaas/instances${refresh ? '?refresh=1' : ''}`)
  return res.items ?? []
}

export async function createDBaaSInstance(input: CreateDBaaSInput): Promise<DBaaSInstance> {
  return http.post<DBaaSInstance>('/api/dbaas/instances', input)
}

export async function dbaaSAction(id: string, action: 'start' | 'stop' | 'restart'): Promise<DBaaSInstance> {
  return http.post<DBaaSInstance>(`/api/dbaas/instances/${encodeURIComponent(id)}/${action}`)
}

export async function deleteDBaaSInstance(id: string, keepVolumes: boolean): Promise<void> {
  await http.delete(`/api/dbaas/instances/${encodeURIComponent(id)}${keepVolumes ? '?keepVolumes=1' : ''}`)
}

export async function revealDBaaSCredentials(id: string): Promise<string> {
  const res = await http.post<{ password: string }>(`/api/dbaas/instances/${encodeURIComponent(id)}/credentials`)
  return res.password
}

export async function setDBaaSSchedule(id: string, hours: number, retention: number): Promise<void> {
  await http.put(`/api/dbaas/instances/${encodeURIComponent(id)}/schedule`, {
    backupIntervalHours: hours,
    backupRetention: retention,
  })
}

export async function listDBaaSBackups(id: string): Promise<DBaaSBackup[]> {
  const res = await http.get<{ items: DBaaSBackup[] }>(`/api/dbaas/instances/${encodeURIComponent(id)}/backups`)
  return res.items ?? []
}

export async function runDBaaSBackup(id: string): Promise<DBaaSBackup> {
  return http.post<DBaaSBackup>(`/api/dbaas/instances/${encodeURIComponent(id)}/backups`)
}

export async function restoreDBaaSFromBackup(id: string, backupId: string): Promise<DBaaSRestoreResult> {
  return http.post<DBaaSRestoreResult>(`/api/dbaas/instances/${encodeURIComponent(id)}/restore`, { backupId })
}

/** 从对象存储直接拉取备份恢复(objectKey 须为 .sql/.rdb;服务端先做安全备份)。 */
export async function restoreDBaaSFromCloud(id: string, providerId: string, objectKey: string): Promise<DBaaSRestoreResult> {
  return http.post<DBaaSRestoreResult>(`/api/dbaas/instances/${encodeURIComponent(id)}/restore`, {
    providerId,
    objectKey,
  })
}

export async function restoreDBaaSUpload(id: string, file: File): Promise<DBaaSRestoreResult> {
  const form = new FormData()
  form.append('file', file)
  return http.post<DBaaSRestoreResult>(`/api/dbaas/instances/${encodeURIComponent(id)}/restore`, form)
}

export function dbaaSBackupDownloadUrl(bid: string): string {
  return `/api/dbaas/backups/${encodeURIComponent(bid)}/download`
}

export async function deleteDBaaSBackup(bid: string): Promise<void> {
  await http.delete(`/api/dbaas/backups/${encodeURIComponent(bid)}`)
}

export async function uploadDBaaSBackup(bid: string, providerId: string): Promise<void> {
  await http.post(`/api/dbaas/backups/${encodeURIComponent(bid)}/upload-objectstore`, { providerId })
}
