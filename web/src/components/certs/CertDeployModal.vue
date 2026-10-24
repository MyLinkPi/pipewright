<script setup lang="ts">
/*
  CertDeployModal.vue — 证书部署目标管理(证书管理页内嵌模态)。

  把一张证书部署到远程机器的指定路径(证书 644 / 私钥 600,先临时文件再原子替换),
  部署成功后可执行命令(如 nginx -s reload);证书签发 / 导入 / 续期后自动联动重新部署。
  视图三态:目标列表 ⇄ 新增/编辑表单 ⇄ 部署历史。
*/
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { Server as ServerIcon, Refresh, Plus, Trash, Rotate, History, Upload, ArrowLeft } from '@vicons/tabler'
import {
  listCertDeploys,
  createCertDeploy,
  updateCertDeploy,
  deleteCertDeploy,
  deployCertDeployNow,
  listCertDeployRuns,
  type Cert,
  type CertDeploy,
  type CertDeployRun,
} from '../../api/certMgmt'
import { listServers, type Server } from '../../api/servers'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'
import { useConfirm } from '../../composables/useConfirm'
import EmptyState from '../ui/EmptyState.vue'
import ErrorState from '../ui/ErrorState.vue'
import AppButton from '../ui/AppButton.vue'
import AppSelect from '../ui/AppSelect.vue'

const props = defineProps<{ cert: Cert }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

type View = 'list' | 'form' | 'history'
const view = ref<View>('list')
const deploys = ref<CertDeploy[]>([])
const servers = ref<Server[]>([])
const loadError = ref('')
const loading = ref(false)

function errMsg(err: unknown, fallback: string): string {
  return err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback
}

async function load(): Promise<void> {
  loading.value = true
  loadError.value = ''
  try {
    const res = await listCertDeploys(props.cert.id)
    deploys.value = res.items
  } catch (err) {
    loadError.value = errMsg(err, t('certMgmt.deploy.loadFail'))
  } finally {
    loading.value = false
  }
}

async function loadServerList(): Promise<void> {
  try {
    servers.value = await listServers()
  } catch {
    servers.value = []
  }
}

onMounted(() => {
  void load()
  void loadServerList()
})

const serverOptions = computed(() =>
  servers.value.map((s) => ({ value: s.id, label: `${s.name}(${s.user}@${s.host})` })),
)

function serverLabel(d: CertDeploy): string {
  const s = servers.value.find((x) => x.id === d.serverId)
  return s ? s.name : d.serverName || d.serverId
}

// ─── 状态徽章 / 时间 ──────────────────────────────────────────────────────────
function statusText(s: CertDeploy['status']): string {
  if (s === 'success') return t('certMgmt.deploy.statusSuccess')
  if (s === 'failed') return t('certMgmt.deploy.statusFailed')
  if (s === 'running') return t('certMgmt.deploy.statusRunning')
  return t('certMgmt.deploy.statusNever')
}
function fmtTime(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  return Number.isNaN(d.getTime()) ? '—' : d.toISOString().replace('T', ' ').slice(0, 19)
}

// ─── 新增 / 编辑表单 ──────────────────────────────────────────────────────────
interface FormState {
  id: string // 空 = 新增
  name: string
  serverId: string
  certPath: string
  keyPath: string
  postCommand: string
  enabled: boolean
}
const form = ref<FormState | null>(null)
const saving = ref(false)

function openCreate(): void {
  form.value = {
    id: '',
    name: '',
    serverId: servers.value[0]?.id ?? '',
    certPath: '',
    keyPath: '',
    postCommand: '',
    enabled: true,
  }
  if (servers.value.length === 0) void loadServerList()
  view.value = 'form'
}

function openEdit(d: CertDeploy): void {
  form.value = {
    id: d.id,
    name: d.name,
    serverId: d.serverId,
    certPath: d.certPath,
    keyPath: d.keyPath,
    postCommand: d.postCommand,
    enabled: d.enabled,
  }
  view.value = 'form'
}

const formValid = computed(() => {
  const f = form.value
  return !!f && f.serverId && f.certPath.trim().startsWith('/') && f.keyPath.trim().startsWith('/') && f.certPath.trim() !== f.keyPath.trim()
})

async function submitForm(): Promise<void> {
  const f = form.value
  if (!f) return
  saving.value = true
  try {
    if (f.id) {
      await updateCertDeploy(f.id, {
        name: f.name,
        certPath: f.certPath.trim(),
        keyPath: f.keyPath.trim(),
        postCommand: f.postCommand,
        enabled: f.enabled,
      })
      toast.success(t('certMgmt.deploy.updated'))
    } else {
      await createCertDeploy({
        certId: props.cert.id,
        serverId: f.serverId,
        name: f.name,
        certPath: f.certPath.trim(),
        keyPath: f.keyPath.trim(),
        postCommand: f.postCommand,
        enabled: f.enabled,
      })
      toast.success(t('certMgmt.deploy.created'))
    }
    view.value = 'list'
    await load()
  } catch (err) {
    toast.error(errMsg(err, f.id ? t('certMgmt.deploy.updateFail') : t('certMgmt.deploy.createFail')))
  } finally {
    saving.value = false
  }
}

// ─── 行操作:立即部署 / 启用开关 / 删除 ────────────────────────────────────────
const busyIds = ref<Set<string>>(new Set())
function isBusy(id: string): boolean {
  return busyIds.value.has(id)
}
function markBusy(id: string, on: boolean): void {
  const next = new Set(busyIds.value)
  if (on) next.add(id)
  else next.delete(id)
  busyIds.value = next
}

function replaceRow(d: CertDeploy): void {
  deploys.value = deploys.value.map((x) => (x.id === d.id ? d : x))
}

async function deployNow(d: CertDeploy): Promise<void> {
  if (isBusy(d.id)) return
  markBusy(d.id, true)
  try {
    const res = await deployCertDeployNow(d.id)
    replaceRow(res)
    if (res.status === 'success') toast.success(t('certMgmt.deploy.deployed'), { detail: serverLabel(d) })
    else toast.error(t('certMgmt.deploy.deployFail'), { detail: res.statusDetail })
  } catch (err) {
    toast.error(errMsg(err, t('certMgmt.deploy.deployFail')))
  } finally {
    markBusy(d.id, false)
  }
}

async function toggleEnabled(d: CertDeploy): Promise<void> {
  if (isBusy(d.id)) return
  markBusy(d.id, true)
  try {
    replaceRow(await updateCertDeploy(d.id, { enabled: !d.enabled }))
  } catch (err) {
    toast.error(errMsg(err, t('certMgmt.deploy.updateFail')))
  } finally {
    markBusy(d.id, false)
  }
}

async function removeRow(d: CertDeploy): Promise<void> {
  const ok = await confirm.open({
    title: t('certMgmt.deploy.delTitle'),
    body: t('certMgmt.deploy.delBody', { server: serverLabel(d), path: d.certPath }),
    confirmLabel: t('certMgmt.deploy.del'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteCertDeploy(d.id)
    deploys.value = deploys.value.filter((x) => x.id !== d.id)
    toast.success(t('certMgmt.deploy.deleted'))
  } catch (err) {
    toast.error(errMsg(err, t('certMgmt.deploy.deleteFail')))
  }
}

// ─── 部署历史 ─────────────────────────────────────────────────────────────────
const historyFor = ref<CertDeploy | null>(null)
const runs = ref<CertDeployRun[]>([])
const runsLoading = ref(false)

async function openHistory(d: CertDeploy): Promise<void> {
  historyFor.value = d
  view.value = 'history'
  runsLoading.value = true
  try {
    runs.value = (await listCertDeployRuns(d.id)).items
  } catch {
    runs.value = []
  } finally {
    runsLoading.value = false
  }
}
</script>

<template>
  <div class="scrim" @click.self="emit('close')">
    <div class="modal modal--wide" role="dialog" :aria-label="t('certMgmt.deploy.title')">
      <header class="modal__head">
        <h3 class="modal__title">
          <span v-if="view === 'form'" class="modal__back" :aria-label="t('certMgmt.deploy.back')" @click="view = 'list'">
            <NIcon :size="15"><ArrowLeft /></NIcon>
          </span>
          <span v-else-if="view === 'history'" class="modal__back" :aria-label="t('certMgmt.deploy.back')" @click="view = 'list'">
            <NIcon :size="15"><ArrowLeft /></NIcon>
          </span>
          {{ view === 'form'
            ? (form?.id ? t('certMgmt.deploy.editTitle') : t('certMgmt.deploy.createTitle'))
            : view === 'history' ? t('certMgmt.deploy.historyTitle') : t('certMgmt.deploy.title') }}
          <span class="modal__dom mono">{{ cert.primaryDomain }}</span>
        </h3>
        <button class="modal__close" :aria-label="t('certMgmt.close')" @click="emit('close')">✕</button>
      </header>

      <!-- 目标列表 -->
      <div v-if="view === 'list'" class="modal__body">
        <p class="modal__hint">{{ t('certMgmt.deploy.subtitle') }}</p>
        <ErrorState v-if="loadError" :title="t('certMgmt.deploy.loadFail')" :description="loadError" @retry="load" />
        <template v-else>
          <EmptyState
            v-if="deploys.length === 0 && !loading"
            :title="t('certMgmt.deploy.empty')"
            :description="t('certMgmt.deploy.subtitle')"
          />
          <div v-else class="dtablewrap">
            <table class="dtbl">
              <thead>
                <tr>
                  <th>{{ t('certMgmt.deploy.colServer') }}</th>
                  <th>{{ t('certMgmt.deploy.colPaths') }}</th>
                  <th>{{ t('certMgmt.deploy.colStatus') }}</th>
                  <th>{{ t('certMgmt.deploy.colLast') }}</th>
                  <th>{{ t('certMgmt.deploy.colEnabled') }}</th>
                  <th class="dtbl__act-h">{{ t('certMgmt.deploy.colActions') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="d in deploys" :key="d.id" class="datarow" :class="{ 'datarow--busy': isBusy(d.id) }">
                  <td>
                    <div class="dname">
                      <NIcon :size="13" class="dname__ic"><ServerIcon /></NIcon>
                      {{ serverLabel(d) }}
                    </div>
                    <div v-if="d.name" class="dsub">{{ d.name }}</div>
                    <div v-if="d.postCommand" class="dsub dsub--cmd mono" :title="d.postCommand">$ {{ d.postCommand }}</div>
                  </td>
                  <td class="dpaths">
                    <div class="mono dpath" :title="d.certPath">{{ d.certPath }}</div>
                    <div class="mono dpath dpath--key" :title="d.keyPath">{{ d.keyPath }}</div>
                  </td>
                  <td>
                    <span class="badge" :class="d.status ? `badge--${d.status}` : 'badge--never'">{{ statusText(d.status) }}</span>
                    <div v-if="d.statusDetail" class="derr" :title="d.statusDetail">{{ d.statusDetail }}</div>
                  </td>
                  <td class="mono dlast">{{ fmtTime(d.lastDeployedAt) }}</td>
                  <td>
                    <button
                      class="autobtn"
                      :class="{ 'autobtn--on': d.enabled }"
                      :disabled="isBusy(d.id)"
                      :title="t('certMgmt.deploy.enabledTitle')"
                      @click="toggleEnabled(d)"
                    >
                      {{ d.enabled ? t('certMgmt.autoOn') : t('certMgmt.autoOff') }}
                    </button>
                  </td>
                  <td class="dtbl__act">
                    <button class="rowbtn" :disabled="isBusy(d.id)" :title="t('certMgmt.deploy.deployNowTitle')" @click="deployNow(d)">
                      <NIcon :size="14"><Upload /></NIcon>
                    </button>
                    <button class="rowbtn" :disabled="isBusy(d.id)" :title="t('certMgmt.deploy.history')" @click="openHistory(d)">
                      <NIcon :size="14"><History /></NIcon>
                    </button>
                    <button class="rowbtn" :disabled="isBusy(d.id)" :title="t('certMgmt.deploy.edit')" @click="openEdit(d)">
                      <NIcon :size="14"><Rotate /></NIcon>
                    </button>
                    <button class="rowbtn rowbtn--danger" :disabled="isBusy(d.id)" :title="t('certMgmt.deploy.del')" @click="removeRow(d)">
                      <NIcon :size="14"><Trash /></NIcon>
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </template>
      </div>
      <footer v-if="view === 'list'" class="modal__foot">
        <AppButton variant="default" :loading="loading" @click="load">
          <NIcon :size="14"><Refresh /></NIcon>&nbsp;{{ t('common.refresh') }}
        </AppButton>
        <AppButton variant="primary" @click="openCreate">
          <NIcon :size="14"><Plus /></NIcon>&nbsp;{{ t('certMgmt.deploy.add') }}
        </AppButton>
      </footer>

      <!-- 新增 / 编辑表单 -->
      <div v-else-if="view === 'form' && form" class="modal__body">
        <label v-if="!form.id" class="field">
          <span class="field__lbl">{{ t('certMgmt.deploy.server') }}</span>
          <AppSelect v-model="form.serverId" :options="serverOptions" />
          <span v-if="servers.length === 0" class="field__hint">{{ t('certMgmt.deploy.noServers') }}</span>
        </label>
        <label class="field">
          <span class="field__lbl">{{ t('certMgmt.deploy.name') }}</span>
          <input v-model="form.name" class="field__in" type="text" :placeholder="t('certMgmt.deploy.namePh')" />
        </label>
        <label class="field">
          <span class="field__lbl">{{ t('certMgmt.deploy.certPath') }}</span>
          <input v-model="form.certPath" class="field__in mono" type="text" :placeholder="t('certMgmt.deploy.certPathPh')" />
        </label>
        <label class="field">
          <span class="field__lbl">{{ t('certMgmt.deploy.keyPath') }}</span>
          <input v-model="form.keyPath" class="field__in mono" type="text" :placeholder="t('certMgmt.deploy.keyPathPh')" />
        </label>
        <label class="field">
          <span class="field__lbl">{{ t('certMgmt.deploy.postCommand') }}</span>
          <input v-model="form.postCommand" class="field__in mono" type="text" placeholder="nginx -s reload" />
        </label>
        <label class="check">
          <input v-model="form.enabled" type="checkbox" />
          <span>{{ t('certMgmt.deploy.enabled') }}</span>
        </label>
        <p class="modal__hint">{{ t('certMgmt.deploy.hint') }}</p>
      </div>
      <footer v-if="view === 'form' && form" class="modal__foot">
        <AppButton variant="default" :disabled="saving" @click="view = 'list'">{{ t('certMgmt.cancel') }}</AppButton>
        <AppButton variant="primary" :loading="saving" :disabled="!formValid" @click="submitForm">
          {{ saving ? t('certMgmt.deploy.submitting') : t('certMgmt.deploy.submit') }}
        </AppButton>
      </footer>

      <!-- 部署历史 -->
      <div v-else-if="view === 'history' && historyFor" class="modal__body">
        <EmptyState v-if="runs.length === 0 && !runsLoading" :title="t('certMgmt.deploy.historyEmpty')" />
        <div v-else class="runs">
          <div v-for="r in runs" :key="r.id" class="run" :class="`run--${r.status}`">
            <div class="run__head">
              <span class="badge" :class="`badge--${r.status}`">
                {{ r.status === 'success' ? t('certMgmt.deploy.statusSuccess') : t('certMgmt.deploy.statusFailed') }}
              </span>
              <span class="tag">{{ r.source === 'renew' ? t('certMgmt.deploy.sourceRenew') : t('certMgmt.deploy.sourceManual') }}</span>
              <span class="run__time mono">{{ fmtTime(r.startedAt) }}</span>
              <span class="run__dur mono">{{ r.durationMs >= 0 ? `${(r.durationMs / 1000).toFixed(1)}s` : '—' }}</span>
            </div>
            <pre v-if="r.detail" class="run__detail">{{ r.detail }}</pre>
          </div>
        </div>
      </div>
      <footer v-if="view === 'history'" class="modal__foot">
        <AppButton variant="default" @click="view = 'list'">{{ t('certMgmt.deploy.back') }}</AppButton>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.scrim {
  position: fixed;
  inset: 0;
  z-index: 60;
  display: grid;
  place-items: center;
  padding: 20px;
  background: color-mix(in srgb, #000 45%, transparent);
}
.modal {
  width: min(760px, 100%);
  max-height: min(86vh, 720px);
  display: flex;
  flex-direction: column;
  border-radius: var(--rounded-card);
  border: 1px solid var(--color-border);
  background: var(--color-card);
  box-shadow: var(--shadow-lg, var(--shadow));
  overflow: hidden;
}
.modal--wide {
  width: min(880px, 100%);
}
.modal__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 14px 18px;
  border-bottom: 1px solid var(--color-border);
  background: var(--color-card-2);
}
.modal__title {
  margin: 0;
  font-size: var(--text-label);
  font-weight: 700;
  color: var(--color-text);
  display: inline-flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.modal__dom {
  font-weight: 400;
  font-size: var(--text-micro);
  color: var(--color-dim);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.modal__back {
  display: inline-flex;
  align-items: center;
  cursor: pointer;
  color: var(--color-dim);
  padding: 2px 4px;
  border-radius: 6px;
}
.modal__back:hover {
  color: var(--color-text);
  background: var(--color-inset);
}
.modal__close {
  border: none;
  background: transparent;
  color: var(--color-dim);
  font-size: 14px;
  cursor: pointer;
  padding: 2px 6px;
  border-radius: 6px;
  flex-shrink: 0;
}
.modal__close:hover {
  color: var(--color-text);
  background: var(--color-inset);
}
.modal__body {
  padding: 16px 18px;
  display: flex;
  flex-direction: column;
  gap: 12px;
  overflow-y: auto;
}
.modal__foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  padding: 12px 18px;
  border-top: 1px solid var(--color-border);
  background: var(--color-card-2);
}
.modal__hint {
  margin: 0;
  font-size: var(--text-micro);
  color: var(--color-faint);
  line-height: 1.5;
}
.field {
  display: flex;
  flex-direction: column;
  gap: 5px;
}
.field__lbl {
  font-size: var(--text-micro);
  font-weight: 600;
  color: var(--color-dim);
}
.field__in {
  font: inherit;
  font-size: var(--text-label);
  padding: 8px 10px;
  border-radius: var(--rounded-md);
  border: 1px solid var(--color-border-strong);
  background: var(--color-inset);
  color: var(--color-text);
  width: 100%;
  box-sizing: border-box;
}
.field__in:focus {
  outline: none;
  border-color: var(--color-primary);
}
.field__hint {
  font-size: var(--text-micro);
  color: var(--color-amber);
}
.check {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: var(--text-label);
  color: var(--color-dim);
  cursor: pointer;
}
.mono {
  font-family: var(--font-mono, monospace);
}

/* 目标表 */
.dtablewrap {
  overflow-x: auto;
}
.dtbl {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-label);
}
.dtbl thead th {
  text-align: left;
  font-size: var(--text-micro);
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--color-faint);
  padding: 6px 10px;
  border-bottom: 1px solid var(--color-border);
  white-space: nowrap;
}
.dtbl td {
  padding: 9px 10px;
  border-bottom: 1px solid var(--color-border);
  vertical-align: top;
}
.dtbl__act {
  white-space: nowrap;
}
.dtbl__act-h {
  text-align: right;
}
.datarow--busy {
  opacity: 0.55;
}
.dname {
  display: flex;
  align-items: center;
  gap: 6px;
  font-weight: 600;
  color: var(--color-text);
}
.dname__ic {
  color: var(--color-dim);
}
.dsub {
  font-size: var(--text-micro);
  color: var(--color-dim);
  margin-top: 2px;
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.dsub--cmd {
  color: var(--color-faint);
}
.dpaths {
  max-width: 260px;
}
.dpath {
  font-size: var(--text-micro);
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.dpath--key {
  color: var(--color-dim);
}
.dlast {
  font-size: var(--text-micro);
  color: var(--color-dim);
  white-space: nowrap;
}
.derr {
  font-size: var(--text-micro);
  color: var(--color-danger, var(--color-amber));
  margin-top: 3px;
  max-width: 200px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* 徽章 */
.badge {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 2px 9px;
  border-radius: 999px;
  font-size: var(--text-micro);
  font-weight: 600;
  white-space: nowrap;
}
.badge--success {
  background: color-mix(in srgb, var(--color-ok, #18a058) 14%, transparent);
  color: var(--color-ok, #18a058);
}
.badge--failed {
  background: color-mix(in srgb, var(--color-danger, #d03050) 14%, transparent);
  color: var(--color-danger, #d03050);
}
.badge--running {
  background: color-mix(in srgb, var(--color-primary) 14%, transparent);
  color: var(--color-primary);
}
.badge--never {
  background: var(--color-inset);
  color: var(--color-dim);
}
.tag {
  display: inline-flex;
  align-items: center;
  padding: 1px 8px;
  border-radius: 999px;
  font-size: var(--text-micro);
  font-weight: 600;
  background: var(--color-inset);
  color: var(--color-dim);
}

/* 行按钮 / 启用按钮 */
.rowbtn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-md);
  background: var(--color-card);
  color: var(--color-dim);
  cursor: pointer;
}
.rowbtn:hover:not(:disabled) {
  color: var(--color-text);
  border-color: var(--color-border-strong);
}
.rowbtn--danger:hover:not(:disabled) {
  color: var(--color-danger, #d03050);
  border-color: color-mix(in srgb, var(--color-danger, #d03050) 45%, var(--color-border));
}
.rowbtn:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.autobtn {
  border: 1px solid var(--color-border);
  border-radius: 999px;
  background: var(--color-card);
  color: var(--color-dim);
  font: inherit;
  font-size: var(--text-micro);
  font-weight: 600;
  padding: 3px 10px;
  cursor: pointer;
  white-space: nowrap;
}
.autobtn--on {
  background: color-mix(in srgb, var(--color-ok, #18a058) 14%, transparent);
  border-color: color-mix(in srgb, var(--color-ok, #18a058) 40%, var(--color-border));
  color: var(--color-ok, #18a058);
}
.autobtn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

/* 历史 */
.runs {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.run {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-md);
  padding: 10px 12px;
  background: var(--color-inset);
}
.run__head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.run__time {
  font-size: var(--text-micro);
  color: var(--color-text);
}
.run__dur {
  font-size: var(--text-micro);
  color: var(--color-faint);
  margin-left: auto;
}
.run__detail {
  margin: 8px 0 0;
  padding: 8px 10px;
  border-radius: 6px;
  background: var(--color-card);
  border: 1px solid var(--color-border);
  font-family: var(--font-mono, monospace);
  font-size: var(--text-micro);
  color: var(--color-dim);
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 180px;
  overflow-y: auto;
}
</style>
