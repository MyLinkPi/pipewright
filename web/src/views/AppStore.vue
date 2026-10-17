<script setup lang="ts">
/*
  AppStore.vue — 应用商店(DPanel 式一键部署)。

  模板卡片网格(内置 + 自定义)→ 点开参数表单(按 params schema 渲染;secret 自动生成)
  → 选目标主机部署 → 展示部署结果与一次性返回的生成口令。部署复用 Stacks 受管链路,
  部署完可在「容器」页对应主机的 Stacks 标签页管理。
  部署成功后结果卡内置「一键服务注册」:选基域 + 服务名/端口(自动预填)→ 幂等注册
  网关子域名并挂载本机实例,nginx 自动收敛,完成态展示 FQDN。
*/
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
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
import { getServerStacks } from '../api/containers'
import {
  listDomains,
  getGateway,
  ensureService,
  listInstances,
  addInstance,
  removeInstance,
  type Domain,
} from '../api/servicereg'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import ErrorState from '../components/ui/ErrorState.vue'
import AppButton from '../components/ui/AppButton.vue'
import AppSelect from '../components/ui/AppSelect.vue'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()
const router = useRouter()

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

// 实例名(可选):生效栈名 = 模板名(留空)或 模板名-实例名,同机多副本按 compose 项目前缀隔离。
const deployInstance = ref('')
const stackNames = ref<string[] | null>(null) // null = 未拉取到(未选机器/不可达),提醒静默跳过
const instanceRe = /^[\w][\w.-]*$/ // 与后端 Stacks 项目名同规则
const instanceInvalid = computed(() => {
  const inst = deployInstance.value.trim()
  return inst !== '' && !instanceRe.test(inst)
})
const effectiveStackName = computed(() => {
  if (!deploying.value) return ''
  const inst = deployInstance.value.trim()
  return inst ? `${deploying.value.name}-${inst}` : deploying.value.name
})
// 与目标机现有栈重名(大小写不敏感:compose v2 会把项目名转小写)→ 非阻断提醒。
const stackNameTaken = computed(() => {
  if (!stackNames.value) return false
  const want = effectiveStackName.value.toLowerCase()
  return stackNames.value.some((n) => n.toLowerCase() === want)
})

// loadStackNames 拉取目标机现有栈名(提醒仅建议性;拉取失败 → 静默跳过)。
async function loadStackNames(serverId: string): Promise<void> {
  stackNames.value = null
  if (!serverId) return
  try {
    const res = await getServerStacks(serverId)
    stackNames.value = res.stacks.map((s) => s.name)
  } catch {
    stackNames.value = null
  }
}
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

// sshDomainAuto 记录上次为 ssh_domain 自动预填的值:当前值与之相等 = 用户未手改,
// 切换目标机时跟随刷新;用户改过的自定义值不覆盖。
const sshDomainAuto = ref('')

// prefillHostParams 选中目标机后为 ssh_domain 预填该机地址(为空或未手改时):SSH 克隆
// 地址主机缺省回落 localhost;后端对留空的约定参数也会注入目标机 Host,此处预填让用户
// 看得见生效值、切换机器自动跟随。
function prefillHostParams(tpl: AppTemplate, serverId: string): void {
  if (!tpl.params.some((p) => p.name === 'ssh_domain')) return
  const srv = servers.value.find((s) => s.id === serverId)
  if (!srv) return
  const cur = (paramValues.value.ssh_domain ?? '').trim()
  if (cur === '' || cur === sshDomainAuto.value) {
    paramValues.value.ssh_domain = srv.host
    sshDomainAuto.value = srv.host
  }
}

function onDeployServer(v: string): void {
  deployServer.value = v
  void loadStackNames(v)
  if (deploying.value) prefillHostParams(deploying.value, v)
}

function openDeploy(tpl: AppTemplate): void {
  deploying.value = tpl
  deployResult.value = null
  deployCpus.value = ''
  deployMemory.value = ''
  deployInstance.value = ''
  stackNames.value = null
  regDoneFqdn.value = ''
  sshDomainAuto.value = ''
  deployServer.value = servers.value.length === 1 ? servers.value[0].id : ''
  const vals: Record<string, string> = {}
  for (const p of tpl.params) vals[p.name] = p.default ?? ''
  paramValues.value = vals
  if (deployServer.value) {
    prefillHostParams(tpl, deployServer.value)
    void loadStackNames(deployServer.value)
  }
}

async function submitDeploy(): Promise<void> {
  if (!deploying.value || !deployServer.value) return
  deployBusy.value = true
  try {
    const res = await deployApp(
      deployServer.value,
      deploying.value.id,
      paramValues.value,
      {
        cpus: deployCpus.value,
        memory: deployMemory.value,
      },
      deployInstance.value,
    )
    deployResult.value = res
    if (res.ok) {
      toast.success(t('appStore.deployed', { name: deploying.value.displayName || deploying.value.name }))
      void prepareReg()
    } else {
      toast.error(res.error || t('appStore.deployFail'))
    }
  } catch (err) {
    toast.error(errMsg(err, t('appStore.deployFail')))
  } finally {
    deployBusy.value = false
  }
}

// ─── 一键服务注册(部署成功后的网关联动)──────────────────────────────────────
// 复用服务注册既有端点:ensureService 幂等建服务(子域名),addInstance 把部署目标机
// 挂为 upstream 成员(非容器实例:服务器 IP + 宿主端口),触发网关 nginx 自动收敛。
const regDomains = ref<Domain[]>([])
const regLoaded = ref(false) // 基域/网关状态是否拉取完成(区分「无基域」与「加载中」)
const regGatewayReady = ref<boolean | null>(null) // null = 未知
const regForm = ref({ domainId: '', name: '', port: '' })
const regBusy = ref(false)
const regDoneFqdn = ref('') // 非空 = 已注册成功,切换结果卡为完成态

const domainOptions = computed(() => regDomains.value.map((d) => ({ value: d.id, label: d.baseDomain })))
const regDomain = computed(() => regDomains.value.find((d) => d.id === regForm.value.domainId))
const regPreview = computed(() => {
  const d = regDomain.value
  const name = regForm.value.name.trim()
  return d && name ? `${name}.${d.baseDomain}` : ''
})

// sanitizeServiceName 把模板名清洗成合法子域前缀(^[a-z0-9][a-z0-9-]{0,62}$)。
function sanitizeServiceName(raw: string): string {
  return raw
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, '-')
    .replace(/^-+/, '')
    .replace(/-+$/, '')
    .slice(0, 63)
}

// pickPortParam 从部署生效参数启发式取宿主端口:键 port 优先,其次 *port;须为 1-65535 整数。
function pickPortParam(params?: Record<string, string>): string {
  if (!params) return ''
  const names = Object.keys(params)
  const key = names.includes('port') ? 'port' : names.find((k) => k.endsWith('port'))
  if (!key) return ''
  const n = Number(params[key])
  return Number.isInteger(n) && n >= 1 && n <= 65535 ? String(n) : ''
}

// prepareReg 部署成功后预取基域与网关状态并预填表单(尽力而为,失败不影响结果展示)。
async function prepareReg(): Promise<void> {
  regDomains.value = []
  regLoaded.value = false
  regGatewayReady.value = null
  regForm.value = {
    domainId: '',
    // 服务名预填用生效栈名(多副本注册不重名),退回模板名。
    name: sanitizeServiceName(deployResult.value?.name || deploying.value?.name || ''),
    port: pickPortParam(deployResult.value?.params),
  }
  regDoneFqdn.value = ''
  try {
    const [dom, gw] = await Promise.all([listDomains(), getGateway()])
    regDomains.value = dom.items
    regGatewayReady.value = gw.configured && gw.servers.some((s) => s.running)
    if (dom.items.length === 1) regForm.value.domainId = dom.items[0].id
  } catch {
    regDomains.value = []
  } finally {
    regLoaded.value = true
  }
}

// submitRegister 一键注册:幂等建服务 → 幂等挂实例(同机已有非容器实例行:端口一致
// 跳过,变了先删后加刷新)→ 完成态展示 FQDN。仅 HTTP 协议;TCP 走服务注册页完整表单。
async function submitRegister(): Promise<void> {
  const res = deployResult.value
  if (!res || regBusy.value) return
  const name = regForm.value.name.trim()
  const port = Number(regForm.value.port)
  if (!regForm.value.domainId || !name || !Number.isInteger(port) || port < 1 || port > 65535) {
    toast.error(t('appStore.deploy.regNeedFields'))
    return
  }
  regBusy.value = true
  try {
    const svc = await ensureService({
      domainId: regForm.value.domainId,
      name,
      protocol: 'http',
      upstreamKind: 'address',
      upstream: '',
      upstreamPort: port,
    })
    const insts = await listInstances(svc.id)
    const stale = insts.items.find((i) => i.serverId === res.serverId && !i.container)
    if (!stale || stale.hostPort !== port) {
      if (stale) await removeInstance(stale.id)
      await addInstance(svc.id, res.serverId, '', port, port)
    }
    regDoneFqdn.value = regPreview.value || name
    toast.success(t('appStore.deploy.regDoneTitle'))
  } catch (err) {
    toast.error(errMsg(err, t('appStore.deploy.regFail')))
  } finally {
    regBusy.value = false
  }
}

function goServiceReg(): void {
  router.push({ name: 'service-registry' })
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
              @update:model-value="onDeployServer"
            />
          </label>
          <label class="field">
            <span class="field__lbl">{{ t('appStore.deploy.instance') }}</span>
            <input v-model="deployInstance" class="field__in" type="text" placeholder="2" />
            <span class="apps__hint">{{ t('appStore.deploy.instanceHint') }}</span>
          </label>
          <p v-if="instanceInvalid" class="apps__sr-warn">{{ t('appStore.deploy.instanceInvalid') }}</p>
          <p v-else-if="stackNameTaken" class="apps__sr-warn">
            {{ t('appStore.deploy.stackTaken', { name: effectiveStackName }) }}
          </p>
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
            <AppButton
              variant="primary"
              type="submit"
              :loading="deployBusy"
              :disabled="!deployServer || missingRequired || instanceInvalid"
            >
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
          <!-- RustFS 联动:部署成功后自动登记对象存储的结果(尽力而为,失败不影响部署) -->
          <p v-if="deployResult.objectStoreRegistered" class="apps__reg-ok">
            {{ t('appStore.deploy.autoRegistered') }}<template v-if="deployResult.objectStoreNote"> · {{ deployResult.objectStoreNote }}</template>
          </p>
          <p v-else-if="deployResult.objectStoreNote" class="apps__reg-warn">{{ deployResult.objectStoreNote }}</p>
          <div v-if="deployResult.ok && deployResult.params" class="apps__params">
            <h4>{{ t('appStore.deploy.generated') }}</h4>
            <ul>
              <li v-for="(v, k) in deployResult.params" :key="k">
                <code>{{ k }}</code> = <code class="apps__secret">{{ v }}</code>
              </li>
            </ul>
            <p class="apps__hint">{{ t('appStore.deploy.onceHint') }}</p>
          </div>

          <!-- 一键服务注册:部署成功后把应用挂到网关子域名(幂等 ensure + 实例挂载自动收敛) -->
          <div v-if="deployResult.ok && regDoneFqdn" class="apps__sr">
            <h4>{{ t('appStore.deploy.regDoneTitle') }}</h4>
            <p class="apps__sr-fqdn"><code>{{ regDoneFqdn }}</code></p>
            <p v-if="regDomain?.hasCert" class="apps__sr-ok">{{ t('appStore.deploy.regHttps') }}</p>
            <p class="apps__hint">{{ t('appStore.deploy.regDoneHint') }}</p>
            <button type="button" class="apps__sr-link" @click="goServiceReg">
              {{ t('appStore.deploy.regGoServiceReg') }} →
            </button>
          </div>
          <div v-else-if="deployResult.ok" class="apps__sr">
            <h4>{{ t('appStore.deploy.regTitle') }}</h4>
            <p class="apps__hint">{{ t('appStore.deploy.regHint') }}</p>
            <template v-if="regDomains.length">
              <div class="apps__grid2">
                <label class="field">
                  <span class="field__lbl">{{ t('appStore.deploy.regDomain') }}</span>
                  <AppSelect
                    :model-value="regForm.domainId"
                    :options="domainOptions"
                    placeholder="example.com"
                    @update:model-value="(v: string) => (regForm.domainId = v)"
                  />
                </label>
                <label class="field">
                  <span class="field__lbl">{{ t('appStore.deploy.regPort') }}</span>
                  <input v-model="regForm.port" class="field__in" type="number" min="1" max="65535" />
                </label>
              </div>
              <label class="field">
                <span class="field__lbl">{{ t('appStore.deploy.regName') }}</span>
                <input v-model="regForm.name" class="field__in" type="text" :placeholder="deploying.name" />
              </label>
              <p class="apps__hint">{{ t('appStore.deploy.regPortHint') }}</p>
              <p v-if="regPreview" class="apps__sr-preview"><code>{{ regPreview }}</code></p>
              <p v-if="regGatewayReady === false" class="apps__sr-warn">{{ t('appStore.deploy.regGatewayMissing') }}</p>
              <div class="apps__form-actions">
                <AppButton
                  variant="primary"
                  type="button"
                  :loading="regBusy"
                  :disabled="!regForm.domainId || !regForm.name.trim() || !regForm.port"
                  @click="submitRegister"
                >
                  {{ t('appStore.deploy.regSubmit') }}
                </AppButton>
              </div>
            </template>
            <template v-else-if="regLoaded">
              <p class="apps__hint">{{ t('appStore.deploy.regNoDomains') }}</p>
              <div class="apps__form-actions">
                <AppButton variant="default" type="button" @click="goServiceReg">
                  {{ t('appStore.deploy.regGoServiceReg') }}
                </AppButton>
              </div>
            </template>
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
.apps__reg-ok {
  font-size: 12px;
  color: #0a7d32;
  margin: 0;
}
.apps__reg-warn {
  font-size: 12px;
  color: #9a6700;
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
.apps__sr {
  display: flex;
  flex-direction: column;
  gap: 8px;
  border: 1px solid var(--border, #d4d4d4);
  border-radius: 8px;
  padding: 10px 12px;
}
.apps__sr h4 {
  margin: 0;
  font-size: 13px;
}
.apps__sr-fqdn {
  margin: 0;
  font-size: 12px;
}
.apps__sr-fqdn code {
  font-size: 13px;
  word-break: break-all;
}
.apps__sr-preview {
  margin: 0;
  font-size: 12px;
}
.apps__sr-ok {
  font-size: 12px;
  color: #0a7d32;
  margin: 0;
}
.apps__sr-warn {
  font-size: 12px;
  color: #9a6700;
  margin: 0;
}
.apps__sr-link {
  align-self: flex-start;
  background: none;
  border: none;
  padding: 0;
  font-size: 12px;
  color: #1a66d4;
  cursor: pointer;
}
.apps__sr-link:hover {
  text-decoration: underline;
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
