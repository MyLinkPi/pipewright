/**
 * Triggers API — aligns to frozen 2.3 contract.
 *
 * GET  /api/projects/{id}/trigger                  → TriggerConfig
 * PUT  /api/projects/{id}/trigger                  → TriggerConfig  (needs CSRF)
 * POST /api/projects/{id}/trigger/secret/reset     → SecretResetResult (needs CSRF)
 * GET  /api/projects/{id}/trigger/webhook-endpoints → WebhookEndpoints (mesh peers)
 *
 * webhookSecretMasked is always masked (e.g. "whsec_••••a91f").
 * The full plaintext secret is ONLY available in SecretResetResult.webhookSecret,
 * returned once at reset time and never again.*/

import { http } from './http'

// ─── Domain types ─────────────────────────────────────────────────────────────

export interface TriggerEvents {
  push: boolean
  tag: boolean
  pullRequest: boolean
  release: boolean
}

export interface TriggerConfig {
  /** Read-only webhook endpoint URL, e.g. "/api/webhooks/<token>". */
  webhookUrl: string
  /**
   * Masked display, e.g. "whsec_••••a91f".
   * Never contains plaintext.  Copy the full secret from SecretResetResult only.
   */
  webhookSecretMasked: string
  events: TriggerEvents
  /** 分支过滤 glob 白名单;空 = 所有分支触发。 */
  branchFilters: string[]
  /** 路径过滤 glob 列表(monorepo · P0);空 = 不启用(放行一切)。 */
  pathFilters: string[]
}

export interface SaveTriggerInput {
  events: TriggerEvents
  branchFilters: string[]
  pathFilters: string[]
}

export interface SecretResetResult {
  /** Full plaintext secret — one-time only, never stored client-side after display. */
  webhookSecret: string
  webhookSecretMasked: string
}

/** 可选接收环境地址(mesh 对端;仅登记了对外地址 publicUrl 的环境)。 */
export interface WebhookEndpoint {
  nodeId: string
  name: string
  /** 该环境 Web 基址,末尾不带斜杠,如 "https://owner.example.com"。 */
  base: string
}

export interface WebhookEndpoints {
  items: WebhookEndpoint[]
}

// ─── API functions ────────────────────────────────────────────────────────────

export async function getTrigger(projectId: string): Promise<TriggerConfig> {
  return http.get<TriggerConfig>(`/api/projects/${projectId}/trigger`)
}

export async function saveTrigger(
  projectId: string,
  input: SaveTriggerInput,
): Promise<TriggerConfig> {
  return http.put<TriggerConfig>(`/api/projects/${projectId}/trigger`, input)
}

export async function resetSecret(projectId: string): Promise<SecretResetResult> {
  return http.post<SecretResetResult>(`/api/projects/${projectId}/trigger/secret/reset`)
}

/**
 * 集群启用时返回可作 webhook 接收端点的其他环境地址(供 Git 平台够不到本环境时,
 * 把端点指到任一可达环境,集群会把投递转发到 token 所属环境)。
 */
export async function getWebhookEndpoints(projectId: string): Promise<WebhookEndpoints> {
  return http.get<WebhookEndpoints>(`/api/projects/${projectId}/trigger/webhook-endpoints`)
}
