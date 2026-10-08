<script setup lang="ts">
/*
  ServiceRegistry.vue — 服务注册网关(nginx)。

  三个区块:
  1. 网关设置:选网关主机/端口/镜像/资源名,部署/移除容器,最近 apply 状态。
  2. 基域:注册 efg.com,上传泛域名证书(到期时间展示),删除。
  3. 服务:注册 abc → abc.efg.com(HTTP/TCP 反代),启停/删除,手动全量收敛。

  约定:*.efg.com 由用户自行解析到网关主机 IP;未配网关时全部操作为配置态(不触网)。
*/
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { NIcon } from 'naive-ui'
import { World, Plus, Trash, Refresh, ShieldLock, Server as ServerIcon, ExternalLink } from '@vicons/tabler'
import {
  getServiceRegSettings,
  updateServiceRegSettings,
  getGateway,
  deployGateway,
  removeGateway,
  listDomains,
  createDomain,
  deleteDomain,
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
const router = useRouter()
const toast = useToast()

// 基域证书统一在「证书管理」页签发/导入(本页只展示同步结果)。
function goCerts(): void {
  void router.push('/certificates')
}
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

// 全部网关主机容器都在运行(含至少一台)才亮绿;逐台明细在主机行展示。
const gwAllRunning = computed(() => {
  const list = gateway.value?.servers ?? []
  return list.length > 0 && list.every((s) => s.running)
})

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
// 多机网关:每台部署完全一致的网关;serverIds 为选中的全部网关主机。
const form = ref({ serverIds: [] as string[], httpPort: 80, httpsPort: 443, image: '', network: '', containerName: '', volumeName: '' })

function toggleHost(id: string): void {
  form.value.serverIds = form.value.serverIds.includes(id)
    ? form.value.serverIds.filter((v) => v !== id)
    : [...form.value.serverIds, id]
}

// serverNameById 把错误文案里的主机引用 id 替换为可读名(逐台错误展示用)。
const serverNameById = computed<Record<string, string>>(() => {
  const m: Record<string, string> = {}
  for (const sv of servers.value) m[sv.id] = sv.name
  for (const s of gateway.value?.servers ?? []) if (s.serverName) m[s.serverId] = s.serverName
  return m
})
function fmtApplyErr(e: string): string {
  let out = e
  for (const [id, name] of Object.entries(serverNameById.value)) {
    if (out.includes(id)) out = out.split(id).join(name)
  }
  return out
}

function startEdit(): void {
  if (!settings.value) return
  form.value = {
    serverIds: [...(settings.value.serverIds ?? [])],
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
      serverIds: form.value.serverIds,
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

// ─── 实例(集群 upstream 成员 = 服务器 + 宿主端口) ─────────────────────────────
// http 服务均可有实例(容器实例由部署自动注册;非容器实例部署注册或人工添加)。展开服务行按需加载。
const expandedSvc = ref('')
const instancesBySvc = ref<Record<string, ServiceInstance[]>>({})
const instForm = ref<{ serviceId: string; serverId: string; container: string; port: number | ''; hostPort: number | '' } | null>(null)
const instBusy = ref(false)

/** http 服务展示实例面板(集群模型:容器/非容器实例一视同仁)。 */
function hasInstancePool(s: RegisteredService): boolean {
  return s.protocol === 'http'
}

async function toggleExpand(s: RegisteredService): Promise<void> {
  if (!hasInstancePool(s)) return
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
    await addInstance(
      instForm.value.serviceId,
      instForm.value.serverId,
      instForm.value.container.trim(),
      Number(instForm.value.port) || 0,
      Number(instForm.value.hostPort) || 0,
    )
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

/** 网关反代端口:实例 hostPort > 实例 port > 服务默认端口。 */
function effectiveHostPort(inst: ServiceInstance, s: RegisteredService): number {
  if (inst.hostPort > 0) return inst.hostPort
  if (inst.port > 0) return inst.port
  return s.upstreamPort
}

/** 实例归属服务器可读名(遗留行 server_id='' 显示待迁移)。 */
function instServerName(inst: ServiceInstance): string {
  if (!inst.serverId) return t('serviceReg.instances.legacy')
  return serverNameById.value[inst.serverId] ?? inst.serverId
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
              'sreg__pill--ok': gwAllRunning,
              'sreg__pill--warn': gateway.configured && !gwAllRunning,
            }"
          >
            {{ gwAllRunning ? t('serviceReg.gateway.running') : gateway.configured ? t('serviceReg.gateway.stopped') : t('serviceReg.gateway.unconfigured') }}
          </span>
        </div>

        <div v-if="!editing" class="sreg__kv">
          <div class="sreg__kv-row">
            <span class="sreg__kv-k"><NIcon :size="13"><ServerIcon /></NIcon>&nbsp;{{ t('serviceReg.gateway.host') }}</span>
            <span class="sreg__kv-v">
              <template v-if="gateway?.servers?.length">
                <span v-for="s in gateway.servers" :key="s.serverId" class="sreg__host">
                  <span class="sreg__dot" :class="s.running ? 'sreg__dot--ok' : 'sreg__dot--bad'" />
                  {{ s.serverName || s.serverId }}<template v-if="s.ports">&nbsp;· {{ s.ports }}</template>
                </span>
              </template>
              <template v-else>—</template>
            </span>
          </div>
          <div class="sreg__kv-row">
            <span class="sreg__kv-k">{{ t('serviceReg.gateway.image') }}</span>
            <span class="sreg__kv-v">{{ settings.image }}</span>
          </div>
          <div class="sreg__kv-row">
            <span class="sreg__kv-k">{{ t('serviceReg.gateway.lastApply') }}</span>
            <span class="sreg__kv-v" :class="{ 'sreg__err': !!settings.lastApplyError }">
              <template v-if="settings.lastApplyError">{{ fmtApplyErr(settings.lastApplyError) }}</template>
              <template v-else-if="settings.lastApplyAt">{{ new Date(settings.lastApplyAt).toLocaleString() }}</template>
              <template v-else>—</template>
            </span>
          </div>
        </div>

        <form v-else class="sreg__form" @submit.prevent="saveSettings">
          <label class="field">
            <span class="field__lbl">{{ t('serviceReg.gateway.host') }}</span>
            <span class="sreg__hint">{{ t('serviceReg.gateway.hostHint') }}</span>
            <ul v-if="servers.length" class="sreg__hosts">
              <li v-for="sv in servers" :key="sv.id">
                <label class="sreg__host-item">
                  <input
                    type="checkbox"
                    :checked="form.serverIds.includes(sv.id)"
                    @change="toggleHost(sv.id)"
                  />
                  <span class="sreg__host-name">{{ sv.name }}</span>
                  <span class="sreg__host-meta">{{ sv.host }}:{{ sv.port }}</span>
                </label>
              </li>
            </ul>
            <span v-else class="sreg__host-meta">{{ t('serviceReg.gateway.noHost') }}</span>
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
            <AppButton variant="primary" :loading="busy" :disabled="!settings.serverIds?.length" @click="doDeployGateway">
              {{ t('serviceReg.gateway.deploy') }}
            </AppButton>
            <AppButton v-if="gateway?.servers?.some((s) => s.installed)" variant="danger" :loading="busy" @click="doRemoveGateway">
              {{ t('serviceReg.removeGateway') }}
            </AppButton>
          </template>
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
              </template>
              <AppButton variant="default" @click="goCerts">{{ t('serviceReg.domains.manageCert') }}</AppButton>
              <AppButton variant="danger" @click="removeDomain(d)"><NIcon :size="13"><Trash /></NIcon></AppButton>
            </div>
          </li>
        </ul>

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
                  v-if="hasInstancePool(s)"
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
                  <span v-if="hasInstancePool(s)" class="sreg__tag">
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

            <!-- 实例面板(http 服务):集群 upstream 成员 = 服务器 + 宿主端口 -->
            <div v-if="expandedSvc === s.id && hasInstancePool(s)" class="sreg__inst">
              <p class="sreg__hint">{{ t('serviceReg.instances.hint') }}</p>
              <ul class="sreg__inst-list">
                <li v-for="inst in instancesBySvc[s.id] ?? []" :key="inst.id" class="sreg__inst-row">
                  <span class="sreg__tag">{{ instServerName(inst) }}</span>
                  <code v-if="inst.container" class="sreg__inst-name">{{ inst.container }}</code>
                  <span v-else class="sreg__tag">{{ t('serviceReg.instances.hostKind') }}</span>
                  <span class="sreg__kv-v">:{{ effectiveHostPort(inst, s) }}</span>
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
                <select v-model="instForm.serverId" class="field__in" required>
                  <option value="" disabled>{{ t('serviceReg.instances.serverPlaceholder') }}</option>
                  <option v-for="sv in servers" :key="sv.id" :value="sv.id">{{ sv.name }} · {{ sv.host }}</option>
                </select>
                <input
                  v-model="instForm.container"
                  class="field__in"
                  type="text"
                  :placeholder="t('serviceReg.instances.containerPlaceholder')"
                />
                <input
                  v-model.number="instForm.port"
                  class="field__in sreg__inst-port"
                  type="number"
                  min="0"
                  max="65535"
                  :placeholder="t('serviceReg.instances.portPlaceholder')"
                />
                <input
                  v-model.number="instForm.hostPort"
                  class="field__in sreg__inst-port"
                  type="number"
                  min="0"
                  max="65535"
                  :placeholder="t('serviceReg.instances.hostPortPlaceholder')"
                />
                <AppButton variant="primary" type="submit" :loading="instBusy" :disabled="!instForm.serverId">
                  {{ t('serviceReg.instances.add') }}
                </AppButton>
                <AppButton variant="default" @click="instForm = null">{{ t('serviceReg.btn.cancel') }}</AppButton>
              </form>
              <AppButton
                v-else
                variant="default"
                @click="instForm = { serviceId: s.id, serverId: '', container: '', port: '', hostPort: '' }"
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
.sreg__form-actions {
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
/* 多机网关:逐台状态行(状态点 + 机器名 + 发布端口)。 */
.sreg__host {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  margin-right: 12px;
  white-space: nowrap;
}
.sreg__dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  flex: none;
}
.sreg__dot--ok {
  background: #22a06b;
}
.sreg__dot--bad {
  background: #b3261e;
}
/* 编辑态:网关主机复选列表(可多选)。 */
.sreg__hosts {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.sreg__host-item {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  cursor: pointer;
}
.sreg__host-name {
  font-weight: 500;
}
.sreg__host-meta {
  font-size: 12px;
  opacity: 0.6;
}
.sreg__hint {
  font-size: 12px;
  opacity: 0.6;
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
.sreg__preview {
  margin: 0;
  font-size: 12px;
  display: flex;
  align-items: center;
  gap: 4px;
  opacity: 0.8;
}
</style>
