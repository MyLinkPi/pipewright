<!--
  ServerStatus.vue — Story 6-1: 多机状态总览(FR-15,服务器层资源指标)

  在一个面板看所有已登记服务器的 CPU 负载 / 内存 / 磁盘使用,免逐台 SSH。
    · 登记服务器列表来自 4-1;指标经 6-1 批量端点并行采集(每台独立)。
    · 某台不可达 → 该卡灰显 + 人读错误,不连累其它台。
    · 某指标缺失(跨平台 best-effort,如 macOS 无 free)→ 该行「不可用」。
    · 「刷新」重取(指标实时、不落库)。
    · 筛选:名称/host 搜索 × 可达性 × 资源紧张(阈值与卡片红色着色一致,见 lib/serverTight)。
    · 终端:可达机器(含本机控制节点)新标签页打开主机 shell(复用 server-terminal 页)。
    · ECS 电源:导入的阿里云 ECS 机器显示电源状态徽标 + 开/关机按钮(发起即返回,关机为
      节省停机模式且需二次确认;电源状态独立 30s 轮询,未配置阿里云时整体隐藏)。

  这是一个**新增**的总览入口,不动 4-1 SettingsServers CRUD、6-2 日志入口。

  另:网格**置首**展示一张「本机(控制节点)」只读卡牌 —— 运行 Pipewright 的这台主机自身的
  CPU/内存/磁盘,由 GET /api/system/metrics 在**本地**采集(不走 SSH、不占登记服务器名额)。
  本机卡不可勾选(不参与批量命令)、无单机 AI 入口;本机请求失败只隐藏该卡,不影响服务器卡。
-->
<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { Robot, Refresh, Terminal, Power } from '@vicons/tabler'
import {
  getAllServerMetrics,
  getLocalHostMetrics,
  listServers,
  LOCAL_HOST_ID,
  type Server,
  type ServerMetrics,
} from '../api/servers'
import { HttpError } from '../api/http'
import { aliyunServerPower, getAliyunPowerStatus } from '../api/aliyunEcs'
import { isServerTight } from '../lib/serverTight'
import { useConfirm } from '../composables/useConfirm'
import { useToast } from '../composables/useToast'
import ServerMetricsCard from '../components/ops/ServerMetricsCard.vue'
import BatchCommandModal from '../components/ops/BatchCommandModal.vue'
import CommandHistoryDrawer, { type RerunPayload } from '../components/ops/CommandHistoryDrawer.vue'
import ContainerAiPanel from '../components/ops/ContainerAiPanel.vue'
import AppButton from '../components/ui/AppButton.vue'
import EmptyState from '../components/ui/EmptyState.vue'
import ErrorState from '../components/ui/ErrorState.vue'
import SkeletonBlock from '../components/ui/SkeletonBlock.vue'

type LoadState = 'idle' | 'loading' | 'error'

const { t } = useI18n()
const router = useRouter()
const toast = useToast()
const confirm = useConfirm()
const aiTargets = ref<string[] | null>(null)

/** 卡片标签区需要的登记信息(纯展示,不含凭据引用)。 */
interface ServerPoolInfo {
  /** 逗号分隔的构建池标签;空串 = 非构建机。 */
  labels: string
  /** 并发构建槽位(0 = 全局默认);tooltip 用。 */
  maxBuilds: number
  /** 调度优先级 0-100;tooltip 用。 */
  priority: number
  /** GPU 机型开关(勾选后卡片额外展示每张显卡的指标)。 */
  gpu: boolean
}

const loadState = ref<LoadState>('idle')
const loadError = ref('')
const metrics = ref<ServerMetrics[]>([])
/** 本机(控制节点)指标;不可采(请求失败 / 前端未知) → null,不渲染本机卡。 */
const localMetrics = ref<ServerMetrics | null>(null)
/** serverId → 展示名(来自登记列表,用于卡片标题)。 */
const nameById = ref<Map<string, string>>(new Map())
/** serverId → 登记行(名称搜索要匹配 host,取自这里)。 */
const serverById = ref<Map<string, Server>>(new Map())
/** serverId → 构建池信息(卡片标签 chips 展示用)。 */
const poolById = ref<Map<string, ServerPoolInfo>>(new Map())

/** 首次加载尚无数据时显示骨架;刷新时保留旧数据(stale-while-revalidate)。 */
const refreshing = ref(false)

const reachableCount = computed(() => metrics.value.filter((m) => m.reachable).length)
const totalCount = computed(() => metrics.value.length)

// ─── 筛选(名称搜索 × 可达性 × 资源状态,仿容器页 controls-row) ────────────────

type ReachFilter = 'all' | 'reachable' | 'unreachable'
type TightFilter = 'all' | 'tight' | 'ok'

const searchText = ref('')
const reachFilter = ref<ReachFilter>('all')
const tightFilter = ref<TightFilter>('all')

const REACH_FILTERS: { key: ReachFilter; labelKey: string }[] = [
  { key: 'all', labelKey: 'serverStatus.filterAll' },
  { key: 'reachable', labelKey: 'serverStatus.filterReachable' },
  { key: 'unreachable', labelKey: 'serverStatus.filterUnreachable' },
]
const TIGHT_FILTERS: { key: TightFilter; labelKey: string }[] = [
  { key: 'all', labelKey: 'serverStatus.filterAll' },
  { key: 'tight', labelKey: 'serverStatus.filterTight' },
  { key: 'ok', labelKey: 'serverStatus.filterOk' },
]

/** 计数基于全量(不随其它筛选条件收缩,与容器页一致)。 */
const reachCounts = computed(() => {
  let reachable = 0
  for (const m of metrics.value) {
    if (m.reachable) reachable++
  }
  return { all: metrics.value.length, reachable, unreachable: metrics.value.length - reachable }
})
const tightCounts = computed(() => {
  let tight = 0
  for (const m of metrics.value) {
    if (isServerTight(m)) tight++
  }
  return { all: metrics.value.length, tight, ok: metrics.value.length - tight }
})

/** 三维叠加过滤后的卡片列表(本机卡固定置首,不参与筛选)。 */
const filteredMetrics = computed(() => {
  const q = searchText.value.trim().toLowerCase()
  return metrics.value.filter((m) => {
    if (q) {
      const name = (nameById.value.get(m.serverId) ?? '').toLowerCase()
      const host = (serverById.value.get(m.serverId)?.host ?? '').toLowerCase()
      if (!name.includes(q) && !host.includes(q)) return false
    }
    if (reachFilter.value === 'reachable' && !m.reachable) return false
    if (reachFilter.value === 'unreachable' && m.reachable) return false
    if (tightFilter.value === 'tight' && !isServerTight(m)) return false
    if (tightFilter.value === 'ok' && isServerTight(m)) return false
    return true
  })
})

// ─── 终端(可达机器新标签页打开主机 shell;本机走本地 PTY) ─────────────────────

function openTerminal(serverId: string): void {
  const url = router.resolve({ name: 'server-terminal', params: { id: serverId } }).href
  window.open(url, '_blank', 'noopener')
}

// ─── ECS 电源(仅导入的阿里云 ECS 机器;失败/未配置整体隐藏电源 UI) ────────────

/** 一台已关联 ECS 的电源信息(状态空 = 未知,如账户凭据失效)。 */
interface EcsInfo {
  status: string
  instanceId: string
  error?: string
}
const ecsById = ref<Map<string, EcsInfo> | null>(null)
/** 正在发起开关机的机器(防重复点击)。 */
const powerBusy = ref<Set<string>>(new Set())

async function loadPowerStatus(): Promise<void> {
  if (loadPowerStatusInflight) return
  loadPowerStatusInflight = true
  try {
    const items = await getAliyunPowerStatus()
    const map = new Map<string, EcsInfo>()
    for (const it of items) {
      // 一台服务器多账户映射罕见;取第一条,不让后来者覆盖已正常解析的条目。
      if (!map.has(it.serverId)) {
        map.set(it.serverId, { status: it.status, instanceId: it.instanceId, error: it.error })
      }
    }
    ecsById.value = map
  } catch {
    // 未配置阿里云服务(503)/网络失败 → 整体隐藏电源 UI,不打扰状态主流程。
    ecsById.value = null
    stopPowerPolling()
  } finally {
    loadPowerStatusInflight = false
  }
}
let loadPowerStatusInflight = false

/** 电源按钮渲染信息;null = 非 ECS 机器(不渲染按钮)。 */
interface PowerButton {
  key: 'start' | 'stop' | 'busy' | 'starting' | 'stopping' | 'unknown'
  label: string
  disabled: boolean
  danger: boolean
}
function powerBtn(m: ServerMetrics): PowerButton | null {
  const info = ecsById.value?.get(m.serverId)
  if (!info) return null
  if (powerBusy.value.has(m.serverId)) {
    return { key: 'busy', label: t('serverStatus.powerBusy'), disabled: true, danger: false }
  }
  switch (info.status) {
    case 'Running':
      return { key: 'stop', label: t('serverStatus.powerStop'), disabled: false, danger: true }
    case 'Stopped':
      return { key: 'start', label: t('serverStatus.powerStart'), disabled: false, danger: false }
    case 'Starting':
      return { key: 'starting', label: t('serverStatus.ecsStarting'), disabled: true, danger: false }
    case 'Stopping':
      return { key: 'stopping', label: t('serverStatus.ecsStopping'), disabled: true, danger: false }
    default:
      return { key: 'unknown', label: t('serverStatus.powerUnknown'), disabled: true, danger: false }
  }
}

async function powerServer(m: ServerMetrics): Promise<void> {
  const btn = powerBtn(m)
  if (!btn || btn.disabled || powerBusy.value.has(m.serverId)) return
  const action = btn.key === 'stop' ? 'stop' : 'start'
  const name = displayName(m)
  if (action === 'stop') {
    // 关机高危:中断构建/服务,二次确认。
    const ok = await confirm.open({
      title: t('serverStatus.powerConfirmTitle', { name }),
      body: t('serverStatus.powerConfirmBody'),
      confirmLabel: t('serverStatus.powerConfirmLabel'),
      variant: 'danger',
    })
    if (!ok) return
  }
  const busy = new Set(powerBusy.value)
  busy.add(m.serverId)
  powerBusy.value = busy
  try {
    const res = await aliyunServerPower(m.serverId, action)
    if (res.failed > 0) {
      const firstErr = res.items.find((it) => !it.ok)?.error ?? ''
      toast.error(t('serverStatus.powerFailedToast'), { detail: firstErr })
    } else {
      toast.success(
        action === 'start' ? t('serverStatus.powerStartToast') : t('serverStatus.powerStopToast'),
      )
    }
    // 发起后立即补一次(转换中),几秒后再补一次(接近目标态);可达性由 12s 指标轮询自然跟进。
    void loadPowerStatus()
    window.setTimeout(() => void loadPowerStatus(), 6000)
  } catch (err) {
    toast.error(t('serverStatus.powerFailedToast'), {
      detail: err instanceof HttpError ? (err.apiError?.message ?? `${err.status}`) : undefined,
    })
  } finally {
    const done = new Set(powerBusy.value)
    done.delete(m.serverId)
    powerBusy.value = done
  }
}

// 电源状态轮询节奏:比指标慢(实例状态变化以十秒计,少打阿里云 API)。
const POWER_REFRESH_MS = 30_000
let powerTimer: ReturnType<typeof setInterval> | null = null

function startPowerPolling(): void {
  if (powerTimer !== null || ecsById.value === null) return
  powerTimer = setInterval(() => void loadPowerStatus(), POWER_REFRESH_MS)
}
function stopPowerPolling(): void {
  if (powerTimer !== null) {
    clearInterval(powerTimer)
    powerTimer = null
  }
}

// ─── 批量执行命令(勾选 → 弹窗执行 → 历史回看) ─────────────────────────────

/** 勾选的目标机 id 集合(卡片复选框)。 */
const selected = ref<Set<string>>(new Set())
const selectedCount = computed(() => selected.value.size)

function toggleSelect(serverId: string): void {
  const next = new Set(selected.value)
  if (next.has(serverId)) {
    next.delete(serverId)
  } else {
    next.add(serverId)
  }
  selected.value = next
}

function selectAll(): void {
  // 只选当前可见(筛选后)的机器,与直觉一致。
  selected.value = new Set(filteredMetrics.value.map((m) => m.serverId))
}

function clearSelection(): void {
  selected.value = new Set()
}

/** 弹窗目标(进入时的机器名单 + 预填命令;重跑入口回填)。 */
const commandModal = ref<{ servers: { id: string; name: string }[]; command: string } | null>(null)
const historyOpen = ref(false)

function openBatchCommand(): void {
  if (selectedCount.value === 0) return
  commandModal.value = {
    servers: [...selected.value].map((id) => ({ id, name: nameById.value.get(id) ?? id })),
    command: '',
  }
}

function openRerun(payload: RerunPayload): void {
  if (payload.serverIds.length === 0) return
  historyOpen.value = false
  commandModal.value = {
    servers: payload.serverIds.map((id) => ({ id, name: nameById.value.get(id) ?? id })),
    command: payload.command,
  }
}

function displayName(m: ServerMetrics): string {
  return nameById.value.get(m.serverId) ?? m.serverId
}

async function load(): Promise<void> {
  const firstLoad = metrics.value.length === 0
  loadState.value = 'loading'
  loadError.value = ''
  if (!firstLoad) refreshing.value = true
  try {
    // 并行:登记列表(取展示名 + 构建池标签)+ 批量指标 + 本机(控制节点)指标。互不阻塞。
    // 本机请求失败(接口 5xx)只让本机卡消失,绝不拖垮已登记服务器的采集展示。
    const [servers, items, local] = await Promise.all([
      listServers(),
      getAllServerMetrics(),
      getLocalHostMetrics().catch(() => null),
    ])
    nameById.value = new Map(servers.map((s) => [s.id, s.name]))
    serverById.value = new Map(servers.map((s) => [s.id, s]))
    poolById.value = new Map(
      servers.map((s) => [
        s.id,
        { labels: s.labels, maxBuilds: s.maxBuilds, priority: s.priority, gpu: s.gpu ?? false },
      ]),
    )
    metrics.value = items
    localMetrics.value = local
    loadState.value = 'idle'
  } catch (err) {
    if (err instanceof HttpError) {
      loadError.value =
        err.status === 0
          ? t('serverStatus.errConnect')
          : (err.apiError?.message ?? t('serverStatus.errLoadStatus', { status: err.status }))
    } else {
      loadError.value = t('serverStatus.errLoadRetry')
    }
    loadState.value = 'error'
  } finally {
    refreshing.value = false
  }
}

// ─── 自动刷新 ────────────────────────────────────────────────────────────────
// 指标实时、不落库,这里定时轮询(stale-while-revalidate:旧数据留屏、后台静默重取)。
// 标签页隐藏时暂停(省 SSH;终端常在新标签开,这页可能被晾在后台),回到前台立即补一次。
const REFRESH_INTERVAL_MS = 12_000
let pollTimer: ReturnType<typeof setInterval> | null = null

function startPolling(): void {
  if (pollTimer !== null) return
  pollTimer = setInterval(() => {
    // 上一轮还在飞 / 出错重试中就跳过这拍,避免叠请求。
    if (loadState.value !== 'loading') void load()
  }, REFRESH_INTERVAL_MS)
}

function stopPolling(): void {
  if (pollTimer !== null) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

function onVisibilityChange(): void {
  if (document.hidden) {
    stopPolling()
    stopPowerPolling()
  } else {
    void load() // 回前台立即补一次,再恢复轮询
    startPolling()
    void loadPowerStatus().then(() => startPowerPolling())
  }
}

onMounted(() => {
  void load()
  startPolling()
  // 电源状态独立节流(实例状态变化以十秒计,不必与指标同频)。
  void loadPowerStatus().then(() => startPowerPolling())
  document.addEventListener('visibilitychange', onVisibilityChange)
})

onUnmounted(() => {
  stopPolling()
  stopPowerPolling()
  document.removeEventListener('visibilitychange', onVisibilityChange)
})
</script>

<template>
  <div class="server-status">
    <header class="view-header">
      <div class="view-header__text">
        <h1 class="view-title">{{ t('serverStatus.title') }}</h1>
        <p class="view-sub">
          {{ t('serverStatus.subtitle') }}
          <span v-if="totalCount > 0" class="view-sub__count">
            · {{ t('serverStatus.reachableSummary', { reachable: reachableCount, total: totalCount }) }}
          </span>
          <span class="view-sub__count">· {{ t('serverStatus.autoRefresh', { n: 12 }) }}</span>
        </p>
      </div>
      <div class="view-header__actions">
        <AppButton variant="default" :disabled="selectedCount === 0" @click="openBatchCommand">
          {{ t('batchCommand.openButton', { n: selectedCount }) }}
        </AppButton>
        <AppButton variant="ghost" @click="historyOpen = true">
          {{ t('batchCommand.historyButton') }}
        </AppButton>
        <AppButton variant="ai" @click="aiTargets = []">
          <Robot class="action-icon" aria-hidden="true" />{{ t('opsChat.title') }}
        </AppButton>
        <AppButton
          variant="default"
          :loading="loadState === 'loading'"
          @click="load"
        >
          <Refresh
            v-if="loadState !== 'loading'"
            class="action-icon"
            aria-hidden="true"
          />{{ t('common.refresh') }}
        </AppButton>
      </div>
    </header>

    <!-- 筛选条:可达性 / 资源状态 chips + 名称搜索(三维叠加;仿容器页 controls-row) -->
    <div v-if="metrics.length > 0" class="controls-row">
      <div class="filter-bar" role="group" :aria-label="t('serverStatus.filterAria')">
        <button
          v-for="f in REACH_FILTERS"
          :key="f.key"
          class="filter-chip"
          :class="{ 'filter-chip--active': reachFilter === f.key }"
          :aria-pressed="reachFilter === f.key"
          @click="reachFilter = f.key"
        >
          {{ t(f.labelKey) }}
          <span class="filter-chip__count">{{ reachCounts[f.key] }}</span>
        </button>
      </div>
      <div class="filter-bar" role="group" :aria-label="t('serverStatus.resourceFilterAria')">
        <button
          v-for="f in TIGHT_FILTERS"
          :key="f.key"
          class="filter-chip"
          :class="{ 'filter-chip--active': tightFilter === f.key }"
          :aria-pressed="tightFilter === f.key"
          @click="tightFilter = f.key"
        >
          {{ t(f.labelKey) }}
          <span class="filter-chip__count">{{ tightCounts[f.key] }}</span>
        </button>
      </div>
      <div class="search-box">
        <input
          v-model="searchText"
          type="search"
          class="search-box__input"
          :placeholder="t('serverStatus.searchPlaceholder')"
          :aria-label="t('serverStatus.searchAria')"
        />
        <button
          v-if="searchText"
          class="search-box__clear"
          :title="t('serverStatus.searchClear')"
          @click="searchText = ''"
        >✕</button>
      </div>
    </div>

    <!-- 批量命令选择条:已选计数 + 全选/清空 -->
    <div v-if="metrics.length > 0" class="select-bar">
      <span class="select-bar__count">
        {{ t('batchCommand.selectedCount', { n: selectedCount, total: totalCount }) }}
      </span>
      <button class="select-bar__btn" type="button" @click="selectAll">{{ t('batchCommand.selectAll') }}</button>
      <button
        class="select-bar__btn"
        type="button"
        :disabled="selectedCount === 0"
        @click="clearSelection"
      >{{ t('batchCommand.clearSelection') }}</button>
    </div>

    <!-- Initial loading skeletons -->
    <div
      v-if="loadState === 'loading' && metrics.length === 0 && localMetrics === null"
      class="metrics-grid"
      aria-busy="true"
      :aria-label="t('serverStatus.loadingAria')"
    >
      <div v-for="n in 4" :key="n" class="skeleton-card">
        <SkeletonBlock :height="20" width="50%" />
        <SkeletonBlock :height="14" width="80%" />
        <SkeletonBlock :height="14" width="80%" />
      </div>
    </div>

    <!-- Load error (whole-page; metrics endpoint unreachable / 5xx) -->
    <ErrorState
      v-else-if="loadState === 'error'"
      :title="t('serverStatus.errTitle')"
      :description="loadError"
      @retry="load"
    />

    <!-- Empty: neither the control node nor any registered server has data -->
    <EmptyState
      v-else-if="metrics.length === 0 && localMetrics === null"
      :title="t('serverStatus.emptyTitle')"
      :description="t('serverStatus.emptyDesc')"
    />

    <!-- Metrics grid: local (control-node) card first, then per-host cards -->
    <div v-else-if="filteredMetrics.length > 0 || localMetrics" class="metrics-grid" :aria-busy="refreshing || undefined">
      <!-- 本机(控制节点)卡牌:固定置首,不参与筛选(它不是登记服务器,不参与批量命令勾选)。 -->
      <ServerMetricsCard
        v-if="localMetrics"
        :name="t('serverStatus.localName')"
        :metrics="localMetrics"
      >
        <template #actions>
          <AppButton
            variant="default"
            :disabled="!localMetrics.reachable"
            :title="localMetrics.reachable ? t('serverStatus.terminalTitle') : t('serverStatus.terminalUnavailable')"
            @click="openTerminal(LOCAL_HOST_ID)"
          >
            <Terminal class="action-icon" aria-hidden="true" />{{ t('opsServer.card.terminal') }}
          </AppButton>
        </template>
      </ServerMetricsCard>
      <ServerMetricsCard
        v-for="m in filteredMetrics"
        :key="m.serverId"
        :name="displayName(m)"
        :metrics="m"
        :pool="poolById.get(m.serverId)"
        :gpu="poolById.get(m.serverId)?.gpu ?? false"
        :ecs="ecsById?.get(m.serverId)"
        selectable
        :selected="selected.has(m.serverId)"
        @toggle="toggleSelect(m.serverId)"
      >
        <template #actions>
          <AppButton
            variant="default"
            :disabled="!m.reachable"
            :title="m.reachable ? t('serverStatus.terminalTitle') : t('serverStatus.terminalUnavailable')"
            @click="openTerminal(m.serverId)"
          >
            <Terminal class="action-icon" aria-hidden="true" />{{ t('opsServer.card.terminal') }}
          </AppButton>
          <!-- ECS 电源按钮:仅导入的阿里云 ECS 机器渲染;转换中/未知禁用 -->
          <AppButton
            v-if="powerBtn(m)"
            :variant="powerBtn(m)!.danger ? 'danger' : 'default'"
            :disabled="powerBtn(m)!.disabled"
            :title="t('serverStatus.powerTitle')"
            @click="powerServer(m)"
          >
            <Power class="action-icon" aria-hidden="true" />{{ powerBtn(m)!.label }}
          </AppButton>
          <AppButton variant="ai" @click="aiTargets = [m.serverId]">
            <Robot class="action-icon" aria-hidden="true" />{{ t('opsChat.title') }}
          </AppButton>
        </template>
      </ServerMetricsCard>
    </div>
    <!-- 筛选后一台不剩:提示调整条件(本机卡不参与筛选,仍会显示) -->
    <EmptyState
      v-else-if="metrics.length > 0"
      :title="t('serverStatus.filteredEmptyTitle')"
      :description="t('serverStatus.filteredEmptyDesc')"
    />
    <!-- 批量执行命令弹窗(勾选机器 → 同步执行 → 逐机结果) -->
    <BatchCommandModal
      v-if="commandModal"
      :servers="commandModal.servers"
      :initial-command="commandModal.command"
      @close="commandModal = null"
    />
    <!-- 执行历史抽屉(列表 → 逐机详情 → 重跑) -->
    <CommandHistoryDrawer
      v-if="historyOpen"
      @close="historyOpen = false"
      @rerun="openRerun"
    />
    <ContainerAiPanel v-if="aiTargets !== null" context="overview" :initial-server-ids="aiTargets" @close="aiTargets = null" />
  </div>
</template>

<style scoped>
.server-status {
  display: flex;
  flex-direction: column;
  gap: 24px;
}

.view-header {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}
.view-header__text {
  display: flex;
  flex: 1 1 240px;
  min-width: 0;
  flex-direction: column;
  gap: 4px;
}
.view-header__actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.action-icon {
  width: 18px;
  height: 18px;
  flex: none;
}
.view-title {
  margin: 0;
  font-size: var(--text-h1, 1.5rem);
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

.view-header__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}

.select-bar {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 8px 12px;
  border: 1px solid var(--color-line);
  border-radius: var(--rounded-lg);
  background: var(--color-surface);
}
.select-bar__count {
  font-size: var(--text-label);
  color: var(--color-dim);
  font-variant-numeric: tabular-nums;
}
.select-bar__btn {
  border: 0;
  background: transparent;
  color: var(--color-dim);
  font: inherit;
  font-size: var(--text-label);
  cursor: pointer;
  padding: 2px 6px;
  border-radius: 6px;
  text-decoration: underline;
  text-underline-offset: 3px;
}
.select-bar__btn:hover:not(:disabled) { color: var(--color-text); }
.select-bar__btn:disabled { opacity: 0.5; cursor: not-allowed; }

/* ——— 筛选条(可达性/资源 chips + 名称搜索;视觉与容器页一致) ——— */
.controls-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 12px;
}
.filter-bar {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
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

.metrics-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(min(280px, 100%), 1fr));
  gap: 16px;
  transition: opacity var(--duration-fast) var(--ease-out-expo);
}
.metrics-grid[aria-busy='true'] {
  opacity: 0.65;
}

.skeleton-card {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 18px;
  border: 1px solid var(--color-line);
  border-radius: var(--rounded-lg);
  background: var(--color-surface);
}
</style>
