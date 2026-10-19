<!--
  MultiLineChart.vue — 通用多序列时序折线(纯 SVG,无第三方图表库)。

  MetricTrendChart 的多量纲泛化:y 轴按数据自适应(漂亮刻度),序列由调用方给定
  (键 / 标签 / 颜色 / 值提取器),null 点断线。x 轴为时间(数值型 x 亦可用,
  labels 由调用方格式化)。
  · 供机器监控页:ssar 平台采样(CPU 分项 / 内存 / 负载 / 重传)与按需查询视图共用。
  · compositor-friendly:静态 SVG path,无逐帧动画。
-->
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

/** 一条序列的定义:key 唯一;label 图例;color CSS 颜色;value 从行数据取值(null = 断线)。 */
export interface ChartSeries<T> {
  key: string
  label: string
  color: string
  value: (row: T) => number | null
}

/** x 轴取值(毫秒时间戳或普通数值;需单调)。 */
export type ChartXValue<T> = (row: T) => number | null

const props = withDefaults(
  defineProps<{
    rows: Array<unknown>
    series: Array<ChartSeries<never>>
    x: ChartXValue<never>
    /** y 轴单位后缀(图例值展示用);如 "%"、"kB"。 */
    unit?: string
    /** y 固定上界(如百分比图 100);不设则自适应。 */
    yMax?: number
    width?: number
    height?: number
    /** x 轴刻度格式化(输入为 x 值)。 */
    xFormat?: (v: number) => string
    /** 值格式化(图例当前值;默认保留 1 位小数)。 */
    valueFormat?: (v: number) => string
  }>(),
  { width: 760, height: 240 },
)

const { t } = useI18n()

const padL = 46
const padR = 12
const padT = 12
const padB = 22

const innerW = computed(() => props.width - padL - padR)
const innerH = computed(() => props.height - padT - padB)

/** 有效行(x 可解析)。 */
const validRows = computed(() =>
  props.rows.filter((r) => props.x(r as never) != null && Number.isFinite(props.x(r as never) as number)),
)

const xRange = computed(() => {
  if (validRows.value.length === 0) return null
  let min = Infinity
  let max = -Infinity
  for (const r of validRows.value) {
    const v = props.x(r as never) as number
    if (v < min) min = v
    if (v > max) max = v
  }
  return { min, max, span: max - min || 1 }
})

/** y 值域(全部序列的有效值;自适应模式)。 */
const yDomain = computed(() => {
  let max = 0
  for (const s of props.series) {
    for (const r of validRows.value) {
      const v = s.value(r as never)
      if (v != null && v > max) max = v
    }
  }
  if (max <= 0) max = 1
  return max
})

/** 漂亮刻度上界:1/2/5×10^n 风格,值域至少给 4 段。 */
const yTop = computed(() => {
  if (props.yMax != null) return props.yMax
  const raw = yDomain.value * 1.08
  const mag = Math.pow(10, Math.floor(Math.log10(raw)))
  for (const m of [1, 2, 2.5, 5, 10]) {
    if (m * mag >= raw) return m * mag
  }
  return 10 * mag
})

function xAt(v: number): number {
  const r = xRange.value
  if (!r) return padL
  return padL + ((v - r.min) / r.span) * innerW.value
}

function yAt(v: number): number {
  const clamped = Math.max(0, Math.min(yTop.value, v))
  return padT + innerH.value - (clamped / yTop.value) * innerH.value
}

/** 每条序列拆成连续非空段的 path(null 断线)。 */
function segmentsFor(s: (typeof props.series)[number]): string[] {
  const segs: string[] = []
  let cur: string[] = []
  for (const r of validRows.value) {
    const xv = props.x(r as never) as number
    const v = s.value(r as never)
    if (v == null || !Number.isFinite(v)) {
      if (cur.length >= 2) segs.push(cur.join(' '))
      cur = []
      continue
    }
    cur.push(`${cur.length === 0 ? 'M' : 'L'}${xAt(xv).toFixed(1)},${yAt(v).toFixed(1)}`)
  }
  if (cur.length >= 2) segs.push(cur.join(' '))
  return segs
}

const lines = computed(() =>
  props.series.map((s) => ({
    ...s,
    paths: segmentsFor(s),
    last: lastValue(s),
  })),
)

function lastValue(s: (typeof props.series)[number]): number | null {
  for (let i = validRows.value.length - 1; i >= 0; i--) {
    const v = s.value(validRows.value[i] as never)
    if (v != null) return v
  }
  return null
}

/** y 轴刻度(4 段)。 */
const yTicks = computed(() => {
  const n = 4
  const out: { v: number; y: number; label: string }[] = []
  for (let i = 0; i <= n; i++) {
    const v = (yTop.value / n) * i
    out.push({ v, y: yAt(v), label: fmtNum(v) })
  }
  return out
})

function fmtNum(v: number): string {
  if (v >= 1e9) return `${(v / 1e9).toFixed(1)}G`
  if (v >= 1e6) return `${(v / 1e6).toFixed(1)}M`
  if (v >= 1e4) return `${(v / 1e3).toFixed(0)}k`
  if (v >= 100) return v.toFixed(0)
  if (v >= 10) return v.toFixed(0)
  return String(Math.round(v * 10) / 10)
}

function fmtVal(v: number): string {
  if (props.valueFormat) return props.valueFormat(v)
  return fmtNum(v)
}

/** x 轴时间标签(首 / 中 / 末)。 */
const xLabels = computed(() => {
  const r = xRange.value
  if (!r) return []
  const fmt =
    props.xFormat ??
    ((v: number) => {
      const d = new Date(v)
      return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
    })
  return [
    { x: padL, text: fmt(r.min), anchor: 'start' as const },
    { x: padL + innerW.value / 2, text: fmt(r.min + r.span / 2), anchor: 'middle' as const },
    { x: padL + innerW.value, text: fmt(r.max), anchor: 'end' as const },
  ]
})

const hasData = computed(() => lines.value.some((l) => l.paths.length > 0))
</script>

<template>
  <div class="mlc">
    <div v-if="!hasData" class="mlc__empty">{{ t('ssarMonitor.chartEmpty') }}</div>
    <template v-else>
      <svg
        class="mlc__svg"
        :viewBox="`0 0 ${width} ${height}`"
        :width="width"
        :height="height"
        role="img"
        :aria-label="t('ssarMonitor.chartAria')"
        preserveAspectRatio="none"
      >
        <g class="mlc__grid">
          <template v-for="tk in yTicks" :key="tk.v">
            <line :x1="padL" :y1="tk.y" :x2="width - padR" :y2="tk.y" />
            <text :x="padL - 6" :y="tk.y + 3" text-anchor="end" class="mlc__axis">{{ tk.label }}</text>
          </template>
        </g>
        <text
          v-for="(xl, i) in xLabels"
          :key="i"
          :x="xl.x"
          :y="height - 6"
          :text-anchor="xl.anchor"
          class="mlc__axis"
        >{{ xl.text }}</text>
        <template v-for="l in lines" :key="l.key">
          <path
            v-for="(d, i) in l.paths"
            :key="i"
            :d="d"
            :stroke="l.color"
            fill="none"
            class="mlc__line"
          />
        </template>
      </svg>
      <ul class="mlc__legend">
        <li v-for="l in lines" :key="l.key" class="mlc__legend-item">
          <span class="mlc__swatch" :style="{ background: l.color }" aria-hidden="true" />
          <span class="mlc__legend-label">{{ l.label }}</span>
          <span class="mlc__legend-val">
            {{ l.last != null ? fmtVal(l.last) + (unit ?? '') : '—' }}
          </span>
        </li>
      </ul>
    </template>
  </div>
</template>

<style scoped>
.mlc {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.mlc__empty {
  padding: 28px 0;
  text-align: center;
  font-size: var(--text-label);
  color: var(--color-faint, var(--color-dim));
}
.mlc__svg {
  display: block;
  width: 100%;
  height: auto;
}
.mlc__grid line {
  stroke: var(--color-line, var(--color-border));
  stroke-width: 1;
}
.mlc__axis {
  fill: var(--color-faint, var(--color-dim));
  font-size: 10px;
  font-variant-numeric: tabular-nums;
}
.mlc__line {
  stroke-width: 2;
  stroke-linecap: round;
  stroke-linejoin: round;
  vector-effect: non-scaling-stroke;
}
.mlc__legend {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  list-style: none;
  margin: 0;
  padding: 0;
}
.mlc__legend-item {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: var(--text-label);
  color: var(--color-dim);
}
.mlc__swatch {
  width: 10px;
  height: 10px;
  border-radius: 2px;
  flex: none;
}
.mlc__legend-val {
  font-variant-numeric: tabular-nums;
  color: var(--color-text);
  font-weight: 600;
}
</style>
