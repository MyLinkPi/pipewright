<!--
  Ceph.vue — Ceph 集群列表 + 创建向导(管理面入口)。

  · 集群卡:名称/状态(created|deploying|ready|failed)/fsid/bootstrap 主机/健康徽标
    (ready 集群并发拉状态,失败静默降级为 —);操作:进入管理 / 删除(仅解除纳管)。
  · 创建向导(3 步):基本信息 → 选主机(登记服务器多选、指定 bootstrap、角色与 OSD 设备)
    → 确认(说明 dashboard 密码自动生成入保险库)。提交后跳转详情页发起部署。
  数据源:GET /api/ceph/clusters(15s 轮询)+ GET /api/ceph/clusters/{id}/status(逐卡)。
-->
<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { Refresh, Plus, Trash } from '@vicons/tabler'
import {
  listCephClusters,
  createCephCluster,
  deleteCephCluster,
  getCephClusterStatus,
  type CephCluster,
  type CephHostInput,
} from '../api/ceph'
import { listServers, type Server } from '../api/servers'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import AppButton from '../components/ui/AppButton.vue'
import AppSelect from '../components/ui/AppSelect.vue'
import EmptyState from '../components/ui/EmptyState.vue'
import ErrorState from '../components/ui/ErrorState.vue'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()
const router = useRouter()

function errMsg(err: unknown, fallback: string): string {
  return err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback
}

// ─── 集群列表 ────────────────────────────────────────────────────────────────
const clusters = ref<CephCluster[]>([])
const loadState = ref<'loading' | 'loaded' | 'error'>('loading')
const loadError = ref('')
const healths = ref<Record<string, string>>({})
let timer: number | undefined

async function load(): Promise<void> {
  try {
    clusters.value = await listCephClusters()
    loadState.value = 'loaded'
    void loadHealth()
  } catch (err) {
    loadState.value = clusters.value.length === 0 ? 'error' : 'loaded'
    loadError.value = errMsg(err, t('ceph.errLoad'))
  }
}

/** ready 集群并发拉健康状态(失败静默;卡片显示 —)。 */
async function loadHealth(): Promise<void> {
  await Promise.all(
    clusters.value
      .filter((c) => c.state === 'ready')
      .map(async (c) => {
        try {
          const st = await getCephClusterStatus(c.id)
          healths.value = { ...healths.value, [c.id]: st.health }
        } catch {
          healths.value = { ...healths.value, [c.id]: '' }
        }
      }),
  )
}

function stateClass(c: CephCluster): string {
  if (c.state === 'ready') return 'ok'
  if (c.state === 'failed') return 'err'
  if (c.state === 'deploying') return 'warn'
  return ''
}

function healthClass(c: CephCluster): string {
  const h = healths.value[c.id] ?? ''
  if (h === 'HEALTH_OK') return 'ok'
  if (h === 'HEALTH_WARN') return 'warn'
  if (h === 'HEALTH_ERR') return 'err'
  return ''
}

async function onDelete(c: CephCluster): Promise<void> {
  const ok = await confirm.open({
    title: t('ceph.list.deleteTitle', { name: c.name }),
    body: t('ceph.list.deleteBody'),
    confirmLabel: t('ceph.list.deleteConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteCephCluster(c.id)
    toast.success(t('ceph.list.deleted'))
    void load()
  } catch (err) {
    toast.error(errMsg(err, t('ceph.list.deleteFailed')))
  }
}

// ─── 创建向导 ────────────────────────────────────────────────────────────────
const servers = ref<Server[]>([])
const wizardOpen = ref(false)
const step = ref(1)
const busy = ref(false)

const form = ref({
  name: '',
  image: '',
  cephadmSourceMode: 'auto',
  cephadmCurlUrl: '',
  publicNetwork: '',
  clusterNetwork: '',
})
/** 每台选中主机的角色/设备;key = serverId。 */
const hostCfg = ref<Record<string, { roles: string[]; devices: string }>>({})
const bootstrapServerId = ref('')

const selectedIds = computed(() => Object.keys(hostCfg.value).filter((id) => hostCfg.value[id] !== undefined))

function openWizard(): void {
  step.value = 1
  form.value = { name: '', image: '', cephadmSourceMode: 'auto', cephadmCurlUrl: '', publicNetwork: '', clusterNetwork: '' }
  hostCfg.value = {}
  bootstrapServerId.value = ''
  wizardOpen.value = true
  if (servers.value.length === 0) {
    void listServers().then((list) => (servers.value = list)).catch(() => undefined)
  }
}

function toggleHost(id: string): void {
  if (hostCfg.value[id]) {
    const next = { ...hostCfg.value }
    delete next[id]
    hostCfg.value = next
    if (bootstrapServerId.value === id) bootstrapServerId.value = ''
  } else {
    hostCfg.value = { ...hostCfg.value, [id]: { roles: ['mon', 'mgr', 'osd'], devices: '' } }
    if (!bootstrapServerId.value) bootstrapServerId.value = id
  }
}

function toggleRole(id: string, role: string): void {
  const cfg = hostCfg.value[id]
  if (!cfg) return
  cfg.roles = cfg.roles.includes(role) ? cfg.roles.filter((r) => r !== role) : [...cfg.roles, role]
}

const nameRe = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/
const step1Valid = computed(() => nameRe.test(form.value.name.trim()))
const step2Valid = computed(
  () => selectedIds.value.length > 0 && !!bootstrapServerId.value && !!hostCfg.value[bootstrapServerId.value],
)

function parseDevices(s: string): string[] {
  return s
    .split(/[\n,]/)
    .map((x) => x.trim())
    .filter((x) => x.length > 0)
}

const submitBody = computed(() => {
  const hosts: CephHostInput[] = selectedIds.value.map((id) => ({
    serverId: id,
    roles: hostCfg.value[id].roles,
    labels: [],
    devices: parseDevices(hostCfg.value[id].devices),
  }))
  return {
    name: form.value.name.trim(),
    bootstrapServerId: bootstrapServerId.value,
    image: form.value.image.trim(),
    cephadmSource:
      form.value.cephadmSourceMode === 'curl' ? `curl:${form.value.cephadmCurlUrl.trim()}` : form.value.cephadmSourceMode,
    publicNetwork: form.value.publicNetwork.trim(),
    clusterNetwork: form.value.clusterNetwork.trim(),
    hosts,
  }
})

const sourceOptions = computed(() => [
  { value: 'auto', label: t('ceph.wizard.srcAuto') },
  { value: 'package', label: t('ceph.wizard.srcPackage') },
  { value: 'curl', label: t('ceph.wizard.srcCurl') },
])

async function submit(): Promise<void> {
  busy.value = true
  try {
    const cl = await createCephCluster(submitBody.value)
    toast.success(t('ceph.wizard.created'))
    wizardOpen.value = false
    void router.push(`/ceph/${cl.id}`)
  } catch (err) {
    toast.error(errMsg(err, t('ceph.wizard.createFailed')))
  } finally {
    busy.value = false
  }
}

onMounted(() => {
  void load()
  timer = window.setInterval(() => void load(), 15_000)
})
onUnmounted(() => {
  if (timer !== undefined) window.clearInterval(timer)
})
</script>

<template>
  <div class="ceph-page">
    <header class="page-head">
      <div>
        <h1>{{ t('ceph.title') }}</h1>
        <p class="sub">{{ t('ceph.subtitle') }}</p>
      </div>
      <div class="head-actions">
        <AppButton variant="default" size="sm" @click="load">
          <Refresh class="btn-ico" />
          <span>{{ t('common.refresh') }}</span>
        </AppButton>
        <AppButton variant="primary" size="sm" @click="openWizard">
          <Plus class="btn-ico" />
          <span>{{ t('ceph.list.create') }}</span>
        </AppButton>
      </div>
    </header>

    <ErrorState v-if="loadState === 'error'" :description="loadError" show-retry @retry="load" />

    <section v-else-if="clusters.length === 0 && loadState === 'loaded'" class="empty-wrap">
      <EmptyState :title="t('ceph.empty.title')" :description="t('ceph.empty.hint')" />
    </section>

    <div v-else class="card-grid">
      <article v-for="c in clusters" :key="c.id" class="cluster-card" @click="router.push(`/ceph/${c.id}`)">
        <header class="cc-head">
          <h3>{{ c.name }}</h3>
          <span class="tag" :class="stateClass(c)">{{ t(`ceph.state.${c.state}`) }}</span>
        </header>
        <dl class="cc-grid">
          <div class="cc-item">
            <dt>{{ t('ceph.list.fsid') }}</dt>
            <dd class="mono">{{ c.fsid ? c.fsid.slice(0, 13) + '…' : '—' }}</dd>
          </div>
          <div class="cc-item">
            <dt>{{ t('ceph.list.bootstrap') }}</dt>
            <dd>{{ c.bootstrapServerName || '—' }}</dd>
          </div>
          <div class="cc-item">
            <dt>{{ t('ceph.list.health') }}</dt>
            <dd>
              <span v-if="c.state === 'ready'" class="tag" :class="healthClass(c)">
                {{ healths[c.id] || '…' }}
              </span>
              <span v-else>—</span>
            </dd>
          </div>
          <div class="cc-item">
            <dt>{{ t('ceph.list.dashboard') }}</dt>
            <dd>
              <a v-if="c.dashboardUrl" :href="c.dashboardUrl" target="_blank" rel="noreferrer" class="link-btn" @click.stop>
                {{ t('ceph.list.openDashboard') }}
              </a>
              <span v-else>—</span>
            </dd>
          </div>
        </dl>
        <footer class="cc-actions">
          <AppButton variant="default" size="sm" @click.stop="router.push(`/ceph/${c.id}`)">
            {{ t('ceph.list.manage') }}
          </AppButton>
          <AppButton variant="danger" size="sm" :disabled="c.state === 'deploying'" @click.stop="onDelete(c)">
            <Trash class="btn-ico" />
            <span>{{ t('ceph.list.remove') }}</span>
          </AppButton>
        </footer>
      </article>
    </div>

    <!-- 创建向导 -->
    <div v-if="wizardOpen" class="modal" role="dialog" aria-modal="true">
      <div class="modal-card">
        <header class="modal-head">
          <h3>{{ t('ceph.wizard.title') }}</h3>
          <button class="modal-close" :aria-label="t('ceph.cancel')" @click="wizardOpen = false">×</button>
        </header>

        <ol class="steps">
          <li :class="{ active: step === 1, done: step > 1 }">{{ t('ceph.wizard.step1') }}</li>
          <li :class="{ active: step === 2, done: step > 2 }">{{ t('ceph.wizard.step2') }}</li>
          <li :class="{ active: step === 3 }">{{ t('ceph.wizard.step3') }}</li>
        </ol>

        <!-- 步骤 1:基本信息 -->
        <form v-if="step === 1" class="wiz-form" @submit.prevent="step = 2">
          <label class="field">
            <span class="field-lbl">{{ t('ceph.wizard.name') }} *</span>
            <input v-model="form.name" class="field-in" type="text" placeholder="prod" />
            <span class="hint">{{ t('ceph.wizard.nameHint') }}</span>
          </label>
          <label class="field">
            <span class="field-lbl">{{ t('ceph.wizard.image') }}</span>
            <input v-model="form.image" class="field-in" type="text" placeholder="quay.io/ceph/ceph:v19.2.1" />
            <span class="hint">{{ t('ceph.wizard.imageHint') }}</span>
          </label>
          <div class="grid2">
            <label class="field">
              <span class="field-lbl">{{ t('ceph.wizard.publicNetwork') }}</span>
              <input v-model="form.publicNetwork" class="field-in" type="text" placeholder="10.0.0.0/24" />
            </label>
            <label class="field">
              <span class="field-lbl">{{ t('ceph.wizard.clusterNetwork') }}</span>
              <input v-model="form.clusterNetwork" class="field-in" type="text" placeholder="10.1.0.0/24" />
            </label>
          </div>
          <label class="field">
            <span class="field-lbl">{{ t('ceph.wizard.source') }}</span>
            <AppSelect v-model="form.cephadmSourceMode" :options="sourceOptions" />
          </label>
          <label v-if="form.cephadmSourceMode === 'curl'" class="field">
            <span class="field-lbl">{{ t('ceph.wizard.curlUrl') }}</span>
            <input v-model="form.cephadmCurlUrl" class="field-in" type="text" placeholder="https://download.ceph.com/cephadm/get" />
          </label>
          <div class="form-actions">
            <AppButton variant="primary" type="submit" :disabled="!step1Valid">{{ t('ceph.next') }}</AppButton>
          </div>
        </form>

        <!-- 步骤 2:选主机 -->
        <div v-else-if="step === 2" class="wiz-form">
          <p v-if="servers.length === 0" class="hint">{{ t('ceph.wizard.noServers') }}</p>
          <div v-for="sv in servers" :key="sv.id" class="host-row" :class="{ picked: !!hostCfg[sv.id] }">
            <label class="host-pick">
              <input type="checkbox" :checked="!!hostCfg[sv.id]" @change="toggleHost(sv.id)" />
              <span class="host-name">{{ sv.name }}</span>
              <span class="mono host-addr">{{ sv.host }}</span>
            </label>
            <label v-if="hostCfg[sv.id]" class="host-boot">
              <input
                type="radio"
                name="bootstrap"
                :value="sv.id"
                :checked="bootstrapServerId === sv.id"
                @change="bootstrapServerId = sv.id"
              />
              {{ t('ceph.wizard.bootstrap') }}
            </label>
            <template v-if="hostCfg[sv.id]">
              <span class="role-toggles">
                <button
                  v-for="role in ['mon', 'mgr', 'osd']"
                  :key="role"
                  type="button"
                  class="tag clickable"
                  :class="{ ok: hostCfg[sv.id].roles.includes(role) }"
                  @click="toggleRole(sv.id, role)"
                >
                  {{ role }}
                </button>
              </span>
              <input
                v-model="hostCfg[sv.id].devices"
                class="field-in devices"
                type="text"
                :placeholder="t('ceph.wizard.devicesPh')"
              />
            </template>
          </div>
          <div class="form-actions">
            <AppButton variant="default" @click="step = 1">{{ t('ceph.prev') }}</AppButton>
            <AppButton variant="primary" :disabled="!step2Valid" @click="step = 3">{{ t('ceph.next') }}</AppButton>
          </div>
        </div>

        <!-- 步骤 3:确认 -->
        <div v-else class="wiz-form">
          <dl class="confirm-grid">
            <dt>{{ t('ceph.wizard.name') }}</dt>
            <dd>{{ submitBody.name }}</dd>
            <dt>{{ t('ceph.wizard.hosts') }}</dt>
            <dd>{{ submitBody.hosts.length }}</dd>
            <dt>{{ t('ceph.wizard.bootstrap') }}</dt>
            <dd>{{ submitBody.bootstrapServerId }}</dd>
            <dt>{{ t('ceph.wizard.devices') }}</dt>
            <dd>{{ submitBody.hosts.reduce((n, h) => n + h.devices.length, 0) }}</dd>
          </dl>
          <p class="hint">{{ t('ceph.wizard.confirmHint') }}</p>
          <p class="hint">{{ t('ceph.wizard.dashboardHint') }}</p>
          <div class="form-actions">
            <AppButton variant="default" @click="step = 2">{{ t('ceph.prev') }}</AppButton>
            <AppButton variant="primary" :loading="busy" @click="submit">{{ t('ceph.wizard.submit') }}</AppButton>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.ceph-page { display: flex; flex-direction: column; gap: 16px; }
.page-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; }
.page-head h1 { margin: 0; font-size: 20px; }
.sub { margin: 4px 0 0; color: var(--color-text-muted, #888); font-size: 13px; }
.head-actions { display: flex; gap: 8px; }
.btn-ico { width: 14px; height: 14px; }

.card-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 12px; }
.cluster-card { border: 1px solid var(--color-border, #333); border-radius: 10px; padding: 14px 16px; cursor: pointer; display: flex; flex-direction: column; gap: 10px; }
.cluster-card:hover { border-color: var(--color-primary, #4c6ef5); }
.cc-head { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
.cc-head h3 { margin: 0; font-size: 15px; }
.cc-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 8px 12px; margin: 0; }
.cc-item dt { font-size: 11px; color: var(--color-text-muted, #888); }
.cc-item dd { margin: 2px 0 0; font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.cc-actions { display: flex; justify-content: space-between; gap: 8px; }

.tag { display: inline-flex; align-items: center; padding: 1px 8px; border-radius: 999px; font-size: 11px; background: var(--color-bg-soft, #1c1c1c); }
.tag.ok { background: var(--color-green-soft, rgba(76, 175, 80, 0.15)); color: var(--color-green, #4caf50); }
.tag.warn { background: var(--color-amber-soft, rgba(255, 193, 7, 0.15)); color: var(--color-amber, #ffc107); }
.tag.err { background: rgba(201, 42, 42, 0.15); color: #e03131; }
.tag.clickable { cursor: pointer; }

.mono { font-family: var(--font-mono, monospace); }
.empty-wrap { border: 1px dashed var(--color-border, #333); border-radius: 10px; padding: 24px; }

/* 向导 */
.modal { position: fixed; inset: 0; background: rgba(0, 0, 0, 0.45); display: flex; align-items: center; justify-content: center; z-index: 60; padding: 16px; }
.modal-card { background: var(--color-bg, #141414); color: inherit; border: 1px solid var(--color-border, #333); border-radius: 12px; width: min(640px, 100%); max-height: 86vh; overflow: auto; padding: 18px 20px; display: flex; flex-direction: column; gap: 12px; }
.modal-head { display: flex; justify-content: space-between; align-items: center; }
.modal-head h3 { margin: 0; font-size: 16px; }
.modal-close { border: none; background: none; font-size: 18px; cursor: pointer; color: inherit; }

.steps { display: flex; gap: 6px; list-style: none; margin: 0; padding: 0; font-size: 12px; }
.steps li { flex: 1; text-align: center; padding: 6px 0; border-radius: 6px; background: var(--color-bg-soft, #1c1c1c); color: var(--color-text-muted, #888); }
.steps li.active { color: var(--color-primary, #4c6ef5); font-weight: 600; }
.steps li.done { text-decoration: line-through; }

.wiz-form { display: flex; flex-direction: column; gap: 10px; }
.field { display: flex; flex-direction: column; gap: 4px; }
.field-lbl { font-size: 12px; color: var(--color-text-muted, #888); }
.field-in { border: 1px solid var(--color-border, #333); border-radius: 6px; padding: 7px 10px; font-size: 13px; background: transparent; color: inherit; width: 100%; }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; }
.hint { font-size: 12px; color: var(--color-text-muted, #888); margin: 0; }
.form-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 6px; }

.host-row { display: flex; align-items: center; gap: 10px; padding: 8px 10px; border: 1px solid var(--color-border, #333); border-radius: 8px; flex-wrap: wrap; }
.host-row.picked { border-color: var(--color-primary, #4c6ef5); }
.host-pick { display: flex; align-items: center; gap: 8px; min-width: 200px; }
.host-name { font-weight: 600; font-size: 13px; }
.host-addr { font-size: 12px; color: var(--color-text-muted, #888); }
.host-boot { font-size: 12px; display: flex; align-items: center; gap: 4px; white-space: nowrap; }
.role-toggles { display: inline-flex; gap: 4px; }
.devices { max-width: 240px; }

.confirm-grid { display: grid; grid-template-columns: auto 1fr; gap: 6px 14px; margin: 0; font-size: 13px; }
.confirm-grid dt { color: var(--color-text-muted, #888); }
.confirm-grid dd { margin: 0; }

.link-btn { background: none; border: none; color: var(--color-primary, #4c6ef5); cursor: pointer; font-size: 13px; padding: 0; text-decoration: none; }
</style>
