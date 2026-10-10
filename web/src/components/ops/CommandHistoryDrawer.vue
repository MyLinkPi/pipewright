<!--
  CommandHistoryDrawer.vue — 批量命令执行历史(服务器状态页)

  右侧抽屉:历史列表(时间 / 命令 / 成败徽标),点击展开该次执行的逐机结果
  (退出码 / 耗时 / stdout / stderr,可折叠);支持「重跑」——把原命令与原目标机
  回填给 BatchCommandModal(仍走确认 + 危险强确认流程)。
  历史由后端落库并保留最近 200 次;404 = 已被裁剪。
-->
<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  getServerCommandRun,
  listServerCommandRuns,
  type CommandRunSummary,
  type CommandRunDetail,
} from '../../api/servers'
import { HttpError } from '../../api/http'
import StatusBadge from '../ui/StatusBadge.vue'

export interface RerunPayload {
  command: string
  serverIds: string[]
}

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'rerun', payload: RerunPayload): void
}>()

const { t } = useI18n()

const runs = ref<CommandRunSummary[]>([])
const loading = ref(false)
const loadError = ref('')
/** 展开详情的 runId → 详情(缓存;置 null 表示加载中/失败沿 detailError)。 */
const detail = ref<Record<string, CommandRunDetail | null>>({})
const detailError = ref<Record<string, string>>({})

function fmtTime(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

function fmtCommand(cmd: string): string {
  return cmd.length > 120 ? cmd.slice(0, 120) + '…' : cmd
}

async function load(): Promise<void> {
  loading.value = true
  loadError.value = ''
  try {
    runs.value = await listServerCommandRuns(50)
  } catch (err) {
    loadError.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('batchCommand.loadFailedStatus', { status: err.status }))
        : t('batchCommand.loadFailed')
  } finally {
    loading.value = false
  }
}

async function toggleDetail(runId: string): Promise<void> {
  if (runId in detail.value) {
    delete detail.value[runId]
    return
  }
  detail.value[runId] = null
  detailError.value[runId] = ''
  try {
    detail.value[runId] = await getServerCommandRun(runId)
  } catch (err) {
    delete detail.value[runId]
    detailError.value[runId] =
      err instanceof HttpError ? (err.apiError?.message ?? '') : t('batchCommand.loadFailed')
  }
}

function rerun(run: CommandRunSummary): void {
  const det = detail.value[run.id]
  const serverIds = det ? det.items.map((it) => it.serverId) : []
  emit('rerun', { command: run.command, serverIds })
}

// 抽屉随 v-if 挂载即拉取。
void load()
</script>

<template>
  <div class="drawer-scrim">
    <aside class="drawer" role="dialog" :aria-label="t('batchCommand.history')">
      <header class="drawer__head">
        <span class="drawer__name">{{ t('batchCommand.history') }}</span>
        <span class="grow" />
        <button class="tool-btn" :disabled="loading" @click="load">↻ {{ t('common.refresh') }}</button>
        <button class="drawer__close" :aria-label="t('batchCommand.close')" @click="emit('close')">✕</button>
      </header>

      <div class="drawer__body">
        <div v-if="loading && runs.length === 0" class="drawer__hint">{{ t('batchCommand.loading') }}</div>
        <div v-else-if="loadError" class="drawer__hint drawer__hint--err" role="alert">⚠ {{ loadError }}</div>
        <div v-else-if="runs.length === 0" class="drawer__hint">{{ t('batchCommand.historyEmpty') }}</div>

        <ul v-else class="runs" role="list">
          <li v-for="run in runs" :key="run.id" class="run">
            <button class="run__row" :aria-expanded="run.id in detail" @click="toggleDetail(run.id)">
              <span class="run__time">{{ fmtTime(run.createdAt) }}</span>
              <span class="run__cmd mono" :title="run.command">{{ fmtCommand(run.command) }}</span>
              <StatusBadge :status="run.failed === 0 ? 'success' : run.ok === 0 ? 'failed' : 'partial'" />
              <span class="run__count">
                {{ t('batchCommand.summary', { ok: run.ok, total: run.total }) }}
              </span>
            </button>

            <!-- 展开的逐机详情 -->
            <div v-if="run.id in detail" class="run__detail">
              <div v-if="detailError[run.id]" class="drawer__hint drawer__hint--err">⚠ {{ detailError[run.id] }}</div>
              <div v-else-if="!detail[run.id]" class="drawer__hint">{{ t('batchCommand.loading') }}</div>
              <template v-else>
                <div v-for="it in detail[run.id]!.items" :key="it.serverId" class="ditem" :class="{ 'ditem--fail': !it.ok }">
                  <header class="ditem__head">
                    <StatusBadge :status="it.ok ? 'success' : 'failed'" />
                    <span class="ditem__name" :title="it.serverId">{{ it.name || it.serverId }}</span>
                    <span class="ditem__meta mono">
                      {{ t('batchCommand.exitCode', { n: it.exitCode }) }} ·
                      {{ t('batchCommand.duration', { n: it.durationMs }) }}
                    </span>
                  </header>
                  <p v-if="it.error" class="ditem__error" role="alert">{{ it.error }}</p>
                  <details v-if="it.stdout" class="ditem__out">
                    <summary>{{ t('batchCommand.stdout') }}</summary>
                    <pre class="mono">{{ it.stdout }}</pre>
                  </details>
                  <details v-if="it.stderr" class="ditem__out">
                    <summary>{{ t('batchCommand.stderr') }}</summary>
                    <pre class="mono">{{ it.stderr }}</pre>
                  </details>
                </div>
                <button class="tool-btn run__rerun" @click="rerun(run)">
                  ↻ {{ t('batchCommand.rerun') }}
                </button>
              </template>
            </div>
          </li>
        </ul>
      </div>

      <footer class="drawer__foot">
        <span class="foot__hint">{{ t('batchCommand.historyFootHint') }}</span>
      </footer>
    </aside>
  </div>
</template>

<style scoped>
.drawer-scrim {
  position: fixed;
  inset: 0;
  z-index: 500;
  background: oklch(0% 0 0 / 0.42);
  display: flex;
  justify-content: flex-end;
  animation: scrimIn var(--duration-fast) var(--ease-out-expo);
}
@keyframes scrimIn {
  from { opacity: 0; }
  to { opacity: 1; }
}
.drawer {
  width: min(1000px, 92vw);
  height: 100%;
  display: flex;
  flex-direction: column;
  background: var(--color-card);
  border-left: 1px solid var(--color-border-strong);
  box-shadow: var(--shadow-modal);
  animation: drawerIn var(--duration-normal) var(--ease-out-expo);
}
@keyframes drawerIn {
  from { transform: translateX(24px); opacity: 0.4; }
  to { transform: translateX(0); opacity: 1; }
}

.drawer__head {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 14px 18px;
  border-bottom: 1px solid var(--color-border);
}
.drawer__name {
  font-size: 1.02rem;
  font-weight: 700;
  color: var(--color-text);
}
.drawer__close {
  border: 0;
  background: transparent;
  color: var(--color-faint);
  font-size: 1rem;
  cursor: pointer;
  padding: 4px 8px;
  border-radius: 6px;
}
.drawer__close:hover { color: var(--color-text); background: var(--color-inset); }

.drawer__body {
  flex: 1;
  overflow-y: auto;
  padding: 14px 18px;
}
.drawer__hint {
  padding: 24px 8px;
  text-align: center;
  color: var(--color-faint);
  font-size: var(--text-label, 0.8rem);
}
.drawer__hint--err { color: var(--color-red); }
.drawer__foot {
  padding: 10px 18px;
  border-top: 1px solid var(--color-border);
}
.foot__hint { font-size: var(--text-label, 0.8rem); color: var(--color-faint); }
.grow { flex: 1; }
.mono { font-family: var(--font-mono, ui-monospace, monospace); }

.tool-btn {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded, 8px);
  background: transparent;
  color: var(--color-dim);
  font: inherit;
  font-size: var(--text-label, 0.8rem);
  padding: 5px 12px;
  cursor: pointer;
  white-space: nowrap;
}
.tool-btn:hover:not(:disabled) { color: var(--color-text); border-color: var(--color-border-strong); }
.tool-btn:disabled { opacity: 0.55; cursor: not-allowed; }

/* ——— run list ——— */
.runs {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.run {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded, 8px);
  background: var(--color-card-2, var(--color-inset));
  overflow: hidden;
}
.run__row {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 10px 12px;
  border: 0;
  background: transparent;
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.run__row:hover { background: var(--color-inset); }
.run__time {
  flex-shrink: 0;
  font-size: var(--text-label, 0.8rem);
  color: var(--color-faint);
  font-variant-numeric: tabular-nums;
}
.run__cmd {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 0.8rem;
  color: var(--color-text);
}
.run__count {
  flex-shrink: 0;
  font-size: var(--text-label, 0.8rem);
  color: var(--color-dim);
  font-variant-numeric: tabular-nums;
}

.run__detail {
  padding: 0 12px 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.ditem {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 8px 10px;
  border: 1px solid var(--color-border);
  border-radius: 6px;
  background: var(--color-card);
}
.ditem--fail { border-color: var(--color-red-line, var(--color-border)); }
.ditem__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.ditem__name {
  font-weight: 650;
  font-size: var(--text-body, 0.9rem);
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 40%;
}
.ditem__meta {
  font-size: var(--text-label, 0.8rem);
  color: var(--color-faint);
  font-variant-numeric: tabular-nums;
}
.ditem__error {
  margin: 0;
  font-size: var(--text-label, 0.8rem);
  color: var(--color-red);
  line-height: 1.5;
}
.ditem__out summary {
  cursor: pointer;
  font-size: var(--text-label, 0.8rem);
  font-weight: 600;
  color: var(--color-dim);
  user-select: none;
}
.ditem__out pre {
  margin: 5px 0 0;
  padding: 8px;
  border-radius: 6px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  font-size: 0.76rem;
  line-height: 1.5;
  color: var(--color-text);
  max-height: 200px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
.run__rerun { align-self: flex-end; }
</style>
