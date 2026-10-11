/**
 * Users.test.ts — 管理员「用户管理」页三区块:用户列表 / 邀请 / 项目成员。
 *
 * 覆盖:用户列表渲染、生成邀请后展示完整注册链接、撤销邀请调用接口。
 * toast / confirm 以 stub 注入,聚焦页面编排逻辑。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

const h = vi.hoisted(() => ({
  listUsers: vi.fn(),
  createInvitation: vi.fn(),
  listInvitations: vi.fn(),
  revokeInvitation: vi.fn(),
  listProjectMembers: vi.fn(),
  addProjectMember: vi.fn(),
  removeProjectMember: vi.fn(),
  listProjects: vi.fn(),
  confirmOpen: vi.fn(),
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warn: vi.fn() },
}))

vi.mock('../api/users', () => ({
  listUsers: (...a: unknown[]) => h.listUsers(...a),
  createInvitation: (...a: unknown[]) => h.createInvitation(...a),
  listInvitations: (...a: unknown[]) => h.listInvitations(...a),
  revokeInvitation: (...a: unknown[]) => h.revokeInvitation(...a),
  listProjectMembers: (...a: unknown[]) => h.listProjectMembers(...a),
  addProjectMember: (...a: unknown[]) => h.addProjectMember(...a),
  removeProjectMember: (...a: unknown[]) => h.removeProjectMember(...a),
}))
vi.mock('../api/projects', () => ({
  listProjects: (...a: unknown[]) => h.listProjects(...a),
}))
vi.mock('../composables/useToast', () => ({
  useToast: () => ({ ...h.toast, toasts: { value: [] }, dismiss: vi.fn(), clear: vi.fn() }),
}))
vi.mock('../composables/useConfirm', () => ({
  useConfirm: () => ({ open: h.confirmOpen, pending: { value: null }, _resolve: vi.fn() }),
}))

import Users from './Users.vue'

beforeEach(() => {
  vi.clearAllMocks()
  h.listUsers.mockResolvedValue([])
  h.listInvitations.mockResolvedValue([])
  h.listProjects.mockResolvedValue([])
  h.confirmOpen.mockResolvedValue(true)
})

async function mountPage() {
  const w = mount(Users)
  await flushPromises()
  return w
}

describe('Users · 用户列表', () => {
  it('渲染普通用户及其角色', async () => {
    h.listUsers.mockResolvedValue([
      { id: 'u1', username: 'alice', role: 'member', status: 'active', createdAt: '2026-01-01T00:00:00Z' },
    ])
    const w = await mountPage()
    expect(w.text()).toContain('alice')
    expect(w.text()).toContain('普通用户')
  })

  it('无用户时展示空状态', async () => {
    const w = await mountPage()
    expect(w.text()).toContain('暂无普通用户')
  })
})

describe('Users · 邀请', () => {
  it('生成邀请 → 展示可复制的完整注册链接', async () => {
    h.createInvitation.mockResolvedValue({
      id: 'i1',
      token: 'inv-token',
      createdBy: 'admin',
      expiresAt: '2026-01-08T00:00:00Z',
      state: 'pending',
      createdAt: '2026-01-01T00:00:00Z',
    })
    const w = await mountPage()

    await w.findAll('button').find((b) => b.text() === '生成邀请')!.trigger('click')
    await flushPromises()

    expect(h.createInvitation).toHaveBeenCalledTimes(1)
    const input = w.find('.invite-new__input').element as HTMLInputElement
    expect(input.value).toContain('/register?token=inv-token')
  })

  it('撤销待用邀请 → 调用接口', async () => {
    h.listInvitations.mockResolvedValue([
      {
        id: 'i1',
        token: 'tok',
        createdBy: 'admin',
        expiresAt: '2026-01-08T00:00:00Z',
        state: 'pending',
        createdAt: '2026-01-01T00:00:00Z',
      },
    ])
    h.revokeInvitation.mockResolvedValue(undefined)
    const w = await mountPage()

    await w.findAll('button').find((b) => b.text() === '撤销')!.trigger('click')
    await flushPromises()

    expect(h.revokeInvitation).toHaveBeenCalledWith('i1')
  })
})