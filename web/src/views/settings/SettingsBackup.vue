<script setup lang="ts">
/**
 * SettingsBackup — 平台自身备份恢复(设置 → 备份)。
 *
 * 区块:
 *  1. 备份策略:定时开关/间隔/保留份数/是否含制品库/是否携带 master key(需口令)/
 *     对象存储自动上传(异地容灾)。
 *  2. 立即备份 + 备份列表:状态轮询(running→completed/failed)、下载、删除、传对象存储。
 *  3. 恢复:上传归档或从对象存储(S3 兼容/OSS)直接拉取 → 服务端校验 → 空闲后自动重启
 *     应用(启动期应用数据);同时展示等价 CLI 命令(pipewright restore)作为离线替代;
 *     master key 归档需输入口令。
 *  4. 待恢复横幅:有 pending 标记时显示,可取消。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useConfirm } from '../../composables/useConfirm'
import {
  getBackupConfig, saveBackupConfig, runBackup, listBackups, backupDownloadUrl,
  deleteBackup, uploadBackupToObjectStore, stageRestore, stageRestoreFromCloud,
  getRestorePending, cancelRestorePending,
  type BackupConfig, type BackupRecord,
} from '../../api/platformBackup'
import { listObjectStores, browseObjectStore, type ObjectStore, type ObjectStoreEntry } from '../../api/objectStores'
import { HttpError } from '../../api/http'
import AppButton from '../../components/ui/AppButton.vue'
import AppBanner from '../../components/ui/AppBanner.vue'
import StatusBadge from '../../components/ui/StatusBadge.vue'

const { t } = useI18n()
const confirm = useConfirm()

// ─── 区块 1:策略 ────────────────────────────────────────────────────────────
const cfg = ref<BackupConfig | null>(null)
const loading = ref(true)
const saving = ref(false)
const savedFlash = ref(false)
const cfgError = ref('')

const providers = ref<ObjectStore[]>([])

const intervalOptions = [
  { label: '6', value: '6' }, { label: '12', value: '12' },
  { label: '24', value: '24' }, { label: '48', value: '48' }, { label: '72', value: '72' },
]

async function loadConfig(): Promise<void> {
  loading.value = true
  cfgError.value = ''
  try {
    const state = await getBackupConfig()
    cfg.value = state.config
    restorePending.value = state.restorePending
  } catch {
    cfgError.value = t('settingsBackup.errLoad')
  } finally {
    loading.value = false
  }
}

async function loadProviders(): Promise<void> {
  try {
    providers.value = (await listObjectStores()) ?? []
  } catch {
    providers.value = []
  }
}

async function saveCfg(): Promise<void> {
  if (!cfg.value) return
  saving.value = true
  savedFlash.value = false
  try {
    await saveBackupConfig(cfg.value)
    savedFlash.value = true
    setTimeout(() => { savedFlash.value = false }, 2500)
  } catch (err) {
    cfgError.value = err instanceof HttpError ? err.message : t('settingsBackup.errSave')
  } finally {
    saving.value = false
  }
}

// ─── 区块 2:备份列表 ────────────────────────────────────────────────────────
const records = ref<BackupRecord[]>([])
const listError = ref('')
const running = ref(false)
let pollTimer: number | undefined

const hasRunning = computed(() => records.value.some((r) => r.status === 'running'))

async function refreshList(): Promise<void> {
  try {
    records.value = await listBackups()
    listError.value = ''
  } catch {
    listError.value = t('settingsBackup.errList')
  }
}

async function runNow(): Promise<void> {
  running.value = true
  try {
    await runBackup({ passphrase: passphrase.value || undefined })
    await refreshList()
  } catch (err) {
    listError.value = err instanceof HttpError ? err.message : t('settingsBackup.errRun')
  } finally {
    running.value = false
  }
}

async function removeRecord(rec: BackupRecord): Promise<void> {
  const ok = await confirm.open({
    title: t('settingsBackup.deleteTitle'),
    body: t('settingsBackup.deleteBody', { name: rec.fileName }),
    confirmLabel: t('settingsBackup.deleteConfirm'),
  })
  if (!ok) return
  try {
    await deleteBackup(rec.id)
    await refreshList()
  } catch (err) {
    listError.value = err instanceof HttpError ? err.message : t('settingsBackup.errDelete')
  }
}

async function uploadToCloud(rec: BackupRecord): Promise<void> {
  const providerId = cfg.value?.objectstoreProviderId ?? ''
  if (!providerId) {
    listError.value = t('settingsBackup.errNoProvider')
    return
  }
  try {
    await uploadBackupToObjectStore(rec.id, providerId)
    await refreshList()
  } catch (err) {
    listError.value = err instanceof HttpError ? err.message : t('settingsBackup.errUpload')
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

// ─── 区块 3:恢复 ────────────────────────────────────────────────────────────
const passphrase = ref('')
const restoreFile = ref<File | null>(null)
const restoreInput = ref<HTMLInputElement | null>(null)
const restoring = ref(false)
const restoreMsg = ref('')
const restoreError = ref('')
const restorePending = ref(false)

function pickFile(e: Event): void {
  const input = e.target as HTMLInputElement
  restoreFile.value = input.files?.[0] ?? null
  restoreError.value = ''
}

async function doRestore(): Promise<void> {
  if (!restoreFile.value) return
  const ok = await confirm.open({
    title: t('settingsBackup.restoreTitle'),
    body: t('settingsBackup.restoreBody'),
    confirmLabel: t('settingsBackup.restoreConfirm'),
  })
  if (!ok) return
  restoring.value = true
  restoreError.value = ''
  restoreMsg.value = ''
  try {
    const res = await stageRestore(restoreFile.value, { passphrase: passphrase.value || undefined })
    restoreMsg.value = res.message
    restorePending.value = true
    restoreFile.value = null
    if (restoreInput.value) restoreInput.value.value = ''
  } catch (err) {
    restoreError.value = err instanceof HttpError ? err.message : t('settingsBackup.errRestore')
  } finally {
    restoring.value = false
  }
}

async function cancelPending(): Promise<void> {
  try {
    await cancelRestorePending()
    restorePending.value = false
    restoreMsg.value = ''
  } catch {
    /* best-effort */
  }
}

// ─── 区块 3b:从对象存储恢复 ──────────────────────────────────────────────────
const cloudProvider = ref('')
const cloudObjects = ref<ObjectStoreEntry[]>([])
const cloudSel = ref('')
const cloudLoading = ref(false)
const cloudError = ref('')

async function loadCloudObjects(): Promise<void> {
  if (!cloudProvider.value) return
  cloudLoading.value = true
  cloudError.value = ''
  cloudSel.value = ''
  try {
    // 平台备份自动上传的对象键前缀为 platform-backups/(相对配置 PathPrefix)。
    const res = await browseObjectStore(cloudProvider.value, { prefix: 'platform-backups/', maxKeys: 200 })
    cloudObjects.value = (res.objects ?? []).filter((o) => o.key.endsWith('.pwb'))
    if (cloudObjects.value.length === 0) cloudError.value = t('settingsBackup.cloudEmpty')
  } catch (err) {
    cloudError.value = err instanceof HttpError ? err.message : t('settingsBackup.errCloudList')
  } finally {
    cloudLoading.value = false
  }
}

async function doRestoreFromCloud(): Promise<void> {
  if (!cloudSel.value) return
  const ok = await confirm.open({
    title: t('settingsBackup.restoreTitle'),
    body: t('settingsBackup.restoreBody'),
    confirmLabel: t('settingsBackup.restoreConfirm'),
  })
  if (!ok) return
  restoring.value = true
  restoreError.value = ''
  restoreMsg.value = ''
  try {
    const res = await stageRestoreFromCloud(cloudProvider.value, cloudSel.value, {
      passphrase: passphrase.value || undefined,
    })
    restoreMsg.value = res.message
    restorePending.value = true
  } catch (err) {
    restoreError.value = err instanceof HttpError ? err.message : t('settingsBackup.errRestore')
  } finally {
    restoring.value = false
  }
}

// ─── 生命周期 ────────────────────────────────────────────────────────────────
onMounted(async () => {
  await loadConfig()
  await loadProviders()
  await refreshList()
  // 有进行中的备份 → 2s 轮询直到终态;页面隐藏时也继续(操作在后台跑)。
  pollTimer = window.setInterval(() => {
    if (hasRunning.value) void refreshList()
  }, 2000)
})

onBeforeUnmount(() => {
  if (pollTimer !== undefined) window.clearInterval(pollTimer)
})
</script>

<template>
  <section class="sb" aria-labelledby="sb-heading">
    <header class="sb-head">
      <div>
        <h2 id="sb-heading" class="sb-title">{{ t('settingsBackup.title') }}</h2>
        <p class="sb-sub">{{ t('settingsBackup.subtitle') }}</p>
      </div>
    </header>

    <!-- 待恢复横幅 -->
    <AppBanner v-if="restorePending" variant="warn" :title="t('settingsBackup.pendingTitle')">
      {{ restoreMsg || t('settingsBackup.pendingBody') }}
      <template #action>
        <AppButton variant="ghost" @click="cancelPending">{{ t('settingsBackup.pendingCancel') }}</AppButton>
      </template>
    </AppBanner>

    <!-- loading / error -->
    <div v-if="loading" class="sb-state" aria-busy="true">{{ t('settingsBackup.loading') }}</div>
    <div v-else-if="cfgError && !cfg" class="sb-state sb-state--error" role="alert">{{ cfgError }}</div>

    <template v-else>
      <!-- ─── 区块 1:策略 ─── -->
      <div class="sb-card">
        <h3 class="sb-card-title">{{ t('settingsBackup.cfgTitle') }}</h3>
        <div class="sb-grid">
          <label class="sb-check">
            <input v-model="cfg!.enabled" type="checkbox" />
            <span>{{ t('settingsBackup.cfgEnabled') }}</span>
          </label>
          <label class="sb-field">
            <span class="sb-field-label">{{ t('settingsBackup.cfgInterval') }}</span>
            <select v-model.number="cfg!.intervalHours" :disabled="!cfg!.enabled" class="sb-input">
              <option v-for="o in intervalOptions" :key="o.value" :value="Number(o.value)">{{ o.label }}</option>
            </select>
            <span class="sb-hint">{{ t('settingsBackup.cfgIntervalUnit') }}</span>
          </label>
          <label class="sb-field">
            <span class="sb-field-label">{{ t('settingsBackup.cfgRetention') }}</span>
            <input v-model.number="cfg!.retentionCount" type="number" min="0" max="365" class="sb-input" />
            <span class="sb-hint">{{ t('settingsBackup.cfgRetentionUnit') }}</span>
          </label>
          <label class="sb-check">
            <input v-model="cfg!.includeArtifacts" type="checkbox" />
            <span>{{ t('settingsBackup.cfgArtifacts') }}</span>
          </label>
          <label class="sb-check" :class="{ 'sb-check--disabled': !cfg!.masterKeyConfigured }">
            <input v-model="cfg!.includeMasterKey" type="checkbox" :disabled="!cfg!.masterKeyConfigured" />
            <span>{{ t('settingsBackup.cfgMasterKey') }}</span>
          </label>
          <label v-if="cfg!.includeMasterKey" class="sb-field sb-field--wide">
            <span class="sb-field-label">{{ t('settingsBackup.cfgPassphrase') }}</span>
            <input v-model="passphrase" type="password" autocomplete="new-password" class="sb-input"
              :placeholder="t('settingsBackup.cfgPassphraseHint')" />
          </label>
          <label class="sb-field sb-field--wide">
            <span class="sb-field-label">{{ t('settingsBackup.cfgProvider') }}</span>
            <select v-model="cfg!.objectstoreProviderId" class="sb-input">
              <option value="">{{ t('settingsBackup.cfgProviderNone') }}</option>
              <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.name }}（{{ p.type }}）</option>
            </select>
            <span class="sb-hint">{{ t('settingsBackup.cfgProviderHint') }}</span>
          </label>
        </div>
        <div class="sb-actions">
          <AppButton variant="primary" :loading="saving" @click="saveCfg">{{ t('settingsBackup.save') }}</AppButton>
          <span v-if="savedFlash" class="sb-flash">{{ t('settingsBackup.saved') }}</span>
          <span v-else-if="cfgError" class="sb-error">{{ cfgError }}</span>
        </div>
      </div>

      <!-- ─── 区块 2:立即备份 + 列表 ─── -->
      <div class="sb-card">
        <div class="sb-card-row">
          <h3 class="sb-card-title">{{ t('settingsBackup.listTitle') }}</h3>
          <div class="sb-actions">
            <AppButton variant="default" @click="refreshList">{{ t('common.refresh') }}</AppButton>
            <AppButton variant="primary" :loading="running || hasRunning" @click="runNow">
              {{ t('settingsBackup.runNow') }}
            </AppButton>
          </div>
        </div>
        <p v-if="listError" class="sb-error" role="alert">{{ listError }}</p>
        <table v-if="records.length" class="sb-table">
          <thead>
            <tr>
              <th>{{ t('settingsBackup.colFile') }}</th>
              <th>{{ t('settingsBackup.colSize') }}</th>
              <th>{{ t('settingsBackup.colKind') }}</th>
              <th>{{ t('settingsBackup.colStatus') }}</th>
              <th>{{ t('settingsBackup.colCreatedAt') }}</th>
              <th class="sb-table-ops">{{ t('settingsBackup.colOps') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in records" :key="r.id">
              <td class="sb-mono" :title="r.sha256">{{ r.fileName }}</td>
              <td>{{ formatSize(r.sizeBytes) }}</td>
              <td>{{ r.kind === 'scheduled' ? t('settingsBackup.kindScheduled') : t('settingsBackup.kindManual') }}</td>
              <td>
                <StatusBadge :status="r.status === 'completed' ? 'success' : r.status === 'failed' ? 'failed' : 'running'" />
                <span v-if="r.status === 'failed' && r.error" class="sb-err-text" :title="r.error">{{ r.error.slice(0, 60) }}</span>
                <span v-if="r.objectKey" class="sb-cloud" :title="r.objectKey">☁</span>
              </td>
              <td>{{ formatTime(r.createdAt) }}</td>
              <td class="sb-table-ops">
                <a v-if="r.status === 'completed'" class="sb-link" :href="backupDownloadUrl(r.id)">{{ t('settingsBackup.opDownload') }}</a>
                <button v-if="r.status === 'completed' && cfg!.objectstoreProviderId" class="sb-link" @click="uploadToCloud(r)">
                  {{ t('settingsBackup.opUpload') }}
                </button>
                <button class="sb-link sb-link--danger" @click="removeRecord(r)">{{ t('settingsBackup.opDelete') }}</button>
              </td>
            </tr>
          </tbody>
        </table>
        <p v-else class="sb-empty">{{ t('settingsBackup.empty') }}</p>
      </div>

      <!-- ─── 区块 3:恢复 ─── -->
      <div class="sb-card">
        <h3 class="sb-card-title">{{ t('settingsBackup.restoreCardTitle') }}</h3>
        <p class="sb-notice">{{ t('settingsBackup.restoreNotice') }}</p>
        <div class="sb-restore">
          <input ref="restoreInput" type="file" accept=".pwb" class="sb-file" :aria-label="t('settingsBackup.pickFile')" @change="pickFile" />
          <input v-model="passphrase" type="password" autocomplete="off" class="sb-input sb-restore-pass"
            :placeholder="t('settingsBackup.cfgPassphraseHint')" :aria-label="t('settingsBackup.cfgPassphrase')" />
          <AppButton variant="danger" :disabled="!restoreFile || restoring" :loading="restoring" @click="doRestore">
            {{ t('settingsBackup.restoreGo') }}
          </AppButton>
        </div>
        <!-- 从对象存储恢复:选配置 → 列出 platform-backups/ 下的 .pwb → 恢复 -->
        <div v-if="providers.length" class="sb-restore">
          <select v-model="cloudProvider" class="sb-input" :aria-label="t('settingsBackup.cloudProvider')"
            @change="cloudObjects = []; cloudSel = ''; cloudError = ''">
            <option value="">{{ t('settingsBackup.cloudProvider') }}</option>
            <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.name }}（{{ p.type }}）</option>
          </select>
          <AppButton :disabled="!cloudProvider" :loading="cloudLoading" @click="loadCloudObjects">
            {{ t('settingsBackup.cloudList') }}
          </AppButton>
          <select v-if="cloudObjects.length" v-model="cloudSel" class="sb-input sb-cloud-sel"
            :aria-label="t('settingsBackup.cloudObject')">
            <option v-for="o in cloudObjects" :key="o.key" :value="o.key">{{ o.key.slice(o.key.lastIndexOf('/') + 1) }}</option>
          </select>
          <AppButton v-if="cloudObjects.length" variant="danger" :disabled="!cloudSel || restoring" @click="doRestoreFromCloud">
            {{ t('settingsBackup.cloudGo') }}
          </AppButton>
        </div>
        <p v-if="cloudError" class="sb-error" role="alert">{{ cloudError }}</p>
        <p v-if="restoreError" class="sb-error" role="alert">{{ restoreError }}</p>
          <p v-if="restoreMsg" class="sb-ok">{{ restoreMsg }}</p>
        <details class="sb-cli">
          <summary>{{ t('settingsBackup.cliSummary') }}</summary>
          <pre class="sb-code">pipewright restore /path/to/pw-backup-xxx.pwb [--passphrase-file &lt;file&gt;]</pre>
          <p class="sb-hint">{{ t('settingsBackup.cliHint') }}</p>
        </details>
      </div>
    </template>
  </section>
</template>

<style scoped>
.sb { display: flex; flex-direction: column; gap: 18px; }
.sb-head { display: flex; align-items: flex-end; justify-content: space-between; gap: 12px; }
.sb-title { margin: 0; font-size: 17px; font-weight: 650; }
.sb-sub { margin: 4px 0 0; font-size: 12.5px; opacity: 0.72; }
.sb-card { border: 1px solid var(--border, #e2e2e2); border-radius: 10px; padding: 16px; display: flex; flex-direction: column; gap: 12px; background: var(--bg-card, #fff); }
.sb-card-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.sb-card-title { margin: 0; font-size: 14px; font-weight: 650; }
.sb-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 12px 18px; }
.sb-field { display: flex; flex-direction: column; gap: 4px; font-size: 12.5px; }
.sb-field--wide { grid-column: 1 / -1; }
.sb-field-label { font-weight: 600; }
.sb-input { padding: 6px 9px; border: 1px solid var(--border, #ccc); border-radius: 6px; font-size: 13px; background: var(--bg-input, #fff); color: inherit; max-width: 320px; }
.sb-check { display: flex; align-items: center; gap: 8px; font-size: 13px; padding-top: 18px; }
.sb-check--disabled { opacity: 0.5; }
.sb-hint { font-size: 11.5px; opacity: 0.65; }
.sb-actions { display: flex; align-items: center; gap: 12px; }
.sb-flash { color: #1a7f37; font-size: 12.5px; }
.sb-error { color: #c62828; font-size: 12.5px; }
.sb-ok { color: #1a7f37; font-size: 12.5px; white-space: pre-line; }
.sb-state { padding: 24px; text-align: center; opacity: 0.7; font-size: 13px; }
.sb-state--error { color: #c62828; }
.sb-table { width: 100%; border-collapse: collapse; font-size: 12.5px; }
.sb-table th, .sb-table td { text-align: left; padding: 7px 8px; border-bottom: 1px solid var(--border, #eee); vertical-align: middle; }
.sb-table th { font-weight: 600; opacity: 0.75; }
.sb-table-ops { white-space: nowrap; }
.sb-mono { font-family: ui-monospace, monospace; }
.sb-err-text { display: block; font-size: 11px; color: #c62828; max-width: 260px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sb-cloud { margin-left: 6px; }
.sb-link { background: none; border: none; padding: 0; color: var(--accent, #3b82f6); cursor: pointer; font-size: 12.5px; text-decoration: none; margin-right: 10px; }
.sb-link--danger { color: #c62828; }
.sb-empty { margin: 0; opacity: 0.6; font-size: 12.5px; }
.sb-notice { margin: 0; font-size: 12.5px; opacity: 0.78; }
.sb-restore { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
.sb-restore-pass { max-width: 260px; }
.sb-cloud-sel { max-width: 340px; }
.sb-file { font-size: 12.5px; }
.sb-cli summary { cursor: pointer; font-size: 12.5px; opacity: 0.8; }
.sb-code { margin: 8px 0 4px; padding: 10px; background: var(--bg-subtle, #f6f6f6); border-radius: 6px; font-size: 12px; overflow-x: auto; }
</style>
