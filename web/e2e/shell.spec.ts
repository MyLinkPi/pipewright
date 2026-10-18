import { test, expect } from '@playwright/test'
import { stubLoggedIn } from './_stubs'

test.describe('App shell navigation', () => {
  test('renders the left rail with grouped nav destinations', async ({ page }) => {
    await stubLoggedIn(page)
    await page.goto('/')

    const nav = page.getByRole('navigation', { name: '主导航' })

    // 顶层散项与底部固定项不经分组折叠,始终直接可见。
    for (const label of ['概览', '用户管理', '设置']) {
      await expect(nav.getByRole('link', { name: label, exact: true })).toBeVisible()
    }

    // 分组默认折叠:仅组头可见,组内条目不露出。
    for (const label of ['交付', '基础设施', '存储与文件', '应用与数据', '监控与告警']) {
      const header = nav.getByRole('button', { name: label, exact: true })
      await expect(header).toBeVisible()
      await expect(header).toHaveAttribute('aria-expanded', 'false')
    }
    await expect(nav.getByRole('link', { name: '项目', exact: true })).toBeHidden()
  })

  test('expanding a group reveals its items and navigates within the shell', async ({ page }) => {
    await stubLoggedIn(page)
    await page.goto('/')

    const nav = page.getByRole('navigation', { name: '主导航' })
    await nav.getByRole('button', { name: '交付', exact: true }).click()

    const projects = nav.getByRole('link', { name: '项目', exact: true })
    await expect(projects).toBeVisible()
    await projects.click()
    await expect(page).toHaveURL(/\/projects$/)
    // Rail persists after navigation (shell layout, not a full reload).
    await expect(nav).toBeVisible()
    // 手动展开的选择被记忆:导航后组仍保持展开。
    await expect(nav.getByRole('button', { name: '交付', exact: true })).toHaveAttribute(
      'aria-expanded',
      'true',
    )
  })

  test('group expansion choice persists across reload', async ({ page }) => {
    await stubLoggedIn(page)
    await page.goto('/')

    const nav = page.getByRole('navigation', { name: '主导航' })
    await nav.getByRole('button', { name: '交付', exact: true }).click()

    await page.reload()
    const navAfter = page.getByRole('navigation', { name: '主导航' })
    await expect(navAfter.getByRole('button', { name: '交付', exact: true })).toHaveAttribute(
      'aria-expanded',
      'true',
    )
  })
})
