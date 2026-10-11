/**
 * MemberSettings.test.ts — 普通用户设置页(/my-settings)。
 *
 * 覆盖:个人资料(用户名 + 角色)渲染、退出登录(确认 → 调接口 → 清会话 → 跳登录页)。
 * 改密 / 会话面板自身行为由其独立测试覆盖,此处仅验证组合渲染。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

const h = vi.hoisted(() => ({
  logout: vi.fn(),
  push: vi.fn(),
  confirmOpen: vi.fn(),
  listSessions: vi.fn(),
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warn: vi.fn() },
}))

vi.mock('../api/auth', () => ({ logout: (...a: unknown[]) => h.logout(...a) }))
vi.mock('../api/account', () => ({
  listSessions: (...a: unknown[]) => h.listSessions(...a),
  changePassword: vi.fn(),
  revokeSession: vi.fn(),
}))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: h.push }) }))
vi.mock('../composables/useConfirm', () => ({
  useConfirm: () => ({ open: h.confirmOpen, pending: { value: null }, _resolve: vi.fn() }),
}))
vi.mock('../composables/useToast', () => ({
  useToast: () => ({ ...h.toast, toasts: { value: [] }, dismiss: vi.fn(), clear: vi.fn() }),
}))

import MemberSettings from './MemberSettings.vue'
import { useSessionStore } from '../stores/session'

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  h.listSessions.mockResolvedValue([])
  h.confirmOpen.mockResolvedValue(true)
})

describe('MemberSettings', () => {
  it('渲染当前用户资料与角色', async () => {
    useSessionStore().setUser({ username: 'alice', role: 'member' })
    const w = mount(MemberSettings)
    await flushPromises()

    expect(w.text()).toContain('alice')
    expect(w.text()).toContain('普通用户')
  })

  it('组合渲染改密面板与会话面板', async () => {
    useSessionStore().setUser({ username: 'alice', role: 'member' })
    const w = mount(MemberSettings)
    await flushPromises()

    expect(w.text()).toContain('修改密码')
    expect(w.text()).toContain('活跃会话')
  })

  it('退出登录 → 调接口、清会话、跳登录页', async () => {
    useSessionStore().setUser({ username: 'alice', role: 'member' })
    h.logout.mockResolvedValue(undefined)
    const w = mount(MemberSettings)
    await flushPromises()

    await w.findAll('button').find((b) => b.text() === '退出登录')!.trigger('click')
    await flushPromises()

    expect(h.logout).toHaveBeenCalledTimes(1)
    expect(useSessionStore().user).toBeNull()
    expect(h.push).toHaveBeenCalledWith('/login')
  })

  it('退出接口失败 → 仍本地登出并跳转', async () => {
    useSessionStore().setUser({ username: 'alice', role: 'member' })
    h.logout.mockRejectedValue(new Error('down'))
    const w = mount(MemberSettings)
    await flushPromises()

    await w.findAll('button').find((b) => b.text() === '退出登录')!.trigger('click')
    await flushPromises()

    expect(useSessionStore().user).toBeNull()
    expect(h.push).toHaveBeenCalledWith('/login')
  })
})