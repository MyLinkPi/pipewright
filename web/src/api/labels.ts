/**
 * Labels API — 机器标签登记处(标签字典,可先建后挂).
 *
 * GET    /api/labels        → { items: LabelDef[] }
 * POST   /api/labels        → LabelDef         (needs CSRF)
 * DELETE /api/labels/:name  → 204              (needs CSRF;仍被机器引用 → 409 label_in_use)
 *
 * 登记处只是字典:servers.labels 仍是机器实际标签的事实来源。悬置标签(还没有任何
 * 机器打)先在这里建,各选择器下拉的候选 = 登记处 ∪ 机器实际标签。
 */

import { ref } from 'vue'
import { http } from './http'

export interface LabelDef {
  name: string
  createdAt: string
}

export async function listLabels(): Promise<LabelDef[]> {
  const data = await http.get<{ items: LabelDef[] }>('/api/labels')
  return data.items ?? []
}

export async function createLabel(name: string): Promise<LabelDef> {
  return http.post<LabelDef>('/api/labels', { name })
}

export async function deleteLabel(name: string): Promise<void> {
  await http.delete<void>(`/api/labels/${encodeURIComponent(name)}`)
}

/**
 * useLabels — 模块级共享的登记处缓存(composable):各选择器组件直接取候选集,
 * 免去逐层传 props。首次访问拉取;create/remove 同步维护缓存。load(true) 强制刷新。
 */
const items = ref<LabelDef[]>([])
let loaded = false
let inflight: Promise<void> | null = null

export function useLabels() {
  async function load(force = false): Promise<void> {
    if (!force && (loaded || inflight)) {
      if (inflight) await inflight
      return
    }
    inflight = listLabels()
      .then((list) => {
        items.value = list
        loaded = true
      })
      .catch(() => {
        /* 登记处暂不可用时静默降级:下拉只剩机器实际标签,不影响主流程 */
      })
      .finally(() => {
        inflight = null
      })
    await inflight
  }

  /** 登记新标签并同步进缓存(已存在视为成功——并发建同名的第二个调用不该报错打断用户)。 */
  async function ensure(name: string): Promise<LabelDef> {
    const created = await createLabel(name)
    items.value = [...items.value.filter((l) => l.name !== created.name), created].sort((a, b) =>
      a.name.localeCompare(b.name),
    )
    return created
  }

  /** 删登记标签并同步缓存(409 in_use 由调用方按业务提示)。 */
  async function remove(name: string): Promise<void> {
    await deleteLabel(name)
    items.value = items.value.filter((l) => l.name !== name)
  }

  return { items, load, ensure, remove }
}
