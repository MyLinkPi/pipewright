/**
 * ProjectPipeline 视图级回归:
 *  ① settings 加载失败时,vars/envs tab 点保存不再「假成功」(错误横幅 + 不发请求)。
 *  ② 脏检查键序无关:画布回写内容等价但键序不同的 stages 不报脏(改回原值不判脏)。
 * 重型子面板全部 stub;API 层 vi.mock。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'

import type { PipelineDTO, PipelineStage } from '../api/pipeline'
import type { SettingsDTO } from '../api/pipelineSettings'
import { saveSettings } from '../api/pipelineSettings'

// ─── API mocks ───────────────────────────────────────────────────────────────

const getPipelineMock = vi.fn()
const getSettingsMock = vi.fn()

vi.mock('../api/pipeline', () => ({
  getPipeline: (...args: unknown[]) => getPipelineMock(...args),
  savePipeline: vi.fn(),
}))
vi.mock('../api/pipelineSettings', () => ({
  getSettings: (...args: unknown[]) => getSettingsMock(...args),
  saveSettings: vi.fn(),
}))
vi.mock('../api/credentials', () => ({ listCredentials: vi.fn().mockResolvedValue([]) }))
vi.mock('../api/projects', () => ({
  listProjects: vi.fn().mockResolvedValue([]),
  updateProject: vi.fn(),
}))
vi.mock('../api/servers', () => ({ listServers: vi.fn().mockResolvedValue([]) }))
vi.mock('../api/notifications', () => ({ listChannels: vi.fn().mockResolvedValue([]) }))
vi.mock('../api/pipelineValidation', () => ({ getValidation: vi.fn() }))

let routeQuery: Record<string, string> = { tab: 'vars' }
vi.mock('vue-router', () => ({
  useRoute: () => ({ params: { id: 'p1' }, query: routeQuery }),
  useRouter: () => ({ replace: vi.fn(), beforeEach: vi.fn(() => vi.fn()) }),
}))

import ProjectPipeline from './ProjectPipeline.vue'

// ─── Fixtures ────────────────────────────────────────────────────────────────

function pipelineDTO(stages: PipelineStage[]): PipelineDTO {
  return { stages, yaml: '', status: 'draft', updatedAt: '2026-01-01T00:00:00Z' }
}

function settingsDTO(): SettingsDTO {
  return {
    build: {
      model: 'dockerfile',
      dockerfilePath: 'Dockerfile',
      toolchain: { language: '', version: '' },
      artifactType: 'image',
      vars: [],
      cache: { enabled: false, paths: [] },
    },
    environments: [],
    updatedAt: '2026-01-01T00:00:00Z',
  }
}

/** 供脏检查用例:stub 画布组件,捕获实例以便手动 emit update。 */
const PipelineCanvasStub = {
  name: 'PipelineCanvas',
  template: '<div class="canvas-stub" />',
  emits: ['update'],
}

function mountView() {
  return mount(ProjectPipeline, {
    global: {
      stubs: {
        PipelineCanvas: PipelineCanvasStub,
        VarsCacheTab: true,
        EnvCredsTab: true,
        TriggersPanel: true,
        RunnerPanel: true,
        ValidationPanel: true,
        AIGenerateWizard: true,
        RiskAnnotationModal: true,
        YamlImportModal: true,
        TemplatePickerModal: true,
        PacPreviewModal: true,
        ProjectPreviewConfig: true,
        AppTooltip: true,
        RouterLink: { template: '<a><slot /></a>' },
      },
    },
  })
}

beforeEach(() => {
  vi.clearAllMocks()
  routeQuery = { tab: 'vars' }
  getPipelineMock.mockResolvedValue(pipelineDTO([]))
})

describe('ProjectPipeline — settings 加载失败时保存不假成功', () => {
  it('loadSettings 失败 → vars tab 保存:错误横幅、不发 saveSettings、无成功横幅', async () => {
    getSettingsMock.mockRejectedValue(new Error('boom'))
    const w = mountView()
    await flushPromises()

    await w.find('.top-btn--save').trigger('click')
    await flushPromises()

    expect(saveSettings).not.toHaveBeenCalled()
    const banner = w.find('.pipeline-banner--error')
    expect(banner.exists()).toBe(true)
    expect(banner.text()).toContain('设置尚未加载成功')
    expect(w.find('.pipeline-banner--success').exists()).toBe(false)
  })

  it('loadSettings 成功 → vars tab 保存:正常发请求并显示成功横幅', async () => {
    getSettingsMock.mockResolvedValue(settingsDTO())
    vi.mocked(saveSettings).mockResolvedValue(settingsDTO())
    const w = mountView()
    await flushPromises()

    await w.find('.top-btn--save').trigger('click')
    await flushPromises()

    expect(saveSettings).toHaveBeenCalledTimes(1)
    expect(w.find('.pipeline-banner--success').exists()).toBe(true)
  })
})

describe('ProjectPipeline — 脏检查键序无关(改回原值不判脏)', () => {
  const serverStages: PipelineStage[] = [
    {
      id: 'stg_1',
      name: '构建',
      kind: 'build',
      jobs: [
        {
          id: 'job_1',
          name: '前端',
          type: 'build_nodejs',
          summary: '',
          // 服务器 DTO 键序:image 在前
          config: { image: 'node:20', commands: 'npm ci' },
        },
      ],
    },
  ]

  it('画布回写内容等价但 config 键序不同 → 不报脏', async () => {
    routeQuery = { tab: 'canvas' }
    getPipelineMock.mockResolvedValue(pipelineDTO(serverStages))
    getSettingsMock.mockResolvedValue(settingsDTO())
    const w = mountView()
    await flushPromises()
    expect(w.find('.unsaved-chip').exists()).toBe(false)

    // JobDrawer 以 {...extras, ...typed} 重建 config → 键序颠倒、内容等价。
    const reordered: PipelineStage[] = [
      {
        ...serverStages[0],
        jobs: [{ ...serverStages[0].jobs[0], config: { commands: 'npm ci', image: 'node:20' } }],
      },
    ]
    w.findComponent(PipelineCanvasStub).vm.$emit('update', reordered)
    await nextTick()

    expect(w.find('.unsaved-chip').exists()).toBe(false)
  })

  it('内容真变更仍报脏(对照组)', async () => {
    routeQuery = { tab: 'canvas' }
    getPipelineMock.mockResolvedValue(pipelineDTO(serverStages))
    getSettingsMock.mockResolvedValue(settingsDTO())
    const w = mountView()
    await flushPromises()

    const changed: PipelineStage[] = [
      {
        ...serverStages[0],
        jobs: [{ ...serverStages[0].jobs[0], config: { image: 'node:22', commands: 'npm ci' } }],
      },
    ]
    w.findComponent(PipelineCanvasStub).vm.$emit('update', changed)
    await nextTick()

    expect(w.find('.unsaved-chip').exists()).toBe(true)
  })
})
