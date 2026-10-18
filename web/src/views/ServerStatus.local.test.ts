/**
 * ServerStatus.local.test.ts — /server-status「本机(控制节点)」卡牌回归测试。
 *
 * 覆盖:本机指标渲染为置首的只读卡牌、与已登记服务器卡并存;本机请求失败只隐藏本机卡、
 * 不拖垮服务器卡;无已登记服务器时仍以本机卡替代空态。
 * i18n 插件与 zh-CN locale 由 src/test/setup.ts 全局安装(故断言中文文案)。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'

const listServersMock = vi.fn()
const getAllServerMetricsMock = vi.fn()
const getLocalHostMetricsMock = vi.fn()

vi.mock('../api/servers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/servers')>()
  return {
    ...actual,
    listServers: (...a: unknown[]) => listServersMock(...a),
    getAllServerMetrics: (...a: unknown[]) => getAllServerMetricsMock(...a),
    getLocalHostMetrics: (...a: unknown[]) => getLocalHostMetricsMock(...a),
  }
})

import ServerStatus from './ServerStatus.vue'
import { LOCAL_HOST_ID } from '../api/servers'
import type { Server, ServerMetrics } from '../api/servers'

function server(id: string, name: string): Server {
  return {
    id,
    name,
    host: '10.0.0.1',
    port: 22,
    user: 'root',
    credentialId: 'c-1',
    credentialName: 'cred',
    sudoCredentialId: '',
    sudoCredentialName: '',
    jumps: [],
    labels: '',
    maxBuilds: 0,
    priority: 0,
    gpu: false,
    poolId: 'default',
    poolName: 'default',
    kind: 'standard',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  }
}

function metrics(serverId: string, over: Partial<ServerMetrics> = {}): ServerMetrics {
  return {
    serverId,
    reachable: true,
    error: '',
    cpu: { loadavg1: 0.5, cores: 4 },
    memory: {
      usedBytes: 1024,
      usedWithCacheBytes: 2048,
      totalBytes: 8192,
      physicalTotalBytes: 0,
      swapUsedBytes: 0,
      swapTotalBytes: 0,
    },
    disk: { path: '/', usedBytes: 100, totalBytes: 1000 },
    disks: [{ path: '/', usedBytes: 100, totalBytes: 1000 }],
    gpu: null,
    collectedAt: '2026-10-11T00:00:00Z',
    ...over,
  }
}

async function mountStatus(): Promise<VueWrapper> {
  const w = mount(ServerStatus)
  await flushPromises()
  return w
}

beforeEach(() => {
  vi.clearAllMocks()
  listServersMock.mockResolvedValue([server('s1', 'web-1')])
  getAllServerMetricsMock.mockResolvedValue([metrics('s1')])
  getLocalHostMetricsMock.mockResolvedValue(metrics(LOCAL_HOST_ID))
})

describe('ServerStatus · 本机(控制节点)卡牌', () => {
  it('渲染置首的本机卡,并与服务器卡并存;本机卡不可勾选', async () => {
    const w = await mountStatus()
    const cards = w.findAll('.metrics-card')
    expect(cards).toHaveLength(2)
    expect(cards[0].text()).toContain('本机(控制节点)')
    expect(cards[1].text()).toContain('web-1')
    // 只读:本机卡没有批量命令复选框(它不是登记服务器)。
    expect(cards[0].find('input[type="checkbox"]').exists()).toBe(false)
    expect(cards[1].find('input[type="checkbox"]').exists()).toBe(true)
    w.unmount()
  })

  it('本机采集失败(reachable:false)时卡片显示人读错误,不影响服务器卡', async () => {
    getLocalHostMetricsMock.mockResolvedValue(
      metrics(LOCAL_HOST_ID, { reachable: false, cpu: null, memory: null, disk: null, error: '本机不可用:缺少 sh 命令' }),
    )
    const w = await mountStatus()
    const cards = w.findAll('.metrics-card')
    expect(cards).toHaveLength(2)
    expect(cards[0].text()).toContain('本机不可用:缺少 sh 命令')
    expect(cards[1].text()).toContain('web-1')
    w.unmount()
  })

  it('本机请求失败只隐藏本机卡,服务器卡照常展示', async () => {
    getLocalHostMetricsMock.mockRejectedValue(new Error('boom'))
    const w = await mountStatus()
    const cards = w.findAll('.metrics-card')
    expect(cards).toHaveLength(1)
    expect(cards[0].text()).toContain('web-1')
    w.unmount()
  })

  it('无已登记服务器时仍展示本机卡,而非空态', async () => {
    listServersMock.mockResolvedValue([])
    getAllServerMetricsMock.mockResolvedValue([])
    const w = await mountStatus()
    const cards = w.findAll('.metrics-card')
    expect(cards).toHaveLength(1)
    expect(cards[0].text()).toContain('本机(控制节点)')
    expect(w.text()).not.toContain('尚无已登记服务器')
    w.unmount()
  })
})
