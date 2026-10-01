/**
 * DNS providers API client (R3 / E3.1 — 零 DNS 体验).
 *
 * A `DnsProvider` is a provider **account** (Cloudflare / DNSPod / 阿里云 DNS)
 * managing **one or more zones** (base domains). Credentials are split by
 * sensitivity:
 *  - `apiId` (DNSPod SecretId / 阿里云 AccessKeyId) is non-secret — stored in
 *    plain sight and echoed back / editable. Cloudflare has none (single token).
 *  - the Secret (API token / key) is **write-only** — supplied once (rotatable),
 *    stored via the backend vault, and **never** returned. The DTO only reflects
 *    whether a credential is configured (`credentialConfigured`).
 *
 * Providers unlock two zero-DNS capabilities:
 *  - DNS-01 ACME on a route (wildcard certs `*.example.com`), and
 *  - instant subdomain allocation (`app-xxxx.<zone>` + auto A record + route).
 *
 * GET    /api/dns/providers                       → { items: DnsProvider[] }
 * POST   /api/dns/providers                       → DnsProvider        (needs CSRF)
 * PUT    /api/dns/providers/{id}                  → DnsProvider        (needs CSRF)
 * DELETE /api/dns/providers/{id}                  → { ok: true }       (needs CSRF)
 * POST   /api/dns/providers/{id}/verify           → VerifyResult       (needs CSRF)
 * POST   /api/dns/providers/{id}/zones            → DnsZone            (needs CSRF)
 * DELETE /api/dns/providers/{id}/zones/{zoneId}   → { ok: true }       (needs CSRF)
 *
 * Writes go through the shared `http` wrapper (auto CSRF + locale). Never returns
 * a Secret.
 */

import { http } from './http'

/** Supported managed-DNS backends for DNS-01 + instant subdomains. */
export type DnsProviderType = 'cloudflare' | 'dnspod' | 'alidns'

/** One managed zone (base domain) under a provider account. */
export interface DnsZone {
  id: string
  /** Apex domain, e.g. `example.com`. Subdomains live under it. */
  baseDomain: string
  createdAt: string
}

/**
 * A provider account managing one or more zones. The Secret is never returned;
 * `apiId` is non-secret and echoed back.
 */
export interface DnsProvider {
  id: string
  type: DnsProviderType
  /** Human-readable label, e.g. "生产区 Cloudflare". */
  name: string
  /** Non-secret credential half: DNSPod SecretId / 阿里云 AccessKeyId; empty for Cloudflare. */
  apiId: string
  /** Zones (base domains) managed by this account — one account, many zones. */
  zones: DnsZone[]
  /** Server-derived: whether a Secret is stored in the vault. Never the Secret itself. */
  credentialConfigured: boolean
  createdAt: string
}

/** Body for creating a provider. `secret` is write-only — sent once, never echoed. */
export interface CreateDnsProviderInput {
  type: DnsProviderType
  name: string
  /** Non-secret credential half (DNSPod SecretId / 阿里云 AccessKeyId). Empty for Cloudflare. */
  apiId: string
  /** Provider API Secret (write-only). Stored via the vault; never returned. */
  secret: string
  /** One or more zones this account manages (≥1 required). */
  baseDomains: string[]
}

/** Body for editing a provider. Omitted fields stay unchanged; `secret` rotates when set. */
export interface UpdateDnsProviderInput {
  name?: string
  apiId?: string
  /** Optional Secret rotation (write-only). */
  secret?: string
}

/** Per-zone verify outcome. */
export interface ZoneVerifyResult {
  id: string
  baseDomain: string
  ok: boolean
  /** Human-readable failure reason (empty on success). */
  error?: string
}

/** Result of a verify call: every zone probed, with an overall ok. */
export interface VerifyDnsProviderResult {
  ok: boolean
  zones: ZoneVerifyResult[]
}

/** List all configured DNS providers. Secrets are never included. */
export async function listDnsProviders(): Promise<DnsProvider[]> {
  const res = await http.get<{ items: DnsProvider[] }>('/api/dns/providers')
  return res.items ?? []
}

/** Add a DNS provider (Secret stored write-only via the vault). Returns the created DTO. */
export async function createDnsProvider(input: CreateDnsProviderInput): Promise<DnsProvider> {
  return http.post<DnsProvider>('/api/dns/providers', input)
}

/** Edit a provider (name / apiId; optional Secret rotation without re-creating). */
export async function updateDnsProvider(id: string, input: UpdateDnsProviderInput): Promise<DnsProvider> {
  return http.put<DnsProvider>(`/api/dns/providers/${encodeURIComponent(id)}`, input)
}

/** Delete a DNS provider (zones removed with it). Routes pinned to it lose DNS-01 until re-attached. */
export async function deleteDnsProvider(id: string): Promise<{ ok: boolean }> {
  return http.delete<{ ok: boolean }>(`/api/dns/providers/${encodeURIComponent(id)}`)
}

/** Verify a provider's credentials reach every managed zone. Returns per-zone results. */
export async function verifyDnsProvider(id: string): Promise<VerifyDnsProviderResult> {
  return http.post<VerifyDnsProviderResult>(`/api/dns/providers/${encodeURIComponent(id)}/verify`, {})
}

/** Add a zone (base domain) to a provider. */
export async function addDnsZone(providerId: string, baseDomain: string): Promise<DnsZone> {
  return http.post<DnsZone>(`/api/dns/providers/${encodeURIComponent(providerId)}/zones`, { baseDomain })
}

/** Remove a zone from a provider. */
export async function removeDnsZone(providerId: string, zoneId: string): Promise<{ ok: boolean }> {
  return http.delete<{ ok: boolean }>(
    `/api/dns/providers/${encodeURIComponent(providerId)}/zones/${encodeURIComponent(zoneId)}`,
  )
}
