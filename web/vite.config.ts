/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 前端构建为静态资源 → web/dist → 由 Go go:embed 嵌入单二进制(无前后端分离部署)。
// base 用绝对根 '/':平台始终服务于根,SPA 深路由(如 /runs/128)回退 index.html 时,
// 资源以 /assets/... 绝对解析,避免相对 './' 在二级深链下解析错位。dev 时代理 API 到 Go。
export default defineConfig({
  plugins: [vue()],
  base: '/',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // 默认指向本机默认后端;多实例联调可用 PIPEWRIGHT_API_TARGET 覆盖(如 :8081)。
      // ws: true 转发 WebSocket 升级(交互终端是平台唯一 WS 点;不加则升级挂死,终端页
      // 永远停在「正在连接」)。勿加 changeOrigin:后端 WS 同源校验比对 Origin 与 Host
      // 头,改写 Host 会被 403。
      '/api': {
        target: process.env.PIPEWRIGHT_API_TARGET ?? 'http://localhost:8080',
        ws: true,
      },
      '/healthz': process.env.PIPEWRIGHT_API_TARGET ?? 'http://localhost:8080',
    },
  },
  // Vitest 单元/组件测试配置(内联到 vite.config,复用同一份插件与解析规则)。
  // jsdom 提供 DOM/localStorage/document.cookie;setup 注册全局 stub。
  // e2e/ 由 Playwright 拥有,排除在 Vitest 之外避免被当成单测收集。
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.{test,spec}.ts'],
    exclude: ['node_modules', 'dist', 'e2e'],
    css: false,
    clearMocks: true,
  },
})
