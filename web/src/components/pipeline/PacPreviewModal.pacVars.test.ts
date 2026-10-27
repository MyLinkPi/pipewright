import { describe, expect, it, vi, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'

// 拉取/更新 API 打桩:预览返回带变量声明的成功结果;updateProject 记录调用。
const previewMock = vi.fn()
const updateProjectMock = vi.fn()
vi.mock('../../api/projects', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../../api/projects')>()
  return {
    ...orig,
    previewPacConfig: (...args: unknown[]) => previewMock(...args),
    updateProject: (...args: unknown[]) => updateProjectMock(...args),
  }
})

import PacPreviewModal from './PacPreviewModal.vue'

const okResult = {
  found: true,
  valid: true,
  ref: 'main',
  file: '.pipewright.yml',
  error: '',
  stageCount: 1,
  stages: [{ name: '源码', kind: 'source', jobCount: 1 }],
  variables: [
    { name: 'DB_CONN', description: '生产数据库连接字符串', required: true, secret: true, default: '' },
    { name: 'BASE_DOMAIN', description: '服务注册基域', required: false, secret: false, default: 'internal.example.com' },
  ],
}

function mountModal(pacFile = '') {
  return mount(PacPreviewModal, {
    props: { projectId: 'p1', defaultBranch: 'main', pacFile },
  })
}

/** 挂载后手动触发一次拉取(弹窗不自动拉取)。 */
async function mountAndFetch(pacFile = '', result: unknown = okResult) {
  previewMock.mockResolvedValue(result)
  const w = mountModal(pacFile)
  await nextTick()
  await w.find('.pp-btn--primary').trigger('click')
  await nextTick()
  await nextTick()
  return w
}

describe('PacPreviewModal 可指定文件 + 变量声明', () => {
  beforeEach(() => {
    previewMock.mockReset()
    updateProjectMock.mockReset()
  })

  it('打开弹窗不自动拉取:显示空态引导,不发请求', async () => {
    previewMock.mockResolvedValue(okResult)
    const w = mountModal()
    await nextTick()
    await nextTick()

    expect(previewMock).not.toHaveBeenCalled()
    expect(w.find('.pp-banner--idle').text()).toContain('拉取并校验')
    expect(w.find('.pp-banner--ok').exists()).toBe(false)
  })

  it('文件输入预填项目 pacFile;点击「拉取并校验」携带 ref 与 file', async () => {
    const w = await mountAndFetch('ci/pipeline.yml')
    expect((w.find('#pp-file').element as HTMLInputElement).value).toBe('ci/pipeline.yml')
    expect(previewMock).toHaveBeenCalledWith('p1', 'main', 'ci/pipeline.yml')
  })

  it('校验通过:渲染变量声明(徽标/描述/默认值)并 emit synced', async () => {
    const w = await mountAndFetch()

    const vars = w.findAll('.pp-var')
    expect(vars).toHaveLength(2)
    expect(vars[0].find('.pp-var-name').text()).toBe('DB_CONN')
    expect(vars[0].find('.pp-badge--req').exists()).toBe(true)
    expect(vars[0].find('.pp-badge--sec').exists()).toBe(true)
    expect(vars[1].find('.pp-var-default').text()).toContain('internal.example.com')
    expect(w.emitted('synced')).toBeTruthy()
  })

  it('校验失败:不 emit synced,展示错误详情', async () => {
    const w = await mountAndFetch('', { ...okResult, valid: false, error: 'pipelineyaml: invalid .pipewright.yml: 至少需要一个 stage' })

    expect(w.emitted('synced')).toBeFalsy()
    expect(w.find('.pp-error-detail').text()).toContain('至少需要一个 stage')
    expect(w.find('.pp-var').exists()).toBe(false)
  })

  it('拉取进行中输入框保持可编辑(改参数不受 in-flight 请求影响)', async () => {
    let resolveFetch!: (v: unknown) => void
    previewMock.mockReturnValue(new Promise((res) => { resolveFetch = res }))
    const w = mountModal()
    await nextTick()
    await w.find('.pp-btn--primary').trigger('click')
    await nextTick()

    // busy 进行中,两个输入框都不应带 disabled。
    expect(w.find('#pp-ref').attributes('disabled')).toBeUndefined()
    expect(w.find('#pp-file').attributes('disabled')).toBeUndefined()

    await w.find('#pp-file').setValue('ci/pipeline.yml')
    expect((w.find('#pp-file').element as HTMLInputElement).value).toBe('ci/pipeline.yml')

    resolveFetch(okResult)
    await nextTick()
    await nextTick()
    expect(w.find('.pp-banner--ok').exists()).toBe(true)
  })

  it('「设为默认」常驻:与项目默认一致时置灰;改动文件后可点并 PATCH pacFile + emit saved', async () => {
    const w = await mountAndFetch() // 项目默认 .pipewright.yml
    const btn = () => w.find('.pp-file-row .pp-btn')

    expect(btn().exists()).toBe(true)
    expect((btn().element as HTMLButtonElement).disabled).toBe(true) // 与默认一致 → 置灰

    previewMock.mockClear()
    await w.find('#pp-file').setValue('ci/pipeline.yml')
    await w.find('.pp-btn--primary').trigger('click')
    await nextTick()
    await nextTick()
    expect(previewMock).toHaveBeenCalledWith('p1', 'main', 'ci/pipeline.yml')

    expect((btn().element as HTMLButtonElement).disabled).toBe(false)
    await btn().trigger('click')
    await nextTick()
    await nextTick()

    expect(updateProjectMock).toHaveBeenCalledWith('p1', { pacFile: 'ci/pipeline.yml' })
    expect(w.emitted('saved')).toBeTruthy()
  })
})
