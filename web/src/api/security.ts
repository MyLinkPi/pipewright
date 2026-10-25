/**
 * Security center (安全中心) API client.
 *
 * Trivy(主机包 + 容器镜像 CVE)与 Nuclei(暴露面探测)扫描;trivy-server 扫描器
 * 登记(应用市场部署自动登记 + 手动登记外部实例);前端可配定时规则;HTML 审计报告
 * 下载。扫描异步:POST 立即返回 running run,结果经 overview/history/findings 轮询。
 *
 * GET    /api/security/overview                    → SecurityOverview
 * GET    /api/security/scanners                    → { items: ScannerInstance[] }
 * POST   /api/security/scanners                    → ScannerInstance       (CSRF)
 * POST   /api/security/scanners/test               → TestResult            (CSRF;直连测试)
 * PUT    /api/security/scanners/{id}               → ScannerInstance       (CSRF)
 * DELETE /api/security/scanners/{id}               → { ok }                (CSRF)
 * POST   /api/security/scanners/{id}/test          → TestResult            (CSRF)
 * POST   /api/security/scans                       → SecRun                (CSRF;单机)
 * POST   /api/security/scans/fleet                 → { items: SecRun[] }   (CSRF;全量)
 * GET    /api/security/scans?serverId&scanType&limit → { items: SecRun[] }
 * GET    /api/security/scans/{id}/findings?severity&source&q&offset&limit → FindingsPage
 * GET    /api/security/schedule                    → Schedule
 * PUT    /api/security/schedule                    → Schedule              (CSRF)
 * GET    /api/security/report?serverId&format=html → HTML 文件下载
 */
import { http } from './http'

export type ScanType = 'trivy' | 'nuclei'
export type RunStatus = 'running' | 'done' | 'failed'

export interface ScanSummary {
  critical: number
  high: number
  medium: number
  low: number
  info: number
  unknown: number
  total: number
  imageCount?: number
  targetCount?: number
}

export interface ScannerInstance {
  id: string
  kind: string
  name: string
  host: string
  port: number
  tokenConfigured: boolean
  source: 'manual' | 'appstore'
  serverId?: string
  stackName?: string
  createdAt: string
  updatedAt: string
}

export interface ScannerTestResult {
  ok: boolean
  latencyMs: number
  detail: string
}

export interface SecRun {
  id: string
  serverId: string
  serverName: string
  scanType: ScanType
  status: RunStatus
  trigger?: string
  summary: ScanSummary
  error?: string
  startedAt: string
  finishedAt?: string
  findingsUrl?: string
}

export interface OverviewServer {
  serverId: string
  serverName: string
  host: string
  trivy?: OverviewRunLite
  nuclei?: OverviewRunLite
  totals?: ScanSummary
}

export interface OverviewRunLite {
  runId: string
  scanType: ScanType
  status: RunStatus
  summary: ScanSummary
  error?: string
  finishedAt?: string
}

export interface SecurityOverview {
  servers: OverviewServer[]
  totals: ScanSummary
  scanned: number
  serverTotal: number
  lastScanAt?: string
}

export interface Finding {
  severity: string
  source: string
  target: string
  id: string
  title?: string
  installedVersion?: string
  fixedVersion?: string
  url?: string
}

export interface FindingsPage {
  total: number
  items: Finding[]
  summary: ScanSummary
}

export interface Schedule {
  enabled: boolean
  expression: string
  scanTypes: ScanType[]
  lastRunAt?: string
  updatedAt: string
}

export interface StartScanInput {
  serverId: string
  scanType: ScanType
  targets?: string[]
}

// ---------- 扫描器 ----------

export const listScanners = (): Promise<{ items: ScannerInstance[] }> =>
  http.get('/api/security/scanners')

export const createScanner = (body: { name: string; host: string; port: number; token?: string }): Promise<ScannerInstance> =>
  http.post('/api/security/scanners', body)

export const updateScanner = (id: string, body: { name?: string; host?: string; port?: number; token?: string }): Promise<ScannerInstance> =>
  http.put(`/api/security/scanners/${id}`, body)

export const deleteScanner = (id: string): Promise<{ ok: boolean }> =>
  http.delete(`/api/security/scanners/${id}`)

export const testScanner = (id: string): Promise<ScannerTestResult> =>
  http.post(`/api/security/scanners/${id}/test`, {})

export const testScannerDirect = (body: { host: string; port: number; token?: string }): Promise<ScannerTestResult> =>
  http.post('/api/security/scanners/test', body)

export interface ScannerLibInfo {
  kind: string
  updatedAt?: string
  note?: string
}

/** getScannerLib:规则库状态(漏洞库/特征模板更新时间)。 */
export const getScannerLib = (id: string): Promise<ScannerLibInfo> =>
  http.get(`/api/security/scanners/${encodeURIComponent(id)}/lib`)

/** updateScannerLib:手动更新规则库(trivy 重启拉库 / nuclei 更新模板)。 */
export const updateScannerLib = (id: string): Promise<ScannerLibInfo> =>
  http.post(`/api/security/scanners/${encodeURIComponent(id)}/lib/update`, {})

// ---------- 扫描 ----------

export const startScan = (input: StartScanInput): Promise<SecRun> =>
  http.post('/api/security/scans', input)

export const startFleetScan = (scanTypes: ScanType[]): Promise<{ items: SecRun[] }> =>
  http.post('/api/security/scans/fleet', { scanTypes })

export const listRuns = (query: { serverId?: string; scanType?: string; limit?: number } = {}): Promise<{ items: SecRun[] }> => {
  const q = new URLSearchParams()
  if (query.serverId) q.set('serverId', query.serverId)
  if (query.scanType) q.set('scanType', query.scanType)
  if (query.limit) q.set('limit', String(query.limit))
  const qs = q.toString()
  return http.get(`/api/security/scans${qs ? `?${qs}` : ''}`)
}

export const getFindings = (
  runId: string,
  filter: { severity?: string; source?: string; q?: string; offset?: number; limit?: number } = {},
): Promise<FindingsPage> => {
  const q = new URLSearchParams()
  if (filter.severity) q.set('severity', filter.severity)
  if (filter.source) q.set('source', filter.source)
  if (filter.q) q.set('q', filter.q)
  if (filter.offset) q.set('offset', String(filter.offset))
  if (filter.limit) q.set('limit', String(filter.limit))
  const qs = q.toString()
  return http.get(`/api/security/scans/${runId}/findings${qs ? `?${qs}` : ''}`)
}

// ---------- 概览与定时 ----------

export const getOverview = (): Promise<SecurityOverview> =>
  http.get('/api/security/overview')

export const getSchedule = (): Promise<Schedule> =>
  http.get('/api/security/schedule')

export const saveSchedule = (body: { enabled: boolean; expression: string; scanTypes: ScanType[] }): Promise<Schedule> =>
  http.put('/api/security/schedule', body)

// ---------- WAF 防护态势 ----------

export interface WafInstanceStatus {
  instanceId: string
  name: string
  endpoint: string
  tokenConfigured: boolean
  ok: boolean
  detail?: string
  latencyMs?: number
  stats?: { blockedToday: number; blocked7d: number }
}

/** getWafOverview:SafeLine 实例的连通与拦截态势(未配置 Token/不可达在 detail 里)。 */
export const getWafOverview = (): Promise<{ items: WafInstanceStatus[] }> =>
  http.get('/api/security/waf')

// ---------- AI 解读 ----------

/** explainFinding 把一条明细交 AI 生成人读解读(Markdown 文本;同 key 结果前端缓存)。 */
export const explainFinding = (body: {
  scanType: ScanType
  source: string
  severity: string
  id: string
  title?: string
  target: string
  installedVersion?: string
  fixedVersion?: string
  url?: string
  serverName: string
}): Promise<{ text: string }> => http.post('/api/security/findings/explain', body)

// ---------- 报告 ----------

/**
 * downloadSecurityReport 拉取 HTML 审计报告并以浏览器下载落盘(serverIds 空 = 全部服务器)。
 * 仿 dbAdmin.downloadDBExport:同源 cookie 鉴权(GET 免 CSRF),错误信封解析,
 * Content-Disposition 文件名,URL.createObjectURL + 临时 <a download> 触发下载。
 */
export async function downloadSecurityReport(serverIds?: string[]): Promise<string> {
  const q = serverIds?.length ? `?serverIds=${serverIds.map(encodeURIComponent).join(',')}` : ''
  const resp = await fetch(`/api/security/report${q}`, { credentials: 'same-origin' })
  if (!resp.ok) {
    let message = `HTTP ${resp.status}`
    try {
      const data = (await resp.json()) as { error?: { message?: string } }
      if (data.error?.message) message = data.error.message
    } catch {
      /* 非 JSON 错误体,保留状态码信息 */
    }
    throw new Error(message)
  }
  const blob = await resp.blob()
  const cd = resp.headers.get('Content-Disposition') ?? ''
  const m = /filename="?([^";]+)"?/.exec(cd)
  const filename = m?.[1] ?? 'security-report.html'
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
  return filename
}
