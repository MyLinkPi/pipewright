/**
 * Certificate management (certmgmt) API client.
 *
 * 平台证书的统一生命周期:acme.sh(控制机本地脚本引擎)自动签发/续期(DNS-01,凭据复用 DNS 提供商)+
 * 手动导入;签发不依赖网关,签好的证书自动同步到被 SAN 覆盖的基域(nginx 网关随即
 * nginx -t + reload)。DTO 绝不含 PEM/密文。
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
 *
 * 证书部署目标(certdeploy;证书签发/导入/续期后自动联动重新部署):
 * GET    /api/cert-deploys?certId=           → { items: CertDeploy[] }
 * POST   /api/cert-deploys                   → CertDeploy         (CSRF)
 * PUT    /api/cert-deploys/{id}              → CertDeploy         (CSRF)
 * DELETE /api/cert-deploys/{id}              → { ok }             (CSRF)
 * POST   /api/cert-deploys/{id}/deploy       → CertDeploy         (CSRF;同步执行,失败也在行内 statusDetail)
 * GET    /api/cert-deploys/{id}/runs         → { items: CertDeployRun[] }
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

/** 签发引擎(控制机本地的 acme.sh 脚本集)状态。 */
export interface CertEngine {
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

/** 部署目标状态:空串 = 从未部署。 */
export type CertDeployStatus = '' | 'running' | 'success' | 'failed'

/** 一个证书部署目标(证书 → 指定服务器上的指定路径 + 部署后命令)。 */
export interface CertDeploy {
  id: string
  certId: string
  serverId: string
  name: string
  kind: 'ssh' | 'safeline' | ''
  wafInstanceId: string
  certName: string
  certPath: string
  keyPath: string
  postCommand: string
  enabled: boolean
  status: CertDeployStatus
  statusDetail: string
  lastDeployedAt: string
  createdAt: string
  updatedAt: string
  /** 冗余展示(list join 填充)。 */
  serverName: string
  certPrimaryDomain: string
}

/** 一次部署的历史记录(source: manual = 手动 / renew = 证书续期联动)。 */
export interface CertDeployRun {
  id: string
  source: 'manual' | 'renew'
  status: 'success' | 'failed'
  detail: string
  startedAt: string
  finishedAt: string
  durationMs: number
}

/** 创建部署目标入参(kind 留空 = ssh;safeline 时仅 certId/wafInstanceId/certName 必填)。 */
export interface CertDeployInput {
  certId: string
  kind?: 'ssh' | 'safeline'
  serverId?: string
  name?: string
  certPath?: string
  keyPath?: string
  postCommand?: string
  wafInstanceId?: string
  certName?: string
  enabled?: boolean
}

/** 更新部署目标入参(undefined = 不改;certId/serverId 不可改)。 */
export interface CertDeployUpdateInput {
  name?: string
  certPath?: string
  keyPath?: string
  postCommand?: string
  certName?: string
  enabled?: boolean
}

export const listCertDeploys = (certId?: string): Promise<{ items: CertDeploy[] }> =>
  http.get(`/api/cert-deploys${certId ? `?certId=${encodeURIComponent(certId)}` : ''}`)

export const createCertDeploy = (input: CertDeployInput): Promise<CertDeploy> =>
  http.post('/api/cert-deploys', input)

export const updateCertDeploy = (id: string, input: CertDeployUpdateInput): Promise<CertDeploy> =>
  http.put(`/api/cert-deploys/${encodeURIComponent(id)}`, input)

export const deleteCertDeploy = (id: string): Promise<{ ok: boolean }> =>
  http.delete(`/api/cert-deploys/${encodeURIComponent(id)}`)

/** 立即部署(同步):执行失败也返回行(status=failed + statusDetail 人话原因)。 */
export const deployCertDeployNow = (id: string): Promise<CertDeploy> =>
  http.post(`/api/cert-deploys/${encodeURIComponent(id)}/deploy`)

export const listCertDeployRuns = (id: string): Promise<{ items: CertDeployRun[] }> =>
  http.get(`/api/cert-deploys/${encodeURIComponent(id)}/runs`)
