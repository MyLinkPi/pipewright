/**
 * Shared build admin API (0086 cluster-shared build machines, owner view).
 *
 * GET    /api/build/shared/jobs        → { jobs: SharedBuildJob[] }  (admin)
 * DELETE /api/build/shared/jobs/:id    → { ok: true }                (admin + CSRF)
 *
 * The jobs list is the owner environment's in-memory registry of cross-environment
 * shared build jobs currently queued/running/awaiting result pickup. Force-cancel
 * aborts a job (queued wait or in-flight SSH execution) and is audited server-side.
 */

import { http } from './http'

export type SharedBuildJobState = 'queued' | 'running' | 'succeeded' | 'failed' | 'canceled'

export interface SharedBuildJob {
  jobId: string
  state: SharedBuildJobState
  /** Originating peer environment display name (owner view). */
  peer: string
  /** Originating environment's run id (traceability). */
  runId: string
  jobName: string
  /** Assigned machine once running; empty while queued. */
  serverId: string
  serverName: string
  exitCode: number
  error?: string
  /** Artifact archive ready for peer pickup (success only). */
  hasResult: boolean
  queuedAt: string
  startedAt?: string
  endedAt?: string
}

export function listSharedBuildJobs(): Promise<SharedBuildJob[]> {
  return http.get<{ jobs: SharedBuildJob[] }>('/api/build/shared/jobs').then((res) => res.jobs)
}

export function cancelSharedBuildJob(jobId: string): Promise<{ ok: boolean }> {
  return http.delete(`/api/build/shared/jobs/${encodeURIComponent(jobId)}`)
}
