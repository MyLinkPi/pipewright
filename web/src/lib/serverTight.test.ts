import { describe, expect, it } from 'vitest'
import { isServerTight } from './serverTight'
import type { ServerMetrics } from '../api/servers'

function metrics(partial: Partial<ServerMetrics> = {}): ServerMetrics {
  return {
    serverId: 's1',
    reachable: true,
    error: '',
    cpu: { loadavg1: 0.5, cores: 4 },
    memory: { usedBytes: 40, usedWithCacheBytes: 50, totalBytes: 100, physicalTotalBytes: 0, swapUsedBytes: 0, swapTotalBytes: 0 },
    disk: { path: '/', usedBytes: 40, totalBytes: 100 },
    disks: [{ path: '/', usedBytes: 40, totalBytes: 100 }],
    gpu: null,
    collectedAt: '2026-10-22T00:00:00Z',
    ...partial,
  }
}

describe('isServerTight', () => {
  it('健康机器不紧张', () => {
    expect(isServerTight(metrics())).toBe(false)
  })

  it('CPU 负载相对核数 ≥ 100% 紧张(阈值含等于)', () => {
    expect(isServerTight(metrics({ cpu: { loadavg1: 4, cores: 4 } }))).toBe(true)
    expect(isServerTight(metrics({ cpu: { loadavg1: 3.99, cores: 4 } }))).toBe(false)
    // 无核数数据不判紧张。
    expect(isServerTight(metrics({ cpu: { loadavg1: 99, cores: null } }))).toBe(false)
    expect(isServerTight(metrics({ cpu: null }))).toBe(false)
  })

  it('内存 used/total ≥ 90% 紧张', () => {
    expect(isServerTight(metrics({ memory: { usedBytes: 90, usedWithCacheBytes: 95, totalBytes: 100, physicalTotalBytes: 0, swapUsedBytes: 0, swapTotalBytes: 0 } }))).toBe(true)
    expect(isServerTight(metrics({ memory: null }))).toBe(false)
  })

  it('任一磁盘 ≥ 90% 紧张;disks 缺失回退根盘', () => {
    expect(
      isServerTight(metrics({ disks: [{ path: '/', usedBytes: 10, totalBytes: 100 }, { path: '/data', usedBytes: 95, totalBytes: 100 }] })),
    ).toBe(true)
    expect(isServerTight(metrics({ disks: null, disk: { path: '/', usedBytes: 91, totalBytes: 100 } }))).toBe(true)
    expect(isServerTight(metrics({ disks: null, disk: null }))).toBe(false)
  })

  it('不可达或指标缺失不算紧张(没有数据就没有结论)', () => {
    expect(isServerTight(metrics({ reachable: false, cpu: null, memory: null, disk: null, disks: null }))).toBe(false)
    expect(isServerTight(null)).toBe(false)
    expect(isServerTight(undefined)).toBe(false)
  })
})
