<script setup lang="ts">
/**
 * LabelSelectorEditor — 标签选值控件(deploy 目标圈选 / 构建机池选择共用)。
 *
 * 从 servers.labels 汇总标签 key(含纯标记)与各 key 取值集合;条件行 = key 下拉 → value 下拉
 * (纯标记无 value)。写回时保持 `k=v,tag` 原格式(后端零改动,同一份语义见
 * internal/runner/selector.go)。此前内联在 JobDrawer,构建机选择(StageDrawer)复用后抽出。
 */
import { computed, ref, watch, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Server } from '../../api/servers'
import { useLabels } from '../../api/labels'
import './pipeline.css'

const props = defineProps<{
  /** 候选机器(从中汇总标签 key/value;无机器 / 无标签时给出空态提示)。 */
  servers?: Server[]
  /** 选择器表达式 `k=v,tag`(v-model)。 */
  modelValue: string
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: string): void
}>()

const { t } = useI18n()

// 候选集 = 机器实际标签 ∪ 登记处标签(悬置标签可先建后选,机器后面再挂)。
const { items: registryLabels, load: loadRegistryLabels } = useLabels()
onMounted(() => {
  void loadRegistryLabels()
})
const registryTerms = computed<string[]>(() => registryLabels.value.map((l) => l.name))

interface SelectorTerm {
  _key: number
  /** key 或纯标记文本 */
  k: string
  /** k=v 的 v;纯标记为 '' */
  v: string
}

let _seq = 0
const terms = ref<SelectorTerm[]>([])

function parseTerms(raw: string): SelectorTerm[] {
  return (raw || '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
    .map((t) => {
      const i = t.indexOf('=')
      if (i > 0) return { _key: ++_seq, k: t.slice(0, i), v: t.slice(i + 1) }
      return { _key: ++_seq, k: t, v: '' }
    })
}

// 外部回写(切 job / 切 stage / 表单重解)→ 重解选择器;自身 emit 引起的回写若与
// lastEmitted 等价则跳过,避免重建行打断正在进行的编辑。
// lastEmitted 必须以 undefined 哨兵起始:若直接取 props.modelValue,immediate 首次回调
// 会因「相等」被跳过,已保存的选择器在重新打开时永远反解不出条件行(回显为空)。
let lastEmitted: string | undefined = undefined
watch(
  () => props.modelValue,
  (raw) => {
    if (raw === lastEmitted) return
    lastEmitted = raw
    terms.value = parseTerms(raw)
  },
  { immediate: true },
)

/** 全部标签 key(去重):k=v 的 key + 纯标记;候选含登记处悬置标签。 */
function labelKeys(): string[] {
  const keys = new Set<string>()
  for (const srv of props.servers ?? []) {
    for (const part of (srv.labels || '').split(',')) {
      const term = part.trim()
      if (!term) continue
      const i = term.indexOf('=')
      keys.add(i > 0 ? term.slice(0, i) : term)
    }
  }
  for (const term of registryTerms.value) {
    const i = term.indexOf('=')
    keys.add(i > 0 ? term.slice(0, i) : term)
  }
  return [...keys].sort()
}

/** 某 key 的取值集合(k=v 项;纯标记 key 返回空 → 无 value 下拉)。 */
function labelValues(key: string): string[] {
  const vals = new Set<string>()
  for (const srv of props.servers ?? []) {
    for (const part of (srv.labels || '').split(',')) {
      const term = part.trim()
      if (!term) continue
      const i = term.indexOf('=')
      if (i > 0 && term.slice(0, i) === key) vals.add(term.slice(i + 1))
    }
  }
  for (const term of registryTerms.value) {
    const i = term.indexOf('=')
    if (i > 0 && term.slice(0, i) === key) vals.add(term.slice(i + 1))
  }
  return [...vals].sort()
}

function isBareTag(key: string): boolean {
  return labelValues(key).length === 0
}

function flushTerms(): void {
  const sel = terms.value
    .filter((r) => r.k.trim())
    .map((r) => (r.v.trim() ? `${r.k.trim()}=${r.v.trim()}` : r.k.trim()))
    .join(',')
  lastEmitted = sel
  emit('update:modelValue', sel)
}

function addTerm(): void {
  const first = labelKeys()[0] ?? ''
  terms.value.push({ _key: ++_seq, k: first, v: labelValues(first)[0] ?? '' })
  flushTerms()
}

function removeTerm(id: number): void {
  terms.value = terms.value.filter((r) => r._key !== id)
  flushTerms()
}

function onTermKeyChange(row: SelectorTerm, key: string): void {
  row.k = key
  row.v = labelValues(key)[0] ?? ''
  flushTerms()
}

function onTermValueChange(row: SelectorTerm, value: string): void {
  row.v = value
  flushTerms()
}
</script>

<template>
  <div class="selector-editor">
    <div v-if="(servers ?? []).length === 0 && registryTerms.length === 0" class="selector-empty">
      {{ t('pipelineJob.selectorNoServers') }}
    </div>
    <div v-else-if="labelKeys().length === 0" class="selector-empty">
      {{ t('pipelineJob.selectorNoLabels') }}
    </div>
    <template v-else>
      <div v-for="row in terms" :key="row._key" class="selector-row">
        <select
          class="drawer-select selector-key"
          :value="row.k"
          :aria-label="t('pipelineJob.selectorKeyAria')"
          @change="onTermKeyChange(row, ($event.target as HTMLSelectElement).value)"
        >
          <option v-for="k in labelKeys()" :key="k" :value="k">{{ k }}</option>
          <!-- 陈旧 key 兜底:标签后来被删时,补原始值 option,避免下拉显示与 model 分叉 -->
          <option v-if="row.k && !labelKeys().includes(row.k)" :value="row.k">{{ row.k }}</option>
        </select>
        <template v-if="!isBareTag(row.k)">
          <span class="selector-eq">=</span>
          <select
            class="drawer-select selector-value"
            :value="row.v"
            :aria-label="t('pipelineJob.selectorValueAria')"
            @change="onTermValueChange(row, ($event.target as HTMLSelectElement).value)"
          >
            <option v-for="v in labelValues(row.k)" :key="v" :value="v">{{ v }}</option>
            <!-- 陈旧 value 兜底:与 JobDrawer runnerServerMissing 同款,
                 当前值不在候选时补原始值 option,否则保存下去的是不可见旧值 -->
            <option v-if="row.v && !labelValues(row.k).includes(row.v)" :value="row.v">{{ row.v }}</option>
          </select>
        </template>
        <button
          class="selector-del"
          :aria-label="t('pipelineJob.selectorDelAria', { k: row.k })"
          @click="removeTerm(row._key)"
        >
          <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
            <path d="M18 6 6 18M6 6l12 12"/>
          </svg>
        </button>
      </div>
      <button class="selector-add" @click="addTerm">
        <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" aria-hidden="true">
          <path d="M12 5v14M5 12h14"/>
        </svg>
        {{ t('pipelineJob.selectorAddTerm') }}
      </button>
    </template>
  </div>
</template>

<style scoped>
.selector-editor {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.selector-empty {
  font-size: 0.76rem;
  color: var(--color-faint);
  font-style: italic;
}

.selector-row {
  display: flex;
  align-items: center;
  gap: 6px;
}

.selector-key {
  flex: 1;
  min-width: 0;
}

.selector-eq {
  color: var(--color-faint);
  font-family: var(--font-mono);
}

.selector-value {
  flex: 1;
  min-width: 0;
}

.selector-del {
  display: inline-grid;
  place-items: center;
  width: 26px;
  height: 26px;
  flex-shrink: 0;
  background: none;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-faint);
  cursor: pointer;
}

.selector-del:hover {
  color: var(--color-red, #d33);
  border-color: var(--color-red, #d33);
}

.selector-add {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  align-self: flex-start;
  padding: 5px 10px;
  background: var(--color-inset);
  border: 1px dashed var(--color-border-strong);
  border-radius: var(--rounded);
  color: var(--color-dim);
  font: inherit;
  font-size: 0.74rem;
  cursor: pointer;
}

.selector-add:hover {
  color: var(--color-primary);
  border-color: var(--color-primary);
}
</style>
