<script setup lang="ts">
/*
  PVEUpgrade.vue — PVE 升级管理(顶级「PVE」页 → /pve/upgrade;route.query.cluster 预选集群)。

  三个 Tab:
    更新与日常升级 — 节点更新矩阵(版本/内核/待更新/订阅/需重启)+ 刷新索引(UPID 轮询)
                     + 串行日常升级(UpgradeProgress 模态)+ 逐节点重启(输入节点名强确认);
    APT 仓库       — AptRepoPanel(按节点清单/启停/镜像与订阅预设/批量/撤销);
    大版本升级     — 既有任务快照(运行中自动开进度)+ 预检报告 + 向导启动(8→9 滚动)。
  集群默认只读:写按钮禁用,横幅引导去 设置 → PVE 解锁。
*/
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import { NIcon } from 'naive-ui'
import { Server, Refresh, ArrowBarUp, Power, List, AlertTriangle } from '@vicons/tabler'
import { listPVEClusters, getPVETaskStatus, type PVECluster } from '../api/pveAdmin'
import {
  getPVEUpdates, refreshPVEUpdates, startPVEUpgrade, rebootPVENode,
  getPVEMajorPreflight, getPVEMajorUpgradeStatus,
  type PVEUpdatesOverview, type PVENodeUpdates, type PVEMajorPreflightReport,
  type PVEMajorUpgradeStatus, type PVEMajorCheck,
} from '../api/pveUpgrade'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import UpgradeProgress from '../components/pve/UpgradeProgress.vue'
import MajorUpgradeWizard from '../components/pve/MajorUpgradeWizard.vue'
import MajorUpgradeProgress from '../components/pve/MajorUpgradeProgress.vue'
import AptRepoPanel from '../components/pve/AptRepoPanel.vue'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()
const route = useRoute()

const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 集群选择 ──────────────────────────────────────────────────────────────────
const clusters = ref<PVECluster[]>([])
const currentId = ref('')
const loadingClusters = ref(true)
const current = computed<PVECluster | null>(() =>
  clusters.value.find((c) => c.id === currentId.value) ?? null)
const ready = computed(() => current.value?.state === 'ready')

async function loadClusters(): Promise<void> {
  loadingClusters.value = true
  try {
    clusters.value = await listPVEClusters()
    // 从 PVE 页跳转携带 ?cluster= 时优先预选。
    const q = route.query.cluster
    const preset = typeof q === 'string' ? clusters.value.find((c) => c.id === q) : undefined
    currentId.value = preset?.id ?? clusters.value.find((c) => c.state === 'ready')?.id ?? clusters.value[0]?.id ?? ''
  } catch (err) {
    toast.error(t('pveUpgrade.errLoad'), { detail: errMsg(err, t('pveUpgrade.errNetwork')) })
  } finally {
    loadingClusters.value = false
  }
}

// ─── Tab ───────────────────────────────────────────────────────────────────────
type Tab = 'updates' | 'repos' | 'major'
const tab = ref<Tab>('updates')

function tabLabel(tb: Tab): string {
  if (tb === 'updates') return t('pveUpgrade.tabUpdates')
  if (tb === 'repos') return t('pveUpgrade.tabRepos')
  return t('pveUpgrade.tabMajor')
}

function onSwitchTab(next: Tab): void {
  tab.value = next
  if (next === 'updates') void loadUpdates()
  else if (next === 'major') void loadMajorStatus()
}

// ─── 更新矩阵 ──────────────────────────────────────────────────────────────────
const updates = ref<PVEUpdatesOverview | null>(null)
const loadingUpdates = ref(false)
const refreshing = ref(false)
const upgradeProgressOpen = ref(false)
const expandedNode = ref('')
let disposed = false
const refreshTimers = new Set<ReturnType<typeof setTimeout>>()

const onlineNodeNames = computed(() =>
  (updates.value?.items ?? []).filter((n) => n.online).map((n) => n.node))

async function loadUpdates(): Promise<void> {
  const cl = current.value
  updates.value = null
  if (!cl || !ready.value) return
  loadingUpdates.value = true
  try {
    updates.value = await getPVEUpdates(cl.id)
  } catch (err) {
    toast.error(t('pveUpgrade.errLoad'), { detail: errMsg(err, t('pveUpgrade.errNetwork')) })
  } finally {
    loadingUpdates.value = false
  }
}

function subLabel(s?: string): string {
  if (s === 'active') return t('pveUpgrade.subActive')
  if (s === 'inactive') return t('pveUpgrade.subInactive')
  return t('pveUpgrade.subUnknown')
}

/** 刷新索引:受理后逐 UPID 轮询至完成,再整体重载矩阵。 */
async function onRefreshIndex(): Promise<void> {
  const cl = current.value
  if (!cl || refreshing.value) return
  refreshing.value = true
  try {
    const items = await refreshPVEUpdates(cl.id)
    toast.success(t('pveUpgrade.refreshAccepted'))
    const jobs: Array<{ node: string; upid: string }> = []
    for (const it of items) {
      if (it.error) toast.error(t('pveUpgrade.errLoad'), { detail: `${it.node}: ${it.error}` })
      if (it.upid) jobs.push({ node: it.node, upid: it.upid })
    }
    await Promise.all(jobs.map((j) => pollRefreshTask(cl.id, j.node, j.upid)))
    await loadUpdates()
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  } finally {
    refreshing.value = false
  }
}

function pollRefreshTask(clusterId: string, node: string, upid: string, tries = 0): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(async () => {
      refreshTimers.delete(timer)
      if (disposed) { resolve(); return }
      try {
        const ts = await getPVETaskStatus(clusterId, node, upid)
        if (ts.done) {
          if (!ts.ok) toast.error(t('pveUpgrade.errLoad'), { detail: `${node}: ${ts.exitStatus}` })
          resolve()
          return
        }
      } catch {
        // 轮询瞬时失败(网络抖动)继续重试;上限后放弃。
      }
      if (tries >= 60) { resolve(); return }
      void pollRefreshTask(clusterId, node, upid, tries + 1).then(resolve)
    }, 2000)
    refreshTimers.add(timer)
  })
}

async function onStartUpgrade(): Promise<void> {
  const cl = current.value
  if (!cl) return
  const ok = await confirm.open({
    title: t('pveUpgrade.upgradeConfirmTitle'),
    body: t('pveUpgrade.upgradeConfirmBody'),
    confirmLabel: t('pveUpgrade.startUpgrade'),
    variant: 'primary',
  })
  if (!ok) return
  try {
    await startPVEUpgrade(cl.id, {})
    toast.success(t('pveUpgrade.upgradeStarted'))
    upgradeProgressOpen.value = true
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  }
}

async function onReboot(n: PVENodeUpdates): Promise<void> {
  const cl = current.value
  if (!cl) return
  const ok = await confirm.open({
    title: t('pveUpgrade.rebootConfirmTitle', { node: n.node }),
    body: t('pveUpgrade.rebootConfirmBody', { node: n.node }),
    confirmText: n.node,
    confirmLabel: t('pveUpgrade.rebootNode'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await rebootPVENode(cl.id, n.node, n.node)
    toast.success(t('pveUpgrade.rebootAccepted'))
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  }
}

// ─── 大版本升级 ────────────────────────────────────────────────────────────────
const majorStatus = ref<PVEMajorUpgradeStatus | null>(null)
const report = ref<PVEMajorPreflightReport | null>(null)
const preflightRunning = ref(false)
const majorWizardOpen = ref(false)
const majorProgressOpen = ref(false)

async function loadMajorStatus(autoOpen = false): Promise<void> {
  const cl = current.value
  majorStatus.value = null
  if (!cl || !ready.value) return
  try {
    const st = await getPVEMajorUpgradeStatus(cl.id)
    majorStatus.value = st
    // 运行中的任务:进入页面/切换集群时自动打开进度。
    if (autoOpen && st.running && st.jobId) majorProgressOpen.value = true
  } catch (err) {
    toast.error(t('pveUpgrade.errLoad'), { detail: errMsg(err, t('pveUpgrade.errNetwork')) })
  }
}

function majorBadgeLabel(st: PVEMajorUpgradeStatus): string {
  if (st.running) return t('pveUpgrade.majorRunning')
  if (st.status === 'done') return t('pveUpgrade.majorDone')
  if (st.status === 'interrupted') return t('pveUpgrade.majorInterrupted')
  return t('pveUpgrade.majorPaused')
}
function majorBadgeCls(st: PVEMajorUpgradeStatus): string {
  if (st.running) return 'run'
  if (st.status === 'done') return 'ok'
  if (st.status === 'interrupted') return 'err'
  return 'warn'
}

function checkLabel(level: PVEMajorCheck['level']): string {
  if (level === 'pass') return t('pveUpgrade.checkPass')
  if (level === 'warn') return t('pveUpgrade.checkWarn')
  return t('pveUpgrade.checkFail')
}
function checkClass(level: PVEMajorCheck['level']): string {
  return level === 'pass' ? 'ok' : level === 'warn' ? 'warn' : 'err'
}

async function onRunPreflight(): Promise<void> {
  const cl = current.value
  if (!cl || preflightRunning.value) return
  preflightRunning.value = true
  try {
    report.value = await getPVEMajorPreflight(cl.id)
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  } finally {
    preflightRunning.value = false
  }
}

function onMajorStarted(): void {
  majorWizardOpen.value = false
  majorProgressOpen.value = true
  toast.success(t('pveUpgrade.majorStarted'))
  void loadMajorStatus()
}

function onMajorDone(): void {
  void loadMajorStatus()
  void loadUpdates()
}

function onClusterChange(): void {
  upgradeProgressOpen.value = false
  majorWizardOpen.value = false
  majorProgressOpen.value = false
  report.value = null
  expandedNode.value = ''
  void loadUpdates()
  void loadMajorStatus(true)
}

onMounted(async () => {
  await loadClusters()
  void loadUpdates()
  void loadMajorStatus(true)
})
onUnmounted(() => {
  disposed = true
  for (const timer of refreshTimers) clearTimeout(timer)
  refreshTimers.clear()
})
</script>

<template>
  <div class="pve-page">
    <header class="page-head">
      <div>
        <h2 class="page-title">{{ t('pveUpgrade.title') }}</h2>
        <p class="page-desc">{{ t('pveUpgrade.sub') }}</p>
      </div>
      <div class="head-ops">
        <select v-if="clusters.length > 0" v-model="currentId" class="cluster-select" :aria-label="t('pveUpgrade.cluster')" @change="onClusterChange">
          <option v-for="cl in clusters" :key="cl.id" :value="cl.id">{{ cl.name }}{{ cl.state === 'ready' ? '' : ` · ${t('pveSettings.stateCreated')}` }}</option>
        </select>
        <router-link class="btn-secondary" to="/pve">{{ t('pveUpgrade.backToPve') }}</router-link>
      </div>
    </header>

    <div v-if="loadingClusters" class="load-state">{{ t('pveUpgrade.loading') }}</div>
    <div v-else-if="clusters.length === 0" class="empty-state">
      <div class="empty-icon" aria-hidden="true"><NIcon :size="22"><Server /></NIcon></div>
      <p class="empty-label">{{ t('pveAdmin.emptyLabel') }}</p>
      <p class="empty-hint">{{ t('pveSettings.emptyHint') }}</p>
    </div>
    <div v-else-if="current && !ready" class="empty-state">
      <div class="empty-icon" aria-hidden="true"><NIcon :size="22"><Server /></NIcon></div>
      <p class="empty-label">{{ t('pveUpgrade.notReady') }}</p>
      <router-link to="/settings/pve" class="empty-link">{{ t('pveUpgrade.goSettings') }} →</router-link>
    </div>

    <template v-if="current && ready">
      <!-- 只读横幅 -->
      <div v-if="current.readonly" class="ro-banner">
        <NIcon :size="14"><AlertTriangle /></NIcon>
        <span>{{ t('pveUpgrade.readonlyHint') }}</span>
        <router-link to="/settings/pve" class="ro-link">{{ t('pveUpgrade.goSettings') }} →</router-link>
      </div>

      <nav class="tabs" role="tablist">
        <button
          v-for="tb in (['updates', 'repos', 'major'] as Tab[])"
          :key="tb"
          class="tab-btn"
          :class="{ active: tab === tb }"
          role="tab"
          :aria-selected="tab === tb"
          @click="onSwitchTab(tb)"
        >{{ tabLabel(tb) }}</button>
      </nav>

      <!-- 更新与日常升级 -->
      <section v-if="tab === 'updates'" class="panel">
        <div class="panel-head">
          <span class="panel-title">{{ t('pveUpgrade.updatesTitle') }}</span>
          <div class="panel-ops">
            <button class="btn-secondary" :disabled="!current || current.readonly || loadingUpdates || refreshing" @click="onRefreshIndex">
              <NIcon :size="13"><Refresh :class="{ spin: refreshing }" /></NIcon>
              {{ refreshing ? t('pveUpgrade.refreshing') : t('pveUpgrade.refreshIndex') }}
            </button>
            <button class="btn-create" :disabled="!current || current.readonly || loadingUpdates" @click="onStartUpgrade">
              <NIcon :size="13"><ArrowBarUp /></NIcon>
              {{ t('pveUpgrade.startUpgrade') }}
            </button>
          </div>
        </div>

        <div v-if="loadingUpdates" class="load-state">{{ t('pveUpgrade.loading') }}</div>
        <div v-else-if="!updates || updates.items.length === 0" class="load-state">{{ t('pveUpgrade.noNodes') }}</div>
        <template v-else>
          <div class="row head updates-cols" aria-hidden="true">
            <span>{{ t('pveUpgrade.node') }}</span>
            <span>{{ t('pveUpgrade.colVersion') }}</span>
            <span>{{ t('pveUpgrade.colKernel') }}</span>
            <span>{{ t('pveUpgrade.colPending') }}</span>
            <span>{{ t('pveUpgrade.colSubscription') }}</span>
            <span>{{ t('pveUpgrade.colState') }}</span>
            <span />
          </div>
          <template v-for="n in updates.items" :key="n.node">
            <div class="row updates-cols">
              <span class="node-cell">
                <span class="node-dot" :class="n.online ? 'up' : 'down'" />
                <strong class="cell-name">{{ n.node }}</strong>
              </span>
              <span class="mono dim">{{ n.version || '—' }}</span>
              <span class="mono dim">{{ n.kernelRunning || '—' }}</span>
              <span>{{ n.pending > 0 ? t('pveUpgrade.pendingN', { n: n.pending }) : t('pveUpgrade.upToDate') }}</span>
              <span><span class="tag" :class="{ ok: n.subscription === 'active' }">{{ subLabel(n.subscription) }}</span></span>
              <span>
                <span v-if="n.needsReboot" class="tag warn">{{ t('pveUpgrade.needsReboot') }}</span>
                <span v-else-if="n.error" class="tag err" :title="n.error">{{ t('pveUpgrade.failed') }}</span>
                <span v-else class="dim">—</span>
              </span>
              <span class="row-ops">
                <button
                  v-if="n.packages && n.packages.length > 0"
                  class="link-btn"
                  :title="t('pveUpgrade.packagesTitle')"
                  @click="expandedNode = expandedNode === n.node ? '' : n.node"
                >
                  <NIcon :size="12"><List /></NIcon>{{ t('pveUpgrade.packagesTitle') }}
                </button>
                <button
                  v-if="current && !current.readonly && n.online"
                  class="act-btn act-btn--danger"
                  :title="t('pveUpgrade.rebootNode')"
                  @click="onReboot(n)"
                >
                  <NIcon :size="13"><Power /></NIcon>
                </button>
              </span>
            </div>
            <!-- 待更新包展开 -->
            <div v-if="expandedNode === n.node && n.packages && n.packages.length > 0" class="pkg-block">
              <div v-for="p in n.packages" :key="p.package" class="pkg-row">
                <strong class="mono">{{ p.package }}</strong>
                <span class="mono dim">{{ p.oldVersion || '—' }} → {{ p.version || '—' }}</span>
              </div>
            </div>
          </template>
        </template>
      </section>

      <!-- APT 仓库 -->
      <section v-else-if="tab === 'repos'">
        <AptRepoPanel :cluster-id="currentId" :nodes="onlineNodeNames" :readonly="current?.readonly ?? true" />
      </section>

      <!-- 大版本升级 -->
      <section v-else-if="tab === 'major'" class="panel">
        <div class="panel-head">
          <div>
            <span class="panel-title">{{ t('pveUpgrade.majorTitle') }}</span>
            <p class="panel-sub">{{ t('pveUpgrade.majorSub') }}</p>
          </div>
          <div class="panel-ops">
            <button class="btn-secondary" :disabled="preflightRunning" @click="onRunPreflight">
              <NIcon :size="13"><List /></NIcon>
              {{ preflightRunning ? t('pveUpgrade.preflightRunning') : t('pveUpgrade.runPreflight') }}
            </button>
            <button class="btn-create" :disabled="!current || current.readonly || !report || report.blocked" @click="majorWizardOpen = true">
              <NIcon :size="13"><ArrowBarUp /></NIcon>
              {{ t('pveUpgrade.startMajor') }}
            </button>
          </div>
        </div>

        <!-- 既有任务快照 -->
        <div v-if="majorStatus && majorStatus.jobId" class="job-summary">
          <span class="tag" :class="majorBadgeCls(majorStatus)">{{ majorBadgeLabel(majorStatus) }}</span>
          <span class="mono dim job-step">{{ majorStatus.step }} — {{ majorStatus.message }}</span>
          <button class="link-btn" @click="majorProgressOpen = true">{{ t('pveUpgrade.majorProgressTitle') }}</button>
        </div>
        <p v-else class="load-state">{{ t('pveUpgrade.majorIdle') }}</p>

        <!-- 预检报告 -->
        <template v-if="report">
          <div class="report-head">
            <span class="sec-title">{{ t('pveUpgrade.preflightTitle') }}</span>
            <span class="dim sub">{{ t('pveUpgrade.preflightAt') }}: {{ new Date(report.at).toLocaleString() }}</span>
            <span class="tag" :class="report.blocked ? 'err' : 'ok'">
              {{ report.blocked ? t('pveUpgrade.preflightBlocked') : t('pveUpgrade.preflightPass') }}
            </span>
          </div>
          <p v-if="report.blocked" class="banner">{{ t('pveUpgrade.preflightBlocked') }}</p>

          <div class="sec">
            <span class="sec-title">{{ t('pveUpgrade.clusterChecks') }}</span>
            <div v-for="ck in report.clusterChecks" :key="ck.key" class="check-row">
              <span class="tag" :class="checkClass(ck.level)">{{ checkLabel(ck.level) }}</span>
              <span class="check-msg">{{ ck.message }}</span>
            </div>
          </div>

          <div class="sec">
            <span class="sec-title">{{ t('pveUpgrade.nodeChecks') }}</span>
            <div v-for="np in report.nodes" :key="np.node" class="node-block">
              <div class="node-block-head">
                <strong>{{ np.node }}</strong>
                <span class="tag" :class="np.online ? 'ok' : 'err'">{{ np.online ? t('pveUpgrade.online') : t('pveUpgrade.offline') }}</span>
                <span class="mono dim">{{ np.version || '—' }}</span>
                <span class="dim sub">{{ t('pveUpgrade.diskFree', { gb: np.diskFreeGb.toFixed(1) }) }}</span>
                <span class="dim sub">{{ t('pveUpgrade.pendingN', { n: np.pending }) }}</span>
              </div>
              <p v-if="np.error" class="banner">{{ np.error }}</p>
              <div v-if="np.pve8to9" class="pve8to9">
                <span class="sec-title">{{ t('pveUpgrade.pve8to9Title') }}</span>
                <span class="dim sub">
                  {{ t('pveUpgrade.pve8to9Summary', { passed: np.pve8to9.passed, skipped: np.pve8to9.skipped, warnings: np.pve8to9.warnings, failures: np.pve8to9.failures }) }}
                </span>
              </div>
              <div v-for="ck in np.checks" :key="ck.key" class="check-row">
                <span class="tag" :class="checkClass(ck.level)">{{ checkLabel(ck.level) }}</span>
                <span class="check-msg">{{ ck.message }}</span>
              </div>
            </div>
          </div>

          <p v-if="report.acksRequired.length > 0" class="hint">{{ t('pveUpgrade.ackRequired') }}: {{ report.acksRequired.length }}</p>
        </template>
      </section>
    </template>

    <!-- 模态:日常升级进度 / 大版本向导 / 大版本进度 -->
    <UpgradeProgress
      v-if="upgradeProgressOpen && current"
      :cluster-id="current.id"
      :cluster-name="current.name"
      @close="upgradeProgressOpen = false"
      @done="loadUpdates"
    />
    <MajorUpgradeWizard
      v-if="majorWizardOpen && current && report"
      :cluster-id="current.id"
      :cluster-name="current.name"
      :report="report"
      @close="majorWizardOpen = false"
      @started="onMajorStarted"
    />
    <MajorUpgradeProgress
      v-if="majorProgressOpen && current"
      :cluster-id="current.id"
      :cluster-name="current.name"
      @close="majorProgressOpen = false"
      @done="onMajorDone"
    />
  </div>
</template>

<style scoped>
.pve-page {
  display: flex;
  flex-direction: column;
  gap: 20px;
  max-width: 1080px;
}
.page-head { display: flex; align-items: flex-start; gap: 16px; }
.page-title { font-size: 1.15rem; font-weight: 700; color: var(--color-text); }
.page-desc { font-size: 0.82rem; color: var(--color-faint); margin-top: 4px; max-width: 72ch; line-height: 1.5; }
.head-ops { margin-left: auto; display: flex; gap: 8px; align-items: center; }
.cluster-select {
  height: 36px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  padding: 0 10px;
  color: var(--color-text);
  font-size: 0.82rem;
}
.btn-secondary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 36px;
  padding: 0 12px;
  background: var(--color-card);
  color: var(--color-text);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  font-size: 0.8rem;
  font-weight: 500;
  cursor: pointer;
  text-decoration: none;
}
.btn-secondary:disabled { opacity: 0.55; cursor: not-allowed; }
.btn-create {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  height: 36px;
  padding: 0 14px;
  background: var(--color-primary);
  color: var(--color-primary-contrast, #fff);
  border: none;
  border-radius: var(--rounded);
  font-size: 0.8rem;
  font-weight: 600;
  cursor: pointer;
}
.btn-create:hover:not(:disabled) { opacity: 0.9; }
.btn-create:disabled { opacity: 0.55; cursor: not-allowed; }
.spin { animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }

.load-state { padding: 28px 16px; text-align: center; color: var(--color-faint); font-size: 0.82rem; }
.empty-state {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 40px 24px;
  text-align: center;
  border: 1.5px dashed var(--color-border-strong);
  border-radius: var(--rounded-card, var(--rounded-lg));
  color: var(--color-dim);
}
.empty-icon {
  width: 40px;
  height: 40px;
  display: grid;
  place-items: center;
  border-radius: var(--rounded-lg);
  background: var(--color-primary-soft);
  color: var(--color-primary);
}
.empty-label { font-size: 0.92rem; font-weight: 600; color: var(--color-text); }
.empty-hint { font-size: 0.78rem; color: var(--color-faint); max-width: 56ch; }
.empty-link { font-size: 0.8rem; color: var(--color-primary); text-decoration: none; }

.ro-banner {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 9px 12px;
  border-radius: var(--rounded);
  background: var(--color-amber-soft, oklch(95% 0.06 80));
  color: var(--color-amber, oklch(75% 0.15 80));
  font-size: 0.78rem;
  line-height: 1.5;
}
.ro-link { margin-left: auto; color: var(--color-primary); text-decoration: none; white-space: nowrap; }

/* Tab */
.tabs { display: flex; gap: 4px; border-bottom: 1px solid var(--color-border); }
.tab-btn {
  border: none;
  background: none;
  color: var(--color-faint);
  padding: 8px 14px;
  font-size: 0.82rem;
  cursor: pointer;
  border-bottom: 2px solid transparent;
}
.tab-btn.active { color: var(--color-primary); border-bottom-color: var(--color-primary); font-weight: 600; }

.panel {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 16px;
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card, var(--rounded-lg));
}
.panel-head { display: flex; align-items: flex-start; gap: 12px; padding-bottom: 4px; }
.panel-title { font-size: 0.9rem; font-weight: 600; color: var(--color-text); }
.panel-sub { font-size: 0.74rem; color: var(--color-faint); margin-top: 3px; line-height: 1.5; max-width: 64ch; }
.panel-ops { margin-left: auto; display: flex; gap: 8px; align-items: center; flex-shrink: 0; }

/* 表格 */
.row {
  display: grid;
  gap: 10px;
  align-items: center;
  padding: 10px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-card);
  font-size: 0.82rem;
}
.row + .row { margin-top: 6px; }
.row.head {
  background: none;
  border: none;
  padding-bottom: 0;
  font-size: 0.7rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.row.head + .row { margin-top: 0; }
.updates-cols { grid-template-columns: 1.1fr 0.8fr 1fr 0.9fr 0.7fr 0.7fr auto; }
.node-cell { display: inline-flex; align-items: center; gap: 6px; min-width: 0; }
.node-dot { width: 8px; height: 8px; border-radius: 50%; flex-shrink: 0; }
.node-dot.up { background: var(--color-green); }
.node-dot.down { background: var(--color-red); }
.cell-name { color: var(--color-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.dim { color: var(--color-dim); }
.sub { font-size: 0.68rem; }
.mono { font-family: var(--font-mono); font-size: 0.78rem; }
.row-ops { display: flex; align-items: center; gap: 8px; justify-content: flex-end; }
.act-btn {
  display: inline-grid;
  place-items: center;
  width: 28px;
  height: 28px;
  background: var(--color-card);
  color: var(--color-dim);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  cursor: pointer;
}
.act-btn:hover:not(:disabled) { color: var(--color-text); border-color: var(--color-border-strong); }
.act-btn--danger { color: var(--color-red); }
.link-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  background: none;
  border: none;
  padding: 0;
  color: var(--color-primary);
  font-size: 0.74rem;
  cursor: pointer;
  white-space: nowrap;
}
.pkg-block {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 8px 14px;
  border: 1px dashed var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-inset);
}
.pkg-row { display: flex; justify-content: space-between; gap: 12px; font-size: 0.76rem; }
.pkg-row strong { color: var(--color-text); font-weight: 600; overflow-wrap: anywhere; }

.tag {
  display: inline-flex;
  align-items: center;
  padding: 1px 8px;
  border-radius: var(--rounded-full);
  font-size: 0.68rem;
  font-weight: 600;
  background: var(--color-inset);
  color: var(--color-dim);
  white-space: nowrap;
}
.tag.ok { background: var(--color-green-soft, oklch(95% 0.04 150)); color: var(--color-green); }
.tag.warn { background: var(--color-amber-soft, oklch(95% 0.06 80)); color: var(--color-amber, oklch(75% 0.15 80)); }
.tag.err { background: var(--color-red-soft, oklch(95% 0.03 25)); color: var(--color-red); }
.tag.run { background: var(--color-primary-soft); color: var(--color-primary); }

/* 大版本 */
.job-summary { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; padding: 4px 0; }
.job-step { font-size: 0.74rem; overflow-wrap: anywhere; }
.report-head { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; padding-top: 6px; border-top: 1px solid var(--color-border); }
.sec { display: flex; flex-direction: column; gap: 6px; }
.sec-title { font-size: 0.74rem; font-weight: 600; color: var(--color-dim); }
.check-row { display: flex; align-items: flex-start; gap: 8px; font-size: 0.78rem; }
.check-msg { color: var(--color-dim); line-height: 1.5; overflow-wrap: anywhere; }
.node-block {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-inset);
}
.node-block-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 0.8rem; }
.node-block-head strong { color: var(--color-text); }
.pve8to9 { display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; }
.banner {
  padding: 9px 12px;
  border-radius: var(--rounded);
  background: var(--color-red-soft, oklch(95% 0.03 25));
  color: var(--color-red);
  font-size: 0.78rem;
  line-height: 1.5;
  overflow-wrap: anywhere;
}
.hint { font-size: 0.74rem; color: var(--color-faint); line-height: 1.5; }

@media (max-width: 960px) {
  .updates-cols { grid-template-columns: 1.1fr 0.8fr 0.9fr auto; }
  .updates-cols > :nth-child(3), .updates-cols > :nth-child(5), .updates-cols > :nth-child(6) { display: none; }
  .panel-head { flex-direction: column; }
  .panel-ops { margin-left: 0; }
}
</style>
