// 状态快照轮询:2 秒一次;页面隐藏时暂停;连续失败 3 次后退避到约 5 秒。
// 轮询只写本 store(只读状态),绝不触碰页面表单草稿。
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { errorText, http, isOffline } from '../api/http'
import type { StatusSnapshot } from '../api/types'
import { connectionState, primaryAction, stateMeta } from '../lib/derive'

export const useStatusStore = defineStore('status', () => {
  const snapshot = ref<StatusSnapshot | null>(null)
  const offline = ref(false)
  const lastError = ref('')
  const loading = ref(false)

  let timer: number | undefined
  let failures = 0
  let skipTicks = 0

  async function refresh(): Promise<void> {
    loading.value = true
    try {
      snapshot.value = await http.get<StatusSnapshot>('/api/status')
      offline.value = false
      lastError.value = ''
      failures = 0
      skipTicks = 0
    } catch (err) {
      failures += 1
      lastError.value = errorText(err)
      if (isOffline(err)) offline.value = true
      if (failures >= 3) skipTicks = 1
    } finally {
      loading.value = false
    }
  }

  function tick(): void {
    if (document.hidden) return
    if (skipTicks > 0) {
      skipTicks -= 1
      return
    }
    void refresh()
  }

  function start(): void {
    stop()
    void refresh()
    timer = window.setInterval(tick, 2000)
    document.addEventListener('visibilitychange', onVisible)
  }

  function stop(): void {
    if (timer !== undefined) {
      window.clearInterval(timer)
      timer = undefined
    }
    document.removeEventListener('visibilitychange', onVisible)
  }

  function onVisible(): void {
    if (!document.hidden) void refresh()
  }

  const state = computed(() => connectionState(snapshot.value, offline.value))
  const meta = computed(() => stateMeta(state.value, snapshot.value))
  const action = computed(() => primaryAction(state.value))

  return {
    snapshot,
    offline,
    lastError,
    loading,
    state,
    meta,
    action,
    refresh,
    start,
    stop,
  }
})
