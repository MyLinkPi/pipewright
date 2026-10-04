<script setup lang="ts">
/**
 * StageDrawer — 阶段级设置的右侧内联检视面板。
 *
 * 复用与 JobDrawer 完全相同的 `.job-drawer` / `.drawer-*` 卡片骨架(来自 pipeline.css):
 * 同一右侧槽位、同样的滚动容器与输入样式,与「点 job 弹出的配置卡」一致 —— 而非另起
 * 一个覆盖式弹层。承载阶段的 条件(when)/ 审批门(gate)/ 矩阵(matrix)/ 旁挂服务
 * (services)/ 后置步骤(post)五块。字段编辑逻辑(分支解析、事件开关、矩阵解析)在此,
 * 经 update-* 事件回传 PipelineCanvas → updateStage 落库。
 */
import { computed, ref, watch, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { localizeName } from '../../lib/pipelineLabels'
import type { PipelineStage, StageWhen, PipelinePostStep, PipelineServiceSpec, StageKind } from '../../api/pipeline'
import type { Server } from '../../api/servers'
import { matchServers } from '../../lib/selectorMatch'
import StagePostEditor from './StagePostEditor.vue'
import StageServicesEditor from './StageServicesEditor.vue'
import LabelSelectorEditor from './LabelSelectorEditor.vue'
import {
  WHEN_EVENTS,
  WHEN_EVENT_LABELS,
  parseBranches,
  branchesToText,
  toggleWhenEvent,
  normalizeWhen,
  parseMatrix,
  matrixToText,
  matrixError,
  matrixSummary,
  type WhenEvent,
} from './stageSettings'
import './pipeline.css'

const props = defineProps<{
  stage: PipelineStage
  stageIndex: number
  /** 阶段总数(默认名提示用;可选)。 */
  stageCount?: number
  /** 服务器池(构建机选择的候选与标签来源;构建机预览用)。 */
  servers?: Server[]
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'update-name', name: string): void
  (e: 'update-kind', kind: StageKind): void
  (e: 'update-when', when: StageWhen | undefined): void
  (e: 'update-gate', value: boolean): void
  (e: 'update-matrix', matrix: Record<string, string[]> | undefined): void
  (e: 'update-post', post: PipelinePostStep[] | undefined): void
  (e: 'update-services', services: PipelineServiceSpec[] | undefined): void
  (e: 'update-runner', runner: string | undefined): void
}>()

const { t } = useI18n()

// ─── 阶段名称 / kind(阶段可自由命名、自由加;不再「每类一个」)────────────────

const localName = ref(props.stage.name)
const localKind = ref<StageKind>((props.stage.kind === 'source' ? 'custom' : props.stage.kind) as StageKind)

watch(
  () => [props.stage.id, props.stage.name, props.stage.kind],
  () => {
    localName.value = props.stage.name
    localKind.value = (props.stage.kind === 'source' ? 'custom' : props.stage.kind) as StageKind
  },
)

/** 打开抽屉时聚焦名称输入(画布「添加阶段 → 自动打开设置」流程依赖)。 */
const nameInput = ref<HTMLInputElement | null>(null)
watch(
  () => props.stage.id,
  () => {
    void nextTick(() => nameInput.value?.focus())
  },
)

const KIND_OPTIONS: Array<{ value: StageKind; labelKey: string }> = [
  { value: 'build', labelKey: 'pipelineCanvas.stageKindBuild' },
  { value: 'deploy', labelKey: 'pipelineCanvas.stageKindDeploy' },
  { value: 'notify', labelKey: 'pipelineCanvas.stageKindNotify' },
  { value: 'custom', labelKey: 'pipelineCanvas.stageKindCustom' },
]

function commitName(): void {
  const v = localName.value.trim()
  if (v && v !== props.stage.name) emit('update-name', v)
  else localName.value = props.stage.name
}

function commitKind(kind: StageKind): void {
  localKind.value = kind
  if (kind !== props.stage.kind) emit('update-kind', kind)
}

const branchText = computed<string>(() => branchesToText(props.stage.when?.branches))
const currentEvents = computed<string[]>(() => props.stage.when?.events ?? [])

const matrixText = computed<string>(() => matrixToText(props.stage.matrix))
const matrixWarn = computed(() => matrixError(props.stage.matrix))
const matrixChip = computed(() => matrixSummary(props.stage.matrix))

function commitBranches(text: string): void {
  emit('update-when', normalizeWhen(parseBranches(text), currentEvents.value))
}
function toggleEvent(ev: WhenEvent): void {
  const events = toggleWhenEvent(currentEvents.value, ev)
  emit('update-when', normalizeWhen(props.stage.when?.branches ?? [], events))
}
function commitMatrix(text: string): void {
  emit('update-matrix', parseMatrix(text))
}

// ─── 构建机选择(RUNNER · FR-8-19)────────────────────────────────────────────
// 节点级配置:本阶段的构建跑在哪台机器上。stage.runner 非空时覆盖项目默认选择器,
// 空 = 跟随项目默认(流水线页「构建配置」tab)。标签模式 = AND 匹配构建机池,
// 钉死模式 = server:<id> 单机;调度侧按「优先级 → 本流水线亲和 → 负载」选机。

type RunnerMode = 'inherit' | 'label' | 'pinned'

const runnerMode = ref<RunnerMode>('inherit')
const runnerSelectorText = ref('')
const runnerServerId = ref('')

// 切换阶段(抽屉复用)或 stage.runner 外部回写时,从 props 重解本地三态。
watch(
  () => [props.stage.id, props.stage.runner] as const,
  () => {
    const v = (props.stage.runner ?? '').trim()
    if (v === '') {
      runnerMode.value = 'inherit'
      runnerSelectorText.value = ''
      runnerServerId.value = ''
    } else if (v.startsWith('server:')) {
      runnerMode.value = 'pinned'
      runnerServerId.value = v.slice('server:'.length)
      runnerSelectorText.value = ''
    } else {
      runnerMode.value = 'label'
      runnerServerId.value = ''
      runnerSelectorText.value = v
    }
  },
  { immediate: true },
)

/** 标签模式选择器的实时命中预览(与服务端 runner 域同一语义;权威裁决在服务端)。 */
const runnerMatched = computed(() => matchServers(runnerSelectorText.value, props.servers ?? []))

function commitRunnerMode(mode: RunnerMode): void {
  if (mode === 'inherit') emit('update-runner', undefined)
  else if (mode === 'label') emit('update-runner', runnerSelectorText.value.trim() || undefined)
  else emit('update-runner', runnerServerId.value !== '' ? `server:${runnerServerId.value}` : undefined)
}

function commitRunnerSelector(value: string): void {
  runnerSelectorText.value = value
  const v = value.trim()
  emit('update-runner', v === '' ? undefined : v)
}

function commitRunnerServer(id: string): void {
  runnerServerId.value = id
  if (id !== '') emit('update-runner', `server:${id}`)
}
</script>

<template>
  <aside class="job-drawer" :aria-label="t('pipelineCanvas.stageSettingsAria')">
    <!-- Head — 与 JobDrawer 同骨架 -->
    <div class="drawer-head">
      <span class="drawer-icon" aria-hidden="true">
        <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
          <circle cx="12" cy="12" r="3"/>
          <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>
        </svg>
      </span>
      <div class="drawer-title">
        {{ localizeName(stage.name) }}
        <small class="drawer-subtitle">{{ t('pipelineCanvas.stageSettingsSub', { n: stageIndex }) }}</small>
      </div>
      <button class="drawer-close" :aria-label="t('pipelineCanvas.closeSettings')" @click="emit('close')">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
          <path d="M18 6 6 18M6 6l12 12"/>
        </svg>
      </button>
    </div>

    <!-- 阶段名称 / 类型 -->
    <div class="drawer-section">
      <div class="drawer-section-label">{{ t('pipelineCanvas.stageIdentitySection') }}</div>
      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineCanvas.stageNameLabel') }}</div>
        <input
          ref="nameInput"
          v-model="localName"
          class="drawer-input"
          type="text"
          :placeholder="t('pipelineCanvas.stageNamePlaceholder')"
          :aria-label="t('pipelineCanvas.stageNameLabel')"
          @blur="commitName"
          @keydown.enter.prevent="commitName"
        />
      </div>
      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineCanvas.stageKindLabel') }}</div>
        <select
          v-model="localKind"
          class="drawer-select"
          :aria-label="t('pipelineCanvas.stageKindLabel')"
          @change="commitKind(($event.target as HTMLSelectElement).value as StageKind)"
        >
          <option v-for="opt in KIND_OPTIONS" :key="opt.value" :value="opt.value">
            {{ t(opt.labelKey) }}
          </option>
        </select>
      </div>
      <p class="drawer-hint">{{ t('pipelineCanvas.stageIdentityHint') }}</p>
    </div>

    <!-- 条件执行(WHEN) -->
    <div class="drawer-section">
      <div class="drawer-section-label">{{ t('pipelineCanvas.whenSectionLabel') }}</div>
      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineCanvas.branchesLabel') }}</div>
        <input
          class="drawer-input"
          type="text"
          :value="branchText"
          :placeholder="t('pipelineCanvas.branchesPlaceholder')"
          :aria-label="t('pipelineCanvas.branchesAria')"
          @change="commitBranches(($event.target as HTMLInputElement).value)"
        />
      </div>
      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineCanvas.eventsLabel') }}</div>
        <div class="drawer-events">
          <label v-for="ev in WHEN_EVENTS" :key="ev" class="drawer-event">
            <input type="checkbox" :checked="currentEvents.includes(ev)" @change="toggleEvent(ev)" />
            <span>{{ WHEN_EVENT_LABELS[ev] }}</span>
          </label>
        </div>
      </div>
      <p class="drawer-hint">{{ t('pipelineCanvas.whenHint') }}</p>
    </div>

    <!-- 构建机选择(RUNNER · FR-8-19):本节点的构建跑在哪台机器上 -->
    <div class="drawer-section">
      <div class="drawer-section-label">{{ t('pipelineCanvas.runnerSectionLabel') }}</div>
      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineCanvas.runnerModeLabel') }}</div>
        <select
          v-model="runnerMode"
          class="drawer-select"
          :aria-label="t('pipelineCanvas.runnerModeLabel')"
          @change="commitRunnerMode(runnerMode)"
        >
          <option value="inherit">{{ t('pipelineCanvas.runnerModeInherit') }}</option>
          <option value="label">{{ t('pipelineCanvas.runnerModePool') }}</option>
          <option value="pinned">{{ t('pipelineCanvas.runnerModePinned') }}</option>
        </select>
      </div>
      <div v-if="runnerMode === 'label'" class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineCanvas.runnerLabel') }}</div>
        <LabelSelectorEditor
          :model-value="runnerSelectorText"
          :servers="props.servers"
          @update:model-value="commitRunnerSelector"
        />
        <p
          v-if="runnerSelectorText.trim()"
          class="drawer-hint"
          :class="{ 'runner-match--none': runnerMatched.length === 0 }"
          role="status"
        >
          {{ runnerMatched.length === 0
            ? t('pipelineCanvas.runnerNoMatch')
            : t('pipelineCanvas.runnerMatchCount', { n: runnerMatched.length }) }}
          <span v-if="runnerMatched.length"> — {{ runnerMatched.map((s) => s.name).join(', ') }}</span>
        </p>
      </div>
      <div v-else-if="runnerMode === 'pinned'" class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineCanvas.runnerServerLabel') }}</div>
        <select
          class="drawer-select"
          :value="runnerServerId"
          :aria-label="t('pipelineCanvas.runnerServerLabel')"
          @change="commitRunnerServer(($event.target as HTMLSelectElement).value)"
        >
          <option value="" disabled>{{ t('pipelineCanvas.runnerServerPick') }}</option>
          <option v-for="s in props.servers ?? []" :key="s.id" :value="s.id">
            {{ s.name }}({{ s.host }})
          </option>
          <option
            v-if="runnerServerId !== '' && !(props.servers ?? []).some((s) => s.id === runnerServerId)"
            :value="runnerServerId"
          >{{ runnerServerId }}</option>
        </select>
      </div>
      <p class="drawer-hint">{{ t('pipelineCanvas.runnerHint') }}</p>
    </div>

    <!-- 审批门(GATE) -->
    <div class="drawer-section">
      <div class="drawer-section-label">{{ t('pipelineCanvas.gateSectionLabel') }}</div>
      <label class="drawer-toggle">
        <input
          type="checkbox"
          :checked="stage.gate === true"
          @change="emit('update-gate', ($event.target as HTMLInputElement).checked)"
        />
        <span>{{ t('pipelineCanvas.gateToggle') }}</span>
      </label>
      <p class="drawer-hint">{{ t('pipelineCanvas.gateHint') }}</p>
    </div>

    <!-- 矩阵构建(MATRIX) -->
    <div class="drawer-section">
      <div class="drawer-section-head">
        <div class="drawer-section-label">{{ t('pipelineCanvas.matrixSectionLabel') }}</div>
        <span v-if="matrixChip" class="drawer-section-badge">{{ matrixChip }}</span>
      </div>
      <div class="drawer-field">
        <div class="drawer-field-label">
          <i18n-t keypath="pipelineCanvas.matrixAxisLabel" tag="span" scope="global">
            <template #code><code>{{ t('pipelineCanvas.matrixAxisCode') }}</code></template>
          </i18n-t>
        </div>
        <textarea
          class="drawer-input drawer-textarea is-mono"
          rows="3"
          :value="matrixText"
          placeholder="go: 1.21, 1.22&#10;os: linux"
          :aria-label="t('pipelineCanvas.matrixAria')"
          @change="commitMatrix(($event.target as HTMLTextAreaElement).value)"
        ></textarea>
      </div>
      <p v-if="matrixWarn" class="drawer-warn" role="alert">{{ matrixWarn }}</p>
      <p class="drawer-hint">
        <i18n-t keypath="pipelineCanvas.matrixHint" tag="span" scope="global">
          <template #code><code>{{ t('pipelineCanvas.matrixHintCode') }}</code></template>
        </i18n-t>
      </p>
    </div>

    <!-- 旁挂服务(SERVICES) -->
    <div class="drawer-section">
      <div class="drawer-section-label">{{ t('pipelineCanvas.servicesSectionLabel') }}</div>
      <StageServicesEditor
        :services="stage.services"
        :stage-id="stage.id"
        @update="emit('update-services', $event)"
      />
      <p class="drawer-hint">
        <i18n-t keypath="pipelineCanvas.servicesHint" tag="span" scope="global">
          <template #code><code>psql -h testdb</code></template>
        </i18n-t>
      </p>
    </div>

    <!-- 后置步骤(POST) -->
    <div class="drawer-section">
      <div class="drawer-section-label">{{ t('pipelineCanvas.postSectionLabel') }}</div>
      <StagePostEditor
        :steps="stage.post"
        :stage-id="stage.id"
        @update="emit('update-post', $event)"
      />
      <p class="drawer-hint">{{ t('pipelineCanvas.postHint') }}</p>
    </div>
  </aside>
</template>

<style scoped>
.drawer-section-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  margin-bottom: 9px;
}
.drawer-section-head .drawer-section-label { margin-bottom: 0; }
.drawer-section-badge {
  font-size: 0.66rem;
  font-weight: 700;
  padding: 2px 8px;
  border-radius: 100px;
  background: var(--color-violet-soft, rgba(124, 58, 237, 0.13));
  color: var(--color-violet, #6d28d9);
  white-space: nowrap;
}

.drawer-textarea {
  height: auto;
  min-height: 78px;
  padding: 9px 11px;
  line-height: 1.5;
  resize: vertical;
}
.is-mono { font-family: var(--font-mono, ui-monospace, monospace); font-size: 0.8rem; }

.drawer-events { display: flex; flex-wrap: wrap; gap: 8px 16px; }
.drawer-event {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 0.82rem;
  color: var(--color-text);
  cursor: pointer;
}
.drawer-event input { width: 15px; height: 15px; accent-color: var(--color-primary); cursor: pointer; }

.drawer-warn { margin: 7px 0 0; font-size: 0.72rem; color: var(--color-danger, #dc2626); line-height: 1.45; }
.runner-match--none { color: var(--color-danger, #dc2626); }

.drawer-hint { margin: 9px 0 0; font-size: 0.72rem; color: var(--color-faint); line-height: 1.5; }
.drawer-hint code,
.drawer-field-label code {
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 0.92em;
  padding: 0 3px;
  border-radius: 3px;
  background: var(--color-inset);
  color: var(--color-dim);
}
</style>
