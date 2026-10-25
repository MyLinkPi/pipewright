<script setup lang="ts">
/**
 * BackupPanel — 已登记实例的备份面板(共享组件;/databases 备份标签页与 /redis 折叠区
 * 各实例化一次)。内容:备份计划(间隔/保留/自动上云) + 备份记录表(状态轮询) +
 * 立即备份/上传恢复/从对象存储恢复 + 下载/删除/传对象存储。
 *
 * 破坏性动作(恢复)经 restoreGate 注入各页自己的写门禁:/databases 传 guardWrite
 * (只读弹解锁),/redis 传只读禁用+确认。组件内部再用 useConfirm 做二次确认。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useConfirm } from '../../composables/useConfirm'
import {
  getDBBackupConfig, setDBBackupConfig, runDBBackup, listDBBackups,
  restoreDBBackupById, restoreDBBackupUpload, restoreDBBackupFromCloud,
  dbBackupDownloadUrl, deleteDBBackup, uploadDBBackupToObjectStore,
  type DBBackupKind, type DBBackupConfig, type DBBackupRecord,
} from '../../api/dbBackup'
import { browseObjectStore, type ObjectStore, type ObjectStoreEntry } from '../../api/objectStores'
import { HttpError } from '../../api/http'
import StatusBadge from '../ui/StatusBadge.vue'

const props = defineProps<{
  kind: DBBackupKind
  instanceId: string
  instanceName: string
  readonly?: boolean
  providers?: ObjectStore[]
  /** 恢复动作的写门禁(各页注入;未提供则直接执行)。 */
  restoreGate?: (action: () => void) => void
}>()

const { t } = useI18n()
const confirm = useConfirm()

// ─── 配置 + 能力 ─────────────────────────────────────────────────────────────
const cfg = ref<DBBackupConfig | null>(null)
const plan = ref({ intervalHours: 0, retention: 7, autoUploadProviderId: '' })
const loading = ref(true)
const saving = ref(false)
const savedFlash = ref(false)
const error = ref('')

const supported = computed(() => cfg.value?.backupable ?? false)

async function loadConfig(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    cfg.value = await getDBBackupConfig(props.kind, props.instanceId)
    plan.value = {
      intervalHours: cfg.value.intervalHours,
      retention: cfg.value.retention,
      autoUploadProviderId: cfg.value.autoUploadProviderId,
    }
  } catch (err) {
    error.value = err instanceof HttpError ? err.message : t('dbBackup.errLoadConfig')
  } finally {
    loading.value = false
  }
}

async function savePlan(): Promise<void> {
  saving.value = true
  savedFlash.value = false
  try {
    await setDBBackupConfig(props.kind, props.instanceId, {
      intervalHours: plan.value.intervalHours,
      retention: plan.value.retention,
      autoUploadProviderId: plan.value.autoUploadProviderId || undefined,
    })
    savedFlash.value = true
    setTimeout(() => { savedFlash.value = false }, 2500)
  } catch (err) {
    error.value = err instanceof HttpError ? err.message : t('dbBackup.errSaveConfig')
  } finally {
    saving.value = false
  }
}

// ─── 记录列表 + 轮询 ─────────────────────────────────────────────────────────
const records = ref<DBBackupRecord[]>([])
const running = ref(false)
let pollTimer: number | undefined

const hasRunning = computed(() => records.value.some((r) => r.status === 'running'))

async function refreshRecords(): Promise<void> {
  try {
    records.value = await listDBBackups(props.kind, props.instanceId)
  } catch {
    /* 列表失败静默(顶部已有配置区错误位) */
  }
}

async function backupNow(): Promise<void> {
  running.value = true
  error.value = ''
  try {
    const rec = await runDBBackup(props.kind, props.instanceId)
    if (rec.status === 'failed') error.value = rec.error || t('dbBackup.errRun')
    await refreshRecords()
  } catch (err) {
    error.value = err instanceof HttpError ? err.message : t('dbBackup.errRun')
  } finally {
    running.value = false
  }
}

// ─── 恢复(三种来源;一律过门禁 + 二次确认) ──────────────────────────────────
const restoring = ref(false)
const uploadInput = ref<HTMLInputElement | null>(null)

function gate(action: () => void): void {
  if (props.restoreGate) {
    props.restoreGate(action)
    return
  }
  action()
}

async function confirmRestore(): Promise<boolean> {
  return confirm.open({
    title: t('dbBackup.restoreTitle'),
    body: t('dbBackup.restoreBody'),
    confirmLabel: t('dbBackup.restoreGo'),
  })
}

async function doRestore(fn: () => Promise<{ ok: boolean; error?: string }>): Promise<void> {
  restoring.value = true
  error.value = ''
  try {
    const res = await fn()
    if (!res.ok) error.value = res.error ?? t('dbBackup.errRestore')
    await refreshRecords()
  } catch (err) {
    error.value = err instanceof HttpError ? err.message : t('dbBackup.errRestore')
  } finally {
    restoring.value = false
  }
}

function restoreFrom(rec: DBBackupRecord): void {
  gate(async () => {
    if (!(await confirmRestore())) return
    await doRestore(() => restoreDBBackupById(props.kind, props.instanceId, rec.id))
  })
}

function onUploadFile(e: Event): Promise<void> {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return Promise.resolve()
  gate(async () => {
    if (!(await confirmRestore())) return
    await doRestore(() => restoreDBBackupUpload(props.kind, props.instanceId, file))
  })
  input.value = ''
  return Promise.resolve()
}

// ─── 从对象存储恢复 ──────────────────────────────────────────────────────────
const cloudProvider = ref('')
const cloudObjects = ref<ObjectStoreEntry[]>([])
const cloudSel = ref('')
const cloudLoading = ref(false)

async function loadCloudObjects(): Promise<void> {
  cloudLoading.value = true
  cloudSel.value = ''
  error.value = ''
  try {
    const res = await browseObjectStore(cloudProvider.value, { prefix: `db-backups/${props.instanceName}/`, maxKeys: 200 })
    cloudObjects.value = (res.objects ?? []).filter((o) => o.key.endsWith('.sql') || o.key.endsWith('.rdb'))
    if (cloudObjects.value.length === 0) error.value = t('dbBackup.cloudEmpty')
  } catch (err) {
    error.value = err instanceof HttpError ? err.message : t('dbBackup.errCloudList')
  } finally {
    cloudLoading.value = false
  }
}

function restoreFromCloud(): void {
  if (!cloudSel.value) return
  gate(async () => {
    if (!(await confirmRestore())) return
    await doRestore(() => restoreDBBackupFromCloud(props.kind, props.instanceId, cloudProvider.value, cloudSel.value))
  })
}

// ─── 记录操作 ────────────────────────────────────────────────────────────────
async function removeRecord(rec: DBBackupRecord): Promise<void> {
  const ok = await confirm.open({
    title: t('dbBackup.deleteTitle'),
    body: t('dbBackup.deleteBody', { name: rec.fileName }),
    confirmLabel: t('dbBackup.deleteGo'),
  })
  if (!ok) return
  try {
    await deleteDBBackup(rec.id)
    await refreshRecords()
  } catch (err) {
    error.value = err instanceof HttpError ? err.message : t('dbBackup.errDelete')
  }
}

async function cloudRecord(rec: DBBackupRecord): Promise<void> {
  if (!providersFirstId.value) {
    error.value = t('dbBackup.errNoProvider')
    return
  }
  try {
    await uploadDBBackupToObjectStore(rec.id, providersFirstId.value)
    await refreshRecords()
  } catch (err) {
    error.value = err instanceof HttpError ? err.message : t('dbBackup.errUpload')
  }
}

const providersFirstId = computed(() => props.providers?.[0]?.id ?? '')

function formatSize(bytes: number): string {
  if (bytes >= 1 << 30) return `${(bytes / (1 << 30)).toFixed(2)} GB`
  if (bytes >= 1 << 20) return `${(bytes / (1 << 20)).toFixed(1)} MB`
  if (bytes >= 1 << 10) return `${(bytes / (1 << 10)).toFixed(1)} KB`
  return `${bytes} B`
}

function formatTime(iso: string): string {
  return iso ? iso.replace('T', ' ').slice(0, 19) : ''
}

function kindLabel(k: string): string {
  return t(`dbBackup.kind_${k}`)
}

// ─── 生命周期 ────────────────────────────────────────────────────────────────
onMounted(async () => {
  await loadConfig()
  await refreshRecords()
  pollTimer = window.setInterval(() => {
    if (hasRunning.value) void refreshRecords()
  }, 2000)
})

onBeforeUnmount(() => {
  if (pollTimer !== undefined) window.clearInterval(pollTimer)
})
</script>

<template>
  <div class="dbbp">
    <div v-if="loading" class="dbbp-state" aria-busy="true">{{ t('dbBackup.loading') }}</div>

    <template v-else>
      <!-- 能力说明(外部 PG 子集 / 外部 Redis 不支持) -->
      <p v-if="cfg?.note" class="dbbp-note" :class="{ 'dbbp-note--block': !supported }" role="note">{{ cfg.note }}</p>

      <template v-if="supported">
        <!-- 备份计划 -->
        <div class="dbbp-row dbbp-plan">
          <label class="dbbp-field">
            <span>{{ t('dbBackup.planInterval') }}</span>
            <input v-model.number="plan.intervalHours" type="number" min="0" max="720" class="dbbp-input dbbp-narrow" />
          </label>
          <label class="dbbp-field">
            <span>{{ t('dbBackup.planRetention') }}</span>
            <input v-model.number="plan.retention" type="number" min="1" max="365" class="dbbp-input dbbp-narrow" />
          </label>
          <label v-if="providers?.length" class="dbbp-field dbbp-grow">
            <span>{{ t('dbBackup.planAutoUpload') }}</span>
            <select v-model="plan.autoUploadProviderId" class="dbbp-input">
              <option value="">{{ t('dbBackup.planNoUpload') }}</option>
              <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.name }}（{{ p.type }}）</option>
            </select>
          </label>
          <button class="dbbp-btn" :disabled="saving" @click="savePlan">{{ t('dbBackup.planSave') }}</button>
          <span v-if="savedFlash" class="dbbp-flash">{{ t('dbBackup.saved') }}</span>
        </div>
        <p class="dbbp-hint">
          {{ plan.intervalHours > 0 ? t('dbBackup.planHintOn', { n: plan.intervalHours }) : t('dbBackup.planHintOff') }}
          <template v-if="cfg?.lastBackupAt"> · {{ t('dbBackup.lastRun') }} {{ formatTime(cfg.lastBackupAt) }}</template>
        </p>

        <!-- 动作行 -->
        <div class="dbbp-row">
          <button class="dbbp-btn dbbp-btn--primary" :disabled="running || hasRunning" @click="backupNow">
            {{ t('dbBackup.runNow') }}
          </button>
          <label class="dbbp-upload">
            <input type="file" :accept="kind === 'redis' ? '.rdb' : '.sql'" class="dbbp-file-hidden" @change="onUploadFile" />
            <span class="dbbp-btn" role="button" tabindex="0">{{ t('dbBackup.restoreUpload') }}</span>
          </label>
          <template v-if="providers?.length">
            <select v-model="cloudProvider" class="dbbp-input" @change="cloudObjects = []; cloudSel = ''">
              <option value="">{{ t('dbBackup.cloudRestore') }}</option>
              <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.name }}（{{ p.type }}）</option>
            </select>
            <button class="dbbp-btn" :disabled="!cloudProvider" @click="loadCloudObjects">{{ t('dbBackup.cloudList') }}</button>
            <select v-if="cloudObjects.length" v-model="cloudSel" class="dbbp-input dbbp-grow">
              <option v-for="o in cloudObjects" :key="o.key" :value="o.key">{{ o.key.slice(o.key.lastIndexOf('/') + 1) }}</option>
            </select>
            <button v-if="cloudObjects.length" class="dbbp-btn dbbp-btn--danger" :disabled="!cloudSel || restoring" @click="restoreFromCloud">
              {{ t('dbBackup.restoreGo') }}
            </button>
          </template>
        </div>

        <!-- 记录表 -->
        <div v-if="records.length" class="dbbp-table-wrap">
          <table class="dbbp-table">
            <thead>
              <tr>
                <th>{{ t('dbBackup.colFile') }}</th>
                <th>{{ t('dbBackup.colSize') }}</th>
                <th>{{ t('dbBackup.colKind') }}</th>
                <th>{{ t('dbBackup.colStatus') }}</th>
                <th>{{ t('dbBackup.colTime') }}</th>
                <th class="dbbp-ops">{{ t('dbBackup.colOps') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="rec in records" :key="rec.id">
                <td class="dbbp-mono" :title="rec.sha256">{{ rec.fileName }}</td>
                <td>{{ formatSize(rec.sizeBytes) }}</td>
                <td>{{ kindLabel(rec.kind) }}</td>
                <td>
                  <StatusBadge :status="rec.status === 'completed' ? 'success' : rec.status === 'failed' ? 'failed' : 'running'" />
                  <span v-if="rec.status === 'failed' && rec.error" class="dbbp-err" :title="rec.error">{{ rec.error.slice(0, 50) }}</span>
                  <span v-if="rec.objectKey" class="dbbp-cloud" title="object storage">☁</span>
                </td>
                <td>{{ formatTime(rec.createdAt) }}</td>
                <td class="dbbp-ops">
                  <a v-if="rec.status === 'completed'" class="dbbp-link" :href="dbBackupDownloadUrl(rec.id)">{{ t('dbBackup.opDownload') }}</a>
                  <button v-if="rec.status === 'completed' && cfg?.restoreable" class="dbbp-link dbbp-link--danger" :disabled="restoring" @click="restoreFrom(rec)">
                    {{ t('dbBackup.opRestore') }}
                  </button>
                  <button v-if="rec.status === 'completed' && providers?.length" class="dbbp-link" title="upload to object storage" @click="cloudRecord(rec)">☁</button>
                  <button class="dbbp-link dbbp-link--danger" @click="removeRecord(rec)">{{ t('dbBackup.opDelete') }}</button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-else class="dbbp-empty">{{ t('dbBackup.empty') }}</p>
        <p class="dbbp-hint">{{ t('dbBackup.restoreNote') }}</p>
      </template>
    </template>

    <p v-if="error" class="dbbp-error" role="alert">{{ error }}</p>
  </div>
</template>

<style scoped>
.dbbp { display: flex; flex-direction: column; gap: 12px; }
.dbbp-state { padding: 24px; text-align: center; opacity: 0.7; font-size: var(--text-body, 13px); }
.dbbp-note { margin: 0; font-size: var(--text-body, 13px); opacity: 0.75; }
.dbbp-note--block { opacity: 1; color: #b45309; }
.dbbp-row { display: flex; gap: 10px; align-items: flex-end; flex-wrap: wrap; }
.dbbp-plan { align-items: flex-end; }
.dbbp-field { display: flex; flex-direction: column; gap: 4px; font-size: 12px; }
.dbbp-field > span { font-weight: 600; opacity: 0.8; }
.dbbp-grow { flex: 1; min-width: 180px; }
.dbbp-narrow { width: 110px; }
.dbbp-input { padding: 6px 9px; border: 1px solid var(--color-border, #ccc); border-radius: 6px; font-size: 13px; background: var(--color-bg-input, #fff); color: inherit; max-width: 280px; }
.dbbp-btn { padding: 6px 14px; border: 1px solid var(--color-border, #ccc); border-radius: 6px; background: var(--color-bg-card, #fff); color: inherit; font-size: 13px; cursor: pointer; }
.dbbp-btn:disabled { opacity: 0.5; cursor: not-allowed; }
.dbbp-btn--primary { border-color: var(--color-primary, #3b82f6); color: var(--color-primary, #3b82f6); font-weight: 600; }
.dbbp-btn--danger { color: #c62828; border-color: #e5b4b4; }
.dbbp-upload { cursor: pointer; }
.dbbp-file-hidden { display: none; }
.dbbp-flash { color: #1a7f37; font-size: 12.5px; }
.dbbp-hint { margin: 0; font-size: 11.5px; opacity: 0.6; }
.dbbp-empty { margin: 0; opacity: 0.6; font-size: 12.5px; }
.dbbp-error { margin: 0; color: #c62828; font-size: 12.5px; }
.dbbp-table-wrap { overflow-x: auto; }
.dbbp-table { width: 100%; border-collapse: collapse; font-size: 12.5px; }
.dbbp-table th, .dbbp-table td { text-align: left; padding: 7px 8px; border-bottom: 1px solid var(--color-border, #eee); vertical-align: middle; }
.dbbp-table th { font-weight: 600; opacity: 0.75; white-space: nowrap; }
.dbbp-ops { white-space: nowrap; }
.dbbp-mono { font-family: ui-monospace, monospace; }
.dbbp-err { display: block; font-size: 11px; color: #c62828; max-width: 240px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.dbbp-cloud { margin-left: 4px; }
.dbbp-link { background: none; border: none; padding: 0; color: var(--color-primary, #3b82f6); cursor: pointer; font-size: 12.5px; text-decoration: none; margin-right: 10px; }
.dbbp-link:disabled { opacity: 0.5; cursor: not-allowed; }
.dbbp-link--danger { color: #c62828; }
</style>
