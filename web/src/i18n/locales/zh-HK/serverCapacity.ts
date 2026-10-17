/**
 * serverCapacity — 選機器時的單行容量摘要(應用商店部署、閘道主機選擇等)。
 * 各部分按數據可用性拼裝,分隔符固定為「 · 」。
 */
export default {
  containers: '{running} 運行 / {total} 容器',
  noRuntime: '無容器運行時',
  unreachable: '不可達',
  load: '負載 {load} / {cores} 核',
  mem: '記憶體 {used} / {total} {unit}',
  disk: '磁碟 {pct}%',
}
