/**
 * Users API — 管理员用户体系(用户 / 邀请 / 项目成员)。
 *
 * GET    /api/users                              → { users: UserDTO[] }
 * POST   /api/invitations                        → InvitationDTO            (需 CSRF)
 * GET    /api/invitations                        → { invitations: InvitationDTO[] }
 * DELETE /api/invitations/{id}                   → 204                     (需 CSRF)
 * GET    /api/projects/{id}/members              → { members: UserDTO[] }
 * POST   /api/projects/{id}/members              → 204 (body {userId} 或 {username};需 CSRF)
 * DELETE /api/projects/{id}/members/{userId}     → 204                     (需 CSRF)
 *
 * 邀请 token 仅在创建响应中返回(用于拼注册链接);列表返回的 token 亦可用于展示,
 * 但产品上只在「刚创建」时提示复制完整链接。
 */

import { http } from './http'
import type { InvitationState } from './auth'
import type { UserRole } from '../stores/session'

/** 普通用户对外视图(绝不含口令哈希)。totpEnabled/passkeyCount 是两步验证概要。 */
export interface UserDTO {
  id: string
  username: string
  role: UserRole
  status: string
  createdAt: string
  totpEnabled: boolean
  passkeyCount: number
}

export interface InvitationDTO {
  id: string
  token: string
  createdBy: string
  expiresAt: string
  usedAt?: string
  state: InvitationState
  createdAt: string
}

export async function listUsers(): Promise<UserDTO[]> {
  const res = await http.get<{ users: UserDTO[] }>('/api/users')
  return res.users
}

export async function createInvitation(): Promise<InvitationDTO> {
  return http.post<InvitationDTO>('/api/invitations')
}

export async function listInvitations(): Promise<InvitationDTO[]> {
  const res = await http.get<{ invitations: InvitationDTO[] }>('/api/invitations')
  return res.invitations
}

export async function revokeInvitation(id: string): Promise<void> {
  await http.delete<void>(`/api/invitations/${id}`)
}

export async function listProjectMembers(projectId: string): Promise<UserDTO[]> {
  const res = await http.get<{ members: UserDTO[] }>(`/api/projects/${projectId}/members`)
  return res.members
}

export async function addProjectMember(projectId: string, userId: string): Promise<void> {
  await http.post<void>(`/api/projects/${projectId}/members`, { userId })
}

export async function removeProjectMember(projectId: string, userId: string): Promise<void> {
  await http.delete<void>(`/api/projects/${projectId}/members/${userId}`)
}
