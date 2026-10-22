/**
 * Aliyun ECS batch-import API client (设置 → 阿里云 ECS).
 *
 * An `AliyunEcsAccount` is one Aliyun **account** wiring (name + AccessKeyId +
 * AccessKeySecret + region). Multiple accounts coexist; multi-region is
 * expressed as multiple entries. Credentials are split by sensitivity
 * (same convention as dnsProviders / objectStores):
 *  - `accessKeyId` is non-secret — stored in plain sight, echoed back, editable.
 *  - the AccessKeySecret is **write-only** — supplied once (rotatable), sealed
 *    server-side via the vault, and **never** returned. Editing with an empty
 *    secret field leaves it unchanged.
 *
 * Import flow: pick an account → list its ECS instances → hand-pick a subset →
 * one SSH credential for the whole batch (existing vault credential or inline
 * password/key that the backend turns into a new credential) → per-instance
 * results. Connection address defaults to the ECS **private** IP (public
 * optional per batch).
 *
 * GET    /api/aliyun/accounts                    → { items: AliyunEcsAccount[] }
 * POST   /api/aliyun/accounts                    → AliyunEcsAccount     (needs CSRF)
 * PUT    /api/aliyun/accounts/{id}               → AliyunEcsAccount     (needs CSRF)
 * DELETE /api/aliyun/accounts/{id}               → { ok: true }         (needs CSRF)
 * POST   /api/aliyun/accounts/{id}/test          → { ok: true }         (needs CSRF)
 * GET    /api/aliyun/accounts/{id}/instances     → { items: AliyunEcsInstance[] }
 * POST   /api/aliyun/import                      → AliyunImportResult   (needs CSRF)
 * GET    /api/aliyun/power/status                → { items: AliyunEcsPowerItem[] }
 * POST   /api/aliyun/power                       → AliyunPowerResult    (needs CSRF)
 */

import { http } from './http'

/** Which address family to use when connecting to an ECS instance. Default: private. */
export type AliyunIpPreference = 'private' | 'public'

/** Inline credential kind for the import wizard: SSH password or PEM private key. */
export type AliyunInlineSecretKind = 'password' | 'key'

/** One Aliyun account wiring. The Secret is never returned by design. */
export interface AliyunEcsAccount {
  id: string
  /** Human-readable label, e.g. "生产区·华东1". */
  name: string
  /** Non-secret credential half (Aliyun AccessKeyId). */
  accessKeyId: string
  /** Region id, e.g. `cn-hangzhou` (one region per account entry). */
  region: string
  createdAt: string
  updatedAt: string
}

/** One ECS instance as listed for import selection. */
export interface AliyunEcsInstance {
  instanceId: string
  name: string
  hostname: string
  privateIps: string[]
  /** Merged public addresses: allocated public IP + EIP. May be empty. */
  publicIps: string[]
  osType: string
  status: string
  instanceType: string
  regionId: string
}

/** Body for creating an account. `secret` is write-only — sent once, never echoed. */
export interface CreateAliyunEcsAccountInput {
  name: string
  accessKeyId: string
  secret: string
  region: string
}

/** Body for editing an account. Omitted fields stay unchanged; `secret` rotates when set. */
export interface UpdateAliyunEcsAccountInput {
  name?: string
  accessKeyId?: string
  region?: string
  /** Optional Secret rotation (write-only; empty string means "leave unchanged" client-side). */
  secret?: string
}

/** Body for a batch import. One credential / port / user for the whole batch. */
export interface AliyunImportInput {
  accountId: string
  instanceIds: string[]
  ipPreference: AliyunIpPreference
  /** Existing vault credential (ssh_key / ssh_password) — mutually exclusive with inlineSecret. */
  credentialId?: string
  /** Inline secret the backend stores as a new vault credential for this batch. */
  inlineSecret?: {
    kind: AliyunInlineSecretKind
    /** Credential display name; empty → backend default ("阿里云 ECS · <region>"). */
    name?: string
    secret: string
  }
  sshUser: string
  sshPort?: number
}

/** Per-instance import outcome (independent — one failure doesn't fail the batch). */
export interface AliyunImportItemResult {
  instanceId: string
  name: string
  serverId: string
  action: 'created' | 'updated' | ''
  ok: boolean
  error?: string
}

/** Whole-batch import summary. */
export interface AliyunImportResult {
  items: AliyunImportItemResult[]
  total: number
  ok: number
  failed: number
}

/**
 * Public Aliyun ECS regions (2026-10 snapshot of the official regions list;
 * retiring local regions excluded). `value` is the region id; `label` carries
 * the zone name and `hint` repeats the id so AppSelect's search matches both.
 */
export const ALIYUN_REGIONS: ReadonlyArray<{ value: string; label: string; hint: string }> = [
  { value: 'cn-qingdao', label: '华北1 青岛 Qingdao', hint: 'cn-qingdao' },
  { value: 'cn-beijing', label: '华北2 北京 Beijing', hint: 'cn-beijing' },
  { value: 'cn-zhangjiakou', label: '华北3 张家口 Zhangjiakou', hint: 'cn-zhangjiakou' },
  { value: 'cn-huhehaote', label: '华北5 呼和浩特 Hohhot', hint: 'cn-huhehaote' },
  { value: 'cn-wulanchabu', label: '华北6 乌兰察布 Ulanqab', hint: 'cn-wulanchabu' },
  { value: 'cn-hangzhou', label: '华东1 杭州 Hangzhou', hint: 'cn-hangzhou' },
  { value: 'cn-shanghai', label: '华东2 上海 Shanghai', hint: 'cn-shanghai' },
  { value: 'cn-wuhan-lr', label: '华中1 武汉 Wuhan（本地地域）', hint: 'cn-wuhan-lr' },
  { value: 'cn-shenzhen', label: '华南1 深圳 Shenzhen', hint: 'cn-shenzhen' },
  { value: 'cn-heyuan', label: '华南2 河源 Heyuan', hint: 'cn-heyuan' },
  { value: 'cn-guangzhou', label: '华南3 广州 Guangzhou', hint: 'cn-guangzhou' },
  { value: 'cn-chengdu', label: '西南1 成都 Chengdu', hint: 'cn-chengdu' },
  { value: 'cn-zhongwei', label: '西北2 中卫 Zhongwei', hint: 'cn-zhongwei' },
  { value: 'cn-hongkong', label: '中国香港 Hong Kong', hint: 'cn-hongkong' },
  { value: 'ap-southeast-1', label: '新加坡 Singapore', hint: 'ap-southeast-1' },
  { value: 'ap-southeast-3', label: '马来西亚 吉隆坡 Kuala Lumpur', hint: 'ap-southeast-3' },
  { value: 'ap-southeast-5', label: '印度尼西亚 雅加达 Jakarta', hint: 'ap-southeast-5' },
  { value: 'ap-southeast-6', label: '菲律宾 马尼拉 Manila', hint: 'ap-southeast-6' },
  { value: 'ap-southeast-7', label: '泰国 曼谷 Bangkok', hint: 'ap-southeast-7' },
  { value: 'ap-southeast-8', label: '马来西亚 柔佛 Johor', hint: 'ap-southeast-8' },
  { value: 'ap-northeast-1', label: '日本 东京 Tokyo', hint: 'ap-northeast-1' },
  { value: 'ap-northeast-2', label: '韩国 首尔 Seoul', hint: 'ap-northeast-2' },
  { value: 'us-west-1', label: '美国 硅谷 Silicon Valley', hint: 'us-west-1' },
  { value: 'us-east-1', label: '美国 弗吉尼亚 Virginia', hint: 'us-east-1' },
  { value: 'eu-central-1', label: '德国 法兰克福 Frankfurt', hint: 'eu-central-1' },
  { value: 'eu-west-1', label: '英国 伦敦 London', hint: 'eu-west-1' },
  { value: 'eu-west-2', label: '法国 巴黎 Paris', hint: 'eu-west-2' },
  { value: 'me-east-1', label: '阿联酋 迪拜 Dubai', hint: 'me-east-1' },
  { value: 'me-central-1', label: '沙特 利雅得 Riyadh', hint: 'me-central-1' },
  { value: 'na-south-1', label: '墨西哥 Mexico', hint: 'na-south-1' },
  { value: 'sa-east-1', label: '巴西 圣保罗 São Paulo', hint: 'sa-east-1' },
]

/** Human label for a region id (falls back to the raw id for unknown/custom values). */
export function aliyunRegionLabel(region: string): string {
  const hit = ALIYUN_REGIONS.find((r) => r.value === region)
  return hit ? hit.label : region
}

/** List all configured Aliyun accounts. Secrets are never included. */
export async function listAliyunAccounts(): Promise<AliyunEcsAccount[]> {
  const res = await http.get<{ items: AliyunEcsAccount[] }>('/api/aliyun/accounts')
  return res.items ?? []
}

/** Add an Aliyun account (Secret sealed write-only via the vault). Returns the created DTO. */
export async function createAliyunAccount(input: CreateAliyunEcsAccountInput): Promise<AliyunEcsAccount> {
  return http.post<AliyunEcsAccount>('/api/aliyun/accounts', input)
}

/** Edit an account (name / accessKeyId / region; optional Secret rotation without re-creating). */
export async function updateAliyunAccount(id: string, input: UpdateAliyunEcsAccountInput): Promise<AliyunEcsAccount> {
  return http.put<AliyunEcsAccount>(`/api/aliyun/accounts/${encodeURIComponent(id)}`, input)
}

/** Delete an account (instance links removed with it; imported servers are untouched). */
export async function deleteAliyunAccount(id: string): Promise<{ ok: boolean }> {
  return http.delete<{ ok: boolean }>(`/api/aliyun/accounts/${encodeURIComponent(id)}`)
}

/** Verify an account's credentials with a minimal DescribeInstances call. */
export async function testAliyunAccount(id: string): Promise<{ ok: boolean }> {
  return http.post<{ ok: boolean }>(`/api/aliyun/accounts/${encodeURIComponent(id)}/test`, {})
}

/** List all ECS instances under the account's region (import selection data source). */
export async function listAliyunInstances(accountId: string): Promise<AliyunEcsInstance[]> {
  const res = await http.get<{ items: AliyunEcsInstance[] }>(
    `/api/aliyun/accounts/${encodeURIComponent(accountId)}/instances`,
  )
  return res.items ?? []
}

/** Import / update the selected instances as servers. Returns per-instance results. */
export async function importAliyunInstances(input: AliyunImportInput): Promise<AliyunImportResult> {
  return http.post<AliyunImportResult>('/api/aliyun/import', input)
}

// ─── ECS power (server-status page power buttons) ─────────────────────────────
//
// GET  /api/aliyun/power/status → { items } (read-only; live per-account status)
// POST /api/aliyun/power        → per-instance results (needs CSRF + audit)
//
// Both operate on **registered servers** linked to imported ECS instances via
// aliyun_ecs_instance_links. Fire-and-forget semantics: start/stop is issued,
// never waited for; already-in-target instances are skipped idempotently.

/** One linked server's live ECS power state (status empty + error set = unknown). */
export interface AliyunEcsPowerItem {
  serverId: string
  accountId: string
  instanceId: string
  /** Aliyun raw status: Running | Stopped | Starting | Stopping | … */
  status: string
  /** Human-readable per-account query failure (credentials / network). */
  error?: string
}

/** Per-instance power action outcome (independent — one failure doesn't fail the rest). */
export interface AliyunPowerResultItem {
  instanceId: string
  serverId?: string
  skipped: boolean
  ok: boolean
  error?: string
}

/** Whole power action summary. */
export interface AliyunPowerResult {
  items: AliyunPowerResultItem[]
  total: number
  ok: number
  failed: number
}

/** Aliyun ECS power action. Stop uses economy mode (StopCharging), never force. */
export type AliyunPowerAction = 'start' | 'stop'

/**
 * Live power status for every imported-ECS-linked server. Servers absent from the
 * list are not ECS-linked (no power UI). Account-level failures surface as items
 * with `error` set and empty `status` — they never fail the whole request.
 */
export async function getAliyunPowerStatus(): Promise<AliyunEcsPowerItem[]> {
  const res = await http.get<{ items: AliyunEcsPowerItem[] }>('/api/aliyun/power/status')
  return res.items ?? []
}

/** Issue start/stop for one registered server's linked ECS instances (no waiting). */
export async function aliyunServerPower(serverId: string, action: AliyunPowerAction): Promise<AliyunPowerResult> {
  return http.post<AliyunPowerResult>('/api/aliyun/power', { serverId, action })
}
