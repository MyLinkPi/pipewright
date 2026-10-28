<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NButton, NCard, NSwitch, useMessage } from 'naive-ui'
import { useLogsStore } from '../stores/logs'
import { useLocalStore } from '../stores/local'

const logs = useLogsStore()
const local = useLocalStore()
const message = useMessage()
const box = ref<HTMLElement | null>(null)

function scrollToBottom(): void {
  const el = box.value
  if (el) el.scrollTop = el.scrollHeight
}

function onScroll(): void {
  const el = box.value
  if (!el) return
  logs.autoScroll = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}

watch(
  () => logs.lines.length,
  async () => {
    if (!logs.autoScroll) return
    await nextTick()
    scrollToBottom()
  },
)

onMounted(() => {
  logs.start()
  void nextTick(scrollToBottom)
})

onBeforeUnmount(() => logs.stop())

function lineClass(line: string): string {
  const lower = line.toLowerCase()
  if (lower.includes('error') || line.includes('失败') || line.includes('错误')) return 'err'
  if (lower.includes('warn') || line.includes('警告')) return 'warn'
  return ''
}

async function copyAll(): Promise<void> {
  try {
    await navigator.clipboard.writeText(logs.lines.join('\n'))
    message.success('已复制当前日志')
  } catch {
    message.error('复制失败（浏览器未授予剪贴板权限）')
  }
}
</script>

<template>
  <n-card title="日志" size="small">
    <template #header-extra>
      <div class="actions">
        <span class="faint">自动滚动</span>
        <n-switch v-model:value="logs.autoScroll" size="small" />
        <n-button size="tiny" @click="copyAll">复制</n-button>
        <n-button size="tiny" @click="logs.clearView()">清屏</n-button>
        <n-button size="tiny" @click="local.openLog()">打开文件</n-button>
      </div>
    </template>
    <div v-if="logs.error" class="err">{{ logs.error }}</div>
    <div ref="box" class="logbox mono" @scroll="onScroll">
      <div v-for="(line, index) in logs.lines" :key="index" class="line" :class="lineClass(line)">
        {{ line }}
      </div>
      <div v-if="logs.lines.length === 0" class="faint empty">暂无日志</div>
    </div>
  </n-card>
</template>

<style scoped>
.actions {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
}

.err {
  margin-bottom: 8px;
  color: var(--color-danger);
  font-size: 13px;
}

.logbox {
  height: calc(100vh - 220px);
  min-height: 320px;
  overflow: auto;
  padding: 10px 12px;
  border-radius: 10px;
  background: var(--color-term);
  border: 1px solid var(--color-border);
  font-size: 12px;
  line-height: 1.55;
  white-space: pre-wrap;
  word-break: break-all;
}

.line {
  padding: 0;
}

.line.err {
  color: var(--color-term-err);
}

.line.warn {
  color: var(--color-term-warn);
}

.empty {
  padding: 6px 0;
}
</style>
