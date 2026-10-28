// 日志增量:按 since/nextSeq 逐拍拉取;reset 时清空重填。上限 2000 行防内存膨胀。
import { defineStore } from 'pinia'
import { ref } from 'vue'
import { errorText, http } from '../api/http'
import type { LogChunk } from '../api/types'

const MAX_LINES = 2000

export const useLogsStore = defineStore('logs', () => {
  const lines = ref<string[]>([])
  const nextSeq = ref(0)
  const autoScroll = ref(true)
  const error = ref('')
  let timer: number | undefined

  async function poll(): Promise<void> {
    try {
      const chunk = await http.get<LogChunk>(`/api/logs?since=${nextSeq.value}&n=500`)
      if (chunk.reset) {
        lines.value = chunk.lines
      } else if (chunk.lines.length > 0) {
        lines.value = lines.value.concat(chunk.lines)
      }
      if (lines.value.length > MAX_LINES) {
        lines.value = lines.value.slice(lines.value.length - MAX_LINES)
      }
      nextSeq.value = chunk.nextSeq
      error.value = ''
    } catch (err) {
      error.value = errorText(err)
    }
  }

  function start(): void {
    stop()
    void poll()
    timer = window.setInterval(() => {
      if (!document.hidden) void poll()
    }, 2000)
  }

  function stop(): void {
    if (timer !== undefined) {
      window.clearInterval(timer)
      timer = undefined
    }
  }

  function clearView(): void {
    lines.value = []
  }

  return { lines, nextSeq, autoScroll, error, poll, start, stop, clearView }
})
