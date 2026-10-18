import { beforeEach, describe, expect, it } from 'vitest'
import type { Component } from 'vue'
import { useNavGroups, type NavGroup, type NavItem } from './useNavGroups'

const STORAGE_KEY = 'pipewright_nav_collapsed_groups'

const item = (name: string, to: string): NavItem => ({
  name,
  to,
  icon: {} as Component,
  labelKey: `nav.${name}`,
  ariaKey: `nav.${name}`,
})

const delivery: NavGroup = {
  name: 'delivery',
  labelKey: 'nav.groupDelivery',
  icon: {} as Component,
  items: [item('projects', '/projects'), item('runs', '/runs')],
}

const infra: NavGroup = {
  name: 'infra',
  labelKey: 'nav.groupInfra',
  icon: {} as Component,
  items: [item('cluster', '/cluster')],
}

const GROUP_NAMES = [delivery.name, infra.name]

/** isActive 闭包读取可变 path,便于在同一实例上模拟路由变化。 */
function setup() {
  let activePath = ''
  const api = useNavGroups(GROUP_NAMES, (i) => activePath.startsWith(i.to))
  return {
    ...api,
    goto(path: string): void {
      activePath = path
    },
  }
}

describe('useNavGroups', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('defaults to all collapsed when no stored record exists', () => {
    const { isOpen } = setup()
    expect(isOpen(delivery)).toBe(false)
    expect(isOpen(infra)).toBe(false)
  })

  it('restores a stored collapsed set', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(['infra']))
    const { isOpen } = setup()
    // 仅 infra 被折叠;delivery 无记录 → 展开态。
    expect(isOpen(delivery)).toBe(true)
    expect(isOpen(infra)).toBe(false)
  })

  it('falls back to all-collapsed on corrupted stored JSON', () => {
    localStorage.setItem(STORAGE_KEY, '{not json')
    const { isOpen } = setup()
    expect(isOpen(delivery)).toBe(false)
    expect(isOpen(infra)).toBe(false)
  })

  it('falls back to all-collapsed when the record is not a string array', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ delivery: true }))
    const { isOpen } = setup()
    expect(isOpen(delivery)).toBe(false)
  })

  it('toggling expands a group and persists the choice', () => {
    const { isOpen, toggle } = setup()
    toggle(delivery)
    expect(isOpen(delivery)).toBe(true)
    expect(isOpen(infra)).toBe(false)
    expect(localStorage.getItem(STORAGE_KEY)).toBe(JSON.stringify(['infra']))
  })

  it('collapsing an open group persists and hides it', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify([]))
    const { isOpen, toggle } = setup()
    expect(isOpen(delivery)).toBe(true)
    toggle(delivery)
    expect(isOpen(delivery)).toBe(false)
    expect(localStorage.getItem(STORAGE_KEY)).toBe(JSON.stringify(['delivery']))
  })

  it('forces a collapsed group open while it contains the active item', () => {
    const { isOpen, goto } = setup()
    goto('/runs/42')
    // runs 在 delivery 组内:即便组在折叠集合里也强制展开,活动页不失去可见入口。
    expect(isOpen(delivery)).toBe(true)
  })

  it('returns to the collapsed choice after navigating away', () => {
    const { isOpen, goto } = setup()
    goto('/cluster')
    expect(isOpen(infra)).toBe(true)
    goto('/dashboard')
    // collapsed 集合未被触碰 → 离开后自动回到折叠态。
    expect(isOpen(infra)).toBe(false)
  })
})
