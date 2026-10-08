<template>
  <div
    v-if="failedSteps.length > 0"
    class="resume-panel"
    role="region"
    :aria-label="t('runDetail.resumeRegionAria')"
  >
    <div class="resume-head">
      <div class="resume-icon" aria-hidden="true">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v6h-6"/>
        </svg>
      </div>
      <div class="resume-body">
        <div class="resume-title">{{ t('runDetail.resumeTitle') }}</div>
        <div class="resume-sub">{{ t('runDetail.resumeDesc') }}</div>
      </div>
      <button
        class="resume-btn"
        :disabled="submitting"
        :aria-busy="submitting"
        @click="handleResume"
      >
        <span v-if="submitting" class="spinner" aria-hidden="true" />
        <svg v-else width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
          <polygon points="5 3 19 12 5 21 5 3"/>
        </svg>
        {{ submitting ? t('runDetail.resumeLaunching') : t('runDetail.resumeLaunch') }}
      </button>
    </div>

    <div v-if="error" class="banner banner--error" role="alert">{{ error }}</div>

    <ul class="resume-nodes">
      <li v-for="fs in failedSteps" :key="fs.ordinal" class="resume-node">
        <div class="resume-node-info">
          <span class="resume-node-name">{{ fs.step.name }}</span>
          <span v-if="fs.step.stage" class="resume-node-stage">{{ fs.step.stage }}</span>
        </div>
        <div class="resume-node-actions" role="group" :aria-label="fs.step.name">
          <button
            type="button"
            class="resume-action"
            :class="{ 'resume-action--retry-active': actions[fs.ordinal] === 'retry' }"
            :aria-pressed="actions[fs.ordinal] === 'retry'"
            @click="setAction(fs.ordinal, 'retry')"
          >
            {{ t('runDetail.resumeRetry') }}
          </button>
          <button
            type="button"
            class="resume-action"
            :class="{ 'resume-action--skip-active': actions[fs.ordinal] === 'skip' }"
            :aria-pressed="actions[fs.ordinal] === 'skip'"
            :title="t('runDetail.resumeSkipHint')"
            @click="setAction(fs.ordinal, 'skip')"
          >
            {{ t('runDetail.resumeSkip') }}
          </button>
        </div>
      </li>
    </ul>
  </div>
</template>

<script setup lang="ts">
/**
 * RunResumePanel — 失败运行「按节点恢复」面板(失败/部分失败状态挂载)。
 *
 * 列出全部失败节点,逐节点选择 重试 / 跳过(默认重试),一键创建派生运行:
 * 已成功节点继承为成功不重跑;跳过节点的下游照常执行;部署节点在派生运行里
 * 只重试失败的机器(服务端增量语义)。成功后跳转子运行详情。
 * 服务端 409 spec_changed → 明确提示配置已变化,无法按节点恢复。
 */
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { resumeRun, type ResumeAction, type RunStep } from '../../api/runs'
import { HttpError } from '../../api/http'

const props = defineProps<{ runId: string; steps: RunStep[] }>()
const { t } = useI18n()
const router = useRouter()

// 失败节点 + 其 ordinal(新响应带 ordinal;老数据回退数组下标,与后端 run_steps.ordinal 同序)。
const failedSteps = computed(() => {
  const out: { ordinal: number; step: RunStep }[] = []
  props.steps.forEach((step, index) => {
    if (step.status === 'failed') out.push({ ordinal: step.ordinal ?? index, step })
  })
  return out
})

// 逐节点处置(默认全部重试;新增失败节点自动补默认值)。
const actions = reactive<Record<number, ResumeAction>>({})
watch(
  () => failedSteps.value.map((f) => f.ordinal).join(','),
  () => {
    for (const f of failedSteps.value) {
      if (actions[f.ordinal] !== 'retry' && actions[f.ordinal] !== 'skip') {
        actions[f.ordinal] = 'retry'
      }
    }
  },
  { immediate: true },
)

function setAction(ordinal: number, action: ResumeAction) {
  actions[ordinal] = action
}

const submitting = ref(false)
const error = ref('')

async function handleResume() {
  if (submitting.value) return
  submitting.value = true
  error.value = ''
  try {
    const child = await resumeRun(props.runId, { ...actions })
    await router.push(`/runs/${child.id}`)
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    submitting.value = false
  }
}

function errorMessage(err: unknown): string {
  if (err instanceof HttpError) {
    switch (err.apiError?.code) {
      case 'spec_changed':
        return t('runDetail.resumeSpecChanged')
      case 'run_not_resumable':
      case 'no_failed_nodes':
      case 'invalid_resume_plan':
        return err.apiError?.message || t('runDetail.resumeNotResumable')
      case 'run_not_found':
        return t('runDetail.resumeRunNotFound')
      case 'run_queue_full':
        return t('runDetail.resumeQueueFull')
      default:
        return err.apiError?.message || t('runDetail.resumeRequestFailed', { status: err.status })
    }
  }
  return t('runDetail.resumeRequestFailed', { status: '' })
}
</script>

<style scoped>
.resume-panel {
  border: 1px solid var(--color-amber-line);
  background: var(--color-amber-soft);
  border-radius: var(--rounded-md);
  padding: 14px 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.resume-head {
  display: flex;
  align-items: center;
  gap: 12px;
}

.resume-icon {
  flex: none;
  width: 34px;
  height: 34px;
  border-radius: 50%;
  display: grid;
  place-items: center;
  color: var(--color-amber);
  background: var(--color-amber-soft);
  border: 1px solid var(--color-amber-line);
}

.resume-body {
  flex: 1;
  min-width: 0;
}

.resume-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text);
}

.resume-sub {
  font-size: 12px;
  color: var(--color-text-muted);
  margin-top: 2px;
}

.resume-btn {
  flex: none;
  display: inline-flex;
  align-items: center;
  gap: 7px;
  padding: 7px 14px;
  border: 1px solid var(--color-amber-line);
  border-radius: var(--rounded-md);
  background: var(--color-amber);
  color: #fff;
  font-size: 12.5px;
  font-weight: 600;
  cursor: pointer;
  transition: opacity var(--duration-fast) ease;
}

.resume-btn:hover:not(:disabled) {
  opacity: 0.9;
}

.resume-btn:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}

.resume-nodes {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.resume-node {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 8px 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-md);
  background: var(--color-bg);
}

.resume-node-info {
  display: flex;
  align-items: baseline;
  gap: 8px;
  min-width: 0;
}

.resume-node-name {
  font-family: var(--font-mono);
  font-size: 12.5px;
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.resume-node-stage {
  flex: none;
  font-size: 11px;
  color: var(--color-text-muted);
}

.resume-node-actions {
  flex: none;
  display: inline-flex;
  gap: 6px;
}

.resume-action {
  padding: 4px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-md);
  background: transparent;
  color: var(--color-text-muted);
  font-size: 12px;
  cursor: pointer;
  transition: all var(--duration-fast) ease;
}

.resume-action--retry-active {
  border-color: var(--color-green);
  background: var(--color-green-soft);
  color: var(--color-green);
  font-weight: 600;
}

.resume-action--skip-active {
  border-color: var(--color-amber);
  background: var(--color-amber-soft);
  color: var(--color-amber);
  font-weight: 600;
}
</style>
