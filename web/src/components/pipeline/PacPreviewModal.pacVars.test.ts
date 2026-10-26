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

describe('PacPreviewModal 可指定文件 + 变量声明', () => {
  beforeEach(() => {
    previewMock.mockReset()
    updateProjectMock.mockReset()
  })

  it('文件输入预填项目 pacFile;拉取并校验携带 ref 与 file', async () => {
    previewMock.mockResolvedValue(okResult)
    const w = mountModal('ci/pipeline.yml')
    await nextTick()
    await nextTick()

    expect((w.find('#pp-file').element as HTMLInputElement).value).toBe('ci/pipeline.yml')
    expect(previewMock).toHaveBeenCalledWith('p1', 'main', 'ci/pipeline.yml')
  })

  it('校验通过:渲染变量声明(徽标/描述/默认值)并 emit synced', async () => {
    previewMock.mockResolvedValue(okResult)
    const w = mountModal()
    await nextTick()
    await nextTick()

    const vars = w.findAll('.pp-var')
    expect(vars).toHaveLength(2)
    expect(vars[0].find('.pp-var-name').text()).toBe('DB_CONN')
    expect(vars[0].find('.pp-badge--req').exists()).toBe(true)
    expect(vars[0].find('.pp-badge--sec').exists()).toBe(true)
    expect(vars[1].find('.pp-var-default').text()).toContain('internal.example.com')
    expect(w.emitted('synced')).toBeTruthy()
  })

  it('校验失败:不 emit synced,展示错误详情', async () => {
    previewMock.mockResolvedValue({ ...okResult, valid: false, error: 'pipelineyaml: invalid .pipewright.yml: 至少需要一个 stage' })
    const w = mountModal()
    await nextTick()
    await nextTick()

    expect(w.emitted('synced')).toBeFalsy()
    expect(w.find('.pp-error-detail').text()).toContain('至少需要一个 stage')
    expect(w.find('.pp-var').exists()).toBe(false)
  })

  it('临时文件 ≠ 项目默认时显示「设为默认」,点击后 PATCH pacFile 并 emit saved', async () => {
    previewMock.mockResolvedValue({ ...okResult, file: 'ci/pipeline.yml' })
    updateProjectMock.mockResolvedValue({})
    const w = mountModal() // 项目默认 .pipewright.yml
    await nextTick()
    await nextTick()

    const fileInput = w.find('#pp-file')
    await fileInput.setValue('ci/pipeline.yml')
    // 换文件后重新校验一次,让 result.file 与输入一致。
    previewMock.mockClear()
    await w.find('.pp-btn--primary').trigger('click')
    await nextTick()
    await nextTick()
    expect(previewMock).toHaveBeenCalledWith('p1', 'main', 'ci/pipeline.yml')

    expect(w.find('.pp-file-row').exists()).toBe(true)
    await w.find('.pp-file-row .pp-btn').trigger('click')
    await nextTick()
    await nextTick()

    expect(updateProjectMock).toHaveBeenCalledWith('p1', { pacFile: 'ci/pipeline.yml' })
    expect(w.emitted('saved')).toBeTruthy()
  })

  it('文件与项目默认一致时不显示「设为默认」', async () => {
    previewMock.mockResolvedValue(okResult)
    const w = mountModal()
    await nextTick()
    await nextTick()
    expect(w.find('.pp-file-row').exists()).toBe(false)
  })
})
