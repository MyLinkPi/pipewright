<!--
  CephClusterDetail.vue — Ceph 集群管理界面(部署面板 + 六个管理 Tab)。

  · 头部:集群名/状态/健康徽标/dashboard 链接与密码查看(走凭据 reveal,留审计)。
  · 部署面板:created/failed 显示「开始部署」;运行中/结束时显示步骤 + 日志流
    (3s 轮询 deploy/status,自动滚动到底)。
  · Tab(集群 ready 后可用):
      总览   — 健康/容量条/mon/mgr/osd 计数/PG 状态/健康明细(15s 轮询);
      主机   — 意图 × orch 合并表 + 添加(选登记服务器+设备)/移除(drain 确认);
      OSD    — 实时表(osd df+metadata)+ 单盘追加 / 安全移除确认;
      存储池 — 列表 + 创建(副本/EC)/配额与副本数编辑/删除(输入名强确认);
      块设备 — 按池筛选镜像 + 创建/扩容/删除;快照子面板(创建/删除/回滚强确认);
      文件系统 — fs 卡片 + 子卷表(路径可复制)/创建/删除。
-->
<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import { Refresh, Rocket, Plus } from '@vicons/tabler'
import {
  getCephCluster,
  deployCephCluster,
  getCephDeployStatus,
  getCephClusterStatus,
  listCephHosts,
  addCephHost,
  removeCephHost,
  listCephOSDs,
  addCephOSD,
  removeCephOSD,
  listCephPools,
  createCephPool,
  updateCephPool,
  deleteCephPool,
  listCephImages,
  createCephImage,
  resizeCephImage,
  deleteCephImage,
  listCephSnapshots,
  createCephSnapshot,
  deleteCephSnapshot,
  rollbackCephSnapshot,
  listCephFilesystems,
  listCephSubvolumes,
  createCephSubvolume,
  deleteCephSubvolume,
  type CephCluster,
  type CephDeployStatus,
  type CephClusterStatus,
  type CephHost,
  type CephOSD,
  type CephPool,
  type CephRBDImage,
  type CephRBDSnapshot,
  type CephFilesystem,
  type CephSubvolume,
} from '../api/ceph'
import { listServers, type Server } from '../api/servers'
import { revealCredential } from '../api/credentials'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import AppButton from '../components/ui/AppButton.vue'
import AppSelect from '../components/ui/AppSelect.vue'
import ErrorState from '../components/ui/ErrorState.vue'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()
const route = useRoute()
const clusterId = route.params.id as string

function errMsg(err: unknown, fallback: string): string {
  return err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback
}

// ─── 集群与部署 ──────────────────────────────────────────────────────────────
const cluster = ref<CephCluster | null>(null)
const deploy = ref<CephDeployStatus | null>(null)
const loadError = ref('')
const deployBusy = ref(false)
const logBox = ref<HTMLElement | null>(null)
let clusterTimer: number | undefined
let deployTimer: number | undefined

async function loadCluster(): Promise<void> {
  try {
    const res = await getCephCluster(clusterId)
    cluster.value = res.cluster
    deploy.value = res.deploy
    loadError.value = ''
  } catch (err) {
    loadError.value = errMsg(err, t('ceph.errLoad'))
  }
}

async function pollDeploy(): Promise<void> {
  try {
    const st = await getCephDeployStatus(clusterId)
    deploy.value = st
    if (st.running) scheduleDeployPoll()
    else void loadCluster()
  } catch {
    /* 轮询失败静默,下个集群轮询周期兜底 */
  }
}

function scheduleDeployPoll(): void {
  if (deployTimer !== undefined) return
  deployTimer = window.setInterval(async () => {
    await pollDeploy()
    if (deploy.value && !deploy.value.running && deployTimer !== undefined) {
      window.clearInterval(deployTimer)
      deployTimer = undefined
    }
  }, 3_000)
}

async function startDeploy(): Promise<void> {
  deployBusy.value = true
  try {
    await deployCephCluster(clusterId)
    toast.success(t('ceph.deploy.started'))
    await pollDeploy()
    scheduleDeployPoll()
    void loadCluster()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.deploy.startFailed')))
  } finally {
    deployBusy.value = false
  }
}

const ready = computed(() => cluster.value?.state === 'ready')
const deployLog = computed(() => deploy.value?.log ?? [])

// ─── Tab 状态与数据 ──────────────────────────────────────────────────────────
type Tab = 'overview' | 'hosts' | 'osds' | 'pools' | 'images' | 'fs'
const tab = ref<Tab>('overview')
const status = ref<CephClusterStatus | null>(null)
const hosts = ref<CephHost[]>([])
const osds = ref<CephOSD[]>([])
const pools = ref<CephPool[]>([])
const images = ref<CephRBDImage[]>([])
const filesystems = ref<CephFilesystem[]>([])
const subvolumes = ref<CephSubvolume[]>([])
const servers = ref<Server[]>([])
let statusTimer: number | undefined

async function loadTab(): Promise<void> {
  if (!ready.value) return
  try {
    if (tab.value === 'overview') status.value = await getCephClusterStatus(clusterId)
    else if (tab.value === 'hosts') hosts.value = await listCephHosts(clusterId)
    else if (tab.value === 'osds') {
      osds.value = await listCephOSDs(clusterId)
      if (hosts.value.length === 0) hosts.value = await listCephHosts(clusterId)
    } else if (tab.value === 'pools') pools.value = await listCephPools(clusterId)
    else if (tab.value === 'images') {
      images.value = await listCephImages(clusterId, imagePool.value || (pools.value[0]?.name ?? ''))
    } else if (tab.value === 'fs') {
      filesystems.value = await listCephFilesystems(clusterId)
      if (filesystems.value.length > 0) subvolumes.value = await listCephSubvolumes(clusterId, filesystems.value[0].name)
    }
  } catch (err) {
    toast.error(errMsg(err, t('ceph.errLoad')))
  }
}

function fmtBytes(n: number | null | undefined): string {
  if (!n || n <= 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`
}

function healthClass(h: string): string {
  if (h === 'HEALTH_OK') return 'ok'
  if (h === 'HEALTH_WARN') return 'warn'
  return 'err'
}

function usedPct(): number {
  const s = status.value
  if (!s || !s.totalBytes) return 0
  return Math.min(100, Math.round((s.usedBytes / s.totalBytes) * 100))
}

// ─── dashboard 密码 ──────────────────────────────────────────────────────────
const dashSecret = ref('')
async function revealDashPassword(): Promise<void> {
  if (!cluster.value?.dashboardCredentialId) return
  try {
    dashSecret.value = await revealCredential(cluster.value.dashboardCredentialId)
    await navigator.clipboard?.writeText(dashSecret.value).catch(() => undefined)
    toast.success(t('ceph.dash.copied'))
  } catch (err) {
    toast.error(errMsg(err, t('ceph.dash.revealFailed')))
  }
}

// ─── 主机 ────────────────────────────────────────────────────────────────────
const addHostOpen = ref(false)
const addHostForm = ref({ serverId: '', labels: '', devices: '' })
const addHostBusy = ref(false)

function openAddHost(): void {
  addHostForm.value = { serverId: '', labels: '', devices: '' }
  addHostOpen.value = true
  if (servers.value.length === 0) {
    void listServers().then((list) => (servers.value = list)).catch(() => undefined)
  }
}

const serverOptions = computed(() =>
  servers.value
    .filter((sv) => !hosts.value.some((h) => h.serverId === sv.id))
    .map((sv) => ({ value: sv.id, label: sv.name })),
)

async function submitAddHost(): Promise<void> {
  addHostBusy.value = true
  try {
    await addCephHost(clusterId, {
      serverId: addHostForm.value.serverId,
      labels: addHostForm.value.labels.split(',').map((x) => x.trim()).filter(Boolean),
      devices: addHostForm.value.devices.split(/[\n,]/).map((x) => x.trim()).filter(Boolean),
    })
    toast.success(t('ceph.hosts.added'))
    addHostOpen.value = false
    void loadTab()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.hosts.addFailed')))
  } finally {
    addHostBusy.value = false
  }
}

async function onRemoveHost(h: CephHost): Promise<void> {
  const ok = await confirm.open({
    title: t('ceph.hosts.removeTitle', { host: h.hostname || h.serverName }),
    body: t('ceph.hosts.removeBody'),
    confirmLabel: t('ceph.hosts.removeConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await removeCephHost(clusterId, h.hostname || h.addr)
    toast.success(t('ceph.hosts.removed'))
    void loadTab()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.hosts.removeFailed')))
  }
}

// ─── OSD ─────────────────────────────────────────────────────────────────────
const osdForm = ref({ host: '', device: '' })
const osdBusy = ref(false)

const hostOptions = computed(() =>
  hosts.value.filter((h) => h.inOrch || h.hostname).map((h) => ({ value: h.hostname || h.addr, label: h.hostname || h.addr })),
)

async function submitAddOSD(): Promise<void> {
  if (!osdForm.value.host || !osdForm.value.device) return
  osdBusy.value = true
  try {
    await addCephOSD(clusterId, osdForm.value.host, osdForm.value.device.trim())
    toast.success(t('ceph.osds.added'))
    osdForm.value = { host: '', device: '' }
    void loadTab()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.osds.addFailed')))
  } finally {
    osdBusy.value = false
  }
}

async function onRemoveOSD(o: CephOSD): Promise<void> {
  const ok = await confirm.open({
    title: t('ceph.osds.removeTitle', { id: o.id }),
    body: t('ceph.osds.removeBody'),
    confirmLabel: t('ceph.osds.removeConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await removeCephOSD(clusterId, o.id)
    toast.success(t('ceph.osds.removed'))
    void loadTab()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.osds.removeFailed')))
  }
}

// ─── 存储池 ──────────────────────────────────────────────────────────────────
const poolModalOpen = ref(false)
const poolForm = ref({ name: '', pgNum: 128, type: 'replicated' as 'replicated' | 'erasure', size: 3, ecProfile: '', apps: 'rbd' })
const poolBusy = ref(false)
const quotaModal = ref<CephPool | null>(null)
const quotaGB = ref(0)
const quotaBusy = ref(false)

const poolTypeOptions = computed(() => [
  { value: 'replicated', label: t('ceph.pools.replicated') },
  { value: 'erasure', label: t('ceph.pools.erasure') },
])

async function submitCreatePool(): Promise<void> {
  poolBusy.value = true
  try {
    await createCephPool(clusterId, {
      name: poolForm.value.name.trim(),
      pgNum: poolForm.value.pgNum,
      type: poolForm.value.type,
      size: poolForm.value.type === 'replicated' ? poolForm.value.size : undefined,
      ecProfile: poolForm.value.type === 'erasure' ? poolForm.value.ecProfile.trim() : undefined,
      apps: poolForm.value.apps.split(',').map((x) => x.trim()).filter(Boolean),
    })
    toast.success(t('ceph.pools.created'))
    poolModalOpen.value = false
    void loadTab()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.pools.createFailed')))
  } finally {
    poolBusy.value = false
  }
}

function openQuota(p: CephPool): void {
  quotaModal.value = p
  quotaGB.value = p.quotaMaxBytes ? Math.round(p.quotaMaxBytes / 1024 ** 3) : 0
}

async function submitQuota(): Promise<void> {
  if (!quotaModal.value) return
  quotaBusy.value = true
  try {
    await updateCephPool(clusterId, quotaModal.value.name, {
      quotaMaxBytes: quotaGB.value > 0 ? Math.round(quotaGB.value * 1024 ** 3) : 0,
    })
    toast.success(t('ceph.pools.quotaSaved'))
    quotaModal.value = null
    void loadTab()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.pools.quotaFailed')))
  } finally {
    quotaBusy.value = false
  }
}

async function onDeletePool(p: CephPool): Promise<void> {
  const ok = await confirm.open({
    title: t('ceph.pools.deleteTitle', { name: p.name }),
    body: t('ceph.pools.deleteBody'),
    confirmText: p.name,
    confirmLabel: t('ceph.pools.deleteConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteCephPool(clusterId, p.name)
    toast.success(t('ceph.pools.deleted'))
    void loadTab()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.pools.deleteFailed')))
  }
}

// ─── RBD ─────────────────────────────────────────────────────────────────────
const imagePool = ref('')
const poolOptions = computed(() => pools.value.map((p) => ({ value: p.name, label: p.name })))
const imgModalOpen = ref(false)
const imgForm = ref({ name: '', sizeGb: 10 })
const imgBusy = ref(false)
const resizeModal = ref<CephRBDImage | null>(null)
const resizeGB = ref(0)
const snapFor = ref('') // 展开快照的镜像名
const snaps = ref<CephRBDSnapshot[]>([])
const snapName = ref('')

async function switchImagePool(pool: string): Promise<void> {
  imagePool.value = pool
  images.value = await listCephImages(clusterId, pool)
}

async function openImages(): Promise<void> {
  if (pools.value.length === 0) pools.value = await listCephPools(clusterId)
  const first = imagePool.value || pools.value[0]?.name || ''
  if (first) await switchImagePool(first)
}

async function submitCreateImage(): Promise<void> {
  imgBusy.value = true
  try {
    await createCephImage(clusterId, imagePool.value, imgForm.value.name.trim(), imgForm.value.sizeGb)
    toast.success(t('ceph.rbd.created'))
    imgModalOpen.value = false
    void openImages()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.rbd.createFailed')))
  } finally {
    imgBusy.value = false
  }
}

async function submitResize(): Promise<void> {
  if (!resizeModal.value) return
  const curGB = Math.round(resizeModal.value.sizeBytes / 1024 ** 3)
  const shrinking = resizeGB.value < curGB
  if (shrinking) {
    const ok = await confirm.open({
      title: t('ceph.rbd.shrinkTitle'),
      body: t('ceph.rbd.shrinkBody'),
      confirmLabel: t('ceph.rbd.shrinkConfirm'),
      variant: 'danger',
    })
    if (!ok) return
  }
  try {
    await resizeCephImage(clusterId, imagePool.value, resizeModal.value.name, resizeGB.value, shrinking)
    toast.success(t('ceph.rbd.resized'))
    resizeModal.value = null
    void openImages()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.rbd.resizeFailed')))
  }
}

async function onDeleteImage(img: CephRBDImage): Promise<void> {
  const ok = await confirm.open({
    title: t('ceph.rbd.deleteTitle', { name: img.name }),
    body: t('ceph.rbd.deleteBody'),
    confirmLabel: t('ceph.rbd.deleteConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteCephImage(clusterId, imagePool.value, img.name)
    toast.success(t('ceph.rbd.deleted'))
    void openImages()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.rbd.deleteFailed')))
  }
}

async function toggleSnaps(img: CephRBDImage): Promise<void> {
  if (snapFor.value === img.name) {
    snapFor.value = ''
    return
  }
  snapFor.value = img.name
  snapName.value = ''
  snaps.value = await listCephSnapshots(clusterId, imagePool.value, img.name)
}

async function submitCreateSnap(): Promise<void> {
  const name = snapName.value.trim()
  if (!name || !snapFor.value) return
  try {
    await createCephSnapshot(clusterId, imagePool.value, snapFor.value, name)
    toast.success(t('ceph.snap.created'))
    snapName.value = ''
    snaps.value = await listCephSnapshots(clusterId, imagePool.value, snapFor.value)
  } catch (err) {
    toast.error(errMsg(err, t('ceph.snap.createFailed')))
  }
}

async function onDeleteSnap(s: CephRBDSnapshot): Promise<void> {
  if (!snapFor.value) return
  const ok = await confirm.open({
    title: t('ceph.snap.deleteTitle', { name: s.name }),
    body: t('ceph.snap.deleteBody'),
    confirmLabel: t('ceph.snap.deleteConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteCephSnapshot(clusterId, imagePool.value, snapFor.value, s.name)
    snaps.value = await listCephSnapshots(clusterId, imagePool.value, snapFor.value)
  } catch (err) {
    toast.error(errMsg(err, t('ceph.snap.deleteFailed')))
  }
}

async function onRollbackSnap(s: CephRBDSnapshot): Promise<void> {
  if (!snapFor.value) return
  const ok = await confirm.open({
    title: t('ceph.snap.rollbackTitle', { name: s.name }),
    body: t('ceph.snap.rollbackBody'),
    confirmText: s.name,
    confirmLabel: t('ceph.snap.rollbackConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await rollbackCephSnapshot(clusterId, imagePool.value, snapFor.value, s.name)
    toast.success(t('ceph.snap.rolledBack'))
  } catch (err) {
    toast.error(errMsg(err, t('ceph.snap.rollbackFailed')))
  }
}

// ─── CephFS ──────────────────────────────────────────────────────────────────
const activeFs = ref('')
const svModalOpen = ref(false)
const svForm = ref({ name: '', sizeGb: 0 })
const svBusy = ref(false)

async function switchFs(fs: string): Promise<void> {
  activeFs.value = fs
  subvolumes.value = await listCephSubvolumes(clusterId, fs)
}

async function openFs(): Promise<void> {
  filesystems.value = await listCephFilesystems(clusterId)
  if (filesystems.value.length === 0) {
    subvolumes.value = []
    return
  }
  const first = activeFs.value || filesystems.value[0].name
  await switchFs(first)
}

async function submitCreateSv(): Promise<void> {
  svBusy.value = true
  try {
    await createCephSubvolume(clusterId, activeFs.value, svForm.value.name.trim(), svForm.value.sizeGb)
    toast.success(t('ceph.fs.svCreated'))
    svModalOpen.value = false
    subvolumes.value = await listCephSubvolumes(clusterId, activeFs.value)
  } catch (err) {
    toast.error(errMsg(err, t('ceph.fs.svCreateFailed')))
  } finally {
    svBusy.value = false
  }
}

async function onDeleteSv(sv: CephSubvolume): Promise<void> {
  const ok = await confirm.open({
    title: t('ceph.fs.svDeleteTitle', { name: sv.name }),
    body: t('ceph.fs.svDeleteBody'),
    confirmLabel: t('ceph.fs.svDeleteConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteCephSubvolume(clusterId, activeFs.value, sv.name)
    subvolumes.value = await listCephSubvolumes(clusterId, activeFs.value)
  } catch (err) {
    toast.error(errMsg(err, t('ceph.fs.svDeleteFailed')))
  }
}

async function copyPath(path: string): Promise<void> {
  await navigator.clipboard?.writeText(path).catch(() => undefined)
  toast.success(t('ceph.fs.pathCopied'))
}

// Tab 切换钩子:images/fs 需要级联加载。
async function onSwitchTab(next: Tab): Promise<void> {
  tab.value = next
  if (next === 'images') {
    await openImages()
    return
  }
  if (next === 'fs') {
    await openFs()
    return
  }
  void loadTab()
}

onMounted(async () => {
  await loadCluster()
  if (deploy.value?.running) scheduleDeployPoll()
  clusterTimer = window.setInterval(() => void loadCluster(), 10_000)
  statusTimer = window.setInterval(() => {
    if (tab.value === 'overview' && ready.value) void loadTab()
  }, 15_000)
  void loadTab()
  await nextTick()
  if (logBox.value) logBox.value.scrollTop = logBox.value.scrollHeight
})
onUnmounted(() => {
  if (clusterTimer !== undefined) window.clearInterval(clusterTimer)
  if (deployTimer !== undefined) window.clearInterval(deployTimer)
  if (statusTimer !== undefined) window.clearInterval(statusTimer)
})
</script>

<template>
  <div class="detail-page">
    <header class="page-head">
      <div>
        <h1>{{ cluster?.name || '…' }}</h1>
        <p class="sub mono">{{ cluster?.fsid || '' }}</p>
      </div>
      <div class="head-actions">
        <a
          v-if="cluster?.dashboardUrl"
          class="link-btn"
          :href="cluster.dashboardUrl"
          target="_blank"
          rel="noreferrer"
        >{{ t('ceph.list.openDashboard') }}</a>
        <AppButton
          v-if="cluster?.dashboardCredentialId"
          variant="default"
          size="sm"
          @click="revealDashPassword"
        >{{ t('ceph.dash.reveal') }}</AppButton>
        <AppButton variant="default" size="sm" @click="loadCluster">
          <Refresh class="btn-ico" />
          <span>{{ t('common.refresh') }}</span>
        </AppButton>
      </div>
    </header>

    <ErrorState v-if="loadError" :description="loadError" show-retry @retry="loadCluster" />
    <p v-if="dashSecret" class="dash-secret mono">{{ t('ceph.dash.password') }}: {{ dashSecret }}</p>

    <!-- 部署面板 -->
    <section v-if="cluster && cluster.state !== 'ready'" class="deploy-card">
      <div class="deploy-head">
        <span class="tag" :class="cluster.state === 'deploying' ? 'warn' : cluster.state === 'failed' ? 'err' : ''">
          {{ t(`ceph.state.${cluster.state}`) }}
        </span>
        <span v-if="deploy?.step" class="deploy-step mono">{{ deploy.step }} · {{ deploy.message }}</span>
        <AppButton
          v-if="cluster.state === 'created' || cluster.state === 'failed'"
          variant="primary"
          size="sm"
          :loading="deployBusy"
          @click="startDeploy"
        >
          <Rocket class="btn-ico" />
          <span>{{ t('ceph.deploy.start') }}</span>
        </AppButton>
      </div>
      <p v-if="deploy?.error" class="deploy-err">{{ deploy.error }}</p>
      <div v-if="deployLog.length > 0" ref="logBox" class="deploy-log mono">
        <div v-for="(line, i) in deployLog" :key="i">{{ line }}</div>
      </div>
    </section>

    <!-- 管理 Tab(ready 后) -->
    <template v-if="ready">
      <nav class="tabs" role="tablist">
        <button
          v-for="tb in (['overview','hosts','osds','pools','images','fs'] as Tab[])"
          :key="tb"
          class="tab-btn"
          :class="{ active: tab === tb }"
          role="tab"
          :aria-selected="tab === tb"
          @click="onSwitchTab(tb)"
        >{{ t(`ceph.tabs.${tb}`) }}</button>
      </nav>

      <!-- 总览 -->
      <section v-if="tab === 'overview'" class="panel">
        <div v-if="status" class="ov-grid">
          <div class="ov-item">
            <span class="ov-label">{{ t('ceph.ov.health') }}</span>
            <span class="tag" :class="healthClass(status.health)">{{ status.health }}</span>
          </div>
          <div class="ov-item">
            <span class="ov-label">{{ t('ceph.ov.capacity') }}</span>
            <div class="cap-bar">
              <div class="cap-fill" :style="{ width: usedPct() + '%' }"></div>
            </div>
            <span class="ov-value">{{ fmtBytes(status.usedBytes) }} / {{ fmtBytes(status.totalBytes) }}({{ usedPct() }}%)</span>
          </div>
          <div class="ov-item">
            <span class="ov-label">{{ t('ceph.ov.mons') }}</span>
            <span class="ov-value">{{ status.monCount }}</span>
          </div>
          <div class="ov-item">
            <span class="ov-label">{{ t('ceph.ov.mgrs') }}</span>
            <span class="ov-value">{{ status.mgrCount }}</span>
          </div>
          <div class="ov-item">
            <span class="ov-label">{{ t('ceph.ov.osds') }}</span>
            <span class="ov-value">{{ t('ceph.ov.osdCounts', { up: status.osdUp, total: status.osdIn }) }}</span>
          </div>
          <div class="ov-item">
            <span class="ov-label">{{ t('ceph.ov.pgs') }}</span>
            <span class="ov-value">
              <span v-for="pg in status.pgsByState" :key="pg.state" class="pg-chip mono">{{ pg.state }} ×{{ pg.count }}</span>
            </span>
          </div>
        </div>
        <div v-if="status?.healthDetail?.length" class="health-detail">
          <h3>{{ t('ceph.ov.healthDetail') }}</h3>
          <p v-for="(d, i) in status.healthDetail" :key="i" class="hd-line">{{ d }}</p>
        </div>
      </section>

      <!-- 主机 -->
      <section v-else-if="tab === 'hosts'" class="panel">
        <div class="panel-head">
          <AppButton variant="primary" size="sm" @click="openAddHost">
            <Plus class="btn-ico" /><span>{{ t('ceph.hosts.add') }}</span>
          </AppButton>
        </div>
        <div class="row head">
          <span>{{ t('ceph.hosts.host') }}</span><span>{{ t('ceph.hosts.addr') }}</span>
          <span>{{ t('ceph.hosts.roles') }}</span><span>{{ t('ceph.hosts.status') }}</span><span></span>
        </div>
        <div v-for="h in hosts" :key="h.serverId" class="row">
          <span class="strong">{{ h.hostname || h.serverName }}</span>
          <span class="mono">{{ h.addr || '—' }}</span>
          <span class="tags">
            <span v-for="r in h.roles" :key="r" class="tag ok">{{ r }}</span>
            <span v-for="d in h.devices" :key="d" class="tag mono">{{ d }}</span>
          </span>
          <span>
            <span v-if="h.inOrch" class="tag ok">{{ h.orchStatus || 'in orch' }}</span>
            <span v-else class="tag warn">{{ t('ceph.hosts.notInOrch') }}</span>
          </span>
          <span>
            <button class="link-btn danger" @click="onRemoveHost(h)">{{ t('ceph.hosts.remove') }}</button>
          </span>
        </div>
        <p v-if="hosts.length === 0" class="placeholder">{{ t('ceph.hosts.empty') }}</p>
      </section>

      <!-- OSD -->
      <section v-else-if="tab === 'osds'" class="panel">
        <div class="panel-head osd-form">
          <AppSelect v-model="osdForm.host" :options="hostOptions" :placeholder="t('ceph.osds.pickHost')" />
          <input v-model="osdForm.device" class="field-in" type="text" placeholder="/dev/sdc" />
          <AppButton variant="primary" size="sm" :loading="osdBusy" :disabled="!osdForm.host || !osdForm.device" @click="submitAddOSD">
            {{ t('ceph.osds.add') }}
          </AppButton>
        </div>
        <div class="row head">
          <span>ID</span><span>{{ t('ceph.osds.host') }}</span><span>{{ t('ceph.osds.device') }}</span>
          <span>{{ t('ceph.osds.status') }}</span><span>{{ t('ceph.osds.usage') }}</span><span></span>
        </div>
        <div v-for="o in osds" :key="o.id" class="row">
          <span class="mono strong">{{ o.id }}</span>
          <span>{{ o.host }}</span>
          <span class="mono">{{ o.device }}</span>
          <span>
            <span class="tag" :class="o.up ? 'ok' : 'err'">{{ o.up ? 'up' : 'down' }}</span>
            <span class="tag" :class="o.in ? 'ok' : 'warn'">{{ o.in ? 'in' : 'out' }}</span>
          </span>
          <span>{{ o.utilization.toFixed(0) }}% · {{ fmtBytes(o.usedBytes) }}/{{ fmtBytes(o.sizeBytes) }}</span>
          <span><button class="link-btn danger" @click="onRemoveOSD(o)">{{ t('ceph.osds.remove') }}</button></span>
        </div>
        <p v-if="osds.length === 0" class="placeholder">{{ t('ceph.osds.empty') }}</p>
      </section>

      <!-- 存储池 -->
      <section v-else-if="tab === 'pools'" class="panel">
        <div class="panel-head">
          <AppButton variant="primary" size="sm" @click="poolModalOpen = true">
            <Plus class="btn-ico" /><span>{{ t('ceph.pools.create') }}</span>
          </AppButton>
        </div>
        <div class="row head pool-cols">
          <span>{{ t('ceph.pools.name') }}</span><span>{{ t('ceph.pools.type') }}</span>
          <span>{{ t('ceph.pools.pg') }}</span><span>{{ t('ceph.pools.used') }}</span>
          <span>{{ t('ceph.pools.quota') }}</span><span></span>
        </div>
        <div v-for="p in pools" :key="p.name" class="row pool-cols">
          <span class="strong mono">{{ p.name }}</span>
          <span>
            {{ p.type === 'erasure' ? t('ceph.pools.erasure') : t('ceph.pools.replicated') }}
            <span v-if="p.type === 'erasure' && p.ecProfile" class="mono">({{ p.ecProfile }})</span>
            <span v-else class="muted">×{{ p.size }}</span>
          </span>
          <span>{{ p.pgNum }}</span>
          <span>{{ fmtBytes(p.bytesUsed) }}</span>
          <span>{{ p.quotaMaxBytes ? fmtBytes(p.quotaMaxBytes) : t('ceph.pools.unlimited') }}</span>
          <span class="row-actions">
            <button class="link-btn" @click="openQuota(p)">{{ t('ceph.pools.setQuota') }}</button>
            <button class="link-btn danger" @click="onDeletePool(p)">{{ t('ceph.pools.remove') }}</button>
          </span>
        </div>
        <p v-if="pools.length === 0" class="placeholder">{{ t('ceph.pools.empty') }}</p>
      </section>

      <!-- 块设备 -->
      <section v-else-if="tab === 'images'" class="panel">
        <div class="panel-head">
          <AppSelect
            :model-value="imagePool"
            :options="poolOptions"
            :placeholder="t('ceph.rbd.pickPool')"
            @update:model-value="switchImagePool"
          />
          <AppButton variant="primary" size="sm" :disabled="!imagePool" @click="imgModalOpen = true">
            <Plus class="btn-ico" /><span>{{ t('ceph.rbd.create') }}</span>
          </AppButton>
        </div>
        <div class="row head img-cols">
          <span>{{ t('ceph.rbd.name') }}</span><span>{{ t('ceph.rbd.size') }}</span>
          <span>{{ t('ceph.rbd.provisioning') }}</span><span></span>
        </div>
        <template v-for="img in images" :key="img.name">
          <div class="row img-cols">
            <span class="strong mono">{{ img.name }}</span>
            <span>{{ fmtBytes(img.sizeBytes) }}</span>
            <span>{{ img.provisioning || '—' }}</span>
            <span class="row-actions">
              <button class="link-btn" @click="toggleSnaps(img)">
                {{ snapFor === img.name ? t('ceph.snap.collapse') : t('ceph.snap.expand') }}
              </button>
              <button class="link-btn" @click="resizeModal = img; resizeGB = Math.round(img.sizeBytes / 1024 ** 3)">
                {{ t('ceph.rbd.resize') }}
              </button>
              <button class="link-btn danger" @click="onDeleteImage(img)">{{ t('ceph.rbd.remove') }}</button>
            </span>
          </div>
          <div v-if="snapFor === img.name" class="snap-block">
            <div class="snap-form">
              <input v-model="snapName" class="field-in" type="text" :placeholder="t('ceph.snap.namePh')" />
              <AppButton variant="default" size="sm" :disabled="!snapName.trim()" @click="submitCreateSnap">
                {{ t('ceph.snap.create') }}
              </AppButton>
            </div>
            <div v-for="s in snaps" :key="s.name" class="snap-row">
              <span class="mono strong">@{{ s.name }}</span>
              <span>{{ fmtBytes(s.sizeBytes) }}</span>
              <span class="row-actions">
                <button class="link-btn danger" @click="onRollbackSnap(s)">{{ t('ceph.snap.rollback') }}</button>
                <button class="link-btn danger" @click="onDeleteSnap(s)">{{ t('ceph.snap.remove') }}</button>
              </span>
            </div>
            <p v-if="snaps.length === 0" class="placeholder">{{ t('ceph.snap.empty') }}</p>
          </div>
        </template>
        <p v-if="images.length === 0" class="placeholder">{{ t('ceph.rbd.empty') }}</p>
      </section>

      <!-- 文件系统 -->
      <section v-else-if="tab === 'fs'" class="panel">
        <div class="fs-cards">
          <div v-for="f in filesystems" :key="f.name" class="fs-card" :class="{ active: activeFs === f.name }" @click="switchFs(f.name)">
            <h4>{{ f.name }}</h4>
            <p class="muted">{{ t('ceph.fs.meta') }}: <span class="mono">{{ f.metadataPool }}</span></p>
            <p class="muted">{{ t('ceph.fs.data') }}: <span class="mono">{{ f.dataPools.join(', ') || '—' }}</span></p>
            <p class="muted">{{ t('ceph.fs.mds') }}: {{ f.mdsCount }}</p>
          </div>
        </div>
        <template v-if="activeFs">
          <div class="panel-head">
            <AppButton variant="primary" size="sm" @click="svModalOpen = true">
              <Plus class="btn-ico" /><span>{{ t('ceph.fs.svCreate') }}</span>
            </AppButton>
          </div>
          <div class="row head sv-cols">
            <span>{{ t('ceph.fs.svName') }}</span><span>{{ t('ceph.fs.svPath') }}</span>
            <span>{{ t('ceph.fs.svUsage') }}</span><span></span>
          </div>
          <div v-for="sv in subvolumes" :key="sv.name" class="row sv-cols">
            <span class="strong mono">{{ sv.name }}</span>
            <span class="mono path" :title="sv.path">{{ sv.path || '—' }}</span>
            <span>{{ fmtBytes(sv.bytesUsed) }}<span v-if="sv.quotaBytes"> / {{ fmtBytes(sv.quotaBytes) }}</span></span>
            <span class="row-actions">
              <button class="link-btn" :disabled="!sv.path" @click="copyPath(sv.path)">{{ t('ceph.fs.copyPath') }}</button>
              <button class="link-btn danger" @click="onDeleteSv(sv)">{{ t('ceph.fs.svRemove') }}</button>
            </span>
          </div>
          <p v-if="subvolumes.length === 0" class="placeholder">{{ t('ceph.fs.svEmpty') }}</p>
        </template>
        <p v-if="filesystems.length === 0" class="placeholder">{{ t('ceph.fs.empty') }}</p>
      </section>
    </template>

    <!-- 添加主机 -->
    <div v-if="addHostOpen" class="modal" role="dialog" aria-modal="true">
      <div class="modal-card">
        <header class="modal-head"><h3>{{ t('ceph.hosts.add') }}</h3><button class="modal-close" @click="addHostOpen = false">×</button></header>
        <div class="wiz-form">
          <label class="field">
            <span class="field-lbl">{{ t('ceph.hosts.pickServer') }}</span>
            <AppSelect v-model="addHostForm.serverId" :options="serverOptions" :placeholder="t('ceph.hosts.pickServerPh')" />
          </label>
          <label class="field">
            <span class="field-lbl">{{ t('ceph.hosts.labels') }}</span>
            <input v-model="addHostForm.labels" class="field-in" type="text" placeholder="osd,rgw" />
          </label>
          <label class="field">
            <span class="field-lbl">{{ t('ceph.hosts.devices') }}</span>
            <textarea v-model="addHostForm.devices" class="field-in" rows="3" :placeholder="t('ceph.wizard.devicesPh')" />
          </label>
          <div class="form-actions">
            <AppButton variant="default" @click="addHostOpen = false">{{ t('ceph.cancel') }}</AppButton>
            <AppButton variant="primary" :loading="addHostBusy" :disabled="!addHostForm.serverId" @click="submitAddHost">
              {{ t('ceph.hosts.add') }}
            </AppButton>
          </div>
        </div>
      </div>
    </div>

    <!-- 建池 -->
    <div v-if="poolModalOpen" class="modal" role="dialog" aria-modal="true">
      <div class="modal-card">
        <header class="modal-head"><h3>{{ t('ceph.pools.create') }}</h3><button class="modal-close" @click="poolModalOpen = false">×</button></header>
        <div class="wiz-form">
          <div class="grid2">
            <label class="field">
              <span class="field-lbl">{{ t('ceph.pools.name') }}</span>
              <input v-model="poolForm.name" class="field-in" type="text" placeholder="data" />
            </label>
            <label class="field">
              <span class="field-lbl">{{ t('ceph.pools.pg') }}</span>
              <input v-model.number="poolForm.pgNum" class="field-in" type="number" min="1" />
            </label>
          </div>
          <label class="field">
            <span class="field-lbl">{{ t('ceph.pools.type') }}</span>
            <AppSelect v-model="poolForm.type" :options="poolTypeOptions" />
          </label>
          <div v-if="poolForm.type === 'replicated'" class="grid2">
            <label class="field">
              <span class="field-lbl">{{ t('ceph.pools.replicas') }}</span>
              <input v-model.number="poolForm.size" class="field-in" type="number" min="2" max="10" />
            </label>
          </div>
          <div v-else class="grid2">
            <label class="field">
              <span class="field-lbl">{{ t('ceph.pools.ecProfile') }}</span>
              <input v-model="poolForm.ecProfile" class="field-in" type="text" :placeholder="t('ceph.pools.ecDefault')" />
            </label>
          </div>
          <label class="field">
            <span class="field-lbl">{{ t('ceph.pools.apps') }}</span>
            <input v-model="poolForm.apps" class="field-in" type="text" placeholder="rbd" />
          </label>
          <div class="form-actions">
            <AppButton variant="default" @click="poolModalOpen = false">{{ t('ceph.cancel') }}</AppButton>
            <AppButton variant="primary" :loading="poolBusy" @click="submitCreatePool">{{ t('ceph.pools.create') }}</AppButton>
          </div>
        </div>
      </div>
    </div>

    <!-- 配额 -->
    <div v-if="quotaModal" class="modal" role="dialog" aria-modal="true">
      <div class="modal-card">
        <header class="modal-head"><h3>{{ t('ceph.pools.setQuota') }} · {{ quotaModal.name }}</h3><button class="modal-close" @click="quotaModal = null">×</button></header>
        <div class="wiz-form">
          <label class="field">
            <span class="field-lbl">{{ t('ceph.pools.quotaGB') }}</span>
            <input v-model.number="quotaGB" class="field-in" type="number" min="0" />
            <span class="hint">{{ t('ceph.pools.quotaHint') }}</span>
          </label>
          <div class="form-actions">
            <AppButton variant="default" @click="quotaModal = null">{{ t('ceph.cancel') }}</AppButton>
            <AppButton variant="primary" :loading="quotaBusy" @click="submitQuota">{{ t('ceph.save') }}</AppButton>
          </div>
        </div>
      </div>
    </div>

    <!-- 建镜像 -->
    <div v-if="imgModalOpen" class="modal" role="dialog" aria-modal="true">
      <div class="modal-card">
        <header class="modal-head"><h3>{{ t('ceph.rbd.create') }} · {{ imagePool }}</h3><button class="modal-close" @click="imgModalOpen = false">×</button></header>
        <div class="wiz-form">
          <div class="grid2">
            <label class="field">
              <span class="field-lbl">{{ t('ceph.rbd.name') }}</span>
              <input v-model="imgForm.name" class="field-in" type="text" placeholder="disk1" />
            </label>
            <label class="field">
              <span class="field-lbl">GiB</span>
              <input v-model.number="imgForm.sizeGb" class="field-in" type="number" min="1" />
            </label>
          </div>
          <div class="form-actions">
            <AppButton variant="default" @click="imgModalOpen = false">{{ t('ceph.cancel') }}</AppButton>
            <AppButton variant="primary" :loading="imgBusy" @click="submitCreateImage">{{ t('ceph.rbd.create') }}</AppButton>
          </div>
        </div>
      </div>
    </div>

    <!-- 扩容 -->
    <div v-if="resizeModal" class="modal" role="dialog" aria-modal="true">
      <div class="modal-card">
        <header class="modal-head"><h3>{{ t('ceph.rbd.resize') }} · {{ resizeModal.name }}</h3><button class="modal-close" @click="resizeModal = null">×</button></header>
        <div class="wiz-form">
          <label class="field">
            <span class="field-lbl">{{ t('ceph.rbd.newSize') }}(GiB)</span>
            <input v-model.number="resizeGB" class="field-in" type="number" min="1" />
            <span class="hint">{{ t('ceph.rbd.resizeHint') }}</span>
          </label>
          <div class="form-actions">
            <AppButton variant="default" @click="resizeModal = null">{{ t('ceph.cancel') }}</AppButton>
            <AppButton variant="primary" @click="submitResize">{{ t('ceph.rbd.resize') }}</AppButton>
          </div>
        </div>
      </div>
    </div>

    <!-- 建子卷 -->
    <div v-if="svModalOpen" class="modal" role="dialog" aria-modal="true">
      <div class="modal-card">
        <header class="modal-head"><h3>{{ t('ceph.fs.svCreate') }} · {{ activeFs }}</h3><button class="modal-close" @click="svModalOpen = false">×</button></header>
        <div class="wiz-form">
          <div class="grid2">
            <label class="field">
              <span class="field-lbl">{{ t('ceph.fs.svName') }}</span>
              <input v-model="svForm.name" class="field-in" type="text" placeholder="vol1" />
            </label>
            <label class="field">
              <span class="field-lbl">{{ t('ceph.fs.svQuota') }}(GiB, 0 = {{ t('ceph.pools.unlimited') }})</span>
              <input v-model.number="svForm.sizeGb" class="field-in" type="number" min="0" />
            </label>
          </div>
          <div class="form-actions">
            <AppButton variant="default" @click="svModalOpen = false">{{ t('ceph.cancel') }}</AppButton>
            <AppButton variant="primary" :loading="svBusy" @click="submitCreateSv">{{ t('ceph.fs.svCreate') }}</AppButton>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.detail-page { display: flex; flex-direction: column; gap: 14px; }
.page-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; }
.page-head h1 { margin: 0; font-size: 20px; }
.sub { margin: 4px 0 0; color: var(--color-text-muted, #888); font-size: 12px; }
.head-actions { display: flex; align-items: center; gap: 8px; }
.btn-ico { width: 14px; height: 14px; }
.mono { font-family: var(--font-mono, monospace); }
.muted { color: var(--color-text-muted, #888); }
.strong { font-weight: 600; }
.placeholder { color: var(--color-text-muted, #888); font-size: 13px; }
.link-btn { background: none; border: none; color: var(--color-primary, #4c6ef5); cursor: pointer; font-size: 13px; padding: 0; text-decoration: none; }
.link-btn.danger { color: #e03131; }
.link-btn:disabled { opacity: .4; cursor: default; }
.dash-secret { margin: 0; font-size: 13px; color: var(--color-amber, #ffc107); }

.tag { display: inline-flex; align-items: center; padding: 1px 8px; border-radius: 999px; font-size: 11px; background: var(--color-bg-soft, #1c1c1c); }
.tag.ok { background: var(--color-green-soft, rgba(76, 175, 80, 0.15)); color: var(--color-green, #4caf50); }
.tag.warn { background: var(--color-amber-soft, rgba(255, 193, 7, 0.15)); color: var(--color-amber, #ffc107); }
.tag.err { background: rgba(201, 42, 42, 0.15); color: #e03131; }

/* 部署面板 */
.deploy-card { border: 1px solid var(--color-border, #333); border-radius: 10px; padding: 14px 16px; display: flex; flex-direction: column; gap: 10px; }
.deploy-head { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.deploy-step { font-size: 12px; color: var(--color-text-muted, #888); }
.deploy-err { margin: 0; font-size: 13px; color: #e03131; white-space: pre-wrap; }
.deploy-log { max-height: 320px; overflow: auto; background: var(--color-bg-soft, #161616); border-radius: 8px; padding: 10px 12px; font-size: 12px; line-height: 1.5; white-space: pre-wrap; }

/* Tab */
.tabs { display: flex; gap: 4px; border-bottom: 1px solid var(--color-border, #333); }
.tab-btn { border: none; background: none; color: var(--color-text-muted, #888); padding: 8px 14px; font-size: 13px; cursor: pointer; border-bottom: 2px solid transparent; }
.tab-btn.active { color: var(--color-primary, #4c6ef5); border-bottom-color: var(--color-primary, #4c6ef5); }

.panel { border: 1px solid var(--color-border, #333); border-radius: 10px; padding: 12px 16px 16px; display: flex; flex-direction: column; gap: 6px; }
.panel-head { display: flex; gap: 8px; align-items: center; padding-bottom: 8px; flex-wrap: wrap; }
.osd-form { min-width: 420px; max-width: 560px; }

.row { display: grid; gap: 8px; padding: 8px 0; align-items: center; font-size: 13px; border-top: 1px solid var(--color-border, #2a2a2a); }
.row.head { border-top: none; color: var(--color-text-muted, #888); font-size: 12px; }
.row > .row-actions { display: flex; gap: 10px; justify-content: flex-end; }
.tags { display: flex; gap: 4px; flex-wrap: wrap; }
.row { grid-template-columns: repeat(6, minmax(0, 1.2fr)) 0.8fr; }
.pool-cols { grid-template-columns: 1.4fr 1.2fr 0.6fr 1fr 1fr 1.4fr; }
.img-cols { grid-template-columns: 2fr 1fr 1fr 2fr; }
.sv-cols { grid-template-columns: 1fr 2.4fr 1fr 1.6fr; }
.path { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

/* 总览 */
.ov-grid { display: flex; flex-wrap: wrap; gap: 24px; padding: 6px 0; }
.ov-item { display: flex; flex-direction: column; gap: 4px; }
.ov-label { font-size: 12px; color: var(--color-text-muted, #888); }
.ov-value { font-size: 14px; font-weight: 600; display: flex; gap: 6px; flex-wrap: wrap; align-items: center; }
.cap-bar { width: 260px; max-width: 60vw; height: 8px; border-radius: 999px; background: var(--color-bg-soft, #1c1c1c); overflow: hidden; }
.cap-fill { height: 100%; background: var(--color-primary, #4c6ef5); transition: width .3s; }
.pg-chip { font-size: 11px; padding: 1px 8px; border-radius: 999px; background: var(--color-bg-soft, #1c1c1c); font-weight: 400; }
.health-detail { border-top: 1px dashed var(--color-border, #2a2a2a); padding-top: 10px; }
.health-detail h3 { margin: 0 0 6px; font-size: 13px; color: var(--color-text-muted, #888); }
.hd-line { margin: 2px 0; font-size: 12px; font-family: var(--font-mono, monospace); }

/* 快照 */
.snap-block { border-top: 1px dashed var(--color-border, #2a2a2a); background: var(--color-bg-soft, #161616); padding: 8px 12px; display: flex; flex-direction: column; gap: 6px; }
.snap-form { display: flex; gap: 8px; }
.snap-form .field-in { max-width: 260px; }
.snap-row { display: flex; gap: 12px; align-items: center; font-size: 13px; justify-content: space-between; }

/* CephFS */
.fs-cards { display: flex; gap: 10px; flex-wrap: wrap; padding: 6px 0 10px; }
.fs-card { border: 1px solid var(--color-border, #333); border-radius: 8px; padding: 10px 14px; min-width: 220px; cursor: pointer; }
.fs-card.active { border-color: var(--color-primary, #4c6ef5); }
.fs-card h4 { margin: 0 0 6px; font-size: 14px; }
.fs-card p { margin: 2px 0; font-size: 12px; }

/* 弹窗 */
.modal { position: fixed; inset: 0; background: rgba(0, 0, 0, 0.45); display: flex; align-items: center; justify-content: center; z-index: 60; padding: 16px; }
.modal-card { background: var(--color-bg, #141414); color: inherit; border: 1px solid var(--color-border, #333); border-radius: 12px; width: min(520px, 100%); max-height: 86vh; overflow: auto; padding: 18px 20px; }
.modal-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 10px; }
.modal-head h3 { margin: 0; font-size: 15px; }
.modal-close { border: none; background: none; font-size: 18px; cursor: pointer; color: inherit; }
.wiz-form { display: flex; flex-direction: column; gap: 10px; }
.field { display: flex; flex-direction: column; gap: 4px; }
.field-lbl { font-size: 12px; color: var(--color-text-muted, #888); }
.field-in { border: 1px solid var(--color-border, #333); border-radius: 6px; padding: 7px 10px; font-size: 13px; background: transparent; color: inherit; width: 100%; }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; }
.hint { font-size: 12px; color: var(--color-text-muted, #888); margin: 0; }
.form-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 6px; }
</style>
