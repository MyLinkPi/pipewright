/**
 * serverTight.ts — 「资源紧张」判定(服务器状态页筛选)。
 *
 * 阈值与 ServerMetricsCard 的红色着色口径一致(CPU >1×核数 偏红、内存/磁盘 ≥90% 红):
 *   · CPU:1 分钟负载相对核数 ≥ 100%;
 *   · 内存:usedBytes/totalBytes ≥ 90%(不含页缓存的「真实占用」口径,与卡片进度条一致);
 *   · 磁盘:任一真实磁盘 usedBytes/totalBytes ≥ 90%(disks 缺失回退根盘单值)。
 * 任一命中即紧张。不可达或指标缺失不算紧张(没有数据就没有结论,用可达性筛选去看它们)。
 */
import type { ServerMetrics } from '../api/servers'

/** CPU 负载相对核数的紧张阈值(1.0 = 负载打满全部核)。 */
export const TIGHT_CPU_RATIO = 1.0
/** 内存/磁盘用量百分比紧张阈值(与卡片进度条 error 变体同档)。 */
export const TIGHT_USAGE_PCT = 90

export function isServerTight(m: ServerMetrics | null | undefined): boolean {
  if (!m || !m.reachable) return false
  const cpu = m.cpu
  if (
    cpu &&
    cpu.cores !== null &&
    cpu.cores > 0 &&
    cpu.loadavg1 !== null &&
    cpu.loadavg1 / cpu.cores >= TIGHT_CPU_RATIO
  ) {
    return true
  }
  const mem = m.memory
  if (mem && mem.totalBytes > 0 && (mem.usedBytes / mem.totalBytes) * 100 >= TIGHT_USAGE_PCT) {
    return true
  }
  const disks = m.disks ?? (m.disk ? [m.disk] : [])
  for (const d of disks) {
    if (d.totalBytes > 0 && (d.usedBytes / d.totalBytes) * 100 >= TIGHT_USAGE_PCT) {
      return true
    }
  }
  return false
}
