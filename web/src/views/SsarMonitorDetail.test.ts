/**
 * SsarMonitorDetail.test.ts — 机器监控·单机详情页测试。
 *
 * 覆盖:进入即拉取 overview(取机器名/状态)+ 平台采样历史 + 细查;图表与细查
 * 表格渲染;未安装机器显示提示面板;返回链接跳回列表路由。
 * i18n 插件与 zh-CN locale 由 src/test/setup.ts 全局安装(故断言中文文案)。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

const getSsarOverviewMock = vi.fn()
const getSsarHistoryMock = vi.fn()
const querySsarMock = vi.fn()
const pushMock = vi.fn()

vi.mock('../api/ssar', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/ssar')>()
  return {
    ...actual,
    getSsarOverview: (...a: unknown[]) => getSsarOverviewMock(...a),
    getSsarHistory: (...a: unknown[]) => getSsarHistoryMock(...a),
    querySsar: (...a: unknown[]) => querySsarMock(...a),
  }
})

vi.mock('vue-router', () => ({
  useRoute: () => ({ params: { id: 's2' } }),
  useRouter: () => ({ push: pushMock }),
}))

import SsarMonitorDetail from './SsarMonitorDetail.vue'

beforeEach(() => {
  vi.clearAllMocks()
  pushMock.mockReset()
  getSsarOverviewMock.mockResolvedValue({
    binaries: { version: 'v1.0.5', packages: [], dir: '', downloading: false, lastErr: '' },
    items: [
      { serverId: 's1', name: 'strata01', installed: false, version: '', collectorActive: false, lastSampleAt: '', error: '' },
      { serverId: 's2', name: 'docker01', installed: true, version: '1.0.5', collectorActive: true, lastSampleAt: '2026-10-19T12:00:00Z', error: '' },
    ],
  })
  getSsarHistoryMock.mockResolvedValue([
    {
      at: '2026-10-19T11:59:00Z',
      cpuUser: 11.5, cpuSystem: 4.2, cpuIowait: 2.1, cpuIdle: 840.1, cpuSteal: 0.5,
      memTotalKb: 16384000, memFreeKb: 2000000, memAvailKb: 8000000,
      load1: 0.5, runq: 2, threads: 27600,
      tcpCurrEstab: 42, tcpRetransSegs: 0.35, tcpSynRetrans: 0.1, tcpFastRetrans: 0.2, tcpRetransFail: null,
    },
  ])
  querySsarMock.mockResolvedValue({
    view: 'cpu',
    columns: [{ key: 'user', unit: 'jiffies/s' }],
    rows: [{ time: '2026-10-19T12:00:00', values: { user: 1.5 } }],
  })
})

describe('SsarMonitorDetail(详情页)', () => {
  it('进入即拉取状态/历史/细查,渲染图表、表格与返回链接', async () => {
    const wrapper = mount(SsarMonitorDetail)
    await flushPromises()

    // 机器名 + 采集中徽标 + 最近采样时间。
    expect(wrapper.text()).toContain('docker01')
    expect(wrapper.text()).toContain('采集中')
    expect(wrapper.text()).toContain('最近采样')
    // 三类数据各拉一次:overview(状态)、history(默认 24h)、query(默认 cpu)。
    expect(getSsarOverviewMock).toHaveBeenCalledTimes(1)
    expect(getSsarHistoryMock).toHaveBeenCalledWith('s2', 24)
    expect(querySsarMock).toHaveBeenCalledWith({ serverId: 's2', view: 'cpu', minutes: 60, step: 1 })
    // 四张总览图表标题 + 细查表格(列头带单位)。
    expect(wrapper.text()).toContain('CPU(jiffies/s,全 CPU 汇总)')
    expect(wrapper.text()).toContain('内存已用 %(含页缓存口径)')
    expect(wrapper.text()).toContain('负载与运行队列')
    expect(wrapper.text()).toContain('TCP 连接与重传')
    expect(wrapper.text()).toContain('user (jiffies/s)')
    expect(wrapper.text()).toContain('1.5')
    // 返回链接跳回列表。
    await wrapper.find('a.back-link').trigger('click')
    expect(pushMock).toHaveBeenCalledWith({ name: 'ssar-monitor' })
  })

  it('未安装机器显示提示面板', async () => {
    getSsarOverviewMock.mockResolvedValue({
      binaries: { version: 'v1.0.5', packages: [], dir: '', downloading: false, lastErr: '' },
      items: [
        { serverId: 's1', name: 'strata01', installed: false, version: '', collectorActive: false, lastSampleAt: '', error: '' },
        { serverId: 's2', name: 'docker01', installed: false, version: '', collectorActive: false, lastSampleAt: '', error: '' },
      ],
    })
    const wrapper = mount(SsarMonitorDetail)
    await flushPromises()

    expect(wrapper.find('.hint-panel').exists()).toBe(true)
    expect(wrapper.text()).toContain('尚未安装 ssar')
  })
})
