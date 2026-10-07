<!--
  ServerStatus.vue — Story 6-1: 多机状态总览(FR-15,服务器层资源指标)

  在一个面板看所有已登记服务器的 CPU 负载 / 内存 / 磁盘使用,免逐台 SSH。
    · 登记服务器列表来自 4-1;指标经 6-1 批量端点并行采集(每台独立)。
    · 某台不可达 → 该卡灰显 + 人读错误,不连累其它台。
    · 某指标缺失(跨平台 best-effort,如 macOS 无 free)→ 该行「不可用」。
    · 「刷新」重取(指标实时、不落库)。

  这是一个**新增**的总览入口,不动 4-1 SettingsServers CRUD、6-2 日志入口。
-->
<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { Robot, Refresh } from '@vicons/tabler'
import { getAllServerMetrics, listServers, type ServerMetrics } from '../api/servers'
import { HttpError } from '../api/http'
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
const aiTargets = ref<string[] | null>(null)

/** 卡片标签区需要的登记信息(纯展示,不含凭据引用)。 */
interface ServerPoolInfo {
  /** 逗号分隔的构建池标签;空串 = 非构建机。 */
  labels: string
  /** 并发构建槽位(0 = 全局默认);tooltip 用。 */
  maxBuilds: number
  /** 调度优先级 0-100;tooltip 用。 */
  priority: number
}

const loadState = ref<LoadState>('idle')
const loadError = ref('')
const metrics = ref<ServerMetrics[]>([])
/** serverId → 展示名(来自登记列表,用于卡片标题)。 */
const nameById = ref<Map<string, string>>(new Map())
/** serverId → 构建池信息(卡片标签 chips 展示用)。 */
const poolById = ref<Map<string, ServerPoolInfo>>(new Map())

/** 首次加载尚无数据时显示骨架;刷新时保留旧数据(stale-while-revalidate)。 */
const refreshing = ref(false)

const reachableCount = computed(() => metrics.value.filter((m) => m.reachable).length)
const totalCount = computed(() => metrics.value.length)

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
  selected.value = new Set(metrics.value.map((m) => m.serverId))
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
    // 并行:登记列表(取展示名 + 构建池标签)+ 批量指标。互不阻塞。
    const [servers, items] = await Promise.all([
      listServers(),
      getAllServerMetrics(),
    ])
    nameById.value = new Map(servers.map((s) => [s.id, s.name]))
    poolById.value = new Map(
      servers.map((s) => [s.id, { labels: s.labels, maxBuilds: s.maxBuilds, priority: s.priority }]),
    )
    metrics.value = items
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
  } else {
    void load() // 回前台立即补一次,再恢复轮询
    startPolling()
  }
}

onMounted(() => {
  void load()
  startPolling()
  document.addEventListener('visibilitychange', onVisibilityChange)
})

onUnmounted(() => {
  stopPolling()
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
      v-if="loadState === 'loading' && metrics.length === 0"
      class="metrics-grid"
      aria-busy="true"
      :aria-label="t('serverStatus.loadingAria')"
    >
      <div v-for="n in 3" :key="n" class="skeleton-card">
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

    <!-- Empty: no registered servers -->
    <EmptyState
      v-else-if="metrics.length === 0"
      :title="t('serverStatus.emptyTitle')"
      :description="t('serverStatus.emptyDesc')"
    />

    <!-- Metrics grid (per-host cards) -->
    <div v-else class="metrics-grid" :aria-busy="refreshing || undefined">
      <ServerMetricsCard
        v-for="m in metrics"
        :key="m.serverId"
        :name="displayName(m)"
        :metrics="m"
        :pool="poolById.get(m.serverId)"
        selectable
        :selected="selected.has(m.serverId)"
        @toggle="toggleSelect(m.serverId)"
      >
        <template #actions>
          <AppButton variant="ai" @click="aiTargets = [m.serverId]">
            <Robot class="action-icon" aria-hidden="true" />{{ t('opsChat.title') }}
          </AppButton>
        </template>
      </ServerMetricsCard>
    </div>
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
    <ContainerAiPanel v-if="aiTargets !== null" :initial-server-ids="aiTargets" @close="aiTargets = null" />
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
