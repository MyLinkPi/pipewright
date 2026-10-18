/**
 * System runtime config API client.
 *
 * 系统级运行时配置(取代需要重启的环境变量)。当前字段:
 *   - publicUrl(平台对外访问地址):通知中的签名审批链接与 PR 状态回写的 target_url
 *     以它为前缀;空 = 未配置(不生成链接)。Passkey RP 配置留空时也以它为推导来源。
 *   - releaseMirror(自升级镜像源):检查更新与二进制下载所用 GitHub 路径兼容镜像的 base;
 *     空 = 未配置(回退 GitHub 官方源)。
 *   - webauthnRpId / webauthnOrigins / passkeyLoginEnabled:Passkey(通行密钥)登录的
 *     依赖方配置;留空 = 自动推导(public_url → 请求 Host),开关默认开。
 * 修改即时生效,无需重启。
 *
 * GET /api/system/config → SystemConfig
 * PUT /api/system/config → SystemConfig   (CSRF;字段按需更新:undefined = 不动该项,空串 = 清除)
 */
import { http } from './http'

export interface SystemConfig {
  publicUrl: string
  releaseMirror: string
  /** Passkey / 两步验证 RP 配置(留空 = 自动推导)。origins 为逗号分隔列表。 */
  webauthnRpId: string
  webauthnOrigins: string
  passkeyLoginEnabled: boolean
  updatedAt: string
  /** 实际生效的升级源(库内配置 > 环境变量 > 默认 GitHub Releases),让默认值透明可见。 */
  effectiveSource: {
    /** config(库内配置) | env(环境变量 PIPEWRIGHT_RELEASE_MIRROR) | default(GitHub 官方) */
    origin: 'config' | 'env' | 'default'
    apiBase: string
    dlBase: string
  }
}

export const getSystemConfig = (): Promise<SystemConfig> =>
  http.get('/api/system/config')

export interface SaveSystemConfigInput {
  publicUrl?: string
  releaseMirror?: string
  webauthnRpId?: string
  webauthnOrigins?: string
  passkeyLoginEnabled?: boolean
}

export const saveSystemConfig = (input: SaveSystemConfigInput): Promise<SystemConfig> => {
  const body: Record<string, unknown> = {}
  if (input.publicUrl !== undefined) body.publicUrl = input.publicUrl
  if (input.releaseMirror !== undefined) body.releaseMirror = input.releaseMirror
  if (input.webauthnRpId !== undefined) body.webauthnRpId = input.webauthnRpId
  if (input.webauthnOrigins !== undefined) body.webauthnOrigins = input.webauthnOrigins
  if (input.passkeyLoginEnabled !== undefined) body.passkeyLoginEnabled = input.passkeyLoginEnabled
  return http.put('/api/system/config', body)
}
