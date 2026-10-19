/**
 * ssar machine-monitoring API — matches the internal/ssar HTTP contract.
 *
 * GET  /api/ssar/overview                              → SsarOverview
 * POST /api/ssar/download                              → 202 (needs CSRF)
 * POST /api/ssar/install   { serverIds }                → { items: SsarInstallResult[] } (CSRF)
 * GET  /api/ssar/query?serverId=&view=&minutes=&step=  → SsarQueryResult (whitelisted views)
 * GET  /api/ssar/history?serverId=&hours=              → { points: SsarHistoryPoint[] }
 *
 * Overview / install statuses degrade per server (one host failing never 500s
 * the batch). Query rows carry machine-local time strings (labeled in the UI).
 */

import { http } from './http'

/** Platform-side cached installer packages (GitHub Release of MyLinkPi/ssar). */
export interface SsarBinStatus {
  /** Cached release tag ("" = nothing cached yet). */
  version: string
  /** Cached asset file names (rpm / deb). */
  packages: string[]
  dir: string
  downloading: boolean
  lastErr: string
}

/** Per-server ssar install / collector status (probe-based, TTL-cached). */
export interface SsarServerStatus {
  serverId: string
  name: string
  installed: boolean
  /** e.g. "1.0.5"; empty = unknown. */
  version: string
  collectorActive: boolean
  /** RFC3339 of the last platform-side sample; "" = never sampled. */
  lastSampleAt: string
  /** Human-readable probe failure (empty on success). */
  error: string
}

export interface SsarOverview {
  binaries: SsarBinStatus
  items: SsarServerStatus[]
}

export interface SsarInstallResult {
  serverId: string
  ok: boolean
  version: string
  error: string
}

/** One platform-side sample row (nullable fields = not collected that round). */
export interface SsarHistoryPoint {
  at: string
  cpuUser: number | null
  cpuSystem: number | null
  cpuIowait: number | null
  cpuIdle: number | null
  cpuSteal: number | null
  memTotalKb: number | null
  memFreeKb: number | null
  memAvailKb: number | null
  load1: number | null
  runq: number | null
  threads: number | null
  tcpCurrEstab: number | null
  tcpRetransSegs: number | null
  tcpSynRetrans: number | null
  tcpFastRetrans: number | null
  tcpRetransFail: number | null
}

export interface SsarQueryColumn {
  key: string
  unit: string
}

export interface SsarQueryRow {
  /** Machine-local time string, echoed as-is from ssar. */
  time: string
  /** Numeric when parseable, string otherwise; placeholder cells are absent. */
  values: Record<string, number | string>
}

export interface SsarQueryResult {
  view: string
  columns: SsarQueryColumn[]
  rows: SsarQueryRow[]
}

/** Whitelisted detail views (mirrors the backend queryViews table). */
export const SSAR_VIEWS = [
  'cpu',
  'mem',
  'tcp',
  'udp',
  'vm',
  'softirq',
  'retran',
  'tcpofo',
  'tcpdrop',
  'tcperr',
  'load5s',
] as const

export type SsarView = (typeof SSAR_VIEWS)[number]

/** Fetch per-server statuses + platform package cache state. */
export async function getSsarOverview(): Promise<SsarOverview> {
  return http.get<SsarOverview>('/api/ssar/overview')
}

/** Trigger an async package download (progress via overview.binaries.downloading). */
export async function triggerSsarDownload(): Promise<void> {
  await http.post<{ ok: boolean }>('/api/ssar/download')
}

/** Deploy the cached package onto the given servers (bounded concurrency, per-host results). */
export async function installSsar(serverIds: string[]): Promise<SsarInstallResult[]> {
  const res = await http.post<{ items: SsarInstallResult[] }>('/api/ssar/install', { serverIds })
  return res.items
}

/** Query machine-side fine-grained history for one whitelisted view. */
export async function querySsar(params: {
  serverId: string
  view: SsarView | string
  minutes?: number
  step?: number
}): Promise<SsarQueryResult> {
  const qs = new URLSearchParams()
  qs.set('serverId', params.serverId)
  qs.set('view', params.view)
  if (params.minutes != null) qs.set('minutes', String(params.minutes))
  if (params.step != null) qs.set('step', String(params.step))
  return http.get<SsarQueryResult>(`/api/ssar/query?${qs.toString()}`)
}

/** Fetch platform-side sampled history (overview charts). */
export async function getSsarHistory(serverId: string, hours = 24): Promise<SsarHistoryPoint[]> {
  const res = await http.get<{ points: SsarHistoryPoint[] }>(
    `/api/ssar/history?serverId=${encodeURIComponent(serverId)}&hours=${hours}`,
  )
  return res.points
}
