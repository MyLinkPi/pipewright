<!--
  Security.vue — 安全中心(Trivy + Nuclei 统一安全面板,仅管理员)。

  五区:
    1. KPI 概览(全员严重度合计 / 覆盖台数 / 最近扫描);
    2. 定时扫描规则(开关 + 预设/自定义 cron + 扫描类型;后端 internal/cron 校验);
    3. 扫描器(trivy-server 列表:测试/删除 + 手动登记外部实例;空态引导去应用市场部署);
    4. 服务器表(每机最新 Trivy/Nuclei 结果徽标 + 单机扫描 + 查看明细);
    5. 扫描历史(含 running 轮询刷新)。
  漏洞明细:弹窗抽屉(严重度/来源/关键词过滤 + 分页,数据 GET /scans/{id}/findings)。
  报告:「导出报告」下载自包含 HTML(浏览器打印即 PDF)。
  轮询:页面挂载期间每 8s 刷新概览+历史(有 running 任务时结果自然前滚)。
-->
<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  getOverview, getSchedule, saveSchedule,
  listScanners, createScanner, deleteScanner, testScanner, testScannerDirect,
  getScannerLib, updateScannerLib, type ScannerLibInfo,
  startScan, startFleetScan, listRuns, getFindings, getRunLogs, downloadSecurityReport, explainFinding,
  getWafOverview, type WafInstanceStatus,
  type SecurityOverview, type Schedule, type ScannerInstance, type SecRun,
  type FindingsPage, type ScanType, type Finding, type RunLogView,
} from '../api/security'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import AppButton from '../components/ui/AppButton.vue'
import AppSelect from '../components/ui/AppSelect.vue'
import SevChips from '../components/security/SevChips.vue'
import AiOpsMarkdown from '../components/ops/AiOpsMarkdown.vue'
import ErrorState from '../components/ui/ErrorState.vue'
import SkeletonBlock from '../components/ui/SkeletonBlock.vue'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

type LoadState = 'idle' | 'loading' | 'error'

const errMsg = (e: unknown, fallback: string): string =>
  e instanceof HttpError ? (e.apiError?.message ?? fallback) : fallback

// ─── 概览 + 历史(轮询)──────────────────────────────────────────────────────
const loadState = ref<LoadState>('idle')
const overview = ref<SecurityOverview | null>(null)
const runs = ref<SecRun[]>([])
const wafItems = ref<WafInstanceStatus[]>([])
const POLL_MS = 8000
let pollTimer: ReturnType<typeof setInterval> | null = null

async function loadAll(): Promise<void> {
  try {
    const [ov, hist, waf] = await Promise.all([getOverview(), listRuns({ limit: 20 }), getWafOverview().catch(() => ({ items: [] as WafInstanceStatus[] }))])
    overview.value = ov
    runs.value = hist.items ?? []
    wafItems.value = waf.items ?? []
    loadState.value = loadState.value === 'error' ? 'idle' : loadState.value
  } catch (e) {
    if (loadState.value !== 'loading') return // 已有数据时静默
    loadState.value = 'error'
  }
}

async function refresh(): Promise<void> {
  loadState.value = 'loading'
  await loadAll()
  if (loadState.value === 'loading') loadState.value = 'idle'
  await Promise.all([loadSchedule(), loadScanners()])
}

const hasRunning = computed(() =>
  runs.value.some((r) => r.status === 'running') ||
  (overview.value?.servers ?? []).some((s) => s.trivy?.status === 'running' || s.nuclei?.status === 'running'),
)

// ─── KPI ────────────────────────────────────────────────────────────────────
const totals = computed(() => overview.value?.totals ?? { critical: 0, high: 0, medium: 0, low: 0, info: 0, unknown: 0, total: 0 })

// ─── 定时规则 ────────────────────────────────────────────────────────────────
const schedule = ref<Schedule | null>(null)
const schedLoaded = ref(false)
const schedSaving = ref(false)
const schedForm = reactive({ enabled: false, preset: 'daily-3', cron: '', trivy: true, nuclei: false })

const PRESET_EXPRS: Record<string, string> = {
  'daily-3': '0 3 * * *',
  'daily-6': '0 6 * * *',
  'every-12h': '0 */12 * * *',
  'weekly-sun-3': '0 3 * * 0',
  custom: '',
}
const presetOptions = computed(() => [
  { value: 'daily-3', label: t('securityCenter.presetDaily3') },
  { value: 'daily-6', label: t('securityCenter.presetDaily6') },
  { value: 'every-12h', label: t('securityCenter.preset12h') },
  { value: 'weekly-sun-3', label: t('securityCenter.presetWeekly') },
  { value: 'custom', label: t('securityCenter.presetCustom') },
])

function applyScheduleToForm(s: Schedule): void {
  schedForm.enabled = s.enabled
  schedForm.trivy = s.scanTypes.includes('trivy')
  schedForm.nuclei = s.scanTypes.includes('nuclei')
  const entry = Object.entries(PRESET_EXPRS).find(([, expr]) => expr === s.expression)
  schedForm.preset = entry ? entry[0] : 'custom'
  schedForm.cron = entry ? s.expression : s.expression
}

async function loadSchedule(): Promise<void> {
  try {
    const s = await getSchedule()
    schedule.value = s
    if (schedLoaded.value) return // 已预填过:轮询/刷新不覆盖用户正在编辑的表单
    applyScheduleToForm(s)
    schedLoaded.value = true
  } catch {
    /* 定时规则加载失败不打断页面;保存时报错 */
  }
}

async function saveSched(): Promise<void> {
  const expression = schedForm.preset === 'custom' ? schedForm.cron.trim() : PRESET_EXPRS[schedForm.preset]
  const types: ScanType[] = [schedForm.trivy && 'trivy', schedForm.nuclei && 'nuclei'].filter(Boolean) as ScanType[]
  if (!types.length) {
    toast.error(t('securityCenter.schedNeedType'))
    return
  }
  schedSaving.value = true
  try {
    schedule.value = await saveSchedule({ enabled: schedForm.enabled, expression, scanTypes: types })
    applyScheduleToForm(schedule.value)
    toast.success(t('securityCenter.schedSaved'))
  } catch (e) {
    toast.error(t('securityCenter.errSave'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
  } finally {
    schedSaving.value = false
  }
}

// ─── 扫描器 ─────────────────────────────────────────────────────────────────
const scanners = ref<ScannerInstance[]>([])
const scannerForm = reactive({ name: '', host: '', port: 4954, token: '' })
const scannerSubmitting = ref(false)
const scannerTesting = ref(false)
const testingId = ref('')
const showAddScanner = ref(false)

// ── 规则库状态(漏洞库/特征模板更新时间 + 手动更新)──
const libInfo = ref<Record<string, ScannerLibInfo>>({})
const libLoading = ref('')
const libUpdating = ref('')

async function loadLibInfo(sc: ScannerInstance): Promise<void> {
  if (sc.kind === 'safeline') return // 雷池控制台自管理,不查
  libLoading.value = sc.id
  try {
    libInfo.value[sc.id] = await getScannerLib(sc.id)
  } catch {
    /* 查询失败不打扰:单元格显示 — */
  } finally {
    libLoading.value = ''
  }
}

async function libUpdate(sc: ScannerInstance): Promise<void> {
  libUpdating.value = sc.id
  try {
    const info = await updateScannerLib(sc.id)
    libInfo.value[sc.id] = info
    toast.success(info.note || t('securityCenter.libNucleiUpdatedNote'))
    void loadLibInfo(sc) // 更新后刷新时间
  } catch (e) {
    toast.error(t('securityCenter.libInfoFail'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
  } finally {
    libUpdating.value = ''
  }
}

async function loadScanners(): Promise<void> {
  try {
    const res = await listScanners()
    scanners.value = res.items ?? []
    for (const sc of scanners.value) void loadLibInfo(sc)
  } catch {
    /* 静默:概览错误态已兜底 */
  }
}

async function submitScanner(): Promise<void> {
  scannerSubmitting.value = true
  try {
    await createScanner({
      name: scannerForm.name.trim(), host: scannerForm.host.trim(),
      port: Number(scannerForm.port) || 4954, token: scannerForm.token || undefined,
    })
    toast.success(t('securityCenter.scannerSaved'))
    showAddScanner.value = false
    scannerForm.name = ''; scannerForm.host = ''; scannerForm.port = 4954; scannerForm.token = ''
    await loadScanners()
  } catch (e) {
    toast.error(t('securityCenter.errSave'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
  } finally {
    scannerSubmitting.value = false
  }
}

async function testDirect(): Promise<void> {
  scannerTesting.value = true
  try {
    const res = await testScannerDirect({
      host: scannerForm.host.trim(), port: Number(scannerForm.port) || 4954, token: scannerForm.token || undefined,
    })
    if (res.ok) toast.success(res.detail)
    else toast.warn(res.detail || t('securityCenter.scannerTestFail'))
  } catch (e) {
    toast.error(t('securityCenter.scannerTestFail'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
  } finally {
    scannerTesting.value = false
  }
}

async function testOne(id: string): Promise<void> {
  testingId.value = id
  try {
    const res = await testScanner(id)
    if (res.ok) toast.success(res.detail)
    else toast.warn(res.detail || t('securityCenter.scannerTestFail'))
  } catch (e) {
    toast.error(t('securityCenter.scannerTestFail'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
  } finally {
    testingId.value = ''
  }
}

async function removeScanner(sc: ScannerInstance): Promise<void> {
  const ok = await confirm.open({
    title: t('securityCenter.scannerDeleteTitle'),
    body: t('securityCenter.scannerDeleteBody', { name: sc.name }),
    confirmLabel: t('securityCenter.scannerDelete'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteScanner(sc.id)
    toast.success(t('securityCenter.scannerDeleted'))
    await loadScanners()
  } catch (e) {
    toast.error(t('securityCenter.errDelete'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
  }
}

// ─── 发起扫描 ────────────────────────────────────────────────────────────────
const scanningServer = ref('')
const fleetScanning = ref(false)

async function scanServer(serverId: string, scanType: ScanType): Promise<void> {
  scanningServer.value = `${serverId}:${scanType}`
  try {
    await startScan({ serverId, scanType })
    toast.success(t('securityCenter.scanStarted'))
    await loadAll()
  } catch (e) {
    toast.error(t('securityCenter.scanStartFail'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
  } finally {
    scanningServer.value = ''
  }
}

async function scanFleet(): Promise<void> {
  const types: ScanType[] = schedForm.trivy || schedForm.nuclei ? [schedForm.trivy && 'trivy', schedForm.nuclei && 'nuclei'].filter(Boolean) as ScanType[] : ['trivy']
  fleetScanning.value = true
  try {
    const res = await startFleetScan(types)
    toast.success(t('securityCenter.scanFleetStarted', { n: res.items?.length ?? 0 }))
    await loadAll()
  } catch (e) {
    toast.error(t('securityCenter.scanStartFail'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
  } finally {
    fleetScanning.value = false
  }
}

// ─── 漏洞明细弹窗 ────────────────────────────────────────────────────────────
const findingsRun = ref<SecRun | null>(null)
const findings = ref<FindingsPage | null>(null)
const findingsLoading = ref(false)
const findingsFilter = reactive({ severity: '', source: '', q: '', offset: 0, limit: 50 })

const severityOptions = computed(() => [
  { value: '', label: t('securityCenter.filterAll') },
  { value: 'critical', label: t('securityCenter.sevCritical') },
  { value: 'high', label: t('securityCenter.sevHigh') },
  { value: 'medium', label: t('securityCenter.sevMedium') },
  { value: 'low', label: t('securityCenter.sevLow') },
  { value: 'info', label: t('securityCenter.sevInfo') },
])
const sourceOptions = computed(() => [
  { value: '', label: t('securityCenter.filterAll') },
  { value: 'host', label: t('securityCenter.srcHost') },
  { value: 'image', label: t('securityCenter.srcImage') },
  { value: 'nuclei', label: t('securityCenter.srcNuclei') },
])

async function loadFindings(): Promise<void> {
  if (!findingsRun.value) return
  findingsLoading.value = true
  try {
    findings.value = await getFindings(findingsRun.value.id, { ...findingsFilter })
  } catch (e) {
    toast.error(t('securityCenter.errLoad'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
    findings.value = null
  } finally {
    findingsLoading.value = false
  }
}

// openFindings 接受 SecRun(历史行)或概览行(runId)的最小交集:{id, serverName, scanType}。
function openFindings(run: { id: string; serverName: string; scanType: ScanType }): void {
  findingsRun.value = { id: run.id, serverId: '', serverName: run.serverName, scanType: run.scanType, status: 'done', summary: { critical: 0, high: 0, medium: 0, low: 0, info: 0, unknown: 0, total: 0 }, startedAt: '' }
  findingsFilter.severity = ''; findingsFilter.source = ''; findingsFilter.q = ''; findingsFilter.offset = 0
  void loadFindings()
}

function findingsFilterChanged(): void {
  findingsFilter.offset = 0
  void loadFindings()
}

function findingsPageMove(delta: number): void {
  findingsFilter.offset = Math.max(0, findingsFilter.offset + delta)
  void loadFindings()
}

// ─── 执行日志弹窗(各步骤原始 stdout/stderr,诊断「扫不出结果」类问题)──────────
const logsRun = ref<SecRun | null>(null)
const logsView = ref<RunLogView | null>(null)
const logsLoading = ref(false)

async function loadLogs(): Promise<void> {
  if (!logsRun.value) return
  logsLoading.value = true
  try {
    logsView.value = await getRunLogs(logsRun.value.id)
  } catch (e) {
    toast.error(t('securityCenter.errLoad'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
    logsView.value = null
  } finally {
    logsLoading.value = false
  }
}

// openLogs 接受 SecRun(历史行)或概览行(runId)的最小交集;失败态 run 也带日志。
function openLogs(run: { id: string; serverName: string; scanType: ScanType }): void {
  logsRun.value = { id: run.id, serverId: '', serverName: run.serverName, scanType: run.scanType, status: 'done', summary: { critical: 0, high: 0, medium: 0, low: 0, info: 0, unknown: 0, total: 0 }, startedAt: '' }
  logsView.value = null
  void loadLogs()
}

// ─── AI 解读(会话内缓存:同 run 同 finding 不重复请求)────────────────────────
const explainCache = new Map<string, string>()
const explainBusy = ref('')
const explainShown = ref('')

const explainKey = (f: Finding): string => `${f.id}|${f.target}|${f.installedVersion ?? ''}`

async function askExplain(f: Finding): Promise<void> {
  const key = explainKey(f)
  if (explainShown.value === key) {
    explainShown.value = '' // 再点收起
    return
  }
  explainShown.value = key
  if (explainCache.has(key)) return
  explainBusy.value = key
  try {
    const res = await explainFinding({
      scanType: findingsRun.value?.scanType ?? 'trivy',
      source: f.source, severity: f.severity, id: f.id,
      title: f.title, target: f.target,
      installedVersion: f.installedVersion, fixedVersion: f.fixedVersion, url: f.url,
      serverName: findingsRun.value?.serverName ?? '',
    })
    explainCache.set(key, res.text)
  } catch (e) {
    explainShown.value = ''
    toast.error(t('securityCenter.aiExplainFailed'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
  } finally {
    explainBusy.value = ''
  }
}

// ─── 报告导出(范围弹窗:全量 / 自选 + 搜索筛选)───────────────────────────────
const exporting = ref(false)
const showExportModal = ref(false)
const exportScope = ref<'all' | 'custom'>('all')
const exportSearch = ref('')
const exportSelected = ref(new Set<string>())

// 按名称/地址筛选候选列表(不区分大小写)。
const exportCandidates = computed(() => {
  const q = exportSearch.value.trim().toLowerCase()
  return (overview.value?.servers ?? []).filter(
    (srv) => !q || srv.serverName.toLowerCase().includes(q) || srv.host.toLowerCase().includes(q),
  )
})

function toggleExportServer(id: string): void {
  const next = new Set(exportSelected.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  exportSelected.value = next
}

function selectAllCandidates(): void {
  const next = new Set(exportSelected.value)
  for (const srv of exportCandidates.value) next.add(srv.serverId)
  exportSelected.value = next
}

function clearExportSelection(): void {
  exportSelected.value = new Set()
}

function openExportModal(): void {
  exportScope.value = 'all'
  exportSearch.value = ''
  exportSelected.value = new Set()
  showExportModal.value = true
}

async function exportReport(): Promise<void> {
  if (exportScope.value === 'custom' && exportSelected.value.size === 0) {
    toast.warn(t('securityCenter.exportNeedSelect'))
    return
  }
  exporting.value = true
  try {
    const ids = exportScope.value === 'custom' ? [...exportSelected.value] : undefined
    const name = await downloadSecurityReport(ids)
    toast.success(t('securityCenter.reportDownloaded', { name }))
    showExportModal.value = false
  } catch (e) {
    toast.error(t('securityCenter.errReport'), { detail: errMsg(e, t('securityCenter.errNetwork')) })
  } finally {
    exporting.value = false
  }
}

// ─── 展示辅助 ────────────────────────────────────────────────────────────────
const fmtTime = (iso?: string): string => (iso ? new Date(iso).toLocaleString() : '—')

const triggerLabel = (r: SecRun): string =>
  r.trigger === 'scheduled' ? t('securityCenter.triggerScheduled') : r.trigger === 'fleet' ? t('securityCenter.triggerFleet') : t('securityCenter.triggerManual')

const typeLabel = (ty: string): string => (ty === 'nuclei' ? 'Nuclei' : 'Trivy')

onMounted(() => {
  void refresh()
  pollTimer = setInterval(() => { void loadAll() }, POLL_MS)
})
onUnmounted(() => { if (pollTimer) clearInterval(pollTimer) })
</script>

<template>
  <header class="view-header">
    <div class="view-header__text">
      <h1 class="view-title">{{ t('securityCenter.title') }}</h1>
      <p class="view-sub">{{ t('securityCenter.subtitle') }}</p>
    </div>
    <div class="view-header__actions">
      <AppButton variant="default" :loading="false" @click="refresh()">{{ t('securityCenter.refresh') }}</AppButton>
      <AppButton variant="default" :loading="fleetScanning" @click="scanFleet()">{{ t('securityCenter.fleetScan') }}</AppButton>
      <AppButton variant="primary" @click="openExportModal()">{{ t('securityCenter.exportReport') }}</AppButton>
    </div>
  </header>

  <ErrorState v-if="loadState === 'error'" :message="t('securityCenter.errLoad')" @retry="refresh()" />
  <SkeletonBlock v-else-if="loadState === 'loading' && !overview" />

  <template v-else>
    <!-- KPI 概览 -->
    <section class="panel" aria-labelledby="sec-kpi-heading">
      <h2 id="sec-kpi-heading" class="panel-title">{{ t('securityCenter.overviewTitle') }}</h2>
      <div class="kpis">
        <div class="kpi kpi--critical"><b>{{ totals.critical }}</b><span>{{ t('securityCenter.kpiCritical') }}</span></div>
        <div class="kpi kpi--high"><b>{{ totals.high }}</b><span>{{ t('securityCenter.kpiHigh') }}</span></div>
        <div class="kpi kpi--medium"><b>{{ totals.medium }}</b><span>{{ t('securityCenter.kpiMedium') }}</span></div>
        <div class="kpi kpi--low"><b>{{ totals.low }}</b><span>{{ t('securityCenter.kpiLow') }}</span></div>
        <div class="kpi"><b>{{ overview?.scanned ?? 0 }}/{{ overview?.serverTotal ?? 0 }}</b><span>{{ t('securityCenter.kpiCoverage') }}</span></div>
        <div class="kpi"><b class="kpi__time">{{ overview?.lastScanAt ? fmtTime(overview.lastScanAt) : t('securityCenter.never') }}</b><span>{{ t('securityCenter.kpiLastScan') }}</span></div>
      </div>
      <p v-if="hasRunning" class="hint">{{ t('securityCenter.runningHint') }}</p>
    </section>

    <!-- WAF 防护态势(SafeLine) -->
    <section v-if="wafItems.length" class="panel" aria-labelledby="sec-waf-heading">
      <h2 id="sec-waf-heading" class="panel-title">{{ t('securityCenter.wafTitle') }}</h2>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th scope="col">{{ t('securityCenter.colServer') }}</th>
              <th scope="col">{{ t('securityCenter.colStatus') }}</th>
              <th scope="col">{{ t('securityCenter.wafBlockedToday') }}</th>
              <th scope="col">{{ t('securityCenter.wafBlocked7d') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="w in wafItems" :key="w.instanceId">
              <td>
                <div class="srv-name">{{ w.name }}</div>
                <div class="hint">{{ w.endpoint }}</div>
              </td>
              <td>
                <template v-if="!w.tokenConfigured">
                  <span class="badge badge--failed">{{ t('securityCenter.wafNoToken') }}</span>
                  <div class="hint">{{ t('securityCenter.wafNoTokenHint') }}</div>
                </template>
                <template v-else-if="!w.ok">
                  <span class="badge badge--failed">{{ t('securityCenter.wafUnreachable') }}</span>
                  <div class="hint">{{ w.detail }}</div>
                </template>
                <span v-else class="badge badge--ok">{{ t('securityCenter.wafOnline') }}</span>
              </td>
              <td><template v-if="w.stats">{{ w.stats.blockedToday }}</template><span v-else class="hint">{{ t('securityCenter.wafStatsNA') }}</span></td>
              <td><template v-if="w.stats">{{ w.stats.blocked7d }}</template><span v-else class="hint">{{ t('securityCenter.wafStatsNA') }}</span></td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- 定时扫描 -->
    <section class="panel" aria-labelledby="sec-sched-heading">
      <h2 id="sec-sched-heading" class="panel-title">{{ t('securityCenter.schedTitle') }}</h2>
      <p class="hint">{{ t('securityCenter.schedHint') }}</p>
      <div class="sched-grid">
        <label class="field field--inline">
          <input v-model="schedForm.enabled" type="checkbox" />
          <span>{{ t('securityCenter.schedEnabled') }}</span>
        </label>
        <label class="field">
          <span>{{ t('securityCenter.schedPreset') }}</span>
          <AppSelect v-model="schedForm.preset" :options="presetOptions" />
        </label>
        <label v-if="schedForm.preset === 'custom'" class="field">
          <span>{{ t('securityCenter.schedCronLabel') }}</span>
          <input v-model="schedForm.cron" type="text" placeholder="0 3 * * *" />
        </label>
        <div class="field field--inline">
          <label class="field field--inline"><input v-model="schedForm.trivy" type="checkbox" /><span>Trivy</span></label>
          <label class="field field--inline"><input v-model="schedForm.nuclei" type="checkbox" /><span>Nuclei</span></label>
        </div>
        <div class="field field--actions">
          <AppButton variant="primary" :loading="schedSaving" @click="saveSched()">{{ t('securityCenter.schedSave') }}</AppButton>
          <span v-if="schedule?.lastRunAt" class="hint">{{ t('securityCenter.schedLastRun') }}:{{ fmtTime(schedule.lastRunAt) }}</span>
        </div>
      </div>
    </section>

    <!-- 扫描器 -->
    <section class="panel" aria-labelledby="sec-scanners-heading">
      <div class="panel-head">
        <h2 id="sec-scanners-heading" class="panel-title">{{ t('securityCenter.scannersTitle') }}</h2>
        <AppButton variant="default" @click="showAddScanner = !showAddScanner">{{ t('securityCenter.addScanner') }}</AppButton>
      </div>
      <p class="hint">{{ t('securityCenter.scannersHint') }}</p>

      <form v-if="showAddScanner" class="add-form" @submit.prevent="submitScanner">
        <label class="field">
          <span>{{ t('securityCenter.scannerName') }}</span>
          <input v-model="scannerForm.name" type="text" required />
        </label>
        <label class="field">
          <span>{{ t('securityCenter.scannerHost') }}</span>
          <input v-model="scannerForm.host" type="text" placeholder="10.0.0.8" required />
        </label>
        <label class="field field--num">
          <span>{{ t('securityCenter.scannerPort') }}</span>
          <input v-model="scannerForm.port" type="number" min="1" max="65535" required />
        </label>
        <label class="field">
          <span>{{ t('securityCenter.scannerToken') }}</span>
          <input v-model="scannerForm.token" type="password" autocomplete="new-password" :placeholder="t('securityCenter.scannerTokenOptional')" />
        </label>
        <div class="field field--actions">
          <AppButton variant="default" :loading="scannerTesting" @click="testDirect()">{{ t('securityCenter.testDirect') }}</AppButton>
          <AppButton variant="primary" type="submit" :loading="scannerSubmitting">{{ t('securityCenter.scannerSave') }}</AppButton>
        </div>
      </form>

      <div v-if="!scanners.length" class="empty-hint">
        <p>{{ t('securityCenter.scannersEmpty') }}</p>
        <p class="hint">{{ t('securityCenter.scannersEmptyDesc') }}</p>
      </div>
      <div v-else class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th scope="col">{{ t('securityCenter.scannerName') }}</th>
              <th scope="col">{{ t('securityCenter.scannerEndpoint') }}</th>
              <th scope="col">{{ t('securityCenter.scannerTokenLabel') }}</th>
              <th scope="col">{{ t('securityCenter.scannerSource') }}</th>
              <th scope="col">{{ t('securityCenter.colLib') }}</th>
              <th scope="col">{{ t('securityCenter.colActions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="sc in scanners" :key="sc.id">
              <td>{{ sc.name }}</td>
              <td>{{ sc.host }}{{ sc.port ? ':' + sc.port : '' }}</td>
              <td>{{ sc.tokenConfigured ? t('securityCenter.scannerTokenConfigured') : t('securityCenter.scannerTokenNone') }}</td>
              <td>{{ sc.source === 'appstore' ? t('securityCenter.scannerSourceAppstore') : t('securityCenter.scannerSourceManual') }}</td>
              <td>
                <template v-if="sc.kind === 'safeline'">
                  <span class="hint">{{ t('securityCenter.libConsoleManaged') }}</span>
                </template>
                <template v-else>
                  <div class="hint" :title="libInfo[sc.id]?.note">{{ libInfo[sc.id]?.updatedAt ? fmtTime(libInfo[sc.id]!.updatedAt) : '—' }}</div>
                </template>
              </td>
              <td class="row-actions">
                <AppButton v-if="sc.kind !== 'safeline'" variant="default" :loading="libUpdating === sc.id" @click="libUpdate(sc)">{{ t('securityCenter.libUpdateBtn') }}</AppButton>
                <AppButton variant="default" :loading="testingId === sc.id" @click="testOne(sc.id)">{{ t('securityCenter.scannerTest') }}</AppButton>
                <AppButton variant="danger" @click="removeScanner(sc)">{{ t('securityCenter.scannerDelete') }}</AppButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- 服务器 -->
    <section class="panel" aria-labelledby="sec-servers-heading">
      <h2 id="sec-servers-heading" class="panel-title">{{ t('securityCenter.serversTitle') }}</h2>
      <div v-if="!overview?.servers?.length" class="empty-hint">{{ t('securityCenter.serversEmpty') }}</div>
      <div v-else class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th scope="col">{{ t('securityCenter.colServer') }}</th>
              <th scope="col">Trivy</th>
              <th scope="col">Nuclei</th>
              <th scope="col">{{ t('securityCenter.colActions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="srv in overview.servers" :key="srv.serverId">
              <td>
                <div class="srv-name">{{ srv.serverName }}</div>
                <div class="hint">{{ srv.host }}</div>
              </td>
              <td>
                <template v-if="srv.trivy">
                  <div v-if="srv.trivy.status === 'running'" class="cell-line"><span class="badge badge--running badge--pulse">{{ t('securityCenter.statusRunning') }}</span></div>
                  <template v-else-if="srv.trivy.status === 'failed'">
                    <div class="cell-line"><span class="badge badge--failed">{{ t('securityCenter.statusFailed') }}</span></div>
                    <div v-if="srv.trivy.error" class="err-line" :title="srv.trivy.error">{{ srv.trivy.error }}</div>
                    <div class="cell-line"><AppButton variant="default" @click="openLogs({ id: srv.trivy!.runId, serverName: srv.serverName, scanType: srv.trivy!.scanType })">{{ t('securityCenter.viewLogs') }}</AppButton></div>
                  </template>
                  <template v-else>
                    <div class="cell-line"><SevChips :summary="srv.trivy.summary" /></div>
                    <div class="cell-line">
                      <AppButton variant="default" @click="openFindings({ id: srv.trivy!.runId, serverName: srv.serverName, scanType: srv.trivy!.scanType })">{{ t('securityCenter.viewFindings') }}</AppButton>
                      <AppButton variant="default" @click="openLogs({ id: srv.trivy!.runId, serverName: srv.serverName, scanType: srv.trivy!.scanType })">{{ t('securityCenter.viewLogs') }}</AppButton>
                    </div>
                  </template>
                </template>
                <span v-else class="hint">{{ t('securityCenter.neverScanned') }}</span>
              </td>
              <td>
                <template v-if="srv.nuclei">
                  <div v-if="srv.nuclei.status === 'running'" class="cell-line"><span class="badge badge--running badge--pulse">{{ t('securityCenter.statusRunning') }}</span></div>
                  <template v-else-if="srv.nuclei.status === 'failed'">
                    <div class="cell-line"><span class="badge badge--failed">{{ t('securityCenter.statusFailed') }}</span></div>
                    <div v-if="srv.nuclei.error" class="err-line" :title="srv.nuclei.error">{{ srv.nuclei.error }}</div>
                    <div class="cell-line"><AppButton variant="default" @click="openLogs({ id: srv.nuclei!.runId, serverName: srv.serverName, scanType: srv.nuclei!.scanType })">{{ t('securityCenter.viewLogs') }}</AppButton></div>
                  </template>
                  <template v-else>
                    <div class="cell-line"><SevChips :summary="srv.nuclei.summary" /></div>
                    <div class="cell-line">
                      <AppButton variant="default" @click="openFindings({ id: srv.nuclei!.runId, serverName: srv.serverName, scanType: srv.nuclei!.scanType })">{{ t('securityCenter.viewFindings') }}</AppButton>
                      <AppButton variant="default" @click="openLogs({ id: srv.nuclei!.runId, serverName: srv.serverName, scanType: srv.nuclei!.scanType })">{{ t('securityCenter.viewLogs') }}</AppButton>
                    </div>
                  </template>
                </template>
                <span v-else class="hint">{{ t('securityCenter.neverScanned') }}</span>
              </td>
              <td class="row-actions">
                <AppButton variant="default" :loading="scanningServer === `${srv.serverId}:trivy`" @click="scanServer(srv.serverId, 'trivy')">{{ t('securityCenter.scanTrivy') }}</AppButton>
                <AppButton variant="default" :loading="scanningServer === `${srv.serverId}:nuclei`" @click="scanServer(srv.serverId, 'nuclei')">{{ t('securityCenter.scanNuclei') }}</AppButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- 扫描历史 -->
    <section class="panel" aria-labelledby="sec-history-heading">
      <h2 id="sec-history-heading" class="panel-title">{{ t('securityCenter.historyTitle') }}</h2>
      <div v-if="!runs.length" class="empty-hint">{{ t('securityCenter.historyEmpty') }}</div>
      <div v-else class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th scope="col">{{ t('securityCenter.colTime') }}</th>
              <th scope="col">{{ t('securityCenter.colServer') }}</th>
              <th scope="col">{{ t('securityCenter.colType') }}</th>
              <th scope="col">{{ t('securityCenter.colStatus') }}</th>
              <th scope="col">{{ t('securityCenter.colTrigger') }}</th>
              <th scope="col">{{ t('securityCenter.colActions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in runs" :key="r.id">
              <td>{{ fmtTime(r.startedAt) }}</td>
              <td>{{ r.serverName }}</td>
              <td>{{ typeLabel(r.scanType) }}</td>
              <td>
                <div v-if="r.status === 'running'" class="cell-line"><span class="badge badge--running badge--pulse">{{ t('securityCenter.statusRunning') }}</span></div>
                <template v-else-if="r.status === 'failed'">
                  <div class="cell-line"><span class="badge badge--failed">{{ t('securityCenter.statusFailed') }}</span></div>
                  <div v-if="r.error" class="err-line" :title="r.error">{{ r.error }}</div>
                </template>
                <SevChips v-else :summary="r.summary" />
              </td>
              <td>{{ triggerLabel(r) }}</td>
              <td>
                <div class="cell-line">
                  <AppButton v-if="r.status === 'done'" variant="default" @click="openFindings(r)">{{ t('securityCenter.viewFindings') }}</AppButton>
                  <AppButton v-if="r.status !== 'running'" variant="default" @click="openLogs(r)">{{ t('securityCenter.viewLogs') }}</AppButton>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- 导出范围弹窗 -->
    <div v-if="showExportModal" class="modal-overlay" @click.self="showExportModal = false">
      <div class="modal modal--export" role="dialog" aria-modal="true">
        <div class="modal__head">
          <h3>{{ t('securityCenter.exportReport') }}</h3>
          <AppButton variant="default" @click="showExportModal = false">{{ t('securityCenter.findingsClose') }}</AppButton>
        </div>
        <div class="export-scope">
          <label class="field field--inline"><input v-model="exportScope" value="all" type="radio" name="sec-export-scope" /><span>{{ t('securityCenter.scopeAll') }}</span></label>
          <label class="field field--inline"><input v-model="exportScope" value="custom" type="radio" name="sec-export-scope" /><span>{{ t('securityCenter.scopeCustom') }}</span></label>
        </div>
        <template v-if="exportScope === 'custom'">
          <div class="export-toolbar">
            <input v-model="exportSearch" type="search" :placeholder="t('securityCenter.exportSearchPh')" />
            <AppButton variant="default" @click="selectAllCandidates()">{{ t('securityCenter.exportSelectAll') }}</AppButton>
            <AppButton variant="default" @click="clearExportSelection()">{{ t('securityCenter.exportClearSel') }}</AppButton>
            <span class="hint">{{ t('securityCenter.exportSelected', { n: exportSelected.size }) }}</span>
          </div>
          <div class="export-list">
            <label v-for="srv in exportCandidates" :key="srv.serverId" class="export-item">
              <input
                type="checkbox"
                :checked="exportSelected.has(srv.serverId)"
                @change="toggleExportServer(srv.serverId)"
              />
              <span class="srv-name">{{ srv.serverName }}</span>
              <span class="hint">{{ srv.host }}</span>
            </label>
            <div v-if="!exportCandidates.length" class="empty-hint">{{ t('securityCenter.exportNoMatch') }}</div>
          </div>
        </template>
        <div class="modal__pager">
          <AppButton variant="primary" :loading="exporting" @click="exportReport()">{{ t('securityCenter.exportConfirmBtn') }}</AppButton>
        </div>
      </div>
    </div>

    <!-- 漏洞明细弹窗 -->
    <div v-if="findingsRun" class="modal-overlay" @click.self="findingsRun = null">
      <div class="modal" role="dialog" aria-modal="true">
        <div class="modal__head">
          <h3>{{ findingsRun.serverName }} · {{ typeLabel(findingsRun.scanType) }} · {{ t('securityCenter.findingsTitle') }}</h3>
          <AppButton variant="default" @click="findingsRun = null">{{ t('securityCenter.findingsClose') }}</AppButton>
        </div>
        <div class="modal__filters">
          <AppSelect v-model="findingsFilter.severity" :options="severityOptions" @update:model-value="findingsFilterChanged()" />
          <AppSelect v-model="findingsFilter.source" :options="sourceOptions" @update:model-value="findingsFilterChanged()" />
          <input v-model="findingsFilter.q" type="search" :placeholder="t('securityCenter.findingsSearch')" @change="findingsFilterChanged()" />
        </div>
        <div v-if="findings" class="modal__meta hint">
          {{ t('securityCenter.findingsTotal', { n: findings.total }) }} · <SevChips :summary="findings.summary" />
        </div>
        <SkeletonBlock v-if="findingsLoading" />
        <div v-else-if="!findings || !findings.items.length" class="empty-hint">{{ t('securityCenter.findingsEmpty') }}</div>
        <div v-else class="table-wrap modal__table">
          <table class="table">
            <thead>
              <tr>
                <th scope="col">{{ t('securityCenter.colSeverity') }}</th>
                <th scope="col">{{ t('securityCenter.colId') }}</th>
                <th scope="col">{{ t('securityCenter.colTarget') }}</th>
                <th scope="col">{{ t('securityCenter.colInstalled') }}</th>
                <th scope="col">{{ t('securityCenter.colFixed') }}</th>
                <th scope="col">{{ t('securityCenter.aiExplain') }}</th>
              </tr>
            </thead>
            <tbody>
              <template v-for="(f, i) in findings.items" :key="`${f.id}-${f.target}-${i}`">
              <tr>
                <td><span class="sev" :class="`sev--${f.severity}`">{{ f.severity }}</span></td>
                <td><a v-if="f.url" :href="f.url" target="_blank" rel="noopener noreferrer">{{ f.id }}</a><template v-else>{{ f.id }}</template></td>
                <td>
                  <div>{{ f.target }}</div>
                  <div v-if="f.title" class="hint">{{ f.title }}</div>
                </td>
                <td>{{ f.installedVersion }}</td>
                <td>{{ f.fixedVersion }}</td>
                <td>
                  <AppButton
                    variant="default"
                    :loading="explainBusy === explainKey(f)"
                    @click="askExplain(f)"
                  >{{ t('securityCenter.aiExplain') }}</AppButton>
                </td>
              </tr>
              <tr v-if="explainShown === explainKey(f)" class="explain-row">
                <td :colspan="6">
                  <AiOpsMarkdown v-if="explainCache.get(explainKey(f))" :text="explainCache.get(explainKey(f))!" />
                  <span v-else class="hint">{{ t('securityCenter.aiExplainLoading') }}</span>
                </td>
              </tr>
              </template>
            </tbody>
          </table>
        </div>
        <div class="modal__pager">
          <AppButton variant="default" :disabled="findingsFilter.offset === 0" @click="findingsPageMove(-findingsFilter.limit)">{{ t('securityCenter.findingsPrev') }}</AppButton>
          <span class="hint">{{ findingsFilter.offset + 1 }}–{{ (findings?.items.length ?? 0) + findingsFilter.offset }} / {{ findings?.total ?? 0 }}</span>
          <AppButton variant="default" :disabled="(findingsFilter.offset + findingsFilter.limit) >= (findings?.total ?? 0)" @click="findingsPageMove(findingsFilter.limit)">{{ t('securityCenter.findingsNext') }}</AppButton>
        </div>
      </div>
    </div>

    <!-- 执行日志弹窗 -->
    <div v-if="logsRun" class="modal-overlay" @click.self="logsRun = null">
      <div class="modal modal--logs" role="dialog" aria-modal="true">
        <div class="modal__head">
          <h3>{{ logsRun.serverName }} · {{ typeLabel(logsRun.scanType) }} · {{ t('securityCenter.logsTitle') }}</h3>
          <AppButton variant="default" @click="logsRun = null">{{ t('securityCenter.findingsClose') }}</AppButton>
        </div>
        <SkeletonBlock v-if="logsLoading" />
        <div v-else-if="!logsView || !logsView.logs.length" class="empty-hint">{{ t('securityCenter.logsEmpty') }}</div>
        <div v-else class="logs-list">
          <details v-for="(l, i) in logsView.logs" :key="i" class="log-step" open>
            <summary class="log-step__head">
              <span class="log-step__name">{{ l.step }}</span>
              <span class="badge" :class="l.exitCode === 0 ? 'badge--ok' : 'badge--failed'">exit {{ l.exitCode }}</span>
            </summary>
            <template v-if="l.stdout">
              <div class="hint">{{ t('securityCenter.logsStdout') }}</div>
              <pre class="log-pre">{{ l.stdout }}</pre>
            </template>
            <template v-if="l.stderr">
              <div class="hint">{{ t('securityCenter.logsStderr') }}</div>
              <pre class="log-pre log-pre--err">{{ l.stderr }}</pre>
            </template>
          </details>
        </div>
      </div>
    </div>
  </template>
</template>

<style scoped>
.view-header { display: flex; justify-content: space-between; align-items: flex-end; gap: 16px; margin-bottom: 20px; }
.view-header__actions { display: flex; gap: 8px; flex-wrap: wrap; }
.view-title { font-size: 20px; margin: 0 0 4px; }
.view-sub { margin: 0; color: var(--pw-text-muted, #5b6474); font-size: 13px; }
.panel { background: var(--pw-panel-bg, #fff); border: 1px solid var(--pw-line, #d9dee8); border-radius: 10px; padding: 16px 18px; margin-bottom: 18px; }
.panel-title { font-size: 15px; margin: 0 0 10px; }
.panel-head { display: flex; justify-content: space-between; align-items: center; gap: 12px; }
.hint { color: var(--pw-text-muted, #5b6474); font-size: 12.5px; margin: 4px 0; }
.empty-hint { color: var(--pw-text-muted, #5b6474); padding: 18px 4px; text-align: center; }

.kpis { display: flex; gap: 10px; flex-wrap: wrap; }
.kpi { flex: 1 1 110px; border: 1px solid var(--pw-line, #d9dee8); border-radius: 8px; padding: 10px 12px; text-align: center; }
.kpi b { display: block; font-size: 22px; }
.kpi span { color: var(--pw-text-muted, #5b6474); font-size: 12px; }
.kpi__time { font-size: 13px !important; margin-top: 8px; }
.kpi--critical b { color: #b3261e; }
.kpi--high b { color: #e8590c; }
.kpi--medium b { color: #b08900; }
.kpi--low b { color: #2f6fab; }

.sched-grid { display: flex; gap: 14px; flex-wrap: wrap; align-items: flex-end; }
.field { display: flex; flex-direction: column; gap: 4px; font-size: 13px; min-width: 160px; }
.field > span { color: var(--pw-text-muted, #5b6474); }
.field input[type='text'], .field input[type='number'], .field input[type='password'], .field input[type='search'] { padding: 7px 9px; border: 1px solid var(--pw-line, #d9dee8); border-radius: 6px; font-size: 13px; }
.field--inline { flex-direction: row; align-items: center; gap: 8px; min-width: auto; }
.field--num { min-width: 110px; }
.field--actions { flex-direction: row; align-items: center; gap: 8px; min-width: auto; }

.add-form { display: flex; gap: 12px; flex-wrap: wrap; align-items: flex-end; border: 1px dashed var(--pw-line, #d9dee8); border-radius: 8px; padding: 12px; margin: 10px 0; }

.table-wrap { overflow-x: auto; }
.table { width: 100%; border-collapse: collapse; font-size: 13px; }
.table th, .table td { border: 1px solid var(--pw-line, #d9dee8); padding: 7px 9px; text-align: left; vertical-align: top; }
.table th { background: var(--pw-bg-subtle, #eef1f6); white-space: nowrap; }
.row-actions { display: flex; gap: 6px; flex-wrap: wrap; }
.srv-name { font-weight: 600; }

.badge { display: inline-block; padding: 1px 8px; border-radius: 10px; font-size: 12px; color: #fff; white-space: nowrap; }
.badge--running { background: #2f6fab; }
.badge--ok { background: #1b7f4d; }
.badge--failed { background: #b3261e; }
.badge--pulse { animation: sec-pulse 1.6s ease-in-out infinite; }
@keyframes sec-pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.55; } }
.cell-line { margin-bottom: 4px; }
.cell-line:last-child { margin-bottom: 0; }
.err-line {
  margin-top: 4px; max-width: 360px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  color: #b3261e; font-size: 12px; cursor: help;
}
.sev { display: inline-block; padding: 1px 8px; border-radius: 10px; font-size: 12px; color: #fff; text-transform: uppercase; }
.sev--critical { background: #b3261e; }
.sev--high { background: #e8590c; }
.sev--medium { background: #b08900; }
.sev--low { background: #2f6fab; }
.sev--info { background: #6b7280; }
.sev--unknown { background: #9aa1ad; }

.modal-overlay { position: fixed; inset: 0; background: rgba(15, 20, 30, 0.45); display: flex; align-items: center; justify-content: center; z-index: 60; padding: 24px; }
.modal { background: var(--pw-panel-bg, #fff); border-radius: 10px; padding: 18px 20px; max-width: 960px; width: 100%; max-height: 85vh; overflow: auto; display: flex; flex-direction: column; gap: 10px; }
.modal__head { display: flex; justify-content: space-between; align-items: center; gap: 12px; }
.modal__head h3 { margin: 0; font-size: 15px; }
.modal__filters { display: flex; gap: 8px; flex-wrap: wrap; }
.modal__filters input[type='search'] { flex: 1; min-width: 180px; padding: 7px 9px; border: 1px solid var(--pw-line, #d9dee8); border-radius: 6px; font-size: 13px; }
.modal__table { max-height: 52vh; overflow: auto; }
.modal__pager { display: flex; align-items: center; gap: 12px; justify-content: flex-end; }
.explain-row > td { background: var(--pw-bg-subtle, #f6f7fa); }
.modal--export { max-width: 560px; }

/* 执行日志弹窗:按步骤折叠,stdout/stderr 等宽字体块。 */
.logs-list { display: flex; flex-direction: column; gap: 10px; overflow: auto; max-height: 62vh; }
.log-step { border: 1px solid var(--pw-line, #d9dee8); border-radius: 8px; padding: 8px 12px; }
.log-step__head { display: flex; align-items: center; justify-content: space-between; gap: 10px; cursor: pointer; }
.log-step__name { font-size: 13px; font-weight: 600; }
.log-pre { margin: 4px 0 8px; padding: 8px 10px; background: var(--pw-code-bg, #f5f6f8); border-radius: 6px; font-size: 12px; line-height: 1.5; white-space: pre-wrap; word-break: break-all; max-height: 320px; overflow: auto; }
.log-pre--err { background: var(--pw-code-bg-err, #fdf2f2); }
.export-scope { display: flex; gap: 18px; }
.export-toolbar { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.export-toolbar input[type='search'] { flex: 1; min-width: 160px; padding: 7px 9px; border: 1px solid var(--pw-line, #d9dee8); border-radius: 6px; font-size: 13px; }
.export-list { max-height: 320px; overflow-y: auto; border: 1px solid var(--pw-line, #d9dee8); border-radius: 8px; padding: 4px 0; }
.export-item { display: flex; align-items: center; gap: 10px; padding: 7px 12px; cursor: pointer; font-size: 13px; }
.export-item:hover { background: var(--pw-bg-subtle, #f6f7fa); }
</style>
