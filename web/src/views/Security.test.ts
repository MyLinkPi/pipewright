/**
 * Security.test.ts — 安全中心页。
 *
 * 覆盖:概览 KPI 与服务器表渲染(徽标计数/未扫描占位);单机 Trivy 扫描触发 startScan
 * 并落提示;定时规则保存(saveSchedule 参数含启用/表达式/类型);导出报告按钮调用
 * downloadSecurityReport;漏洞明细弹窗打开 + 严重度过滤联动 getFindings。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'

const h = vi.hoisted(() => ({
  getOverview: vi.fn(),
  getSchedule: vi.fn(),
  saveSchedule: vi.fn(),
  listScanners: vi.fn(),
  createScanner: vi.fn(),
  deleteScanner: vi.fn(),
  testScanner: vi.fn(),
  testScannerDirect: vi.fn(),
  startScan: vi.fn(),
  startFleetScan: vi.fn(),
  listRuns: vi.fn(),
  getFindings: vi.fn(),
  downloadSecurityReport: vi.fn(),
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warn: vi.fn() },
  confirmOpen: vi.fn(),
}))

vi.mock('../api/security', () => ({
  getOverview: (...a: unknown[]) => h.getOverview(...a),
  getSchedule: (...a: unknown[]) => h.getSchedule(...a),
  saveSchedule: (...a: unknown[]) => h.saveSchedule(...a),
  listScanners: (...a: unknown[]) => h.listScanners(...a),
  createScanner: (...a: unknown[]) => h.createScanner(...a),
  deleteScanner: (...a: unknown[]) => h.deleteScanner(...a),
  testScanner: (...a: unknown[]) => h.testScanner(...a),
  testScannerDirect: (...a: unknown[]) => h.testScannerDirect(...a),
  startScan: (...a: unknown[]) => h.startScan(...a),
  startFleetScan: (...a: unknown[]) => h.startFleetScan(...a),
  listRuns: (...a: unknown[]) => h.listRuns(...a),
  getFindings: (...a: unknown[]) => h.getFindings(...a),
  downloadSecurityReport: (...a: unknown[]) => h.downloadSecurityReport(...a),
}))
vi.mock('../composables/useToast', () => ({
  useToast: () => ({ ...h.toast, toasts: { value: [] }, dismiss: vi.fn(), clear: vi.fn() }),
}))
vi.mock('../composables/useConfirm', () => ({
  useConfirm: () => ({ open: h.confirmOpen, pending: { value: null }, _resolve: vi.fn() }),
}))

import Security from './Security.vue'
import AppSelect from '../components/ui/AppSelect.vue'
import type { SecurityOverview, SecRun } from '../api/security'

const summary = (over: Partial<Record<string, number>> = {}) => ({
  critical: 1, high: 2, medium: 3, low: 4, info: 0, unknown: 0, total: 10, ...over,
})

const mkOverview = (): SecurityOverview => ({
  servers: [
    {
      serverId: 's1', serverName: '机器A', host: '10.0.0.1',
      trivy: { runId: 'r1', scanType: 'trivy', status: 'done', summary: summary(), finishedAt: '2026-10-24T00:00:00Z' },
      nuclei: { runId: 'r2', scanType: 'nuclei', status: 'running', summary: summary({ total: 0, critical: 0, high: 0, medium: 0, low: 0 }) },
      totals: summary(),
    },
    { serverId: 's2', serverName: '机器B', host: '10.0.0.2' },
  ],
  totals: summary(),
  scanned: 1,
  serverTotal: 2,
  lastScanAt: '2026-10-24T00:00:00Z',
})

beforeEach(() => {
  vi.clearAllMocks()
  h.getOverview.mockResolvedValue(mkOverview())
  h.listRuns.mockResolvedValue({ items: [] as SecRun[] })
  h.getSchedule.mockResolvedValue({ enabled: false, expression: '0 3 * * *', scanTypes: ['trivy'], updatedAt: '' })
  h.listScanners.mockResolvedValue({ items: [] })
})

describe('Security 安全面板', () => {
  it('渲染 KPI 与服务器表:严重度计数、扫描中徽标、未扫描占位', async () => {
    const w = mount(Security)
    await flushPromises()

    expect(w.text()).toContain('安全中心')
    expect(w.text()).toContain('C 1 · H 2 · M 3 · L 4')
    expect(w.text()).toContain('机器A')
    expect(w.text()).toContain('机器B')
    expect(w.text()).toContain('扫描中')
    expect(w.text()).toContain('未扫描')
    // 有 running 任务 → 提示自动刷新。
    expect(w.text()).toContain('自动刷新')
  })

  it('点「Trivy 扫描」→ startScan 携带 serverId/scanType,成功提示', async () => {
    h.startScan.mockResolvedValue({ id: 'r3', serverId: 's1', serverName: '机器A', scanType: 'trivy', status: 'running', summary: summary({ total: 0 }), startedAt: '' })
    const w = mount(Security)
    await flushPromises()

    const btn = w.findAll('button').find((b) => b.text().includes('Trivy 扫描'))!
    await btn.trigger('click')
    await flushPromises()

    expect(h.startScan).toHaveBeenCalledWith({ serverId: 's1', scanType: 'trivy' })
    expect(h.toast.success).toHaveBeenCalled()
  })

  it('定时规则保存:表达式取预设映射,类型来自勾选;toast 成功', async () => {
    h.saveSchedule.mockResolvedValue({ enabled: true, expression: '0 5 * * *', scanTypes: ['trivy', 'nuclei'], updatedAt: '' })
    const w = mount(Security)
    await flushPromises()

    // 勾选 nuclei + 切预设到「每天 06:00」再保存。
    const nucleiBox = w.findAll('input[type="checkbox"]').find((c) => (c.element as HTMLInputElement).closest('label')?.textContent?.includes('Nuclei'))!
    await nucleiBox.setValue(true)
    const saveBtn = w.findAll('button').find((b) => b.text().includes('保存规则'))!
    await saveBtn.trigger('click')
    await flushPromises()

    expect(h.saveSchedule).toHaveBeenCalledWith({ enabled: false, expression: '0 3 * * *', scanTypes: ['trivy', 'nuclei'] })
    expect(h.toast.success).toHaveBeenCalled()
  })

  it('定时保存空类型被前端拦截(不发请求)', async () => {
    const w = mount(Security)
    await flushPromises()
    // 取消 trivy 勾选(默认唯一类型)→ 保存 → 前端报错,不调 API。
    const trivyBox = w.findAll('input[type="checkbox"]').find((c) => (c.element as HTMLInputElement).closest('label')?.textContent?.trim() === 'Trivy')!
    await trivyBox.setValue(false)
    const saveBtn = w.findAll('button').find((b) => b.text().includes('保存规则'))!
    await saveBtn.trigger('click')
    await flushPromises()
    expect(h.saveSchedule).not.toHaveBeenCalled()
    expect(h.toast.error).toHaveBeenCalled()
  })

  it('「导出报告」调用 downloadSecurityReport', async () => {
    h.downloadSecurityReport.mockResolvedValue('pipewright-security-report-x.html')
    const w = mount(Security)
    await flushPromises()
    await w.findAll('button').find((b) => b.text().includes('导出报告'))!.trigger('click')
    await flushPromises()
    expect(h.downloadSecurityReport).toHaveBeenCalledTimes(1)
    expect(h.toast.success).toHaveBeenCalled()
  })

  it('查看明细 → 弹窗打开,getFindings 随严重度过滤联动', async () => {
    h.getFindings.mockResolvedValue({ total: 1, items: [{ severity: 'high', source: 'host', target: 'openssl', id: 'CVE-1', url: 'https://x' }], summary: summary() })
    const w = mount(Security)
    await flushPromises()

    await w.findAll('button').find((b) => b.text().includes('查看明细'))!.trigger('click')
    await flushPromises()
    expect(h.getFindings).toHaveBeenCalledWith('r1', expect.objectContaining({ severity: '' }))
    expect(w.text()).toContain('CVE-1')

    // 切换严重度过滤(AppSelect 声明的事件名为 update:modelValue;顺序:预设 → 严重度 → 来源)。
    const severitySelect = w.findAllComponents(AppSelect)[1]!
    ;(severitySelect.vm as unknown as { $emit: (e: string, v: string) => void }).$emit('update:modelValue', 'high')
    await nextTick()
    await flushPromises()
    expect(h.getFindings).toHaveBeenLastCalledWith('r1', expect.objectContaining({ severity: 'high', offset: 0 }))
  })
})
