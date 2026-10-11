/**
 * ActiveSessionsPanel.test.ts — 活跃会话面板(管理员/成员共用)。
 *
 * 覆盖:列表渲染(含当前会话标记)、撤销非当前会话(调接口 + 刷新)、
 * 撤销当前会话(清本地会话 + 跳登录页)。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

const h = vi.hoisted(() => ({
  listSessions: vi.fn(),
  revokeSession: vi.fn(),
  push: vi.fn(),
  confirmOpen: vi.fn(),
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warn: vi.fn() },
}))

vi.mock('../../api/account', () => ({
  listSessions: (...a: unknown[]) => h.listSessions(...a),
  revokeSession: (...a: unknown[]) => h.revokeSession(...a),
  changePassword: vi.fn(),
}))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: h.push }) }))
vi.mock('../../composables/useConfirm', () => ({
  useConfirm: () => ({ open: h.confirmOpen, pending: { value: null }, _resolve: vi.fn() }),
}))
vi.mock('../../composables/useToast', () => ({
  useToast: () => ({ ...h.toast, toasts: { value: [] }, dismiss: vi.fn(), clear: vi.fn() }),
}))

import ActiveSessionsPanel from './ActiveSessionsPanel.vue'
import { useSessionStore } from '../../stores/session'

function session(id: string, current: boolean) {
  return {
    id,
    createdAt: '2026-01-01T00:00:00Z',
    lastSeenAt: '2026-01-01T00:00:00Z',
    expiresAt: '2026-01-08T00:00:00Z',
    current,
  }
}

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  h.confirmOpen.mockResolvedValue(true)
})

describe('ActiveSessionsPanel', () => {
  it('渲染会话列表(含当前会话标记)', async () => {
    h.listSessions.mockResolvedValue([session('s1', true), session('s2', false)])
    const w = mount(ActiveSessionsPanel)
    await flushPromises()

    expect(w.text()).toContain('s1')
    expect(w.text()).toContain('s2')
    expect(w.text()).toContain('当前会话')
  })

  it('撤销非当前会话 → 调接口并刷新列表', async () => {
    h.listSessions.mockResolvedValue([session('s1', true), session('s2', false)])
    h.revokeSession.mockResolvedValue(undefined)
    const w = mount(ActiveSessionsPanel)
    await flushPromises()

    const revokeBtns = w.findAll('button').filter((b) => b.text() === '注销')
    await revokeBtns[1].trigger('click')
    await flushPromises()

    expect(h.revokeSession).toHaveBeenCalledWith('s2')
    expect(h.listSessions).toHaveBeenCalledTimes(2)
    expect(h.push).not.toHaveBeenCalled()
  })

  it('撤销当前会话 → 清本地会话并跳登录页', async () => {
    h.listSessions.mockResolvedValue([session('s1', true)])
    h.revokeSession.mockResolvedValue(undefined)
    const w = mount(ActiveSessionsPanel)
    await flushPromises()

    await w.findAll('button').find((b) => b.text() === '注销')!.trigger('click')
    await flushPromises()

    expect(useSessionStore().user).toBeNull()
    expect(h.push).toHaveBeenCalledWith('/login')
  })
})