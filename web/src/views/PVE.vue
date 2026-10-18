<script setup lang="ts">
/*
  PVE.vue — PVE 集群管理(顶部导航「PVE」;集群经服务器登记自动发现,Token 在 设置 → PVE)。

  本页:
    - 概览:版本/quorum/节点在线表 + 全集群资源用量(CPU/内存/存储);
    - 实例:全部虚拟机与容器(qemu/lxc),按节点/类型/状态过滤;
    - 电源操作:start/shutdown/stop/reboot/reset/suspend/resume——集群默认只读,解锁是
      显式确认动作(服务端强校验 + 审计;解锁入口在 设置 → PVE);
    - 任务:操作受理后返回 UPID,轮询任务状态至完成(成功/失败 toast);任务中心列最近任务。
  Token 密文存后端保险库,本页只见 hasToken/state。
*/
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { Server, Refresh, Rotate, PlayerPlay, PlayerStop, PlayerPause, PlayerTrackNext, Power, Clock } from '@vicons/tabler'
import {
  listPVEClusters, getPVEOverview, listPVEVMs, pveVMAction, getPVETaskStatus, listPVETasks,
  type PVECluster, type PVEOverview, type PVEVM, type PVETask,
} from '../api/pveAdmin'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 集群选择 ──────────────────────────────────────────────────────────────────
const clusters = ref<PVECluster[]>([])
const currentId = ref('')
const loadingClusters = ref(true)
const current = computed<PVECluster | null>(() =>
  clusters.value.find((c) => c.id === currentId.value) ?? null)

async function loadClusters(keepCurrent = false): Promise<void> {
  loadingClusters.value = true
  try {
    clusters.value = await listPVEClusters()
    if (!keepCurrent || !clusters.value.some((c) => c.id === currentId.value)) {
      currentId.value = clusters.value.find((c) => c.state === 'ready')?.id ?? clusters.value[0]?.id ?? ''
    }
  } catch (err) {
    toast.error(t('pveAdmin.errLoadClusters'), { detail: errMsg(err, t('pveAdmin.errNetwork')) })
  } finally {
    loadingClusters.value = false
  }
}

// ─── 概览与实例 ────────────────────────────────────────────────────────────────
const overview = ref<PVEOverview | null>(null)
const vms = ref<PVEVM[]>([])
const tasks = ref<PVETask[]>([])
const loadingData = ref(false)
const filterNode = ref('')
const filterType = ref('')
const filterStatus = ref('')

const nodes = computed(() => overview.value?.nodes ?? [])
const ready = computed(() => current.value?.state === 'ready')

const filteredVMs = computed(() =>
  vms.value.filter((v) =>
    (!filterNode.value || v.node === filterNode.value) &&
    (!filterType.value || v.type === filterType.value) &&
    (!filterStatus.value || v.status === filterStatus.value)))

function fmtBytes(n: number): string {
  if (!n || n <= 0) return '0'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++ }
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`
}

function fmtUptime(s: number): string {
  if (!s) return '—'
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d > 0) return t('pveAdmin.uptimeDays', { d, h })
  if (h > 0) return t('pveAdmin.uptimeHours', { h, m })
  return t('pveAdmin.uptimeMinutes', { m })
}

function pct(used: number, total: number): number {
  if (!total) return 0
  return Math.min(100, Math.round((used / total) * 100))
}

async function loadData(): Promise<void> {
  const cl = current.value
  overview.value = null
  vms.value = []
  tasks.value = []
  if (!cl || !ready.value) return
  loadingData.value = true
  try {
    const [ov, vmList, taskList] = await Promise.all([
      getPVEOverview(cl.id),
      listPVEVMs(cl.id),
      listPVETasks(cl.id, 30),
    ])
    overview.value = ov
    vms.value = vmList
    tasks.value = taskList
  } catch (err) {
    toast.error(t('pveAdmin.errLoadData'), { detail: errMsg(err, t('pveAdmin.errNetwork')) })
  } finally {
    loadingData.value = false
  }
}

function onClusterChange(): void {
  filterNode.value = ''
  filterType.value = ''
  filterStatus.value = ''
  void loadData()
}

// ─── 电源操作 + UPID 轮询 ─────────────────────────────────────────────────────
type VMAction = 'start' | 'shutdown' | 'stop' | 'reboot' | 'reset' | 'suspend' | 'resume'
const actingVM = ref('')
let pollTimer: ReturnType<typeof setTimeout> | null = null

const actionDefs: Array<{ action: VMAction; icon: typeof Power; danger?: boolean }> = [
  { action: 'start', icon: PlayerPlay },
  { action: 'shutdown', icon: Power, danger: true },
  { action: 'stop', icon: PlayerStop, danger: true },
  { action: 'reboot', icon: PlayerTrackNext, danger: true },
  { action: 'reset', icon: Rotate, danger: true },
  { action: 'suspend', icon: PlayerPause, danger: true },
  { action: 'resume', icon: PlayerPlay },
]

function confirmKey(action: VMAction): string {
  return `pveAdmin.action_${action}`
}

async function runAction(vm: PVEVM, action: VMAction): Promise<void> {
  const cl = current.value
  if (!cl || actingVM.value) return
  if (action !== 'start' && action !== 'resume') {
    const ok = await confirm.open({
      title: t('pveAdmin.actionConfirmTitle', { action: t(confirmKey(action)), name: vm.name }),
      body: t('pveAdmin.actionConfirmBody'),
      confirmLabel: t(confirmKey(action)),
      variant: 'danger',
    })
    if (!ok) return
  }
  actingVM.value = `${vm.node}/${vm.vmid}`
  try {
    const ref = await pveVMAction(cl.id, vm.node, vm.vmid, vm.type, action)
    toast.info(t('pveAdmin.actionAccepted', { name: vm.name }))
    pollTask(cl.id, ref.node, ref.upid, vm.name)
  } catch (err) {
    toast.error(t('pveAdmin.errAction'), { detail: errMsg(err, t('pveAdmin.errNetwork')) })
  } finally {
    actingVM.value = ''
  }
}

function pollTask(clusterId: string, node: string, upid: string, name: string, tries = 0): void {
  if (pollTimer) clearTimeout(pollTimer)
  pollTimer = setTimeout(async () => {
    try {
      const ts = await getPVETaskStatus(clusterId, node, upid)
      if (ts.done) {
        if (ts.ok) toast.success(t('pveAdmin.taskDone', { name }))
        else toast.error(t('pveAdmin.taskFailed', { name }), { detail: ts.exitStatus })
        void loadData()
        return
      }
    } catch {
      // 轮询瞬时失败(网络抖动)继续重试;上限后放弃提示。
    }
    if (tries >= 60) {
      toast.info(t('pveAdmin.taskPollGaveUp', { name }))
      return
    }
    pollTask(clusterId, node, upid, name, tries + 1)
  }, 2000)
}

onMounted(async () => {
  await loadClusters()
  await loadData()
})
onUnmounted(() => { if (pollTimer) clearTimeout(pollTimer) })
</script>

<template>
  <div class="pve-page">
    <header class="page-head">
      <div>
        <h2 class="page-title">{{ t('pveAdmin.title') }}</h2>
        <p class="page-desc">{{ t('pveAdmin.desc') }}</p>
      </div>
      <div class="head-ops">
        <select v-if="clusters.length > 0" v-model="currentId" class="cluster-select" :aria-label="t('pveAdmin.clusterLabel')" @change="onClusterChange">
          <option v-for="cl in clusters" :key="cl.id" :value="cl.id">{{ cl.name }}{{ cl.state === 'ready' ? '' : ` · ${t('pveSettings.stateCreated')}` }}</option>
        </select>
        <button class="btn-secondary" :disabled="!ready || loadingData" @click="loadData">
          <NIcon :size="13"><Refresh :class="{ spin: loadingData }" /></NIcon>
          {{ t('pveAdmin.refresh') }}
        </button>
      </div>
    </header>

    <div v-if="loadingClusters" class="load-state">{{ t('pveAdmin.loading') }}</div>
    <div v-else-if="clusters.length === 0" class="empty-state">
      <div class="empty-icon" aria-hidden="true"><NIcon :size="22"><Server /></NIcon></div>
      <p class="empty-label">{{ t('pveAdmin.emptyLabel') }}</p>
      <p class="empty-hint">{{ t('pveSettings.emptyHint') }}</p>
    </div>
    <div v-else-if="current && !ready" class="empty-state">
      <div class="empty-icon" aria-hidden="true"><NIcon :size="22"><Server /></NIcon></div>
      <p class="empty-label">{{ t('pveAdmin.notReadyLabel') }}</p>
      <router-link to="/settings/pve" class="empty-link">{{ t('pveAdmin.goConfig') }} →</router-link>
    </div>

    <template v-if="current && ready">
      <!-- 概览:版本/quorum/资源 -->
      <section class="overview">
        <div class="ov-cards">
          <div class="ov-card">
            <span class="ov-label">{{ t('pveAdmin.ovVersion') }}</span>
            <strong class="ov-value mono">{{ overview?.version || '—' }}</strong>
          </div>
          <div class="ov-card">
            <span class="ov-label">{{ t('pveAdmin.ovQuorum') }}</span>
            <strong class="ov-value" :class="overview?.quorate ? 'ok' : 'bad'">
              {{ overview?.quorate ? t('pveAdmin.quorate') : t('pveAdmin.noQuorum') }}
            </strong>
          </div>
          <div class="ov-card">
            <span class="ov-label">{{ t('pveAdmin.ovNodes') }}</span>
            <strong class="ov-value">{{ nodes.filter((n) => n.online).length }}/{{ nodes.length }}</strong>
          </div>
        </div>
        <div class="res-bars">
          <div class="res-bar">
            <span class="res-label">{{ t('pveAdmin.resCpu') }}</span>
            <div class="bar"><div class="bar-fill" :style="{ width: pct(overview?.resources.cpuUsed ?? 0, overview?.resources.cpuTotal ?? 0) + '%' }" /></div>
            <span class="res-value mono">{{ (overview?.resources.cpuUsed ?? 0).toFixed(1) }} / {{ overview?.resources.cpuTotal ?? 0 }} {{ t('pveAdmin.cores') }}</span>
          </div>
          <div class="res-bar">
            <span class="res-label">{{ t('pveAdmin.resMem') }}</span>
            <div class="bar"><div class="bar-fill" :style="{ width: pct(overview?.resources.memUsed ?? 0, overview?.resources.memTotal ?? 0) + '%' }" /></div>
            <span class="res-value mono">{{ fmtBytes(overview?.resources.memUsed ?? 0) }} / {{ fmtBytes(overview?.resources.memTotal ?? 0) }}</span>
          </div>
          <div class="res-bar">
            <span class="res-label">{{ t('pveAdmin.resStorage') }}</span>
            <div class="bar"><div class="bar-fill" :style="{ width: pct(overview?.resources.storageUsed ?? 0, overview?.resources.storageTotal ?? 0) + '%' }" /></div>
            <span class="res-value mono">{{ fmtBytes(overview?.resources.storageUsed ?? 0) }} / {{ fmtBytes(overview?.resources.storageTotal ?? 0) }}</span>
          </div>
        </div>
        <!-- 节点表 -->
        <div class="node-strip">
          <div v-for="n in nodes" :key="n.name" class="node-chip" :class="{ 'node-chip--down': !n.online }">
            <span class="node-dot" :class="n.online ? 'up' : 'down'" />
            <span class="node-name">{{ n.name }}</span>
            <span class="mono node-ip">{{ n.ip || '—' }}</span>
          </div>
        </div>
      </section>

      <!-- 实例表 -->
      <section class="vms">
        <div class="list-head">
          <span class="list-title">{{ t('pveAdmin.vmsTitle') }}</span>
          <span class="list-count">{{ t('pveAdmin.vmCount', { n: filteredVMs.length, total: vms.length }) }}</span>
          <div class="filters">
            <select v-model="filterNode" class="filter-select" :aria-label="t('pveAdmin.filterNode')">
              <option value="">{{ t('pveAdmin.allNodes') }}</option>
              <option v-for="n in nodes" :key="n.name" :value="n.name">{{ n.name }}</option>
            </select>
            <select v-model="filterType" class="filter-select" :aria-label="t('pveAdmin.filterType')">
              <option value="">{{ t('pveAdmin.allTypes') }}</option>
              <option value="qemu">VM</option>
              <option value="lxc">LXC</option>
            </select>
            <select v-model="filterStatus" class="filter-select" :aria-label="t('pveAdmin.filterStatus')">
              <option value="">{{ t('pveAdmin.allStatuses') }}</option>
              <option value="running">{{ t('pveAdmin.statusRunning') }}</option>
              <option value="stopped">{{ t('pveAdmin.statusStopped') }}</option>
            </select>
          </div>
        </div>
        <div v-if="loadingData" class="load-state">{{ t('pveAdmin.loading') }}</div>
        <div v-else-if="filteredVMs.length === 0" class="load-state">{{ t('pveAdmin.noVMs') }}</div>
        <template v-else>
          <div class="vm-row vm-row--head" aria-hidden="true">
            <span>VMID</span>
            <span>{{ t('pveAdmin.colName') }}</span>
            <span>{{ t('pveAdmin.colNode') }}</span>
            <span>{{ t('pveAdmin.colType') }}</span>
            <span>{{ t('pveAdmin.colStatus') }}</span>
            <span>{{ t('pveAdmin.colUptime') }}</span>
            <span />
          </div>
          <div v-for="vm in filteredVMs" :key="`${vm.node}-${vm.vmid}`" class="vm-row">
            <span class="mono dim">{{ vm.vmid }}</span>
            <strong class="vm-name">{{ vm.name }}</strong>
            <span class="dim">{{ vm.node }}</span>
            <span><span class="type-badge" :class="vm.type">{{ vm.type === 'qemu' ? 'VM' : 'LXC' }}</span></span>
            <span class="vm-status" :class="vm.status === 'running' ? 'running' : 'stopped'">
              <span class="status-dot" />{{ vm.status }}
            </span>
            <span class="dim">{{ fmtUptime(vm.uptime) }}</span>
            <span class="row-ops">
              <template v-if="current && !current.readonly">
                <button
                  v-for="def in actionDefs"
                  :key="def.action"
                  class="act-btn"
                  :class="{ 'act-btn--danger': def.danger }"
                  :title="t(`pveAdmin.action_${def.action}`)"
                  :disabled="actingVM !== ''"
                  @click="runAction(vm, def.action)"
                >
                  <NIcon :size="13"><component :is="def.icon" /></NIcon>
                </button>
              </template>
              <span v-else class="ro-hint"><NIcon :size="12"><Clock /></NIcon>{{ t('pveAdmin.readonlyHint') }}</span>
            </span>
          </div>
        </template>
      </section>

      <!-- 任务中心 -->
      <section class="tasks">
        <div class="list-head">
          <span class="list-title">{{ t('pveAdmin.tasksTitle') }}</span>
        </div>
        <div v-if="tasks.length === 0" class="load-state">{{ t('pveAdmin.noTasks') }}</div>
        <template v-else>
          <div class="task-row task-row--head" aria-hidden="true">
            <span>{{ t('pveAdmin.colTaskType') }}</span>
            <span>{{ t('pveAdmin.colNode') }}</span>
            <span>{{ t('pveAdmin.colTaskStatus') }}</span>
            <span>{{ t('pveAdmin.colTaskTime') }}</span>
          </div>
          <div v-for="task in tasks" :key="task.upid" class="task-row">
            <span class="mono dim">{{ task.type }}</span>
            <span class="dim">{{ task.node }}</span>
            <span class="task-status" :class="task.status === 'OK' ? 'ok' : task.status === 'ERROR' ? 'bad' : 'run'">
              {{ task.status === '' ? t('pveAdmin.taskRunning') : task.status }}
            </span>
            <span class="mono dim">{{ task.startTime ? new Date(task.startTime * 1000).toLocaleString() : '—' }}</span>
          </div>
        </template>
      </section>
    </template>
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
.cluster-select, .filter-select {
  height: 36px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  padding: 0 10px;
  color: var(--color-text);
  font-size: 0.82rem;
}
.filter-select { height: 30px; font-size: 0.76rem; }

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
}
.btn-secondary:disabled { opacity: 0.55; cursor: not-allowed; }
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
  border-radius: var(--rounded-card);
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

.overview {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 16px;
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
}
.ov-cards { display: flex; gap: 12px; flex-wrap: wrap; }
.ov-card {
  flex: 1;
  min-width: 140px;
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 10px 14px;
  background: var(--color-inset);
  border-radius: var(--rounded);
}
.ov-label { font-size: 0.72rem; color: var(--color-faint); }
.ov-value { font-size: 0.98rem; color: var(--color-text); }
.ov-value.ok { color: var(--color-green); }
.ov-value.bad { color: var(--color-red); }
.res-bars { display: flex; flex-direction: column; gap: 8px; }
.res-bar { display: grid; grid-template-columns: 72px 1fr auto; gap: 10px; align-items: center; }
.res-label { font-size: 0.74rem; color: var(--color-dim); }
.bar { height: 8px; border-radius: var(--rounded-full); background: var(--color-inset); overflow: hidden; }
.bar-fill { height: 100%; border-radius: var(--rounded-full); background: var(--color-primary); transition: width 0.3s; }
.res-value { font-size: 0.72rem; color: var(--color-faint); white-space: nowrap; }
.node-strip { display: flex; gap: 8px; flex-wrap: wrap; }
.node-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-full);
  font-size: 0.76rem;
  background: var(--color-inset);
}
.node-chip--down { opacity: 0.6; }
.node-dot { width: 8px; height: 8px; border-radius: 50%; }
.node-dot.up { background: var(--color-green); }
.node-dot.down { background: var(--color-red); }
.node-name { font-weight: 600; color: var(--color-text); }
.node-ip { color: var(--color-faint); font-size: 0.7rem; }

.list-head { display: flex; align-items: baseline; gap: 10px; }
.list-title { font-size: 0.9rem; font-weight: 600; color: var(--color-text); }
.list-count { font-size: 0.74rem; color: var(--color-faint); }
.filters { margin-left: auto; display: flex; gap: 6px; }

.vm-row, .task-row {
  display: grid;
  gap: 10px;
  align-items: center;
  padding: 10px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-card);
  font-size: 0.82rem;
}
.vm-row { grid-template-columns: 0.5fr 1.4fr 0.8fr 0.6fr 0.9fr 0.9fr auto; }
.task-row { grid-template-columns: 1.2fr 0.8fr 0.8fr 1.4fr; }
.vm-row + .vm-row, .task-row + .task-row { margin-top: 6px; }
.vm-row--head, .task-row--head {
  background: none;
  border: none;
  padding-bottom: 0;
  font-size: 0.7rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.vm-row--head + .vm-row, .task-row--head + .task-row { margin-top: 0; }
.vm-name { color: var(--color-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.dim { color: var(--color-dim); }
.mono { font-family: var(--font-mono); font-size: 0.78rem; }
.type-badge {
  font-size: 0.66rem;
  padding: 2px 7px;
  border-radius: var(--rounded-full);
  font-weight: 600;
  background: var(--color-inset);
  color: var(--color-dim);
}
.type-badge.qemu { background: var(--color-primary-soft); color: var(--color-primary); }
.vm-status { display: inline-flex; align-items: center; gap: 6px; font-size: 0.78rem; }
.vm-status.running { color: var(--color-green); }
.vm-status.stopped { color: var(--color-faint); }
.status-dot { width: 7px; height: 7px; border-radius: 50%; background: currentColor; }
.row-ops { display: flex; align-items: center; gap: 4px; justify-content: flex-end; }
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
.act-btn:disabled { opacity: 0.5; cursor: not-allowed; }
.act-btn--danger { color: var(--color-red); }
.ro-hint { display: inline-flex; align-items: center; gap: 4px; font-size: 0.72rem; color: var(--color-faint); }
.task-status.ok { color: var(--color-green); }
.task-status.bad { color: var(--color-red); }
.task-status.run { color: var(--color-faint); }

@media (max-width: 960px) {
  .vm-row { grid-template-columns: 0.5fr 1.2fr 0.8fr auto; }
  .vm-row > :nth-child(4), .vm-row > :nth-child(5), .vm-row > :nth-child(6) { display: none; }
  .task-row { grid-template-columns: 1fr 0.8fr 0.8fr; }
  .task-row > :nth-child(4) { display: none; }
}
</style>
