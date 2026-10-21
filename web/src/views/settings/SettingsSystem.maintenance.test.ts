import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import SettingsSystem from './SettingsSystem.vue'

const api = vi.hoisted(() => ({
  version: { version: 'dev', commit: '', builtAt: '', platform: '', goVersion: '' },
  retention: {
    enabled: false,
    keepPerProject: 0,
    maxAgeDays: 0,
    cacheMaxGb: 10,
    imageKeepTags: 10,
  },
  cleanup: {
    target: 'local-temp',
    ok: true,
    items: 3,
    bytes: 2097152,
    steps: [
      '扫描目录 /tmp:仅匹配构建前缀且超过 48h 的条目',
      '删除 /tmp/pipewright-build-abc123',
    ],
    machines: [
      { serverId: 'srv-1', name: 'build-1', ok: true, command: '$ sh -c find /tmp/pipewright-remote /tmp/pipewright-shared -mindepth 1 -maxdepth 1 -mtime +2 -exec rm -rf {} +' },
    ],
  },
  systemConfig: {
    publicUrl: '',
    releaseMirror: '',
    webauthnRpId: '',
    webauthnOrigins: '',
    passkeyLoginEnabled: true,
    updatedAt: '',
    effectiveSource: { origin: 'default', apiBase: '', dlBase: '' },
  },
  setRetention: vi.fn(),
  runCleanup: vi.fn(),
}))
vi.mock('../../api/version', () => ({
  getVersion: vi.fn().mockResolvedValue(api.version),
  checkUpdate: vi.fn(),
  applyUpdate: vi.fn(),
  updateJobStatus: vi.fn(),
}))
vi.mock('../../api/retention', () => ({
  getRetentionConfig: vi.fn().mockResolvedValue({ ...api.retention }),
  setRetentionConfig: vi.fn().mockImplementation(async (input: typeof api.retention) => {
    api.setRetention(input)
    return { ...input }
  }),
}))
vi.mock('../../api/maintenance', () => ({
  runMaintenanceCleanup: vi.fn().mockImplementation(async (target: string) => {
    api.runCleanup(target)
    return { ...api.cleanup, target }
  }),
}))
vi.mock('../../api/systemConfig', () => ({
  getSystemConfig: vi.fn().mockResolvedValue({ ...api.systemConfig }),
  saveSystemConfig: vi.fn(),
}))

let w: VueWrapper | undefined
async function mountPanel() {
  w = mount(SettingsSystem)
  await flushPromises()
  return w
}
afterEach(() => {
  w?.unmount()
  vi.clearAllMocks()
})

describe('settings system disk governance', () => {
  it('renders the disk cleanup card with four targets', async () => {
    const wrap = await mountPanel()
    // 卡片标题 + 四个「立即清理」按钮。
    expect(wrap.text()).toContain('磁盘清理')
    const runButtons = wrap.findAll('button').filter((b) => b.text().includes('立即清理'))
    expect(runButtons.length).toBe(4)
  })

  it('runs cleanup via the API and shows the reclaimed summary', async () => {
    const wrap = await mountPanel()
    const runButtons = wrap.findAll('button').filter((b) => b.text().includes('立即清理'))
    await runButtons[0].trigger('click')
    await flushPromises()
    expect(api.runCleanup).toHaveBeenCalledOnce()
    // 结果文案包含条数与人读大小。
    expect(wrap.text()).toContain('已清理 3 项')
    expect(wrap.text()).toContain('2.0 MB')
  })

  it('shows the execution details: deleted paths and per-machine commands', async () => {
    const wrap = await mountPanel()
    const runButtons = wrap.findAll('button').filter((b) => b.text().includes('立即清理'))
    await runButtons[0].trigger('click')
    await flushPromises()
    // 执行明细可展开,删除路径逐条可见。
    expect(wrap.text()).toContain('执行明细')
    expect(wrap.text()).toContain('删除 /tmp/pipewright-build-abc123')
    // 每台机器实际执行的命令原文可见。
    expect(wrap.text()).toContain('build-1(srv-1)')
    expect(wrap.text()).toContain('find /tmp/pipewright-remote /tmp/pipewright-shared')
    expect(wrap.text()).toContain('-mtime +2')
  })

  it('saves the retention policy including the new disk fields', async () => {
    const wrap = await mountPanel()
    const inputs = wrap.findAll('input[type="number"]')
    // 表单顺序:语言无 → keepPerProject / maxAgeDays / cacheMaxGb / imageKeepTags。
    await inputs[2].setValue('20')
    await inputs[3].setValue('0')
    const save = wrap.findAll('button').find((b) => b.text().includes('保存保留策略'))
    await save!.trigger('click')
    await flushPromises()
    expect(api.setRetention).toHaveBeenCalledWith(
      expect.objectContaining({ cacheMaxGb: 20, imageKeepTags: 0, enabled: false }),
    )
  })
})
