<script setup lang="ts">
/*
  ServiceRegistry.vue — 服务注册网关(nginx)。

  三个区块:
  1. 网关设置:选网关主机/端口/镜像/资源名,部署/移除容器,证书上传 token 管理,最近 apply 状态。
  2. 基域:注册 efg.com,上传泛域名证书(到期时间展示),删除。
  3. 服务:注册 abc → abc.efg.com(HTTP/TCP 反代),启停/删除,手动全量收敛。

  约定:*.efg.com 由用户自行解析到网关主机 IP;未配网关时全部操作为配置态(不触网)。
*/
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { World, Plus, Trash, Refresh, ShieldLock, Key, Server as ServerIcon, ExternalLink } from '@vicons/tabler'
import {
  getServiceRegSettings,
  updateServiceRegSettings,
  generateUploadToken,
  revokeUploadToken,
  getGateway,
  deployGateway,
  removeGateway,
  listDomains,
  createDomain,
  deleteDomain,
  uploadCert,
  listServices,
  createService,
  deleteService,
  setServiceEnabled,
  applyServiceReg,
  listInstances,
  addInstance,
  removeInstance,
  setInstanceAttached,
  type Settings,
  type Domain,
  type RegisteredService,
  type Gateway,
  type ServiceInstance,
  type ServiceProtocol,
  type UpstreamKind,
} from '../api/servicereg'
import { listServers, type Server } from '../api/servers'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import EmptyState from '../components/ui/EmptyState.vue'
import ErrorState from '../components/ui/ErrorState.vue'
import AppButton from '../components/ui/AppButton.vue'
import AppSelect from '../components/ui/AppSelect.vue'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

function errMsg(err: unknown, fallback: string): string {
  return err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback
}

// ─── 数据 ────────────────────────────────────────────────────────────────────
const loadError = ref('')
const loading = ref(true)
const settings = ref<Settings | null>(null)
const gateway = ref<Gateway | null>(null)
const domains = ref<Domain[]>([])
const services = ref<RegisteredService[]>([])
const servers = ref<Server[]>([])

async function load(): Promise<void> {
  loading.value = domains.value.length === 0
  loadError.value = ''
  try {
    const [st, gw, dm, sv, ss] = await Promise.all([
      getServiceRegSettings(),
      getGateway(),
      listDomains(),
      listDomains().then(() => listServices()),
      listServers(),
    ])
    settings.value = st
    gateway.value = gw
    domains.value = dm.items
    services.value = sv.items
    servers.value = ss
  } catch (err) {
    loadError.value = errMsg(err, t('serviceReg.errLoad'))
  } finally {
    loading.value = false
  }
}
onMounted(load)

// ─── 网关设置 ─────────────────────────────────────────────────────────────────
const busy = ref(false)
const editing = ref(false)
const form = ref({ serverId: '', httpPort: 80, httpsPort: 443, image: '', network: '', containerName: '', volumeName: '' })

function startEdit(): void {
  if (!settings.value) return
  form.value = {
    serverId: settings.value.serverId,
    httpPort: settings.value.httpPort,
    httpsPort: settings.value.httpsPort,
    image: settings.value.image,
    network: settings.value.network,
    containerName: settings.value.containerName,
    volumeName: settings.value.volumeName,
  }
  editing.value = true
}

async function saveSettings(): Promise<void> {
  busy.value = true
  try {
    settings.value = await updateServiceRegSettings({
      serverId: form.value.serverId,
      httpPort: Number(form.value.httpPort),
      httpsPort: Number(form.value.httpsPort),
      image: form.value.image,
      network: form.value.network,
      containerName: form.value.containerName,
      volumeName: form.value.volumeName,
    })
    editing.value = false
    gateway.value = await getGateway()
    toast.success(t('serviceReg.settingsSaved'))
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.settingsSaveFail')))
  } finally {
    busy.value = false
  }
}

async function doDeployGateway(): Promise<void> {
  busy.value = true
  try {
    gateway.value = await deployGateway()
    toast.success(t('serviceReg.gatewayDeployed'))
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.gatewayDeployFail')))
  } finally {
    busy.value = false
  }
}

async function doRemoveGateway(): Promise<void> {
  const ok = await confirm.open({
    title: t('serviceReg.removeGatewayTitle'),
    body: t('serviceReg.removeGatewayBody'),
    confirmLabel: t('serviceReg.removeGateway'),
    variant: 'danger',
  })
  if (!ok) return
  busy.value = true
  try {
    await removeGateway()
    gateway.value = await getGateway()
    toast.success(t('serviceReg.gatewayRemoved'))
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.errOp')))
  } finally {
    busy.value = false
  }
}

async function doApply(): Promise<void> {
  busy.value = true
  try {
    await applyServiceReg()
    settings.value = await getServiceRegSettings()
    toast.success(t('serviceReg.applied'))
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.applyFail')))
    settings.value = await getServiceRegSettings()
  } finally {
    busy.value = false
  }
}

// token 管理:明文只显示一次。
const newToken = ref('')
const tokenBusy = ref(false)
async function doGenerateToken(): Promise<void> {
  tokenBusy.value = true
  try {
    const res = await generateUploadToken()
    newToken.value = res.token
    settings.value = await getServiceRegSettings()
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.errOp')))
  } finally {
    tokenBusy.value = false
  }
}
async function doRevokeToken(): Promise<void> {
  const ok = await confirm.open({
    title: t('serviceReg.revokeTokenTitle'),
    body: t('serviceReg.revokeTokenBody'),
    confirmLabel: t('serviceReg.revokeToken'),
    variant: 'danger',
  })
  if (!ok) return
  tokenBusy.value = true
  try {
    await revokeUploadToken()
    newToken.value = ''
    settings.value = await getServiceRegSettings()
    toast.success(t('serviceReg.tokenRevoked'))
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.errOp')))
  } finally {
    tokenBusy.value = false
  }
}
function copyCurl(): void {
  const base = window.location.origin
  const cmd = `curl -X POST ${base}/api/servicereg/cert -H "Authorization: Bearer ${newToken.value}" -H "Content-Type: application/json" -d '{"baseDomain":"efg.com","certPem":"<fullchain pem>","keyPem":"<privkey pem>"}'`
  void navigator.clipboard?.writeText(cmd).then(() => toast.success(t('serviceReg.curlCopied')))
}

// ─── 基域 ────────────────────────────────────────────────────────────────────
const newDomain = ref('')
const addingDomain = ref(false)
async function addDomain(): Promise<void> {
  const v = newDomain.value.trim()
  if (!v) return
  addingDomain.value = true
  try {
    await createDomain(v)
    newDomain.value = ''
    const dm = await listDomains()
    domains.value = dm.items
    toast.success(t('serviceReg.domainAdded'))
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.domainAddFail')))
  } finally {
    addingDomain.value = false
  }
}
async function removeDomain(d: Domain): Promise<void> {
  const ok = await confirm.open({
    title: t('serviceReg.delDomainTitle'),
    body: t('serviceReg.delDomainBody', { domain: d.baseDomain }),
    confirmLabel: t('serviceReg.btn.delete'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteDomain(d.id)
    const [dm, sv] = await Promise.all([listDomains(), listServices()])
    domains.value = dm.items
    services.value = sv.items
    toast.success(t('serviceReg.domainDeleted'))
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.errOp')))
  }
}

// 证书上传:两个文件输入(cert/key PEM)。
const certFiles = ref<{ domain: Domain; cert: string; key: string } | null>(null)
function pickCert(d: Domain): void {
  certFiles.value = { domain: d, cert: '', key: '' }
}
function onCertPicked(e: Event, which: 'cert' | 'key'): void {
  const f = (e.target as HTMLInputElement).files?.[0]
  if (!f || !certFiles.value) return
  const reader = new FileReader()
  reader.onload = () => {
    if (certFiles.value) certFiles.value[which] = String(reader.result ?? '')
  }
  reader.readAsText(f)
}
const certUploading = ref(false)
async function submitCert(): Promise<void> {
  if (!certFiles.value || !certFiles.value.cert || !certFiles.value.key) return
  certUploading.value = true
  try {
    await uploadCert(certFiles.value.domain.baseDomain, certFiles.value.cert, certFiles.value.key)
    certFiles.value = null
    const dm = await listDomains()
    domains.value = dm.items
    toast.success(t('serviceReg.certUploaded'))
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.certUploadFail')))
  } finally {
    certUploading.value = false
  }
}
function certDaysLeft(d: Domain): number | null {
  if (!d.certExpiresAt) return null
  const ms = new Date(d.certExpiresAt).getTime() - Date.now()
  return Number.isNaN(ms) ? null : Math.floor(ms / 86_400_000)
}

// ─── 服务 ────────────────────────────────────────────────────────────────────
const showSvcForm = ref(false)
const svcForm = ref({
  domainId: '',
  name: '',
  protocol: 'http' as ServiceProtocol,
  upstreamKind: 'container' as UpstreamKind,
  upstream: '',
  upstreamPort: 8080,
  tcpListenPort: 13306,
})
const addingSvc = ref(false)

const domainOptions = computed(() =>
  domains.value.map((d) => ({ value: d.id, label: d.baseDomain })),
)
const svcPreview = computed(() => {
  const d = domains.value.find((x) => x.id === svcForm.value.domainId)
  if (!d || !svcForm.value.name) return ''
  return svcForm.value.protocol === 'http'
    ? `${svcForm.value.name}.${d.baseDomain}`
    : `:${svcForm.value.tcpListenPort} → ${svcForm.value.upstream}:${svcForm.value.upstreamPort}`
})

async function addService(): Promise<void> {
  addingSvc.value = true
  try {
    await createService({
      domainId: svcForm.value.domainId,
      name: svcForm.value.name.trim(),
      protocol: svcForm.value.protocol,
      upstreamKind: svcForm.value.upstreamKind,
      upstream: svcForm.value.upstream.trim(),
      upstreamPort: Number(svcForm.value.upstreamPort),
      tcpListenPort: svcForm.value.protocol === 'tcp' ? Number(svcForm.value.tcpListenPort) : undefined,
    })
    showSvcForm.value = false
    const sv = await listServices()
    services.value = sv.items
    settings.value = await getServiceRegSettings()
    toast.success(t('serviceReg.serviceAdded'))
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.serviceAddFail')))
  } finally {
    addingSvc.value = false
  }
}
async function toggleService(s: RegisteredService): Promise<void> {
  try {
    await setServiceEnabled(s.id, !s.enabled)
    const sv = await listServices()
    services.value = sv.items
    settings.value = await getServiceRegSettings()
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.errOp')))
  }
}
async function removeService(s: RegisteredService): Promise<void> {
  const ok = await confirm.open({
    title: t('serviceReg.delServiceTitle'),
    body: s.protocol === 'http' ? s.fqdn : `:${s.tcpListenPort}`,
    confirmLabel: t('serviceReg.btn.delete'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteService(s.id)
    const sv = await listServices()
    services.value = sv.items
    settings.value = await getServiceRegSettings()
    toast.success(t('serviceReg.serviceDeleted'))
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.errOp')))
  }
}

// ─── 实例(多实例 upstream;instance_rolling 轮转的单元) ─────────────────────
// 仅 http+container 服务有实例;展开服务行按需加载实例清单。
const expandedSvc = ref('')
const instancesBySvc = ref<Record<string, ServiceInstance[]>>({})
const instForm = ref<{ serviceId: string; container: string; port: number | '' } | null>(null)
const instBusy = ref(false)

function isContainerSvc(s: RegisteredService): boolean {
  return s.protocol === 'http' && s.upstreamKind === 'container'
}

async function toggleExpand(s: RegisteredService): Promise<void> {
  if (!isContainerSvc(s)) return
  if (expandedSvc.value === s.id) {
    expandedSvc.value = ''
    return
  }
  expandedSvc.value = s.id
  await reloadInstances(s.id)
}

async function reloadInstances(serviceId: string): Promise<void> {
  try {
    const res = await listInstances(serviceId)
    instancesBySvc.value = { ...instancesBySvc.value, [serviceId]: res.items }
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.errLoad')))
  }
}

async function submitInstance(): Promise<void> {
  if (!instForm.value) return
  instBusy.value = true
  try {
    await addInstance(instForm.value.serviceId, instForm.value.container.trim(), Number(instForm.value.port) || 0)
    instForm.value = null
    await reloadInstances(expandedSvc.value)
    settings.value = await getServiceRegSettings()
    toast.success(t('serviceReg.instances.added'))
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.instances.addFail')))
  } finally {
    instBusy.value = false
  }
}

async function toggleInstance(inst: ServiceInstance): Promise<void> {
  try {
    await setInstanceAttached(inst.id, !inst.attached)
    await reloadInstances(inst.serviceId)
    settings.value = await getServiceRegSettings()
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.errOp')))
  }
}

async function removeInst(inst: ServiceInstance): Promise<void> {
  const ok = await confirm.open({
    title: t('serviceReg.instances.delTitle'),
    body: inst.container,
    confirmLabel: t('serviceReg.btn.delete'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await removeInstance(inst.id)
    await reloadInstances(inst.serviceId)
    settings.value = await getServiceRegSettings()
  } catch (err) {
    toast.error(errMsg(err, t('serviceReg.errOp')))
  }
}

function effectivePort(inst: ServiceInstance, s: RegisteredService): number {
  return inst.port > 0 ? inst.port : s.upstreamPort
}
</script>

<template>
  <div class="sreg">
    <header class="sreg__header">
      <div>
        <span class="sreg__eyebrow">
          <NIcon :size="13"><World /></NIcon>
          {{ t('serviceReg.eyebrow') }}
        </span>
        <h1 class="sreg__title">{{ t('serviceReg.title') }}</h1>
        <p class="sreg__sub">{{ t('serviceReg.subtitle') }}</p>
      </div>
      <div class="sreg__actions">
        <AppButton variant="default" :loading="busy" @click="doApply">
          <NIcon :size="14"><Refresh /></NIcon>&nbsp;{{ t('serviceReg.apply') }}
        </AppButton>
        <AppButton variant="default" :disabled="loading" @click="load">
          {{ t('common.refresh') }}
        </AppButton>
      </div>
    </header>

    <div v-if="loading" class="sreg__loading" aria-busy="true">{{ t('serviceReg.loading') }}</div>
    <ErrorState
      v-else-if="loadError"
      :title="t('serviceReg.errTitle')"
      :description="loadError"
      @retry="load"
    />

    <template v-else-if="settings">
      <!-- 网关设置 -->
      <section class="sreg__card">
        <div class="sreg__card-head">
          <h2>{{ t('serviceReg.gateway.title') }}</h2>
          <span
            v-if="gateway"
            class="sreg__pill"
            :class="{
              'sreg__pill--ok': gateway.running,
              'sreg__pill--warn': gateway.configured && !gateway.running,
            }"
          >
            {{ gateway.running ? t('serviceReg.gateway.running') : gateway.configured ? t('serviceReg.gateway.stopped') : t('serviceReg.gateway.unconfigured') }}
          </span>
        </div>

        <div v-if="!editing" class="sreg__kv">
          <div class="sreg__kv-row">
            <span class="sreg__kv-k"><NIcon :size="13"><ServerIcon /></NIcon>&nbsp;{{ t('serviceReg.gateway.host') }}</span>
            <span class="sreg__kv-v">
              {{ gateway?.serverName || '—' }}
              <template v-if="gateway?.ports"> · {{ gateway.ports }}</template>
            </span>
          </div>
          <div class="sreg__kv-row">
            <span class="sreg__kv-k">{{ t('serviceReg.gateway.image') }}</span>
            <span class="sreg__kv-v">{{ settings.image }}</span>
          </div>
          <div class="sreg__kv-row">
            <span class="sreg__kv-k">{{ t('serviceReg.gateway.lastApply') }}</span>
            <span class="sreg__kv-v" :class="{ 'sreg__err': !!settings.lastApplyError }">
              <template v-if="settings.lastApplyError">{{ settings.lastApplyError }}</template>
              <template v-else-if="settings.lastApplyAt">{{ new Date(settings.lastApplyAt).toLocaleString() }}</template>
              <template v-else>—</template>
            </span>
          </div>
        </div>

        <form v-else class="sreg__form" @submit.prevent="saveSettings">
          <label class="field">
            <span class="field__lbl">{{ t('serviceReg.gateway.host') }}</span>
            <AppSelect
              :model-value="form.serverId"
              :options="[{ value: '', label: t('serviceReg.gateway.noHost') }, ...servers.map((sv) => ({ value: sv.id, label: sv.name }))]"
              @update:model-value="(v: string) => (form.serverId = v)"
            />
          </label>
          <div class="sreg__grid2">
            <label class="field">
              <span class="field__lbl">HTTP {{ t('serviceReg.gateway.port') }}</span>
              <input v-model.number="form.httpPort" class="field__in" type="number" min="1" max="65535" />
            </label>
            <label class="field">
              <span class="field__lbl">HTTPS {{ t('serviceReg.gateway.port') }}</span>
              <input v-model.number="form.httpsPort" class="field__in" type="number" min="1" max="65535" />
            </label>
          </div>
          <label class="field">
            <span class="field__lbl">{{ t('serviceReg.gateway.image') }}</span>
            <input v-model="form.image" class="field__in" type="text" />
          </label>
          <div class="sreg__grid3">
            <label class="field">
              <span class="field__lbl">{{ t('serviceReg.gateway.network') }}</span>
              <input v-model="form.network" class="field__in" type="text" />
            </label>
            <label class="field">
              <span class="field__lbl">{{ t('serviceReg.gateway.container') }}</span>
              <input v-model="form.containerName" class="field__in" type="text" />
            </label>
            <label class="field">
              <span class="field__lbl">{{ t('serviceReg.gateway.volume') }}</span>
              <input v-model="form.volumeName" class="field__in" type="text" />
            </label>
          </div>
          <div class="sreg__form-actions">
            <AppButton variant="primary" type="submit" :loading="busy">{{ t('serviceReg.btn.save') }}</AppButton>
            <AppButton variant="default" @click="editing = false">{{ t('serviceReg.btn.cancel') }}</AppButton>
          </div>
        </form>

        <div class="sreg__card-actions">
          <template v-if="!editing">
            <AppButton variant="default" @click="startEdit">{{ t('serviceReg.btn.edit') }}</AppButton>
            <AppButton variant="primary" :loading="busy" :disabled="!settings.serverId" @click="doDeployGateway">
              {{ t('serviceReg.gateway.deploy') }}
            </AppButton>
            <AppButton v-if="gateway?.installed" variant="danger" :loading="busy" @click="doRemoveGateway">
              {{ t('serviceReg.removeGateway') }}
            </AppButton>
          </template>
        </div>

        <!-- 证书上传 token -->
        <div class="sreg__token">
          <div class="sreg__token-head">
            <NIcon :size="14"><Key /></NIcon>
            <span>{{ t('serviceReg.token.title') }}</span>
            <span class="sreg__kv-v">{{ settings.hasUploadToken ? t('serviceReg.token.active') : t('serviceReg.token.none') }}</span>
          </div>
          <p class="sreg__hint">{{ t('serviceReg.token.hint') }}</p>
          <div v-if="newToken" class="sreg__token-new">
            <code>{{ newToken }}</code>
            <AppButton variant="default" @click="copyCurl">{{ t('serviceReg.token.copyCurl') }}</AppButton>
          </div>
          <div class="sreg__token-actions">
            <AppButton variant="default" :loading="tokenBusy" @click="doGenerateToken">{{ t('serviceReg.token.generate') }}</AppButton>
            <AppButton v-if="settings.hasUploadToken" variant="danger" :loading="tokenBusy" @click="doRevokeToken">
              {{ t('serviceReg.revokeToken') }}
            </AppButton>
          </div>
        </div>
      </section>

      <!-- 基域 -->
      <section class="sreg__card">
        <div class="sreg__card-head">
          <h2>{{ t('serviceReg.domains.title') }}</h2>
          <form class="sreg__inline-form" @submit.prevent="addDomain">
            <input
              v-model="newDomain"
              class="field__in"
              type="text"
              :placeholder="t('serviceReg.domains.placeholder')"
              aria-label="base domain"
            />
            <AppButton variant="primary" type="submit" :loading="addingDomain" :disabled="!newDomain.trim()">
              <NIcon :size="14"><Plus /></NIcon>&nbsp;{{ t('serviceReg.domains.add') }}
            </AppButton>
          </form>
        </div>
        <p class="sreg__hint">{{ t('serviceReg.domains.hint') }}</p>

        <EmptyState
          v-if="domains.length === 0"
          :title="t('serviceReg.domains.empty')"
          :description="t('serviceReg.domains.emptyDesc')"
        />
        <ul v-else class="sreg__list">
          <li v-for="d in domains" :key="d.id" class="sreg__row">
            <div class="sreg__row-main">
              <span class="sreg__row-title">*.{{ d.baseDomain }}</span>
              <span v-if="d.hasCert" class="sreg__pill sreg__pill--ok">
                <NIcon :size="12"><ShieldLock /></NIcon>&nbsp;HTTPS
              </span>
              <span v-else class="sreg__pill sreg__pill--warn">{{ t('serviceReg.domains.httpOnly') }}</span>
            </div>
            <div class="sreg__row-side">
              <template v-if="d.hasCert">
                <span class="sreg__kv-v">
                  {{ t('serviceReg.domains.expires') }}:
                  {{ certDaysLeft(d) !== null ? t('serviceReg.domains.daysLeft', { n: certDaysLeft(d) ?? 0 }) : '—' }}
                </span>
                <AppButton variant="default" @click="pickCert(d)">{{ t('serviceReg.domains.replaceCert') }}</AppButton>
              </template>
              <AppButton v-else variant="default" @click="pickCert(d)">{{ t('serviceReg.domains.uploadCert') }}</AppButton>
              <AppButton variant="danger" @click="removeDomain(d)"><NIcon :size="13"><Trash /></NIcon></AppButton>
            </div>
          </li>
        </ul>

        <!-- 证书上传对话框(简化为内联卡) -->
        <div v-if="certFiles" class="sreg__cert-form">
          <h3>*.{{ certFiles.domain.baseDomain }} — {{ t('serviceReg.cert.title') }}</h3>
          <label class="field">
            <span class="field__lbl">{{ t('serviceReg.cert.certFile') }}</span>
            <input class="field__in" type="file" accept=".pem,.crt,.txt" @change="onCertPicked($event, 'cert')" />
          </label>
          <label class="field">
            <span class="field__lbl">{{ t('serviceReg.cert.keyFile') }}</span>
            <input class="field__in" type="file" accept=".pem,.key,.txt" @change="onCertPicked($event, 'key')" />
          </label>
          <div class="sreg__form-actions">
            <AppButton
              variant="primary"
              :loading="certUploading"
              :disabled="!certFiles.cert || !certFiles.key"
              @click="submitCert"
            >
              {{ t('serviceReg.cert.upload') }}
            </AppButton>
            <AppButton variant="default" @click="certFiles = null">{{ t('serviceReg.btn.cancel') }}</AppButton>
          </div>
        </div>
      </section>

      <!-- 服务 -->
      <section class="sreg__card">
        <div class="sreg__card-head">
          <h2>{{ t('serviceReg.services.title') }}</h2>
          <AppButton variant="primary" :disabled="domains.length === 0" @click="showSvcForm = !showSvcForm">
            <NIcon :size="14"><Plus /></NIcon>&nbsp;{{ t('serviceReg.services.add') }}
          </AppButton>
        </div>
        <p class="sreg__hint">{{ t('serviceReg.services.hint') }}</p>

        <form v-if="showSvcForm" class="sreg__form" @submit.prevent="addService">
          <div class="sreg__grid2">
            <label class="field">
              <span class="field__lbl">{{ t('serviceReg.services.domain') }}</span>
              <AppSelect
                :model-value="svcForm.domainId"
                :options="domainOptions"
                @update:model-value="(v: string) => (svcForm.domainId = v)"
              />
            </label>
            <label class="field">
              <span class="field__lbl">{{ t('serviceReg.services.name') }}</span>
              <input v-model="svcForm.name" class="field__in" type="text" placeholder="abc" required />
            </label>
          </div>
          <div class="sreg__grid3">
            <label class="field">
              <span class="field__lbl">{{ t('serviceReg.services.protocol') }}</span>
              <AppSelect
                :model-value="svcForm.protocol"
                :options="[
                  { value: 'http', label: 'HTTP' },
                  { value: 'tcp', label: 'TCP' },
                ]"
                @update:model-value="(v) => (svcForm.protocol = v as ServiceProtocol)"
              />
            </label>
            <label class="field">
              <span class="field__lbl">{{ t('serviceReg.services.upstreamKind') }}</span>
              <AppSelect
                :model-value="svcForm.upstreamKind"
                :options="[
                  { value: 'container', label: t('serviceReg.services.kindContainer') },
                  { value: 'address', label: t('serviceReg.services.kindAddress') },
                ]"
                @update:model-value="(v) => (svcForm.upstreamKind = v as UpstreamKind)"
              />
            </label>
            <label class="field">
              <span class="field__lbl">
                {{ svcForm.upstreamKind === 'container' ? t('serviceReg.services.container') : t('serviceReg.services.address') }}
              </span>
              <input v-model="svcForm.upstream" class="field__in" type="text" required />
            </label>
          </div>
          <div class="sreg__grid2">
            <label class="field">
              <span class="field__lbl">{{ t('serviceReg.services.upstreamPort') }}</span>
              <input v-model.number="svcForm.upstreamPort" class="field__in" type="number" min="1" max="65535" required />
            </label>
            <label v-if="svcForm.protocol === 'tcp'" class="field">
              <span class="field__lbl">{{ t('serviceReg.services.listenPort') }}</span>
              <input v-model.number="svcForm.tcpListenPort" class="field__in" type="number" min="1" max="65535" required />
            </label>
          </div>
          <p v-if="svcPreview" class="sreg__preview">
            <NIcon :size="13"><ExternalLink /></NIcon>&nbsp;
            <code>{{ svcPreview }}</code>
          </p>
          <div class="sreg__form-actions">
            <AppButton variant="primary" type="submit" :loading="addingSvc">{{ t('serviceReg.btn.create') }}</AppButton>
            <AppButton variant="default" @click="showSvcForm = false">{{ t('serviceReg.btn.cancel') }}</AppButton>
          </div>
        </form>

        <EmptyState
          v-if="services.length === 0"
          :title="t('serviceReg.services.empty')"
          :description="t('serviceReg.services.emptyDesc')"
        />
        <ul v-else class="sreg__list">
          <li
            v-for="s in services"
            :key="s.id"
            class="sreg__svc"
            :class="{ 'sreg__row--off': !s.enabled }"
          >
            <div class="sreg__row">
              <div class="sreg__row-main">
                <button
                  v-if="isContainerSvc(s)"
                  class="sreg__expander"
                  :aria-expanded="expandedSvc === s.id"
                  :aria-label="t('serviceReg.instances.toggle')"
                  @click="toggleExpand(s)"
                >
                  {{ expandedSvc === s.id ? '▾' : '▸' }}
                </button>
                <span class="sreg__row-title">
                  <code v-if="s.protocol === 'http'">{{ s.fqdn }}</code>
                  <code v-else>:{{ s.tcpListenPort }}</code>
                </span>
                <span class="sreg__kv-v">
                  → {{ s.upstream }}:{{ s.upstreamPort }}
                  <span class="sreg__tag">{{ s.protocol.toUpperCase() }}</span>
                  <span v-if="isContainerSvc(s)" class="sreg__tag">
                    {{ t('serviceReg.instances.count', { n: (instancesBySvc[s.id] ?? []).length }) }}
                  </span>
                  <span v-if="!s.enabled" class="sreg__tag sreg__tag--off">{{ t('serviceReg.services.disabled') }}</span>
                </span>
              </div>
              <div class="sreg__row-side">
                <a
                  v-if="s.protocol === 'http'"
                  class="sreg__link"
                  :href="`https://${s.fqdn}`"
                  target="_blank"
                  rel="noopener"
                >
                  <NIcon :size="13"><ExternalLink /></NIcon>
                </a>
                <AppButton variant="default" @click="toggleService(s)">
                  {{ s.enabled ? t('serviceReg.services.disable') : t('serviceReg.services.enable') }}
                </AppButton>
                <AppButton variant="danger" @click="removeService(s)"><NIcon :size="13"><Trash /></NIcon></AppButton>
              </div>
            </div>

            <!-- 实例面板(http+container 服务):多实例 upstream 成员管理 -->
            <div v-if="expandedSvc === s.id && isContainerSvc(s)" class="sreg__inst">
              <p class="sreg__hint">{{ t('serviceReg.instances.hint') }}</p>
              <ul class="sreg__inst-list">
                <li v-for="inst in instancesBySvc[s.id] ?? []" :key="inst.id" class="sreg__inst-row">
                  <code class="sreg__inst-name">{{ inst.container }}</code>
                  <span class="sreg__kv-v">:{{ effectivePort(inst, s) }}</span>
                  <span v-if="!inst.attached" class="sreg__tag sreg__tag--off">{{ t('serviceReg.instances.detached') }}</span>
                  <span class="sreg__inst-spacer" />
                  <AppButton variant="default" @click="toggleInstance(inst)">
                    {{ inst.attached ? t('serviceReg.instances.detach') : t('serviceReg.instances.attach') }}
                  </AppButton>
                  <AppButton variant="danger" @click="removeInst(inst)"><NIcon :size="13"><Trash /></NIcon></AppButton>
                </li>
              </ul>
              <form
                v-if="instForm && instForm.serviceId === s.id"
                class="sreg__inst-form"
                @submit.prevent="submitInstance"
              >
                <input
                  v-model="instForm.container"
                  class="field__in"
                  type="text"
                  :placeholder="t('serviceReg.services.name') + '-2'"
                  required
                />
                <input
                  v-model.number="instForm.port"
                  class="field__in sreg__inst-port"
                  type="number"
                  min="0"
                  max="65535"
                  :placeholder="t('serviceReg.instances.portPlaceholder')"
                />
                <AppButton variant="primary" type="submit" :loading="instBusy" :disabled="!instForm.container.trim()">
                  {{ t('serviceReg.instances.add') }}
                </AppButton>
                <AppButton variant="default" @click="instForm = null">{{ t('serviceReg.btn.cancel') }}</AppButton>
              </form>
              <AppButton
                v-else
                variant="default"
                @click="instForm = { serviceId: s.id, container: '', port: '' }"
              >
                {{ t('serviceReg.instances.add') }}
              </AppButton>
            </div>
          </li>
        </ul>
      </section>
    </template>
  </div>
</template>

<style scoped>
.sreg {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.sreg__svc {
  list-style: none;
}
.sreg__svc .sreg__row {
  border-bottom: none;
}
.sreg__svc > .sreg__row {
  border-bottom: 1px solid var(--border, #ececec);
}
.sreg__expander {
  border: none;
  background: none;
  cursor: pointer;
  color: inherit;
  font-size: 12px;
  padding: 2px 6px;
  opacity: 0.7;
}
.sreg__inst {
  padding: 6px 0 12px 26px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  border-bottom: 1px solid var(--border, #ececec);
}
.sreg__inst-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.sreg__inst-row {
  display: flex;
  align-items: center;
  gap: 8px;
}
.sreg__inst-name {
  font-size: 12px;
}
.sreg__inst-spacer {
  flex: 1;
}
.sreg__inst-form {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-wrap: wrap;
}
.sreg__inst-port {
  width: 120px;
}
.sreg__header {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 12px;
}
.sreg__eyebrow {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  text-transform: uppercase;
  letter-spacing: 0.08em;
  opacity: 0.7;
}
.sreg__title {
  margin: 4px 0 2px;
  font-size: 22px;
}
.sreg__sub {
  margin: 0;
  font-size: 13px;
  opacity: 0.75;
}
.sreg__actions {
  display: flex;
  gap: 8px;
}
.sreg__loading {
  opacity: 0.6;
  font-size: 13px;
}
.sreg__card {
  border: 1px solid var(--border, #e2e2e2);
  border-radius: 10px;
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.sreg__card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.sreg__card-head h2 {
  margin: 0;
  font-size: 16px;
}
.sreg__card-actions,
.sreg__form-actions,
.sreg__token-actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.sreg__kv {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.sreg__kv-row {
  display: flex;
  gap: 10px;
  font-size: 13px;
  align-items: center;
}
.sreg__kv-k {
  min-width: 130px;
  opacity: 0.65;
  display: inline-flex;
  align-items: center;
}
.sreg__kv-v {
  font-size: 13px;
}
.sreg__form {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.sreg__grid2,
.sreg__grid3 {
  display: grid;
  gap: 10px;
}
.sreg__grid2 {
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
}
.sreg__grid3 {
  grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
}
.field {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.field__lbl {
  font-size: 12px;
  opacity: 0.7;
}
.field__in {
  padding: 7px 10px;
  border: 1px solid var(--border, #d4d4d4);
  border-radius: 7px;
  font-size: 13px;
  background: var(--bg, inherit);
  color: inherit;
}
.sreg__inline-form {
  display: flex;
  gap: 8px;
  align-items: center;
}
.sreg__pill {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 11px;
  padding: 2px 8px;
  border-radius: 999px;
  border: 1px solid var(--border, #d4d4d4);
}
.sreg__pill--ok {
  color: #0a7d32;
  border-color: #0a7d3255;
}
.sreg__pill--warn {
  color: #9a6700;
  border-color: #9a670055;
}
.sreg__err {
  color: #b3261e;
}
.sreg__hint {
  margin: 0;
  font-size: 12px;
  opacity: 0.65;
}
.sreg__token {
  border-top: 1px dashed var(--border, #e2e2e2);
  padding-top: 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.sreg__token-head {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
}
.sreg__token-new {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.sreg__token-new code {
  font-size: 12px;
  padding: 4px 8px;
  border-radius: 6px;
  background: var(--code-bg, #f4f4f4);
  word-break: break-all;
}
.sreg__list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
}
.sreg__row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 10px 0;
  border-bottom: 1px solid var(--border, #ececec);
  flex-wrap: wrap;
}
.sreg__row:last-child {
  border-bottom: none;
}
.sreg__row--off {
  opacity: 0.55;
}
.sreg__row-main {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.sreg__row-side {
  display: flex;
  align-items: center;
  gap: 8px;
}
.sreg__row-title code {
  font-size: 13px;
  font-weight: 600;
}
.sreg__tag {
  font-size: 10px;
  padding: 1px 6px;
  border-radius: 4px;
  border: 1px solid var(--border, #d4d4d4);
  margin-left: 6px;
}
.sreg__tag--off {
  color: #b3261e;
  border-color: #b3261e55;
}
.sreg__link {
  display: inline-flex;
  color: inherit;
}
.sreg__cert-form {
  border: 1px dashed var(--border, #d4d4d4);
  border-radius: 8px;
  padding: 12px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.sreg__cert-form h3 {
  margin: 0;
  font-size: 13px;
}
.sreg__preview {
  margin: 0;
  font-size: 12px;
  display: flex;
  align-items: center;
  gap: 4px;
  opacity: 0.8;
}
</style>
