<!--
  SsarMonitor.vue — 机器监控(自维护 fork MyLinkPi/ssar)。

  三区:
    1. 平台安装包状态卡(GitHub Release 缓存 / 手动触发下载;进度轮询);
    2. 服务器列表(每台安装状态 / 采集器存活 / 最近采样;单台或批量「安装/升级」,
       选中行进入详情);
    3. 选中机器详情:平台采样总览图表(CPU 分项 / 内存 / 负载 / TCP)+ 机器侧
       细粒度按需查询(白名单视图 + 时长/步长,结果画多序列图 + 原始表格;时间列为
       机器本地时间)。
  数据:GET /api/ssar/overview(20s 轮询)/history(1h..7d)/query;写动作 POST
  download / install(带 CSRF;审计在后端)。
-->
<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  getSsarOverview,
  triggerSsarDownload,
  installSsar,
  querySsar,
  getSsarHistory,
  SSAR_VIEWS,
  type SsarOverview,
  type SsarServerStatus,
  type SsarInstallResult,
  type SsarHistoryPoint,
  type SsarQueryResult,
  type SsarView,
} from '../api/ssar'
import { HttpError } from '../api/http'
import AppButton from '../components/ui/AppButton.vue'
import ErrorState from '../components/ui/ErrorState.vue'
import SkeletonBlock from '../components/ui/SkeletonBlock.vue'
import MultiLineChart, { type ChartSeries } from '../components/metrics/MultiLineChart.vue'

type LoadState = 'idle' | 'loading' | 'error'

const { t } = useI18n()

const loadState = ref<LoadState>('idle')
const loadError = ref('')
const overview = ref<SsarOverview | null>(null)
const selectedId = ref('')

// ─── 总览轮询(状态探测走后端 TTL 缓存;下载进度也在这看)────────────────────
const OVERVIEW_POLL_MS = 20_000
let overviewTimer: ReturnType<typeof setInterval> | null = null

async function loadOverview(): Promise<void> {
  try {
    overview.value = await getSsarOverview()
    loadState.value = loadState.value === 'error' ? 'idle' : loadState.value
  } catch (e) {
    if (loadState.value !== 'loading') return // 已有数据时静默,不打断页面
    loadState.value = 'error'
    loadError.value = e instanceof HttpError ? e.message : String(e)
  }
}

async function initialLoad(): Promise<void> {
  loadState.value = 'loading'
  await loadOverview()
  if (loadState.value === 'loading') loadState.value = 'idle'
}

// ─── 安装 / 下载动作 ──────────────────────────────────────────────────────────
const installing = ref(false)
const installBanner = ref('')
const downloading = ref(false)

async function downloadPackages(): Promise<void> {
  downloading.value = true
  try {
    await triggerSsarDownload()
  } catch {
    // 202 端点失败少见;轮询状态里会体现 lastErr。
  } finally {
    downloading.value = false
  }
}

async function runInstall(serverIds: string[]): Promise<void> {
  if (serverIds.length === 0 || installing.value) return
  installing.value = true
  installBanner.value = ''
  try {
    const results = await installSsar(serverIds)
    const ok = results.filter((r: SsarInstallResult) => r.ok).length
    installBanner.value =
      ok === results.length
        ? t('ssarMonitor.installAllOk', { n: ok })
        : t('ssarMonitor.installPartial', { ok, fail: results.length - ok })
    const failed = results.find((r: SsarInstallResult) => !r.ok && r.error)
    if (failed) installBanner.value += ` · ${failed.error}`
    await loadOverview()
  } catch (e) {
    installBanner.value = e instanceof HttpError ? e.message : t('ssarMonitor.installFailed')
  } finally {
    installing.value = false
  }
}

function installAll(): void {
  const ids = (overview.value?.items ?? []).map((s) => s.serverId)
  void runInstall(ids)
}

function installOne(s: SsarServerStatus): void {
  void runInstall([s.serverId])
}

// ─── 选中机器的平台采样总览 ───────────────────────────────────────────────────
const historyHours = ref(24)
const historyLoading = ref(false)
const historyPoints = ref<SsarHistoryPoint[]>([])
const HISTORY_WINDOWS = [1, 6, 24, 72, 168]

const selected = computed(() =>
  (overview.value?.items ?? []).find((s) => s.serverId === selectedId.value) ?? null,
)

async function loadHistory(): Promise<void> {
  if (!selectedId.value) return
  historyLoading.value = true
  try {
    historyPoints.value = await getSsarHistory(selectedId.value, historyHours.value)
  } catch {
    historyPoints.value = []
  } finally {
    historyLoading.value = false
  }
}

function selectServer(id: string): void {
  selectedId.value = selectedId.value === id ? '' : id
  historyPoints.value = []
  drillRows.value = null
  if (selectedId.value) {
    void loadHistory()
    void runDrill()
  }
}

const ACCENT = '#4f7cff'
const GREEN = '#16a34a'
const YELLOW = '#e0a32e'
const RED = '#d64545'
const PURPLE = '#9333ea'
const CYAN = '#0891b2'

/** CPU 分项(jiffies/s,全 CPU 汇总口径;null 断线)。 */
const cpuSeries: ChartSeries<SsarHistoryPoint>[] = [
  { key: 'user', label: t('ssarMonitor.cpuUser'), color: ACCENT, value: (r) => r.cpuUser },
  { key: 'system', label: t('ssarMonitor.cpuSystem'), color: RED, value: (r) => r.cpuSystem },
  { key: 'iowait', label: t('ssarMonitor.cpuIowait'), color: YELLOW, value: (r) => r.cpuIowait },
  { key: 'steal', label: t('ssarMonitor.cpuSteal'), color: PURPLE, value: (r) => r.cpuSteal },
]

/** 内存已用 %(total-available 口径,含页缓存;无 available 时回退 total-free)。 */
function memUsedPct(r: SsarHistoryPoint): number | null {
  if (r.memTotalKb == null || r.memTotalKb <= 0) return null
  const free = r.memAvailKb ?? r.memFreeKb
  if (free == null) return null
  return ((r.memTotalKb - free) / r.memTotalKb) * 100
}

const memSeries: ChartSeries<SsarHistoryPoint>[] = [
  { key: 'used', label: t('ssarMonitor.memUsed'), color: GREEN, value: memUsedPct },
]

const loadSeries: ChartSeries<SsarHistoryPoint>[] = [
  { key: 'load1', label: t('ssarMonitor.load1'), color: ACCENT, value: (r) => r.load1 },
  { key: 'runq', label: t('ssarMonitor.runq'), color: YELLOW, value: (r) => r.runq },
]

const tcpSeries: ChartSeries<SsarHistoryPoint>[] = [
  { key: 'estab', label: t('ssarMonitor.tcpEstab'), color: CYAN, value: (r) => r.tcpCurrEstab },
  { key: 'retrans', label: t('ssarMonitor.tcpRetrans'), color: RED, value: (r) => r.tcpRetransSegs },
]

const historyX = (r: SsarHistoryPoint): number | null => {
  const ms = new Date(r.at).getTime()
  return Number.isNaN(ms) ? null : ms
}

// ─── 机器侧细粒度按需查询 ─────────────────────────────────────────────────────
const drillView = ref<SsarView | string>('cpu')
const drillMinutes = ref(60)
const drillStep = ref(1)
const drillLoading = ref(false)
const drillError = ref('')
const drillRows = ref<SsarQueryResult | null>(null)

const DRILL_WINDOWS = [
  { minutes: 30, step: 1 },
  { minutes: 60, step: 1 },
  { minutes: 180, step: 5 },
  { minutes: 360, step: 5 },
  { minutes: 1440, step: 10 },
  { minutes: 4320, step: 30 },
  { minutes: 10080, step: 60 },
]

const drillWindowLabel = (w: { minutes: number; step: number }): string =>
  w.minutes < 60
    ? t('ssarMonitor.minutesLabel', { n: w.minutes })
    : w.minutes < 1440
      ? t('ssarMonitor.hoursLabel', { n: w.minutes / 60 })
      : t('ssarMonitor.daysLabel', { n: w.minutes / 1440 })

function applyDrillWindow(w: { minutes: number; step: number }): void {
  drillMinutes.value = w.minutes
  drillStep.value = w.step
  void runDrill()
}

async function runDrill(): Promise<void> {
  if (!selectedId.value || drillLoading.value) return
  drillLoading.value = true
  drillError.value = ''
  try {
    drillRows.value = await querySsar({
      serverId: selectedId.value,
      view: drillView.value,
      minutes: drillMinutes.value,
      step: drillStep.value,
    })
  } catch (e) {
    drillRows.value = null
    drillError.value = e instanceof HttpError ? e.message : String(e)
  } finally {
    drillLoading.value = false
  }
}

/** 细查图表序列:取含数值的前 6 列(load5s 的 time 键除外)。 */
const DRILL_PALETTE = [ACCENT, GREEN, YELLOW, RED, PURPLE, CYAN]

const drillChartSeries = computed<ChartSeries<Record<string, number | string>>[]>(() => {
  const res = drillRows.value
  if (!res || res.rows.length === 0) return []
  const numericKeys = res.columns
    .map((c) => c.key)
    .filter((k) => res.rows.some((row) => typeof row.values[k] === 'number'))
    .slice(0, 6)
  return numericKeys.map((k, i) => ({
    key: k,
    label: k,
    color: DRILL_PALETTE[i % DRILL_PALETTE.length],
    value: (row: Record<string, number | string>) => {
      const v = row[k]
      return typeof v === 'number' ? v : null
    },
  }))
})

/** 细查图 x 轴:ssar collect_datetime(机器本地时间)。 */
const drillX = (row: Record<string, number | string>): number | null => {
  // QueryRow 展开为 { __time, ...values } 由 drillTableRows 提供同构行。
  const time = (row as { __time?: string }).__time
  if (!time) return null
  // 机器本地时间无时区;按本地时区解析仅用于排序/横向定位(展示用原串)。
  const ms = new Date(time.replace(' ', 'T')).getTime()
  return Number.isNaN(ms) ? null : ms
}

/** 图表行:把 QueryRow 摊平为 { __time, ...values }。 */
const drillChartRows = computed(() =>
  (drillRows.value?.rows ?? []).map((r) => ({ __time: r.time, ...r.values })),
)

/** 表格列(含时间列;机器本地时间)。 */
const drillTableColumns = computed(() => {
  const res = drillRows.value
  if (!res) return []
  return [{ key: 'collect_datetime', unit: '' }, ...res.columns]
})

function cellValue(row: { time: string; values: Record<string, number | string> }, key: string): string {
  if (key === 'collect_datetime') return row.time
  const v = row.values[key]
  if (v == null) return '—'
  if (typeof v === 'number') return String(Math.round(v * 100) / 100)
  return v
}

// ─── 展示格式化 ──────────────────────────────────────────────────────────────
function fmtTime(iso: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString()
}

function statusBadge(s: SsarServerStatus): { cls: string; text: string } {
  if (s.error) return { cls: 'badge--err', text: s.error }
  if (!s.installed) return { cls: 'badge--none', text: t('ssarMonitor.notInstalled') }
  const ver = `ssar v${s.version || '?'}`
  if (!s.collectorActive) return { cls: 'badge--warn', text: t('ssarMonitor.collectorStopped') + ' · ' + ver }
  return { cls: 'badge--ok', text: t('ssarMonitor.collectorActive') + ' · ' + ver }
}

const binLine = computed(() => {
  const b = overview.value?.binaries
  if (!b) return ''
  if (b.downloading) return t('ssarMonitor.binDownloading')
  if (b.version) return t('ssarMonitor.binVersion', { version: b.version, n: b.packages.length })
  if (b.lastErr) return t('ssarMonitor.binErr', { err: b.lastErr })
  return t('ssarMonitor.binEmpty')
})

onMounted(() => {
  void initialLoad()
  overviewTimer = setInterval(() => void loadOverview(), OVERVIEW_POLL_MS)
})
onUnmounted(() => {
  if (overviewTimer) clearInterval(overviewTimer)
})
</script>

<template>
  <div class="ssarmon">
    <header class="view-header">
      <div class="view-header__text">
        <h1 class="view-title">{{ t('ssarMonitor.title') }}</h1>
        <p class="view-sub">{{ t('ssarMonitor.subtitle') }}</p>
      </div>
      <div class="view-header__actions">
        <AppButton :loading="loadState === 'loading'" @click="initialLoad">
          {{ t('common.refresh') }}
        </AppButton>
      </div>
    </header>

    <ErrorState
      v-if="loadState === 'error'"
      :title="t('ssarMonitor.errLoadTitle')"
      :description="loadError"
      @retry="initialLoad"
    />
    <template v-else>
      <SkeletonBlock v-if="!overview" />

      <template v-else>
        <!-- ── 平台安装包状态 ─────────────────────────────────────────── -->
        <section class="panel bin-panel" aria-labelledby="ssar-bin-heading">
          <div class="bin-panel__main">
            <h2 id="ssar-bin-heading" class="panel-title">{{ t('ssarMonitor.binHeading') }}</h2>
            <p class="bin-panel__line" :class="{ 'bin-panel__line--err': !!overview.binaries.lastErr && !overview.binaries.version }">
              {{ binLine }}
            </p>
            <p v-if="overview.binaries.packages.length" class="bin-panel__pkgs">
              {{ overview.binaries.packages.join(' · ') }}
            </p>
          </div>
          <div class="bin-panel__actions">
            <AppButton
              variant="primary"
              :loading="downloading || overview.binaries.downloading"
              :disabled="overview.binaries.downloading"
              @click="downloadPackages"
            >
              {{ overview.binaries.version ? t('ssarMonitor.binRedownload') : t('ssarMonitor.binDownload') }}
            </AppButton>
            <AppButton
              variant="primary"
              :loading="installing"
              :disabled="overview.items.length === 0"
              @click="installAll"
            >
              {{ t('ssarMonitor.installAll') }}
            </AppButton>
          </div>
        </section>

        <p v-if="installBanner" class="install-banner" role="status">{{ installBanner }}</p>

        <!-- ── 服务器列表 ─────────────────────────────────────────────── -->
        <section class="panel" aria-labelledby="ssar-servers-heading">
          <h2 id="ssar-servers-heading" class="panel-title">{{ t('ssarMonitor.serversHeading') }}</h2>
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th scope="col">{{ t('ssarMonitor.colServer') }}</th>
                  <th scope="col">{{ t('ssarMonitor.colStatus') }}</th>
                  <th scope="col">{{ t('ssarMonitor.colLastSample') }}</th>
                  <th scope="col" class="col-act"><span class="sr-only">{{ t('ssarMonitor.colActions') }}</span></th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="s in overview.items"
                  :key="s.serverId"
                  :class="{ 'row--selected': s.serverId === selectedId }"
                  class="row--click"
                  @click="selectServer(s.serverId)"
                >
                  <td class="cell-name">{{ s.name }}</td>
                  <td><span class="badge" :class="statusBadge(s).cls">{{ statusBadge(s).text }}</span></td>
                  <td class="cell-mono">{{ s.lastSampleAt ? fmtTime(s.lastSampleAt) : '—' }}</td>
                  <td class="col-act">
                    <AppButton :loading="installing" @click.stop="installOne(s)">
                      {{ s.installed ? t('ssarMonitor.upgrade') : t('ssarMonitor.install') }}
                    </AppButton>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>

        <!-- ── 选中机器详情 ────────────────────────────────────────────── -->
        <section v-if="selected" class="panel" aria-labelledby="ssar-detail-heading">
          <div class="detail-head">
            <h2 id="ssar-detail-heading" class="panel-title">
              {{ t('ssarMonitor.detailHeading', { name: selected.name }) }}
            </h2>
            <div class="detail-head__range">
              <AppButton
                v-for="h in HISTORY_WINDOWS"
                :key="h"
                :variant="historyHours === h ? 'primary' : 'default'"
                :disabled="historyLoading"
                @click="historyHours = h; loadHistory()"
              >
                {{ h < 24 ? t('ssarMonitor.hoursLabel', { n: h }) : t('ssarMonitor.daysLabel', { n: h / 24 }) }}
              </AppButton>
            </div>
          </div>

          <div class="charts-grid">
            <div class="chart-card">
              <h3 class="chart-card__title">{{ t('ssarMonitor.chartCpu') }}</h3>
              <SkeletonBlock v-if="historyLoading" />
              <MultiLineChart v-else :rows="historyPoints" :series="cpuSeries" :x="historyX" unit=" j/s" />
            </div>
            <div class="chart-card">
              <h3 class="chart-card__title">{{ t('ssarMonitor.chartMem') }}</h3>
              <SkeletonBlock v-if="historyLoading" />
              <MultiLineChart v-else :rows="historyPoints" :series="memSeries" :x="historyX" unit="%" :y-max="100" />
            </div>
            <div class="chart-card">
              <h3 class="chart-card__title">{{ t('ssarMonitor.chartLoad') }}</h3>
              <SkeletonBlock v-if="historyLoading" />
              <MultiLineChart v-else :rows="historyPoints" :series="loadSeries" :x="historyX" />
            </div>
            <div class="chart-card">
              <h3 class="chart-card__title">{{ t('ssarMonitor.chartTcp') }}</h3>
              <SkeletonBlock v-if="historyLoading" />
              <MultiLineChart v-else :rows="historyPoints" :series="tcpSeries" :x="historyX" />
            </div>
          </div>

          <!-- 细查:机器侧 ssar 高粒度历史 -->
          <div class="drill">
            <div class="drill__bar">
              <label class="drill__label" for="ssar-drill-view">{{ t('ssarMonitor.drillView') }}</label>
              <select id="ssar-drill-view" v-model="drillView" class="select" @change="runDrill">
                <option v-for="v in SSAR_VIEWS" :key="v" :value="v">{{ v }}</option>
              </select>
              <div class="drill__windows">
                <AppButton
                  v-for="w in DRILL_WINDOWS"
                  :key="w.minutes"
                  :variant="drillMinutes === w.minutes ? 'primary' : 'default'"
                  :disabled="drillLoading"
                  @click="applyDrillWindow(w)"
                >
                  {{ drillWindowLabel(w) }}
                </AppButton>
              </div>
              <span class="drill__hint">{{ t('ssarMonitor.drillMachineLocal') }}</span>
            </div>

            <p v-if="drillError" class="drill__error" role="alert">{{ drillError }}</p>
            <SkeletonBlock v-if="drillLoading" />
            <template v-else-if="drillRows">
              <MultiLineChart
                :rows="drillChartRows"
                :series="drillChartSeries"
                :x="drillX"
                :x-format="(v: number) => new Date(v).toLocaleTimeString()"
              />
              <div v-if="drillRows.rows.length" class="table-wrap drill__table">
                <table class="table">
                  <thead>
                    <tr>
                      <th v-for="c in drillTableColumns" :key="c.key" scope="col">
                        {{ c.key === 'collect_datetime' ? t('ssarMonitor.drillTime') : c.key + (c.unit ? ` (${c.unit})` : '') }}
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="(row, i) in drillRows.rows.slice().reverse()" :key="i">
                      <td
                        v-for="c in drillTableColumns"
                        :key="c.key"
                        class="cell-mono"
                      >{{ cellValue(row, c.key) }}</td>
                    </tr>
                  </tbody>
                </table>
              </div>
            </template>
          </div>
        </section>
      </template>
    </template>
  </div>
</template>

<style scoped>
.ssarmon {
  display: flex;
  flex-direction: column;
  gap: 18px;
}
.bin-panel {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}
.bin-panel__line {
  margin: 6px 0 0;
  font-size: var(--text-label);
  color: var(--color-dim);
}
.bin-panel__line--err {
  color: var(--color-danger, #d64545);
}
.bin-panel__pkgs {
  margin: 4px 0 0;
  font-family: var(--font-mono);
  font-size: 0.76rem;
  color: var(--color-faint, var(--color-dim));
  word-break: break-all;
}
.bin-panel__actions {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
}
.install-banner {
  margin: 0;
  padding: 10px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  background: var(--color-card);
  font-size: var(--text-label);
  color: var(--color-text);
}
.badge {
  display: inline-block;
  padding: 3px 10px;
  border-radius: 999px;
  font-size: 0.76rem;
  font-weight: 600;
  white-space: nowrap;
}
.badge--ok {
  background: color-mix(in srgb, var(--color-success, #16a34a) 14%, transparent);
  color: var(--color-success, #16a34a);
}
.badge--warn {
  background: color-mix(in srgb, var(--color-warning, #e0a32e) 16%, transparent);
  color: var(--color-warning, #e0a32e);
}
.badge--err {
  background: color-mix(in srgb, var(--color-danger, #d64545) 14%, transparent);
  color: var(--color-danger, #d64545);
}
.badge--none {
  background: color-mix(in srgb, var(--color-dim) 14%, transparent);
  color: var(--color-dim);
}
.row--click {
  cursor: pointer;
}
.row--selected td {
  background: color-mix(in srgb, var(--color-accent, #4f7cff) 7%, transparent);
}
.detail-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}
.detail-head__range {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.charts-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(360px, 1fr));
  gap: 14px;
}
.chart-card {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  background: var(--color-card);
  box-shadow: var(--shadow);
  padding: 12px 14px;
}
.chart-card__title {
  margin: 0 0 8px;
  font-size: 0.84rem;
  font-weight: 600;
  color: var(--color-dim);
}
.drill {
  margin-top: 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.drill__bar {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.drill__label {
  font-size: var(--text-label);
  color: var(--color-dim);
}
.drill__windows {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.drill__hint {
  font-size: 0.76rem;
  color: var(--color-faint, var(--color-dim));
}
.drill__error {
  margin: 0;
  color: var(--color-danger, #d64545);
  font-size: var(--text-label);
}
.drill__table {
  max-height: 360px;
  overflow: auto;
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
  white-space: nowrap;
}
.col-act {
  text-align: right;
  white-space: nowrap;
}
</style>
