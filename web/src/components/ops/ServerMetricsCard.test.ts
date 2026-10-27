import { describe, it, expect } from 'vitest'
import { mount } from '@vue/test-utils'
import ServerMetricsCard from './ServerMetricsCard.vue'
import type { GpuMetric, ServerMetrics } from '../../api/servers'

const metrics: ServerMetrics = {
  serverId: 'a',
  reachable: true,
  error: '',
  cpu: null,
  memory: null,
  disk: null,
  disks: null,
  gpu: null,
  collectedAt: '2026-10-07T00:00:00Z',
}

describe('server metric card actions', () => {
  it.each([true, false])(
    'keeps optional actions inside the card when reachable=%s',
    (reachable) => {
      const w = mount(ServerMetricsCard, {
        props: { name: 'Server A', metrics: { ...metrics, reachable } },
        slots: { actions: '<button>AI assistant</button>' },
      })
      expect(w.get('article .metrics-card__actions button').text()).toBe(
        'AI assistant',
      )
      expect(w.findAll('.metrics-card__actions')).toHaveLength(1)
      w.unmount()
    },
  )
  it('does not add an empty actions area to other uses', () => {
    const w = mount(ServerMetricsCard, { props: { name: 'Server A', metrics } })
    expect(w.find('.metrics-card__actions').exists()).toBe(false)
    w.unmount()
  })
})

/** 单卡:有显存字节口径(NVIDIA)与只有百分比(AMD)两种形态。 */
const nvGpu: GpuMetric = {
  devices: [
    {
      index: 0,
      name: 'NVIDIA CMP 40HX',
      gpuUtil: 0,
      memUtil: 82,
      memTotalBytes: 8589934592,
      memUsedBytes: 7120289792,
      memFreeBytes: 1469644800,
      tempC: 37,
      fanSpeedPct: 36,
      powerDrawW: 13,
      gpuClockMhz: 300,
      memClockMhz: 405,
      encodeUtil: 0,
      decodeUtil: 0,
    },
    {
      index: 1,
      name: 'NVIDIA CMP 40HX',
      gpuUtil: 97,
      memUtil: 41,
      memTotalBytes: 8589934592,
      memUsedBytes: 3520289792,
      memFreeBytes: 5069644800,
      tempC: 61,
      fanSpeedPct: null,
      powerDrawW: 95,
      gpuClockMhz: 1560,
      memClockMhz: 1000,
      encodeUtil: 12,
      decodeUtil: 0,
    },
  ],
}

const amdGpu: GpuMetric = {
  devices: [
    {
      index: 0,
      name: 'AMD Instinct MI60 / MI50',
      gpuUtil: 0,
      memUtil: 0,
      memTotalBytes: null,
      memUsedBytes: null,
      memFreeBytes: null,
      tempC: 44,
      fanSpeedPct: 14,
      powerDrawW: 19,
      gpuClockMhz: 926,
      memClockMhz: 350,
      encodeUtil: null,
      decodeUtil: null,
    },
  ],
}

describe('server metric card disk rows (all real disks)', () => {
  it('renders one labeled row per real disk with its own usage bar', () => {
    const w = mount(ServerMetricsCard, {
      props: {
        name: 'srv-1',
        metrics: {
          ...metrics,
          disk: { path: '/', usedBytes: 100, totalBytes: 1000 },
          disks: [
            { path: '/', usedBytes: 100, totalBytes: 1000 },
            { path: '/data', usedBytes: 600, totalBytes: 2000 },
          ],
        },
      },
    })
    // 系统盘 + 数据盘各一行(标签含挂载点;tmpfs/nfs 等伪文件系统后端已剔除不进前端)。
    const labels = w.findAll('.metric-row__label').map((l) => l.text())
    const diskLabels = labels.filter((x) => x.includes('/'))
    expect(diskLabels).toHaveLength(2)
    expect(diskLabels.some((x) => x.includes('/data'))).toBe(true)
    // 每块盘独立渲染百分比与 used/total(600/2000 = 30%;2000B → 2.0 KiB)。
    expect(w.text()).toContain('30%')
    expect(w.text()).toContain('2.0 KiB')
    w.unmount()
  })

  it('shows a single unavailable disk row when collection failed', () => {
    const w = mount(ServerMetricsCard, { props: { name: 'srv-2', metrics } })
    const labels = w.findAll('.metric-row__label').map((l) => l.text())
    expect(labels.filter((x) => x.includes('/'))).toHaveLength(1)
    w.unmount()
  })
})

describe('server metric card GPU row (opt-in per server)', () => {
  it('renders one block per card on GPU machines (multi-card)', () => {
    const w = mount(ServerMetricsCard, {
      props: { name: 'gpu-1', metrics: { ...metrics, gpu: nvGpu }, gpu: true },
    })
    const cards = w.findAll('.gpu-card')
    expect(cards).toHaveLength(2)
    expect(cards[0].get('.gpu-card__idx').text()).toBe('#0')
    expect(cards[0].get('.gpu-card__name').text()).toBe('NVIDIA CMP 40HX')
    // 三行同构单行进度条(利用率 / 显存 / 风扇);显存大小不展示,只留百分比,行尾频率裸 MHz 靠右。
    // 显存百分比仍按字节口径精算(7120289792 / 8589934592 ≈ 82.9% → 83%)。
    expect(cards[0].findAll('.gpu-bar')).toHaveLength(3)
    expect(cards[0].findAll('.gpu-bar__value').map((v) => v.text())).toEqual(['0%', '83%', '36%'])
    expect(cards[0].text()).not.toContain('GiB')
    expect(cards[0].text()).toContain('300MHz')
    expect(cards[0].text()).toContain('405MHz')
    expect(cards[1].text()).toContain('97')
    // 风扇为 null 的卡不多渲染该段:第 2 张卡仍有温度/功耗读数。
    expect(cards[1].text()).toContain('61')
    w.unmount()
  })

  it('falls back to the mem_util percentage when the card reports no bytes (AMD)', () => {
    const w = mount(ServerMetricsCard, {
      props: { name: 'gpu-2', metrics: { ...metrics, gpu: amdGpu }, gpu: true },
    })
    const card = w.get('.gpu-card')
    expect(card.text()).toContain('AMD Instinct MI60 / MI50')
    expect(card.findAll('.gpu-bar')).toHaveLength(3)
    // 无字节口径 → 不渲染 used/total 读数。
    expect(card.text()).not.toContain('GiB')
    // 频率裸 MHz(不带「核心/显存」前缀),温度/功耗在行尾。
    expect(card.text()).toContain('926MHz')
    expect(card.text()).toContain('350MHz')
    expect(card.text()).toContain('44')
    w.unmount()
  })

  it('shows unavailable when a GPU machine reports no cards (nvtop missing)', () => {
    const w = mount(ServerMetricsCard, {
      props: { name: 'gpu-3', metrics: { ...metrics, gpu: null }, gpu: true },
    })
    expect(w.find('.gpu-card').exists()).toBe(false)
    // 不渲染裸 key / 不报错,只给出「不可用」文案。
    expect(w.text()).not.toContain('opsServer.metrics.gpuUnavailable')
    w.unmount()
  })

  it('does not render the GPU row on non-GPU servers, even if cards came back', () => {
    const w = mount(ServerMetricsCard, {
      props: { name: 'plain-1', metrics: { ...metrics, gpu: nvGpu }, gpu: false },
    })
    expect(w.find('.gpu-card').exists()).toBe(false)
    w.unmount()
  })
})