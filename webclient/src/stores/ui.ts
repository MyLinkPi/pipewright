// 界面偏好(主题):与平台一致的双主题 token;选择存 localStorage。
import { defineStore } from 'pinia'
import { ref } from 'vue'

export type ThemeName = 'dark' | 'light'

export const useUiStore = defineStore('ui', () => {
  const stored = localStorage.getItem('pw_theme')
  const theme = ref<ThemeName>(stored === 'light' ? 'light' : 'dark')

  function setTheme(next: ThemeName): void {
    theme.value = next
    localStorage.setItem('pw_theme', next)
    document.documentElement.dataset.theme = next
  }

  function apply(): void {
    document.documentElement.dataset.theme = theme.value
  }

  return { theme, setTheme, apply }
})
