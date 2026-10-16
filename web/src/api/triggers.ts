/**
 * Triggers API — aligns to frozen 2.3 contract.
 *
 * GET  /api/projects/{id}/trigger                  → TriggerConfig
 * PUT  /api/projects/{id}/trigger                  → TriggerConfig  (needs CSRF)
 * POST /api/projects/{id}/trigger/secret/reset     → SecretResetResult (needs CSRF)
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
