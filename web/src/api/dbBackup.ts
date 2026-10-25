/**
 * Instance backup & restore API(功能长在 /databases 与 /redis 页的备份面板)。
 *
 * kind 前缀:mysql/postgres → /api/db-instances;redis → /api/redis-instances。
 * 按备份记录寻址的端点共用 /api/db-backups/*。
 */

import { http } from './http'

export type DBBackupKind = 'mysql' | 'postgres' | 'redis'

export interface DBBackupConfig {
  intervalHours: number
  retention: number
  lastBackupAt: string
  autoUploadProviderId: string
  /** 能力判定(mode: protocol|ssh|none;外部 Redis 不可备份时 note 说明原因)。 */
  mode: string
  backupable: boolean
  restoreable: boolean
  note: string
}

export interface DBBackupConfigInput {
  intervalHours: number
  retention: number
  autoUploadProviderId?: string
}

export interface DBBackupRecord {
  id: string
  instanceKind: string
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

export interface DBRestoreResult {
  ok: boolean
  error?: string
  safetyBackup?: DBBackupRecord
}

function prefix(kind: DBBackupKind): string {
  return kind === 'redis' ? '/api/redis-instances' : '/api/db-instances'
}

export async function getDBBackupConfig(kind: DBBackupKind, id: string): Promise<DBBackupConfig> {
  return http.get<DBBackupConfig>(`${prefix(kind)}/${encodeURIComponent(id)}/backup-config`)
}

export async function setDBBackupConfig(kind: DBBackupKind, id: string, input: DBBackupConfigInput): Promise<void> {
  await http.put(`${prefix(kind)}/${encodeURIComponent(id)}/backup-config`, input)
}

export async function runDBBackup(kind: DBBackupKind, id: string): Promise<DBBackupRecord> {
  return http.post<DBBackupRecord>(`${prefix(kind)}/${encodeURIComponent(id)}/backups`)
}

export async function listDBBackups(kind: DBBackupKind, id: string): Promise<DBBackupRecord[]> {
  const res = await http.get<{ items: DBBackupRecord[] }>(`${prefix(kind)}/${encodeURIComponent(id)}/backups`)
  return res.items ?? []
}

export async function restoreDBBackupById(kind: DBBackupKind, id: string, backupId: string): Promise<DBRestoreResult> {
  return http.post<DBRestoreResult>(`${prefix(kind)}/${encodeURIComponent(id)}/restore`, { backupId })
}

export async function restoreDBBackupFromCloud(
  kind: DBBackupKind,
  id: string,
  providerId: string,
  objectKey: string,
): Promise<DBRestoreResult> {
  return http.post<DBRestoreResult>(`${prefix(kind)}/${encodeURIComponent(id)}/restore`, { providerId, objectKey })
}

export async function restoreDBBackupUpload(kind: DBBackupKind, id: string, file: File): Promise<DBRestoreResult> {
  const form = new FormData()
  form.append('file', file)
  return http.post<DBRestoreResult>(`${prefix(kind)}/${encodeURIComponent(id)}/restore`, form)
}

export function dbBackupDownloadUrl(bid: string): string {
  return `/api/db-backups/${encodeURIComponent(bid)}/download`
}

export async function deleteDBBackup(bid: string): Promise<void> {
  await http.delete(`/api/db-backups/${encodeURIComponent(bid)}`)
}

export async function uploadDBBackupToObjectStore(bid: string, providerId: string): Promise<void> {
  await http.post(`/api/db-backups/${encodeURIComponent(bid)}/upload-objectstore`, { providerId })
}
