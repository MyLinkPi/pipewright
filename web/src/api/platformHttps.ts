/**
 * Platform HTTPS (host nginx) API client.
 *
 * 平台自身 Web 页面的 HTTPS 访问:当 nginx 所在机器(通常即平台所在主机,登记为目标服务器)
 * 装有宿主 nginx 时,自动下发证书 + 写 conf.d vhost(443 ssl 反代平台 Web 端口 + 80→443 跳转)
 * + nginx -t + reload。证书复用证书管理模块(certId 软引用,续期后自动重新下发);DTO 绝不含 PEM。
 *
 * GET  /api/platform-https/settings           → PlatformHttpsSettings
 * PUT  /api/platform-https/settings           → { settings, applyError? }   (CSRF;apply=true 保存后立即应用)
 * GET  /api/platform-https/detect?serverId=   → PlatformHttpsDetect
 * POST /api/platform-https/apply              → PlatformHttpsSettings       (CSRF;重新应用/收敛)
 * POST /api/platform-https/disable            → PlatformHttpsSettings       (CSRF;移除远端配置并复位)
 */
import { http } from './http'
import type { Cert } from './certMgmt'

/** 平台 HTTPS 设置(单行;status ''=从未应用 | active | failed)。 */
export interface PlatformHttpsSettings {
  enabled: boolean
  serverId: string
  domain: string
  certId: string
  upstreamHost: string
  upstreamPort: number
  effectiveUpstream: string
  httpRedirect: boolean
  status: '' | 'active' | 'failed'
  statusDetail: string
  lastAppliedAt: string
  updatedAt: string
}

/** 宿主 nginx 探测结果。 */
export interface PlatformHttpsDetect {
  serverId: string
  installed: boolean
  version: string
  isRoot: boolean
  sudoOk: boolean
  /** 服务器绑定了 sudo 密码凭据(root / 免密已可用时不探测密码)。 */
  sudoPwdConfigured: boolean
  /** sudo -S 密码验证通过(仅非 root 且无免密 sudo 时探测)。 */
  sudoPwdOk: boolean
  confDIncluded: boolean
  managedConf: boolean
}

/** 保存入参(与后端 SettingsInput 对齐;未传字段保持不变)。 */
export interface PlatformHttpsSaveInput {
  enabled?: boolean
  serverId?: string
  domain?: string
  certId?: string
  upstreamHost?: string
  upstreamPort?: number
  httpRedirect?: boolean
  /** 保存成功且启用时立即应用(应用失败仍 200,回填 status=failed + applyError)。 */
  apply?: boolean
}

export interface PlatformHttpsSaveResult {
  settings: PlatformHttpsSettings
  applyError?: string
}

export const getPlatformHttpsSettings = (): Promise<PlatformHttpsSettings> =>
  http.get('/api/platform-https/settings')

export const savePlatformHttpsSettings = (input: PlatformHttpsSaveInput): Promise<PlatformHttpsSaveResult> =>
  http.put('/api/platform-https/settings', input)

export const detectPlatformHttps = (serverId: string): Promise<PlatformHttpsDetect> =>
  http.get(`/api/platform-https/detect?serverId=${encodeURIComponent(serverId)}`)

export const applyPlatformHttps = (): Promise<PlatformHttpsSettings> =>
  http.post('/api/platform-https/apply')

export const disablePlatformHttps = (): Promise<PlatformHttpsSettings> =>
  http.post('/api/platform-https/disable')

/**
 * 证书是否覆盖访问域名(前端下拉过滤用,与服务端 domainCoveredBy 同规则):
 * 精确等于某 SAN,或被某通配符 SAN 覆盖(子域带 "." 分隔,防 evilefg.com 误匹配 *.efg.com)。
 */
export function certCoversDomain(cert: Pick<Cert, 'domains'>, domain: string): boolean {
  const d = domain.trim().toLowerCase()
  if (!d) return false
  return cert.domains.some((san) => {
    if (san === d) return true
    if (san.startsWith('*.')) {
      const base = san.slice(2)
      return d.endsWith('.' + base)
    }
    return false
  })
}
