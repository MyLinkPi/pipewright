<script setup lang="ts">
/*
  Tunnels.vue — SSH 隧道管理(local -L / remote -R / dynamic -D)。配置 CRUD(仿
  SettingsServers 的 Modal 表单)与状态监控(仿 Pm2 的轮询/筛选/KPI)同页。
  隧道跑在独立 helper 子进程里:重启 pipewright 不影响隧道,本页只读运行态。
*/
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  listTunnels, createTunnel, updateTunnel, deleteTunnel,
  startTunnel, stopTunnel, restartTunnel, testTunnel, getTunnelLogs,
  type SshTunnel, type TunnelInput, type TunnelMode,
} from '../api/tunnels'
import { listServers } from '../api/servers'
import { listCredentials } from '../api/credentials'
import { HttpError } from '../api/http'
import { tunnelBucket, tunnelStateMeta, tunnelModeMeta, tunnelActionsFor, tunnelBindLabel, tunnelDestLabel, tunnelTargetLabel, tunnelUptimeLabel, type TunnelBucket } from '../lib/tunnelState'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import { useDirtyGuard } from '../composables/useDirtyGuard'
import AppButton from '../components/ui/AppButton.vue'
import EmptyState from '../components/ui/EmptyState.vue'
import ErrorState from '../components/ui/ErrorState.vue'
import SkeletonBlock from '../components/ui/SkeletonBlock.vue'

type LoadState = 'idle' | 'loading' | 'error'
type StateFilter = 'all' | TunnelBucket
type ConnectKind = 'server' | 'direct'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()
const dirtyGuard = useDirtyGuard()

const POLL_MS = 10_000
let timer: ReturnType<typeof setInterval> | null = null

// ─── 列表与轮询 ────────────────────────────────────────────────────────────────
const loadState = ref<LoadState>('idle')
const loadError = ref('')
const tunnels = ref<SshTunnel[]>([])
const refreshing = ref(false)

async function load(): Promise<void> {
  const firstLoad = tunnels.value.length === 0
  loadState.value = 'loading'
  loadError.value = ''
  if (!firstLoad) refreshing.value = true
  try {
    tunnels.value = await listTunnels()
    loadState.value = 'idle'
  } catch (err) {
    if (err instanceof HttpError) {
      loadError.value =
        err.status === 0 ? t('tunnels.errConnect') : (err.apiError?.message ?? t('tunnels.errLoad', { status: err.status }))
    } else {
      loadError.value = t('tunnels.errRetry')
    }
    loadState.value = 'error'
  } finally {
    refreshing.value = false
  }
}

function scheduleLoad(): void {
  // 页面不可见时暂停轮询、回前台立即补拉(ServerStatus 同款)。
  if (typeof document !== 'undefined' && document.hidden) return
  void load()
}
function onVisibility(): void {
  if (!document.hidden) void load()
}

onMounted(() => {
  void load()
  void loadFormSources()
  timer = setInterval(scheduleLoad, POLL_MS)
  document.addEventListener('visibilitychange', onVisibility)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
  document.removeEventListener('visibilitychange', onVisibility)
})

// ─── KPI 与筛选 ────────────────────────────────────────────────────────────────
const counts = computed(() => {
  let running = 0
  let pending = 0
  let off = 0
  for (const tn of tunnels.value) {
    const b = tunnelBucket(tn.status)
    if (b === 'running') running++
    else if (b === 'pending') pending++
    else off++
  }
  return { all: tunnels.value.length, running, pending, off }
})

const stateFilter = ref<StateFilter>('all')
const FILTER_OPTIONS: { key: StateFilter; labelKey: string }[] = [
  { key: 'all', labelKey: 'tunnels.filterAll' },
  { key: 'running', labelKey: 'tunnels.filterRunning' },
  { key: 'pending', labelKey: 'tunnels.filterPending' },
  { key: 'off', labelKey: 'tunnels.filterOff' },
]
function filterCount(key: StateFilter): number {
  return counts.value[key]
}

const searchText = ref('')
const visibleTunnels = computed(() => {
  const q = searchText.value.trim().toLowerCase()
  return tunnels.value.filter((tn) => {
    if (stateFilter.value !== 'all' && tunnelBucket(tn.status) !== stateFilter.value) return false
    if (!q) return true
    return (
      tn.name.toLowerCase().includes(q) ||
      tn.serverName.toLowerCase().includes(q) ||
      tn.host.toLowerCase().includes(q) ||
      tn.destHost.toLowerCase().includes(q)
    )
  })
})

// ─── 行操作 ────────────────────────────────────────────────────────────────────
const busy = ref<Set<string>>(new Set())
function setBusy(key: string, on: boolean): void {
  const next = new Set(busy.value)
  if (on) next.add(key)
  else next.delete(key)
  busy.value = next
}

function httpErrMsg(err: unknown, fallback: string): string {
  if (err instanceof HttpError) return err.apiError?.message ?? fallback
  return fallback
}

async function runAction(tn: SshTunnel, action: 'start' | 'stop' | 'restart'): Promise<void> {
  const label = t(`tunnels.action${action[0].toUpperCase()}${action.slice(1)}` as 'tunnels.actionStart')
  if (action !== 'start') {
    const ok = await confirm.open({
      title: t('tunnels.confirmActionTitle', { label, name: tn.name }),
      body: t('tunnels.confirmActionBody', { label }),
      confirmLabel: label,
      variant: 'danger',
    })
    if (!ok) return
  }
  const key = `${tn.id}:${action}`
  setBusy(key, true)
  try {
    const fn = action === 'start' ? startTunnel : action === 'stop' ? stopTunnel : restartTunnel
    await fn(tn.id)
    toast.success(t('tunnels.actionDone', { label, name: tn.name }))
    setTimeout(() => void load(), 600)
  } catch (err) {
    toast.error(t('tunnels.actionFail', { label, name: tn.name }), { detail: httpErrMsg(err, t('tunnels.errNetwork')) })
  } finally {
    setBusy(key, false)
  }
}

const testingId = ref('')
const testResult = ref<{ name: string; ok: boolean; error: string | null; latencyMs: number } | null>(null)
async function runTest(tn: SshTunnel): Promise<void> {
  if (testingId.value) return
  testingId.value = tn.id
  testResult.value = null
  try {
    const res = await testTunnel(tn.id)
    testResult.value = { name: tn.name, ok: res.ok, error: res.error, latencyMs: res.latencyMs }
    if (res.ok) toast.success(t('tunnels.testOk', { name: tn.name }), { detail: t('tunnels.testLatency', { n: res.latencyMs }) })
    else toast.error(t('tunnels.testFail', { name: tn.name }), { detail: res.error ?? '' })
  } catch (err) {
    toast.error(t('tunnels.testFail', { name: tn.name }), { detail: httpErrMsg(err, t('tunnels.errNetwork')) })
  } finally {
    testingId.value = ''
  }
}

// ─── 表单(新建/编辑)─────────────────────────────────────────────────────────
const servers = ref<{ id: string; name: string }[]>([])
const sshCredentials = ref<{ id: string; name: string }[]>([])

async function loadFormSources(): Promise<void> {
  try {
    const [srvs, creds] = await Promise.all([listServers(), listCredentials()])
    servers.value = srvs.map((s) => ({ id: s.id, name: s.name }))
    sshCredentials.value = creds
      .filter((c) => c.type === 'ssh_key' || c.type === 'ssh_password')
      .map((c) => ({ id: c.id, name: c.name }))
  } catch {
    // 下拉数据源加载失败不阻断页面:打开表单时再试一次。
  }
}

const modalOpen = ref(false)
const modalMode = ref<'add' | 'edit'>('add')
const editingId = ref('')
const formSubmitting = ref(false)
const formBanner = ref('')
let formSnapshot = ''

interface FormState {
  name: string
  connect: ConnectKind
  serverId: string
  host: string
  port: number | null
  user: string
  credentialId: string
  mode: TunnelMode
  bindHost: string
  bindPort: number | null
  destHost: string
  destPort: number | null
  enabled: boolean
}
const form = ref<FormState>(emptyForm())
const formErrors = ref<Record<string, string>>({})

function emptyForm(): FormState {
  return {
    name: '', connect: 'direct', serverId: '', host: '', port: 22, user: '', credentialId: '',
    mode: 'local', bindHost: '127.0.0.1', bindPort: null, destHost: '', destPort: null, enabled: true,
  }
}

function openAddModal(): void {
  void loadFormSources()
  modalMode.value = 'add'
  editingId.value = ''
  form.value = emptyForm()
  formErrors.value = {}
  formBanner.value = ''
  formSnapshot = JSON.stringify(form.value)
  modalOpen.value = true
}

function openEditModal(tn: SshTunnel): void {
  void loadFormSources()
  modalMode.value = 'edit'
  editingId.value = tn.id
  form.value = {
    name: tn.name,
    connect: tn.serverId ? 'server' : 'direct',
    serverId: tn.serverId,
    host: tn.host,
    port: tn.port || 22,
    user: tn.user,
    credentialId: tn.credentialId,
    mode: tn.mode,
    bindHost: tn.bindHost || '127.0.0.1',
    bindPort: tn.bindPort,
    destHost: tn.destHost,
    destPort: tn.destPort || null,
    enabled: tn.enabled,
  }
  formErrors.value = {}
  formBanner.value = ''
  formSnapshot = JSON.stringify(form.value)
  modalOpen.value = true
}

async function closeModal(): Promise<void> {
  if (JSON.stringify(form.value) !== formSnapshot) {
    const ok = await dirtyGuard.confirmDiscard()
    if (!ok) return
  }
  modalOpen.value = false
}

// ─── VPN(-w)字段标签换轨:vpn 模式下 bind/dest 字段承载隧道地址与 tun 序号 ────
const bindHostLabel = computed(() => (form.value.mode === 'vpn' ? t('tunnels.fieldLocalIP') : t('tunnels.fieldBindHost')))
const bindPortLabel = computed(() => (form.value.mode === 'vpn' ? t('tunnels.fieldLocalTun') : t('tunnels.fieldBindPort')))
const bindHintText = computed(() => (form.value.mode === 'vpn' ? t('tunnels.vpnBindHint') : t('tunnels.bindHint')))
const destHostLabel = computed(() => (form.value.mode === 'vpn' ? t('tunnels.fieldRemoteIP') : t('tunnels.fieldDestHost')))
const destPortLabel = computed(() => (form.value.mode === 'vpn' ? t('tunnels.fieldRemoteTun') : t('tunnels.fieldDestPort')))

function isIPv4(s: string): boolean {
  const parts = s.split('.')
  if (parts.length !== 4) return false
  return parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) >= 0 && Number(p) <= 255)
}

/** 两地址是否同一 /30(点对点网段)。 */
function sameSubnet30(a: string, b: string): boolean {
  const toInt = (ip: string) => ip.split('.').reduce((acc, o) => acc * 256 + Number(o), 0)
  return (toInt(a) & ~3) === (toInt(b) & ~3)
}

function validateForm(): boolean {
  const errs: Record<string, string> = {}
  const f = form.value
  if (!f.name.trim()) errs.name = t('tunnels.errName')
  if (f.connect === 'server') {
    if (!f.serverId) errs.serverId = t('tunnels.errServer')
  } else {
    if (!f.host.trim()) errs.host = t('tunnels.errHost')
    if (!f.user.trim()) errs.user = t('tunnels.errUser')
    if (!f.credentialId) errs.credentialId = t('tunnels.errCredential')
    if (f.port === null || f.port < 1 || f.port > 65535) errs.port = t('tunnels.errPortRange')
  }
  if (f.mode === 'vpn') {
    // vpn:两端隧道 IP(同 /30)+ tun 序号 0-255。
    if (!isIPv4(f.bindHost.trim())) errs.bindHost = t('tunnels.errVpnIP')
    if (f.bindPort === null || f.bindPort < 0 || f.bindPort > 255) errs.bindPort = t('tunnels.errVpnTunIdx')
    if (!isIPv4(f.destHost.trim())) errs.destHost = t('tunnels.errVpnIP')
    else if (isIPv4(f.bindHost.trim()) && !sameSubnet30(f.bindHost.trim(), f.destHost.trim())) errs.destHost = t('tunnels.errVpnSameSubnet')
    if (f.destPort === null || f.destPort < 0 || f.destPort > 255) errs.destPort = t('tunnels.errVpnTunIdx')
  } else {
    if (f.bindHost.trim() === '') errs.bindHost = t('tunnels.errBindHost')
    if (f.bindPort === null || f.bindPort < 0 || f.bindPort > 65535) errs.bindPort = t('tunnels.errBindPort')
    if (f.mode !== 'dynamic') {
      if (!f.destHost.trim()) errs.destHost = t('tunnels.errDestHost')
      if (f.destPort === null || f.destPort < 1 || f.destPort > 65535) errs.destPort = t('tunnels.errPortRange')
    }
  }
  formErrors.value = errs
  return Object.keys(errs).length === 0
}

function toInput(): TunnelInput {
  const f = form.value
  return {
    name: f.name.trim(),
    serverId: f.connect === 'server' ? f.serverId : '',
    host: f.connect === 'direct' ? f.host.trim() : '',
    port: f.connect === 'direct' ? (f.port ?? 22) : 0,
    user: f.connect === 'direct' ? f.user.trim() : '',
    credentialId: f.connect === 'direct' ? f.credentialId : '',
    mode: f.mode,
    bindHost: f.bindHost.trim(),
    bindPort: f.bindPort ?? 0,
    destHost: f.mode === 'dynamic' ? '' : f.destHost.trim(),
    destPort: f.mode === 'dynamic' ? 0 : (f.destPort ?? 0),
    enabled: f.enabled,
  }
}

async function handleFormSubmit(): Promise<void> {
  if (formSubmitting.value || !validateForm()) return
  formSubmitting.value = true
  formBanner.value = ''
  try {
    if (modalMode.value === 'add') {
      await createTunnel(toInput())
      toast.success(t('tunnels.createDone', { name: form.value.name.trim() }))
    } else {
      await updateTunnel(editingId.value, toInput())
      toast.success(t('tunnels.updateDone', { name: form.value.name.trim() }))
    }
    modalOpen.value = false
    await load()
  } catch (err) {
    formBanner.value = httpErrMsg(err, t('tunnels.errSubmit'))
  } finally {
    formSubmitting.value = false
  }
}

// ─── 删除 ──────────────────────────────────────────────────────────────────────
const deleteModalOpen = ref(false)
const deleting = ref<SshTunnel | null>(null)
const deleteSubmitting = ref(false)
const deleteBanner = ref('')

function openDeleteModal(tn: SshTunnel): void {
  deleting.value = tn
  deleteBanner.value = ''
  deleteModalOpen.value = true
}
function closeDeleteModal(): void {
  deleteModalOpen.value = false
  deleting.value = null
}
async function confirmDelete(): Promise<void> {
  if (!deleting.value || deleteSubmitting.value) return
  deleteSubmitting.value = true
  deleteBanner.value = ''
  try {
    await deleteTunnel(deleting.value.id)
    toast.success(t('tunnels.deleteDone', { name: deleting.value.name }))
    closeDeleteModal()
    await load()
  } catch (err) {
    deleteBanner.value = httpErrMsg(err, t('tunnels.errDelete'))
  } finally {
    deleteSubmitting.value = false
  }
}

// ─── 日志弹窗 ──────────────────────────────────────────────────────────────────
const logsTarget = ref<SshTunnel | null>(null)
const logsLines = ref<string[]>([])
const logsLoading = ref(false)
const LOG_LINES = 300

async function openLogs(tn: SshTunnel): Promise<void> {
  logsTarget.value = tn
  logsLines.value = []
  logsLoading.value = true
  try {
    logsLines.value = await getTunnelLogs(tn.id, LOG_LINES)
  } catch {
    logsLines.value = []
  } finally {
    logsLoading.value = false
  }
}
</script>

<template>
  <div class="tunnels-view">
    <header class="view-header">
      <div class="view-header__text">
        <h1 class="view-title">{{ t('tunnels.title') }}</h1>
        <p class="view-sub">
          {{ t('tunnels.subtitle') }}
          <span v-if="tunnels.length > 0" class="view-sub__count">
            {{ t('tunnels.countSummary', { total: counts.all, running: counts.running }) }}
          </span>
          <span class="view-sub__count">{{ t('tunnels.autoRefresh', { n: POLL_MS / 1000 }) }}</span>
        </p>
      </div>
      <div class="header-actions">
        <AppButton variant="primary" @click="openAddModal">{{ t('tunnels.addTunnel') }}</AppButton>
        <AppButton variant="default" :loading="loadState === 'loading' && tunnels.length === 0" @click="load">
          {{ t('common.refresh') }}
        </AppButton>
      </div>
    </header>

    <!-- 首屏骨架 -->
    <div v-if="loadState === 'loading' && tunnels.length === 0" class="skeletons" aria-busy="true" :aria-label="t('tunnels.loadingAria')">
      <div v-for="n in 3" :key="n" class="skeleton-panel">
        <SkeletonBlock :height="20" width="40%" />
        <SkeletonBlock :height="44" width="100%" />
      </div>
    </div>

    <ErrorState
      v-else-if="loadState === 'error' && tunnels.length === 0"
      :title="t('tunnels.errTitle')"
      :description="loadError"
      @retry="load"
    />

    <EmptyState
      v-else-if="tunnels.length === 0"
      :title="t('tunnels.emptyTitle')"
      :description="t('tunnels.emptyDesc')"
    />

    <template v-else>
      <!-- 聚合 KPI 条 -->
      <section class="kpi-strip" :aria-label="t('tunnels.kpiAria')">
        <div class="kpi kpi--total">
          <span class="kpi__num">{{ counts.all }}</span>
          <span class="kpi__label">{{ t('tunnels.kpiTotal') }}</span>
        </div>
        <div class="kpi kpi--running">
          <span class="kpi__num">{{ counts.running }}</span>
          <span class="kpi__label">{{ t('tunnels.kpiRunning') }}</span>
        </div>
        <div class="kpi kpi--pending">
          <span class="kpi__num">{{ counts.pending }}</span>
          <span class="kpi__label">{{ t('tunnels.kpiPending') }}</span>
        </div>
        <div class="kpi kpi--off">
          <span class="kpi__num">{{ counts.off }}</span>
          <span class="kpi__label">{{ t('tunnels.kpiOff') }}</span>
        </div>
      </section>

      <!-- 状态筛选 + 搜索 -->
      <div class="controls-row">
        <div class="filter-bar" role="group" :aria-label="t('tunnels.filterAria')">
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
            :placeholder="t('tunnels.searchPlaceholder')"
            :aria-label="t('tunnels.searchAria')"
          />
          <button v-if="searchText" class="search-box__clear" :title="t('tunnels.searchClear')" @click="searchText = ''">✕</button>
        </div>
      </div>

      <EmptyState v-if="visibleTunnels.length === 0" :title="t('tunnels.noMatchTitle')" :description="t('tunnels.noMatchDesc')" />

      <!-- 隧道行列表 -->
      <section v-else class="tunnel-list" :aria-label="t('tunnels.listAria')" :aria-busy="refreshing || undefined">
        <div v-for="tn in visibleTunnels" :key="tn.id" class="tunnel-row">
          <div class="tunnel-row__main">
            <div class="tunnel-id">
              <span class="state-dot" :class="`state-dot--${tunnelStateMeta(tn.status).tone}`" :title="tunnelStateMeta(tn.status).label"></span>
              <div class="tunnel-id__text">
                <span class="tunnel-name">{{ tn.name }}</span>
                <span class="tunnel-target mono">{{ tunnelTargetLabel(tn) }}</span>
              </div>
              <span class="mode-badge" :title="tunnelModeMeta(tn.mode).flag">{{ tunnelModeMeta(tn.mode).label }}</span>
            </div>
            <div class="tunnel-mapping mono">
              <template v-if="tn.mode === 'remote'">
                <span class="mapping-side">{{ t('tunnels.sideRemote') }}</span>
              </template>
              <template v-else>
                <span class="mapping-side">{{ t('tunnels.sideLocal') }}</span>
              </template>
              <span class="mapping-addr">{{ tunnelBindLabel(tn) }}</span>
              <template v-if="tn.mode !== 'dynamic'">
                <span class="mapping-arrow">{{ tn.mode === 'vpn' ? '⇄' : '→' }}</span>
                <span class="mapping-addr">{{ tunnelDestLabel(tn) }}</span>
              </template>
              <span v-else class="mapping-socks">SOCKS5</span>
            </div>
            <div class="tunnel-runtime">
              <span class="state-label" :class="`state-label--${tunnelStateMeta(tn.status).tone}`">{{ tunnelStateMeta(tn.status).label }}</span>
              <span v-if="tn.pid" class="runtime-item mono">pid {{ tn.pid }}</span>
              <span v-if="tn.startedAt" class="runtime-item">{{ tunnelUptimeLabel(tn.startedAt) }}</span>
              <span v-if="tn.restarts > 0" class="runtime-item" :title="t('tunnels.restartsTitle', { n: tn.restarts })">
                {{ t('tunnels.restarts', { n: tn.restarts }) }}
              </span>
            </div>
            <div v-if="tn.lastError" class="tunnel-error" :title="tn.lastError">{{ tn.lastError }}</div>
          </div>
          <div class="tunnel-row__actions">
            <button
              v-for="spec in tunnelActionsFor(tn.status)"
              :key="spec.action"
              class="btn-ghost"
              :disabled="busy.has(`${tn.id}:${spec.action}`)"
              @click="runAction(tn, spec.action)"
            >
              {{ spec.label }}
            </button>
            <button class="btn-ghost" :disabled="testingId === tn.id" @click="runTest(tn)">
              {{ testingId === tn.id ? t('tunnels.testing') : t('tunnels.test') }}
            </button>
            <button class="btn-ghost" @click="openLogs(tn)">{{ t('tunnels.logs') }}</button>
            <button class="btn-ghost" @click="openEditModal(tn)">{{ t('tunnels.edit') }}</button>
            <button class="btn-ghost btn-danger" @click="openDeleteModal(tn)">{{ t('tunnels.delete') }}</button>
          </div>
        </div>
      </section>
    </template>

    <!-- ─── add / edit modal ─────────────────────────────────────────────────── -->
    <div v-if="modalOpen" class="modal-backdrop">
      <div class="modal modal--form" role="dialog" aria-modal="true" aria-labelledby="tunnel-modal-title">
        <h3 id="tunnel-modal-title" class="modal-title">
          {{ modalMode === 'add' ? t('tunnels.addTunnel') : t('tunnels.editTunnel') }}
        </h3>

        <div v-if="formBanner" class="banner banner--error" role="alert">{{ formBanner }}</div>

        <form class="tunnel-form" @submit.prevent="handleFormSubmit">
          <div class="modal-body">
            <label class="field">
              <span class="field-label">{{ t('tunnels.fieldName') }}</span>
              <input v-model="form.name" class="field-input" type="text" placeholder="db-tunnel" autocomplete="off" />
              <span v-if="formErrors.name" class="field-error">{{ formErrors.name }}</span>
            </label>

            <!-- 连接方式:绑定已登记服务器(含跳板链)/ 直连任意主机,二选一 -->
            <div class="field">
              <span class="field-label">{{ t('tunnels.fieldConnect') }}</span>
              <div class="radio-row" role="radiogroup" :aria-label="t('tunnels.fieldConnect')">
                <label class="radio-pill" :class="{ 'radio-pill--active': form.connect === 'server' }">
                  <input v-model="form.connect" type="radio" value="server" />
                  {{ t('tunnels.connectServer') }}
                </label>
                <label class="radio-pill" :class="{ 'radio-pill--active': form.connect === 'direct' }">
                  <input v-model="form.connect" type="radio" value="direct" />
                  {{ t('tunnels.connectDirect') }}
                </label>
              </div>
              <span class="field-hint">{{ t('tunnels.connectHint') }}</span>
            </div>

            <label v-if="form.connect === 'server'" class="field">
              <span class="field-label">{{ t('tunnels.fieldServer') }}</span>
              <select v-model="form.serverId" class="field-input">
                <option value="" disabled>{{ t('tunnels.selectServer') }}</option>
                <option v-for="s in servers" :key="s.id" :value="s.id">{{ s.name }}</option>
              </select>
              <span v-if="formErrors.serverId" class="field-error">{{ formErrors.serverId }}</span>
            </label>

            <template v-else>
              <div class="field-row">
                <label class="field field-grow">
                  <span class="field-label">{{ t('tunnels.fieldHost') }}</span>
                  <input v-model="form.host" class="field-input" type="text" placeholder="203.0.113.10" autocomplete="off" />
                  <span v-if="formErrors.host" class="field-error">{{ formErrors.host }}</span>
                </label>
                <label class="field field-port">
                  <span class="field-label">{{ t('tunnels.fieldPort') }}</span>
                  <input v-model.number="form.port" class="field-input" type="number" min="1" max="65535" />
                  <span v-if="formErrors.port" class="field-error">{{ formErrors.port }}</span>
                </label>
              </div>
              <label class="field">
                <span class="field-label">{{ t('tunnels.fieldUser') }}</span>
                <input v-model="form.user" class="field-input" type="text" placeholder="root" autocomplete="off" />
                <span v-if="formErrors.user" class="field-error">{{ formErrors.user }}</span>
              </label>
              <label class="field">
                <span class="field-label">{{ t('tunnels.fieldCredential') }}</span>
                <select v-model="form.credentialId" class="field-input">
                  <option value="" disabled>{{ t('tunnels.selectCredential') }}</option>
                  <option v-for="c in sshCredentials" :key="c.id" :value="c.id">{{ c.name }}</option>
                </select>
                <span v-if="formErrors.credentialId" class="field-error">{{ formErrors.credentialId }}</span>
                <span class="field-hint">{{ t('tunnels.credentialHint') }}</span>
              </label>
            </template>

            <!-- 模式:本地 local(-L)/ 远程 remote(-R)/ 动态 dynamic(-D)/ VPN vpn(-w) -->
            <div class="field">
              <span class="field-label">{{ t('tunnels.fieldMode') }}</span>
              <div class="radio-row" role="radiogroup" :aria-label="t('tunnels.fieldMode')">
                <label
                  v-for="m in (['local', 'remote', 'dynamic', 'vpn'] as const)"
                  :key="m"
                  class="radio-pill"
                  :class="{ 'radio-pill--active': form.mode === m }"
                >
                  <input v-model="form.mode" type="radio" :value="m" />
                  {{ tunnelModeMeta(m).label }} <span class="mono">{{ tunnelModeMeta(m).flag }}</span>
                </label>
              </div>
              <span class="field-hint">{{ t(`tunnels.modeHint_${form.mode}` as 'tunnels.modeHint_local') }}</span>
            </div>

            <!-- VPN(-w)不推荐警示:引导改用集群自组网 -->
            <div v-if="form.mode === 'vpn'" class="banner banner--warn" role="alert">
              <span>{{ t('tunnels.vpnWarn') }}</span>
            </div>

            <div class="field-row">
              <label class="field field-grow">
                <span class="field-label">{{ bindHostLabel }}</span>
                <input v-model="form.bindHost" class="field-input" type="text" :placeholder="form.mode === 'vpn' ? '10.255.0.2' : '127.0.0.1'" autocomplete="off" />
                <span v-if="formErrors.bindHost" class="field-error">{{ formErrors.bindHost }}</span>
              </label>
              <label class="field field-port">
                <span class="field-label">{{ bindPortLabel }}</span>
                <input v-model.number="form.bindPort" class="field-input" type="number" min="0" :max="form.mode === 'vpn' ? 255 : 65535" placeholder="0" />
                <span v-if="formErrors.bindPort" class="field-error">{{ formErrors.bindPort }}</span>
              </label>
            </div>
            <span class="field-hint field-hint--block">{{ bindHintText }}</span>

            <div v-if="form.mode !== 'dynamic'" class="field-row">
              <label class="field field-grow">
                <span class="field-label">{{ destHostLabel }}</span>
                <input v-model="form.destHost" class="field-input" type="text" :placeholder="form.mode === 'vpn' ? '10.255.0.1' : '10.0.0.5'" autocomplete="off" />
                <span v-if="formErrors.destHost" class="field-error">{{ formErrors.destHost }}</span>
              </label>
              <label class="field field-port">
                <span class="field-label">{{ destPortLabel }}</span>
                <input v-model.number="form.destPort" class="field-input" type="number" min="1" :max="form.mode === 'vpn' ? 255 : 65535" />
                <span v-if="formErrors.destPort" class="field-error">{{ formErrors.destPort }}</span>
              </label>
            </div>
            <span v-if="form.mode === 'local'" class="field-hint field-hint--block">{{ t('tunnels.destHintLocal') }}</span>
            <span v-else-if="form.mode === 'remote'" class="field-hint field-hint--block">{{ t('tunnels.destHintRemote') }}</span>

            <label class="check-field">
              <input v-model="form.enabled" type="checkbox" />
              <span>{{ t('tunnels.fieldEnabled') }}</span>
              <span class="field-hint">{{ t('tunnels.enabledHint') }}</span>
            </label>
          </div>
          <div class="modal-actions">
            <button type="button" class="btn-ghost" :disabled="formSubmitting" @click="closeModal">{{ t('tunnels.cancel') }}</button>
            <button type="submit" class="btn-primary" :disabled="formSubmitting">
              {{ formSubmitting ? t('tunnels.saving') : (modalMode === 'add' ? t('tunnels.create') : t('tunnels.save')) }}
            </button>
          </div>
        </form>
      </div>
    </div>

    <!-- ─── delete confirm modal ─────────────────────────────────────────────── -->
    <div v-if="deleteModalOpen" class="modal-backdrop">
      <div class="modal modal--sm" role="dialog" aria-modal="true" aria-labelledby="tunnel-del-title">
        <h3 id="tunnel-del-title" class="modal-title">{{ t('tunnels.deleteTunnel') }}</h3>
        <div v-if="deleteBanner" class="banner banner--error" role="alert">{{ deleteBanner }}</div>
        <p class="modal-text">
          <i18n-t keypath="tunnels.deleteConfirm" tag="span">
            <template #name><strong>{{ deleting?.name }}</strong></template>
          </i18n-t>
        </p>
        <p class="modal-text modal-text--dim">{{ t('tunnels.deleteHint') }}</p>
        <div class="modal-actions">
          <button type="button" class="btn-ghost" :disabled="deleteSubmitting" @click="closeDeleteModal">{{ t('tunnels.cancel') }}</button>
          <button type="button" class="btn-primary btn-danger" :disabled="deleteSubmitting" @click="confirmDelete">
            {{ deleteSubmitting ? t('tunnels.deleting') : t('tunnels.confirmDelete') }}
          </button>
        </div>
      </div>
    </div>

    <!-- ─── helper logs modal ────────────────────────────────────────────────── -->
    <div v-if="logsTarget" class="modal-backdrop">
      <div class="modal modal--wide" role="dialog" aria-modal="true" aria-labelledby="tunnel-logs-title">
        <div class="logs-modal-head">
          <h3 id="tunnel-logs-title" class="modal-title">{{ t('tunnels.logsTitle', { name: logsTarget.name }) }}</h3>
          <button type="button" class="btn-ghost" @click="logsTarget = null">{{ t('tunnels.close') }}</button>
        </div>
        <p class="logs-modal-sub mono">{{ tunnelTargetLabel(logsTarget) }} · {{ tunnelModeMeta(logsTarget.mode).flag }}</p>
        <div class="logs-body mono" :aria-busy="logsLoading || undefined">
          <div v-if="logsLoading" class="logs-empty">{{ t('tunnels.logsFetching') }}</div>
          <div v-else-if="logsLines.length === 0" class="logs-empty">{{ t('tunnels.logsEmpty') }}</div>
          <pre v-else>{{ logsLines.join('\n') }}</pre>
        </div>
        <p class="logs-foot">{{ t('tunnels.logsFootHint', { n: LOG_LINES }) }}</p>
      </div>
    </div>
  </div>
</template>

<style scoped>
.tunnels-view {
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
.kpi--total::before { background: var(--color-primary); }
.kpi--running::before { background: var(--color-green); }
.kpi--pending::before { background: var(--color-amber, #b88600); }
.kpi--off::before { background: var(--color-faint); }
.kpi__num {
  font-size: var(--text-kpi);
  font-weight: 700;
  line-height: 1;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}
.kpi__label {
  font-size: var(--text-caps);
  letter-spacing: 0.04em;
  text-transform: uppercase;
  color: var(--color-faint);
}

/* 筛选 + 搜索 */
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

/* 隧道行 */
.tunnel-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
  transition: opacity var(--duration-fast) var(--ease-out-expo);
}
.tunnel-list[aria-busy='true'] {
  opacity: 0.7;
}
.tunnel-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 12px;
  padding: 14px 18px;
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
}
.tunnel-row__main {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 18px;
  min-width: 0;
}
.tunnel-id {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 200px;
}
.tunnel-id__text {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.tunnel-name {
  font-weight: 600;
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tunnel-target {
  font-size: var(--text-caption, 0.72rem);
  color: var(--color-faint);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.mode-badge {
  flex-shrink: 0;
  font-size: var(--text-micro);
  font-weight: 600;
  padding: 2px 8px;
  border-radius: 999px;
  color: var(--color-primary);
  background: color-mix(in oklch, var(--color-primary) 12%, transparent);
}
.tunnel-mapping {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: var(--text-label);
  color: var(--color-text);
  min-width: 220px;
}
.mapping-side {
  font-size: var(--text-micro);
  color: var(--color-faint);
  border: 1px solid var(--color-border);
  border-radius: 999px;
  padding: 1px 7px;
  white-space: nowrap;
}
.mapping-arrow {
  color: var(--color-faint);
}
.mapping-socks {
  font-size: var(--text-micro);
  color: var(--color-cyan, #0891b2);
  border: 1px solid currentColor;
  border-radius: 999px;
  padding: 1px 7px;
}
.tunnel-runtime {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: var(--text-label);
  color: var(--color-dim);
  flex-wrap: wrap;
}
.state-label {
  font-weight: 600;
}
.state-label--green { color: var(--color-green); }
.state-label--amber { color: var(--color-amber, #b88600); }
.state-label--red { color: var(--color-red); }
.state-label--faint { color: var(--color-faint); }
.runtime-item {
  color: var(--color-faint);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}
.tunnel-error {
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: var(--text-caption, 0.72rem);
  color: var(--color-red);
}
.state-dot {
  flex-shrink: 0;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--color-faint);
}
.state-dot--green { background: var(--color-green); }
.state-dot--amber { background: var(--color-amber, #b88600); }
.state-dot--red { background: var(--color-red); }
.tunnel-row__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  flex-shrink: 0;
}

/* modal(结构同 SettingsServers) */
.modal-backdrop {
  position: fixed;
  inset: 0;
  background: color-mix(in oklch, var(--color-text) 40%, transparent);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
  z-index: 100;
}
.modal {
  width: 100%;
  max-width: 640px;
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-lg);
  padding: 24px;
  display: flex;
  flex-direction: column;
  gap: 16px;
  box-shadow: var(--shadow-lg, 0 24px 60px rgba(0, 0, 0, 0.24));
}
.modal--wide {
  max-width: min(1000px, 92vw);
}
.modal--sm {
  max-width: 520px;
}
.modal--form {
  max-height: calc(100vh - 48px);
  max-height: min(90vh, calc(100dvh - 48px));
}
.tunnel-form {
  display: flex;
  flex-direction: column;
  gap: 16px;
  min-height: 0;
}
.modal-body {
  min-height: 0;
  overflow-y: auto;
  overscroll-behavior: contain;
}
.modal-body > :last-child {
  margin-bottom: 0;
}
.tunnel-form .modal-actions {
  border-top: 1px solid var(--color-border);
  padding-top: 12px;
}
.modal-title {
  font-size: var(--text-heading);
  font-weight: 600;
  color: var(--color-text);
}
.modal-text {
  font-size: var(--text-label);
  color: var(--color-dim);
  line-height: 1.5;
}
.modal-text--dim {
  color: var(--color-faint);
}
.modal-actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  margin-top: 4px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-bottom: 14px;
}
.field-row {
  display: flex;
  gap: 12px;
}
.field-grow {
  flex: 1;
}
.field-port {
  width: 110px;
}
.field-label {
  font-size: var(--text-label);
  font-weight: 500;
  color: var(--color-dim);
}
.field-input {
  padding: 8px 12px;
  font-size: var(--text-body);
  color: var(--color-text);
  background: var(--color-bg, var(--color-surface));
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  transition: border-color var(--duration-fast);
}
.field-input:focus {
  outline: none;
  border-color: var(--color-primary);
}
.field-error {
  font-size: var(--text-label);
  color: var(--color-danger, #d4503e);
}
.field-hint {
  font-size: var(--text-caption, 0.7rem);
  color: var(--color-faint);
  line-height: 1.4;
}
.field-hint--block {
  display: block;
  margin: -8px 0 10px;
}

/* 单选胶囊(连接方式/模式) */
.radio-row {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.radio-pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 7px 14px;
  font-size: var(--text-label);
  font-weight: 600;
  color: var(--color-dim);
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: 999px;
  cursor: pointer;
  transition: all var(--duration-fast) var(--ease-out-expo);
}
.radio-pill input {
  position: absolute;
  opacity: 0;
  pointer-events: none;
}
.radio-pill:hover {
  border-color: var(--color-border-strong);
  color: var(--color-text);
}
.radio-pill--active {
  color: var(--color-primary);
  border-color: var(--color-primary);
  background: color-mix(in oklch, var(--color-primary) 10%, transparent);
}
.check-field {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: var(--text-label);
  color: var(--color-text);
  margin-bottom: 0;
}

/* 按钮 */
.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 8px 14px;
  font-size: var(--text-label);
  font-weight: 600;
  color: #fff;
  background: var(--color-primary);
  border: none;
  border-radius: var(--radius-md);
  cursor: pointer;
  white-space: nowrap;
}
.btn-primary:hover:not(:disabled) {
  filter: brightness(1.08);
}
.btn-primary:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.btn-ghost {
  padding: 6px 12px;
  font-size: var(--text-label);
  font-weight: 500;
  color: var(--color-dim);
  background: transparent;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: color var(--duration-fast), border-color var(--duration-fast), background var(--duration-fast);
}
.btn-ghost:hover:not(:disabled) {
  color: var(--color-text);
  border-color: var(--color-dim);
}
.btn-ghost:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.btn-danger {
  color: var(--color-danger, #d4503e);
}
.btn-danger:hover:not(:disabled) {
  border-color: var(--color-danger, #d4503e);
  color: var(--color-danger, #d4503e);
}
.btn-primary.btn-danger {
  color: #fff;
  background: var(--color-danger, #d4503e);
}
.banner {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  font-size: var(--text-label);
  border-radius: var(--radius-md);
}
.banner--error {
  color: var(--color-danger, #d4503e);
  background: color-mix(in oklch, var(--color-danger, #d4503e) 10%, transparent);
}
.banner--warn {
  color: var(--color-warn, #b88600);
  background: color-mix(in oklch, var(--color-warn, #b88600) 12%, transparent);
}

/* 日志弹窗 */
.logs-modal-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}
.logs-modal-sub {
  font-size: 0.78rem;
  color: var(--color-dim);
  margin-top: -8px;
}
.logs-body {
  background: var(--color-inset, #0b1220);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  padding: 12px 14px;
  max-height: 55vh;
  overflow: auto;
  font-size: var(--text-caption, 0.72rem);
  line-height: 1.55;
  color: var(--color-text);
}
.logs-body pre {
  margin: 0;
  white-space: pre-wrap;
  word-break: break-all;
  font-family: inherit;
}
.logs-empty {
  color: var(--color-faint);
}
.logs-foot {
  font-size: var(--text-caption, 0.7rem);
  color: var(--color-faint);
}

.mono {
  font-family: var(--font-mono, ui-monospace, SFMono-Regular, Menlo, monospace);
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
