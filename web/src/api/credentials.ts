/**
 * Credentials API — aligns to frozen 1.3 contract.
 *
 * GET    /api/credentials            → Credential[]
 * POST   /api/credentials            → Credential  (needs CSRF)
 * PATCH  /api/credentials/:id        → Credential  (needs CSRF)
 * DELETE /api/credentials/:id        → 204          (needs CSRF)
 * POST   /api/credentials/:id/reveal → { secret }   (needs CSRF; audited; no UI caller)
 * POST   /api/credentials/ssh-keypair → { privateKey, publicKey } (needs CSRF; stateless, nothing persisted)
 *
 * List/get never return plaintext — only maskedValue is exposed. The reveal endpoint
 * remains part of the contract (auth + CSRF + `credential_reveal` audit), but the UI
 * no longer surfaces plaintext, so no client wrapper is kept here.
 */

import { http } from './http'

export type CredentialType = 'git_token' | 'git_http' | 'ssh_key' | 'ssh_password' | 'registry' | 'sudo_password' | 'object_store_secret'

/**
 * 由专属设置页全权管理的凭据类型,不应在通用凭据列表/选择器里露出。
 * dns_token 由「DNS 提供商」页创建、轮换、删除(保险库仅存密文 + credential_id 引用):
 * 在别处列出只会出现类型未知的「DNS · ××」条目,误删会让提供商凭据悬挂,误选会把
 * DNS Secret 泄进流水线环境变量。
 * object_store_secret(对象存储 AccessKeySecret)同理,由「对象存储」页全权管理。
 */
export const SYSTEM_MANAGED_CREDENTIAL_TYPES: readonly string[] = ['dns_token', 'object_store_secret']

/** 报告某凭据类型是否由专属设置页管理(通用界面应过滤)。 */
export function isSystemManagedType(type: CredentialType | string): boolean {
  return SYSTEM_MANAGED_CREDENTIAL_TYPES.includes(type)
}

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

/** SSH key algorithms the backend keypair generator accepts ("ed25519" is the default). */
export type SshKeyAlgorithm =
  | 'ed25519'
  | 'rsa2048'
  | 'rsa3072'
  | 'rsa4096'
  | 'ecdsa256'
  | 'ecdsa384'
  | 'ecdsa521'

export interface SshKeyPair {
  /** OpenSSH PEM private key (no passphrase) — fill into the secret field once, never stored by the server. */
  privateKey: string
  /** Single-line authorized_keys public key (e.g. "ssh-ed25519 AAAA…"). */
  publicKey: string
}

/**
 * Ask the server to generate a fresh SSH key pair (POST + CSRF; stateless crypto —
 * nothing is persisted, the private key is returned exactly once). The user deploys
 * the public key to the target host / Git platform, the private key goes into the
 * credential form and is sealed on create like any pasted key.
 */
export async function generateSshKeyPair(algorithm: SshKeyAlgorithm): Promise<SshKeyPair> {
  return http.post<SshKeyPair>('/api/credentials/ssh-keypair', { algorithm })
}

/**
 * Reveal the plaintext secret on explicit demand (POST + CSRF; audited server-side
 * as `credential_reveal`). The only endpoint that returns plaintext — use sparingly.
 */
export async function revealCredential(id: string): Promise<string> {
  const res = await http.post<{ secret: string }>(`/api/credentials/${id}/reveal`, {})
  return res.secret
}
