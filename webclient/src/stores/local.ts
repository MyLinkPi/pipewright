// 宿主信息与本机动作(/local/*):服务安装/启停/卸载、开机自启、升级、退出、打开日志。
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { errorText, http } from '../api/http'
import type { LocalInfo, UIPrefs } from '../api/types'

export const useLocalStore = defineStore('local', () => {
  const info = ref<LocalInfo | null>(null)
  const error = ref('')
  const busy = ref('')

  const canInstall = computed(() => info.value?.canInstallService ?? false)
  const isWindows = computed(() => info.value?.platform === 'windows')

  async function load(): Promise<void> {
    try {
      info.value = await http.get<LocalInfo>('/local/info')
      error.value = ''
    } catch (err) {
      error.value = errorText(err)
    }
  }

  async function run(key: string, action: () => Promise<unknown>): Promise<void> {
    busy.value = key
    try {
      await action()
      error.value = ''
    } catch (err) {
      error.value = errorText(err)
      throw err
    } finally {
      busy.value = ''
      await load()
    }
  }

  function installService(): Promise<void> {
    return run('install', () => http.post('/local/service/install'))
  }

  function serviceAction(action: 'start' | 'stop' | 'uninstall'): Promise<void> {
    return run(action, () => http.post(`/local/service/${action}`))
  }

  function setAutoStart(enabled: boolean): Promise<void> {
    return run('autostart', () => http.post('/local/autostart', { enabled }))
  }

  function upgrade(tag: string, includeUnstable = false): Promise<void> {
    return run('upgrade', () => http.post('/local/upgrade', { tag, includeUnstable }))
  }

  /** 读界面偏好(主题/升级是否包含非稳定版本)。 */
  async function loadPrefs(): Promise<UIPrefs> {
    return http.get<UIPrefs>('/local/prefs')
  }

  /** 写界面偏好(整份提交,后端读-改-写落盘)。 */
  async function savePrefs(prefs: UIPrefs): Promise<void> {
    await http.post('/local/prefs', prefs)
  }

  function openLog(): Promise<void> {
    return run('open-log', () => http.post('/local/open-log'))
  }

  function revealDataDir(): Promise<void> {
    return run('reveal', () => http.post('/local/reveal-data-dir'))
  }

  async function quit(): Promise<void> {
    await http.post('/local/quit')
  }

  return {
    info,
    error,
    busy,
    canInstall,
    isWindows,
    load,
    installService,
    serviceAction,
    setAutoStart,
    upgrade,
    loadPrefs,
    savePrefs,
    openLog,
    revealDataDir,
    quit,
  }
})
