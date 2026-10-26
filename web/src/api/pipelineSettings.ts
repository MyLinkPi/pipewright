/**
 * Pipeline build/deploy settings API — aligns to frozen 2.4 contract.
 *
 * GET /api/projects/{id}/pipeline/settings  → SettingsDTO  (lazily creates default on first access)
 * PUT /api/projects/{id}/pipeline/settings  → SettingsDTO  (needs CSRF)
 *
 * Independent of the 2.2 pipeline spec. Secret variables / registry credentials are
 * stored as a credentialId reference only — the server never returns plaintext, only
 * a server-computed maskedValue. On save, secret items send {key,secret:true,credentialId}
 * (no plaintext / mask).
 */

import { http } from './http'

// ─── Enums (frozen) ──────────────────────────────────────────────────────────

export type BuildModel = 'dockerfile' | 'toolchain'
export type ArtifactType = 'image' | 'jar' | 'dist'

// ─── Domain types (frozen DTO shape) ─────────────────────────────────────────

export interface BuildVar {
  id: string
  key: string
  secret: boolean
  /** Present only for non-secret vars. */
  value?: string
  /** Present only for secret vars — vault reference. */
  credentialId?: string
  /** Server-computed mask for secret vars, e.g. "••••_zzz" — never plaintext. */
  maskedValue?: string
}

export interface Toolchain {
  language: string
  version: string
}

export interface Cache {
  enabled: boolean
  paths: string[]
}

/** One variable declared in the repo YAML top-level `variables:` block (read-only metadata). */
export interface PacVarDef {
  name: string
  description: string
  required: boolean
  secret: boolean
  default: string
}

/**
 * Pipeline-as-code variables: declarations synced from the repo YAML (defs — read-only here,
 * maintained by the preview pull & validate) plus the values filled in on this tab.
 * Secret values follow the same iron rule as build vars: credentialId reference + server mask.
 */
export interface PacVarsConfig {
  defs: PacVarDef[]
  values: BuildVar[]
  ref?: string
  file?: string
  syncedAt?: string
}

export interface BuildConfig {
  model: BuildModel
  dockerfilePath: string
  toolchain: Toolchain
  artifactType: ArtifactType
  vars: BuildVar[]
  cache: Cache
}

export interface SettingsDTO {
  build: BuildConfig
  /** Pipeline-as-code variable declarations + filled values (absent when never synced). */
  pacVars?: PacVarsConfig
  updatedAt: string
}

// ─── Save request types (secret items send only credentialId; no plaintext/mask) ──

export interface SaveBuildVar {
  id?: string
  key: string
  secret: boolean
  value?: string
  credentialId?: string
}

export interface SaveSettingsInput {
  build: {
    model: BuildModel
    dockerfilePath?: string
    toolchain?: Toolchain
    artifactType: ArtifactType
    vars: SaveBuildVar[]
    cache: Cache
  }
  /**
   * Pipeline-as-code variable values (defs are repo-owned, never sent). Omit to keep the
   * current values (older clients / AI applies don't wipe them); send to replace.
   */
  pacVars?: {
    values: SaveBuildVar[]
  }
}

// ─── API functions ────────────────────────────────────────────────────────────

export async function getSettings(projectId: string): Promise<SettingsDTO> {
  return http.get<SettingsDTO>(`/api/projects/${projectId}/pipeline/settings`)
}

export async function saveSettings(
  projectId: string,
  input: SaveSettingsInput,
): Promise<SettingsDTO> {
  return http.put<SettingsDTO>(`/api/projects/${projectId}/pipeline/settings`, input)
}
