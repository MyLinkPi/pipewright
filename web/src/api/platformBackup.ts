/**
 * Platform backup & restore API (设置 → 备份).
 *
 * GET    /api/backup/config                   → BackupConfigState
 * PUT    /api/backup/config                   (CSRF)
 * POST   /api/backup/run                      → BackupRecord (async; poll list)
 * GET    /api/backup/list                     → BackupRecord[]
 * GET    /api/backup/{id}/download            (attachment stream)
 * DELETE /api/backup/{id}                     (CSRF)
 * POST   /api/backup/{id}/upload-objectstore  (CSRF)
 * POST   /api/backup/restore                  (multipart; CSRF) → RestoreStageResult
 * GET    /api/backup/restore/pending          → RestorePendingState
 * DELETE /api/backup/restore/pending          (CSRF)
 */

import { http } from './http'

export interface BackupConfig {
  enabled: boolean
  intervalHours: number
  retentionCount: number
  includeArtifacts: boolean
  includeMasterKey: boolean
  objectstoreProviderId: string
  lastRunAt: string
  masterKeyConfigured: boolean
}

export interface BackupConfigState {
  config: BackupConfig
  restorePending: boolean
}

export interface BackupRecord {
  id: string
  fileName: string
  sizeBytes: number
  sha256: string
  kind: string
  status: string
  error: string
  objectKey: string
  dbDriver: string
  createdAt: string
  finishedAt: string
}

export interface RestoreStageResult {
  status: 'restarting' | 'deferred'
  message: string
}

export interface RestorePendingState {
  pending: boolean
  createdAt?: string
}

export async function getBackupConfig(): Promise<BackupConfigState> {
  return http.get<BackupConfigState>('/api/backup/config')
}

export async function saveBackupConfig(config: BackupConfig): Promise<void> {
  await http.put('/api/backup/config', config)
}

export async function runBackup(opts: { includeArtifacts?: boolean; passphrase?: string }): Promise<BackupRecord> {
  return http.post<BackupRecord>('/api/backup/run', opts)
}

export async function listBackups(): Promise<BackupRecord[]> {
  const res = await http.get<{ items: BackupRecord[] }>('/api/backup/list')
  return res.items ?? []
}

export function backupDownloadUrl(id: string): string {
  return `/api/backup/${encodeURIComponent(id)}/download`
}

export async function deleteBackup(id: string): Promise<void> {
  await http.delete(`/api/backup/${encodeURIComponent(id)}`)
}

export async function uploadBackupToObjectStore(id: string, providerId: string): Promise<void> {
  await http.post(`/api/backup/${encodeURIComponent(id)}/upload-objectstore`, { providerId })
}

export async function stageRestore(
  file: File,
  opts: { passphrase?: string; force?: boolean },
): Promise<RestoreStageResult> {
  const form = new FormData()
  form.append('file', file)
  if (opts.passphrase) form.append('passphrase', opts.passphrase)
  if (opts.force) form.append('force', '1')
  return http.post<RestoreStageResult>('/api/backup/restore', form)
}

/** 从对象存储直接拉取归档恢复(服务端下载到暂存区后走同一校验/重启流程)。 */
export async function stageRestoreFromCloud(
  providerId: string,
  objectKey: string,
  opts: { passphrase?: string; force?: boolean } = {},
): Promise<RestoreStageResult> {
  return http.post<RestoreStageResult>('/api/backup/restore', {
    providerId,
    objectKey,
    passphrase: opts.passphrase || undefined,
    force: opts.force || undefined,
  })
}

export async function getRestorePending(): Promise<RestorePendingState> {
  return http.get<RestorePendingState>('/api/backup/restore/pending')
}

export async function cancelRestorePending(): Promise<void> {
  await http.delete('/api/backup/restore/pending')
}
