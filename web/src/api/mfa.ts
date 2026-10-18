/**
 * MFA(两步验证)API client。
 *
 * GET    /api/account/mfa                        → MFAStatus(状态 + 通行密钥列表)
 * POST   /api/account/mfa/totp/setup             → TOTPSetup(密钥/otpauth/备份码,明文仅此一次)
 * POST   /api/account/mfa/totp/enable            → 204(验证 6 位码启用)
 * POST   /api/account/mfa/totp/disable           → 204(验证码或备份码关闭)
 * POST   /api/account/mfa/totp/backup-codes      → { backupCodes }(重新生成)
 * POST   /api/account/mfa/passkeys/register/begin → { options }(浏览器原生创建选项)
 * POST   /api/account/mfa/passkeys/register/finish → PasskeyInfo(提交 PublicKeyCredential 原生 JSON)
 * DELETE /api/account/mfa/passkeys/{id}           → 204
 * GET    /api/auth/options                        → { passkey_login }(公开;登录页按钮显隐)
 * POST   /api/auth/login/mfa                      → { username, role }(登录第二步)
 * POST   /api/auth/passkey/login/{begin,finish}   → Passkey 登录(公开)
 */
import { http } from './http'
import type { SessionUser } from './auth'

export interface PasskeyInfo {
  id: number
  name: string
  createdAt: string
  lastUsedAt: string | null
}

export interface MFAStatus {
  totpEnabled: boolean
  totpSetupPending: boolean
  backupCodesRemaining: number
  passkeys: PasskeyInfo[]
}

export interface TOTPSetup {
  secret: string
  otpauthUrl: string
  backupCodes: string[]
}

export const fetchMFAStatus = (): Promise<MFAStatus> => http.get('/api/account/mfa')

export const setupTOTP = (): Promise<TOTPSetup> => http.post('/api/account/mfa/totp/setup')

export const enableTOTP = (code: string): Promise<void> =>
  http.post('/api/account/mfa/totp/enable', { code })

export const disableTOTP = (code: string): Promise<void> =>
  http.post('/api/account/mfa/totp/disable', { code })

export const regenerateBackupCodes = (code: string): Promise<{ backupCodes: string[] }> =>
  http.post('/api/account/mfa/totp/backup-codes', { code })

export const beginPasskeyRegistration = (name: string): Promise<{ options: unknown }> =>
  http.post('/api/account/mfa/passkeys/register/begin', { name })

export const finishPasskeyRegistration = (
  credential: Record<string, unknown>,
): Promise<PasskeyInfo> =>
  http.post('/api/account/mfa/passkeys/register/finish', credential)

export const deletePasskey = (id: number): Promise<void> =>
  http.delete(`/api/account/mfa/passkeys/${id}`)

/** 登录页能力探测(公开):是否展示「使用通行密钥登录」。 */
export const fetchAuthOptions = (): Promise<{ passkeyLogin: boolean }> =>
  http.get('/api/auth/options')

/** 登录第二步:提交 TOTP 验证码或备份码(pending 会话 cookie 已就位)。 */
export const loginMFA = (code: string): Promise<SessionUser> =>
  http.post('/api/auth/login/mfa', { code })

/** Passkey 登录:begin → 浏览器断言 → finish(成功即返回会话身份)。 */
export const beginPasskeyLogin = (): Promise<{ options: unknown }> =>
  http.post('/api/auth/passkey/login/begin')

export const finishPasskeyLogin = (credential: Record<string, unknown>): Promise<SessionUser> =>
  http.post('/api/auth/passkey/login/finish', credential)

/** 管理员:清除成员 TOTP / 全部通行密钥(锁死恢复通道)。 */
export const adminResetTOTP = (userId: string): Promise<void> =>
  http.delete(`/api/users/${encodeURIComponent(userId)}/mfa/totp`)

export const adminResetPasskeys = (userId: string): Promise<{ removed: number }> =>
  http.delete(`/api/users/${encodeURIComponent(userId)}/mfa/passkeys`)
