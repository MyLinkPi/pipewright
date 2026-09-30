<script setup lang="ts">
/**
 * RunnerPanel — 构建机池选择器(FR-8-14 续 / FR-8-19 池化).
 *
 * 三种模式:本地构建(默认)/ 按标签选择(在打了标签的服务器池里按「优先级 → 流水线亲和 → 负载」
 * 选机;输入选择器实时预览命中机器)/ 指定服务器(钉死单机,等价 server:<id>)。
 * 构建仍由控制机克隆后经 SSH 传工作区到选中机器的容器里执行;token 只在控制机。
 */
import { ref, computed, onMounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { getRunner, saveRunnerSelector } from '../api/runner'
import { listServers, type Server } from '../api/servers'
import { HttpError } from '../api/http'

const props = defineProps<{ projectId: string }>()

const { t } = useI18n()

type LoadState = 'idle' | 'loading' | 'error'
type Mode = 'local' | 'label' | 'server'

const loadState = ref<LoadState>('idle')
const loadError = ref('')

const servers = ref<Server[]>([])
const mode = ref<Mode>('local')
const selectorText = ref('') // label 模式的选择器表达式
const selectedServer = ref('') // server 模式钉死的机器 id
const saveSubmitting = ref(false)
const saveBanner = ref('')
const saveSuccess = ref(false)

// 客户端标签匹配预览(与服务端 runner 域同一语义:逗号分隔项全部命中才匹配;纯 tag 不匹配 k=v)。
function matchServers(selector: string): Server[] {
  const terms = selector.split(',').map((s) => s.trim()).filter(Boolean)
  if (terms.length === 0) return []
  return servers.value.filter((s) => {
    const labels = (s.labels ?? '').split(',').map((x) => x.trim()).filter(Boolean)
    return terms.every((term) => labels.includes(term))
  })
}

const matched = computed(() => (mode.value === 'label' ? matchServers(selectorText.value) : []))

async function load(): Promise<void> {
  loadState.value = 'loading'
  loadError.value = ''
  try {
    const [cfg, srv] = await Promise.all([getRunner(props.projectId), listServers().catch(() => [])])
    servers.value = srv
    const sel = (cfg.selector ?? '').trim()
    if (sel === '') {
      mode.value = 'local'
    } else if (sel.startsWith('server:')) {
      mode.value = 'server'
      selectedServer.value = sel.slice('server:'.length)
    } else {
      mode.value = 'label'
      selectorText.value = sel
    }
    loadState.value = 'idle'
  } catch (err) {
    loadState.value = 'error'
    loadError.value = err instanceof HttpError ? err.message : t('projectPanels.runner.errLoad')
  }
}

async function handleSave(): Promise<void> {
  saveBanner.value = ''
  saveSuccess.value = false
  let selector = ''
  if (mode.value === 'label') {
    selector = selectorText.value.trim()
    if (selector === '') return // 空表达式不落库,提示走本地模式
    if (matched.value.length === 0) {
      saveSuccess.value = false
      saveBanner.value = t('projectPanels.runner.noMatch')
      return
    }
  } else if (mode.value === 'server') {
    if (!selectedServer.value) return
    selector = `server:${selectedServer.value}`
  }
  saveSubmitting.value = true
  try {
    const cfg = await saveRunnerSelector(props.projectId, selector)
    saveSuccess.value = true
    saveBanner.value =
      cfg.selector === ''
        ? t('projectPanels.runner.setLocal')
        : t('projectPanels.runner.setSelector', { selector: cfg.selector })
  } catch (err) {
    saveSuccess.value = false
    saveBanner.value =
      err instanceof HttpError ? (err.apiError?.message ?? t('projectPanels.runner.errSaveFailed')) : t('projectPanels.runner.errSaveRetry')
  } finally {
    saveSubmitting.value = false
  }
}

onMounted(load)
watch(() => props.projectId, load)
</script>

<template>
  <section class="config-card" aria-labelledby="runner-heading">
    <div class="card-head">
      <span class="card-icon" aria-hidden="true">
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9">
          <rect x="2" y="4" width="20" height="6" rx="1" /><rect x="2" y="14" width="20" height="6" rx="1" />
          <path d="M6 7h.01M6 17h.01" />
        </svg>
      </span>
      <h2 id="runner-heading" class="card-title">{{ t('projectPanels.runner.title') }}</h2>
      <span class="card-sub">{{ t('projectPanels.runner.sub') }}</span>
    </div>
    <div class="card-body card-body--pad">
      <p v-if="loadState === 'loading'" class="runner-loading">{{ t('projectPanels.runner.loading') }}</p>
      <p v-else-if="loadState === 'error'" class="runner-error" role="alert">{{ loadError }}</p>
      <template v-else>
        <label class="runner-field-label" for="runner-mode">{{ t('projectPanels.runner.whereLabel') }}</label>
        <select id="runner-mode" v-model="mode" class="runner-select" @change="saveSuccess = false">
          <option value="local">{{ t('projectPanels.runner.optionLocal') }}</option>
          <option value="label">{{ t('projectPanels.runner.optionLabel') }}</option>
          <option value="server">{{ t('projectPanels.runner.optionServer') }}</option>
        </select>

        <template v-if="mode === 'label'">
          <label class="runner-field-label" for="runner-selector">{{ t('projectPanels.runner.selectorLabel') }}</label>
          <input
            id="runner-selector"
            v-model="selectorText"
            class="runner-input"
            type="text"
            :placeholder="t('projectPanels.runner.selectorPlaceholder')"
            @input="saveSuccess = false"
          >
          <p class="runner-hint">
            {{ t('projectPanels.runner.selectorHint') }}
          </p>
          <p
            v-if="selectorText.trim()"
            class="runner-match"
            :class="matched.length === 0 ? 'runner-match--none' : ''"
            role="status"
          >
            {{ matched.length === 0 ? t('projectPanels.runner.noMatch') : t('projectPanels.runner.matchCount', { n: matched.length }) }}
            <span v-if="matched.length"> — {{ matched.map((s) => s.name).join(', ') }}</span>
          </p>
        </template>

        <template v-else-if="mode === 'server'">
          <label class="runner-field-label" for="runner-server">{{ t('projectPanels.runner.serverLabel') }}</label>
          <select id="runner-server" v-model="selectedServer" class="runner-select" @change="saveSuccess = false">
            <option value="">{{ t('projectPanels.runner.serverPick') }}</option>
            <option v-for="s in servers" :key="s.id" :value="s.id">{{ t('projectPanels.runner.optionRemote', { name: s.name, host: s.host }) }}</option>
          </select>
        </template>

        <p v-else class="runner-hint">
          {{ t('projectPanels.runner.hint') }}
        </p>
        <p
          v-if="saveBanner"
          class="runner-banner"
          :class="saveSuccess ? 'runner-banner--ok' : 'runner-banner--err'"
          role="status"
        >{{ saveBanner }}</p>
        <div class="runner-save">
          <button class="btn-primary" :disabled="saveSubmitting" :aria-busy="saveSubmitting" @click="handleSave">
            <span v-if="saveSubmitting" class="spinner" aria-hidden="true" />
            {{ saveSubmitting ? t('projectPanels.runner.saving') : t('projectPanels.runner.save') }}
          </button>
        </div>
      </template>
    </div>
  </section>
</template>

<style scoped>
.config-card {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-lg, 12px);
  background: var(--color-card);
  overflow: hidden;
}
.card-head {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 14px 16px;
  border-bottom: 1px solid var(--color-border);
}
.card-icon {
  display: grid;
  place-items: center;
  width: 26px;
  height: 26px;
  border-radius: var(--rounded-md);
  background: var(--color-primary-soft);
  color: var(--color-primary);
  flex: none;
}
.card-title { font-size: 0.92rem; font-weight: 650; color: var(--color-text); }
.card-sub { font-size: 0.76rem; color: var(--color-faint); flex: 1; min-width: 0; }
.card-body--pad { padding: 16px; display: flex; flex-direction: column; gap: 12px; }
.runner-loading, .runner-error { margin: 0; font-size: 0.82rem; }
.runner-error { color: var(--color-danger, #dc2626); }
.runner-loading { color: var(--color-faint); }
.runner-field-label { font-size: 0.8rem; font-weight: 600; color: var(--color-text); }
.runner-select, .runner-input {
  width: 100%;
  height: 36px;
  padding: 0 11px;
  font: inherit;
  font-size: 0.85rem;
  color: var(--color-text);
  background: var(--color-bg-subtle, var(--color-card));
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-md);
  box-sizing: border-box;
}
.runner-select:focus, .runner-input:focus { outline: none; border-color: var(--color-primary); }
.runner-hint { margin: 0; font-size: 0.7rem; color: var(--color-faint); line-height: 1.45; }
.runner-match { margin: 0; font-size: 0.74rem; color: var(--color-success, #16a34a); font-weight: 500; }
.runner-match--none { color: var(--color-danger, #dc2626); }
.runner-banner { margin: 0; font-size: 0.8rem; font-weight: 500; }
.runner-banner--ok { color: var(--color-success, #16a34a); }
.runner-banner--err { color: var(--color-danger, #dc2626); }
.runner-save { display: flex; justify-content: flex-end; }
</style>
