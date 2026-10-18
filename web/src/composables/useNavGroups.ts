/**
 * useNavGroups — 侧栏导航的分组折叠状态。
 *
 * 菜单项过多时按组收纳:组头点击开合,状态持久化到 localStorage
 * (`pipewright_nav_collapsed_groups`,存已折叠组名的 JSON 数组)。
 *   - 默认(无记录)全部折叠 —— 首屏即紧凑;
 *   - isOpen 强制放行「包含当前路由条目」的组:活动页永不因折叠失去可见入口,
 *     且离开该组后自动回到折叠态(collapsed 集合未被触碰,用户选择不被覆盖);
 *   - localStorage 不可用(隐私模式/配额)时静默降级为仅内存态。
 */
import type { Component } from 'vue'
import { ref } from 'vue'

export interface NavItem {
  name: string
  to: string
  icon: Component
  /** i18n key under `nav.*` for the visible label. */
  labelKey: string
  /** i18n key under `nav.*` for the accessible name. */
  ariaKey: string
}

export interface NavGroup {
  name: string
  /** i18n key under `nav.*` for the group header label. */
  labelKey: string
  icon: Component
  items: NavItem[]
}

const STORAGE_KEY = 'pipewright_nav_collapsed_groups'

/**
 * 读取已折叠组名集合;无记录或记录损坏(非 JSON / 非字符串数组)返回 null,
 * 由调用方落到默认态(全部折叠)。
 */
function readCollapsed(): Set<string> | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw === null) return null
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed)) return null
    return new Set(parsed.filter((v): v is string => typeof v === 'string'))
  } catch {
    return null
  }
}

function writeCollapsed(names: Set<string>): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify([...names]))
  } catch {
    // 静默降级:隐私模式/配额满 → 仅本次会话内存态生效。
  }
}

export function useNavGroups(
  groupNames: readonly string[],
  isActive: (item: NavItem) => boolean,
) {
  const collapsed = ref<Set<string>>(readCollapsed() ?? new Set(groupNames))

  function isOpen(group: NavGroup): boolean {
    return !collapsed.value.has(group.name) || group.items.some(isActive)
  }

  function toggle(group: NavGroup): void {
    if (collapsed.value.has(group.name)) collapsed.value.delete(group.name)
    else collapsed.value.add(group.name)
    writeCollapsed(collapsed.value)
  }

  return { collapsed, isOpen, toggle }
}
