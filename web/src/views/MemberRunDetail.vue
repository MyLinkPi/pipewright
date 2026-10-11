<script setup lang="ts">
/**
 * MemberRunDetail — 普通用户只读运行详情(/my-runs/:id)。
 *
 * 复用只读展示组件 RunStepList / RunTerminal / ArtifactList;不提供任何管理动作
 * (取消 / 审批 / 部署 / 诊断 / diff / 恢复 等一律不出现)。
 * 数据来源均在 memberGate 白名单内:GET /api/runs/{id}、/{id}/events、/{id}/artifacts(下载)。
 */
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import AppButton from '../components/ui/AppButton.vue'
import RunStepList from '../components/run/RunStepList.vue'
import RunTerminal from '../components/run/RunTerminal.vue'
import ArtifactList from '../components/run/ArtifactList.vue'
import { getRun, subscribeRunEvents, type RunDetail, type RunStatus } from '../api/runs'
import { HttpError } from '../api/http'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const runId = computed(() => String(route.params.id ?? ''))

const run = ref<RunDetail | null>(null)
const loadError = ref('')
const loading = ref(true)
const selectedStepOrdinal = ref<number | null>(null)

let unsubscribe: (() => void) | null = null

function statusLabel(status: RunStatus): string {
  return t(`runStatus.${status}`)
}

/** 进行中(可实时 tail)→ true;终态 → false(仅历史回放)。 */
const isLive = computed(() => {
  const s = run.value?.status
  return s === 'queued' || s === 'running' || s === 'waiting_approval'
})

function shortCommit(commit: string): string {
  return commit.length > 7 ? commit.slice(0, 7) : commit
}

function formatDuration(ms: number | null): string {
  if (ms === null || ms === undefined) return '—'
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  const r = s % 60
  return `${m}m ${r}s`
}

function formatTime(iso: string | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString('zh-CN', {
    year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}

async function load(): Promise<void> {
  loading.value = true
  loadError.value = ''
  try {
    run.value = await getRun(runId.value)
    loading.value = false
    attachEvents()
  } catch (err) {
    loading.value = false
    if (err instanceof HttpError && (err.status === 403 || err.status === 404)) {
      loadError.value = t('myRunDetail.notFound')
      return
    }
    loadError.value = err instanceof Error ? err.message : t('myRunDetail.loadError')
  }
}

/** 订阅状态/步骤事件,实时更新详情(与 RunTerminal 各自独立订阅,互不干扰)。 */
function attachEvents(): void {
  if (unsubscribe) return
  unsubscribe = subscribeRunEvents(runId.value, {
    onStatus: (e) => {
      if (run.value && e.runId === run.value.id) run.value.status = e.status
    },
    onStep: (e) => {
      if (!run.value || e.runId !== run.value.id) return
      const idx = run.value.steps.findIndex((s) => s.id === e.step.id)
      if (idx >= 0) run.value.steps.splice(idx, 1, e.step)
      else run.value.steps.push(e.step)
    },
  })
}

function goBack(): void {
  void router.push({ name: 'my-runs' })
}

onMounted(load)
onUnmounted(() => {
  if (unsubscribe) {
    unsubscribe()
    unsubscribe = null
  }
})
</script>

<template>
  <div class="member-run-detail">
    <header class="head">
      <button type="button" class="back-btn" :aria-label="t('myRunDetail.backAria')" @click="goBack">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M15 18l-6-6 6-6" />
        </svg>
        <span>{{ t('myRunDetail.back') }}</span>
      </button>
      <h1 class="title">{{ t('myRunDetail.title') }}</h1>
    </header>

    <div v-if="loading" class="state" aria-busy="true">{{ t('myRunDetail.loading') }}</div>

    <div v-else-if="loadError" class="state state--error" role="alert">
      {{ loadError }}
      <AppButton variant="default" @click="load">{{ t('myRunDetail.retry') }}</AppButton>
    </div>

    <template v-else-if="run">
      <!-- meta -->
      <section class="card meta">
        <div class="meta-row">
          <span class="meta-key">{{ t('myRunDetail.metaProject') }}</span>
          <span class="meta-val">{{ run.projectName || run.projectId }}</span>
        </div>
        <div class="meta-row">
          <span class="meta-key">{{ t('myRunDetail.metaStatus') }}</span>
          <span class="status-pill">{{ statusLabel(run.status) }}</span>
        </div>
        <div class="meta-row">
          <span class="meta-key">{{ t('myRunDetail.metaBranch') }}</span>
          <span class="meta-val mono">{{ run.trigger.branch || '—' }}</span>
        </div>
        <div class="meta-row">
          <span class="meta-key">{{ t('myRunDetail.metaCommit') }}</span>
          <span class="meta-val mono">{{ run.trigger.commit ? shortCommit(run.trigger.commit) : '—' }}</span>
        </div>
        <div class="meta-row">
          <span class="meta-key">{{ t('myRunDetail.metaTrigger') }}</span>
          <span class="meta-val">{{ run.trigger.type === 'webhook' ? 'Webhook' : t('myRunDetail.triggerManual') }} · {{ run.trigger.actor }}</span>
        </div>
        <div class="meta-row">
          <span class="meta-key">{{ t('myRunDetail.metaCreated') }}</span>
          <span class="meta-val">{{ formatTime(run.createdAt) }}</span>
        </div>
        <div class="meta-row">
          <span class="meta-key">{{ t('myRunDetail.metaDuration') }}</span>
          <span class="meta-val">{{ formatDuration(run.durationMs) }}</span>
        </div>
      </section>

      <!-- steps + logs -->
      <section class="card">
        <h2 class="section-title">{{ t('myRunDetail.stepsTitle') }}</h2>
        <RunStepList
          :steps="run.steps"
          :selected="selectedStepOrdinal"
          @select="selectedStepOrdinal = $event"
        />
      </section>

      <section class="card">
        <h2 class="section-title">{{ t('myRunDetail.logsTitle') }}</h2>
        <div class="log-wrap">
          <RunTerminal
            :run-id="run.id"
            :live="isLive"
            :filter-ordinal="selectedStepOrdinal"
            :steps="run.steps"
          />
        </div>
      </section>

      <!-- artifacts -->
      <section class="card">
        <h2 class="section-title">{{ t('myRunDetail.artifactsTitle') }}</h2>
        <ArtifactList v-if="run.artifacts && run.artifacts.length > 0" :artifacts="run.artifacts" :run-id="run.id" />
        <p v-else class="empty-note">{{ t('myRunDetail.artifactsEmpty') }}</p>
      </section>
    </template>
  </div>
</template>

<style scoped>
.member-run-detail {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.head {
  display: flex;
  align-items: center;
  gap: 14px;
}
.back-btn {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  background: none;
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  color: var(--color-dim);
  cursor: pointer;
  font: inherit;
  font-size: 0.82rem;
  padding: 6px 12px;
}
.back-btn:hover {
  color: var(--color-text);
  border-color: var(--color-faint);
}
.back-btn svg {
  width: 15px;
  height: 15px;
}
.title {
  font-size: 1.15rem;
  font-weight: 700;
  color: var(--color-text);
}
.state {
  padding: 40px 0;
  text-align: center;
  color: var(--color-faint);
  font-size: 0.86rem;
}
.state--error {
  color: var(--color-red);
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
}
.card {
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  box-shadow: var(--shadow);
  padding: 18px 20px;
}
.section-title {
  font-size: 0.92rem;
  font-weight: 600;
  color: var(--color-text);
  margin-bottom: 12px;
}
.meta {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(190px, 1fr));
  gap: 12px 22px;
}
.meta-row {
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.meta-key {
  font-size: 0.72rem;
  color: var(--color-faint);
}
.meta-val {
  font-size: 0.85rem;
  color: var(--color-text);
}
.mono {
  font-family: var(--font-mono);
}
.status-pill {
  align-self: flex-start;
  font-size: 0.72rem;
  font-weight: 600;
  padding: 2px 9px;
  border-radius: var(--rounded-sm);
  background: var(--color-card-2);
  border: 1px solid var(--color-border-strong);
  color: var(--color-dim);
}
.log-wrap {
  height: 420px;
}
.empty-note {
  font-size: 0.83rem;
  color: var(--color-faint);
}
</style>
