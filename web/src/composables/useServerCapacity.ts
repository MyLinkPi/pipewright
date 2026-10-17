/**
 * useServerCapacity — 选机器时的「这台机现在多满」一行容量摘要。
 *
 * 应用商店部署、网关主机选择等需要挑目标机的地方,只看机器名/地址不够 —— 用户需要知道
 * 每台机已经跑了多少容器、负载/内存/磁盘还剩多少,才能把新应用放到合适的机器上。
 *
 * 数据复用两个既有批量只读端点,不新增后端契约:
 *   - GET /api/servers/containers(容器数;后端 30s TTL 缓存)
 *   - GET /api/servers/metrics(CPU/内存/磁盘;后端 stale-while-revalidate 快照缓存)
 * 两个端点热路径都是缓存回放,打开选择器不会触发全量 SSH 采集。
 *
 * 容量是**参考信息**:任一来源拉取失败都静默降级(不弹错、不阻塞选择器,能展示多少
 * 展示多少);数据年龄由后端 collectedAt 自描述,这里不做二次刷新轮询(选机器是低频
 * 动作,挂载时拉一次足够;部署等写操作会让后端缓存失效,重开页面即得新数据)。
 */
import { ref, type Ref } from 'vue'
import { getAllContainers, type ServerContainers } from '../api/containers'
import { getAllServerMetrics, type ServerMetrics } from '../api/servers'
import { t } from '../i18n'

/** 一台机器的容量行(为空串表示暂无可用信息,调用方不渲染)。 */
export type ServerCapacityLine = string

export interface ServerCapacity {
  /** 数据是否已到(未到时 capacityLine 一律返回空串,选项先显示、hint 后到)。 */
  loaded: Ref<boolean>
  /** 拉取两个来源(各自容错,失败得空表);挂载时调用一次。 */
  load: () => Promise<void>
  /** 单台机器的一行容量摘要,如「3 运行 / 5 容器 · 负载 0.42 / 8 核 · 内存 4.2 / 16 GiB · 磁盘 38%」。 */
  capacityLine: (serverId: string) => ServerCapacityLine
}

export function useServerCapacity(): ServerCapacity {
  const loaded = ref(false)
  const containersById = ref(new Map<string, ServerContainers>())
  const metricsById = ref(new Map<string, ServerMetrics>())

  async function load(): Promise<void> {
    const [cont, met] = await Promise.all([
      getAllContainers().catch(() => [] as ServerContainers[]),
      getAllServerMetrics().catch(() => [] as ServerMetrics[]),
    ])
    containersById.value = new Map(cont.map((c) => [c.serverId, c]))
    metricsById.value = new Map(met.map((m) => [m.serverId, m]))
    loaded.value = true
  }

  function capacityLine(serverId: string): ServerCapacityLine {
    const parts: string[] = []
    const c = containersById.value.get(serverId)
    const m = metricsById.value.get(serverId)

    if (c) {
      if (!c.reachable) parts.push(t('serverCapacity.unreachable'))
      else if (!c.runtime) parts.push(t('serverCapacity.noRuntime'))
      else parts.push(t('serverCapacity.containers', { running: c.running, total: c.total }))
    }
    if (m?.cpu && m.cpu.loadavg1 != null) {
      parts.push(
        t('serverCapacity.load', {
          load: m.cpu.loadavg1.toFixed(2),
          cores: m.cpu.cores != null ? String(m.cpu.cores) : '–',
        }),
      )
    }
    if (m?.memory && m.memory.totalBytes > 0) {
      // usedBytes 为进程真实占用口径(不含可回收页缓存),与服务器状态页「已用」一致。
      const { used, total, unit } = fmtBytePair(m.memory.usedBytes, m.memory.totalBytes)
      parts.push(t('serverCapacity.mem', { used, total, unit }))
    }
    if (m?.disk && m.disk.totalBytes > 0) {
      const pct = Math.round((m.disk.usedBytes / m.disk.totalBytes) * 100)
      parts.push(t('serverCapacity.disk', { pct }))
    }
    return parts.join(' · ')
  }

  return { loaded, load, capacityLine }
}

/** 把 used/total 按同一单位(total 决定单位)格式化为裸数值对 + 单位,如 (4.5, 16, GiB)。 */
function fmtBytePair(used: number, total: number): { used: string; total: string; unit: string } {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let exp = 0
  let u = total
  while (u >= 1024 && exp < units.length - 1) {
    u /= 1024
    exp++
  }
  const scale = (n: number): number => n / Math.pow(1024, exp)
  // ≥10 的值取整显示(16 而非 16.0,摘要行更紧凑),小值保留 1 位小数。
  const fmt = (n: number): string => (n >= 10 ? String(Math.round(n)) : n.toFixed(1))
  return { used: fmt(scale(used)), total: fmt(scale(total)), unit: units[exp] }
}
