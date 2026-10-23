<script setup lang="ts">
// 集群级共享构建机(0066)owner 管理页:本端共享机总览 + 跨环境共享构建任务列表(排队/运行/
// 待回拉),支持强制取消。任务表是后端进程内存态(重启即清),与构建调度语义一致。
import { onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { HttpError } from '../../api/http'
import { listServers, type Server } from '../../api/servers'
import { cancelSharedBuildJob, listSharedBuildJobs, type SharedBuildJob, type SharedBuildJobState } from '../../api/sharedBuild'

const { t } = useI18n()

const state = ref<'loading' | 'ready' | 'error'>('loading')
const errorMsg = ref('')
const machines = ref<Server[]>([])
const jobs = ref<SharedBuildJob[]>([])
// 任务端点与机器列表解耦:服务端运行时未装配(legacy 执行器/旧版本)时
// /api/build/shared/jobs 整体缺席(404)——机器总览照常,任务区降级提示,轮询自动恢复。
const jobsUnavailable = ref(false)
const jobsError = ref('')
const actionError = ref('')
const cancelTarget = ref('')
let pollTimer: ReturnType<typeof setInterval> | null = null

function fmtTime(iso?: string): string {
  if (!iso) return '—'
  try {
    return new Date(iso).toLocaleString()
  } catch {
    return iso
  }
}

function stateClass(s: SharedBuildJobState): string {
  switch (s) {
    case 'succeeded':
      return 'st-ok'
    case 'failed':
      return 'st-fail'
    case 'canceled':
      return 'st-mute'
    case 'running':
      return 'st-run'
    default:
      return 'st-queue'
  }
}

async function load(): Promise<void> {
  // 机器列表失败才算整页错误;任务列表的失败只影响任务区(404 = 端点缺席,其余错误内联展示)。
  const [svrsRes, jobsRes] = await Promise.allSettled([listServers(), listSharedBuildJobs()])
  if (svrsRes.status === 'rejected') {
    const err = svrsRes.reason
    errorMsg.value = err instanceof Error ? err.message : String(err)
    state.value = 'error'
    return
  }
  machines.value = svrsRes.value.filter((s) => s.sharedBuild)
  state.value = 'ready'
  jobsUnavailable.value = false
  jobsError.value = ''
  if (jobsRes.status === 'fulfilled') {
    jobs.value = jobsRes.value
    return
  }
  jobs.value = []
  const err = jobsRes.reason
  if (err instanceof HttpError && err.status === 404) {
    jobsUnavailable.value = true
  } else {
    jobsError.value = err instanceof Error ? err.message : String(err)
  }
}

async function cancelJob(job: SharedBuildJob): Promise<void> {
  cancelTarget.value = job.jobId
  actionError.value = ''
  try {
    await cancelSharedBuildJob(job.jobId)
    await load()
  } catch (err) {
    actionError.value = err instanceof Error ? err.message : String(err)
  } finally {
    cancelTarget.value = ''
  }
}

onMounted(() => {
  void load()
  // 5s 轮询:排队/上机状态实时性(无 SSE,轻量页)。
  pollTimer = setInterval(() => void load(), 5000)
})
onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})
</script>

<template>
  <div class="shared-build">
    <section class="block">
      <h2>{{ t('sharedBuild.machinesTitle') }}</h2>
      <p class="hint">{{ t('sharedBuild.machinesHint') }}</p>
      <div v-if="state === 'loading'" class="muted">{{ t('sharedBuild.loading') }}</div>
      <div v-else-if="state === 'error'" class="err">{{ t('sharedBuild.loadFail') }} {{ errorMsg }}</div>
      <template v-else>
        <p v-if="machines.length === 0" class="muted">{{ t('sharedBuild.noMachines') }}</p>
        <ul v-else class="machine-list">
          <li v-for="m in machines" :key="m.id" class="machine-row">
            <span class="machine-name">{{ m.name }}</span>
            <span class="mono machine-addr">{{ m.user }}@{{ m.host }}:{{ m.port }}</span>
            <span class="badge">🏗 {{ m.labels }}</span>
            <span class="badge">⏳ ×{{ m.maxBuilds || 1 }} · P{{ m.priority }}</span>
          </li>
        </ul>
      </template>
    </section>

    <section class="block">
      <h2>{{ t('sharedBuild.jobsTitle') }}</h2>
      <p class="hint">{{ t('sharedBuild.jobsHint') }}</p>
      <p v-if="actionError" class="err">{{ actionError }}</p>
      <p v-if="state === 'ready' && jobsUnavailable" class="muted">{{ t('sharedBuild.jobsUnavailable') }}</p>
      <div v-else-if="state === 'ready' && jobsError" class="err">{{ t('sharedBuild.loadFail') }} {{ jobsError }}</div>
      <div v-else-if="state === 'ready' && jobs.length === 0" class="muted">{{ t('sharedBuild.noJobs') }}</div>
      <table v-else-if="state === 'ready'" class="jobs-table">
        <thead>
          <tr>
            <th>{{ t('sharedBuild.colPeer') }}</th>
            <th>{{ t('sharedBuild.colJob') }}</th>
            <th>{{ t('sharedBuild.colMachine') }}</th>
            <th>{{ t('sharedBuild.colState') }}</th>
            <th>{{ t('sharedBuild.colQueued') }}</th>
            <th>{{ t('sharedBuild.colEnded') }}</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="j in jobs" :key="j.jobId">
            <td>{{ j.peer || '—' }}<div class="mono sub">{{ j.runId }}</div></td>
            <td>{{ j.jobName || '—' }}</td>
            <td>{{ j.serverName || '—' }}</td>
            <td>
              <span class="state" :class="stateClass(j.state)">{{ t(`sharedBuild.state.${j.state}`) }}</span>
              <div v-if="j.state === 'failed' && j.error" class="sub err-text" :title="j.error">{{ j.error }}</div>
            </td>
            <td>{{ fmtTime(j.queuedAt) }}</td>
            <td>{{ fmtTime(j.endedAt) }}</td>
            <td>
              <button
                v-if="j.state === 'queued' || j.state === 'running'"
                type="button"
                class="btn-cancel"
                :disabled="cancelTarget === j.jobId"
                @click="cancelJob(j)"
              >
                {{ cancelTarget === j.jobId ? t('sharedBuild.cancelling') : t('sharedBuild.cancel') }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </section>
  </div>
</template>

<style scoped>
.shared-build {
  display: flex;
  flex-direction: column;
  gap: 24px;
}

.block h2 {
  font-size: var(--text-title);
  font-weight: 600;
  margin-bottom: 6px;
}

.hint {
  font-size: var(--text-label, 13px);
  color: var(--color-faint);
  margin-bottom: 12px;
}

.muted {
  color: var(--color-faint);
}

.err {
  color: var(--color-danger, #c0392b);
}

.machine-list {
  list-style: none;
  padding: 0;
  margin: 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.machine-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  padding: 8px 10px;
  border: 1px solid var(--color-border);
  border-radius: 8px;
}

.machine-name {
  font-weight: 600;
}

.machine-addr {
  color: var(--color-faint);
  font-size: var(--text-label, 13px);
}

.badge {
  font-size: var(--text-label, 13px);
  padding: 2px 8px;
  border-radius: 10px;
  background: var(--color-hover);
}

.jobs-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-body, 14px);
}

.jobs-table th {
  text-align: left;
  font-weight: 600;
  font-size: var(--text-label, 13px);
  color: var(--color-faint);
  padding: 6px 10px;
  border-bottom: 1px solid var(--color-border);
}

.jobs-table td {
  padding: 8px 10px;
  border-bottom: 1px solid var(--color-border);
  vertical-align: top;
}

.state {
  font-size: var(--text-label, 13px);
  font-weight: 600;
}

.st-ok { color: var(--color-success, #27ae60); }
.st-fail { color: var(--color-danger, #c0392b); }
.st-run { color: var(--color-primary, #2563eb); }
.st-queue { color: var(--color-faint); }
.st-mute { color: var(--color-faint); }

.sub {
  font-size: var(--text-label, 13px);
  color: var(--color-faint);
}

.err-text {
  max-width: 320px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--color-danger, #c0392b);
}

.btn-cancel {
  border: 1px solid var(--color-border);
  background: transparent;
  color: var(--color-danger, #c0392b);
  border-radius: 6px;
  padding: 4px 10px;
  cursor: pointer;
  font-size: var(--text-label, 13px);
}

.btn-cancel:hover:not(:disabled) {
  background: var(--color-hover);
}

.btn-cancel:disabled {
  opacity: 0.6;
  cursor: default;
}
</style>
