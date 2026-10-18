/**
 * Redis 管理 API client（设置 · Redis 注册 + 顶级「Redis」页键浏览）。
 *
 * 一个 `RedisInstance` 是一个外部 Redis 接入（host:port + 可选密码 + 默认逻辑库号）。
 * 密码经后端保险库密文存储，**绝不回显**——列表只以 `passwordConfigured` 告知是否已配置
 * （无密码实例合法）。实例默认只读，删键/改 TTL 前须显式解锁写模式（`setReadonly`）。
 *
 * 键空间可能百万级：键浏览一律 SCAN 游标分页（`nextCursor` 为 '0' 即遍历完成），
 * 绝不整库拉取；值预览由服务端截断（string 前 2KB / 集合前 100 成员）。
 *
 * GET    /api/redis-instances                        → { items: RedisInstance[] }
 * POST   /api/redis-instances                        → RedisInstance          (needs CSRF)
 * POST   /api/redis-instances/test                   → RedisTestResult        (needs CSRF)
 * PUT    /api/redis-instances/{id}                   → RedisInstance          (needs CSRF)
 * DELETE /api/redis-instances/{id}                   → { ok: true }           (needs CSRF)
 * POST   /api/redis-instances/{id}/test              → RedisTestResult        (needs CSRF)
 * PUT    /api/redis-instances/{id}/readonly          → RedisInstance          (needs CSRF)
 * GET    /api/redis-instances/{id}/info?db=          → RedisServerInfo
 * GET    /api/redis-instances/{id}/keys?db=&cursor=&match=&count=  → RedisScanResult
 * GET    /api/redis-instances/{id}/keys/detail?key=&db=            → RedisKeyDetail
 * DELETE /api/redis-instances/{id}/keys?key=&db=     → { ok: true }           (needs CSRF)
 * POST   /api/redis-instances/{id}/keys/ttl          → { ok: true }           (needs CSRF)
 */

import { http } from './http'

/** 一条受管 Redis 实例。密码永不返回。 */
export interface RedisInstance {
  id: string
  name: string
  host: string
  port: number
  /** host:port（展示用）。 */
  endpoint: string
  /** 默认逻辑库号（0-15；管理页可临时切换）。 */
  dbNum: number
  /** 只读锁：true 时删键/改 TTL 被服务端拒绝。 */
  readonly: boolean
  /** manual = 手工登记；appstore = 应用市场部署 redis 自动登记。 */
  source: 'manual' | 'appstore'
  serverId: string
  stackName: string
  /** 服务端派生：是否配置了密码（无密码实例合法）。 */
  passwordConfigured: boolean
  createdAt: string
}

/** 创建实例入参。密码可空（内网无密码实例）。 */
export interface CreateRedisInstanceInput {
  name: string
  host: string
  port: number
  dbNum?: number
  password?: string
}

/** 编辑入参；缺省字段不变，password 非空即轮换（空串 = 清除）。 */
export interface UpdateRedisInstanceInput {
  name?: string
  host?: string
  port?: number
  dbNum?: number
  password?: string
}

/** 连接测试结果。 */
export interface RedisTestResult {
  ok: boolean
  latencyMs: number
  version: string
  /** 失败原因（成功为 null）。绝不含密码。 */
  error: string | null
}

/** 服务端概览（INFO + DBSIZE）。 */
export interface RedisServerInfo {
  version: string
  uptimeSeconds: number
  usedMemory: string
  connectedClients: number
  /** 当前库键数（DBSIZE）。 */
  dbSize: number
  /** 各逻辑库键数（键为库号字符串）。 */
  keyspace: Record<string, number>
}

/** 键浏览单页结果。nextCursor 为 '0' 表示遍历完成。 */
export interface RedisScanResult {
  keys: string[]
  nextCursor: string
}

/** hash 类型成员预览。 */
export interface RedisHashMember {
  field: string
  value: string
}

/** zset 类型成员预览。 */
export interface RedisZEntry {
  member: string
  score: number
}

/** 单键详情（值预览由服务端截断）。 */
export interface RedisKeyDetail {
  key: string
  type: string
  /** -1 = 持久化。 */
  ttlSeconds: number
  memoryBytes: number
  strValue: string
  strLen: number
  truncated: boolean
  len: number
  hashMembers: RedisHashMember[] | null
  listItems: string[] | null
  setItems: string[] | null
  zsetItems: RedisZEntry[] | null
}

/** 列出全部 Redis 实例（永不包含密码）。 */
export async function listRedisInstances(): Promise<RedisInstance[]> {
  const res = await http.get<{ items: RedisInstance[] }>('/api/redis-instances')
  return res.items ?? []
}

/** 登记实例（密码经保险库密文存储；可空 = 无密码实例）。 */
export async function createRedisInstance(input: CreateRedisInstanceInput): Promise<RedisInstance> {
  return http.post<RedisInstance>('/api/redis-instances', input)
}

/** 用未保存的凭据直连测试（密码用完即弃）。 */
export async function testRedisConnectionDirect(input: CreateRedisInstanceInput): Promise<RedisTestResult> {
  return http.post<RedisTestResult>('/api/redis-instances/test', input)
}

/** 编辑实例（密码留空不改动）。 */
export async function updateRedisInstance(
  id: string,
  input: UpdateRedisInstanceInput,
): Promise<RedisInstance> {
  return http.put<RedisInstance>(`/api/redis-instances/${encodeURIComponent(id)}`, input)
}

/** 删除实例。 */
export async function deleteRedisInstance(id: string): Promise<{ ok: boolean }> {
  return http.delete<{ ok: boolean }>(`/api/redis-instances/${encodeURIComponent(id)}`)
}

/** 测试已存实例连通性（PING + 版本）。 */
export async function testRedisInstance(id: string): Promise<RedisTestResult> {
  return http.post<RedisTestResult>(`/api/redis-instances/${encodeURIComponent(id)}/test`, {})
}

/** 解锁/上锁写模式（false = 解锁，是显式审计动作）。 */
export async function setRedisReadonly(id: string, readonly: boolean): Promise<RedisInstance> {
  return http.put<RedisInstance>(`/api/redis-instances/${encodeURIComponent(id)}/readonly`, { readonly })
}

/** 服务端概览（db 为目标逻辑库）。 */
export async function getRedisInfo(id: string, db: number): Promise<RedisServerInfo> {
  return http.get<RedisServerInfo>(`/api/redis-instances/${encodeURIComponent(id)}/info?db=${db}`)
}

/**
 * SCAN 游标分页浏览键（键空间可能百万级，绝不整库拉取）。
 * cursor 为 '0' 表示从头开始；返回 nextCursor 为 '0' 表示遍历完成。
 */
export async function scanRedisKeys(
  id: string,
  params: { db: number; cursor?: string; match?: string; count?: number },
): Promise<RedisScanResult> {
  const qs = new URLSearchParams()
  qs.set('db', String(params.db))
  if (params.cursor !== undefined) qs.set('cursor', params.cursor)
  if (params.match !== undefined) qs.set('match', params.match)
  if (params.count !== undefined) qs.set('count', String(params.count))
  return http.get<RedisScanResult>(
    `/api/redis-instances/${encodeURIComponent(id)}/keys?${qs.toString()}`,
  )
}

/** 单键详情（值预览截断）。 */
export async function getRedisKeyDetail(id: string, key: string, db: number): Promise<RedisKeyDetail> {
  const qs = new URLSearchParams()
  qs.set('key', key)
  qs.set('db', String(db))
  return http.get<RedisKeyDetail>(
    `/api/redis-instances/${encodeURIComponent(id)}/keys/detail?${qs.toString()}`,
  )
}

/** 删除键（须已解锁写模式）。 */
export async function deleteRedisKey(id: string, key: string, db: number): Promise<{ ok: boolean }> {
  const qs = new URLSearchParams()
  qs.set('key', key)
  qs.set('db', String(db))
  return http.delete<{ ok: boolean }>(
    `/api/redis-instances/${encodeURIComponent(id)}/keys?${qs.toString()}`,
  )
}

/** 设置/清除过期（ttl >= 0 = 秒；-1 = 持久化；须已解锁写模式）。 */
export async function setRedisKeyTTL(
  id: string,
  input: { db: number; key: string; ttl: number },
): Promise<{ ok: boolean }> {
  return http.post<{ ok: boolean }>(`/api/redis-instances/${encodeURIComponent(id)}/keys/ttl`, input)
}
