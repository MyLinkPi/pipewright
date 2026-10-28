import { test, expect } from '@playwright/test'
import type { Page } from '@playwright/test'

// PW_UI_URL 由 scripts/client-e2e.sh 注入(带界面令牌)。未设置时整个文件 skip。
const base = process.env.PW_UI_URL
test.skip(!base, 'PW_UI_URL 未设置:请用 make client-e2e 启动真实客户端')

test.describe.configure({ mode: 'serial' })

async function openApp(page: Page): Promise<void> {
  await page.goto(base as string)
  if ((await page.locator('.sidebar').count()) === 0) {
    const info = await page.evaluate(() => ({
      href: location.href,
      hash: location.hash,
      token: sessionStorage.getItem('pw_ui_token'),
    }))
    console.error('openApp debug:', JSON.stringify(info))
  }
  await expect(page.locator('.sidebar')).toBeVisible()
}

// seedConfig 通过界面令牌写入一份「已配置但未连接」的最小配置,供单项修改用例使用。
async function seedConfig(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const token = sessionStorage.getItem('pw_ui_token') ?? ''
    const resp = await fetch('/api/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', 'X-PW-Token': token },
      body: JSON.stringify({
        enabled: false,
        networkName: 'e2e-net',
        networkSecret: 'e2e-secret',
        peers: ['tcp://127.0.0.1:11010'],
        hostname: 'e2e-host-a',
      }),
    })
    if (!resp.ok) throw new Error(`seed config failed: ${await resp.text()}`)
  })
}

test('未配置:概览显示引导,主按钮为导入配置', async ({ page }) => {
  await openApp(page)
  await expect(page.locator('.hero-state')).toContainText('未配置')
  await expect(page.getByRole('button', { name: '导入配置' })).toBeVisible()
})

test('配置总览:展示全量配置项且不含密钥明文', async ({ page }) => {
  await openApp(page)
  await page.getByRole('link', { name: '配置总览' }).click()
  const content = page.locator('.content')
  await expect(content.getByText('连接开关')).toBeVisible()
  await expect(content.getByText('虚拟 IP')).toBeVisible()
  await expect(content.getByText('阿里云安全组')).toBeVisible()
  await expect(page.locator('body')).not.toContainText('e2e-secret')
})

test('连接参数:空导入禁用,非法 CIDR 即时提示', async ({ page }) => {
  await openApp(page)
  await page.getByRole('link', { name: '连接参数' }).click()
  await expect(page.getByRole('button', { name: '导入', exact: true })).toBeDisabled()

  await page.getByRole('link', { name: '网段路由' }).click()
  await page.getByPlaceholder('手动添加 CIDR，如 192.168.30.0/24').fill('bad')
  await page.getByRole('button', { name: '添加' }).click()
  await expect(page.getByText('不是合法 CIDR', { exact: false })).toBeVisible()
})

test('单项修改:hostname 保存只提交该字段,并在配置总览回显', async ({ page }) => {
  await openApp(page)
  await seedConfig(page)
  await page.reload()
  await expect(page.locator('.hero-state')).toContainText('已断开')

  await page.getByRole('link', { name: '连接参数' }).click()
  await page.getByPlaceholder('留空 = 计算机名').fill('e2e-host-b')
  const [request] = await Promise.all([
    page.waitForRequest(
      (req) => req.url().endsWith('/api/config') && req.method() === 'PUT',
    ),
    page.getByRole('button', { name: '保存连接参数' }).click(),
  ])
  const payload = JSON.parse(request.postData() ?? '{}')
  expect(Object.keys(payload)).toEqual(['hostname'])
  expect(payload.hostname).toBe('e2e-host-b')

  await page.getByRole('link', { name: '配置总览' }).click()
  await expect(page.getByText('e2e-host-b').first()).toBeVisible()
})

test('网段路由:保存后轮询不回写表单', async ({ page }) => {
  await openApp(page)
  await page.getByRole('link', { name: '网段路由' }).click()
  await page.getByPlaceholder('手动添加 CIDR，如 192.168.30.0/24').fill('10.10.0.0/16')
  await page.getByRole('button', { name: '添加' }).click()
  await page.getByRole('button', { name: '保存并应用' }).click()
  await expect(page.locator('.content').getByText('10.10.0.0/16').first()).toBeVisible()
  // 跨过一次 2 秒状态轮询:表单由草稿驱动,不应被轮询回写。
  await page.waitForTimeout(2500)
  await expect(page.locator('.content').getByText('10.10.0.0/16').first()).toBeVisible()
  await expect(page.getByText('有未保存修改')).toHaveCount(0)
})

test('安全组:空行保存给出三段校验提示', async ({ page }) => {
  await openApp(page)
  await page.getByRole('link', { name: '阿里云安全组' }).click()
  await page.getByRole('button', { name: '添加安全组' }).click()
  await page.getByRole('button', { name: '保存并应用' }).click()
  await expect(page.getByText('安全组每行须填', { exact: false })).toBeVisible()
})

test('日志:可进入并显示日志容器', async ({ page }) => {
  await openApp(page)
  await page.getByRole('link', { name: '日志' }).click()
  await expect(page.locator('.logbox')).toBeVisible()
})

test('设置:非 Windows 隐藏服务管理,保留退出入口', async ({ page }) => {
  await openApp(page)
  await page.getByRole('link', { name: '设置' }).click()
  await expect(page.getByText('后台服务', { exact: true })).toHaveCount(0)
  await expect(page.getByText('退出客户端')).toBeVisible()
})
