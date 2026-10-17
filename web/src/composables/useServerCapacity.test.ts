/**
 * useServerCapacity.test.ts — 选机器容量摘要 composable。
 *
 * 覆盖:数据到位时逐段拼装(容器数/负载/内存/磁盘,「 · 」连接);部分数据缺失只
 * 展示可用段;不可达/无运行时分支;两个数据源任一失败静默降级(空行,不抛错)。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { setLocale } from '../i18n'
import { useServerCapacity } from './useServerCapacity'
import { getAllContainers, type ServerContainers } from '../api/containers'
import { getAllServerMetrics, type ServerMetrics } from '../api/servers'

vi.mock('../api/containers', () => ({ getAllContainers: vi.fn() }))
vi.mock('../api/servers', () => ({ getAllServerMetrics: vi.fn() }))

const cont = vi.mocked(getAllContainers)
const met = vi.mocked(getAllServerMetrics)

const GiB = 1024 ** 3

function mem(usedBytes: number, totalBytes: number) {
  return {
    usedBytes,
    usedWithCacheBytes: usedBytes,
    totalBytes,
    physicalTotalBytes: totalBytes,
    swapUsedBytes: 0,
    swapTotalBytes: 0,
  }
}

function containers(over: Partial<ServerContainers>): ServerContainers {
  return {
    serverId: 's1', reachable: true, runtime: 'docker', error: '',
    containers: [], running: 0, total: 0, collectedAt: '',
    ...over,
  }
}

function metrics(over: Partial<ServerMetrics>): ServerMetrics {
  return {
    serverId: 's1', reachable: true, error: '',
    cpu: null, memory: null, disk: null, disks: null, gpu: null, collectedAt: '',
    ...over,
  }
}

beforeEach(() => {
  vi.clearAllMocks()
  setLocale('zh-CN')
  cont.mockResolvedValue([])
  met.mockResolvedValue([])
})

async function loaded() {
  const c = useServerCapacity()
  await c.load()
  return c
}

describe('useServerCapacity 容量行拼装', () => {
  it('数据齐全 → 容器数/负载/内存/磁盘四段以「 · 」连接', async () => {
    cont.mockResolvedValue([containers({ serverId: 's1', running: 3, total: 5 })])
    met.mockResolvedValue([
      metrics({
        serverId: 's1',
        cpu: { loadavg1: 0.42, cores: 8 },
        memory: mem(4.5 * GiB, 16 * GiB),
        disk: { path: '/', usedBytes: 380, totalBytes: 1000 },
      }),
    ])
    const c = await loaded()
    expect(c.capacityLine('s1')).toBe('3 运行 / 5 容器 · 负载 0.42 / 8 核 · 内存 4.5 / 16 GiB · 磁盘 38%')
    expect(c.loaded.value).toBe(true)
  })

  it('部分数据缺失 → 只展示可用段;完全无数据 → 空串', async () => {
    met.mockResolvedValue([
      metrics({ serverId: 's1', cpu: { loadavg1: 1.2, cores: 4 }, memory: null, disk: null }),
    ])
    const c = await loaded()
    expect(c.capacityLine('s1')).toBe('负载 1.20 / 4 核')
    expect(c.capacityLine('unknown')).toBe('')
  })

  it('容器不可达 → 提示不可达;连得上但无运行时 → 提示无运行时', async () => {
    cont.mockResolvedValue([
      containers({ serverId: 's1', reachable: false, runtime: '', error: 'x' }),
      containers({ serverId: 's2', reachable: true, runtime: '', error: 'x' }),
    ])
    const c = await loaded()
    expect(c.capacityLine('s1')).toBe('不可达')
    expect(c.capacityLine('s2')).toBe('无容器运行时')
  })

  it('任一数据源失败 → 静默降级,不抛错;未加载完成 → 空串', async () => {
    cont.mockRejectedValue(new Error('boom'))
    met.mockResolvedValue([metrics({ serverId: 's1', cpu: { loadavg1: 0.1, cores: 2 } })])
    const c = await loaded()
    expect(c.capacityLine('s1')).toBe('负载 0.10 / 2 核')

    cont.mockRejectedValue(new Error('boom'))
    met.mockRejectedValue(new Error('boom'))
    const both = await loaded()
    expect(both.capacityLine('s1')).toBe('')

    const pending = useServerCapacity()
    expect(pending.capacityLine('s1')).toBe('')
    expect(pending.loaded.value).toBe(false)
  })

  it('内存/磁盘 total 为 0 或 cpu 缺 loadavg → 跳过该段(不产生 NaN)', async () => {
    met.mockResolvedValue([
      metrics({
        serverId: 's1',
        cpu: { loadavg1: null, cores: 8 },
        memory: mem(100, 0),
        disk: { path: '/', usedBytes: 5, totalBytes: 0 },
      }),
    ])
    const c = await loaded()
    expect(c.capacityLine('s1')).toBe('')
  })
})
