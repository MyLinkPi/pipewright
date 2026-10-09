/**
 * stableJson — 键序无关的 JSON 序列化(脏检查快照用)。
 *
 * 编辑器常以「展开重建」的方式回写配置(如 JobDrawer 的 {...extras, ...typed}),
 * 键序与服务器 DTO 不同;直接 JSON.stringify 全串比较会把内容等价的回写误判为脏。
 * 这里递归排序纯对象的键(数组保持原序),让快照比较只看内容、不看键序。
 */

/** 递归规范化:纯对象按键名排序重建;数组/原始值保持原样。 */
function canonicalize(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonicalize)
  if (value !== null && typeof value === 'object') {
    const src = value as Record<string, unknown>
    const out: Record<string, unknown> = {}
    for (const k of Object.keys(src).sort()) {
      out[k] = canonicalize(src[k])
    }
    return out
  }
  return value
}

/** 键序无关的序列化:内容等价的结构(无论键序)产出同一字符串。 */
export function stableStringify(value: unknown): string {
  return JSON.stringify(canonicalize(value))
}
