// 界面偏好(主题):与平台一致的双主题 token。
//
// 主题不存 localStorage,而由后端持有(<dataDir>/ui-prefs.json):界面服务每次启动是随机
// 端口,浏览器按源(origin)隔离 localStorage,端口一变旧主题就读不回来(切换后重启不保存)。
// 后端把主题注入 index.html 的 <html data-theme>,首屏即为正确主题;此处据其初始化,
// 切换时回写后端(同时让宿主同步原生标题栏配色)。
import { defineStore } from 'pinia'
import { ref } from 'vue'
import { http } from '../api/http'

export type ThemeName = 'dark' | 'light'

// initialTheme 取服务端注入的 data-theme(缺省深色,与后端默认一致)。
function initialTheme(): ThemeName {
  return document.documentElement.dataset.theme === 'light' ? 'light' : 'dark'
}

export const useUiStore = defineStore('ui', () => {
  const theme = ref<ThemeName>(initialTheme())

  function setTheme(next: ThemeName): void {
    theme.value = next
    document.documentElement.dataset.theme = next
    // 回写后端:持久化偏好 + 同步宿主原生标题栏(Windows 深色模式)。
    // 失败静默,不阻塞界面即时切换(主题已应用到 DOM)。
    void http.post('/local/theme', { theme: next }).catch(() => {})
  }

  function apply(): void {
    document.documentElement.dataset.theme = theme.value
  }

  return { theme, setTheme, apply }
})
