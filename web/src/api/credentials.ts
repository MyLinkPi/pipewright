/**
 * Credentials API — aligns to frozen 1.3 contract.
 *
 * GET    /api/credentials            → Credential[]
 * POST   /api/credentials            → Credential  (needs CSRF)
 * PATCH  /api/credentials/:id        → Credential  (needs CSRF)
 * DELETE /api/credentials/:id        → 204          (needs CSRF)
 * POST   /api/credentials/:id/reveal → { secret }   (needs CSRF; audited; no UI caller)
 *
 * List/get never return plaintext — only maskedValue is exposed. The reveal endpoint
 * remains part of the contract (auth + CSRF + `credential_reveal` audit), but the UI
 * no longer surfaces plaintext, so no client wrapper is kept here.
 */

import { http } from './http'

export type CredentialType = 'git_token' | 'git_http' | 'ssh_key' | 'ssh_password' | 'registry' | 'sudo_password'

export interface Credential {
  id: string
  name: string
  type: CredentialType
  scope: string
  username: string
  /** Server-computed mask, e.g. "ghp_••••a91f" — never plaintext. */
  maskedValue: string
  lastUsedAt: string | null
  createdAt: string
}

export interface CreateCredentialInput {
  name: string
  type: CredentialType
  scope: string
  username?: string
  /** Plaintext secret — sent once on creation, never returned by the server. */
  secret: string
}

export interface UpdateCredentialInput {
  name?: string
  scope?: string
  username?: string
  /** Providing secret rotates the key. */
  secret?: string
}

export async function listCredentials(): Promise<Credential[]> {
  return http.get<Credential[]>('/api/credentials')
}

export async function createCredential(input: CreateCredentialInput): Promise<Credential> {
  return http.post<Credential>('/api/credentials', input)
}

export async function updateCredential(
  id: string,
  input: UpdateCredentialInput,
): Promise<Credential> {
  return http.patch<Credential>(`/api/credentials/${id}`, input)
}

export async function deleteCredential(id: string): Promise<void> {
  return http.delete<void>(`/api/credentials/${id}`)
}
