/**
 * Database admin (phpMyAdmin-style online management) API client.
 *
 * 实例 = 一个外部 MySQL/PostgreSQL 接入(密码经 vault 密文,绝不回显)。
 * 实例默认只读;解锁写模式是显式动作(PUT readonly)。应用市场部署 mysql/postgres
 * 模板成功后自动登记(source=appstore)。
 *
 * GET/POST   /api/db-instances                     → { items } / DBInstance   (POST CSRF)
 * POST       /api/db-instances/test                → DBTestResult            (CSRF;未存凭据直连测试)
 * PUT/DELETE /api/db-instances/{id}                → DBInstance / { ok }     (CSRF)
 * POST       /api/db-instances/{id}/test           → DBTestResult            (CSRF)
 * PUT        /api/db-instances/{id}/readonly       → DBInstance              (CSRF;解锁/上锁)
 * GET        /api/db-instances/{id}/info           → DBServerInfo
 * GET/POST   /api/db-instances/{id}/databases      → { items } / { ok }      (POST CSRF)
 * DELETE     /api/db-instances/{id}/databases/{db} → { ok }                  (CSRF)
 * GET        /api/db-instances/{id}/databases/{db}/tables                    → { items }
 * GET        .../tables/{t}/structure              → TableStructure
 * GET        .../tables/{t}/rows?limit&offset&sort&dir&where → BrowseResult
 * POST/PUT   .../tables/{t}/rows                   → { ok }                  (CSRF;插入/更新)
 * POST       .../tables/{t}/rows/get | rows/delete → 行回显 / { ok }         (CSRF)
 * POST       .../tables/{t}/drop | truncate        → { ok }                  (CSRF)
 * POST       /api/db-instances/{id}/query          → QueryResult             (CSRF;SQL 控制台)
 * POST       /api/db-instances/{id}/import         → ImportResult            (CSRF)
 * GET        /api/db-instances/{id}/export         → 文件流(附件下载)
 * GET/POST   /api/db-instances/{id}/users[-...]    → 用户/授权管理           (写方法 CSRF)
 */
import { http, HttpError } from './http'

export type DBEngine = 'mysql' | 'postgres'
export type DBTLAMode = '' | 'skip-verify'
export type GrantTemplate = 'all' | 'read' | 'crud'

export interface DBInstance {
  id: string
  name: string
  engine: DBEngine
  host: string
  port: number
  username: string
  tls: DBTLAMode
  readonly: boolean
  source: 'manual' | 'appstore'
  serverId?: string
  stackName?: string
  createdAt: string
  updatedAt: string
}

export interface DBTestResult {
  ok: boolean
  latencyMs: number
  version: string
  error?: string
}

export interface DBServerInfo {
  engine: DBEngine
  version: string
  versionFull: string
  uptimeSeconds: number
}

export interface DatabaseInfo {
  name: string
  tablesCount: number
  sizeBytes: number
}

export interface TableInfo {
  schema: string
  name: string
  /** MySQL = 表名;PostgreSQL = schema.表名(后续引用整体传回)。 */
  ref: string
  engine?: string
  collation?: string
  rows: number
  sizeBytes: number
  comment?: string
}

export interface ColumnInfo {
  name: string
  dataType: string
  nullable: boolean
  default: string | null
  extra?: string
  key?: string
  comment?: string
}

export interface IndexInfo {
  name: string
  unique: boolean
  primary: boolean
  columns: string[]
  definition?: string
}

export interface TableStructure {
  columns: ColumnInfo[]
  indexes: IndexInfo[]
  primaryKeys: string[]
}

export interface BrowseResult {
  total: number
  columns: string[]
  rows: unknown[][]
}

export interface RowKey {
  columns: string[]
  values: string[]
}

export interface QueryResult {
  isSelect: boolean
  columns?: string[]
  rows?: unknown[][]
  rowsAffected?: number
  truncated?: boolean
}

export interface ImportResult {
  ok: boolean
  message: string
}

export interface DBUser {
  name: string
  host?: string
  super?: boolean
}

export interface GrantDBRow {
  database: string
  connect: boolean
}

export interface GrantsView {
  engine: DBEngine
  lines?: string[]
  rows?: GrantDBRow[]
}

// ─── 实例注册 ──────────────────────────────────────────────────────────────────

export const listDBInstances = (): Promise<{ items: DBInstance[] }> =>
  http.get('/api/db-instances')

export interface CreateDBInstanceInput {
  name: string
  engine: DBEngine
  host: string
  port: number
  username: string
  password: string
  tls?: DBTLAMode
}

export const createDBInstance = (in_: CreateDBInstanceInput): Promise<DBInstance> =>
  http.post('/api/db-instances', in_)

export interface UpdateDBInstanceInput {
  name?: string
  host?: string
  port?: number
  username?: string
  password?: string
  tls?: DBTLAMode
}

export const updateDBInstance = (id: string, in_: UpdateDBInstanceInput): Promise<DBInstance> =>
  http.put(`/api/db-instances/${encodeURIComponent(id)}`, in_)

export const deleteDBInstance = (id: string): Promise<{ ok: boolean }> =>
  http.delete(`/api/db-instances/${encodeURIComponent(id)}`)

export const testDBInstance = (id: string): Promise<DBTestResult> =>
  http.post(`/api/db-instances/${encodeURIComponent(id)}/test`)

export const testDBConnectionDirect = (in_: CreateDBInstanceInput): Promise<DBTestResult> =>
  http.post('/api/db-instances/test', in_)

export const setDBInstanceReadonly = (id: string, readonly: boolean): Promise<DBInstance> =>
  http.put(`/api/db-instances/${encodeURIComponent(id)}/readonly`, { readonly })

// ─── 管理操作 ──────────────────────────────────────────────────────────────────

const base = (id: string) => `/api/db-instances/${encodeURIComponent(id)}`
const dbSeg = (db: string) => encodeURIComponent(db)
const tableSeg = (table: string) => encodeURIComponent(table)

export const getDBServerInfo = (id: string): Promise<DBServerInfo> =>
  http.get(`${base(id)}/info`)

export const listDBDatabases = (id: string): Promise<{ items: DatabaseInfo[] }> =>
  http.get(`${base(id)}/databases`)

export const createDBDatabase = (id: string, name: string): Promise<{ ok: boolean }> =>
  http.post(`${base(id)}/databases`, { name })

export const dropDBDatabase = (id: string, db: string): Promise<{ ok: boolean }> =>
  http.delete(`${base(id)}/databases/${dbSeg(db)}`)

export const listDBTables = (id: string, db: string): Promise<{ items: TableInfo[] }> =>
  http.get(`${base(id)}/databases/${dbSeg(db)}/tables`)

export const getDBTableStructure = (id: string, db: string, table: string): Promise<TableStructure> =>
  http.get(`${base(id)}/databases/${dbSeg(db)}/tables/${tableSeg(table)}/structure`)

export interface BrowseParams {
  limit?: number
  offset?: number
  sort?: string
  dir?: 'asc' | 'desc'
  where?: string
}

export const browseDBTable = (
  id: string,
  db: string,
  table: string,
  params: BrowseParams = {},
): Promise<BrowseResult> => {
  const q = new URLSearchParams()
  if (params.limit) q.set('limit', String(params.limit))
  if (params.offset) q.set('offset', String(params.offset))
  if (params.sort) q.set('sort', params.sort)
  if (params.dir) q.set('dir', params.dir)
  if (params.where) q.set('where', params.where)
  const qs = q.toString()
  return http.get(`${base(id)}/databases/${dbSeg(db)}/tables/${tableSeg(table)}/rows${qs ? `?${qs}` : ''}`)
}

export const insertDBRow = (
  id: string,
  db: string,
  table: string,
  values: Record<string, unknown>,
): Promise<{ ok: boolean }> =>
  http.post(`${base(id)}/databases/${dbSeg(db)}/tables/${tableSeg(table)}/rows`, { values })

export const updateDBRow = (
  id: string,
  db: string,
  table: string,
  key: RowKey,
  values: Record<string, unknown>,
): Promise<{ ok: boolean }> =>
  http.put(`${base(id)}/databases/${dbSeg(db)}/tables/${tableSeg(table)}/rows`, { key, values })

export const getDBRow = (
  id: string,
  db: string,
  table: string,
  key: RowKey,
): Promise<{ values: Record<string, unknown> }> =>
  http.post(`${base(id)}/databases/${dbSeg(db)}/tables/${tableSeg(table)}/rows/get`, { key })

export const deleteDBRow = (
  id: string,
  db: string,
  table: string,
  key: RowKey,
): Promise<{ ok: boolean }> =>
  http.post(`${base(id)}/databases/${dbSeg(db)}/tables/${tableSeg(table)}/rows/delete`, { key })

export const dropDBTable = (id: string, db: string, table: string): Promise<{ ok: boolean }> =>
  http.post(`${base(id)}/databases/${dbSeg(db)}/tables/${tableSeg(table)}/drop`)

export const truncateDBTable = (id: string, db: string, table: string): Promise<{ ok: boolean }> =>
  http.post(`${base(id)}/databases/${dbSeg(db)}/tables/${tableSeg(table)}/truncate`)

export const runDBQuery = (
  id: string,
  database: string,
  sql: string,
): Promise<QueryResult> =>
  http.post(`${base(id)}/query`, { database, sql })

export const importDBSQL = (id: string, database: string, sql: string): Promise<ImportResult> =>
  http.post(`${base(id)}/import`, { database, sql })

// ─── 用户与授权 ────────────────────────────────────────────────────────────────

export const listDBUsers = (id: string): Promise<{ items: DBUser[] }> =>
  http.get(`${base(id)}/users`)

export const createDBUser = (
  id: string,
  in_: { name: string; host?: string; password: string },
): Promise<{ ok: boolean }> => http.post(`${base(id)}/users`, in_)

export const setDBUserPassword = (
  id: string,
  in_: { name: string; host?: string; password: string },
): Promise<{ ok: boolean }> => http.post(`${base(id)}/users/password`, in_)

export const dropDBUser = (
  id: string,
  in_: { name: string; host?: string },
): Promise<{ ok: boolean }> => http.post(`${base(id)}/users/delete`, in_)

export const getDBUserGrants = (
  id: string,
  name: string,
  host?: string,
): Promise<GrantsView> => {
  const q = new URLSearchParams({ name })
  if (host) q.set('host', host)
  return http.get(`${base(id)}/users/grants?${q.toString()}`)
}

export const grantDBUser = (
  id: string,
  in_: { name: string; host?: string; database: string; template: GrantTemplate },
): Promise<{ ok: boolean }> => http.post(`${base(id)}/users/grant`, in_)

export const revokeDBUser = (
  id: string,
  in_: { name: string; host?: string; database: string },
): Promise<{ ok: boolean }> => http.post(`${base(id)}/users/revoke`, in_)

// ─── 导出(附件下载)──────────────────────────────────────────────────────────

/**
 * 导出表/整库为 SQL 或 CSV 并触发浏览器下载。GET 流式响应;服务端错误(未写出
 * 字节时)解析为统一错误信封抛 HttpError。
 */
export async function downloadDBExport(
  id: string,
  spec: { database: string; table?: string; format: 'sql' | 'csv' },
): Promise<void> {
  const q = new URLSearchParams({ database: spec.database, format: spec.format })
  if (spec.table) q.set('table', spec.table)
  const resp = await fetch(`${base(id)}/export?${q.toString()}`, { credentials: 'same-origin' })
  if (!resp.ok) {
    let message = `HTTP ${resp.status}`
    try {
      const body = (await resp.json()) as { error?: { message?: string } }
      if (body?.error?.message) message = body.error.message
    } catch {
      /* 非 JSON 错误体(如已截断的流)保持 HTTP 状态提示 */
    }
    throw new HttpError(resp.status, null, message)
  }
  const blob = await resp.blob()
  const cd = resp.headers.get('Content-Disposition') ?? ''
  const m = /filename="([^"]+)"/.exec(cd)
  const filename = m?.[1] ?? `export.${spec.format}`
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}
