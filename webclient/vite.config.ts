// Vite 配置:产物直接落到 internal/pwclient/uiserver/dist(供 go:embed)。
// 开发联调:先跑 `pwclient ui --no-open --ui-listen 127.0.0.1:18080`,再 `npm run dev`,
// /api 与 /local 由 dev server 代理到该地址。
import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

const backend = 'http://127.0.0.1:18080'

export default defineConfig({
  plugins: [vue()],
  base: './',
  build: {
    outDir: '../internal/pwclient/uiserver/dist',
    emptyOutDir: true,
  },
  server: {
    port: 5174,
    strictPort: true,
    proxy: {
      '/api': backend,
      '/local': backend,
    },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.ts'],
    restoreMocks: true,
  },
})
