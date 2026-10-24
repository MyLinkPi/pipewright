<!--
  SevChips.vue — 安全中心严重度计数徽章组。

  非零严重度逐个渲染彩色小徽章(严重/高危/中危/低危/信息/未知);全零(无任何发现)
  渲染单个绿色「未发现问题」。替代旧版纯文本 "C 0 · H 0 · M 0 · L 0"。
-->
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { ScanSummary } from '../../api/security'

const props = defineProps<{ summary: ScanSummary }>()
const { t } = useI18n()

const defs = computed(() => [
  { key: 'critical', label: t('securityCenter.sevCritical'), count: props.summary.critical },
  { key: 'high', label: t('securityCenter.sevHigh'), count: props.summary.high },
  { key: 'medium', label: t('securityCenter.sevMedium'), count: props.summary.medium },
  { key: 'low', label: t('securityCenter.sevLow'), count: props.summary.low },
  { key: 'info', label: t('securityCenter.sevInfo'), count: props.summary.info },
  { key: 'unknown', label: t('securityCenter.sevUnknown'), count: props.summary.unknown },
])
const visible = computed(() => defs.value.filter((d) => d.count > 0))
</script>

<template>
  <span v-if="!visible.length" class="sev-chips">
    <span class="sev-chip sev-chip--clean">{{ t('securityCenter.noFindings') }}</span>
  </span>
  <span v-else class="sev-chips">
    <span v-for="d in visible" :key="d.key" class="sev-chip" :class="`sev-chip--${d.key}`">
      {{ d.label }} <b>{{ d.count }}</b>
    </span>
  </span>
</template>

<style scoped>
.sev-chips { display: inline-flex; gap: 4px; flex-wrap: wrap; }
.sev-chip {
  display: inline-flex; align-items: center; gap: 4px;
  padding: 1px 8px; border-radius: 999px; font-size: 12px; line-height: 18px; white-space: nowrap;
}
.sev-chip b { font-variant-numeric: tabular-nums; font-weight: 700; }
.sev-chip--critical { color: #b3261e; background: rgba(179, 38, 30, 0.1); }
.sev-chip--high     { color: #d1490b; background: rgba(232, 89, 12, 0.12); }
.sev-chip--medium   { color: #8a6d00; background: rgba(176, 137, 0, 0.14); }
.sev-chip--low      { color: #2f6fab; background: rgba(47, 111, 171, 0.12); }
.sev-chip--info     { color: #5b6474; background: rgba(107, 114, 128, 0.14); }
.sev-chip--unknown  { color: #5b6474; background: rgba(154, 161, 173, 0.18); }
.sev-chip--clean    { color: #1b7f4d; background: rgba(27, 127, 77, 0.12); }
</style>
