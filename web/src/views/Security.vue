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
  startScan, startFleetScan, listRuns, getFindings, downloadSecurityReport, explainFinding,
  type SecurityOverview, type Schedule, type ScannerInstance, type SecRun,
  type FindingsPage, type ScanType, type Finding,
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
const POLL_MS = 8000
let pollTimer: ReturnType<typeof setInterval> | null = null

async function loadAll(): Promise<void> {
  try {
    const [ov, hist] = await Promise.all([getOverview(), listRuns({ limit: 20 })])
    overview.value = ov
    runs.value = hist.items ?? []
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

async function loadScanners(): Promise<void> {
  try {
    const res = await listScanners()
    scanners.value = res.items ?? []
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

// ─── 报告导出 ────────────────────────────────────────────────────────────────
const exporting = ref(false)

async function exportReport(): Promise<void> {
  exporting.value = true
  try {
    const name = await downloadSecurityReport()
    toast.success(t('securityCenter.reportDownloaded', { name }))
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
      <AppButton variant="primary" :loading="exporting" @click="exportReport()">{{ t('securityCenter.exportReport') }}</AppButton>
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
              <th scope="col">{{ t('securityCenter.colActions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="sc in scanners" :key="sc.id">
              <td>{{ sc.name }}</td>
              <td>{{ sc.host }}:{{ sc.port }}</td>
              <td>{{ sc.tokenConfigured ? t('securityCenter.scannerTokenConfigured') : t('securityCenter.scannerTokenNone') }}</td>
              <td>{{ sc.source === 'appstore' ? t('securityCenter.scannerSourceAppstore') : t('securityCenter.scannerSourceManual') }}</td>
              <td class="row-actions">
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
                  </template>
                  <template v-else>
                    <div class="cell-line"><SevChips :summary="srv.trivy.summary" /></div>
                    <div class="cell-line"><AppButton variant="default" @click="openFindings({ id: srv.trivy!.runId, serverName: srv.serverName, scanType: srv.trivy!.scanType })">{{ t('securityCenter.viewFindings') }}</AppButton></div>
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
                  </template>
                  <template v-else>
                    <div class="cell-line"><SevChips :summary="srv.nuclei.summary" /></div>
                    <div class="cell-line"><AppButton variant="default" @click="openFindings({ id: srv.nuclei!.runId, serverName: srv.serverName, scanType: srv.nuclei!.scanType })">{{ t('securityCenter.viewFindings') }}</AppButton></div>
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
                <AppButton v-if="r.status === 'done'" variant="default" @click="openFindings(r)">{{ t('securityCenter.viewFindings') }}</AppButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

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
</style>
