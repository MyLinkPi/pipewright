<script setup lang="ts">
/*
  AppStore.vue — 应用商店(DPanel 式一键部署)。

  模板卡片网格(内置 + 自定义)→ 点开参数表单(按 params schema 渲染;secret 自动生成)
  → 选目标主机部署 → 展示部署结果与一次性返回的生成口令。部署复用 Stacks 受管链路,
  部署完可在「容器」页对应主机的 Stacks 标签页管理。
*/
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { Apps, Rocket, Trash } from '@vicons/tabler'
import {
  listAppTemplates,
  createAppTemplate,
  deleteAppTemplate,
  deployApp,
  type AppTemplate,
  type AppParamSpec,
  type AppDeployResult,
} from '../api/apps'
import { listServers, type Server } from '../api/servers'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import ErrorState from '../components/ui/ErrorState.vue'
import AppButton from '../components/ui/AppButton.vue'
import AppSelect from '../components/ui/AppSelect.vue'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

function errMsg(err: unknown, fallback: string): string {
  return err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback
}

// ─── 模板与主机加载 ───────────────────────────────────────────────────────────
const templates = ref<AppTemplate[]>([])
const servers = ref<Server[]>([])
const loadState = ref<'loading' | 'loaded' | 'error'>('loading')
const loadError = ref('')

async function load(): Promise<void> {
  loadState.value = templates.value.length === 0 ? 'loading' : 'loaded'
  loadError.value = ''
  try {
    const [tpl, sv] = await Promise.all([listAppTemplates(), listServers()])
    templates.value = tpl.items
    servers.value = sv
    loadState.value = 'loaded'
  } catch (err) {
    loadState.value = templates.value.length === 0 ? 'error' : 'loaded'
    loadError.value = errMsg(err, t('appStore.errLoad'))
  }
}
onMounted(load)

const builtinTemplates = computed(() => templates.value.filter((x) => x.builtin))
const customTemplates = computed(() => templates.value.filter((x) => !x.builtin))

// ─── 部署对话框 ───────────────────────────────────────────────────────────────
const deploying = ref<AppTemplate | null>(null)
const deployServer = ref('')
const paramValues = ref<Record<string, string>>({})
const deployCpus = ref('')
const deployMemory = ref('')
const deployBusy = ref(false)
const deployResult = ref<AppDeployResult | null>(null)

const serverOptions = computed(() => servers.value.map((s) => ({ value: s.id, label: s.name })))
const missingRequired = computed(() => {
  if (!deploying.value) return false
  return deploying.value.params.some(
    (p) =>
      p.required &&
      !p.autoGenerate &&
      !(paramValues.value[p.name] ?? '').trim() &&
      !(p.default ?? '').trim(),
  )
})

function openDeploy(tpl: AppTemplate): void {
  deploying.value = tpl
  deployResult.value = null
  deployCpus.value = ''
  deployMemory.value = ''
  deployServer.value = servers.value.length === 1 ? servers.value[0].id : ''
  const vals: Record<string, string> = {}
  for (const p of tpl.params) vals[p.name] = p.default ?? ''
  paramValues.value = vals
}

async function submitDeploy(): Promise<void> {
  if (!deploying.value || !deployServer.value) return
  deployBusy.value = true
  try {
    const res = await deployApp(deployServer.value, deploying.value.id, paramValues.value, {
      cpus: deployCpus.value,
      memory: deployMemory.value,
    })
    deployResult.value = res
    if (res.ok) toast.success(t('appStore.deployed', { name: deploying.value.displayName || deploying.value.name }))
    else toast.error(res.error || t('appStore.deployFail'))
  } catch (err) {
    toast.error(errMsg(err, t('appStore.deployFail')))
  } finally {
    deployBusy.value = false
  }
}

// ─── 自定义模板 ───────────────────────────────────────────────────────────────
const showCustomForm = ref(false)
const customForm = ref({
  name: '',
  displayName: '',
  description: '',
  icon: '',
  composeYaml: '',
  paramsText: '',
})
const customBusy = ref(false)

async function submitCustom(): Promise<void> {
  customBusy.value = true
  try {
    // params 用简洁行式语法:name:type:default(required 标 *;secret 空缺自动生成)。
    const params: AppParamSpec[] = []
    for (const line of customForm.value.paramsText.split('\n')) {
      const raw = line.trim()
      if (!raw) continue
      const required = raw.startsWith('*')
      const body = required ? raw.slice(1).trim() : raw
      const [name, type = 'string', def = ''] = body.split(':').map((x) => x.trim())
      params.push({
        name,
        type: type === 'int' || type === 'secret' ? type : 'string',
        default: def,
        required,
        autoGenerate: type === 'secret',
      })
    }
    await createAppTemplate({
      name: customForm.value.name,
      displayName: customForm.value.displayName,
      description: customForm.value.description,
      icon: customForm.value.icon,
      composeYaml: customForm.value.composeYaml,
      params,
    })
    showCustomForm.value = false
    customForm.value = { name: '', displayName: '', description: '', icon: '', composeYaml: '', paramsText: '' }
    await load()
    toast.success(t('appStore.templateCreated'))
  } catch (err) {
    toast.error(errMsg(err, t('appStore.templateCreateFail')))
  } finally {
    customBusy.value = false
  }
}

async function removeCustom(tpl: AppTemplate): Promise<void> {
  const ok = await confirm.open({
    title: t('appStore.delTemplateTitle'),
    body: tpl.displayName || tpl.name,
    confirmLabel: t('serviceReg.btn.delete'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteAppTemplate(tpl.id)
    await load()
    toast.success(t('appStore.templateDeleted'))
  } catch (err) {
    toast.error(errMsg(err, t('appStore.errOp')))
  }
}
</script>

<template>
  <div class="apps">
    <header class="apps__header">
      <div>
        <span class="apps__eyebrow">
          <NIcon :size="13"><Apps /></NIcon>
          {{ t('appStore.eyebrow') }}
        </span>
        <h1 class="apps__title">{{ t('appStore.title') }}</h1>
        <p class="apps__sub">{{ t('appStore.subtitle') }}</p>
      </div>
      <div class="apps__actions">
        <AppButton variant="default" :disabled="loadState === 'loading'" @click="load">
          {{ t('common.refresh') }}
        </AppButton>
        <AppButton variant="primary" @click="showCustomForm = !showCustomForm">
          {{ t('appStore.newTemplate') }}
        </AppButton>
      </div>
    </header>

    <div v-if="loadState === 'loading'" aria-busy="true">{{ t('appStore.loading') }}</div>
    <ErrorState v-else-if="loadState === 'error'" :title="t('appStore.errTitle')" :description="loadError" @retry="load" />

    <template v-else>
      <!-- 内置模板 -->
      <section v-if="builtinTemplates.length > 0">
        <h2 class="apps__section">{{ t('appStore.builtinSection') }}</h2>
        <div class="apps__grid">
          <article v-for="tpl in builtinTemplates" :key="tpl.id" class="apps__card">
            <span class="apps__icon" aria-hidden="true">{{ tpl.icon || '📦' }}</span>
            <div class="apps__card-body">
              <h3 class="apps__card-title">{{ tpl.displayName || tpl.name }}</h3>
              <p class="apps__card-desc">{{ tpl.description }}</p>
            </div>
            <AppButton variant="primary" :disabled="servers.length === 0" @click="openDeploy(tpl)">
              <NIcon :size="14"><Rocket /></NIcon>&nbsp;{{ t('appStore.deployBtn') }}
            </AppButton>
          </article>
        </div>
      </section>

      <!-- 自定义模板 -->
      <section>
        <h2 class="apps__section">{{ t('appStore.customSection') }}</h2>
        <div v-if="customTemplates.length === 0" class="apps__empty-custom">
          {{ t('appStore.customEmpty') }}
        </div>
        <div v-else class="apps__grid">
          <article v-for="tpl in customTemplates" :key="tpl.id" class="apps__card">
            <span class="apps__icon" aria-hidden="true">{{ tpl.icon || '📦' }}</span>
            <div class="apps__card-body">
              <h3 class="apps__card-title">{{ tpl.displayName || tpl.name }}</h3>
              <p class="apps__card-desc">{{ tpl.description }}</p>
            </div>
            <div class="apps__card-actions">
              <AppButton variant="primary" :disabled="servers.length === 0" @click="openDeploy(tpl)">
                <NIcon :size="14"><Rocket /></NIcon>&nbsp;{{ t('appStore.deployBtn') }}
              </AppButton>
              <AppButton variant="danger" @click="removeCustom(tpl)"><NIcon :size="13"><Trash /></NIcon></AppButton>
            </div>
          </article>
        </div>

        <!-- 新建自定义模板 -->
        <form v-if="showCustomForm" class="apps__form" @submit.prevent="submitCustom">
          <div class="apps__grid2">
            <label class="field">
              <span class="field__lbl">{{ t('appStore.form.name') }}</span>
              <input v-model="customForm.name" class="field__in" type="text" placeholder="my-app" required />
            </label>
            <label class="field">
              <span class="field__lbl">{{ t('appStore.form.displayName') }}</span>
              <input v-model="customForm.displayName" class="field__in" type="text" />
            </label>
            <label class="field">
              <span class="field__lbl">{{ t('appStore.form.icon') }}</span>
              <input v-model="customForm.icon" class="field__in" type="text" placeholder="📦" />
            </label>
            <label class="field">
              <span class="field__lbl">{{ t('appStore.form.description') }}</span>
              <input v-model="customForm.description" class="field__in" type="text" />
            </label>
          </div>
          <label class="field">
            <span class="field__lbl">{{ t('appStore.form.compose') }}</span>
            <textarea v-model="customForm.composeYaml" class="field__in apps__ta" rows="8" required></textarea>
          </label>
          <label class="field">
            <span class="field__lbl">{{ t('appStore.form.params') }}</span>
            <textarea
              v-model="customForm.paramsText"
              class="field__in apps__ta"
              rows="3"
              placeholder="*password:secret&#10;port:int:8080"
            ></textarea>
            <span class="apps__hint">{{ t('appStore.form.paramsHint') }}</span>
          </label>
          <div class="apps__form-actions">
            <AppButton variant="primary" type="submit" :loading="customBusy">{{ t('serviceReg.btn.create') }}</AppButton>
            <AppButton variant="default" @click="showCustomForm = false">{{ t('serviceReg.btn.cancel') }}</AppButton>
          </div>
        </form>
      </section>
    </template>

    <!-- 部署对话框(内联卡) -->
    <div v-if="deploying" class="apps__modal" role="dialog" aria-modal="true">
      <div class="apps__modal-card">
        <header class="apps__modal-head">
          <span class="apps__icon" aria-hidden="true">{{ deploying.icon || '📦' }}</span>
          <h3>{{ deploying.displayName || deploying.name }}</h3>
          <button class="apps__modal-close" :aria-label="t('serviceReg.btn.cancel')" @click="deploying = null">×</button>
        </header>

        <form v-if="!deployResult" class="apps__form" @submit.prevent="submitDeploy">
          <label class="field">
            <span class="field__lbl">{{ t('appStore.deploy.target') }}</span>
            <AppSelect
              :model-value="deployServer"
              :options="serverOptions"
              :placeholder="t('appStore.deploy.pickTarget')"
              @update:model-value="(v: string) => (deployServer = v)"
            />
          </label>
          <label v-for="p in deploying.params" :key="p.name" class="field">
            <span class="field__lbl">
              {{ p.label || p.name }}
              <span v-if="p.required" class="apps__req">*</span>
              <span v-if="p.autoGenerate" class="apps__hint">· {{ t('appStore.deploy.autogen') }}</span>
            </span>
            <input
              v-if="p.type !== 'secret' || !p.autoGenerate"
              v-model="paramValues[p.name]"
              class="field__in"
              :type="p.type === 'secret' ? 'password' : p.type === 'int' ? 'number' : 'text'"
              :required="p.required && !p.autoGenerate && !p.default"
            />
            <input v-else class="field__in" type="password" :value="t('appStore.deploy.autoValue')" disabled />
          </label>
          <div class="apps__grid2">
            <label class="field">
              <span class="field__lbl">{{ t('appStore.deploy.cpuLimit') }}</span>
              <input v-model="deployCpus" class="field__in" type="text" placeholder="1.0" />
            </label>
            <label class="field">
              <span class="field__lbl">{{ t('appStore.deploy.memoryLimit') }}</span>
              <input v-model="deployMemory" class="field__in" type="text" placeholder="512m" />
            </label>
          </div>
          <p class="apps__hint">{{ t('appStore.deploy.limitsHint') }}</p>
          <p class="apps__hint">{{ t('appStore.deploy.hint') }}</p>
          <div class="apps__form-actions">
            <AppButton variant="primary" type="submit" :loading="deployBusy" :disabled="!deployServer || missingRequired">
              <NIcon :size="14"><Rocket /></NIcon>&nbsp;{{ t('appStore.deploy.submit') }}
            </AppButton>
            <AppButton variant="default" @click="deploying = null">{{ t('serviceReg.btn.cancel') }}</AppButton>
          </div>
        </form>

        <div v-else class="apps__result">
          <p :class="deployResult.ok ? 'apps__ok' : 'apps__fail'">
            {{ deployResult.ok ? t('appStore.deploy.ok') : t('appStore.deploy.fail') }}
          </p>
          <p v-if="deployResult.error" class="apps__fail-detail">{{ deployResult.error }}</p>
          <div v-if="deployResult.ok && deployResult.params" class="apps__params">
            <h4>{{ t('appStore.deploy.generated') }}</h4>
            <ul>
              <li v-for="(v, k) in deployResult.params" :key="k">
                <code>{{ k }}</code> = <code class="apps__secret">{{ v }}</code>
              </li>
            </ul>
            <p class="apps__hint">{{ t('appStore.deploy.onceHint') }}</p>
          </div>
          <pre v-if="deployResult.output" class="apps__output">{{ deployResult.output }}</pre>
          <div class="apps__form-actions">
            <AppButton variant="default" @click="deploying = null">{{ t('appStore.deploy.close') }}</AppButton>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.apps {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.apps__header {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.apps__eyebrow {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  text-transform: uppercase;
  letter-spacing: 0.08em;
  opacity: 0.7;
}
.apps__title {
  margin: 4px 0 2px;
  font-size: 22px;
}
.apps__sub {
  margin: 0;
  font-size: 13px;
  opacity: 0.75;
}
.apps__actions {
  display: flex;
  gap: 8px;
}
.apps__section {
  font-size: 15px;
  margin: 8px 0 0;
}
.apps__grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  gap: 12px;
  margin-top: 10px;
}
.apps__card {
  border: 1px solid var(--border, #e2e2e2);
  border-radius: 10px;
  padding: 14px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.apps__icon {
  font-size: 26px;
  line-height: 1;
}
.apps__card-body {
  flex: 1;
}
.apps__card-title {
  margin: 0 0 4px;
  font-size: 15px;
}
.apps__card-desc {
  margin: 0;
  font-size: 12px;
  opacity: 0.7;
}
.apps__card-actions {
  display: flex;
  gap: 8px;
}
.apps__empty-custom {
  font-size: 13px;
  opacity: 0.6;
  padding: 10px 0;
}
.apps__form {
  display: flex;
  flex-direction: column;
  gap: 10px;
  border: 1px dashed var(--border, #d4d4d4);
  border-radius: 8px;
  padding: 12px;
  margin-top: 10px;
}
.apps__grid2 {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 10px;
}
.field {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.field__lbl {
  font-size: 12px;
  opacity: 0.75;
}
.field__in {
  padding: 7px 10px;
  border: 1px solid var(--border, #d4d4d4);
  border-radius: 7px;
  font-size: 13px;
  background: var(--bg, inherit);
  color: inherit;
}
.apps__ta {
  font-family: ui-monospace, monospace;
  resize: vertical;
}
.apps__hint {
  font-size: 11px;
  opacity: 0.6;
}
.apps__req {
  color: #b3261e;
}
.apps__form-actions {
  display: flex;
  gap: 8px;
}
.apps__modal {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.35);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 16px;
  z-index: 50;
}
.apps__modal-card {
  background: var(--bg, #fff);
  border-radius: 12px;
  padding: 18px;
  width: min(760px, 100%);
  max-height: 85vh;
  overflow: auto;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.apps__modal-head {
  display: flex;
  align-items: center;
  gap: 10px;
}
.apps__modal-head h3 {
  margin: 0;
  flex: 1;
  font-size: 16px;
}
.apps__modal-close {
  border: none;
  background: none;
  font-size: 20px;
  cursor: pointer;
  color: inherit;
  opacity: 0.6;
}
.apps__ok {
  color: #0a7d32;
  font-weight: 600;
  margin: 0;
}
.apps__fail {
  color: #b3261e;
  font-weight: 600;
  margin: 0;
}
.apps__fail-detail {
  font-size: 12px;
  color: #b3261e;
  white-space: pre-wrap;
  margin: 0;
}
.apps__params h4 {
  margin: 0 0 6px;
  font-size: 13px;
}
.apps__params ul {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 12px;
}
.apps__secret {
  word-break: break-all;
}
.apps__output {
  font-size: 11px;
  background: var(--code-bg, #f4f4f4);
  border-radius: 6px;
  padding: 8px;
  overflow: auto;
  max-height: 200px;
  margin: 0;
}
</style>
