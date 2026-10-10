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

describe('server metric card GPU row (opt-in per server)', () => {
  it('renders one block per card on GPU machines (multi-card)', () => {
    const w = mount(ServerMetricsCard, {
      props: { name: 'gpu-1', metrics: { ...metrics, gpu: nvGpu }, gpu: true },
    })
    const cards = w.findAll('.gpu-card')
    expect(cards).toHaveLength(2)
    expect(cards[0].get('.gpu-card__idx').text()).toBe('#0')
    expect(cards[0].get('.gpu-card__name').text()).toBe('NVIDIA CMP 40HX')
    // 两条带标签的进度条(利用率 / 显存)+ 显存字节行 + 遥测行。
    // 显存百分比按字节口径精算(7120289792 / 8589934592 ≈ 82.9% → 83%),与展示的字节一致。
    expect(cards[0].findAll('.gpu-bar')).toHaveLength(2)
    expect(cards[0].findAll('.gpu-bar__value').map((v) => v.text())).toEqual(['0%', '83%'])
    expect(cards[0].text()).toContain('8.0 GiB')
    expect(cards[1].text()).toContain('97')
    // 风扇为 null 的卡不多渲染该段:第 2 张卡的遥测行仍有温度/功耗。
    expect(cards[1].text()).toContain('61')
    w.unmount()
  })

  it('falls back to the mem_util percentage when the card reports no bytes (AMD)', () => {
    const w = mount(ServerMetricsCard, {
      props: { name: 'gpu-2', metrics: { ...metrics, gpu: amdGpu }, gpu: true },
    })
    const card = w.get('.gpu-card')
    expect(card.text()).toContain('AMD Instinct MI60 / MI50')
    expect(card.findAll('.gpu-bar')).toHaveLength(2)
    // 无字节口径 → 不渲染 used/total 行。
    expect(card.text()).not.toContain('GiB')
    expect(card.find('.gpu-card__telemetry').exists()).toBe(true)
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