<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { localizeName } from '../../lib/pipelineLabels'
import type { PipelineJob, PipelineStage } from '../../api/pipeline'
import type { Credential } from '../../api/credentials'
import type { Server } from '../../api/servers'
import type { NotificationChannel } from '../../api/notifications'
import {
  getJobTypeSpec,
  splitConfig,
  jobTypeLabel,
  isScriptClassType,
  type JobField,
} from './jobConfigSchema'
import { configUsesTemplate } from './stepCompile'
import {
  isStudioNode,
  parsePromotedParams,
  promotedValues,
  applyPromotedValues,
  type PromotedParam,
} from './studioCompile'
import { createCustomNode } from '../../api/customNodes'
import { matchServers } from '../../lib/selectorMatch'
import JobTypeIcon from './JobTypeIcon.vue'
import StepBuilder from './StepBuilder.vue'
import StudioInstanceParams from './StudioInstanceParams.vue'
import LabelSelectorEditor from './LabelSelectorEditor.vue'

const props = defineProps<{
  job: PipelineJob
  stage: PipelineStage
  credentials?: Credential[]
  servers?: Server[]
  channels?: NotificationChannel[]
  /** 全流水线的产物来源候选(部署节点「产物来源」下拉;画布按流水线顺序传入)。 */
  producerOptions?: ProducerOption[]
}>()

/** 产物来源候选:job 名(即产物元数据 sourceJob)+ 类型 + 所在阶段名。 */
export interface ProducerOption {
  name: string
  type: string
  stageName: string
}

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'update', patch: Partial<PipelineJob>): void
  (e: 'change-type'): void
}>()

const { t } = useI18n()

// ─── Local editable copy ──────────────────────────────────────────────────────

interface KVRow {
  _key: number
  k: string
  v: string
}

let _kvSeq = 0

const localName    = ref(props.job.name)
const localType    = ref(props.job.type)
const localSummary = ref(props.job.summary)
/** Schema-owned keys for the current type (the typed form binds here) */
const typedConfig  = ref<Record<string, string>>({})
/** Keys NOT covered by the type schema — editable in the advanced section */
const extraRows    = ref<KVRow[]>([])

const showAdvanced = ref(false)

// 仅当「选了不同 job」或「改了 job 类型」时才重置视图模式(steps/raw);纯内容编辑不重置。
// 否则:在「原始参数」视图编辑某字段 → blur 提交 → 父回写 props.job → 本 watch 再跑 →
// 会把视图无端切回「可视化步骤」(用户实测的 bug)。
let lastModeKey = ''

function hydrate(job: PipelineJob): void {
  localName.value    = job.name
  localType.value    = job.type
  localSummary.value = job.summary
  const modeKey = `${job.id} ${job.type}`
  const repickView = modeKey !== lastModeKey
  lastModeKey = modeKey
  splitOnType(job.type, job.config ?? {}, repickView)
  resyncRunnerState()
}

/** Recompute typed config + raw extras for a given type, preserving all values. */
function splitOnType(type: string, config: Record<string, string>, repickView = true): void {
  const { extras } = splitConfig(type, config)
  const typed: Record<string, string> = {}
  for (const [k, v] of Object.entries(config)) {
    if (!extras.some(([ek]) => ek === k)) typed[k] = v
  }
  typedConfig.value = typed
  extraRows.value = extras.map(([k, v]) => ({ _key: ++_kvSeq, k, v }))
  showAdvanced.value = extras.length > 0
  if (repickView) pickViewMode(config)
}

/** 规范化内容键:区分「自己 flush 后父回写的回声」与「真正的外部变更」。 */
function jobContentKey(
  name: string,
  type: string,
  summary: string | undefined,
  config: Record<string, string> | undefined,
): string {
  const cfg = config ?? {}
  const sorted = Object.keys(cfg).sort().map((k) => [k, cfg[k] ?? ''] as const)
  return JSON.stringify({ name, type, summary: summary ?? '', config: sorted })
}

// Resync when the selected job changes (initial hydrate runs after the
// step-builder computeds below are declared, to avoid a temporal dead zone).
//
// flush → 画布 updateJob 以新对象回写 props.job。若照单全收 hydrate,splitOnType 会给
// 全部 extraRows 换新 _key,高级区 KV 输入框被整体重建 —— blur 提交的瞬间点向下一个
// 输入框/删除按钮会落空(焦点被吞、click 丢失)。内容与本地当前组合一致(即回声)时
// 跳过;仅真正外部变更(切换 job / 换类型 / 保存回填)才全量重建。
watch(() => props.job, (next, prev) => {
  const incoming = jobContentKey(next.name, next.type, next.summary, next.config)
  // 空值回退用 prev(回调时 props.job 已是新值):若回退新值,本地名称/类型恰被清空时
  // 外部仅改名/改类型会被误判为回声而跳过 hydrate;回退 prev 则回声(next===prev 的相关
  // 分量)仍可识别,真外部变更也能触发重建。flush 侧回退仍用当时的 props.job(语义:空 = 沿用当前值)。
  const current = jobContentKey(
    localName.value.trim() || prev.name,
    localType.value.trim() || prev.type,
    localSummary.value,
    currentConfig(),
  )
  if (incoming === current) return
  hydrate(next)
})

// ─── Field schema for the current type ────────────────────────────────────────

const spec = computed(() => getJobTypeSpec(localType.value))

/** 步骤构建器拥有的 config 键(commands 多行 / artifactPath 多行)。 */
const STEP_OWNED_KEYS = new Set(['commands', 'artifactPath'])

// 「自定义脚本」节点(script/custom)天然就是写命令文本,可视化步骤构建器对它是冗余:
// 这些类型不提供 steps/raw 切换、默认就用原始参数(命令文本框)。可视化步骤仅保留给
// 前端构建/后端构建/模板等带预置语义的脚本类节点。
const RAW_ONLY_SCRIPT_TYPES = new Set(['script', 'custom'])

// 执行/缓存类高级字段:能力保留,但默认收进可折叠「高级」分区,主表单只留镜像/命令/工作目录。
const ADVANCED_FIELD_KEYS = new Set(['timeoutSeconds', 'retries', 'cpu', 'memory', 'cachePaths', 'cacheKey'])

/** 该类型能用可视化步骤构建器吗(脚本类 + 有 commands/artifactPath 字段;但 script/custom 除外)。 */
const canUseStepBuilder = computed<boolean>(() => {
  if (!isScriptClassType(localType.value)) return false
  if (RAW_ONLY_SCRIPT_TYPES.has(localType.value)) return false
  if (!spec.value) return false
  return spec.value.fields.some((f) => STEP_OWNED_KEYS.has(f.key))
})

/**
 * 视图模式:'steps' 走可视化步骤构建器,'raw' 走原始 typed 表单。
 * 默认对脚本类显示步骤视图;但若 config 用了模板渲染(commandTemplate/params,
 * 步骤构建器不覆盖那套语义)则回退到原始视图,避免误编译丢数据。
 */
const viewMode = ref<'steps' | 'raw'>('raw')

function pickViewMode(config: Record<string, string>): void {
  if (canUseStepBuilder.value && !configUsesTemplate(config)) {
    viewMode.value = 'steps'
  } else {
    viewMode.value = 'raw'
  }
}

/** 步骤模式下,typed 表单只渲染「非步骤拥有」的字段(image/workDir/模板字段等)。 */
const visibleFields = computed<JobField[]>(() => {
  if (!spec.value) return []
  return spec.value.fields.filter((f) => {
    if (f.when && !f.when(typedConfig.value)) return false
    if (viewMode.value === 'steps' && STEP_OWNED_KEYS.has(f.key)) return false
    // runner(构建机)由下方独立「构建机」区块渲染,不进常规字段循环。
    if (f.key === 'runner') return false
    return true
  })
})

// 高级(执行/缓存)字段折叠:主表单只显示非高级字段;高级字段收进默认折叠分区。
const showExecAdvanced = ref(false)
/** 当前可见字段里第一个高级字段的 key —— 用于在它前面插入「高级」折叠开关。 */
const firstAdvancedKey = computed<string>(
  () => visibleFields.value.find((f) => ADVANCED_FIELD_KEYS.has(f.key))?.key ?? '',
)

/** 步骤构建器回传的编译片段并入 typedConfig 落库。 */
function onStepsUpdate(patch: { commands: string; artifactPath: string }): void {
  typedConfig.value = {
    ...typedConfig.value,
    commands: patch.commands,
    artifactPath: patch.artifactPath,
  }
  flush()
}

// ─── 工作室节点「实例参数短清单」 ─────────────────────────────────────────────
// 把工作室(templated / 带 __studio)节点拖进流水线后,实例编辑默认只面对作者「提升」
// 出来的少数参数(类型化控件),而非整段 commandTemplate/params 原始文本。提升参数的
// 定义(label/type/options)来自 __studio 元信息,只读消费;短清单只编辑各参数的「值」,
// 经 applyPromotedValues 写回 config.params 文本。原始视图收进可折叠「高级」入口兜底。

/** 当前完整 config(typed + extras 合并),供 studio 解析提升参数定义/值。 */
const liveConfig = computed<Record<string, string>>(() => currentConfig())

/** 是否工作室节点(templated 或带 __studio)。 */
const isStudio = computed<boolean>(() => isStudioNode(localType.value, liveConfig.value))

/** 提升参数定义(只读):key/label/type/options + 当前值(default 字段)。 */
const promotedParams = computed<PromotedParam[]>(() =>
  isStudio.value ? parsePromotedParams(liveConfig.value) : [],
)

/** 该节点是否值得显示短清单(工作室节点且至少有一个提升参数)。 */
const showShortlist = computed<boolean>(() => promotedParams.value.length > 0)

/** 短清单当前值表(key → value),由 params 文本解析而来。 */
const shortlistValues = computed<Record<string, string>>(() => promotedValues(liveConfig.value))

/** 工作室实例:原始参数视图(image/params/commandTemplate 等)默认收起。 */
const showStudioRaw = ref(false)

/** 短清单改值 → 写回 params 文本(只动 value,不碰 __studio 定义)→ flush。 */
function onShortlistUpdate(values: Record<string, string>): void {
  const params = applyPromotedValues(promotedParams.value, values)
  typedConfig.value = { ...typedConfig.value, params }
  flush()
}

function credentialOptions(field: JobField): Credential[] {
  const all = props.credentials ?? []
  if (!field.credentialType) return all
  const types = Array.isArray(field.credentialType) ? field.credentialType : [field.credentialType]
  return all.filter((c) => types.includes(c.type))
}

const CHANNEL_TYPE_LABELS: Record<string, string> = {
  webhook: 'Webhook',
  email: t('pipelineJob.channelTypeEmail'),
  feishu: t('pipelineJob.channelTypeFeishu'),
  wecom: t('pipelineJob.channelTypeWecom'),
  dingtalk: t('pipelineJob.channelTypeDingtalk'),
}

function channelTypeLabel(type: string): string {
  return CHANNEL_TYPE_LABELS[type] ?? type
}

/** producer 下拉选项(按 producerKinds 过滤类型;排除自身)。 */
function producerCandidates(field: JobField): ProducerOption[] {
  const all = props.producerOptions ?? []
  const kinds = field.producerKinds
  return all.filter((o) => o.name !== props.job.name && (!kinds || kinds.includes(o.type)))
}

function producerLabel(o: ProducerOption): string {
  return `${o.name}(${jobTypeLabel(o.type)})`
}

// ─── 构建机选择(节点级 runner · FR-8-19)────────────────────────────────────
// 脚本类节点各自声明「本节点跑在哪台构建机上」:config.runner 覆盖 > 阶段覆盖 > 项目默认;
// 空 = 跟随。选择器语法与全系统一致:标签项(`linux,arch=arm64`)/ `server:<id>` 钉死。
// 同一阶段内选择器相同的节点共享一台机器与工作区;不同选择器各自派发(后端按组调度)。

type RunnerMode = 'inherit' | 'label' | 'pinned'

const runnerMode = ref<RunnerMode>('inherit')
const runnerSelectorText = ref('')
const runnerServerId = ref('')
/** 最近一次写入 config 的值 + 所属 job:回显时区分「自己提交的回显」与「外部修改/无关字段编辑」。 */
const lastRunnerCommit = ref<string | undefined>(undefined)
const lastRunnerKey = ref('')

/** 从当前 config 重解三态。自己的回显/同 job 上其它字段的编辑不重置 —— 否则
 * 「切到标签/钉死但还没选具体值」或「顺手改了命令」都会把模式弹回「跟随项目默认」。
 * job 切换/类型切换(force)或 runner 被外部修改时才全量重解。 */
function resyncRunnerState(force = false): void {
  const key = `${props.job.id}|${props.job.type}`
  const committed = (typedConfig.value['runner'] ?? '').trim()
  if (!force && key === lastRunnerKey.value && committed === (lastRunnerCommit.value ?? '')) return
  lastRunnerKey.value = key
  lastRunnerCommit.value = committed === '' ? undefined : committed
  if (committed === '') {
    runnerMode.value = 'inherit'
    runnerSelectorText.value = ''
    runnerServerId.value = ''
  } else if (committed.startsWith('server:')) {
    runnerMode.value = 'pinned'
    runnerServerId.value = committed.slice('server:'.length)
    runnerSelectorText.value = ''
  } else {
    runnerMode.value = 'label'
    runnerServerId.value = ''
    runnerSelectorText.value = committed
  }
}

/** 标签模式选择器的实时命中预览(与服务端 runner 域同一语义;权威裁决在服务端)。 */
const runnerMatched = computed(() => matchServers(runnerSelectorText.value, props.servers ?? []))

/** 写回 runner:空 = 删除键(跟随默认),非空 = 选择器表达式。 */
function commitRunnerValue(value: string): void {
  const v = value.trim()
  lastRunnerCommit.value = v === '' ? undefined : v
  if (v === '') {
    const next = { ...typedConfig.value }
    delete next['runner']
    typedConfig.value = next
    flush()
  } else {
    setField('runner', v)
  }
}

/** 模式切换:先落本地态;选了具体值(标签文本/钉死机器)才提交,未选时保持本地编辑不落库。 */
function commitRunnerMode(mode: RunnerMode): void {
  runnerMode.value = mode
  if (mode === 'inherit') {
    commitRunnerValue('')
    return
  }
  const v = mode === 'label' ? runnerSelectorText.value.trim() : runnerServerId.value !== '' ? `server:${runnerServerId.value}` : ''
  if (v === '') return // 等用户选完标签/机器再落库
  commitRunnerValue(v)
}

function commitRunnerSelector(value: string): void {
  runnerSelectorText.value = value
  commitRunnerValue(value)
}

function commitRunnerServer(id: string): void {
  runnerServerId.value = id
  if (id === '') return
  commitRunnerValue(`server:${id}`)
}

/** 钉死模式下机器是否仍在服务器池(已删机器显示原始 id 兜底项)。 */
const runnerServerMissing = computed(
  () => runnerServerId.value !== '' && !(props.servers ?? []).some((s) => s.id === runnerServerId.value),
)

// Initial hydrate:此时上方所有声明均已求值(避免 step-builder 相关 computed 的暂时性死区)。
hydrate(props.job)

// ─── Value get/set ────────────────────────────────────────────────────────────

function fieldValue(key: string): string {
  return typedConfig.value[key] ?? ''
}

/** Default-display value for selects so the first option shows before any edit. */
function selectValue(field: JobField): string {
  const v = typedConfig.value[field.key]
  if (v) return v
  return field.options?.[0]?.value ?? ''
}

function updateLocal(key: string, value: string): void {
  typedConfig.value = { ...typedConfig.value, [key]: value }
}

function setField(key: string, value: string): void {
  updateLocal(key, value)
  flush()
}

// ─── Raw extras (advanced) ─────────────────────────────────────────────────────

function addExtra(): void {
  extraRows.value.push({ _key: ++_kvSeq, k: '', v: '' })
}

function removeExtra(key: number): void {
  extraRows.value = extraRows.value.filter((r) => r._key !== key)
  flush()
}

function extrasToObject(rows: KVRow[]): Record<string, string> {
  const out: Record<string, string> = {}
  for (const row of rows) {
    const key = row.k.trim()
    if (key) out[key] = row.v
  }
  return out
}

// ─── Flush ─────────────────────────────────────────────────────────────────────

/** Build the full config object from typed form + advanced extras (typed wins on conflict). */
function currentConfig(): Record<string, string> {
  return { ...extrasToObject(extraRows.value), ...typedConfig.value }
}

function flush(): void {
  emit('update', {
    name:    localName.value.trim() || props.job.name,
    type:    localType.value.trim() || props.job.type,
    summary: localSummary.value,
    config:  currentConfig(),
  })
}

// ─── Save as custom node (复用库 Tier 2) ──────────────────────────────────────
// Snapshot this node's type + summary + config into the reuse library so it can be
// picked into any pipeline later, pre-filled. Free-form config (no schema enforced).

const savePanelOpen = ref(false)
const saveName      = ref('')
const saveDesc      = ref('')
const saving        = ref(false)
const saveError     = ref('')
const saveOk        = ref(false)

function openSavePanel(): void {
  saveName.value = localName.value.trim()
  saveDesc.value = ''
  saveError.value = ''
  saveOk.value = false
  savePanelOpen.value = true
}

function cancelSave(): void {
  savePanelOpen.value = false
  saveError.value = ''
}

async function confirmSave(): Promise<void> {
  const name = saveName.value.trim()
  if (!name) {
    saveError.value = t('pipelineJob.saveErrEmptyName')
    return
  }
  saving.value = true
  saveError.value = ''
  try {
    await createCustomNode({
      name,
      description: saveDesc.value.trim(),
      nodeType: localType.value.trim() || props.job.type,
      summary: localSummary.value,
      config: currentConfig(),
    })
    saveOk.value = true
    savePanelOpen.value = false
  } catch (err) {
    saveError.value =
      err instanceof Error && err.message ? err.message : t('pipelineJob.saveErrFailed')
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <aside class="job-drawer" :aria-label="t('pipelineJob.drawerAria')">
    <!-- Head -->
    <div class="drawer-head">
      <span class="drawer-icon" aria-hidden="true">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9">
          <rect x="3" y="4" width="18" height="6" rx="1.6"/>
          <rect x="3" y="14" width="18" height="6" rx="1.6"/>
        </svg>
      </span>
      <div class="drawer-title">
        {{ localName || t('pipelineJob.unnamedJob') }}
        <small class="drawer-subtitle">{{ localizeName(stage.name) }} · {{ t('pipelineJob.thisPipeline') }}</small>
      </div>
      <button
        class="drawer-close"
        :aria-label="t('pipelineJob.closeDrawer')"
        @click="emit('close')"
      >
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
          <path d="M18 6 6 18M6 6l12 12"/>
        </svg>
      </button>
    </div>

    <!-- Basic info section -->
    <div class="drawer-section">
      <div class="drawer-section-label">{{ t('pipelineJob.basicInfo') }}</div>

      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineJob.jobNameLabel') }}</div>
        <input
          v-model="localName"
          class="drawer-input"
          type="text"
          :placeholder="t('pipelineJob.jobNamePlaceholder')"
          :aria-label="t('pipelineJob.jobNameLabel')"
          @blur="flush"
        />
      </div>

      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineJob.jobTypeLabel') }}</div>
        <button
          type="button"
          class="type-trigger"
          :aria-label="t('pipelineJob.changeType')"
          @click="emit('change-type')"
        >
          <JobTypeIcon :type="localType" :size="32" />
          <span class="type-trigger-body">
            <span class="type-trigger-name">{{ jobTypeLabel(localType) }}</span>
            <span class="type-trigger-desc">{{ spec ? spec.description : localType }}</span>
          </span>
          <span class="type-trigger-action">{{ t('pipelineJob.change') }}</span>
        </button>
      </div>

      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineJob.summaryLabel') }}</div>
        <input
          v-model="localSummary"
          class="drawer-input"
          type="text"
          :placeholder="t('pipelineJob.summaryPlaceholder')"
          :aria-label="t('pipelineJob.summaryLabel')"
          @blur="flush"
        />
      </div>
    </div>

    <!-- 工作室节点「实例参数短清单」:只展示作者提升的参数,类型化控件,default 预填。 -->
    <div v-if="showShortlist" class="drawer-section">
      <div class="drawer-section-label">{{ t('pipelineJob.instanceParams') }}</div>
      <p class="shortlist-hint">
        {{ t('pipelineJob.instanceParamsHint') }}
      </p>
      <StudioInstanceParams
        :params="promotedParams"
        :model-value="shortlistValues"
        @update:model-value="onShortlistUpdate"
      />
    </div>

    <!-- 工作室实例:原始命令模板/参数视图收进可折叠「高级」入口(默认收起)。 -->
    <div v-if="showShortlist && spec" class="drawer-section drawer-studio-raw-toggle">
      <button
        class="advanced-toggle"
        :class="{ 'advanced-toggle--open': showStudioRaw }"
        :aria-expanded="showStudioRaw"
        @click="showStudioRaw = !showStudioRaw"
      >
        <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" aria-hidden="true">
          <path d="M9 18l6-6-6-6"/>
        </svg>
        {{ t('pipelineJob.advancedRawToggle') }}
      </button>
    </div>

    <!-- Typed config section (per-type form) -->
    <div v-if="spec && (!showShortlist || showStudioRaw)" class="drawer-section">
      <div class="drawer-section-head">
        <div class="drawer-section-label">{{ spec.label }}{{ t('pipelineJob.configSuffix') }}</div>
        <div v-if="canUseStepBuilder && !showShortlist" class="view-switch" role="tablist" :aria-label="t('pipelineJob.configView')">
          <button
            class="view-tab"
            :class="{ 'view-tab--active': viewMode === 'steps' }"
            role="tab"
            :aria-selected="viewMode === 'steps'"
            @click="viewMode = 'steps'"
          >
            {{ t('pipelineJob.visualSteps') }}
          </button>
          <button
            class="view-tab"
            :class="{ 'view-tab--active': viewMode === 'raw' }"
            role="tab"
            :aria-selected="viewMode === 'raw'"
            @click="viewMode = 'raw'"
          >
            {{ t('pipelineJob.rawParams') }}
          </button>
        </div>
      </div>

      <template v-for="field in visibleFields" :key="field.key">
        <!-- 高级折叠开关:插在第一个高级(执行/缓存)字段前 -->
        <button
          v-if="field.key === firstAdvancedKey"
          type="button"
          class="advanced-toggle exec-advanced-toggle"
          :class="{ 'advanced-toggle--open': showExecAdvanced }"
          :aria-expanded="showExecAdvanced"
          @click="showExecAdvanced = !showExecAdvanced"
        >
          <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" aria-hidden="true">
            <path d="M9 18l6-6-6-6"/>
          </svg>
          {{ t('pipelineJob.execAdvancedToggle') }}
        </button>
      <div
        v-show="!ADVANCED_FIELD_KEYS.has(field.key) || showExecAdvanced"
        class="drawer-field"
      >
        <div class="drawer-field-label">{{ field.label }}</div>

        <!-- textarea -->
        <textarea
          v-if="field.kind === 'textarea'"
          :value="fieldValue(field.key)"
          class="drawer-input drawer-textarea"
          :class="{ 'is-mono': field.monospace }"
          rows="4"
          :placeholder="field.placeholder"
          :aria-label="field.label"
          @input="updateLocal(field.key, ($event.target as HTMLTextAreaElement).value)"
          @blur="flush"
        ></textarea>

        <!-- select -->
        <select
          v-else-if="field.kind === 'select'"
          :value="selectValue(field)"
          class="drawer-select"
          :aria-label="field.label"
          @change="setField(field.key, ($event.target as HTMLSelectElement).value)"
        >
          <option v-for="opt in field.options" :key="opt.value" :value="opt.value">
            {{ opt.label }}
          </option>
        </select>

        <!-- credential picker -->
        <select
          v-else-if="field.kind === 'credential'"
          :value="fieldValue(field.key)"
          class="drawer-select"
          :aria-label="field.label"
          @change="setField(field.key, ($event.target as HTMLSelectElement).value)"
        >
          <option value="">{{ t('pipelineJob.credUnselected') }}</option>
          <option v-for="c in credentialOptions(field)" :key="c.id" :value="c.id">
            {{ c.name }} ({{ c.maskedValue }})
          </option>
        </select>

        <!-- server picker -->
        <select
          v-else-if="field.kind === 'server'"
          :value="fieldValue(field.key)"
          class="drawer-select"
          :aria-label="field.label"
          @change="setField(field.key, ($event.target as HTMLSelectElement).value)"
        >
          <option value="">{{ t('pipelineJob.credUnselected') }}</option>
          <option v-for="srv in (servers ?? [])" :key="srv.id" :value="srv.id">
            {{ srv.name }} · {{ srv.host }}
          </option>
        </select>

        <!-- notification channel picker -->
        <select
          v-else-if="field.kind === 'channel'"
          :value="fieldValue(field.key)"
          class="drawer-select"
          :aria-label="field.label"
          @change="setField(field.key, ($event.target as HTMLSelectElement).value)"
        >
          <option value="">{{ t('pipelineJob.channelUnselected') }}</option>
          <option v-for="ch in (channels ?? [])" :key="ch.id" :value="ch.id">
            {{ ch.name }} · {{ channelTypeLabel(ch.type) }}{{ ch.enabled ? '' : t('pipelineJob.channelDisabled') }}
          </option>
        </select>

        <!-- 标签选择器(选值控件:从机器实际标签里选 key/value,多条件且/或显式选) -->
        <LabelSelectorEditor
          v-else-if="field.kind === 'labelSelector'"
          :model-value="typedConfig.selector ?? ''"
          :servers="props.servers"
          @update:model-value="(v: string) => setField('selector', v)"
        />

        <!-- 产物来源(精确绑定:选哪个节点产出的产物;无「自动」) -->
        <select
          v-else-if="field.kind === 'producer'"
          :value="fieldValue(field.key)"
          class="drawer-select"
          :aria-label="field.label"
          @change="setField(field.key, ($event.target as HTMLSelectElement).value)"
        >
          <option value="">{{ t('pipelineJob.producerUnselected') }}</option>
          <option v-for="o in producerCandidates(field)" :key="o.name" :value="o.name">
            {{ producerLabel(o) }} · {{ o.stageName }}
          </option>
        </select>

        <!-- toggle -->
        <label v-else-if="field.kind === 'toggle'" class="drawer-toggle">
          <input
            type="checkbox"
            :checked="fieldValue(field.key) === 'true'"
            :aria-label="field.label"
            @change="setField(field.key, ($event.target as HTMLInputElement).checked ? 'true' : 'false')"
          />
          <span>{{ field.hint || t('pipelineJob.toggleEnable') }}</span>
        </label>

        <!-- number / text -->
        <input
          v-else
          :value="fieldValue(field.key)"
          class="drawer-input"
          :class="{ 'is-mono': field.monospace }"
          :type="field.kind === 'number' ? 'number' : 'text'"
          :placeholder="field.placeholder"
          :aria-label="field.label"
          @input="updateLocal(field.key, ($event.target as HTMLInputElement).value)"
          @blur="flush"
        />

        <p v-if="field.hint && field.kind !== 'toggle'" class="field-hint">{{ field.hint }}</p>
      </div>
      </template>

      <!-- 可视化步骤构建器(脚本类节点;编译进 commands/artifactPath,经 update 落库) -->
      <div v-if="canUseStepBuilder && viewMode === 'steps'" class="drawer-field step-builder-field">
        <StepBuilder :config="typedConfig" @update="onStepsUpdate" />
        <p class="field-hint">
          {{ t('pipelineJob.stepBuilderHint') }}
        </p>
      </div>
    </div>

    <!-- Advanced raw KV (extras not covered by the schema) -->
    <!-- 构建机选择(RUNNER · FR-8-19):脚本类节点各自声明跑在哪台构建机上 -->
    <div v-if="isScriptClassType(localType)" class="drawer-section">
      <div class="drawer-section-label">{{ t('pipelineCanvas.runnerSectionLabel') }}</div>
      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineCanvas.runnerModeLabel') }}</div>
        <select
          v-model="runnerMode"
          class="drawer-select"
          :aria-label="t('pipelineCanvas.runnerModeLabel')"
          @change="commitRunnerMode(($event.target as HTMLSelectElement).value as RunnerMode)"
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
          class="field-hint"
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
          <option v-if="runnerServerMissing" :value="runnerServerId">{{ runnerServerId }}</option>
        </select>
      </div>
      <p class="field-hint">{{ t('pipelineJob.jobRunnerHint') }}</p>
    </div>

    <div class="drawer-section">
      <button
        class="advanced-toggle"
        :class="{ 'advanced-toggle--open': showAdvanced }"
        :aria-expanded="showAdvanced"
        @click="showAdvanced = !showAdvanced"
      >
        <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" aria-hidden="true">
          <path d="M9 18l6-6-6-6"/>
        </svg>
        {{ t('pipelineJob.advancedRawParams') }}<span v-if="extraRows.length" class="advanced-count">{{ extraRows.length }}</span>
      </button>

      <div v-if="showAdvanced" class="advanced-body">
        <div v-if="extraRows.length === 0" class="kv-empty">
          {{ spec ? t('pipelineJob.kvEmptyWithSpec') : t('pipelineJob.kvEmptyNoSpec') }}
        </div>

        <div
          v-for="row in extraRows"
          :key="row._key"
          class="kv-row"
        >
          <input
            v-model="row.k"
            class="kv-input"
            type="text"
            :placeholder="t('pipelineJob.kvKeyPlaceholder')"
            :aria-label="t('pipelineJob.kvKeyAria', { n: row._key })"
            @blur="flush"
          />
          <input
            v-model="row.v"
            class="kv-input"
            type="text"
            :placeholder="t('pipelineJob.kvValuePlaceholder')"
            :aria-label="t('pipelineJob.kvValueAria', { n: row._key })"
            @blur="flush"
          />
          <button
            class="kv-del"
            :aria-label="t('pipelineJob.kvDelAria', { label: row.k || row._key })"
            @click="removeExtra(row._key)"
          >
            <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
              <path d="M18 6 6 18M6 6l12 12"/>
            </svg>
          </button>
        </div>

        <button class="kv-add-btn" @click="addExtra">
          <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" aria-hidden="true">
            <path d="M12 5v14M5 12h14"/>
          </svg>
          {{ t('pipelineJob.addParam') }}
        </button>
      </div>
    </div>

    <!-- Save as custom node (复用库 Tier 2) -->
    <div class="drawer-section drawer-reuse">
      <div v-if="!savePanelOpen" class="reuse-row">
        <button class="reuse-btn" @click="openSavePanel">
          <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
            <path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z"/>
            <path d="M17 21v-8H7v8M7 3v5h8"/>
          </svg>
          {{ t('pipelineJob.saveAsCustomNode') }}
        </button>
        <span v-if="saveOk" class="reuse-ok" role="status">{{ t('pipelineJob.savedToLibrary') }}</span>
      </div>

      <div v-else class="reuse-panel">
        <div class="reuse-panel-title">{{ t('pipelineJob.saveAsCustomNode') }}</div>
        <p class="reuse-panel-hint">{{ t('pipelineJob.savePanelHint') }}</p>
        <input
          v-model="saveName"
          class="drawer-input"
          type="text"
          :placeholder="t('pipelineJob.saveNamePlaceholder')"
          :aria-label="t('pipelineJob.saveNameAria')"
          @keydown.enter.prevent="confirmSave"
        />
        <input
          v-model="saveDesc"
          class="drawer-input"
          type="text"
          :placeholder="t('pipelineJob.saveDescPlaceholder')"
          :aria-label="t('pipelineJob.saveDescAria')"
        />
        <p v-if="saveError" class="reuse-error" role="alert">{{ saveError }}</p>
        <div class="reuse-actions">
          <button class="reuse-cancel" :disabled="saving" @click="cancelSave">{{ t('pipelineJob.cancel') }}</button>
          <button class="reuse-confirm" :disabled="saving" @click="confirmSave">
            {{ saving ? t('pipelineJob.saving') : t('pipelineJob.save') }}
          </button>
        </div>
      </div>
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

.drawer-section-head .drawer-section-label {
  margin-bottom: 0;
}

.view-switch {
  display: inline-flex;
  padding: 2px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-full);
}

.view-tab {
  padding: 3px 10px;
  background: none;
  border: none;
  border-radius: var(--rounded-full);
  color: var(--color-faint);
  font: inherit;
  font-size: 0.72rem;
  font-weight: 600;
  cursor: pointer;
  transition: color var(--duration-fast), background-color var(--duration-fast);
}

.view-tab:hover {
  color: var(--color-text);
}

.view-tab--active {
  background: var(--color-primary);
  color: #fff;
}

.view-tab:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}

.step-builder-field {
  margin-top: 4px;
}

.shortlist-hint {
  margin: 0 0 12px;
  font-size: 0.73rem;
  line-height: 1.45;
  color: var(--color-faint);
}

.drawer-studio-raw-toggle {
  padding-top: 0;
  margin-top: -4px;
}

.kv-empty {
  font-size: 0.78rem;
  color: var(--color-faint);
  font-style: italic;
  margin-bottom: 8px;
}

.field-desc {
  margin: 6px 0 0;
  font-size: 0.74rem;
  color: var(--color-faint);
}

.type-trigger {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 8px 10px;
  background: var(--color-inset);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  cursor: pointer;
  text-align: left;
  transition: border-color var(--duration-fast), box-shadow var(--duration-fast);
}

.type-trigger:hover {
  border-color: var(--color-primary);
}

.type-trigger:focus-visible {
  outline: none;
  border-color: var(--color-primary);
  box-shadow: 0 0 0 3px var(--color-primary-soft);
}

.type-trigger-body {
  display: flex;
  flex-direction: column;
  gap: 1px;
  min-width: 0;
  flex: 1;
}

.type-trigger-name {
  font-size: 0.86rem;
  font-weight: 600;
  color: var(--color-text);
}

.type-trigger-desc {
  font-size: 0.73rem;
  color: var(--color-faint);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.type-trigger-action {
  flex-shrink: 0;
  font-size: 0.74rem;
  font-weight: 600;
  color: var(--color-primary);
}

.field-hint {
  margin: 5px 0 0;
  font-size: 0.72rem;
  color: var(--color-faint);
  line-height: 1.4;
}

.runner-match--none {
  color: var(--color-danger, #dc2626);
}

.drawer-textarea {
  height: auto;
  min-height: 84px;
  padding: 9px 11px;
  line-height: 1.5;
  resize: vertical;
}

.is-mono {
  font-family: var(--font-mono);
  font-size: 0.8rem;
}

.drawer-toggle {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: 0.8rem;
  color: var(--color-dim);
  cursor: pointer;
}

.drawer-toggle input {
  width: 15px;
  height: 15px;
  accent-color: var(--color-primary);
  cursor: pointer;
}

.advanced-toggle {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  background: none;
  border: none;
  padding: 0;
  color: var(--color-dim);
  font: inherit;
  font-size: 0.76rem;
  font-weight: 600;
  cursor: pointer;
  text-transform: uppercase;
  letter-spacing: 0.02em;
}

.advanced-toggle svg {
  transition: transform var(--duration-fast);
}

.advanced-toggle--open svg {
  transform: rotate(90deg);
}

.advanced-toggle:hover {
  color: var(--color-text);
}

.advanced-count {
  display: inline-grid;
  place-items: center;
  min-width: 16px;
  height: 16px;
  padding: 0 4px;
  border-radius: 8px;
  background: var(--color-primary-soft);
  color: var(--color-primary);
  font-size: 0.66rem;
  font-weight: 700;
}

.advanced-body {
  margin-top: 11px;
}

/* ——— Save as custom node ——— */
.drawer-reuse {
  border-top: 1px dashed var(--color-border);
  padding-top: 14px;
}

.reuse-row {
  display: flex;
  align-items: center;
  gap: 10px;
}

.reuse-btn {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  padding: 7px 12px;
  background: var(--color-inset);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  color: var(--color-text);
  font: inherit;
  font-size: 0.8rem;
  font-weight: 600;
  cursor: pointer;
  transition: border-color var(--duration-fast), color var(--duration-fast);
}

.reuse-btn:hover {
  border-color: var(--color-primary);
  color: var(--color-primary);
}

.reuse-ok {
  font-size: 0.76rem;
  font-weight: 600;
  color: var(--color-green, var(--color-primary));
}

.reuse-panel {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px;
  background: var(--color-inset);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
}

.reuse-panel-title {
  font-size: 0.84rem;
  font-weight: 650;
  color: var(--color-text);
}

.reuse-panel-hint {
  margin: 0;
  font-size: 0.73rem;
  line-height: 1.45;
  color: var(--color-faint);
}

.reuse-error {
  margin: 0;
  font-size: 0.74rem;
  color: var(--color-red, #d33);
}

.reuse-actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 2px;
}

.reuse-cancel,
.reuse-confirm {
  padding: 6px 14px;
  border-radius: var(--rounded);
  font: inherit;
  font-size: 0.8rem;
  font-weight: 600;
  cursor: pointer;
  transition: opacity var(--duration-fast), background-color var(--duration-fast);
}

.reuse-cancel {
  background: none;
  border: 1px solid var(--color-border-strong);
  color: var(--color-dim);
}

.reuse-cancel:hover:not(:disabled) {
  color: var(--color-text);
}

.reuse-confirm {
  background: var(--color-primary);
  border: 1px solid var(--color-primary);
  color: #fff;
}

.reuse-confirm:hover:not(:disabled) {
  opacity: 0.9;
}

.reuse-cancel:disabled,
.reuse-confirm:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}
</style>
