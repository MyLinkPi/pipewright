/**
 * Auth API — aligns to frozen 1.2 contract (+ 用户体系扩展).
 *
 * GET  /api/auth/session            → { username, role } | 401
 * POST /api/auth/login              → { username, role } | 401 | 429
 * POST /api/auth/logout             → 204
 * POST /api/auth/register           → { username, role } | 404/409/410/422  (公开;邀请注册)
 * GET  /api/auth/invitations/{token} → { state } | 404                    (公开;邀请状态)
 */

import { http, HttpError } from './http'
import type { UserRole } from '../stores/session'

export interface SessionUser {
  username: string
  role: UserRole
}

/** 邀请状态:待用 / 已用 / 已过期。 */
export type InvitationState = 'pending' | 'used' | 'expired'

export interface InvitationInfo {
  state: InvitationState
}

/**
 * Fetch the current session from the server.
 *
 * Returns:
 *   - SessionUser   — authenticated
 *   - null          — confirmed 401 (not logged in)
 *
 * Throws HttpError (status ≠ 401) or network Error for 5xx / network faults,
 * so callers can distinguish "not logged in" from "backend down".
 */
export async function fetchSession(): Promise<SessionUser | null> {
  try {
    return await http.get<SessionUser>('/api/auth/session')
  } catch (err) {
    if (err instanceof HttpError && err.status === 401) {
      return null
    }
    // Re-throw so route guards / stores can handle 5xx / network errors
    // without incorrectly treating an authenticated user as unauthenticated.
    throw err
  }
}

/**
 * 登录结果:账户已启用 TOTP 时后端先返回 mfaRequired(pending 会话 cookie 已下发),
 * 前端进入验证码步骤,再经 loginMFA(mfa API)完成登录。
 */
export type LoginResult = { mfaRequired: true } | ({ mfaRequired: false } & SessionUser)

export async function login(username: string, password: string): Promise<LoginResult> {
  const data = await http.post<Partial<SessionUser> & { mfa_required?: boolean }>(
    '/api/auth/login',
    { username, password },
  )
  if (data?.mfa_required) {
    return { mfaRequired: true }
  }
  return {
    mfaRequired: false,
    username: data.username ?? '',
    role: data.role === 'member' ? 'member' : 'admin',
  }
}

export async function logout(): Promise<void> {
  await http.post<void>('/api/auth/logout')
}

/**
 * Register a normal user via an invitation token (public endpoint).
 * On success the backend issues session cookies (auto-login) and returns the identity.
 * Errors (HttpError.apiError.code): invitation_invalid / invitation_used /
 * invitation_expired / username_taken / invalid_username / weak_password.
 */
export async function register(
  token: string,
  username: string,
  password: string,
): Promise<SessionUser> {
  return http.post<SessionUser>('/api/auth/register', { token, username, password })
}

/** Look up an invitation's state for the registration page (public, read-only). 404 → invalid. */
export async function fetchInvitation(token: string): Promise<InvitationInfo> {
  return http.get<InvitationInfo>(`/api/auth/invitations/${encodeURIComponent(token)}`)
}
