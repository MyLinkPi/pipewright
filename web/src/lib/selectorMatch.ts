/**
 * selectorMatch — 目标选择器客户端匹配预览.
 *
 * 与服务端同一语义(internal/runner/selector.go 纯函数层,构建机池与部署目标圈选共用):
 * 逗号分隔项全部命中才匹配(AND);纯 tag 不匹配 k=v(严格逐项相等,不猜语义);
 * `server:<id>` 钉死单机。仅用于输入实时预览,权威裁决在服务端。
 */
import type { Server } from '../api/servers'

/** 把选择器表达式拆成标签项(逗号分隔、去空白、丢空项)。 */
export function parseSelectorTerms(selector: string): string[] {
  return selector.split(',').map((s) => s.trim()).filter(Boolean)
}

/** 返回选择器命中的机器(空表达式 / 零命中 → 空数组;`server:<id>` → 命中的那台)。 */
export function matchServers(selector: string, servers: Server[]): Server[] {
  const sel = selector.trim()
  if (sel.startsWith('server:')) {
    const id = sel.slice('server:'.length)
    const hit = servers.find((s) => s.id === id)
    return hit ? [hit] : []
  }
  const terms = parseSelectorTerms(sel)
  if (terms.length === 0) return []
  return servers.filter((s) => {
    const labels = (s.labels ?? '').split(',').map((x) => x.trim()).filter(Boolean)
    return terms.every((term) => labels.includes(term))
  })
}
