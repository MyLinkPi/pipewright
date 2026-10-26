<script setup lang="ts">
/**
 * VarsCacheTab — 变量与缓存 tab (Story 2.4).
 * Build variables (plain / secret-via-vault) + dependency cache. Edits a local
 * BuildConfig copy and emits `update` upward; the parent (ProjectPipeline) owns save.
 *
 * 流水线即代码变量(第三张卡片):仓库 YAML 顶层 variables 声明(defs,经「预览仓库配置」
 * 拉取校验同步,此处只读展示)在每行填写值——secret 变量选保险库凭据(credentialId),
 * 明文变量填文本。行不可增删(声明属于仓库);值编辑经 `updatePacVars` 上抛,父组件统一保存。
 *
 * 构建模型(Dockerfile/工具链)与产物类型(镜像/JAR/dist)已下沉到画布的 build_image 节点表单
 * (job.config),执行引擎只读节点配置;此处不再渲染那两块全局设置。但仍把 model/dockerfilePath/
 * toolchain/artifactType 原值透传进 composed,保证保存/AI 应用不抹除既有值(API 契约字段保留)。
 *
 * Secret variables reference a vault credential (credentialId) — plaintext is never
 * entered, stored, or shown here; only the server-computed mask appears.
 */
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type {
  BuildConfig,
  BuildVar,
  BuildModel,
  ArtifactType,
  PacVarsConfig,
} from '../../api/pipelineSettings'
import { isSystemManagedType, type Credential } from '../../api/credentials'

interface Props {
  build: BuildConfig
  credentials: Credential[]
  disabled?: boolean
  /** Pipeline-as-code variable declarations + filled values (null = never synced). */
  pacVars?: PacVarsConfig | null
}
const props = defineProps<Props>()

const { t } = useI18n()

const emit = defineEmits<{
  update: [build: BuildConfig]
  updatePacVars: [values: BuildVar[]]
}>()

// ─── Local editable copy ────────────────────────────────────────────────────────

let keySeq = 0
interface VarRow extends BuildVar {
  _key: number
}

const model = ref<BuildModel>(props.build.model)
const dockerfilePath = ref(props.build.dockerfilePath || 'Dockerfile')
const language = ref(props.build.toolchain.language)
const version = ref(props.build.toolchain.version)
const artifactType = ref<ArtifactType>(props.build.artifactType)
const vars = ref<VarRow[]>(props.build.vars.map((v) => ({ ...v, _key: keySeq++ })))
const cacheEnabled = ref(props.build.cache.enabled)
const cachePaths = ref<string>(props.build.cache.paths.join('\n'))

// Re-sync when the upstream build changes (e.g. after a save round-trip).
// 父回写大多是本地 emit 的回声(每敲一键 → emit → 父写回 :build)。若照单全收重置本地态,
// keySeq++ 换掉全部 _key,输入框 DOM 被整体替换 → 每键失焦。仅当回写内容与本地当前组合
// 确有差异(保存后服务端回填 id 等)才重建。
watch(
  () => props.build,
  (b) => {
    if (buildKey(b) === buildKey(composed.value)) return
    model.value = b.model
    dockerfilePath.value = b.dockerfilePath || 'Dockerfile'
    language.value = b.toolchain.language
    version.value = b.toolchain.version
    artifactType.value = b.artifactType
    vars.value = b.vars.map((v) => ({ ...v, _key: keySeq++ }))
    cacheEnabled.value = b.cache.enabled
    cachePaths.value = b.cache.paths.join('\n')
  },
)

// ─── Emit composed BuildConfig on any change ────────────────────────────────────

const composed = computed<BuildConfig>(() => ({
  model: model.value,
  dockerfilePath: dockerfilePath.value.trim() || 'Dockerfile',
  toolchain: { language: language.value.trim(), version: version.value.trim() },
  artifactType: artifactType.value,
  vars: vars.value.map(({ _key, ...v }) => {
    void _key
    return v
  }),
  cache: {
    enabled: cacheEnabled.value,
    paths: cachePaths.value
      .split('\n')
      .map((p) => p.trim())
      .filter(Boolean),
  },
}))

// 规范化内容键:用于比对「子组件当前内容」与「上游 props.build」是否真不同。
// 防双向绑定回环:父收到 update 后回写 :build,本组件 watch(()=>props.build) 重置本地态
// (含 keySeq++ 造新 _key)→ composed 重算 → 若无脑 emit 则父再回写 → 无限循环 → 渲染器 OOM。
// 仅当规范化内容确有差异时才 emit,回环在内容收敛后即止。
function buildKey(b: BuildConfig): string {
  return JSON.stringify({
    model: b.model,
    dockerfilePath: (b.dockerfilePath || 'Dockerfile').trim() || 'Dockerfile',
    language: (b.toolchain?.language ?? '').trim(),
    version: (b.toolchain?.version ?? '').trim(),
    artifactType: b.artifactType,
    vars: b.vars.map((v) => ({
      id: v.id ?? '',
      key: v.key,
      secret: !!v.secret,
      value: v.value ?? '',
      credentialId: v.credentialId ?? '',
    })),
    cacheEnabled: !!b.cache?.enabled,
    cachePaths: (b.cache?.paths ?? []).map((p) => p.trim()).filter(Boolean),
  })
}

watch(
  composed,
  (b) => {
    if (buildKey(b) !== buildKey(props.build)) emit('update', b)
  },
  { deep: true },
)

// ─── Variable row ops ───────────────────────────────────────────────────────────

function addVar(secret: boolean): void {
  vars.value.push({
    _key: keySeq++,
    id: '',
    key: '',
    secret,
    value: secret ? undefined : '',
    credentialId: secret ? '' : undefined,
  })
}

function removeVar(rowKey: number): void {
  vars.value = vars.value.filter((v) => v._key !== rowKey)
}

function toggleSecret(row: VarRow): void {
  row.secret = !row.secret
  if (row.secret) {
    row.value = undefined
    row.credentialId = ''
  } else {
    row.credentialId = undefined
    row.value = ''
  }
}

// Credentials suitable for secret values; system-managed types (dns_token 等,由专属
// 设置页管理)不作为流水线可引用的凭据选项,防误选与误删悬挂。
const credOptions = computed(() =>
  props.credentials.filter((c) => !isSystemManagedType(c.type)),
)

function maskFor(row: VarRow): string {
  if (row.maskedValue) return row.maskedValue
  const c = props.credentials.find((x) => x.id === row.credentialId)
  return c ? c.maskedValue : '••••'
}

// ─── Pipeline-as-code variables(仓库声明 + 填写值)──────────────────────────────

/** 一行 = 一条仓库声明(defs 只读)+ 本地编辑的值副本。 */
interface PacRow {
  name: string
  description: string
  required: boolean
  secret: boolean
  default: string
  row: VarRow
}

const pacDefs = computed(() => props.pacVars?.defs ?? [])

function pacRowsFrom(defs: PacVarsConfig['defs'], values: BuildVar[]): PacRow[] {
  const byKey = new Map<string, BuildVar>()
  for (const v of values) byKey.set(v.key, v)
  return defs.map((d) => {
    const v = byKey.get(d.name)
    return {
      name: d.name,
      description: d.description,
      required: d.required,
      secret: d.secret,
      default: d.default,
      row: {
        _key: keySeq++,
        id: v?.id ?? '',
        key: d.name,
        secret: d.secret, // secret 与声明一致,不受误传值影响
        value: d.secret ? undefined : (v?.value ?? ''),
        credentialId: d.secret ? (v?.credentialId ?? '') : undefined,
        maskedValue: v?.maskedValue,
      },
    }
  })
}

const pacRows = ref<PacRow[]>(pacRowsFrom(pacDefs.value, props.pacVars?.values ?? []))

/** 已填的值(未触碰/清空的行不上送,保持「未填写」语义)。 */
const pacComposed = computed<BuildVar[]>(() =>
  pacRows.value
    .filter((p) => (p.secret ? !!p.row.credentialId : (p.row.value ?? '').trim() !== ''))
    .map((p) => ({
      id: p.row.id || '',
      key: p.name,
      secret: p.secret,
      value: p.secret ? undefined : p.row.value,
      credentialId: p.secret ? p.row.credentialId : undefined,
    })),
)

// 双向回环防护(与构建变量 buildKey 同思路):值键只比「已填内容」,defs 键单独比,
// 两侧同构可比——父回写相同内容时既不重建行(防每键失焦)也不再 emit(防无限循环)。
function pacValuesKey(values: BuildVar[]): string {
  const filled = values
    .filter((v) => (v.secret ? !!v.credentialId : (v.value ?? '') !== ''))
    .map((v) => JSON.stringify({ key: v.key, secret: !!v.secret, value: v.value ?? '', credentialId: v.credentialId ?? '' }))
  return JSON.stringify(filled)
}

function pacDefsKey(defs: PacVarsConfig['defs']): string {
  return JSON.stringify((defs ?? []).map((d) => `${d.name}:${d.secret ? 1 : 0}:${d.required ? 1 : 0}`))
}

let lastPacDefsKey = pacDefsKey(pacDefs.value)

watch(
  () => props.pacVars,
  (pv) => {
    const defsChanged = pacDefsKey(pv?.defs ?? []) !== lastPacDefsKey
    const valuesDiffer = pacValuesKey(pv?.values ?? []) !== pacValuesKey(pacComposed.value)
    if (!defsChanged && !valuesDiffer) return
    lastPacDefsKey = pacDefsKey(pv?.defs ?? [])
    pacRows.value = pacRowsFrom(pv?.defs ?? [], pv?.values ?? [])
  },
)

watch(
  pacComposed,
  (vals) => {
    if (pacValuesKey(vals) !== pacValuesKey(props.pacVars?.values ?? [])) emit('updatePacVars', vals)
  },
  { deep: true },
)

function pacMaskFor(p: PacRow): string {
  if (p.row.maskedValue) return p.row.maskedValue
  const c = props.credentials.find((x) => x.id === p.row.credentialId)
  return c ? c.maskedValue : '••••'
}

function pacFilled(p: PacRow): boolean {
  return p.secret ? !!p.row.credentialId : (p.row.value ?? '').trim() !== ''
}
</script>

<template>
  <div class="vars-root">
    <!-- 构建模型 / 产物类型已移到画布 build_image 节点(job.config),此处不再重复设置。 -->

    <!-- ─── Build variables ─────────────────────────────────────────────────── -->
    <section class="card">
      <header class="card-head">
        <span class="card-ic" aria-hidden="true">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"><path d="M4 7h16M4 12h10M4 17h7"/></svg>
        </span>
        {{ t('pipelinePanels.vcBuildVars') }}
        <span class="card-sub">{{ t('pipelinePanels.vcBuildVarsSub') }}</span>
      </header>

      <div v-if="vars.length" class="vhd" aria-hidden="true">
        <span>{{ t('pipelinePanels.vcColKey') }}</span><span>{{ t('pipelinePanels.vcColValueRef') }}</span><span>{{ t('pipelinePanels.vcColSource') }}</span><span/>
      </div>

      <div
        v-for="row in vars"
        :key="row._key"
        class="vrow"
      >
        <input
          v-model="row.key"
          class="vk mono"
          type="text"
          placeholder="KEY"
          :aria-label="t('pipelinePanels.vcVarKeyAria')"
          :disabled="disabled"
        >

        <template v-if="row.secret">
          <select
            v-model="row.credentialId"
            class="vsel"
            :aria-label="t('pipelinePanels.vcVaultCredAria')"
            :disabled="disabled"
          >
            <option value="" disabled>{{ t('pipelinePanels.vcSelectVaultCred') }}</option>
            <option v-for="c in credOptions" :key="c.id" :value="c.id">
              {{ c.name }} · {{ c.maskedValue }}
            </option>
          </select>
        </template>
        <input
          v-else
          v-model="row.value"
          class="vv mono"
          type="text"
          :placeholder="t('pipelinePanels.vcVarValuePlaceholder')"
          :aria-label="t('pipelinePanels.vcVarValueAria')"
          :disabled="disabled"
        >

        <button
          type="button"
          class="vfrom"
          :class="row.secret ? 'vfrom--vault' : 'vfrom--plain'"
          :disabled="disabled"
          :aria-label="row.secret ? t('pipelinePanels.vcSwitchToPlainAria') : t('pipelinePanels.vcSwitchToVaultAria')"
          :title="row.secret ? t('pipelinePanels.vcSwitchToPlainTitle') : t('pipelinePanels.vcSwitchToVaultTitle')"
          @click="toggleSecret(row)"
        >
          <template v-if="row.secret">{{ t('pipelinePanels.vcVault') }} <span class="mask mono">{{ maskFor(row) }}</span></template>
          <template v-else>{{ t('pipelinePanels.vcPlain') }}</template>
        </button>

        <button
          type="button"
          class="vdel"
          :aria-label="t('pipelinePanels.vcDelVarAria')"
          :disabled="disabled"
          @click="removeVar(row._key)"
        >
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M3 6h18M8 6V4h8v2M6 6l1 14h10l1-14"/></svg>
        </button>
      </div>

      <p v-if="!vars.length" class="vempty">{{ t('pipelinePanels.vcEmpty') }}</p>

      <div class="addrow">
        <button type="button" class="addbtn" :disabled="disabled" @click="addVar(false)">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>
          {{ t('pipelinePanels.vcAddPlainVar') }}
        </button>
        <button type="button" class="addbtn addbtn--vault" :disabled="disabled" @click="addVar(true)">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" aria-hidden="true"><rect x="4" y="11" width="16" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/></svg>
          {{ t('pipelinePanels.vcAddVaultSecret') }}
        </button>
      </div>
    </section>

    <!-- ─── Pipeline-as-code variables(仓库声明)────────────────────────── -->
    <section class="card">
      <header class="card-head">
        <span class="card-ic" aria-hidden="true">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"><path d="M6 3v12"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/></svg>
        </span>
        {{ t('pipelinePanels.vcPacVars') }}
        <span class="card-sub">{{ t('pipelinePanels.vcPacVarsSub') }}</span>
      </header>

      <p v-if="props.pacVars?.ref || props.pacVars?.file" class="pac-src">
        {{ t('pipelinePanels.vcPacSource', { ref: props.pacVars.ref, file: props.pacVars.file }) }}
      </p>

      <div v-if="pacRows.length" class="pachd" aria-hidden="true">
        <span>{{ t('pipelinePanels.vcColKey') }}</span><span>{{ t('pipelinePanels.vcPacColDesc') }}</span><span>{{ t('pipelinePanels.vcColValueRef') }}</span>
      </div>

      <div v-for="p in pacRows" :key="p.name" class="pacrow" :class="{ 'pacrow--missing': p.required && !pacFilled(p) }">
        <div class="pac-name">
          <code class="mono">{{ p.name }}</code>
          <span v-if="p.required" class="pacbadge pacbadge--req">{{ t('pipelinePanels.vcPacRequired') }}</span>
          <span v-if="p.secret" class="pacbadge pacbadge--sec">{{ t('pipelinePanels.vcPacSecret') }}</span>
        </div>

        <div class="pac-desc">
          <p>{{ p.description || '—' }}</p>
          <p v-if="p.default" class="pac-default">{{ t('pipelinePanels.vcPacDefaultPrefix') }}<code class="mono">{{ p.default }}</code></p>
        </div>

        <div class="pac-val">
          <template v-if="p.secret">
            <select
              v-model="p.row.credentialId"
              class="vsel"
              :aria-label="t('pipelinePanels.vcVaultCredAria')"
              :disabled="disabled"
            >
              <option value="">{{ t('pipelinePanels.vcPacSelectCred') }}</option>
              <option v-for="c in credOptions" :key="c.id" :value="c.id">
                {{ c.name }} · {{ c.maskedValue }}
              </option>
            </select>
            <span v-if="p.row.credentialId" class="pac-mask mono">{{ pacMaskFor(p) }}</span>
          </template>
          <input
            v-else
            v-model="p.row.value"
            class="vv mono"
            type="text"
            :placeholder="p.default ? p.default : t('pipelinePanels.vcPacValuePlaceholder')"
            :aria-label="t('pipelinePanels.vcVarValueAria')"
            :disabled="disabled"
          >
        </div>
      </div>

      <p v-if="!pacRows.length" class="vempty">{{ t('pipelinePanels.vcPacEmpty') }}</p>
    </section>

    <!-- ─── Dependency cache ────────────────────────────────────────────────── -->
    <section class="card">
      <header class="card-head">
        <span class="card-ic" aria-hidden="true">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"><ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v6c0 1.7 3.6 3 8 3s8-1.3 8-3V6M4 12v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6"/></svg>
        </span>
        {{ t('pipelinePanels.vcDepCache') }}
        <span class="card-sub">{{ t('pipelinePanels.vcDepCacheSub') }}</span>
      </header>

      <div class="cache-toggle">
        <button
          type="button"
          class="sw"
          :class="{ 'sw--off': !cacheEnabled }"
          role="switch"
          :aria-checked="cacheEnabled"
          :aria-label="t('pipelinePanels.vcEnableCacheAria')"
          :disabled="disabled"
          @click="cacheEnabled = !cacheEnabled"
        ><span class="sw-knob" aria-hidden="true"/></button>
        <div class="cache-tx">
          <b>{{ t('pipelinePanels.vcEnableCache') }}</b>
          <span>{{ t('pipelinePanels.vcEnableCacheDesc') }}</span>
        </div>
      </div>

      <div class="cache-paths" :class="{ 'cache-paths--off': !cacheEnabled }">
        <label class="md-label" for="cache-paths">{{ t('pipelinePanels.vcCachePaths') }}</label>
        <textarea
          id="cache-paths"
          v-model="cachePaths"
          class="md-input mono cache-area"
          rows="3"
          placeholder="node_modules&#10;.npm"
          :disabled="disabled || !cacheEnabled"
        />
      </div>
    </section>
  </div>
</template>

<style scoped>
.vars-root {
  display: flex;
  flex-direction: column;
  gap: 18px;
  align-items: stretch;
  max-width: 980px;
}

.card {
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  box-shadow: var(--shadow);
  overflow: hidden;
}

.card-head {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 13px 18px;
  border-bottom: 1px solid var(--color-border);
  font-size: 0.88rem;
  font-weight: 600;
  color: var(--color-text);
}
.card-ic {
  width: 22px; height: 22px;
  border-radius: 6px;
  background: var(--color-primary-soft);
  color: var(--color-primary);
  display: grid; place-items: center;
  flex: none;
}
.card-ic svg { width: 13px; height: 13px; }
.card-sub {
  margin-left: 4px;
  font-size: 0.73rem;
  color: var(--color-faint);
  font-weight: 400;
}

.md-label {
  display: block;
  font-size: var(--text-label);
  font-weight: 500;
  color: var(--color-dim);
  margin-bottom: 5px;
}
.md-input {
  width: 100%;
  height: 34px;
  border: 1px solid var(--color-border-strong);
  background: var(--color-inset);
  color: var(--color-text);
  border-radius: var(--rounded);
  padding: 0 11px;
  font-size: 0.82rem;
  font-family: var(--font-sans);
}
.md-input:focus { outline: none; border-color: var(--color-primary); box-shadow: 0 0 0 3px var(--color-primary-soft); }
.mono { font-family: var(--font-mono); }

/* ─── Variable table ─────────────────────────────────────────────────────── */
.vhd, .vrow {
  display: grid;
  grid-template-columns: 180px 1fr 128px 34px;
  gap: 10px;
  align-items: center;
  padding: 9px 18px;
  border-bottom: 1px solid var(--color-border);
}
.vhd {
  font-size: 0.68rem;
  color: var(--color-faint);
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.04em;
  background: var(--color-card-2);
  padding-top: 8px; padding-bottom: 8px;
}
.vk, .vv, .vsel {
  height: 32px;
  border: 1px solid var(--color-border-strong);
  background: var(--color-inset);
  color: var(--color-text);
  border-radius: 8px;
  padding: 0 10px;
  font-size: 0.79rem;
  width: 100%;
}
.vk:focus, .vv:focus, .vsel:focus { outline: none; border-color: var(--color-primary); box-shadow: 0 0 0 3px var(--color-primary-soft); }
.vsel { font-family: var(--font-sans); cursor: pointer; }

.vfrom {
  height: 28px;
  border-radius: 7px;
  font-size: 0.7rem;
  font-weight: 500;
  padding: 0 9px;
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  white-space: nowrap;
  font-family: var(--font-sans);
}
.vfrom--plain { color: var(--color-dim); background: var(--color-inset); border: 1px solid var(--color-border); }
.vfrom--vault { color: var(--color-cyan); background: var(--color-cyan-soft); border: 1px solid var(--color-cyan-line); }
.vfrom:hover:not(:disabled) { filter: brightness(1.1); }
.vfrom:focus-visible { outline: 2px solid var(--color-primary); outline-offset: 1px; }
.vfrom .mask { letter-spacing: 0.06em; }

.vdel {
  width: 30px; height: 30px;
  border: 1px solid var(--color-border);
  background: transparent;
  color: var(--color-faint);
  border-radius: 7px;
  cursor: pointer;
  display: grid; place-items: center;
}
.vdel:hover:not(:disabled) { color: var(--color-red); border-color: var(--color-red-line); }
.vdel:focus-visible { outline: 2px solid var(--color-primary); }

.vempty { padding: 18px; font-size: 0.8rem; color: var(--color-faint); text-align: center; }

.addrow { display: flex; gap: 8px; padding: 12px 18px; flex-wrap: wrap; }
.addbtn {
  display: inline-flex; align-items: center; gap: 6px;
  font-size: 0.79rem; font-weight: 500;
  color: var(--color-primary);
  background: transparent;
  border: 1px dashed var(--color-border-strong);
  border-radius: 8px;
  padding: 7px 12px;
  cursor: pointer;
  font-family: var(--font-sans);
}
.addbtn:hover:not(:disabled) { border-color: var(--color-primary); background: var(--color-primary-soft); }
.addbtn:focus-visible { outline: 2px solid var(--color-primary); outline-offset: 2px; }
.addbtn--vault { color: var(--color-cyan); }
.addbtn--vault:hover:not(:disabled) { border-color: var(--color-cyan-line); background: var(--color-cyan-soft); }

/* ─── Pipeline-as-code variables ─────────────────────────────────────────── */
.pac-src {
  padding: 10px 18px 0;
  font-size: 0.72rem;
  color: var(--color-faint);
}
.pachd, .pacrow {
  display: grid;
  grid-template-columns: 200px 1fr 1.2fr;
  gap: 12px;
  align-items: center;
  padding: 9px 18px;
  border-bottom: 1px solid var(--color-border);
}
.pachd {
  font-size: 0.68rem;
  color: var(--color-faint);
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.04em;
  background: var(--color-card-2);
  padding-top: 8px; padding-bottom: 8px;
}
.pacrow--missing { background: var(--color-amber-soft, var(--color-inset)); }
.pac-name {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  min-width: 0;
}
.pac-name code {
  font-size: 0.78rem;
  font-weight: 600;
  color: var(--color-text);
  background: var(--color-inset);
  border-radius: 4px;
  padding: 1px 5px;
}
.pacbadge {
  flex: none;
  font-size: 0.62rem;
  font-weight: 600;
  border-radius: 4px;
  padding: 1px 6px;
  border: 1px solid transparent;
}
.pacbadge--req {
  color: var(--color-amber, var(--color-dim));
  background: var(--color-amber-soft, var(--color-inset));
  border-color: var(--color-amber-line, var(--color-border));
}
.pacbadge--sec {
  color: var(--color-cyan);
  background: var(--color-cyan-soft);
  border-color: var(--color-cyan-line);
}
.pac-desc p { margin: 0; font-size: 0.76rem; color: var(--color-dim); line-height: 1.45; overflow-wrap: anywhere; }
.pac-default { margin-top: 2px !important; color: var(--color-faint) !important; font-size: 0.72rem !important; }
.pac-val { display: flex; align-items: center; gap: 8px; min-width: 0; }
.pac-val .vsel, .pac-val .vv { width: 100%; }
.pac-mask { font-size: 0.72rem; color: var(--color-faint); letter-spacing: 0.06em; flex: none; }

/* ─── Cache ──────────────────────────────────────────────────────────────── */
.cache-toggle { display: flex; align-items: center; gap: 14px; padding: 14px 18px; border-bottom: 1px solid var(--color-border); }
.sw {
  width: 40px; height: 23px;
  border-radius: 100px;
  background: var(--color-primary);
  position: relative;
  cursor: pointer;
  flex: none;
  border: none;
  padding: 0;
  transition: background-color var(--duration-fast);
}
.sw-knob { position: absolute; top: 2.5px; left: 19px; width: 18px; height: 18px; border-radius: 50%; background: #fff; transition: left var(--duration-fast); }
.sw--off { background: var(--color-border-strong); }
.sw--off .sw-knob { left: 2.5px; }
.sw:focus-visible { outline: 2px solid var(--color-primary); outline-offset: 2px; }
.cache-tx b { font-size: 0.83rem; font-weight: 500; }
.cache-tx span { display: block; font-size: 0.73rem; color: var(--color-faint); margin-top: 2px; }

.cache-paths { padding: 14px 18px; transition: opacity var(--duration-fast); }
.cache-paths--off { opacity: 0.5; }
.cache-area { height: auto; padding: 8px 11px; line-height: 1.6; resize: vertical; }
</style>
