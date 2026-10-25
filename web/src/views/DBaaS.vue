<script setup lang="ts">
/**
 * DBaaS — 数据库即服务(MySQL / PostgreSQL / Redis)。
 *
 * 区块:
 *  1. 实例列表:引擎/版本/目标机/端口/容器实时状态/上次备份/计划;行内 启动/停止/重启/
 *     备份/详情/删除(可选保留数据卷)。
 *  2. 创建向导弹窗:引擎+版本(白名单)→ 目标服务器 → 端口(默认自动分配)→ 口令
 *     (默认自动生成,创建响应一次性展示)→ CPU/内存限额 → 备份计划。
 *  3. 详情抽屉:连接信息(host:port/用户/口令揭示[审计]/DSN 复制 + 跳转在线管理)、
 *     备份列表(立即备份/下载/恢复[先自动安全备份]/删除/传对象存储)、备份计划编辑。
 */
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useConfirm } from '../composables/useConfirm'
import {
  listDBaaSInstances, createDBaaSInstance, dbaaSAction, deleteDBaaSInstance,
  revealDBaaSCredentials, setDBaaSSchedule, listDBaaSBackups, runDBaaSBackup,
  restoreDBaaSFromBackup, restoreDBaaSUpload, restoreDBaaSFromCloud,
  dbaaSBackupDownloadUrl, deleteDBaaSBackup,
  uploadDBaaSBackup, type DBaaSInstance, type DBaaSBackup, type DBaaSEngine,
} from '../api/dbaas'
import { listServers, type Server } from '../api/servers'
import { listObjectStores, browseObjectStore, type ObjectStore, type ObjectStoreEntry } from '../api/objectStores'
import { HttpError } from '../api/http'
import AppButton from '../components/ui/AppButton.vue'
import AppBanner from '../components/ui/AppBanner.vue'
import StatusBadge from '../components/ui/StatusBadge.vue'

const { t } = useI18n()
const confirm = useConfirm()

// ─── 实例列表 ────────────────────────────────────────────────────────────────
const instances = ref<DBaaSInstance[]>([])
const loadState = ref<'loading' | 'ok' | 'error'>('loading')
const loadError = ref('')
const actionError = ref('')
const busyId = ref('')

async function load(refresh = true): Promise<void> {
  loadState.value = 'loading'
  try {
    instances.value = await listDBaaSInstances(refresh)
    loadState.value = 'ok'
  } catch (err) {
    loadState.value = 'error'
    loadError.value = err instanceof HttpError ? err.message : String(err)
  }
}

async function doAction(inst: DBaaSInstance, action: 'start' | 'stop' | 'restart'): Promise<void> {
  busyId.value = inst.id
  actionError.value = ''
  try {
    await dbaaSAction(inst.id, action)
    await load()
  } catch (err) {
    actionError.value = err instanceof HttpError ? err.message : String(err)
  } finally {
    busyId.value = ''
  }
}

async function removeInstance(inst: DBaaSInstance): Promise<void> {
  const keep = await confirm.open({
    title: t('dbaas.deleteTitle', { name: inst.name }),
    body: t('dbaas.deleteBody'),
    confirmLabel: t('dbaas.deleteConfirm'),
    confirmText: inst.name,
  })
  if (!keep) return
  // 二段确认:是否保留数据卷。
  const keepVolumes = await confirm.open({
    title: t('dbaas.deleteVolumesTitle'),
    body: t('dbaas.deleteVolumesBody'),
    confirmLabel: t('dbaas.deleteVolumesKeep'),
  })
  busyId.value = inst.id
  try {
    await deleteDBaaSInstance(inst.id, keepVolumes)
    detail.value = null
    await load()
  } catch (err) {
    actionError.value = err instanceof HttpError ? err.message : String(err)
  } finally {
    busyId.value = ''
  }
}

function liveBadge(s: string): 'success' | 'failed' | 'queued' {
  if (s.startsWith('Up')) return 'success'
  if (s === '') return 'queued'
  return 'failed'
}

// ─── 创建向导 ────────────────────────────────────────────────────────────────
const showCreate = ref(false)
const creating = ref(false)
const createError = ref('')
const createdPassword = ref('')
const createdName = ref('')
const servers = ref<Server[]>([])

const engineMeta: Record<DBaaSEngine, { versions: string[]; icon: string }> = {
  mysql: { versions: ['8.0', '8.4'], icon: '🐬' },
  postgres: { versions: ['16', '17'], icon: '🐘' },
  redis: { versions: ['7'], icon: '🟥' },
}

const form = ref({
  name: '', engine: 'mysql' as DBaaSEngine, version: '8.4',
  serverId: '', port: 0, adminPassword: '',
  cpus: '', memory: '', backupIntervalHours: 24, backupRetention: 7,
})

function openCreate(): void {
  form.value = {
    name: '', engine: 'mysql', version: '8.4', serverId: servers.value[0]?.id ?? '',
    port: 0, adminPassword: '', cpus: '', memory: '',
    backupIntervalHours: 24, backupRetention: 7,
  }
  createError.value = ''
  createdPassword.value = ''
  showCreate.value = true
}

function switchEngine(e: DBaaSEngine): void {
  form.value.engine = e
  form.value.version = engineMeta[e].versions[0]
}

async function submitCreate(): Promise<void> {
  creating.value = true
  createError.value = ''
  try {
    const inst = await createDBaaSInstance({
      name: form.value.name, engine: form.value.engine, version: form.value.version,
      serverId: form.value.serverId, port: form.value.port || undefined,
      adminPassword: form.value.adminPassword || undefined,
      cpus: form.value.cpus || undefined, memory: form.value.memory || undefined,
      backupIntervalHours: form.value.backupIntervalHours,
      backupRetention: form.value.backupRetention,
    })
    createdPassword.value = inst.createPassword ?? ''
    createdName.value = inst.name
    showCreate.value = false
    await load()
  } catch (err) {
    createError.value = err instanceof HttpError ? err.message : String(err)
  } finally {
    creating.value = false
  }
}

// ─── 详情抽屉 ────────────────────────────────────────────────────────────────
const detail = ref<DBaaSInstance | null>(null)
const detailBackups = ref<DBaaSBackup[]>([])
const detailError = ref('')
const detailBusy = ref(false)
const revealed = ref('')
const providers = ref<ObjectStore[]>([])
const restoreFile = ref<File | null>(null)
const restoreInput = ref<HTMLInputElement | null>(null)

async function openDetail(inst: DBaaSInstance): Promise<void> {
  detail.value = inst
  revealed.value = ''
  restoreFile.value = null
  detailError.value = ''
  await reloadDetail()
}

async function reloadDetail(): Promise<void> {
  if (!detail.value) return
  try {
    detailBackups.value = await listDBaaSBackups(detail.value.id)
    detailError.value = ''
  } catch (err) {
    detailError.value = err instanceof HttpError ? err.message : String(err)
  }
}

async function reveal(): Promise<void> {
  if (!detail.value) return
  try {
    revealed.value = await revealDBaaSCredentials(detail.value.id)
  } catch (err) {
    detailError.value = err instanceof HttpError ? err.message : String(err)
  }
}

function dsn(inst: DBaaSInstance): string {
  switch (inst.engine) {
    case 'mysql': return `mysql://root:${revealed.value || '***'}@${inst.host}:${inst.hostPort}/`
    case 'postgres': return `postgres://postgres:${revealed.value || '***'}@${inst.host}:${inst.hostPort}/postgres`
    case 'redis': return `redis://:${revealed.value || '***'}@${inst.host}:${inst.hostPort}/0`
  }
}

async function saveSchedule(): Promise<void> {
  if (!detail.value) return
  detailBusy.value = true
  try {
    await setDBaaSSchedule(detail.value.id, detail.value.backupIntervalHours, detail.value.backupRetention)
    await load()
  } catch (err) {
    detailError.value = err instanceof HttpError ? err.message : String(err)
  } finally {
    detailBusy.value = false
  }
}

async function backupNow(): Promise<void> {
  if (!detail.value) return
  detailBusy.value = true
  detailError.value = ''
  try {
    const rec = await runDBaaSBackup(detail.value.id)
    if (rec.status === 'failed') detailError.value = rec.error || 'backup failed'
    await reloadDetail()
    await load()
  } catch (err) {
    detailError.value = err instanceof HttpError ? err.message : String(err)
  } finally {
    detailBusy.value = false
  }
}

async function restoreFrom(bid: string): Promise<void> {
  if (!detail.value) return
  const ok = await confirm.open({
    title: t('dbaas.restoreTitle'),
    body: t('dbaas.restoreBody'),
    confirmLabel: t('dbaas.restoreConfirm'),
  })
  if (!ok) return
  detailBusy.value = true
  try {
    const res = await restoreDBaaSFromBackup(detail.value.id, bid)
    if (!res.ok) detailError.value = res.error ?? 'restore failed'
    await reloadDetail()
  } catch (err) {
    detailError.value = err instanceof HttpError ? err.message : String(err)
  } finally {
    detailBusy.value = false
  }
}

async function restoreUpload(e: Event): Promise<void> {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file || !detail.value) return
  const ok = await confirm.open({
    title: t('dbaas.restoreTitle'),
    body: t('dbaas.restoreBody'),
    confirmLabel: t('dbaas.restoreConfirm'),
  })
  if (!ok) return
  detailBusy.value = true
  try {
    const res = await restoreDBaaSUpload(detail.value.id, file)
    if (!res.ok) detailError.value = res.error ?? 'restore failed'
    await reloadDetail()
  } catch (err) {
    detailError.value = err instanceof HttpError ? err.message : String(err)
  } finally {
    detailBusy.value = false
    input.value = ''
  }
}

async function removeBackup(b: DBaaSBackup): Promise<void> {
  try {
    await deleteDBaaSBackup(b.id)
    await reloadDetail()
  } catch (err) {
    detailError.value = err instanceof HttpError ? err.message : String(err)
  }
}

// ─── 从对象存储恢复(云端备份直拉) ────────────────────────────────────────────
const cloudProvider = ref('')
const cloudObjects = ref<ObjectStoreEntry[]>([])
const cloudSel = ref('')
const cloudLoading = ref(false)

async function loadCloudObjects(): Promise<void> {
  if (!detail.value || !cloudProvider.value) return
  cloudLoading.value = true
  cloudSel.value = ''
  detailError.value = ''
  try {
    // DBaaS 上传的对象键前缀为 dbaas/<实例名>/(相对配置 PathPrefix)。
    const res = await browseObjectStore(cloudProvider.value, { prefix: `dbaas/${detail.value.name}/`, maxKeys: 200 })
    cloudObjects.value = (res.objects ?? []).filter((o) => o.key.endsWith('.sql') || o.key.endsWith('.rdb'))
    if (cloudObjects.value.length === 0) detailError.value = t('dbaas.cloudEmpty')
  } catch (err) {
    detailError.value = err instanceof HttpError ? err.message : t('dbaas.errCloudList')
  } finally {
    cloudLoading.value = false
  }
}

async function restoreFromCloud(): Promise<void> {
  if (!detail.value || !cloudSel.value) return
  const ok = await confirm.open({
    title: t('dbaas.restoreTitle'),
    body: t('dbaas.restoreBody'),
    confirmLabel: t('dbaas.restoreConfirm'),
  })
  if (!ok) return
  detailBusy.value = true
  try {
    const res = await restoreDBaaSFromCloud(detail.value.id, cloudProvider.value, cloudSel.value)
    if (!res.ok) detailError.value = res.error ?? 'restore failed'
    await reloadDetail()
  } catch (err) {
    detailError.value = err instanceof HttpError ? err.message : String(err)
  } finally {
    detailBusy.value = false
  }
}

async function cloudBackup(b: DBaaSBackup): Promise<void> {
  if (providers.value.length === 0) {
    detailError.value = t('dbaas.errNoProvider')
    return
  }
  try {
    await uploadDBaaSBackup(b.id, providers.value[0].id)
    await reloadDetail()
  } catch (err) {
    detailError.value = err instanceof HttpError ? err.message : String(err)
  }
}

function formatSize(bytes: number): string {
  if (bytes >= 1 << 30) return `${(bytes / (1 << 30)).toFixed(2)} GB`
  if (bytes >= 1 << 20) return `${(bytes / (1 << 20)).toFixed(1)} MB`
  if (bytes >= 1 << 10) return `${(bytes / (1 << 10)).toFixed(1)} KB`
  return `${bytes} B`
}

function formatTime(iso: string): string {
  return iso ? iso.replace('T', ' ').slice(0, 19) : ''
}

const engineLabel = (e: string): string => t(`dbaas.engine_${e}`)

// ─── 生命周期 ────────────────────────────────────────────────────────────────
onMounted(async () => {
  await load()
  try {
    servers.value = (await listServers()) ?? []
  } catch {
    servers.value = []
  }
  try {
    providers.value = (await listObjectStores()) ?? []
  } catch {
    providers.value = []
  }
})
</script>

<template>
  <section class="db" aria-labelledby="db-heading">
    <header class="db-head">
      <div>
        <h2 id="db-heading" class="db-title">{{ t('dbaas.title') }}</h2>
        <p class="db-sub">{{ t('dbaas.subtitle') }}</p>
      </div>
      <div class="db-actions">
        <AppButton @click="load()">{{ t('common.refresh') }}</AppButton>
        <AppButton variant="primary" @click="openCreate">{{ t('dbaas.create') }}</AppButton>
      </div>
    </header>

    <AppBanner v-if="createdPassword" variant="success" :title="t('dbaas.createdTitle', { name: createdName })">
      {{ t('dbaas.createdBody') }}
      <code class="db-pass">{{ createdPassword }}</code>
    </AppBanner>

    <p v-if="actionError" class="db-error" role="alert">{{ actionError }}</p>

    <div v-if="loadState === 'loading'" class="db-state" aria-busy="true">{{ t('dbaas.loading') }}</div>
    <div v-else-if="loadState === 'error'" class="db-state db-state--error" role="alert">{{ loadError }}</div>

    <table v-else-if="instances.length" class="db-table">
      <thead>
        <tr>
          <th>{{ t('dbaas.colName') }}</th>
          <th>{{ t('dbaas.colEngine') }}</th>
          <th>{{ t('dbaas.colServer') }}</th>
          <th>{{ t('dbaas.colEndpoint') }}</th>
          <th>{{ t('dbaas.colLive') }}</th>
          <th>{{ t('dbaas.colLastBackup') }}</th>
          <th>{{ t('dbaas.colPlan') }}</th>
          <th class="db-ops">{{ t('dbaas.colOps') }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="inst in instances" :key="inst.id">
          <td class="db-mono">{{ inst.name }}</td>
          <td>{{ engineMeta[inst.engine]?.icon }} {{ engineLabel(inst.engine) }} {{ inst.version }}</td>
          <td>{{ inst.serverName || inst.serverId }}</td>
          <td class="db-mono">{{ inst.host }}:{{ inst.hostPort }}</td>
          <td><StatusBadge v-if="inst.liveStatus" :status="liveBadge(inst.liveStatus)" />{{ inst.liveStatus }}</td>
          <td>{{ inst.lastBackupAt ? formatTime(inst.lastBackupAt) : '—' }}</td>
          <td>{{ inst.backupIntervalHours > 0 ? t('dbaas.everyHours', { n: inst.backupIntervalHours }) : '—' }}</td>
          <td class="db-ops">
            <button class="db-link" :disabled="busyId === inst.id" @click="openDetail(inst)">{{ t('dbaas.opDetail') }}</button>
            <button class="db-link" :disabled="busyId === inst.id" @click="doAction(inst, 'restart')">{{ t('dbaas.opRestart') }}</button>
            <button class="db-link" :disabled="busyId === inst.id" @click="doAction(inst, inst.liveStatus.startsWith('Up') ? 'stop' : 'start')">
              {{ inst.liveStatus.startsWith('Up') ? t('dbaas.opStop') : t('dbaas.opStart') }}
            </button>
            <button class="db-link db-link--danger" :disabled="busyId === inst.id" @click="removeInstance(inst)">{{ t('dbaas.opDelete') }}</button>
          </td>
        </tr>
      </tbody>
    </table>
    <p v-else class="db-empty">{{ t('dbaas.empty') }}</p>

    <!-- 创建向导 -->
    <div v-if="showCreate" class="db-modal" role="dialog" :aria-label="t('dbaas.createTitle')">
      <div class="db-modal-card">
        <h3 class="db-modal-title">{{ t('dbaas.createTitle') }}</h3>
        <div class="db-form">
          <label class="db-field">
            <span>{{ t('dbaas.fName') }}</span>
            <input v-model="form.name" class="db-input" :placeholder="t('dbaas.fNameHint')" />
          </label>
          <div class="db-field">
            <span>{{ t('dbaas.fEngine') }}</span>
            <div class="db-engines">
              <button v-for="(meta, e) in engineMeta" :key="e" type="button" class="db-engine"
                :class="{ 'db-engine--on': form.engine === e }" @click="switchEngine(e as DBaaSEngine)">
                <span>{{ meta.icon }}</span>{{ engineLabel(e) }}
              </button>
            </div>
          </div>
          <label class="db-field">
            <span>{{ t('dbaas.fVersion') }}</span>
            <select v-model="form.version" class="db-input">
              <option v-for="v in engineMeta[form.engine].versions" :key="v" :value="v">{{ v }}</option>
            </select>
          </label>
          <label class="db-field">
            <span>{{ t('dbaas.fServer') }}</span>
            <select v-model="form.serverId" class="db-input">
              <option v-for="s in servers" :key="s.id" :value="s.id">{{ s.name }}（{{ s.host }}）</option>
            </select>
          </label>
          <label class="db-field">
            <span>{{ t('dbaas.fPort') }}</span>
            <input v-model.number="form.port" type="number" min="0" max="65535" class="db-input"
              :placeholder="t('dbaas.fPortAuto')" />
          </label>
          <label class="db-field">
            <span>{{ t('dbaas.fPassword') }}</span>
            <input v-model="form.adminPassword" type="text" class="db-input" autocomplete="off"
              :placeholder="t('dbaas.fPasswordAuto')" />
          </label>
          <label class="db-field">
            <span>{{ t('dbaas.fCpus') }}</span>
            <input v-model="form.cpus" class="db-input" placeholder="2" />
          </label>
          <label class="db-field">
            <span>{{ t('dbaas.fMemory') }}</span>
            <input v-model="form.memory" class="db-input" placeholder="1g" />
          </label>
          <label class="db-field">
            <span>{{ t('dbaas.fInterval') }}</span>
            <input v-model.number="form.backupIntervalHours" type="number" min="0" max="720" class="db-input" />
          </label>
          <label class="db-field">
            <span>{{ t('dbaas.fRetention') }}</span>
            <input v-model.number="form.backupRetention" type="number" min="1" max="365" class="db-input" />
          </label>
        </div>
        <p v-if="createError" class="db-error" role="alert">{{ createError }}</p>
        <div class="db-modal-actions">
          <AppButton @click="showCreate = false">{{ t('dbaas.cancel') }}</AppButton>
          <AppButton variant="primary" :loading="creating"
            :disabled="!form.name || !form.serverId" @click="submitCreate">{{ t('dbaas.createGo') }}</AppButton>
        </div>
      </div>
    </div>

    <!-- 详情抽屉 -->
    <div v-if="detail" class="db-modal" role="dialog" :aria-label="t('dbaas.detailTitle')">
      <div class="db-modal-card db-modal-card--wide">
        <div class="db-modal-head">
          <h3 class="db-modal-title">{{ engineMeta[detail.engine]?.icon }} {{ detail.name }}
            <span class="db-sub-inline">{{ engineLabel(detail.engine) }} {{ detail.version }} · {{ detail.serverName }}</span>
          </h3>
          <button class="db-close" :aria-label="t('dbaas.close')" @click="detail = null">✕</button>
        </div>
        <p v-if="detailError" class="db-error" role="alert">{{ detailError }}</p>

        <!-- 连接信息 -->
        <div class="db-card">
          <h4 class="db-card-title">{{ t('dbaas.connTitle') }}</h4>
          <div class="db-conn">
            <div class="db-conn-row"><span>host:port</span><code>{{ detail.host }}:{{ detail.hostPort }}</code></div>
            <div class="db-conn-row"><span>{{ t('dbaas.connUser') }}</span><code>{{ detail.engine === 'postgres' ? 'postgres' : detail.engine === 'mysql' ? 'root' : '—' }}</code></div>
            <div class="db-conn-row"><span>{{ t('dbaas.connPass') }}</span>
              <span class="db-conn-pass">
                <code v-if="revealed">{{ revealed }}</code>
                <AppButton variant="ghost" @click="reveal">{{ t('dbaas.reveal') }}</AppButton>
              </span>
            </div>
            <div class="db-conn-row"><span>DSN</span><code class="db-dsn">{{ dsn(detail) }}</code></div>
          </div>
          <div class="db-actions">
            <router-link v-if="detail.engine !== 'redis'" to="/databases" class="db-link">{{ t('dbaas.openDatabases') }} →</router-link>
            <router-link v-else to="/redis" class="db-link">{{ t('dbaas.openRedis') }} →</router-link>
          </div>
        </div>

        <!-- 备份计划 -->
        <div class="db-card">
          <h4 class="db-card-title">{{ t('dbaas.planTitle') }}</h4>
          <div class="db-plan">
            <label class="db-field db-field--inline">
              <span>{{ t('dbaas.fInterval') }}</span>
              <input v-model.number="detail.backupIntervalHours" type="number" min="0" max="720" class="db-input" />
            </label>
            <label class="db-field db-field--inline">
              <span>{{ t('dbaas.fRetention') }}</span>
              <input v-model.number="detail.backupRetention" type="number" min="1" max="365" class="db-input" />
            </label>
            <AppButton :loading="detailBusy" @click="saveSchedule">{{ t('dbaas.savePlan') }}</AppButton>
          </div>
        </div>

        <!-- 备份列表 -->
        <div class="db-card">
          <div class="db-card-row">
            <h4 class="db-card-title">{{ t('dbaas.backupsTitle') }}</h4>
            <div class="db-actions">
              <label class="db-upload">
                <input ref="restoreInput" type="file" accept=".sql,.rdb" class="db-file-hidden" @change="restoreUpload" />
                <span class="db-link">{{ t('dbaas.restoreUpload') }}</span>
              </label>
              <AppButton variant="primary" :loading="detailBusy" @click="backupNow">{{ t('dbaas.backupNow') }}</AppButton>
            </div>
          </div>
          <!-- 从对象存储恢复:选配置 → 列出 dbaas/<实例名>/ 下的备份对象 → 恢复 -->
          <div v-if="providers.length" class="db-cloud">
            <select v-model="cloudProvider" class="db-input db-cloud-sel" :aria-label="t('dbaas.cloudProvider')"
              @change="cloudObjects = []; cloudSel = ''">
              <option value="">{{ t('dbaas.cloudRestore') }}</option>
              <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.name }}（{{ p.type }}）</option>
            </select>
            <AppButton :disabled="!cloudProvider" :loading="cloudLoading" @click="loadCloudObjects">
              {{ t('dbaas.cloudList') }}
            </AppButton>
            <select v-if="cloudObjects.length" v-model="cloudSel" class="db-input db-cloud-sel"
              :aria-label="t('dbaas.cloudObject')">
              <option v-for="o in cloudObjects" :key="o.key" :value="o.key">{{ o.key.slice(o.key.lastIndexOf('/') + 1) }}</option>
            </select>
            <AppButton v-if="cloudObjects.length" variant="danger" :disabled="!cloudSel || detailBusy" @click="restoreFromCloud">
              {{ t('dbaas.restoreConfirm') }}
            </AppButton>
          </div>
          <table v-if="detailBackups.length" class="db-table db-table--tight">
            <thead>
              <tr>
                <th>{{ t('dbaas.bFile') }}</th>
                <th>{{ t('dbaas.bSize') }}</th>
                <th>{{ t('dbaas.bKind') }}</th>
                <th>{{ t('dbaas.bStatus') }}</th>
                <th>{{ t('dbaas.bTime') }}</th>
                <th class="db-ops">{{ t('dbaas.colOps') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="b in detailBackups" :key="b.id">
                <td class="db-mono" :title="b.sha256">{{ b.fileName }}</td>
                <td>{{ formatSize(b.sizeBytes) }}</td>
                <td>{{ t(`dbaas.kind_${b.kind}`) }}</td>
                <td>
                  <StatusBadge :status="b.status === 'completed' ? 'success' : b.status === 'failed' ? 'failed' : 'running'" />
                  <span v-if="b.status === 'failed' && b.error" class="db-err" :title="b.error">{{ b.error.slice(0, 40) }}</span>
                  <span v-if="b.objectKey" class="db-cloud" title="object storage">☁</span>
                </td>
                <td>{{ formatTime(b.createdAt) }}</td>
                <td class="db-ops">
                  <a v-if="b.status === 'completed'" class="db-link" :href="dbaaSBackupDownloadUrl(b.id)">{{ t('dbaas.opDownload') }}</a>
                  <button v-if="b.status === 'completed'" class="db-link" @click="restoreFrom(b.id)">{{ t('dbaas.opRestore') }}</button>
                  <button v-if="b.status === 'completed' && providers.length" class="db-link" @click="cloudBackup(b)">☁</button>
                  <button class="db-link db-link--danger" @click="removeBackup(b)">{{ t('dbaas.opDelete') }}</button>
                </td>
              </tr>
            </tbody>
          </table>
          <p v-else class="db-empty">{{ t('dbaas.noBackups') }}</p>
          <p class="db-hint">{{ t('dbaas.restoreNote') }}</p>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.db { display: flex; flex-direction: column; gap: 16px; }
.db-head { display: flex; align-items: flex-end; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.db-title { margin: 0; font-size: 18px; font-weight: 700; }
.db-sub { margin: 4px 0 0; font-size: 12.5px; opacity: 0.72; }
.db-actions { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.db-state { padding: 28px; text-align: center; opacity: 0.7; }
.db-state--error { color: #c62828; opacity: 1; }
.db-error { color: #c62828; font-size: 12.5px; margin: 0; }
.db-table { width: 100%; border-collapse: collapse; font-size: 12.5px; }
.db-table th, .db-table td { text-align: left; padding: 8px; border-bottom: 1px solid var(--color-border, #eee); vertical-align: middle; }
.db-table th { font-weight: 600; opacity: 0.75; }
.db-table--tight th, .db-table--tight td { padding: 6px; }
.db-ops { white-space: nowrap; }
.db-mono { font-family: ui-monospace, monospace; }
.db-err { display: block; font-size: 11px; color: #c62828; max-width: 220px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.db-cloud { margin-left: 4px; }
.db-link { background: none; border: none; padding: 0; color: var(--color-primary, #3b82f6); cursor: pointer; font-size: 12.5px; text-decoration: none; margin-right: 10px; }
.db-link--danger { color: #c62828; }
.db-link:disabled { opacity: 0.5; cursor: not-allowed; }
.db-empty { margin: 0; opacity: 0.6; font-size: 12.5px; }
.db-pass { font-family: ui-monospace, monospace; padding: 2px 8px; background: var(--color-bg-subtle, #f4f4f4); border-radius: 4px; margin-left: 8px; user-select: all; }
.db-hint { font-size: 11.5px; opacity: 0.6; margin: 4px 0 0; }
/* 弹窗/抽屉 */
.db-modal { position: fixed; inset: 0; background: rgba(0, 0, 0, 0.45); display: flex; align-items: center; justify-content: center; z-index: 60; padding: 20px; }
.db-modal-card { background: var(--color-bg-card, #fff); border-radius: 12px; padding: 20px; width: min(520px, 100%); max-height: 90vh; overflow-y: auto; display: flex; flex-direction: column; gap: 14px; }
.db-modal-card--wide { width: min(760px, 100%); }
.db-modal-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 10px; }
.db-modal-title { margin: 0; font-size: 15px; font-weight: 700; }
.db-sub-inline { font-weight: 400; font-size: 12px; opacity: 0.65; margin-left: 8px; }
.db-close { background: none; border: none; font-size: 14px; cursor: pointer; opacity: 0.6; }
.db-form { display: grid; grid-template-columns: 1fr 1fr; gap: 10px 14px; }
.db-field { display: flex; flex-direction: column; gap: 4px; font-size: 12.5px; }
.db-field > span { font-weight: 600; }
.db-field--inline { flex-direction: row; align-items: center; gap: 8px; }
.db-input { padding: 6px 9px; border: 1px solid var(--color-border, #ccc); border-radius: 6px; font-size: 13px; background: var(--color-bg-input, #fff); color: inherit; width: 100%; box-sizing: border-box; }
.db-engines { display: flex; gap: 8px; }
.db-engine { display: flex; align-items: center; gap: 6px; padding: 8px 14px; border: 1px solid var(--color-border, #ccc); border-radius: 8px; background: transparent; cursor: pointer; font-size: 13px; color: inherit; }
.db-engine--on { border-color: var(--color-primary, #3b82f6); color: var(--color-primary, #3b82f6); font-weight: 600; }
.db-modal-actions { display: flex; justify-content: flex-end; gap: 10px; }
.db-card { border: 1px solid var(--color-border, #e2e2e2); border-radius: 10px; padding: 14px; display: flex; flex-direction: column; gap: 10px; }
.db-card-row { display: flex; align-items: center; justify-content: space-between; gap: 10px; flex-wrap: wrap; }
.db-card-title { margin: 0; font-size: 13.5px; font-weight: 650; }
.db-conn { display: flex; flex-direction: column; gap: 6px; font-size: 12.5px; }
.db-conn-row { display: flex; gap: 12px; align-items: center; }
.db-conn-row > span:first-child { width: 90px; opacity: 0.65; }
.db-conn-pass { display: flex; align-items: center; gap: 8px; }
.db-dsn { word-break: break-all; }
.db-plan { display: flex; gap: 14px; align-items: center; flex-wrap: wrap; }
.db-plan .db-input { width: 110px; }
.db-cloud { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.db-cloud-sel { max-width: 280px; width: auto; }
.db-file-hidden { display: none; }
.db-upload { cursor: pointer; }
</style>
