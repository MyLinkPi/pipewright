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
  name: string
  ok: boolean
  output: string
  error: string
  /** 生效参数(含自动生成的 secret;仅本次部署响应一次性返回)。 */
  params?: Record<string, string>
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
): Promise<AppDeployResult> =>
  http.post(`/api/servers/${encodeURIComponent(serverId)}/apps/deploy`, {
    templateId,
    params,
    ...(limits?.cpus?.trim() ? { cpus: limits.cpus.trim() } : {}),
    ...(limits?.memory?.trim() ? { memory: limits.memory.trim() } : {}),
  })
