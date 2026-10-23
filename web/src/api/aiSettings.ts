/**
 * AI Settings API — aligns to frozen 7.1 contract.
 *
 * GET  /api/settings/ai          → AISettings
 * PUT  /api/settings/ai          → AISettings  (needs CSRF)
 * POST /api/settings/ai/test     → AITestResult (needs CSRF)
 *
 * apiKey is WRITE-ONLY: the server never returns plaintext.
 * GET/PUT responses only include apiKeyMasked (e.g. "sk-ant-••••a91f").
 * Ollama does not require an apiKey.
 */

import { http } from './http'

export type AIProvider = 'claude' | 'openai' | ''

export interface AIBudget {
  monthlyTokenLimit: number | null
}

/**
 * Context-management declaration for the ops agent. 0 / null = use server
 * defaults (window 200k tokens, auto-compact on, keep-recent 15k, …).
 */
export interface AIContext {
  windowTokens: number
  maxOutputTokens: number
  historyMessages: number
  historyBytes: number
  autoCompact: boolean | null
  keepRecentTokens: number
  bufferTokens: number
}

/** Web search/fetch tools provider ('' = implicit DeepSeek, kept for legacy rows). */
export type AIWebProvider = 'deepseek' | 'tavily' | 'bocha' | 'ollama' | 'glm' | ''

/**
 * Web search/fetch tools config. provider '' falls back to the LLM config
 * (usable only when the LLM destination is official DeepSeek). available/reason
 * reflect the resolved destination while the toggle is on.
 */
export interface AIWebSettings {
  enabled: boolean
  provider: AIWebProvider
  baseUrl: string
  /** Server-computed mask, e.g. "••••a91f" — never plaintext. */
  apiKeyMasked: string
  available: boolean
  reason: string
}

/** GET /api/settings/ai response — never contains plaintext apiKey */
export interface AISettings {
  configured: boolean
  enabled: boolean
  provider: AIProvider
  baseUrl: string
  model: string
  /** Server-computed mask, e.g. "sk-ant-••••a91f" — never plaintext. */
  apiKeyMasked: string
  budget: AIBudget
  context: AIContext
  web: AIWebSettings
  updatedAt: string | null
}

/** PUT /api/settings/ai request body */
export interface SaveAISettingsInput {
  provider: AIProvider
  baseUrl: string
  model: string
  /**
   * Write-only: omit or leave empty to keep existing key unchanged.
   * Non-empty rotates to the new key.
   */
  apiKey?: string
  budget: AIBudget
  context: AIContext
  enabled: boolean
  /** Web search/fetch tools sub-config; apiKey has the same write-only semantics. */
  web: {
    enabled: boolean
    provider: AIWebProvider
    baseUrl: string
    apiKey?: string
  }
}

/** POST /api/settings/ai/test response */
export interface AITestResult {
  ok: boolean
  latencyMs: number
  detail: string
  error: string | null
}

/** POST /api/settings/ai/test request body (all optional — falls back to saved config) */
export interface TestAIConnectionInput {
  provider?: AIProvider
  baseUrl?: string
  model?: string
  /** Write-only draft key for testing before saving. */
  apiKey?: string
}

export async function getAISettings(): Promise<AISettings> {
  return http.get<AISettings>('/api/settings/ai')
}

export async function saveAISettings(
  input: SaveAISettingsInput,
): Promise<AISettings> {
  return http.put<AISettings>('/api/settings/ai', input)
}

export async function testAIConnection(
  draft?: TestAIConnectionInput,
): Promise<AITestResult> {
  return http.post<AITestResult>('/api/settings/ai/test', draft ?? {})
}

/** POST /api/settings/ai/web/test request body (all optional — falls back to saved web config) */
export interface TestWebToolsInput {
  provider?: AIWebProvider
  baseUrl?: string
  /** Write-only draft key for testing before saving. */
  apiKey?: string
}

/**
 * Fires one real count=1 search against the resolved web tools destination
 * (billed only on success, per DeepSeek pricing).
 */
export async function testWebTools(
  draft?: TestWebToolsInput,
): Promise<AITestResult> {
  return http.post<AITestResult>('/api/settings/ai/web/test', draft ?? {})
}

export interface AIModelsResult {
  ok: boolean
  items: { id: string; name: string }[]
  error?: string
}
export function listAIModels(
  draft: TestAIConnectionInput,
  signal?: AbortSignal,
): Promise<AIModelsResult> {
  return http.post('/api/settings/ai/models', draft, { signal })
}
