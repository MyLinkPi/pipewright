<script setup lang="ts">
/*
  Pm2.vue — pm2 进程管理总览(仿 Containers.vue)。顶部聚合 KPI + 状态筛选,下面每台
  服务器一张 Pm2ServerCard。日志走抽屉、详情走弹窗。每 12s 自动刷新聚合。
  生命周期操作按进程名批量并发(Promise.allSettled),危险动作二次确认。
*/
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { getAllPm2, getServerPm2, type ServerPm2, type Pm2Process } from '../api/pm2'
import { listServers, serviceAction, LOCAL_HOST_ID, type Server, type ServiceAction } from '../api/servers'
import { HttpError } from '../api/http'
import { pm2Bucket, type Pm2Bucket } from '../lib/pm2State'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import AppButton from '../components/ui/AppButton.vue'
import EmptyState from '../components/ui/EmptyState.vue'
import ErrorState from '../components/ui/ErrorState.vue'
import SkeletonBlock from '../components/ui/SkeletonBlock.vue'
import Pm2ServerCard from '../components/pm2/Pm2ServerCard.vue'
import Pm2LogsDrawer from '../components/pm2/Pm2LogsDrawer.vue'
import Pm2DetailModal from '../components/pm2/Pm2DetailModal.vue'

type LoadState = 'idle' | 'loading' | 'error'
type StateFilter = 'all' | Pm2Bucket

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

const loadState = ref<LoadState>('idle')
const loadError = ref('')
const groups = ref<ServerPm2[]>([])
const serverById = ref<Map<string, Server>>(new Map())
const refreshing = ref(false)

const POLL_MS = 12_000
let timer: ReturnType<typeof setInterval> | null = null

// ─── 聚合统计 ─────────────────────────────────────────────────────────────────
const totalProcesses = computed(() => groups.value.reduce((n, g) => n + g.total, 0))
const onlineProcesses = computed(() => groups.value.reduce((n, g) => n + g.online, 0))
const erroredProcesses = computed(() => {
  let n = 0
  for (const g of groups.value) for (const p of g.processes) if (pm2Bucket(p.status) === 'errored') n++
  return n
})
const stoppedProcesses = computed(() => totalProcesses.value - onlineProcesses.value - erroredProcesses.value)
const coveredServers = computed(() => groups.value.filter((g) => g.reachable && g.runtime).length)

function serverName(id: string): string {
  // 控制节点本机不在登记表里,卡片标题用固定文案。
  if (id === LOCAL_HOST_ID) return t('pm2.localName')
  return serverById.value.get(id)?.name ?? id
}
function serverHost(id: string): string {
  if (id === LOCAL_HOST_ID) return t('pm2.localHost')
  const s = serverById.value.get(id)
  return s ? `${s.user}@${s.host}:${s.port}` : ''
}

// ─── 状态筛选 ─────────────────────────────────────────────────────────────────
const stateFilter = ref<StateFilter>('all')

const bucketCounts = computed(() => {
  let online = 0
  let stopped = 0
  let errored = 0
  for (const g of groups.value) {
    for (const p of g.processes) {
      const b = pm2Bucket(p.status)
      if (b === 'online') online++
      else if (b === 'errored') errored++
      else stopped++
    }
  }
  return { all: online + stopped + errored, online, stopped, errored }
})

interface FilterOption {
  key: StateFilter
  labelKey: string
}
const FILTER_OPTIONS: FilterOption[] = [
  { key: 'all', labelKey: 'pm2.filterAll' },
  { key: 'online', labelKey: 'pm2.filterOnline' },
  { key: 'stopped', labelKey: 'pm2.filterStopped' },
  { key: 'errored', labelKey: 'pm2.filterErrored' },
]
function filterCount(key: StateFilter): number {
  return bucketCounts.value[key]
}

// ─── 文本搜索 ─────────────────────────────────────────────────────────────────
// 透传给各 Pm2ServerCard,与状态筛选叠加(按名称 / 脚本,忽略大小写)。
const searchText = ref('')

// ─── 批量操作 ─────────────────────────────────────────────────────────────────
// 父统一持有选择集;key = `${serverId}::${processName}`(按进程名,cluster 全实例生效)。
const bulkMode = ref(false)
const selected = ref<Set<string>>(new Set())
const bulkBusy = ref(false)
const selectedCount = computed(() => selected.value.size)

function selectKey(serverId: string, name: string): string {
  return `${serverId}::${name}`
}
function toggleSelect(serverId: string, name: string): void {
  const key = selectKey(serverId, name)
  const next = new Set(selected.value)
  if (next.has(key)) next.delete(key)
  else next.add(key)
  selected.value = next
}
function clearSelection(): void {
  selected.value = new Set()
}
function toggleBulkMode(): void {
  bulkMode.value = !bulkMode.value
  if (!bulkMode.value) clearSelection()
}

interface BulkAction {
  action: ServiceAction
  labelKey: string
  danger: boolean
}
const BULK_ACTIONS: BulkAction[] = [
  { action: 'start', labelKey: 'pm2.actionStart', danger: false },
  { action: 'stop', labelKey: 'pm2.actionStop', danger: true },
  { action: 'restart', labelKey: 'pm2.actionRestart', danger: true },
  { action: 'delete', labelKey: 'pm2.actionDelete', danger: true },
]

async function runBulk({ action, labelKey, danger }: BulkAction): Promise<void> {
  if (selected.value.size === 0 || bulkBusy.value) return
  const label = t(labelKey)
  const targets = [...selected.value].map((key) => {
    const i = key.indexOf('::')
    return { serverId: key.slice(0, i), name: key.slice(i + 2) }
  })
  const n = targets.length
  if (danger) {
    const ok = await confirm.open({
      title: t('pm2.confirmTitle', { label, n }),
      body:
        action === 'delete'
          ? t('pm2.confirmBodyDelete', { label, n })
          : t('pm2.confirmBodyAction', { label, n }),
      confirmLabel: t('pm2.confirmLabel', { label, n }),
      variant: 'danger',
    })
    if (!ok) return
  }
  bulkBusy.value = true
  try {
    const results = await Promise.allSettled(
      targets.map((tg) => serviceAction(tg.serverId, { type: 'pm2', target: tg.name, action })),
    )
    let okCount = 0
    let failCount = 0
    for (const r of results) {
      if (r.status === 'fulfilled' && r.value.ok) okCount++
      else failCount++
    }
    if (failCount === 0)
      toast.success(t('pm2.toastDone', { label }), {
        detail: t('pm2.toastDoneDetail', { n: okCount }),
      })
    else if (okCount === 0)
      toast.error(t('pm2.toastFail', { label }), {
        detail: t('pm2.toastFailDetail', { n: failCount }),
      })
    else
      toast.warn(t('pm2.toastPartial', { label }), {
        detail: t('pm2.toastPartialDetail', { ok: okCount, fail: failCount }),
      })
    clearSelection()
    await load()
  } finally {
    bulkBusy.value = false
  }
}

// ─── 日志抽屉 / 详情弹窗 ──────────────────────────────────────────────────────
const logsTarget = ref<{ serverId: string; process: Pm2Process } | null>(null)
function openLogs(serverId: string, p: Pm2Process): void {
  logsTarget.value = { serverId, process: p }
}

const detailTarget = ref<{ serverId: string; process: Pm2Process } | null>(null)
function openDetail(serverId: string, p: Pm2Process): void {
  detailTarget.value = { serverId, process: p }
}

// ─── 主机终端(卡片头部「终端」按钮) ────────────────────────────────────────────
// 新标签页打开全屏运维终端页(复用 server-terminal;本机 __local__ 走本地 PTY)。
const router = useRouter()
function openTerminal(serverId: string): void {
  const url = router.resolve({ name: 'server-terminal', params: { id: serverId } }).href
  window.open(url, '_blank', 'noopener')
}

// ─── 加载 ─────────────────────────────────────────────────────────────────────
async function load(): Promise<void> {
  const firstLoad = groups.value.length === 0
  loadState.value = 'loading'
  loadError.value = ''
  if (!firstLoad) refreshing.value = true
  try {
    // 并行:登记服务器 + 各台 pm2 聚合 + **控制节点本机**pm2(serverID = __local__)。
    // 本机请求失败(接口 5xx)只让本机卡消失,绝不拖垮已登记服务器的展示。
    const [servers, items, local] = await Promise.all([
      listServers(),
      getAllPm2(),
      getServerPm2(LOCAL_HOST_ID).catch(() => null),
    ])
    const m = new Map<string, Server>()
    for (const s of servers) m.set(s.id, s)
    serverById.value = m
    // 本机置首:它的 pm2 清单由后端在**本机**执行(不经 SSH)。
    groups.value = local ? [local, ...items] : items
    loadState.value = 'idle'
  } catch (err) {
    if (err instanceof HttpError) {
      loadError.value =
        err.status === 0
          ? t('pm2.errConnect')
          : (err.apiError?.message ?? t('pm2.errLoadStatus', { status: err.status }))
    } else {
      loadError.value = t('pm2.errLoadRetry')
    }
    loadState.value = 'error'
  } finally {
    refreshing.value = false
  }
}

onMounted(() => {
  void load()
  timer = setInterval(() => void load(), POLL_MS)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>

<template>
  <div class="pm2-view">
    <header class="view-header">
      <div class="view-header__text">
        <h1 class="view-title">{{ t('pm2.title') }}</h1>
        <p class="view-sub">
          {{ t('pm2.subtitle') }}
          <span v-if="totalProcesses > 0" class="view-sub__count">
            {{ t('pm2.countSummary', { total: totalProcesses, online: onlineProcesses }) }}
          </span>
          <span class="view-sub__count">{{ t('pm2.autoRefresh', { n: POLL_MS / 1000 }) }}</span>
        </p>
      </div>
      <div class="header-actions">
        <AppButton :variant="bulkMode ? 'primary' : 'default'" @click="toggleBulkMode">
          {{ bulkMode ? t('pm2.bulkExit') : t('pm2.bulkEnter') }}
        </AppButton>
        <AppButton variant="default" :loading="loadState === 'loading' && groups.length === 0" @click="load">
          {{ t('common.refresh') }}
        </AppButton>
      </div>
    </header>

    <!-- 首屏骨架 -->
    <div v-if="loadState === 'loading' && groups.length === 0" class="skeletons" aria-busy="true" :aria-label="t('pm2.loadingAria')">
      <div v-for="n in 2" :key="n" class="skeleton-panel">
        <SkeletonBlock :height="20" width="40%" />
        <SkeletonBlock :height="48" width="100%" />
        <SkeletonBlock :height="48" width="100%" />
      </div>
    </div>

    <ErrorState
      v-else-if="loadState === 'error' && groups.length === 0"
      :title="t('pm2.errTitle')"
      :description="loadError"
      @retry="load"
    />

    <EmptyState
      v-else-if="groups.length === 0"
      :title="t('pm2.emptyTitle')"
      :description="t('pm2.emptyDesc')"
    />

    <template v-else>
      <!-- 聚合 KPI 条 -->
      <section class="kpi-strip" :aria-label="t('pm2.kpiStripAria')">
        <div class="kpi kpi--total">
          <span class="kpi__num">{{ totalProcesses }}</span>
          <span class="kpi__label">{{ t('pm2.kpiTotal') }}</span>
        </div>
        <div class="kpi kpi--online">
          <span class="kpi__num">{{ onlineProcesses }}</span>
          <span class="kpi__label">{{ t('pm2.kpiOnline') }}</span>
        </div>
        <div class="kpi kpi--stopped">
          <span class="kpi__num">{{ stoppedProcesses }}</span>
          <span class="kpi__label">{{ t('pm2.kpiStopped') }}</span>
        </div>
        <div class="kpi kpi--errored">
          <span class="kpi__num">{{ erroredProcesses }}</span>
          <span class="kpi__label">{{ t('pm2.kpiErrored') }}</span>
        </div>
        <div class="kpi kpi--hosts">
          <span class="kpi__num">{{ coveredServers }}<span class="kpi__den">/{{ groups.length }}</span></span>
          <span class="kpi__label">{{ t('pm2.kpiHosts') }}</span>
        </div>
      </section>

      <!-- 状态筛选段 + 文本搜索(均作用于各卡片的进程行) -->
      <div class="controls-row">
        <div class="filter-bar" role="group" :aria-label="t('pm2.filterAria')">
          <button
            v-for="f in FILTER_OPTIONS"
            :key="f.key"
            class="filter-chip"
            :class="{ 'filter-chip--active': stateFilter === f.key }"
            :aria-pressed="stateFilter === f.key"
            @click="stateFilter = f.key"
          >
            {{ t(f.labelKey) }}
            <span class="filter-chip__count">{{ filterCount(f.key) }}</span>
          </button>
        </div>
        <div class="search-box">
          <input
            v-model="searchText"
            type="search"
            class="search-box__input"
            :placeholder="t('pm2.searchPlaceholder')"
            :aria-label="t('pm2.searchAria')"
          />
          <button v-if="searchText" class="search-box__clear" :title="t('pm2.searchClear')" @click="searchText = ''">✕</button>
        </div>
      </div>

      <!-- 批量操作条 -->
      <div v-if="bulkMode" class="bulk-bar" role="group" :aria-label="t('pm2.bulkAria')">
        <span class="bulk-bar__count">{{ t('pm2.bulkSelected') }} <strong>{{ selectedCount }}</strong> {{ t('pm2.bulkSelectedUnit') }}</span>
        <div class="bulk-bar__actions">
          <button
            v-for="a in BULK_ACTIONS"
            :key="a.action"
            class="bulk-btn"
            :class="{ 'bulk-btn--danger': a.danger }"
            :disabled="selectedCount === 0 || bulkBusy"
            @click="runBulk(a)"
          >
            {{ t(a.labelKey) }}
          </button>
          <button class="bulk-btn bulk-btn--ghost" :disabled="selectedCount === 0 || bulkBusy" @click="clearSelection">
            {{ t('pm2.bulkClear') }}
          </button>
        </div>
      </div>

      <!-- 逐台卡片 -->
      <section class="cards" :aria-label="t('pm2.cardsAria')" :aria-busy="refreshing || undefined">
        <Pm2ServerCard
          v-for="g in groups"
          :key="g.serverId"
          :group="g"
          :name="serverName(g.serverId)"
          :host="serverHost(g.serverId)"
          :state-filter="stateFilter"
          :search="searchText"
          :bulk-mode="bulkMode"
          :selected-set="selected"
          @toggle-select="(name) => toggleSelect(g.serverId, name)"
          @changed="load"
          @logs="(p) => openLogs(g.serverId, p)"
          @detail="(p) => openDetail(g.serverId, p)"
          @terminal="openTerminal(g.serverId)"
        />
      </section>
    </template>

    <!-- pm2 进程日志抽屉 -->
    <Pm2LogsDrawer
      v-if="logsTarget"
      :server-id="logsTarget.serverId"
      :process-name="logsTarget.process.name"
      :pm-id="logsTarget.process.id"
      @close="logsTarget = null"
    />

    <!-- pm2 进程详情弹窗 -->
    <Pm2DetailModal
      v-if="detailTarget"
      :process="detailTarget.process"
      @close="detailTarget = null"
    />
  </div>
</template>

<style scoped>
.pm2-view {
  display: flex;
  flex-direction: column;
  gap: 22px;
}

.view-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}
.view-header__text {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.header-actions {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-shrink: 0;
}
.view-title {
  margin: 0;
  font-size: var(--text-h1);
  font-weight: 700;
  color: var(--color-text);
}
.view-sub {
  margin: 0;
  font-size: var(--text-label);
  color: var(--color-dim);
}
.view-sub__count {
  color: var(--color-faint);
  font-variant-numeric: tabular-nums;
}

/* 筛选段 + 搜索同一行 */
.controls-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 12px;
}

/* 状态筛选段 */
.filter-bar {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

/* 文本搜索 */
.search-box {
  position: relative;
  display: inline-flex;
  align-items: center;
  flex: 0 1 320px;
  min-width: 200px;
}
.search-box__input {
  width: 100%;
  padding: 7px 30px 7px 13px;
  font-size: var(--text-label);
  color: var(--color-text);
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: 999px;
  transition: border-color var(--duration-fast) var(--ease-out-expo);
}
.search-box__input::placeholder {
  color: var(--color-faint);
}
.search-box__input:focus {
  outline: none;
  border-color: var(--color-primary);
}
.search-box__clear {
  position: absolute;
  right: 10px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 16px;
  height: 16px;
  font-size: 11px;
  line-height: 1;
  color: var(--color-faint);
  background: transparent;
  border: none;
  border-radius: 50%;
  cursor: pointer;
}
.search-box__clear:hover {
  color: var(--color-text);
}

/* 批量操作条 */
.bulk-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 12px;
  padding: 10px 16px;
  background: var(--color-card-2);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
}
.bulk-bar__count {
  font-size: var(--text-label);
  color: var(--color-dim);
}
.bulk-bar__count strong {
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}
.bulk-bar__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.bulk-btn {
  font-size: var(--text-label);
  font-weight: 600;
  padding: 6px 14px;
  border-radius: var(--rounded-sm);
  border: 1px solid var(--color-border-strong);
  background: var(--color-card);
  color: var(--color-text);
  cursor: pointer;
  transition: all var(--duration-fast) var(--ease-out-expo);
}
.bulk-btn:hover:not(:disabled) {
  border-color: var(--color-primary);
  color: var(--color-primary);
}
.bulk-btn--danger:hover:not(:disabled) {
  border-color: var(--color-red);
  color: var(--color-red);
}
.bulk-btn--ghost {
  color: var(--color-dim);
}
.bulk-btn--ghost:hover:not(:disabled) {
  color: var(--color-text);
  border-color: var(--color-text);
}
.bulk-btn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.filter-chip {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  padding: 6px 13px;
  font-size: var(--text-label);
  font-weight: 600;
  color: var(--color-dim);
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: 999px;
  cursor: pointer;
  transition: color var(--duration-fast) var(--ease-out-expo), border-color var(--duration-fast) var(--ease-out-expo), background var(--duration-fast) var(--ease-out-expo);
}
.filter-chip:hover {
  color: var(--color-text);
  border-color: var(--color-border-strong);
}
.filter-chip--active {
  color: #fff;
  background: var(--color-primary);
  border-color: var(--color-primary);
}
.filter-chip__count {
  font-size: var(--text-micro);
  font-variant-numeric: tabular-nums;
  padding: 1px 7px;
  border-radius: 999px;
  background: var(--color-inset);
  color: var(--color-faint);
}
.filter-chip--active .filter-chip__count {
  background: oklch(100% 0 0 / 0.22);
  color: #fff;
}

/* KPI 条 */
.kpi-strip {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: 12px;
}
.kpi {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 16px 18px;
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  overflow: hidden;
}
.kpi::before {
  content: '';
  position: absolute;
  inset: 0 auto 0 0;
  width: 3px;
  background: var(--color-faint);
}
.kpi--online::before { background: var(--color-green); }
.kpi--stopped::before { background: var(--color-faint); }
.kpi--errored::before { background: var(--color-red); }
.kpi--total::before { background: var(--color-primary); }
.kpi--hosts::before { background: var(--color-cyan); }
.kpi__num {
  font-size: var(--text-kpi);
  font-weight: 700;
  line-height: 1;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}
.kpi__den {
  font-size: var(--text-body);
  font-weight: 600;
  color: var(--color-faint);
}
.kpi__label {
  font-size: var(--text-caps);
  letter-spacing: 0.04em;
  text-transform: uppercase;
  color: var(--color-faint);
}

/* 卡片列表 */
.cards {
  display: flex;
  flex-direction: column;
  gap: 16px;
  transition: opacity var(--duration-fast) var(--ease-out-expo);
}
.cards[aria-busy='true'] {
  opacity: 0.7;
}

.skeletons {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.skeleton-panel {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 18px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-lg, 12px);
  background: var(--color-card);
}
</style>
