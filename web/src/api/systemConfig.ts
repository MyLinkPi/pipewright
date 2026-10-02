/**
 * System runtime config API client.
 *
 * 系统级运行时配置(取代需要重启的环境变量)。当前字段:
 *   - publicUrl(平台对外访问地址):通知中的签名审批链接与 PR 状态回写的 target_url
 *     以它为前缀;空 = 未配置(不生成链接)。
 *   - releaseMirror(自升级镜像源):检查更新与二进制下载的 GitHub 路径兼容镜像 base;
 *     空 = 未配置(回退 GitHub 官方源)。
 * 修改即时生效,无需重启。
 *
 * GET /api/system/config → SystemConfig
 * PUT /api/system/config → SystemConfig   (CSRF;字段按需更新:undefined = 不动该项,空串 = 清除)
 */
import { http } from './http'

export interface SystemConfig {
  publicUrl: string
  releaseMirror: string
  updatedAt: string
}

export const getSystemConfig = (): Promise<SystemConfig> =>
  http.get('/api/system/config')

export const saveSystemConfig = (
  publicUrl: string | undefined,
  releaseMirror?: string,
): Promise<SystemConfig> => {
  const body: Record<string, unknown> = {}
  if (publicUrl !== undefined) body.publicUrl = publicUrl
  if (releaseMirror !== undefined) body.releaseMirror = releaseMirror
  return http.put('/api/system/config', body)
}
