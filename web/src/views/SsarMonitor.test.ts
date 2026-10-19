/**
 * SsarMonitor.test.ts — 机器监控·列表页回归测试。
 *
 * 核心回归:overview.binaries.packages 为 null(后端 nil 切片序列化)时页面不白屏
 * —— 归一化后按空数组消费,面板/表格照常渲染。另覆盖:点机器行 / 「详情 →」
 * 跳转独立详情路由(不再把详情藏在页底)。
 * i18n 插件与 zh-CN locale 由 src/test/setup.ts 全局安装(故断言中文文案)。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

const getSsarOverviewMock = vi.fn()
const getSsarHistoryMock = vi.fn()
const querySsarMock = vi.fn()
const triggerSsarDownloadMock = vi.fn()
const installSsarMock = vi.fn()
const pushMock = vi.fn()

vi.mock('../api/ssar', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/ssar')>()
  return {
    ...actual,
    getSsarOverview: (...a: unknown[]) => getSsarOverviewMock(...a),
    getSsarHistory: (...a: unknown[]) => getSsarHistoryMock(...a),
    querySsar: (...a: unknown[]) => querySsarMock(...a),
    triggerSsarDownload: (...a: unknown[]) => triggerSsarDownloadMock(...a),
    installSsar: (...a: unknown[]) => installSsarMock(...a),
  }
})

vi.mock('vue-router', () => ({
  useRoute: () => ({ params: {} }),
  useRouter: () => ({ push: pushMock }),
}))

import SsarMonitor from './SsarMonitor.vue'
import type { SsarOverview } from '../api/ssar'

function overview(over: Partial<SsarOverview> = {}): SsarOverview {
  return {
    binaries: {
      version: '',
      // @ts-expect-error 故意构造后端 nil 切片序列化出的 null(回归场景)
      packages: null,
      dir: '/var/lib/pipewright/bin/ssar',
      downloading: false,
      lastErr: '',
    },
    items: [
      {
        serverId: 's1',
        name: 'strata01',
        installed: false,
        version: '',
        collectorActive: false,
        lastSampleAt: '',
        error: '',
      },
      {
        serverId: 's2',
        name: 'docker01',
        installed: true,
        version: '1.0.5',
        collectorActive: true,
        lastSampleAt: '2026-10-19T12:00:00Z',
        error: '',
      },
    ],
    ...over,
  }
}

beforeEach(() => {
  vi.clearAllMocks()
  pushMock.mockReset()
  getSsarOverviewMock.mockResolvedValue(overview())
})

describe('SsarMonitor(列表页)', () => {
  it('packages 为 null 不白屏:归一化后渲染面板与服务器表', async () => {
    const wrapper = mount(SsarMonitor)
    await flushPromises()

    // 未缓存文案仍在(未被 null .length 异常打断渲染)。
    expect(wrapper.text()).toContain('尚未缓存安装包')
    // 两台服务器均渲染,含未装/已装两种徽标文案 + 每行都有详情入口。
    expect(wrapper.text()).toContain('strata01')
    expect(wrapper.text()).toContain('docker01')
    expect(wrapper.text()).toContain('未安装')
    expect(wrapper.text()).toContain('采集中')
    expect(wrapper.text()).toContain('详情 →')
    // 归一化后 packages 为数组(包名区不炸)。
    expect((wrapper.vm as unknown as { overview: SsarOverview }).overview.binaries.packages).toEqual([])
  })

  it('已缓存包时展示包名;点机器行与「详情 →」都跳转详情路由', async () => {
    getSsarOverviewMock.mockResolvedValue(
      overview({
        binaries: {
          version: 'v1.0.5',
          packages: ['ssar-1.0.5-1.x86_64.rpm', 'ssar_1.0.5-1_amd64.deb'],
          dir: '/var/lib/pipewright/bin/ssar',
          downloading: false,
          lastErr: '',
        },
      }),
    )

    const wrapper = mount(SsarMonitor)
    await flushPromises()

    // 包名与版本展示。
    expect(wrapper.text()).toContain('ssar-1.0.5-1.x86_64.rpm')
    expect(wrapper.text()).toContain('v1.0.5')

    // 点第二行(docker01)→ 跳转对应详情路由;页面本身不再渲染详情区。
    await wrapper.findAll('tbody tr').at(1)!.trigger('click')
    expect(pushMock).toHaveBeenCalledWith({ name: 'ssar-monitor-detail', params: { id: 's2' } })
    expect(wrapper.text()).not.toContain('细查视图')

    // 「详情 →」链接同样跳转。
    pushMock.mockClear()
    await wrapper.findAll('tbody tr a.link-btn').at(0)!.trigger('click')
    expect(pushMock).toHaveBeenCalledWith({ name: 'ssar-monitor-detail', params: { id: 's1' } })
  })
})
