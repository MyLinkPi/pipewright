/**
 * ChangePasswordPanel.test.ts — 改口令面板(管理员/成员共用)。
 *
 * 覆盖:客户端校验(过短 / 两次不一致)、成功路径(调接口 + emit updated + toast)、
 * 以及 invalid_current_password 的服务端错误映射。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import { HttpError } from '../../api/http'

const h = vi.hoisted(() => ({
  changePassword: vi.fn(),
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warn: vi.fn() },
}))

vi.mock('../../api/account', () => ({
  changePassword: (...a: unknown[]) => h.changePassword(...a),
  listSessions: vi.fn().mockResolvedValue([]),
  revokeSession: vi.fn(),
}))
vi.mock('../../composables/useToast', () => ({
  useToast: () => ({ ...h.toast, toasts: { value: [] }, dismiss: vi.fn(), clear: vi.fn() }),
}))

import ChangePasswordPanel from './ChangePasswordPanel.vue'

beforeEach(() => {
  vi.clearAllMocks()
})

/** 面板内三个 password 输入:当前 / 新 / 确认。 */
function pwInputs(w: VueWrapper) {
  return w.findAll('input[type="password"]')
}

describe('ChangePasswordPanel', () => {
  it('新口令过短 → 内联错误且不提交', async () => {
    const w = mount(ChangePasswordPanel)
    const inputs = pwInputs(w)
    await inputs[0].setValue('oldpass1')
    await inputs[1].setValue('short')
    await inputs[2].setValue('short')
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(h.changePassword).not.toHaveBeenCalled()
    expect(w.text()).toContain('新密码至少 8 位')
  })

  it('两次新口令不一致 → 内联错误且不提交', async () => {
    const w = mount(ChangePasswordPanel)
    const inputs = pwInputs(w)
    await inputs[0].setValue('oldpass1')
    await inputs[1].setValue('newpass1')
    await inputs[2].setValue('newpass2')
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(h.changePassword).not.toHaveBeenCalled()
    expect(w.text()).toContain('两次输入的新密码不一致')
  })

  it('成功 → 调接口、emit updated、toast 成功', async () => {
    h.changePassword.mockResolvedValue(undefined)
    const w = mount(ChangePasswordPanel)
    const inputs = pwInputs(w)
    await inputs[0].setValue('oldpass1')
    await inputs[1].setValue('newpass1')
    await inputs[2].setValue('newpass1')
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(h.changePassword).toHaveBeenCalledWith({ currentPassword: 'oldpass1', newPassword: 'newpass1' })
    expect(w.emitted('updated')).toHaveLength(1)
    expect(h.toast.success).toHaveBeenCalled()
  })

  it('invalid_current_password → 当前口令内联错误、不 emit', async () => {
    h.changePassword.mockRejectedValue(
      new HttpError(401, { code: 'invalid_current_password', message: 'bad' }, 'bad'),
    )
    const w = mount(ChangePasswordPanel)
    const inputs = pwInputs(w)
    await inputs[0].setValue('wrong')
    await inputs[1].setValue('newpass1')
    await inputs[2].setValue('newpass1')
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(w.text()).toContain('当前密码错误')
    expect(w.emitted('updated')).toBeUndefined()
  })
})