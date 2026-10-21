/**
 * Disk maintenance manual-cleanup API.
 *
 * POST /api/maintenance/cleanup → MaintenanceResult   (needs CSRF)
 *
 * Runs one cleanup target synchronously (same implementation as the background
 * sweepers): buildcache (size-cap LRU eviction), repocache (unreferenced git
 * mirrors), remote-workspaces (stale /tmp/pipewright-* dirs on build machines),
 * local-temp (stale pipewright-* files in the server OS temp dir). Remote and
 * local sweeps only remove entries older than 48h — in-flight builds are never
 * touched.
 */

import { http } from './http'

export type MaintenanceTarget = 'buildcache' | 'repocache' | 'images' | 'remote-workspaces' | 'local-temp'

export interface MachineSweepResult {
  serverId: string
  name: string
  ok: boolean
  /** Exact command executed on that machine ($ prefix, same echo format as build logs). */
  command: string
  error?: string
}

export interface MaintenanceResult {
  target: string
  ok: boolean
  items: number
  bytes: number
  detail?: string
  /** What the cleanup actually did: scanned dirs, deleted paths (capped server-side). */
  steps?: string[]
  machines?: MachineSweepResult[]
}

export async function runMaintenanceCleanup(target: MaintenanceTarget): Promise<MaintenanceResult> {
  return http.post<MaintenanceResult>('/api/maintenance/cleanup', { target })
}
