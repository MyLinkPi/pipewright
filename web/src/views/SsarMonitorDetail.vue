<!--
  SsarMonitorDetail.vue — 机器监控·单机详情页(/ssar-monitor/:id)。

  从列表页点机器进入;头部「← 返回机器列表」+ 机器名 + ssar 状态徽标 + 最近采样。
  两区:
    1. 总览图表(平台侧周期采样,60s 自动刷新):CPU 分项 / 内存已用 % / 负载与
       运行队列 / TCP 连接与重传;时间窗 1h..7d;
    2. 细查(机器侧 ssar 本地高粒度历史,按需查询):白名单视图 + 时长/步长 →
       多序列折线 + 原始表格(时间列为机器本地时间)。
-->
<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import {
  getSsarOverview,
  querySsar,
  getSsarHistory,
  SSAR_VIEWS,
  type SsarServerStatus,
  type SsarHistoryPoint,
  type SsarQueryResult,
  type SsarView,
} from '../api/ssar'
import { HttpError } from '../api/http'
import AppButton from '../components/ui/AppButton.vue'
import SkeletonBlock from '../components/ui/SkeletonBlock.vue'
import MultiLineChart, { type ChartSeries } from '../components/metrics/MultiLineChart.vue'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const serverId = route.params.id as string

// ─── 机器身份 / 状态(单次拉取;安装动作回列表页做)──────────────────────────
const server = ref<SsarServerStatus | null>(null)

async function loadStatus(): Promise<void> {
  try {
    const o = await getSsarOverview()
    server.value = o.items.find((s) => s.serverId === serverId) ?? null
  } catch {
    server.value = null // 状态拉不到不阻塞图表
  }
}

const notReady = computed(() => {
  const s = server.value
  return !!s && !s.installed
})

// ─── 平台采样总览(60s 自动刷新,与采样周期同拍)──────────────────────────────
const historyHours = ref(24)
const historyLoading = ref(false)
const historyPoints = ref<SsarHistoryPoint[]>([])
const HISTORY_WINDOWS = [1, 6, 24, 72, 168]
const HISTORY_POLL_MS = 60_000
let historyTimer: ReturnType<typeof setInterval> | null = null

async function loadHistory(): Promise<void> {
  historyLoading.value = true
  try {
    historyPoints.value = await getSsarHistory(serverId, historyHours.value)
  } catch {
    historyPoints.value = []
  } finally {
    historyLoading.value = false
  }
}

function setHistoryWindow(h: number): void {
  if (historyHours.value === h) return
  historyHours.value = h
  void loadHistory()
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
  if (drillLoading.value) return
  drillLoading.value = true
  drillError.value = ''
  try {
    drillRows.value = await querySsar({
      serverId,
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

/** 细查图表序列:取含数值的前 6 列。 */
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

function goBack(): void {
  void router.push({ name: 'ssar-monitor' })
}

function refreshAll(): void {
  void loadStatus()
  void loadHistory()
  void runDrill()
}

onMounted(() => {
  void loadStatus()
  void loadHistory()
  void runDrill()
  historyTimer = setInterval(() => void loadHistory(), HISTORY_POLL_MS)
})
onUnmounted(() => {
  if (historyTimer) clearInterval(historyTimer)
})
</script>

<template>
  <div class="ssardet">
    <a class="link-btn back-link" href="#" @click.prevent="goBack">← {{ t('ssarMonitor.back') }}</a>

    <header class="view-header">
      <div class="view-header__text">
        <h1 class="view-title">
          {{ server?.name || '…' }}
          <span v-if="server" class="badge" :class="statusBadge(server).cls">{{ statusBadge(server).text }}</span>
        </h1>
        <p class="view-sub">
          {{ t('ssarMonitor.lastSampleLabel') }}:{{ fmtTime(server?.lastSampleAt || '') }}
        </p>
      </div>
      <div class="view-header__actions">
        <AppButton :loading="historyLoading" @click="refreshAll">{{ t('common.refresh') }}</AppButton>
      </div>
    </header>

    <p v-if="notReady" class="hint-panel" role="status">{{ t('ssarMonitor.notInstalledHint') }}</p>

    <!-- ── 总览图表(平台采样;60s 自动刷新)────────────────────────────── -->
    <div class="detail-head">
      <h2 class="panel-title">{{ t('ssarMonitor.overviewHeading') }}</h2>
      <div class="detail-head__range">
        <AppButton
          v-for="h in HISTORY_WINDOWS"
          :key="h"
          :variant="historyHours === h ? 'primary' : 'default'"
          :disabled="historyLoading"
          @click="setHistoryWindow(h)"
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

    <!-- ── 细查:机器侧 ssar 高粒度历史 ──────────────────────────────────── -->
    <section class="panel drill-panel" aria-labelledby="ssar-drill-heading">
      <h2 id="ssar-drill-heading" class="panel-title">{{ t('ssarMonitor.drillView') }}</h2>
      <p class="drill__hint">{{ t('ssarMonitor.drillMachineLocal') }}</p>

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
                  {{
                    c.key === 'collect_datetime'
                      ? t('ssarMonitor.drillTime')
                      : c.key + (c.unit ? ` (${c.unit})` : '')
                  }}
                </th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="(row, i) in drillRows.rows.slice().reverse()" :key="i">
                <td v-for="c in drillTableColumns" :key="c.key" class="cell-mono">
                  {{ cellValue(row, c.key) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </section>
  </div>
</template>

<style scoped>
.ssardet {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.back-link {
  font-size: 0.84rem;
}
.link-btn {
  color: var(--color-accent, #4f7cff);
  text-decoration: none;
}
.link-btn:hover {
  text-decoration: underline;
}
.badge {
  display: inline-block;
  padding: 3px 10px;
  border-radius: 999px;
  font-size: 0.76rem;
  font-weight: 600;
  white-space: nowrap;
  vertical-align: middle;
  margin-left: 8px;
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
.hint-panel {
  margin: 0;
  padding: 10px 14px;
  border: 1px solid color-mix(in srgb, var(--color-warning, #e0a32e) 40%, transparent);
  border-radius: var(--rounded-card);
  background: color-mix(in srgb, var(--color-warning, #e0a32e) 8%, transparent);
  font-size: var(--text-label);
  color: var(--color-text);
}
.detail-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
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
.drill-panel {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.drill__hint {
  margin: -6px 0 0;
  font-size: 0.76rem;
  color: var(--color-faint, var(--color-dim));
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
.cell-mono {
  font-family: var(--font-mono);
  font-size: 0.8rem;
  white-space: nowrap;
}
</style>
