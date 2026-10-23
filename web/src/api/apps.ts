/**
 * App store (DPanel-style one-click deploy) API client.
 *
 * 模板 = docker-compose YAML + 参数 schema。内置模板(MySQL/Redis/…)启动 seed;自定义
 * 模板完整 CRUD。部署:参数渲染(secret 空缺自动生成,生成值仅响应一次性返回)→ 复用
 * Stacks 受管链路(/opt/pipewright/stacks/<name> + docker compose up -d)。
 *
 * GET    /api/ops/apps                    → { items: AppTemplate[] }
 * POST   /api/ops/apps                    → AppTemplate          (CSRF)
 * GET    /api/ops/apps/{id}               → AppTemplate
 * PUT    /api/ops/apps/{id}               → AppTemplate          (CSRF;内置只读)
 * DELETE /api/ops/apps/{id}               → { ok }               (CSRF;内置只读)
 * POST   /api/servers/{id}/apps/deploy    → AppDeployResult      (CSRF)
 * GET    /api/servers/{id}/apps/upgrade-context → AppUpgradeContext(升级预填;secret 只回名)
 * POST   /api/servers/{id}/apps/upgrade   → AppDeployResult      (CSRF;原位升级,参数原位保留)
 * GET    /api/servers/apps/instances      → { items: AppInstance[] }(全机已部署实例枚举)
 */
import { http } from './http'

export type AppParamType = 'string' | 'int' | 'secret'

export interface AppParamSpec {
  name: string
  label?: string
  type: AppParamType
  default?: string
  required: boolean
  autoGenerate?: boolean
}

export interface AppTemplate {
  id: string
  name: string
  displayName: string
  description: string
  icon: string
  composeYaml: string
  params: AppParamSpec[]
  builtin: boolean
  createdAt: string
  updatedAt: string
}

export interface AppDeployResult {
  serverId: string
  /** 生效栈名:模板名,或填写实例名后的「模板名-实例名」。 */
  name: string
  ok: boolean
  output: string
  error: string
  /** 生效参数(含自动生成的 secret;仅本次部署响应一次性返回)。 */
  params?: Record<string, string>
  /** RustFS 联动:部署成功后是否已自动登记为对象存储配置(尽力而为,失败不影响 ok)。 */
  objectStoreRegistered?: boolean
  /** RustFS 联动登记的人话备注(成功说明或失败原因;未触发时缺省)。 */
  objectStoreNote?: string
  /** mysql/postgres 联动:部署成功后是否已自动登记为数据库实例(尽力而为,失败不影响 ok)。 */
  dbRegistered?: boolean
  /** mysql/postgres 联动登记的人话备注(成功说明或失败原因;未触发时缺省)。 */
  dbNote?: string
  /** redis 联动:部署成功后是否已自动登记为 Redis 管理实例(尽力而为,失败不影响 ok)。 */
  redisRegistered?: boolean
  /** redis 联动登记的人话备注(成功说明或失败原因;未触发时缺省)。 */
  redisNote?: string
}

export interface AppTemplateInput {
  name: string
  displayName: string
  description: string
  icon: string
  composeYaml: string
  params: AppParamSpec[]
}

export const listAppTemplates = (): Promise<{ items: AppTemplate[] }> =>
  http.get('/api/ops/apps')

export const createAppTemplate = (in_: AppTemplateInput): Promise<AppTemplate> =>
  http.post('/api/ops/apps', in_)

export const deleteAppTemplate = (id: string): Promise<{ ok: boolean }> =>
  http.delete(`/api/ops/apps/${encodeURIComponent(id)}`)

/** 部署时可选的全服务资源限制(注入 compose deploy.resources.limits)。 */
export interface AppDeployLimits {
  /** CPU 核数上限,如 "1.0"。 */
  cpus?: string
  /** 内存硬上限,如 "512m"。 */
  memory?: string
}

export const deployApp = (
  serverId: string,
  templateId: string,
  params: Record<string, string>,
  limits?: AppDeployLimits,
  instance?: string,
): Promise<AppDeployResult> =>
  http.post(`/api/servers/${encodeURIComponent(serverId)}/apps/deploy`, {
    templateId,
    params,
    ...(instance?.trim() ? { instance: instance.trim() } : {}),
    ...(limits?.cpus?.trim() ? { cpus: limits.cpus.trim() } : {}),
    ...(limits?.memory?.trim() ? { memory: limits.memory.trim() } : {}),
  })

/** 升级上下文:既有实例的参数现值(found=false = 无参数记录,不可原位升级)。 */
export interface AppUpgradeContext {
  found: boolean
  /** 生效栈名(模板名或 模板名-实例名)。 */
  stackName: string
  /** 非 secret 参数现值(升级表单预填用)。 */
  params: Record<string, string>
  /** 有现值的 secret 参数名(不回值;升级时留空 = 保持不变,绝不重新生成)。 */
  secrets: string[]
  /** 原部署的资源限制(升级沿用;本次显式给非空值才覆盖)。 */
  cpus: string
  memory: string
}

export const getAppUpgradeContext = (
  serverId: string,
  templateId: string,
  instance?: string,
): Promise<AppUpgradeContext> =>
  http.get(
    `/api/servers/${encodeURIComponent(serverId)}/apps/upgrade-context?templateId=${encodeURIComponent(templateId)}` +
      (instance?.trim() ? `&instance=${encodeURIComponent(instance.trim())}` : ''),
  )

/**
 * 原位升级已部署实例:重渲染最新模板 + 重走 Stacks 部署链路(compose up -d 只重建有变化的
 * 服务,数据卷按项目名保持挂载)。显式给的非空参数覆盖现值,缺省/留空 = 保持不变
 * (secret 留空不会被重新生成,既有口令不动)。
 */
export const upgradeApp = (
  serverId: string,
  templateId: string,
  params: Record<string, string>,
  limits?: AppDeployLimits,
  instance?: string,
): Promise<AppDeployResult> =>
  http.post(`/api/servers/${encodeURIComponent(serverId)}/apps/upgrade`, {
    templateId,
    params,
    ...(instance?.trim() ? { instance: instance.trim() } : {}),
    ...(limits?.cpus?.trim() ? { cpus: limits.cpus.trim() } : {}),
    ...(limits?.memory?.trim() ? { memory: limits.memory.trim() } : {}),
  })

/** 已部署实例:某台机器上带参数边车的栈 = 应用商店部署的实例(「升级」直达入口的数据)。 */
export interface AppInstance {
  serverId: string
  serverName: string
  /** false = 该机器不可达(单条占位,无栈信息)。 */
  reachable: boolean
  error?: string
  stackName: string
  templateId: string
  templateName?: string
  displayName?: string
  icon?: string
  /** 实例名后缀(栈名去掉「模板名-」前缀;单实例部署 → 后端 omitempty 省略 = undefined)。 */
  instance?: string
  cpus?: string
  memory?: string
  /** RFC3339 部署/最近升级时间;旧边车无记录 → 空。 */
  deployedAt?: string
  /** 边车模板 ID 已不在模板库(不可升级)。 */
  templateGone?: boolean
  /** 模板在部署之后又改过 = 有可升级内容。 */
  upgradeAvailable?: boolean
  /** 旧版部署无边车:不可原位升级,走「重新部署」纳入管理(口令会重新生成,数据卷保留)。 */
  legacy?: boolean
}

export const listAppInstances = (): Promise<{ items: AppInstance[] }> =>
  http.get('/api/servers/apps/instances')
