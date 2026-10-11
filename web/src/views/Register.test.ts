/**
 * Register.test.ts — 邀请注册页(/register?token=...):邀请状态机 + 提交分支。
 *
 * fetchInvitation 返回的 state 决定页面形态(pending→表单;used / expired;404→invalid);
 * 提交成功 → 写入本地会话并跳 /my-projects;后端错误码映射到具体字段/状态。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { HttpError } from '../api/http'

const h = vi.hoisted(() => ({
  routeQuery: {} as Record<string, unknown>,
  replace: vi.fn(),
  fetchInvitation: vi.fn(),
  register: vi.fn(),
}))

vi.mock('vue-router', () => ({
  useRoute: () => ({ query: h.routeQuery }),
  useRouter: () => ({ replace: h.replace }),
}))
vi.mock('../api/auth', () => ({
  fetchInvitation: (...a: unknown[]) => h.fetchInvitation(...a),
  register: (...a: unknown[]) => h.register(...a),
}))

import Register from './Register.vue'
import { useSessionStore } from '../stores/session'

beforeEach(() => {
  setActivePinia(createPinia())
  vi.clearAllMocks()
  h.routeQuery = {}
})

async function mountPage(): Promise<VueWrapper> {
  const w = mount(Register)
  await flushPromises()
  return w
}

describe('Register · 邀请状态', () => {
  it('pending → 渲染注册表单', async () => {
    h.routeQuery = { token: 'tok' }
    h.fetchInvitation.mockResolvedValue({ state: 'pending' })
    const w = await mountPage()
    expect(h.fetchInvitation).toHaveBeenCalledWith('tok')
    expect(w.find('#reg-username').exists()).toBe(true)
    expect(w.text()).toContain('创建你的账号')
  })

  it('缺 token → 无效提示(不请求接口)', async () => {
    const w = await mountPage()
    expect(h.fetchInvitation).not.toHaveBeenCalled()
    expect(w.text()).toContain('邀请链接无效')
  })

  it('used → 已使用提示(不渲染表单)', async () => {
    h.routeQuery = { token: 'tok' }
    h.fetchInvitation.mockResolvedValue({ state: 'used' })
    const w = await mountPage()
    expect(w.text()).toContain('邀请已被使用')
    expect(w.find('#reg-username').exists()).toBe(false)
  })

  it('expired → 已过期提示', async () => {
    h.routeQuery = { token: 'tok' }
    h.fetchInvitation.mockResolvedValue({ state: 'expired' })
    const w = await mountPage()
    expect(w.text()).toContain('邀请已过期')
  })

  it('404 → 无效提示', async () => {
    h.routeQuery = { token: 'tok' }
    h.fetchInvitation.mockRejectedValue(new HttpError(404, null, 'not found'))
    const w = await mountPage()
    expect(w.text()).toContain('邀请链接无效')
  })
})

describe('Register · 提交', () => {
  it('成功 → 写会话并跳转 /my-projects', async () => {
    h.routeQuery = { token: 'tok' }
    h.fetchInvitation.mockResolvedValue({ state: 'pending' })
    h.register.mockResolvedValue({ username: 'alice', role: 'member' })
    const w = await mountPage()

    await w.find('#reg-username').setValue('alice')
    await w.find('#reg-password').setValue('password1')
    await w.find('#reg-confirm').setValue('password1')
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(h.register).toHaveBeenCalledWith('tok', 'alice', 'password1')
    expect(useSessionStore().user).toEqual({ username: 'alice', role: 'member' })
    expect(h.replace).toHaveBeenCalledWith('/my-projects')
  })

  it('用户名非法 → 内联错误且不请求接口', async () => {
    h.routeQuery = { token: 'tok' }
    h.fetchInvitation.mockResolvedValue({ state: 'pending' })
    const w = await mountPage()

    await w.find('#reg-username').setValue('ab')
    await w.find('#reg-password').setValue('password1')
    await w.find('#reg-confirm').setValue('password1')
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(h.register).not.toHaveBeenCalled()
    expect(w.text()).toContain('用户名需为')
  })

  it('口令不一致 → 确认字段内联错误', async () => {
    h.routeQuery = { token: 'tok' }
    h.fetchInvitation.mockResolvedValue({ state: 'pending' })
    const w = await mountPage()

    await w.find('#reg-username').setValue('alice')
    await w.find('#reg-password').setValue('password1')
    await w.find('#reg-confirm').setValue('password2')
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(h.register).not.toHaveBeenCalled()
    expect(w.text()).toContain('两次输入的密码不一致')
  })

  it('username_taken → 用户名内联错误', async () => {
    h.routeQuery = { token: 'tok' }
    h.fetchInvitation.mockResolvedValue({ state: 'pending' })
    h.register.mockRejectedValue(new HttpError(409, { code: 'username_taken', message: 'taken' }, 'taken'))
    const w = await mountPage()

    await w.find('#reg-username').setValue('alice')
    await w.find('#reg-password').setValue('password1')
    await w.find('#reg-confirm').setValue('password1')
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(w.text()).toContain('该用户名已被占用')
  })

  it('invitation_used → 切换到已使用状态', async () => {
    h.routeQuery = { token: 'tok' }
    h.fetchInvitation.mockResolvedValue({ state: 'pending' })
    h.register.mockRejectedValue(new HttpError(410, { code: 'invitation_used', message: 'used' }, 'used'))
    const w = await mountPage()

    await w.find('#reg-username').setValue('alice')
    await w.find('#reg-password').setValue('password1')
    await w.find('#reg-confirm').setValue('password1')
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(w.text()).toContain('邀请已被使用')
  })
})