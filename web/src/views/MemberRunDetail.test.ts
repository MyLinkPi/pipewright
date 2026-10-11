/**
 * MemberRunDetail.test.ts — 普通用户只读运行详情(/my-runs/:id)。
 *
 * 覆盖:详情 meta 与分节渲染(步骤/日志/产物子组件挂载)、404/403 → 未找到提示 + 重试、
 * 返回按钮回到运行列表。子组件以 stub 注入,聚焦本页编排与只读语义。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { HttpError } from '../api/http'

const h = vi.hoisted(() => ({
  getRun: vi.fn(),
  subscribeRunEvents: vi.fn(),
  push: vi.fn(),
}))

vi.mock('../api/runs', () => ({
  getRun: (...a: unknown[]) => h.getRun(...a),
  subscribeRunEvents: (...a: unknown[]) => h.subscribeRunEvents(...a),
}))
vi.mock('vue-router', () => ({
  useRoute: () => ({ params: { id: 'r1' } }),
  useRouter: () => ({ push: h.push }),
}))
vi.mock('../components/run/RunStepList.vue', () => ({
  default: { name: 'RunStepList', template: '<div class="step-list-stub" />' },
}))
vi.mock('../components/run/RunTerminal.vue', () => ({
  default: { name: 'RunTerminal', template: '<div class="terminal-stub" />' },
}))
vi.mock('../components/run/ArtifactList.vue', () => ({
  default: { name: 'ArtifactList', template: '<div class="artifact-stub" />' },
}))

import MemberRunDetail from './MemberRunDetail.vue'

function runDetail() {
  return {
    id: 'r1',
    projectId: 'p1',
    projectName: 'acme-web',
    status: 'success',
    trigger: { type: 'manual', branch: 'main', commit: 'abcdef1234', actor: 'admin' },
    steps: [],
    artifacts: [],
    durationMs: 12000,
    createdAt: '2026-01-01T00:00:00Z',
  }
}

beforeEach(() => {
  vi.clearAllMocks()
  h.subscribeRunEvents.mockReturnValue(() => {})
})

describe('MemberRunDetail', () => {
  it('渲染 meta 与步骤/日志/产物分节', async () => {
    h.getRun.mockResolvedValue(runDetail())
    const w = mount(MemberRunDetail)
    await flushPromises()

    expect(h.getRun).toHaveBeenCalledWith('r1')
    expect(w.text()).toContain('acme-web')
    expect(w.text()).toContain('main')
    expect(w.text()).toContain('成功')
    expect(w.text()).toContain('步骤')
    expect(w.text()).toContain('日志')
    expect(w.text()).toContain('产物')
    expect(w.find('.step-list-stub').exists()).toBe(true)
    expect(w.find('.terminal-stub').exists()).toBe(true)
  })

  it('无产物 → 展示空提示(不挂载产物列表)', async () => {
    h.getRun.mockResolvedValue(runDetail())
    const w = mount(MemberRunDetail)
    await flushPromises()

    expect(w.find('.artifact-stub').exists()).toBe(false)
    expect(w.text()).toContain('本次运行没有产物')
  })

  it('404 → 未找到提示 + 重试按钮', async () => {
    h.getRun.mockRejectedValue(new HttpError(404, null, 'nf'))
    const w = mount(MemberRunDetail)
    await flushPromises()

    expect(w.text()).toContain('运行不存在或你无权访问')
    expect(w.findAll('button').some((b) => b.text() === '重试')).toBe(true)
  })

  it('返回按钮 → 回到运行列表', async () => {
    h.getRun.mockResolvedValue(runDetail())
    const w = mount(MemberRunDetail)
    await flushPromises()

    await w.find('.back-btn').trigger('click')
    expect(h.push).toHaveBeenCalledWith({ name: 'my-runs' })
  })
})