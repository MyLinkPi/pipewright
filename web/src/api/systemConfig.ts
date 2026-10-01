/**
 * System runtime config API client.
 *
 * 系统级运行时配置(取代需要重启的环境变量)。本期仅 publicUrl(平台对外访问地址):
 * 通知中的签名审批链接与 PR 状态回写的 target_url 以它为前缀;空 = 未配置(不生成链接)。
 * 修改即时生效,无需重启。
 *
 * GET /api/system/config → SystemConfig
 * PUT /api/system/config → SystemConfig   (CSRF;publicUrl 须为 http(s) origin,空 = 清除)
 */
import { http } from './http'

export interface SystemConfig {
  publicUrl: string
  updatedAt: string
}

export const getSystemConfig = (): Promise<SystemConfig> =>
  http.get('/api/system/config')

export const saveSystemConfig = (publicUrl: string): Promise<SystemConfig> =>
  http.put('/api/system/config', { publicUrl })
