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
            :class="{ 'resume-action--busy': pendingOrdinal === fs.ordinal }"
            :disabled="submitting"
            :aria-busy="pendingOrdinal === fs.ordinal"
            @click="handleNodeAction(fs.ordinal, 'retry')"
          >
            <span v-if="pendingOrdinal === fs.ordinal" class="spinner" aria-hidden="true" />
            {{ pendingOrdinal === fs.ordinal ? t('runDetail.resumeLaunching') : t('runDetail.resumeRetry') }}
          </button>
          <button
            type="button"
            class="resume-action"
            :disabled="submitting"
            :title="t('runDetail.resumeSkipHint')"
            @click="handleNodeAction(fs.ordinal, 'skip')"
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
 * RunResumePanel — 失败运行「按节点恢复」面板(失败/部分失败状态挂载),**一步式**交互:
 *
 * 点击某节点的「重试 / 跳过」立即创建派生运行并跳转——
 *   - 点击的节点按所选处置执行(重试=真实重跑 / 跳过=标记跳过放行下游);
 *   - 其余失败节点默认重试;已成功节点继承为成功不重跑;
 *   - 部署节点在派生运行里只重试失败的机器(服务端增量语义)。
 * 父运行历史保持不动;失败(如 409 spec_changed)时面板内出错误横幅,可再次点击。
 */
import { computed, ref } from 'vue'
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

const submitting = ref(false)
const pendingOrdinal = ref<number | null>(null)
const error = ref('')

async function handleNodeAction(ordinal: number, action: ResumeAction) {
  if (submitting.value) return
  submitting.value = true
  pendingOrdinal.value = ordinal
  error.value = ''
  // 处置计划:点击节点按所选动作,其余失败节点默认重试。
  const actions: Record<number, ResumeAction> = {}
  for (const f of failedSteps.value) {
    actions[f.ordinal] = f.ordinal === ordinal ? action : 'retry'
  }
  try {
    const child = await resumeRun(props.runId, actions)
    // 成功即跳转子运行详情;保持 submitting 禁用按钮防重复点击(组件随路由卸载)。
    await router.push(`/runs/${child.id}`)
  } catch (err) {
    error.value = errorMessage(err)
    submitting.value = false
    pendingOrdinal.value = null
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
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-md);
  background: transparent;
  color: var(--color-text-muted);
  font-size: 12px;
  cursor: pointer;
  transition: all var(--duration-fast) ease;
}

/* 悬停预览所选动作的主色:重试=绿 / 跳过=amber。 */
.resume-action:first-child:not(:disabled):hover {
  border-color: var(--color-green);
  background: var(--color-green-soft);
  color: var(--color-green);
}

.resume-action:last-child:not(:disabled):hover {
  border-color: var(--color-amber);
  background: var(--color-amber-soft);
  color: var(--color-amber);
}

.resume-action:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}

/* 提交中被点击的按钮呈重试主色,提示「这一下正在创建派生运行」。 */
.resume-action--busy {
  border-color: var(--color-green);
  background: var(--color-green-soft);
  color: var(--color-green);
  font-weight: 600;
}

/* 本组件不在 RunDetail 的 scoped 作用域内,自带 spinner(1em 圈,currentColor)。 */
.spinner {
  width: 11px;
  height: 11px;
  flex: none;
  border: 2px solid currentColor;
  border-top-color: transparent;
  border-radius: 50%;
  animation: resume-spin 0.7s linear infinite;
}

@keyframes resume-spin {
  to {
    transform: rotate(360deg);
  }
}

@media (prefers-reduced-motion: reduce) {
  .spinner {
    animation: none;
  }
}
</style>
