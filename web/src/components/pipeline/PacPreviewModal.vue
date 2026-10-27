<script setup lang="ts">
/**
 * PacPreviewModal — fetch & validate the repo's pipeline file (default `.pipewright.yml`,
 * per-project pacFile, or an ad-hoc `file` override) at a chosen ref BEFORE relying on it
 * to drive runs (GitOps Slice 3).
 *
 * At runtime, an invalid file is silently ignored (the run falls back to the stored UI
 * pipeline) with no feedback. This modal gives that feedback up front: pick a ref (and
 * optionally a file), fetch, and see whether the file is found, whether it is valid, the
 * parse error if not, a summary of the stages the runtime would use, and the variables the
 * YAML declares (synced into pipeline settings on success — the「变量与缓存」tab shows them).
 *
 * When the validated file differs from the project's configured default, a
 * "set as project default" action persists it via PATCH /projects/{id} (emits `saved`).
 * It never reveals secrets.
 */
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { previewPacConfig, updateProject, type PacPreviewResult } from '../../api/projects'
import { HttpError } from '../../api/http'

const props = defineProps<{
  projectId: string
  /** Project default branch — pre-fills the ref input. */
  defaultBranch: string
  /** Project-configured pipeline file ('' = default `.pipewright.yml`). */
  pacFile?: string
}>()

const DEFAULT_PAC_FILE = '.pipewright.yml'

const { t } = useI18n()

const emit = defineEmits<{
  (e: 'close'): void
  /** Fired after a successful preview — the server has synced variable declarations into settings. */
  (e: 'synced'): void
  /** Fired after the validated file was persisted as the project default (pacFile updated). */
  (e: 'saved'): void
}>()

const projectFile = computed(() => props.pacFile?.trim() || DEFAULT_PAC_FILE)
const refInput = ref(props.defaultBranch ?? '')
const fileInput = ref(projectFile.value)
const busy = ref(false)
const errorMsg = ref('')
const result = ref<PacPreviewResult | null>(null)

const fileDiffers = computed(() => fileInput.value.trim() !== projectFile.value.trim())
const savingFile = ref(false)
const savedFileNote = ref('')
const savedFileErr = ref('')

async function runPreview(): Promise<void> {
  if (busy.value) return
  busy.value = true
  errorMsg.value = ''
  savedFileNote.value = ''
  savedFileErr.value = ''
  result.value = null
  try {
    const res = await previewPacConfig(props.projectId, refInput.value, fileInput.value.trim() || undefined)
    result.value = res
    // Valid preview ⇒ the server synced the declared variables into pipeline settings;
    // let the parent refresh the「变量与缓存」tab data.
    if (res.found && res.valid) emit('synced')
  } catch (err) {
    if (err instanceof HttpError) {
      errorMsg.value = err.status === 0
        ? t('projectPipeline.pacPreviewConnFailed')
        : (err.apiError?.message ?? t('projectPipeline.pacPreviewFailed', { status: err.status }))
    } else {
      errorMsg.value = t('projectPipeline.pacPreviewFailedRetry')
    }
  } finally {
    busy.value = false
  }
}

async function saveAsDefaultFile(): Promise<void> {
  if (savingFile.value || !fileDiffers.value) return
  savingFile.value = true
  savedFileNote.value = ''
  savedFileErr.value = ''
  try {
    await updateProject(props.projectId, { pacFile: fileInput.value.trim() })
    savedFileNote.value = t('projectPipeline.pacPreviewSetDefaultDone')
    emit('saved')
  } catch {
    savedFileErr.value = t('projectPipeline.pacPreviewSetDefaultFailed')
  } finally {
    savingFile.value = false
  }
}

// 不自动拉取:打开弹窗常是为了改文件路径/分支,auto-run 会立刻按默认参数克隆仓库,
// 既慢又干扰输入。首次结果由用户点击「拉取并校验」触发(空态显示引导提示)。
</script>

<template>
  <div class="pp-backdrop" role="dialog" aria-modal="true" aria-labelledby="pp-title">
    <div class="pp-modal">
      <header class="pp-head">
        <div>
          <h2 id="pp-title" class="pp-title">{{ t('projectPipeline.pacPreviewTitle') }}</h2>
          <p class="pp-sub">
            {{ t('projectPipeline.pacPreviewSub') }}
            <code>{{ fileInput.trim() || DEFAULT_PAC_FILE }}</code>
          </p>
        </div>
        <button class="pp-close" :aria-label="t('projectPipeline.pacPreviewCloseAria')" @click="emit('close')">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>
        </button>
      </header>

      <div class="pp-body">
        <div class="pp-ref-row">
          <label for="pp-ref" class="pp-label">{{ t('projectPipeline.pacPreviewRefLabel') }}</label>
          <!-- 输入框在拉取期间保持可编辑:参数在点击「拉取并校验」那一刻才读取,auto-run
               的浅克隆再慢也不锁输入(否则一打开弹窗就灰掉,没法改文件路径)。 -->
          <input
            id="pp-ref"
            v-model="refInput"
            class="pp-ref-input pp-ref-input--ref"
            type="text"
            spellcheck="false"
            autocomplete="off"
            :placeholder="defaultBranch || 'master'"
            @keydown.enter.prevent="runPreview"
          />
          <label for="pp-file" class="pp-label">{{ t('projectPipeline.pacPreviewFileLabel') }}</label>
          <input
            id="pp-file"
            v-model="fileInput"
            class="pp-ref-input"
            type="text"
            spellcheck="false"
            autocomplete="off"
            :placeholder="DEFAULT_PAC_FILE"
            @keydown.enter.prevent="runPreview"
          />
          <button class="pp-btn pp-btn--primary" :disabled="busy" @click="runPreview">
            <span v-if="busy" class="pp-spin" aria-hidden="true"/>
            {{ t('projectPipeline.pacPreviewFetch') }}
          </button>
        </div>

        <!-- persist the ad-hoc file as the project default:常驻显示(入口可见),
             与项目当前配置一致时置灰——不再依赖「先改动才出现」。 -->
        <div class="pp-file-row">
          <button
            class="pp-btn pp-btn--ghost"
            :disabled="savingFile || !fileDiffers"
            :title="fileDiffers ? '' : t('projectPipeline.pacPreviewSetDefaultSame')"
            @click="saveAsDefaultFile"
          >
            {{ t('projectPipeline.pacPreviewSetDefault') }}
          </button>
          <span v-if="savedFileNote" class="pp-file-note pp-file-note--ok">{{ savedFileNote }}</span>
          <span v-if="savedFileErr" class="pp-file-note pp-file-note--err" role="alert">{{ savedFileErr }}</span>
        </div>

        <!-- request-level error (network / unexpected) -->
        <div v-if="errorMsg" class="pp-banner pp-banner--error" role="alert">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/></svg>
          <span>{{ errorMsg }}</span>
        </div>

        <!-- result -->
        <template v-else-if="result">
          <!-- not found -->
          <div v-if="!result.found" class="pp-banner pp-banner--warn" role="status">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/></svg>
            <span>{{ t('projectPipeline.pacPreviewNotFound', { ref: result.ref, file: result.file }) }}</span>
          </div>

          <!-- found but invalid -->
          <template v-else-if="!result.valid">
            <div class="pp-banner pp-banner--error" role="alert">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/></svg>
              <span>{{ t('projectPipeline.pacPreviewInvalid', { ref: result.ref, file: result.file }) }}</span>
            </div>
            <pre v-if="result.error" class="pp-error-detail">{{ result.error }}</pre>
          </template>

          <!-- found and valid -->
          <template v-else>
            <div class="pp-banner pp-banner--ok" role="status">
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" aria-hidden="true"><path d="M20 6 9 17l-5-5"/></svg>
              <span>{{ t('projectPipeline.pacPreviewValid', { ref: result.ref, count: result.stageCount, file: result.file }) }}</span>
            </div>
            <ul v-if="result.stages.length" class="pp-stages">
              <li v-for="(stage, i) in result.stages" :key="i" class="pp-stage">
                <span class="pp-stage-name">{{ stage.name }}</span>
                <span class="pp-stage-kind">{{ stage.kind }}</span>
                <span class="pp-stage-jobs">{{ t('projectPipeline.pacPreviewJobCount', { count: stage.jobCount }) }}</span>
              </li>
            </ul>

            <!-- declared variables (synced into「变量与缓存」) -->
            <div v-if="result.variables.length" class="pp-vars">
              <h3 class="pp-vars-title">{{ t('projectPipeline.pacPreviewVarsTitle', { count: result.variables.length }) }}</h3>
              <p class="pp-vars-hint">{{ t('projectPipeline.pacPreviewVarsHint') }}</p>
              <ul class="pp-vars-list">
                <li v-for="v in result.variables" :key="v.name" class="pp-var">
                  <span class="pp-var-name mono">{{ v.name }}</span>
                  <span v-if="v.required" class="pp-badge pp-badge--req">{{ t('projectPipeline.pacVarRequired') }}</span>
                  <span v-if="v.secret" class="pp-badge pp-badge--sec">{{ t('projectPipeline.pacVarSecret') }}</span>
                  <span class="pp-var-desc">{{ v.description }}</span>
                  <span v-if="v.default" class="pp-var-default mono">default: {{ v.default }}</span>
                </li>
              </ul>
            </div>
          </template>
        </template>

        <!-- idle: 尚未拉取(打开弹窗不自动拉取,等用户确认参数) -->
        <div v-else class="pp-banner pp-banner--idle" role="status">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg>
          <span>{{ t('projectPipeline.pacPreviewIdle') }}</span>
        </div>
      </div>

      <footer class="pp-foot">
        <button class="pp-btn" @click="emit('close')">{{ t('projectPipeline.pacPreviewCloseBtn') }}</button>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.pp-backdrop {
  position: fixed;
  inset: 0;
  z-index: 60;
  background: oklch(0% 0 0 / 0.42);
  backdrop-filter: blur(2px);
  display: grid;
  place-items: center;
  padding: 24px;
}

.pp-modal {
  width: min(800px, 100%);
  max-height: min(86vh, 720px);
  display: flex;
  flex-direction: column;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-card);
  box-shadow: 0 24px 64px oklch(0% 0 0 / 0.36);
  overflow: hidden;
}

.pp-head {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 18px 20px 14px;
  border-bottom: 1px solid var(--color-border);
}

.pp-title {
  font-size: 1.02rem;
  font-weight: 700;
  color: var(--color-text);
  letter-spacing: -0.01em;
}

.pp-sub {
  margin-top: 4px;
  font-size: 0.8rem;
  color: var(--color-faint);
  line-height: 1.5;
}

.pp-sub code {
  font-family: var(--font-mono, monospace);
  background: var(--color-inset);
  padding: 1px 5px;
  border-radius: 4px;
  font-size: 0.92em;
}

.pp-close {
  margin-left: auto;
  flex: none;
  width: 28px;
  height: 28px;
  display: grid;
  place-items: center;
  border: none;
  background: none;
  color: var(--color-faint);
  border-radius: 6px;
  cursor: pointer;
  transition: color var(--duration-fast), background-color var(--duration-fast);
}

.pp-close:hover { color: var(--color-text); background: var(--color-inset); }
.pp-close:focus-visible { outline: 2px solid var(--color-primary); outline-offset: 2px; }

.pp-body {
  padding: 16px 20px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.pp-ref-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.pp-label {
  font-size: 0.8rem;
  font-weight: 600;
  color: var(--color-dim);
  flex: none;
}

.pp-ref-input {
  flex: 1;
  height: 34px;
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 0.82rem;
  color: var(--color-text);
  background: var(--color-canvas, var(--color-inset));
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-md);
  padding: 0 12px;
  min-width: 0;
}

.pp-ref-input--ref { flex: 0.8; }

.pp-ref-input:focus { outline: none; border-color: var(--color-primary); box-shadow: 0 0 0 3px var(--color-primary-soft); }
.pp-ref-input:disabled { opacity: 0.6; }

.pp-file-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.pp-file-note { font-size: 0.76rem; }
.pp-file-note--ok { color: var(--color-green); }
.pp-file-note--err { color: var(--color-red); }

.pp-banner {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  font-size: 0.8rem;
  line-height: 1.5;
  padding: 9px 12px;
  border-radius: var(--rounded-md);
  border: 1px solid transparent;
}

.pp-banner--error {
  background: var(--color-red-soft);
  border-color: var(--color-red-line);
  color: var(--color-red);
}

.pp-banner--ok {
  background: var(--color-green-soft);
  border-color: var(--color-green-line);
  color: var(--color-green);
}

.pp-banner--warn {
  background: var(--color-amber-soft, var(--color-inset));
  border-color: var(--color-amber-line, var(--color-border-strong));
  color: var(--color-amber, var(--color-dim));
}

.pp-banner--idle {
  background: var(--color-inset);
  border-color: var(--color-border);
  color: var(--color-dim);
}

.pp-banner svg { flex: none; margin-top: 2px; }

.pp-error-detail {
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 0.76rem;
  line-height: 1.5;
  color: var(--color-red);
  background: var(--color-red-soft);
  border: 1px solid var(--color-red-line);
  border-radius: var(--rounded-md);
  padding: 10px 12px;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 200px;
  overflow-y: auto;
}

.pp-stages {
  display: flex;
  flex-direction: column;
  gap: 6px;
  list-style: none;
  margin: 0;
  padding: 0;
}

.pp-stage {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 12px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-md);
  font-size: 0.82rem;
}

.pp-stage-name {
  font-weight: 600;
  color: var(--color-text);
}

.pp-stage-kind {
  font-family: var(--font-mono, monospace);
  font-size: 0.72rem;
  color: var(--color-cyan);
  background: var(--color-card-2);
  border: 1px solid var(--color-border);
  border-radius: 4px;
  padding: 1px 6px;
}

.pp-stage-jobs {
  margin-left: auto;
  font-size: 0.76rem;
  color: var(--color-faint);
}

/* ─── Declared variables ──────────────────────────────────────────────────── */
.pp-vars {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-md);
  padding: 12px 14px;
  background: var(--color-card-2);
}

.pp-vars-title {
  font-size: 0.84rem;
  font-weight: 600;
  color: var(--color-text);
  margin: 0 0 4px;
}

.pp-vars-hint {
  font-size: 0.73rem;
  color: var(--color-faint);
  margin: 0 0 10px;
  line-height: 1.5;
}

.pp-vars-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  list-style: none;
  margin: 0;
  padding: 0;
}

.pp-var {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-md);
  font-size: 0.78rem;
  flex-wrap: wrap;
}

.pp-var-name {
  font-weight: 600;
  color: var(--color-text);
}

.pp-var-desc {
  color: var(--color-dim);
  flex: 1;
  min-width: 120px;
}

.pp-var-default {
  font-size: 0.72rem;
  color: var(--color-faint);
}

.pp-badge {
  flex: none;
  font-size: 0.66rem;
  font-weight: 600;
  border-radius: 4px;
  padding: 1px 6px;
  border: 1px solid transparent;
}

.pp-badge--req {
  color: var(--color-amber, var(--color-dim));
  background: var(--color-amber-soft, var(--color-inset));
  border-color: var(--color-amber-line, var(--color-border));
}

.pp-badge--sec {
  color: var(--color-cyan);
  background: var(--color-cyan-soft);
  border-color: var(--color-cyan-line);
}

.mono { font-family: var(--font-mono, monospace); }

.pp-foot {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 10px;
  padding: 14px 20px;
  border-top: 1px solid var(--color-border);
}

.pp-btn {
  height: 34px;
  font-size: 0.83rem;
  font-weight: 500;
  font-family: var(--font-sans);
  border: 1px solid var(--color-border-strong);
  background: var(--color-card-2);
  color: var(--color-text);
  border-radius: var(--rounded);
  padding: 0 14px;
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  gap: 7px;
  transition: border-color var(--duration-fast), background-color var(--duration-fast);
}

.pp-btn:hover:not(:disabled) { border-color: var(--color-faint); }
.pp-btn:focus-visible { outline: 2px solid var(--color-primary); outline-offset: 2px; }
.pp-btn:disabled { opacity: 0.45; cursor: not-allowed; }

.pp-btn--primary {
  background: var(--color-primary);
  color: #fff;
  border-color: transparent;
  font-weight: 600;
  flex: none;
}

.pp-btn--primary:hover:not(:disabled) { background: var(--color-primary-press); }

.pp-btn--ghost {
  background: transparent;
  border-style: dashed;
  color: var(--color-primary);
}

.pp-btn--ghost:hover:not(:disabled) { background: var(--color-primary-soft); }

.pp-spin {
  display: inline-block;
  width: 12px;
  height: 12px;
  border: 2px solid rgba(255, 255, 255, 0.35);
  border-top-color: currentColor;
  border-radius: var(--rounded-full);
  animation: pp-spin 0.7s linear infinite;
}

@keyframes pp-spin { to { transform: rotate(360deg); } }

@media (prefers-reduced-motion: reduce) {
  .pp-spin { animation: none; }
}
</style>
