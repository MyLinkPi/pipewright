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
vi.mock('../api/objectStores', () => ({ listObjectStores: vi.fn().mockResolvedValue([]) }))
vi.mock('../api/pipelineValidation', () => ({ getValidation: vi.fn().mockResolvedValue({ ready: true, issues: [] }) }))

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

  it('loadSettings 成功且无任何脏改动 → 保存:不发包,如实提示「没有未保存的修改」', async () => {
    getSettingsMock.mockResolvedValue(settingsDTO())
    const w = mountView()
    await flushPromises()

    await w.find('.top-btn--save').trigger('click')
    await flushPromises()

    expect(saveSettings).not.toHaveBeenCalled()
    expect(w.find('.pipeline-banner--success').text()).toContain('没有未保存的修改')
  })
})

// ─── #190 回归:按 dirty 全 scope 保存,不再按激活 tab 决定范围 ────────────────

/** 供 settings 脏用例:stub VarsCacheTab,捕获实例以手动 emit update。 */
const VarsCacheTabStub = {
  name: 'VarsCacheTab',
  template: '<div class="vars-stub" />',
  emits: ['update'],
}

/** 供模板应用用例:stub TemplatePickerModal,可手动 emit applied。 */
const TemplatePickerModalStub = {
  name: 'TemplatePickerModal',
  template: '<div class="tpl-stub" />',
  emits: ['applied', 'close'],
}

function mountViewEx(extra: Record<string, unknown> = {}) {
  return mount(ProjectPipeline, {
    global: {
      stubs: {
        PipelineCanvas: PipelineCanvasStub,
        VarsCacheTab: VarsCacheTabStub,
        TriggersPanel: true,
        RunnerPanel: true,
        ValidationPanel: true,
        AIGenerateWizard: true,
        RiskAnnotationModal: true,
        YamlImportModal: true,
        TemplatePickerModal: TemplatePickerModalStub,
        PacPreviewModal: true,
        ProjectPreviewConfig: true,
        AppTooltip: true,
        RouterLink: { template: '<a><slot /></a>' },
        ...extra,
      },
    },
  })
}

describe('ProjectPipeline — 按 dirty 全 scope 保存(#190)', () => {
  const serverStage: PipelineStage = { id: 'p_s1', name: 's1', jobs: [] } as unknown as PipelineStage

  function dirtyBuild(dto: SettingsDTO): SettingsDTO {
    return { ...dto, build: { ...dto.build, dockerfilePath: 'Dockerfile.prod' } }
  }

  it('激活 vars tab 但只有画布脏 → 保存发 savePipeline 不发 saveSettings(与激活 tab 无关)', async () => {
    getSettingsMock.mockResolvedValue(settingsDTO())
    getPipelineMock.mockResolvedValue(pipelineDTO([serverStage]))
    const { savePipeline } = await import('../api/pipeline')
    vi.mocked(savePipeline).mockResolvedValue(pipelineDTO([serverStage]))
    const w = mountViewEx()
    await flushPromises()

    // 画布替换 stages → pipeline 脏;settings 未动。
    w.findComponent(PipelineCanvasStub).vm.$emit('update', [{ ...serverStage, name: 's1-renamed' } as unknown as PipelineStage])
    await nextTick()
    await w.find('.top-btn--save').trigger('click')
    await flushPromises()

    expect(savePipeline).toHaveBeenCalledTimes(1)
    expect(saveSettings).not.toHaveBeenCalled()
    expect(w.find('.pipeline-banner--success').text()).toContain('流水线已保存')
  })

  it('settings 与画布都脏 → 两个 scope 依次保存', async () => {
    getSettingsMock.mockResolvedValue(settingsDTO())
    vi.mocked(saveSettings).mockResolvedValue(settingsDTO())
    getPipelineMock.mockResolvedValue(pipelineDTO([serverStage]))
    const { savePipeline } = await import('../api/pipeline')
    vi.mocked(savePipeline).mockResolvedValue(pipelineDTO([serverStage]))
    const w = mountViewEx()
    await flushPromises()

    w.findComponent(VarsCacheTabStub).vm.$emit('update', dirtyBuild(settingsDTO()).build)
    w.findComponent(PipelineCanvasStub).vm.$emit('update', [{ ...serverStage, name: 'renamed' } as unknown as PipelineStage])
    await nextTick()
    await w.find('.top-btn--save').trigger('click')
    await flushPromises()

    expect(saveSettings).toHaveBeenCalledTimes(1)
    expect(savePipeline).toHaveBeenCalledTimes(1)
    expect(w.find('.pipeline-banner--success').exists()).toBe(true)
  })

  it('settings 保存成功但画布保存失败 → 错误横幅如实列出已保存 scope(#190 部分失败可见)', async () => {
    getSettingsMock.mockResolvedValue(settingsDTO())
    vi.mocked(saveSettings).mockResolvedValue(settingsDTO())
    getPipelineMock.mockResolvedValue(pipelineDTO([serverStage]))
    const { savePipeline } = await import('../api/pipeline')
    vi.mocked(savePipeline).mockRejectedValue(new Error('boom'))
    const w = mountViewEx()
    await flushPromises()

    w.findComponent(VarsCacheTabStub).vm.$emit('update', dirtyBuild(settingsDTO()).build)
    w.findComponent(PipelineCanvasStub).vm.$emit('update', [{ ...serverStage, name: 'renamed' } as unknown as PipelineStage])
    await nextTick()
    await w.find('.top-btn--save').trigger('click')
    await flushPromises()

    expect(saveSettings).toHaveBeenCalledTimes(1)
    const banner = w.find('.pipeline-banner--error')
    expect(banner.exists()).toBe(true)
    expect(banner.text()).toContain('变量/环境')
  })

  it('模板应用只重载流水线,不再覆写未保存的 settings 草稿(#190)', async () => {
    getSettingsMock.mockResolvedValue(settingsDTO())
    getPipelineMock.mockResolvedValue(pipelineDTO([serverStage]))
    const w = mountViewEx()
    await flushPromises()
    const settingsCallsBefore = getSettingsMock.mock.calls.length

    // 打开模板弹窗(v-if 挂载),再触发 applied(stages-only 持久化):
    // 此前会 loadSettings() 把草稿冲掉。
    // 头部按钮按顺序找到「模板」入口(点击后模板弹窗挂载)。
    for (const btn of w.findAll('.top-btn')) {
      await btn.trigger('click')
      if (w.findComponent(TemplatePickerModalStub).exists()) break
    }
    expect(w.findComponent(TemplatePickerModalStub).exists()).toBe(true)
    w.findComponent(TemplatePickerModalStub).vm.$emit('applied', pipelineDTO([serverStage]))
    await flushPromises()

    expect(getPipelineMock.mock.calls.length).toBeGreaterThanOrEqual(2)
    expect(getSettingsMock.mock.calls.length).toBe(settingsCallsBefore) // 未再拉 settings
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
    // 位置回归:未保存提示应与状态标签同处标题区,而非混进右上操作按钮组
    expect(w.find('.pipeline-title .unsaved-chip').exists()).toBe(true)
    expect(w.find('.pipeline-top-actions .unsaved-chip').exists()).toBe(false)
  })
})
