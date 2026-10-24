/**
 * Runs.test.ts — /runs 视图「项目筛选」回归测试。
 *
 * 覆盖:项目列表前 N 个渲染为横向标签 + 下拉兜底;选择项目后 listRuns 带上 projectId;
 * 空结果时「清除筛选」同时重置项目筛选与状态筛选;无项目时不渲染项目筛选区。
 * i18n 插件与 zh-CN locale 由 src/test/setup.ts 全局安装。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

const listRunsMock = vi.fn()
const listProjectsMock = vi.fn()
const pushMock = vi.fn()

vi.mock('../api/runs', () => ({
  listRuns: (...args: unknown[]) => listRunsMock(...args),
  triggerManual: vi.fn(),
}))
vi.mock('../api/projects', () => ({
  listProjects: (...args: unknown[]) => listProjectsMock(...args),
}))
vi.mock('../api/parameters', () => ({
  getParameters: vi.fn().mockResolvedValue([]),
  validateParamValues: vi.fn(),
}))
vi.mock('vue-router', () => ({
  useRouter: () => ({ push: pushMock }),
  useRoute: () => ({ query: {} }),
}))

import Runs from './Runs.vue'
import AppSelect from '../components/ui/AppSelect.vue'
import type { Project } from '../api/projects'

function project(id: string, name: string): Project {
  return {
    id,
    name,
    repoUrl: 'https://example.com/x.git',
    defaultBranch: 'main',
    credentialId: 'c-1',
    credentialName: 'cred',
    pacEnabled: false,
    prStatusEnabled: false,
    lastRunStatus: null,
    lastRunAt: null,
    serviceRegs: [],
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  }
}

async function mountRuns(): Promise<VueWrapper> {
  const w = mount(Runs)
  await flushPromises()
  return w
}

function projectGroup(w: VueWrapper) {
  return w.get('[role="group"][aria-label="按项目筛选"]')
}

function lastParams(): Record<string, unknown> {
  const calls = listRunsMock.mock.calls
  return calls[calls.length - 1]?.[0] as Record<string, unknown>
}

beforeEach(() => {
  // Runs.vue 现按角色分流详情路由 → 需激活 pinia 供 useSessionStore() 使用。
  setActivePinia(createPinia())
  vi.clearAllMocks()
  listRunsMock.mockResolvedValue({ items: [], page: 1, total: 0 })
  listProjectsMock.mockResolvedValue([project('p-a', 'acme-web'), project('p-b', 'billing-svc')])
})

describe('Runs · 项目筛选', () => {
  it('初次加载不带 projectId;点击项目标签后 listRuns 带上该项目', async () => {
    const w = await mountRuns()
    expect(listRunsMock).toHaveBeenCalledTimes(1)
    expect(lastParams()).toEqual({})

    const btn = projectGroup(w).findAll('button').find((b) => b.text() === 'acme-web')
    expect(btn).toBeTruthy()
    await btn!.trigger('click')
    await flushPromises()

    expect(listRunsMock).toHaveBeenCalledTimes(2)
    expect(lastParams()).toEqual({ projectId: 'p-a' })
  })

  it('横向标签最多 6 个,下拉兜底列出全部项目', async () => {
    listProjectsMock.mockResolvedValue(
      Array.from({ length: 8 }, (_, i) => project(`p-${i}`, `proj-${i}`)),
    )
    const w = await mountRuns()

    const labels = projectGroup(w).findAll('button').map((b) => b.text())
    expect(labels).toEqual(['全部项目', 'proj-0', 'proj-1', 'proj-2', 'proj-3', 'proj-4', 'proj-5'])

    const options = w.findComponent(AppSelect).props('options') as Array<{ value: string }>
    expect(options.map((o) => o.value)).toEqual([
      '',
      'p-0',
      'p-1',
      'p-2',
      'p-3',
      'p-4',
      'p-5',
      'p-6',
      'p-7',
    ])
  })

  it('下拉兜底可选择不在标签中的项目', async () => {
    listProjectsMock.mockResolvedValue(
      Array.from({ length: 8 }, (_, i) => project(`p-${i}`, `proj-${i}`)),
    )
    const w = await mountRuns()

    w.findComponent(AppSelect).vm.$emit('update:modelValue', 'p-7')
    await flushPromises()

    expect(lastParams()).toEqual({ projectId: 'p-7' })
  })

  it('无匹配结果时「清除筛选」同时重置项目筛选', async () => {
    const w = await mountRuns()
    const group = projectGroup(w)
    await group.findAll('button').find((b) => b.text() === 'billing-svc')!.trigger('click')
    await flushPromises()

    expect(w.text()).toContain('没有符合当前筛选条件的运行。')

    const clearBtn = w.findAll('button').find((b) => b.text() === '清除筛选')
    expect(clearBtn).toBeTruthy()
    await clearBtn!.trigger('click')
    await flushPromises()

    expect(lastParams()).toEqual({})
    const allBtn = projectGroup(w).findAll('button').find((b) => b.text() === '全部项目')
    expect(allBtn!.classes()).toContain('filter-tab--active')
  })

  it('无项目时不渲染项目筛选区', async () => {
    listProjectsMock.mockResolvedValue([])
    const w = await mountRuns()
    expect(w.find('[role="group"][aria-label="按项目筛选"]').exists()).toBe(false)
  })
})
