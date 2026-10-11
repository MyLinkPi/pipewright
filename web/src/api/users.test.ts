/**
 * users.test.ts — 管理员用户体系 API 层(/api/users、/api/invitations、项目成员)。
 *
 * 只校验「URL / 方法 / 解包」契约:各函数把后端信封({users}/{invitations}/{members})
 * 解包为数组,写入类端点携带正确的路径与 body。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'

const getMock = vi.fn()
const postMock = vi.fn()
const deleteMock = vi.fn()

vi.mock('./http', () => ({
  http: {
    get: (...a: unknown[]) => getMock(...a),
    post: (...a: unknown[]) => postMock(...a),
    delete: (...a: unknown[]) => deleteMock(...a),
  },
}))

import {
  listUsers,
  createInvitation,
  listInvitations,
  revokeInvitation,
  listProjectMembers,
  addProjectMember,
  removeProjectMember,
} from './users'

beforeEach(() => {
  vi.clearAllMocks()
})

describe('users api', () => {
  it('listUsers 解包 { users }', async () => {
    getMock.mockResolvedValue({ users: [{ id: 'u1' }] })
    await expect(listUsers()).resolves.toEqual([{ id: 'u1' }])
    expect(getMock).toHaveBeenCalledWith('/api/users')
  })

  it('createInvitation POST /api/invitations', async () => {
    postMock.mockResolvedValue({ id: 'i1', token: 'tok' })
    await expect(createInvitation()).resolves.toEqual({ id: 'i1', token: 'tok' })
    expect(postMock).toHaveBeenCalledWith('/api/invitations')
  })

  it('listInvitations 解包 { invitations }', async () => {
    getMock.mockResolvedValue({ invitations: [{ id: 'i1' }] })
    await expect(listInvitations()).resolves.toEqual([{ id: 'i1' }])
    expect(getMock).toHaveBeenCalledWith('/api/invitations')
  })

  it('revokeInvitation DELETE 指定 id', async () => {
    deleteMock.mockResolvedValue(undefined)
    await revokeInvitation('i1')
    expect(deleteMock).toHaveBeenCalledWith('/api/invitations/i1')
  })

  it('listProjectMembers 解包 { members }', async () => {
    getMock.mockResolvedValue({ members: [{ id: 'u1' }] })
    await expect(listProjectMembers('p1')).resolves.toEqual([{ id: 'u1' }])
    expect(getMock).toHaveBeenCalledWith('/api/projects/p1/members')
  })

  it('addProjectMember POST body { userId }', async () => {
    postMock.mockResolvedValue(undefined)
    await addProjectMember('p1', 'u1')
    expect(postMock).toHaveBeenCalledWith('/api/projects/p1/members', { userId: 'u1' })
  })

  it('removeProjectMember DELETE 指定成员', async () => {
    deleteMock.mockResolvedValue(undefined)
    await removeProjectMember('p1', 'u1')
    expect(deleteMock).toHaveBeenCalledWith('/api/projects/p1/members/u1')
  })
})