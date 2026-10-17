/**
 * serverCapacity — 选机器时的单行容量摘要(应用商店部署、网关主机选择等)。
 * 各部分按数据可用性拼装,分隔符固定为「 · 」。
 */
export default {
  containers: '{running} 运行 / {total} 容器',
  noRuntime: '无容器运行时',
  unreachable: '不可达',
  load: '负载 {load} / {cores} 核',
  mem: '内存 {used} / {total} {unit}',
  disk: '磁盘 {pct}%',
}
