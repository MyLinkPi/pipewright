/**
 * Certificate management (certmgmt) API client.
 *
 * 平台证书的统一生命周期:acme.sh 自动签发/续期(DNS-01,凭据复用 DNS 提供商集成)+
 * 手动导入;签发引擎(acme.sh 宿主机脚本)跑在服务注册网关主机上,签好的证书自动同步到被
 * SAN 覆盖的基域(nginx 网关随即 nginx -t + reload)。DTO 绝不含 PEM/密文。
 *
 * GET    /api/certmgmt/certs                 → { items: Cert[] }
 * POST   /api/certmgmt/certs                 → Cert               (CSRF;201,异步签发,返回 pending 行)
 * POST   /api/certmgmt/certs/import          → Cert               (CSRF;201,同步导入)
 * GET    /api/certmgmt/certs/{id}            → Cert
 * POST   /api/certmgmt/certs/{id}/renew      → Cert               (CSRF;acme 异步重签 / manual 同步重新下发)
 * POST   /api/certmgmt/certs/{id}/auto-renew → Cert               (CSRF)
 * DELETE /api/certmgmt/certs/{id}            → { ok }             (CSRF)
 * GET    /api/certmgmt/engine                → CertEngine
 * POST   /api/certmgmt/engine/deploy         → CertEngine         (CSRF)
 */
import { http } from './http'

export type CertSource = 'acme' | 'manual'
export type CertValidation = 'dns' | 'manual'
export type CertStatus = 'pending' | 'issued' | 'failed'
export type CertCA = 'letsencrypt' | 'zerossl' | 'buypass'
export type CertKeyType = 'ec-256' | 'ec-384' | 'rsa-2048'

/** 一张证书(时间均为 RFC3339;空串 = 未知)。 */
export interface Cert {
  id: string
  primaryDomain: string
  domains: string[]
  source: CertSource
  ca: CertCA | ''
  validation: CertValidation
  dnsProviderId: string
  keyType: CertKeyType
  autoRenew: boolean
  status: CertStatus
  statusDetail: string
  subject: string
  issuer: string
  notBefore: string
  notAfter: string
  lastIssuedAt: string
  createdAt: string
  updatedAt: string
}

/** 签发引擎(网关主机上的 acme.sh 脚本集)状态。 */
export interface CertEngine {
  configured: boolean
  serverId: string
  serverName: string
  installed: boolean
  ready: boolean
  version: string
}

/** 创建 ACME 证书入参(DNS-01:主域 + 附加 SAN + DNS 提供商)。 */
export interface CertCreateInput {
  primaryDomain: string
  domains?: string[]
  dnsProviderId: string
  ca?: CertCA | ''
  keyType?: CertKeyType
  autoRenew?: boolean
}

/** 手动导入入参(PEM 全文;cert/key 须配对)。 */
export interface CertImportInput {
  certPem: string
  keyPem: string
}

export const listCerts = (): Promise<{ items: Cert[] }> =>
  http.get('/api/certmgmt/certs')

export const getCert = (id: string): Promise<Cert> =>
  http.get(`/api/certmgmt/certs/${encodeURIComponent(id)}`)

export const createCert = (input: CertCreateInput): Promise<Cert> =>
  http.post('/api/certmgmt/certs', input)

export const importCert = (input: CertImportInput): Promise<Cert> =>
  http.post('/api/certmgmt/certs/import', input)

export const renewCert = (id: string): Promise<Cert> =>
  http.post(`/api/certmgmt/certs/${encodeURIComponent(id)}/renew`)

export const deleteCert = (id: string): Promise<{ ok: boolean }> =>
  http.delete(`/api/certmgmt/certs/${encodeURIComponent(id)}`)

export const setCertAutoRenew = (id: string, enabled: boolean): Promise<Cert> =>
  http.post(`/api/certmgmt/certs/${encodeURIComponent(id)}/auto-renew`, { enabled })

export const getCertEngine = (): Promise<CertEngine> =>
  http.get('/api/certmgmt/engine')

export const deployCertEngine = (): Promise<CertEngine> =>
  http.post('/api/certmgmt/engine/deploy')
