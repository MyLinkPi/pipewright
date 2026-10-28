import { defineConfig } from '@playwright/test'

// PW_UI_URL 由 scripts/client-e2e.sh 注入(带界面令牌的完整地址)。
// 未设置时 e2e 用例自动 skip(避免误跑成"连不上后端"的失败)。
// 默认 chromium(CI 用 npx playwright install chromium);本地可用 PW_BROWSER=firefox
// 或 PW_CHANNEL=chrome 复用已安装的浏览器(受宿主 glib 版本影响时会启动失败)。
const browserName = (process.env.PW_BROWSER ?? 'chromium') as 'chromium' | 'firefox' | 'webkit'
const channel = process.env.PW_CHANNEL as 'chrome' | 'msedge' | undefined

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  fullyParallel: false,
  workers: 1,
  reporter: 'list',
  use: {
    headless: true,
    viewport: { width: 1280, height: 800 },
  },
  projects: [
    {
      name: browserName,
      use: {
        browserName,
        ...(channel && browserName === 'chromium' ? { channel } : {}),
      },
    },
  ],
})
