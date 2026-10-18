import { defineConfig, devices } from '@playwright/test'

/**
 * Playwright e2e (smoke) config.
 *
 * Drives the Vite dev server (no real Go backend needed): every spec stubs
 * the `/api` routes with page.route, so flows are deterministic and offline.
 * Backend integration e2e lives elsewhere (whole-system run), not here.
 *
 * Run: npm run e2e   (browsers must be installed: npx playwright install chromium)
 */
const PORT = 5174
const BASE_URL = `http://localhost:${PORT}`

export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: BASE_URL,
    // Playwright 默认 locale 是 en-US,而应用按 navigator.languages 侦测语言、
    // 全部用例断言中文文案 —— 不钉死 locale 会整套渲染成英文而挂掉。
    locale: 'zh-CN',
    trace: 'on-first-retry',
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
  ],
  webServer: {
    // Strict port so the baseURL is predictable; dev server is enough for smoke.
    command: `npm run dev -- --port ${PORT} --strictPort`,
    url: BASE_URL,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
})
