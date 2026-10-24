<script setup lang="ts">
/**
 * MyProjects — 普通用户「我的项目」(/my-projects)。
 *
 * 只读列表:后端 `GET /api/projects` 已按成员项目过滤(memberGate + ListPagedByIDs),
 * 前端无需再过滤。行内「运行」跳转到复用的运行页(/my-runs?projectId=<id>)。
 * 无创建 / 编辑 / 删除 / 代码浏览等管理员入口。
 */
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import AppButton from '../components/ui/AppButton.vue'
import EmptyState from '../components/ui/EmptyState.vue'
import SkeletonBlock from '../components/ui/SkeletonBlock.vue'
import { listProjects, type Project, type RunStatus } from '../api/projects'

const { t } = useI18n()
const router = useRouter()

type LoadState = 'loading' | 'idle' | 'error'

const loadState = ref<LoadState>('loading')
const loadError = ref('')
const projects = ref<Project[]>([])

// 后端状态词表与共享 runStatus.* 词条一一对应(与 Projects.vue 同一映射,8 语言齐备)。
function runStatusLabel(s: RunStatus): string {
  return t(`runStatus.${s}`)
}

// 相对时间(与 Projects.vue 卡片同一文案:刚刚 / N 分钟前 / N 小时前 / N 天前)。
function relativeTime(isoStr: string): string {
  const diff = Date.now() - new Date(isoStr).getTime()
  const s = Math.floor(diff / 1000)
  if (s < 60) return t('time.justNow')
  const m = Math.floor(s / 60)
  if (m < 60) return t('time.minAgo', { n: m })
  const h = Math.floor(m / 60)
  if (h < 24) return t('time.hourAgo', { n: h })
  const d = Math.floor(h / 24)
  return t('time.dayAgo', { n: d })
}

const isEmpty = computed(() => loadState.value === 'idle' && projects.value.length === 0)

async function load(): Promise<void> {
  loadState.value = 'loading'
  loadError.value = ''
  try {
    projects.value = await listProjects()
    loadState.value = 'idle'
  } catch (err) {
    loadError.value = err instanceof Error ? err.message : t('myProjects.loadError')
    loadState.value = 'error'
  }
}

function goRun(id: string): void {
  void router.push({ name: 'my-runs', query: { projectId: id } })
}

onMounted(load)
</script>

<template>
  <div class="my-projects">
    <header class="page-head">
      <div>
        <h1 class="page-title">{{ t('myProjects.title') }}</h1>
        <p class="page-sub">{{ t('myProjects.subtitle') }}</p>
      </div>
    </header>

    <!-- loading -->
    <div v-if="loadState === 'loading'" class="stack" aria-busy="true">
      <SkeletonBlock :height="64" />
      <SkeletonBlock :height="64" />
      <SkeletonBlock :height="64" />
    </div>

    <!-- error -->
    <p v-else-if="loadState === 'error'" class="load-error" role="alert">
      {{ loadError }}
      <button type="button" class="link" @click="load">{{ t('myProjects.retry') }}</button>
    </p>

    <!-- empty -->
    <EmptyState
      v-else-if="isEmpty"
      :title="t('myProjects.emptyTitle')"
      :description="t('myProjects.emptyDesc')"
    />

    <!-- table -->
    <div v-else class="table-wrap">
      <table class="table">
        <thead>
          <tr>
            <th scope="col">{{ t('myProjects.colName') }}</th>
            <th scope="col">{{ t('myProjects.colRepo') }}</th>
            <th scope="col">{{ t('myProjects.colBranch') }}</th>
            <th scope="col">{{ t('myProjects.colLastRun') }}</th>
            <th scope="col" class="col-act"><span class="sr-only">{{ t('myProjects.runBtn') }}</span></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="p in projects" :key="p.id">
            <td class="cell-name">{{ p.name }}</td>
            <td class="cell-mono">{{ p.repoUrl }}</td>
            <td class="cell-mono">{{ p.defaultBranch || '—' }}</td>
            <td>
              <span
                v-if="p.lastRunStatus"
                class="status"
                role="img"
                :aria-label="t('projects.runStatusAria', { status: runStatusLabel(p.lastRunStatus) })"
              >{{ runStatusLabel(p.lastRunStatus) }}</span>
              <span v-if="p.lastRunStatus && p.lastRunAt" class="cell-time" :title="p.lastRunAt">
                {{ relativeTime(p.lastRunAt) }}
              </span>
              <span v-if="!p.lastRunStatus" class="status status--none">{{ t('myProjects.lastRunNone') }}</span>
            </td>
            <td class="col-act">
              <AppButton variant="primary" @click="goRun(p.id)">{{ t('myProjects.runBtn') }}</AppButton>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<style scoped>
.my-projects {
  display: flex;
  flex-direction: column;
  gap: 18px;
}

.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
}
.page-title {
  font-size: 1.25rem;
  font-weight: 700;
  color: var(--color-text);
}
.page-sub {
  font-size: 0.83rem;
  color: var(--color-faint);
  margin-top: 5px;
}

.stack {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.load-error {
  font-size: 0.86rem;
  color: var(--color-red);
}
.link {
  background: none;
  border: none;
  color: var(--color-primary);
  cursor: pointer;
  font: inherit;
  text-decoration: underline;
  text-underline-offset: 2px;
  padding: 0;
  margin-left: 6px;
}

.table-wrap {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  background: var(--color-card);
  box-shadow: var(--shadow);
  overflow-x: auto;
}
.table {
  width: 100%;
  border-collapse: collapse;
  font-size: 0.86rem;
}
.table th {
  text-align: left;
  font-weight: 600;
  color: var(--color-faint);
  font-size: 0.76rem;
  padding: 12px 14px;
  border-bottom: 1px solid var(--color-border);
  white-space: nowrap;
}
.table td {
  padding: 13px 14px;
  border-bottom: 1px solid var(--color-border);
  color: var(--color-text);
  vertical-align: middle;
}
.table tbody tr:last-child td {
  border-bottom: none;
}
.cell-name {
  font-weight: 600;
}
.cell-mono {
  font-family: var(--font-mono);
  font-size: 0.8rem;
  color: var(--color-dim);
  max-width: 320px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.col-act {
  text-align: right;
  width: 1%;
  white-space: nowrap;
}
.status {
  display: inline-block;
  font-size: 0.72rem;
  font-weight: 600;
  padding: 2px 9px;
  border-radius: var(--rounded-sm);
  background: var(--color-card-2);
  border: 1px solid var(--color-border-strong);
  color: var(--color-dim);
}
.cell-time {
  margin-left: 8px;
  font-size: 0.76rem;
  color: var(--color-faint);
  white-space: nowrap;
}
.sr-only {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}
</style>
