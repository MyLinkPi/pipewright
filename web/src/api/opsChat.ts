import { http, HttpError } from './http'
import { resetOpsAuth } from '../utils/opsAuth'
import { currentLocale } from '../i18n'

export type Status =
  | 'queued'
  | 'planning'
  | 'running'
  | 'awaiting_confirmation'
  | 'awaiting_analysis_consent'
  | 'succeeded'
  | 'partial_failed'
  | 'failed'
  | 'interrupted'
  | 'unknown'
export interface Provider {
  provider: string
  model: string
  configHash: string
}
export interface Capabilities {
  available: boolean
  modelAvailable: boolean
  provider: Provider
  limits: Record<string, number>
}
export interface ContextUsage {
  usedTokens: number
  windowTokens: number
  percent: number
}
export interface Session {
  id: string
  revision: number
  title: string
  draft: string
  serverIds: string[] | null
  /** 会话维度:缺省=机器;"project"=项目(绑定 projectId,可选关联机器)。 */
  scope?: string
  projectId?: string
  activeRunId: string
  watermark: number
  storageBytes?: number
  /** Latest measured context usage (absent until a round reports provider usage). */
  contextUsage?: ContextUsage
  createdAt: string
  updatedAt: string
}
export interface Run {
  phase?: 'analysis'
  id: string
  sessionId: string
  clientRequestId: string
  status: Status
  cancelRequested: boolean
  createdAt: string
}
export interface ToolArgs {
  container?: string
  argv?: string[]
  unit?: string
  lines?: number
  action?: string
  path?: string
  ref?: string
  branch?: string
  runId?: string
  stepOrdinal?: number
  status?: string
  limit?: number
  params?: Record<string, string>
}
export interface Tool {
  toolId: string
  mutation: boolean
  schema: unknown
  /** 发给模型的工具说明(后端目录携带;调试/展示用)。 */
  description?: string
  /** target 参数是否适用(机器工具 true;项目工具 false)。 */
  targeted?: boolean
}
export interface Resources {
  disks:
    | {
        filesystem: string
        mount: string
        totalBytes: number
        usedBytes: number
        availableBytes: number
        usedPercent: number
      }[]
    | null
  memory: {
    totalBytes: number
    usedBytes: number
    availableBytes: number
    swapTotalBytes: number
    swapUsedBytes: number
  } | null
  load: {
    one: number
    five: number
    fifteen: number
    uptimeSeconds: number
  } | null
  unavailable: string[] | null
}
export interface Call {
  callId: string
  runId: string
  sessionId: string
  serverId: string
  serverName: string
  toolId: string
  args: ToolArgs
  object: string
  purpose?: string
  reason?: string
  status: Status
  argsHash: string
  targetHash: string
  collectedAt?: string
  exitCode?: number
  output: string
  error: string
  truncated: boolean
  resources?: Resources
}
export interface BoundCall {
  callId: string
  serverId: string
  toolId: string
  object: string
  purpose?: string
  reason?: string
  argsHash: string
  targetHash: string
}
export interface Confirmation {
  runId: string
  nonce: string
  expiresAt: string
  calls: BoundCall[]
  valid: boolean
}
export interface Entry {
  targetNames?: Record<string, string>
	 replaceSeq?: number
	 revisionSeq?: number
	 requestId?: string
	 call?: Call
	 run?: Run
	 session?: Session
	 confirmation?: Confirmation
	 summary?: RunSummary
  sessionId: string
  seq: number
  kind: string
  runId?: string
  callId?: string
  text?: string
  status?: Status | 'partial'
  /** Provider-measured usage of the round that produced this reply (if any). */
  usage?: { inputTokens: number; outputTokens: number }
  createdAt: string
}
export interface Occupant { id: string; name: string; cpuPercent: number; memoryPercent?: number; memoryUsage?: string }
export interface CallFact {callId: string;toolId: string;status: Status;truncated: boolean;error?: string;excerpt?: string;resources?: Resources;occupants?: Occupant[]}
export interface RunSummary {servers: {serverId: string;serverName: string;facts: CallFact[];missing: string[]}[]}
export interface EntryPage {
  entries: Entry[] | null
  cursor: string
  watermark: number
  reset: boolean
}
export interface SessionPage {
  sessions: Session[] | null
  cursor: string
  activeSessionId: string
}
export interface Snapshot {
  session: Session
  runs: Run[] | null
  calls: Call[] | null
  confirmations: Confirmation[] | null
  entries: EntryPage
  watermark: number
}
export interface Preview {
  callIds: string[]
  items: {
    callId: string
    targetAlias: string
    toolId: string
    output: string
    error: string
    status: Status
  }[]
  hash: string
  provider: Provider
}
export interface Turn {
  analyzeResults?: boolean
  provider?: Provider
  clientRequestId: string
  revision: number
  text: string
}
export interface Patch {
  revision: number
  title?: string
  draft?: string
  serverIds?: string[]
  /** 项目维度会话切换绑定项目(机器维度会话忽略/拒绝)。 */
  projectId?: string
}
export interface Retry {
  clientRequestId: string
  revision: number
  callIds: string[]
}
export interface Analysis {
  clientRequestId: string
  revision: number
  runId?: string
  callIds?: string[]
  analyzeResults?: boolean
  previewHash?: string
  provider: Provider
  consent?: boolean
}
const root = '/api/ai/ops'
const path = (id: string): string =>
  `${root}/sessions/${encodeURIComponent(id)}`

export interface RejectInput { runId: string; nonce: string; calls: BoundCall[]; reason?: string }
export interface ContinueInput { advice?: string }

async function rejectBatch(sessionId: string, input: RejectInput): Promise<Run> {
  return http.post<Run>(`/api/ai/ops/sessions/${sessionId}/runs/${input.runId}/reject`, input)
}
export async function continueRun(sessionId: string, runId: string, input: ContinueInput = {}): Promise<Run> {
  return http.post<Run>(`/api/ai/ops/sessions/${sessionId}/runs/${runId}/continue`, input)
}

export const opsApi = {
  capabilities: () => http.get<Capabilities>(`${root}/capabilities`),
  tools: (scope = '') =>
    http.get<Tool[]>(`${root}/tools${scope ? '?scope=' + encodeURIComponent(scope) : ''}`),
  list: (context='') => http.get<SessionPage>(`${root}/sessions?limit=100${context?'&context='+encodeURIComponent(context):''}`),
  create: (serverIds: string[], context = '', scope = '', projectId = '') =>
    http.post<Session>(`${root}/sessions`, {
      title: '',
      serverIds,
      ...(scope ? { scope } : {}),
      ...(projectId ? { projectId } : {}),
      ...(context ? { context } : {}),
    }),
  snapshot: (id: string) => http.get<Snapshot>(path(id)),
  patch: (id: string, input: Patch) => http.patch<Session>(path(id), input),
  activate: (id: string,context='') =>
    http.post<{ ok: boolean }>(`${path(id)}/activate`, context?{context}:{}),
  delete: (id: string) => http.delete<void>(path(id)),
  entries: (id: string, before: string) =>
    http.get<EntryPage>(
      `${path(id)}/entries?limit=50&before=${encodeURIComponent(before)}`,
    ),
  call: (id: string, callId: string) =>
    http.get<Call>(`${path(id)}/calls/${encodeURIComponent(callId)}`),
  turn: (id: string, input: Turn) => http.post<Run>(`${path(id)}/turns`, input),
  retry: (id: string, input: Retry) =>
    http.post<Run>(`${path(id)}/retry`, input),
  cancel: (id: string, run: string) =>
    http.post<Run>(`${path(id)}/runs/${encodeURIComponent(run)}/cancel`, {}),
  reject: (id: string, input: RejectInput) =>
    http.post<Run>(`${root}/sessions/${id}/runs/${input.runId}/reject`, input),
  continueRun: (id: string, runId: string, input: ContinueInput = {}) =>
    http.post<Run>(`${root}/sessions/${id}/runs/${runId}/continue`, input),
  confirm: (id: string, input: Confirmation) =>
    http.post<Run>(`${path(id)}/calls/confirm`, {
      runId: input.runId,
      nonce: input.nonce,
      calls: input.calls,
    }),
  reissue: (id: string, run: string) =>
    http.post<Confirmation>(
      `${path(id)}/runs/${encodeURIComponent(run)}/confirmation`,
      {},
    ),
  preview: (id: string, callIds: string[]) =>
    http.get<Preview>(
      `${path(id)}/analysis/preview?callIds=${encodeURIComponent(callIds.join(','))}`,
    ),
  analyze: (id: string, input: Analysis) =>
    http.post<Run>(`${path(id)}/analysis`, input),
}

export function requestId(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6]! & 15) | 64
  bytes[8] = (bytes[8]! & 63) | 128
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}
export const utf8Bytes = (text: string): number =>
  new TextEncoder().encode(text).length
export const execArgvLimits = { count: 61, tokenBytes: 2048, totalBytes: 4096 }
export function isValidExecArgv(argv: unknown): argv is string[] {
  if (!Array.isArray(argv) || argv.length < 1 || argv.length > execArgvLimits.count)
    return false
  const program = argv[0]
  if (typeof program !== 'string' || !/^[A-Za-z0-9_./][A-Za-z0-9_./+-]*$/.test(program) || program.endsWith('/'))
    return false
  const basename = program.slice(program.lastIndexOf('/') + 1)
  if (basename === '.' || basename === '..') return false
  let total = 0
  for (const token of argv) {
    // eslint-disable-next-line no-control-regex -- 拒绝控制字符正是该校验的目的
    if (typeof token !== 'string' || /[\u0000-\u001f\u007f-\u009f]/.test(token)) return false
    for (const character of token) {
      const codePoint = character.codePointAt(0)!
      if (codePoint >= 0xd800 && codePoint <= 0xdfff) return false
    }
    const bytes = utf8Bytes(token)
    if (bytes > execArgvLimits.tokenBytes) return false
    total += bytes
  }
  return total <= execArgvLimits.totalBytes
}
export function errorCode(error: unknown): string {
  if (error instanceof HttpError) {
    if (error.status === 401) return 'unauthorized'
    if (error.status === 403) return 'forbidden'
    if (error.status === 0) return 'network'
    const code = error.apiError?.code
    if (code && /^ops_[a-z_]+$/.test(code)) return code
  }
  return 'network'
}
export interface StreamEvent {
  type: string
  id: string
  data: unknown
}
// SSE framing, including split UTF-8 and CRLF chunks. Limit individual frames.
export function sseParser(
  deliver: (event: StreamEvent) => void,
): (chunk: string) => void {
  let buffer = ''
  const maxFrame = 256 * 1024
  return (chunk) => {
    buffer += chunk
    let match: RegExpExecArray | null
    while ((match = /\r?\n\r?\n/.exec(buffer))) {
      const block = buffer.slice(0, match.index)
      buffer = buffer.slice(match.index + match[0].length)
      if (block.length > maxFrame) throw new Error('SSE frame too large')
      let type = 'message',
        id = ''
      const data: string[] = []
      for (const line of block.split(/\r?\n/)) {
        const colon = line.indexOf(':')
        const key = colon < 0 ? line : line.slice(0, colon)
        const value = colon < 0 ? '' : line.slice(colon + 1).replace(/^ /, '')
        if (key === 'event') type = value
        else if (key === 'id' && !value.includes('\0')) id = value
        else if (key === 'data') data.push(value)
      }
      if (data.length)
        deliver({ type, id, data: JSON.parse(data.join('\n')) as unknown })
    }
    if (buffer.length > maxFrame) throw new Error('SSE frame too large')
  }
}
export async function streamOps(
  id: string,
  after: string,
  signal: AbortSignal,
  deliver: (event: StreamEvent) => void,
): Promise<void> {
  const response = await fetch(
    `${path(id)}/events?after=${encodeURIComponent(after)}`,
    {
      credentials: 'same-origin',
      cache: 'no-store',
      signal,
      headers: {
        Accept: 'text/event-stream',
        'X-Pipewright-Locale': currentLocale(),
      },
    },
  )
  if (response.status === 401) resetOpsAuth()
  if (!response.ok)
    throw new HttpError(response.status, null, 'Stream unavailable')
  if (
    !response.body ||
    !response.headers.get('content-type')?.includes('text/event-stream')
  )
    throw new Error('Invalid stream')
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  const parse = sseParser(deliver)
  try {
    while (!signal.aborted) {
      const part = await reader.read()
      if (part.done) break
      parse(decoder.decode(part.value, { stream: true }))
    }
    parse(decoder.decode())
  } finally {
    await reader.cancel().catch(() => undefined)
    reader.releaseLock()
  }
}
