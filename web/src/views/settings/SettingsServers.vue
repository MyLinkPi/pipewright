<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import {
  listServers,
  createServer,
  updateServer,
  deleteServer,
  testServer,
} from '../../api/servers'
import type {
  Server,
  ServerJump,
  CreateServerInput,
  UpdateServerInput,
  ServerTestResult,
} from '../../api/servers'
import { listCredentials } from '../../api/credentials'
import type { Credential } from '../../api/credentials'
import { useLabels } from '../../api/labels'
import { HttpError } from '../../api/http'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useDirtyGuard } from '../../composables/useDirtyGuard'
import ServiceLogViewer from '../../components/ops/ServiceLogViewer.vue'
import ServiceOpsPanel from '../../components/ops/ServiceOpsPanel.vue'

const router = useRouter()
const { t } = useI18n()
const { confirmDiscard } = useDirtyGuard()
const formSnapshot = ref('')

// ─── state ──────────────────────────────────────────────────────────────────

type LoadState = 'idle' | 'loading' | 'error'

const loadState = ref<LoadState>('idle')
const loadError = ref('')
const servers = ref<Server[]>([])

// SSH credentials available to bind (ssh_key private key, or ssh_password login password).
const sshCredentials = ref<Credential[]>([])

// Optional sudo_password credentials (privilege escalation for non-root logins).
const sudoCredentials = ref<Credential[]>([])

// ─── 机器标签登记处(标签字典,可先建后挂)─────────────────────────────────────
// 登记处建/删标签;servers.labels 仍是机器实际标签的事实来源。下拉候选 = 登记处 ∪ 机器实际标签。

const {
  items: registryLabels,
  load: loadRegistryLabels,
  ensure: ensureLabel,
  remove: removeLabelReg,
} = useLabels()

const attachedLabelTerms = computed<string[]>(() => {
  const set = new Set<string>()
  for (const s of servers.value) {
    for (const p of (s.labels || '').split(',')) {
      const term = p.trim()
      if (term) set.add(term)
    }
  }
  return [...set].sort()
})

const registryLabelNames = computed<string[]>(() => registryLabels.value.map((l) => l.name))

/** 标签当前被几台机器引用(展示用;权威裁决在服务端)。 */
function labelUsage(name: string): number {
  return servers.value.filter((s) =>
    (s.labels || '').split(',').map((x) => x.trim()).includes(name),
  ).length
}

// ─── add / edit modal ───────────────────────────────────────────────────────

const modalOpen = ref(false)
const modalMode = ref<'add' | 'edit'>('add')
const editingId = ref<string | null>(null)

// 跳板链表单行:rid 供 v-for 稳定 key;errors 存每行自己的校验文案。
interface JumpRow {
  rid: number
  host: string
  port: number
  user: string
  credentialId: string
  errors: { host: string; port: string; user: string; credentialId: string }
}

const MAX_JUMPS = 5
let jumpRidSeed = 0

function newJumpRow(init?: Partial<Omit<JumpRow, 'rid' | 'errors'>>): JumpRow {
  jumpRidSeed += 1
  return {
    rid: jumpRidSeed,
    host: init?.host ?? '',
    port: init?.port ?? 22,
    user: init?.user ?? '',
    credentialId: init?.credentialId ?? '',
    errors: { host: '', port: '', user: '', credentialId: '' },
  }
}

const form = ref({
  name: '',
  host: '',
  port: 22,
  user: '',
  credentialId: '',
  sudoCredentialId: '',
  jumps: [] as JumpRow[],
  labels: '',
  maxBuilds: 0,
  priority: 0,
})

const formErrors = ref({
  name: '',
  host: '',
  port: '',
  user: '',
  credentialId: '',
  labels: '',
  maxBuilds: '',
  priority: '',
})

const formBanner = ref('')
const formSubmitting = ref(false)

// ─── delete confirm ─────────────────────────────────────────────────────────

const deleteModalOpen = ref(false)
const deletingServer = ref<Server | null>(null)
const deleteSubmitting = ref(false)
const deleteBanner = ref('')

// ─── test connection ────────────────────────────────────────────────────────

const testingId = ref<string | null>(null)
const testResults = ref<Record<string, ServerTestResult>>({})

// ─── service logs viewer (Story 6-2, FR-16) ──────────────────────────────────

const logsModalOpen = ref(false)
const logsServer = ref<Server | null>(null)

function openLogsModal(s: Server): void {
  logsServer.value = s
  logsModalOpen.value = true
}

function closeLogsModal(): void {
  logsModalOpen.value = false
  logsServer.value = null
}

// ─── service operations panel (Story 6-3, FR-17) ─────────────────────────────

const opsModalOpen = ref(false)
const opsServer = ref<Server | null>(null)

function openOpsModal(s: Server): void {
  opsServer.value = s
  opsModalOpen.value = true
}

function closeOpsModal(): void {
  opsModalOpen.value = false
  opsServer.value = null
}

// ─── container terminal (Story 6-4, FR-18) ───────────────────────────────────
// 升级为独立全屏「AI 运维终端」页(左终端 / 右 AI 助手),替代旧内联弹窗。

function openTerminal(s: Server): void {
  // 全屏终端在**新标签页**打开,保留当前服务器列表页(终端是长会话,不该顶掉原页面)。
  const route = router.resolve({ name: 'server-terminal', params: { id: s.id } })
  window.open(route.href, '_blank', 'noopener')
}

// ─── helpers ────────────────────────────────────────────────────────────────

const hasSSHCredentials = computed(() => sshCredentials.value.length > 0)

function credentialLabel(id: string): string {
  const c = sshCredentials.value.find((c) => c.id === id)
  return c ? c.name : t('settingsServers.credentialDeleted')
}

// ─── data loading ────────────────────────────────────────────────────────────

async function loadServers(): Promise<void> {
  loadState.value = 'loading'
  loadError.value = ''
  try {
    const [srv, creds] = await Promise.all([listServers(), listCredentials()])
    servers.value = srv
    sshCredentials.value = creds.filter((c) => c.type === 'ssh_key' || c.type === 'ssh_password')
    sudoCredentials.value = creds.filter((c) => c.type === 'sudo_password')
    loadState.value = 'idle'
  } catch (err) {
    if (err instanceof HttpError) {
      if (err.status === 0) {
        loadError.value = t('settingsServers.errLoadConn')
      } else if (err.apiError?.code === 'vault_unconfigured') {
        loadError.value = t('settingsServers.errVaultUnconfigured')
      } else {
        loadError.value = err.apiError?.message ?? t('settingsServers.errLoadStatus', { status: err.status })
      }
    } else {
      loadError.value = t('settingsServers.errLoadRetry')
    }
    loadState.value = 'error'
  }
}

onMounted(() => {
  void loadServers()
  void loadRegistryLabels()
})

// ─── 登记处管理(建/删标签)──────────────────────────────────────────────────

const newLabelName = ref('')
const labelSubmitting = ref(false)
const labelError = ref('')

function labelErrMsg(e: unknown, fallback: string): string {
  if (e instanceof HttpError) {
    if (e.status === 409) return t('settingsServers.labelExists')
    if (e.status === 400) return t('settingsServers.labelInvalid')
  }
  return fallback
}

async function handleCreateLabel(): Promise<void> {
  const name = newLabelName.value.trim()
  if (!name || labelSubmitting.value) return
  labelSubmitting.value = true
  labelError.value = ''
  try {
    await ensureLabel(name)
    newLabelName.value = ''
  } catch (e) {
    labelError.value = labelErrMsg(e, t('settingsServers.labelCreateFailed'))
  } finally {
    labelSubmitting.value = false
  }
}

async function handleDeleteLabel(name: string): Promise<void> {
  if (labelSubmitting.value) return
  labelError.value = ''
  try {
    await removeLabelReg(name)
  } catch (e) {
    if (e instanceof HttpError && e.status === 409) labelError.value = t('settingsServers.labelInUse')
    else labelError.value = t('settingsServers.labelDeleteFailed')
  }
}

// ─── 服务器表单的标签 chips(候选 = 登记处 ∪ 机器实际标签;支持inline新建并登记)────

const formLabelTerms = computed<string[]>(() =>
  form.value.labels.split(',').map((x) => x.trim()).filter(Boolean),
)

const formLabelCandidates = computed<string[]>(() => {
  const taken = new Set(formLabelTerms.value)
  return [...new Set([...registryLabelNames.value, ...attachedLabelTerms.value])]
    .filter((n) => !taken.has(n))
    .sort()
})

const formAddPick = ref('')
const inlineLabelName = ref('')

function addFormLabel(term: string): void {
  formAddPick.value = ''
  if (!term || formLabelTerms.value.includes(term)) return
  form.value.labels = formLabelTerms.value.concat(term).join(',')
}

function removeFormLabel(term: string): void {
  form.value.labels = formLabelTerms.value.filter((x) => x !== term).join(',')
}

/** 表单里 inline 新建:登记进字典并挂到本机;同名已登记 → 直接挂。 */
async function createAndAttachLabel(): Promise<void> {
  const name = inlineLabelName.value.trim()
  if (!name || labelSubmitting.value) return
  labelSubmitting.value = true
  labelError.value = ''
  try {
    await ensureLabel(name)
    addFormLabel(name)
    inlineLabelName.value = ''
  } catch (e) {
    if (e instanceof HttpError && e.status === 409) {
      addFormLabel(name)
      inlineLabelName.value = ''
    } else {
      labelError.value = labelErrMsg(e, t('settingsServers.labelCreateFailed'))
    }
  } finally {
    labelSubmitting.value = false
  }
}

// ─── modal open / close ─────────────────────────────────────────────────────

function openAddModal(): void {
  modalMode.value = 'add'
  editingId.value = null
  form.value = {
    name: '',
    host: '',
    port: 22,
    user: '',
    credentialId: sshCredentials.value[0]?.id ?? '',
    sudoCredentialId: '',
    jumps: [],
    labels: '',
    maxBuilds: 0,
    priority: 0,
  }
  clearFormErrors()
  formBanner.value = ''
  modalOpen.value = true
  formSnapshot.value = JSON.stringify(form.value)
}

function openEditModal(s: Server): void {
  modalMode.value = 'edit'
  editingId.value = s.id
  form.value = {
    name: s.name,
    host: s.host,
    port: s.port,
    user: s.user,
    credentialId: s.credentialId,
    sudoCredentialId: s.sudoCredentialId ?? '',
    jumps: (s.jumps ?? []).map((j) => newJumpRow(j)),
    labels: s.labels ?? '',
    maxBuilds: s.maxBuilds ?? 0,
    priority: s.priority ?? 0,
  }
  clearFormErrors()
  formBanner.value = ''
  modalOpen.value = true
  formSnapshot.value = JSON.stringify(form.value)
}

// ─── 跳板链行操作(连接顺序:第 1 跳最先连,目标经最后一跳到达)────────────────

function addJump(): void {
  if (form.value.jumps.length >= MAX_JUMPS) return
  form.value.jumps.push(newJumpRow())
}

function removeJump(idx: number): void {
  form.value.jumps.splice(idx, 1)
}

function closeModal(): void {
  if (formSubmitting.value) return
  modalOpen.value = false
}

/** Shared close path for ✕ / 取消: confirm before discarding a dirty form. */
async function requestClose(): Promise<void> {
  if (JSON.stringify(form.value) !== formSnapshot.value && !(await confirmDiscard())) return
  closeModal()
}

// ─── form validation ─────────────────────────────────────────────────────────

function clearFormErrors(): void {
  formErrors.value = { name: '', host: '', port: '', user: '', credentialId: '', labels: '', maxBuilds: '', priority: '' }
}

function validateForm(): boolean {
  clearFormErrors()
  let ok = true
  if (!form.value.name.trim()) {
    formErrors.value.name = t('settingsServers.errNameRequired')
    ok = false
  }
  if (!form.value.host.trim()) {
    formErrors.value.host = t('settingsServers.errHostRequired')
    ok = false
  }
  if (!form.value.user.trim()) {
    formErrors.value.user = t('settingsServers.errUserRequired')
    ok = false
  }
  if (!Number.isInteger(form.value.port) || form.value.port < 1 || form.value.port > 65535) {
    formErrors.value.port = t('settingsServers.errPortRange')
    ok = false
  }
  if (!form.value.credentialId) {
    formErrors.value.credentialId = t('settingsServers.errCredentialRequired')
    ok = false
  }
  // 逐跳校验跳板链(错误挂在各自行上)。
  for (const row of form.value.jumps) {
    row.errors = { host: '', port: '', user: '', credentialId: '' }
    if (!row.host.trim()) {
      row.errors.host = t('settingsServers.errJumpHostRequired')
      ok = false
    }
    if (!Number.isInteger(row.port) || row.port < 1 || row.port > 65535) {
      row.errors.port = t('settingsServers.errPortRange')
      ok = false
    }
    if (!row.user.trim()) {
      row.errors.user = t('settingsServers.errJumpUserRequired')
      ok = false
    }
    if (!row.credentialId) {
      row.errors.credentialId = t('settingsServers.errJumpCredentialRequired')
      ok = false
    }
  }
  const mb = Number(form.value.maxBuilds)
  if (!Number.isInteger(mb) || mb < 0 || mb > 64) {
    formErrors.value.maxBuilds = t('settingsServers.errMaxBuildsRange')
    ok = false
  }
  const pr = Number(form.value.priority)
  if (!Number.isInteger(pr) || pr < 0 || pr > 100) {
    formErrors.value.priority = t('settingsServers.errPriorityRange')
    ok = false
  }
  return ok
}

// ─── form submit ─────────────────────────────────────────────────────────────

/** 表单行 → 契约跳板链(create/update 都是整体提交,替换语义)。 */
function jumpsToPayload(): ServerJump[] {
  return form.value.jumps.map((row) => ({
    host: row.host.trim(),
    port: row.port,
    user: row.user.trim(),
    credentialId: row.credentialId,
  }))
}

async function handleFormSubmit(): Promise<void> {
  if (!validateForm()) return
  formSubmitting.value = true
  formBanner.value = ''
  try {
    if (modalMode.value === 'add') {
      const payload: CreateServerInput = {
        name: form.value.name.trim(),
        host: form.value.host.trim(),
        port: form.value.port,
        user: form.value.user.trim(),
        credentialId: form.value.credentialId,
        sudoCredentialId: form.value.sudoCredentialId,
        jumps: jumpsToPayload(),
        labels: form.value.labels.trim(),
        maxBuilds: Number(form.value.maxBuilds) || 0,
        priority: Number(form.value.priority) || 0,
      }
      const created = await createServer(payload)
      servers.value = [created, ...servers.value]
    } else if (editingId.value) {
      const payload: UpdateServerInput = {
        name: form.value.name.trim(),
        host: form.value.host.trim(),
        port: form.value.port,
        user: form.value.user.trim(),
        credentialId: form.value.credentialId,
        sudoCredentialId: form.value.sudoCredentialId,
        jumps: jumpsToPayload(),
        labels: form.value.labels.trim(),
        maxBuilds: Number(form.value.maxBuilds) || 0,
        priority: Number(form.value.priority) || 0,
      }
      const updated = await updateServer(editingId.value, payload)
      servers.value = servers.value.map((s) => (s.id === updated.id ? updated : s))
      delete testResults.value[updated.id]
    }
    modalOpen.value = false
  } catch (err) {
    if (err instanceof HttpError) {
      if (err.status === 0) {
        formBanner.value = t('settingsServers.errConnRetry')
      } else {
        formBanner.value = err.apiError?.message ?? t('settingsServers.errSaveStatus', { status: err.status })
      }
    } else {
      formBanner.value = t('settingsServers.errSaveRetry')
    }
  } finally {
    formSubmitting.value = false
  }
}

// ─── delete ──────────────────────────────────────────────────────────────────

function openDeleteModal(s: Server): void {
  deletingServer.value = s
  deleteBanner.value = ''
  deleteModalOpen.value = true
}

function closeDeleteModal(): void {
  if (deleteSubmitting.value) return
  deleteModalOpen.value = false
  deletingServer.value = null
}

async function confirmDelete(): Promise<void> {
  if (!deletingServer.value) return
  deleteSubmitting.value = true
  deleteBanner.value = ''
  const id = deletingServer.value.id
  try {
    await deleteServer(id)
    servers.value = servers.value.filter((s) => s.id !== id)
    delete testResults.value[id]
    deleteModalOpen.value = false
    deletingServer.value = null
  } catch (err) {
    if (err instanceof HttpError) {
      if (err.status === 0) {
        deleteBanner.value = t('settingsServers.errConnRetry')
      } else {
        deleteBanner.value = err.apiError?.message ?? t('settingsServers.errDeleteStatus', { status: err.status })
      }
    } else {
      deleteBanner.value = t('settingsServers.errDeleteRetry')
    }
  } finally {
    deleteSubmitting.value = false
  }
}

// ─── test connection ──────────────────────────────────────────────────────────

async function handleTest(s: Server): Promise<void> {
  testingId.value = s.id
  try {
    const result = await testServer(s.id)
    testResults.value = { ...testResults.value, [s.id]: result }
  } catch (err) {
    let message = t('settingsServers.errTestRetry')
    if (err instanceof HttpError) {
      message = err.apiError?.message ?? t('settingsServers.errTestStatus', { status: err.status })
    }
    testResults.value = {
      ...testResults.value,
      [s.id]: { ok: false, latencyMs: 0, output: '', error: message },
    }
  } finally {
    if (testingId.value === s.id) testingId.value = null
  }
}
</script>

<template>
  <div class="servers-root">
    <!-- ─── section header ──────────────────────────────────────────────────── -->
    <div class="section-head">
      <div class="section-head-text">
        <h2 class="section-title">{{ t('settingsServers.title') }}</h2>
        <p class="section-desc">
          {{ t('settingsServers.desc') }}
        </p>
      </div>
      <button
        class="btn-primary"
        :disabled="loadState === 'loading' || !hasSSHCredentials"
        :title="!hasSSHCredentials ? t('settingsServers.addDisabledHint') : ''"
        @click="openAddModal"
      >
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" aria-hidden="true">
          <path d="M12 5v14M5 12h14" />
        </svg>
        {{ t('settingsServers.addServer') }}
      </button>
    </div>

    <!-- ─── no-credential hint ────────────────────────────────────────────────── -->
    <div v-if="loadState === 'idle' && !hasSSHCredentials" class="banner banner--warn" role="status">
      <span>{{ t('settingsServers.noCredentialHint') }}</span>
    </div>

    <!-- ─── load error banner ─────────────────────────────────────────────────── -->
    <div v-if="loadState === 'error'" class="banner banner--error" role="alert">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
        <circle cx="12" cy="12" r="9" /><path d="M12 8v4M12 16h.01" />
      </svg>
      <span>{{ loadError }}</span>
      <button class="banner-retry" @click="loadServers">↻ {{ t('settingsServers.retry') }}</button>
    </div>

    <!-- ─── servers panel ─────────────────────────────────────────────────────── -->
    <div class="panel" :class="{ 'panel--loading': loadState === 'loading' }">
      <div class="panel-head">
        <span>{{ t('settingsServers.registeredServers') }}</span>
        <span class="panel-meta" v-if="loadState === 'idle'">{{ t('settingsServers.serverCount', { n: servers.length }) }}</span>
      </div>

      <template v-if="loadState === 'loading'">
        <div class="skel-row" v-for="i in 3" :key="i" aria-hidden="true">
          <div class="skel-bar" />
        </div>
      </template>

      <template v-else-if="loadState === 'idle' && servers.length === 0">
        <div class="empty-row" role="status">{{ t('settingsServers.emptyList') }}</div>
      </template>

      <ul v-else class="server-list">
        <li v-for="s in servers" :key="s.id" class="server-row">
          <div class="server-main">
            <div class="server-name">{{ s.name }}</div>
            <div class="server-addr">
              <span class="mono">{{ s.user }}@{{ s.host }}:{{ s.port }}</span>
              <span v-if="s.jumps?.length" class="cred-tag cred-tag--jump" :title="t('settingsServers.jumpBadgeHint', { n: s.jumps.length })">⛓ {{ t('settingsServers.jumpBadge', { n: s.jumps.length }) }}</span>
              <span class="cred-tag">🔑 {{ credentialLabel(s.credentialId) }}</span>
              <span v-if="s.labels" class="cred-tag cred-tag--pool" :title="t('settingsServers.poolBadgeHint', { slots: s.maxBuilds || 1, priority: s.priority })">🏗 {{ s.labels }}</span>
            </div>
            <!-- test result -->
            <div
              v-if="testResults[s.id]"
              class="test-result"
              :class="testResults[s.id].ok ? 'test-result--ok' : 'test-result--fail'"
              role="status"
            >
              <template v-if="testResults[s.id].ok">
                ✓ {{ t('settingsServers.testOk', { ms: testResults[s.id].latencyMs }) }}
                <span class="mono uname">{{ testResults[s.id].output }}</span>
              </template>
              <template v-else>
                ✕ {{ testResults[s.id].error }}
              </template>
            </div>
          </div>
          <div class="server-actions">
            <button class="btn-ghost" :disabled="testingId === s.id" @click="handleTest(s)">
              {{ testingId === s.id ? t('settingsServers.testing') : t('settingsServers.testConnection') }}
            </button>
            <button class="btn-ghost" @click="openLogsModal(s)">{{ t('settingsServers.logs') }}</button>
            <button class="btn-ghost" @click="openTerminal(s)">{{ t('settingsServers.terminal') }}</button>
            <button class="btn-ghost" @click="openOpsModal(s)">{{ t('settingsServers.serviceOps') }}</button>
            <button class="btn-ghost" @click="openEditModal(s)">{{ t('settingsServers.edit') }}</button>
            <button class="btn-ghost btn-danger" @click="openDeleteModal(s)">{{ t('settingsServers.delete') }}</button>
          </div>
        </li>
      </ul>
    </div>

    <!-- ─── 机器标签登记处(标签字典:可先建后挂的悬置标签)────────────────────── -->
    <div class="panel">
      <div class="panel-head">
        <span>{{ t('settingsServers.labelRegistryTitle') }}</span>
        <span class="panel-meta">{{ t('settingsServers.labelRegistrySub') }}</span>
      </div>
      <div v-if="labelError" class="banner banner--error" role="alert">{{ labelError }}</div>
      <div class="labelreg-body">
        <div class="labelreg-create">
          <input
            v-model="newLabelName"
            class="field-input labelchips-new"
            type="text"
            :placeholder="t('settingsServers.newLabelPlaceholder')"
            :aria-label="t('settingsServers.newLabelAria')"
            autocomplete="off"
            @keydown.enter.prevent="handleCreateLabel"
          />
          <button class="btn-ghost" :disabled="labelSubmitting || !newLabelName.trim()" @click="handleCreateLabel">
            {{ t('settingsServers.createLabel') }}
          </button>
        </div>
        <div v-if="registryLabels.length === 0" class="empty-row" role="status">
          {{ t('settingsServers.labelRegistryEmpty') }}
        </div>
        <ul v-else class="labelreg-list">
          <li v-for="l in registryLabels" :key="l.name" class="labelreg-row">
            <span class="labelchip labelchip--static">
              {{ l.name }}
              <span class="labelreg-usage">
                {{ labelUsage(l.name) > 0
                  ? t('settingsServers.labelUsage', { n: labelUsage(l.name) })
                  : t('settingsServers.labelDangling') }}
              </span>
            </span>
            <button class="btn-ghost btn-danger" @click="handleDeleteLabel(l.name)">
              {{ t('settingsServers.delete') }}
            </button>
          </li>
        </ul>
      </div>
    </div>

    <!-- ─── add / edit modal ──────────────────────────────────────────────────── -->
    <div v-if="modalOpen" class="modal-backdrop">
      <div class="modal" role="dialog" aria-modal="true" aria-labelledby="server-modal-title">
        <h3 id="server-modal-title" class="modal-title">
          {{ modalMode === 'add' ? t('settingsServers.addServer') : t('settingsServers.editServer') }}
        </h3>

        <div v-if="formBanner" class="banner banner--error" role="alert">{{ formBanner }}</div>

        <form @submit.prevent="handleFormSubmit">
          <label class="field">
            <span class="field-label">{{ t('settingsServers.fieldName') }}</span>
            <input v-model="form.name" class="field-input" type="text" placeholder="web-prod-1" autocomplete="off" />
            <span v-if="formErrors.name" class="field-error">{{ formErrors.name }}</span>
          </label>

          <div class="field-row">
            <label class="field field-grow">
              <span class="field-label">{{ t('settingsServers.fieldHost') }}</span>
              <input v-model="form.host" class="field-input" type="text" placeholder="10.0.0.5" autocomplete="off" />
              <span v-if="formErrors.host" class="field-error">{{ formErrors.host }}</span>
            </label>
            <label class="field field-port">
              <span class="field-label">{{ t('settingsServers.fieldPort') }}</span>
              <input v-model.number="form.port" class="field-input" type="number" min="1" max="65535" />
              <span v-if="formErrors.port" class="field-error">{{ formErrors.port }}</span>
            </label>
          </div>

          <label class="field">
            <span class="field-label">{{ t('settingsServers.fieldUser') }}</span>
            <input v-model="form.user" class="field-input" type="text" placeholder="deploy" autocomplete="off" />
            <span v-if="formErrors.user" class="field-error">{{ formErrors.user }}</span>
          </label>

          <label class="field">
            <span class="field-label">{{ t('settingsServers.fieldCredential') }}</span>
            <select v-model="form.credentialId" class="field-input">
              <option value="" disabled>{{ t('settingsServers.selectCredential') }}</option>
              <option v-for="c in sshCredentials" :key="c.id" :value="c.id">{{ c.name }}</option>
            </select>
            <span v-if="formErrors.credentialId" class="field-error">{{ formErrors.credentialId }}</span>
          </label>

          <!-- Optional sudo escalation credential: used via sudo -S when the login user
               is neither root nor passwordless-sudo capable (e.g. platform HTTPS). -->
          <label class="field">
            <span class="field-label">{{ t('settingsServers.fieldSudoCredential') }}</span>
            <select v-model="form.sudoCredentialId" class="field-input">
              <option value="">{{ t('settingsServers.sudoCredentialNone') }}</option>
              <option v-for="c in sudoCredentials" :key="c.id" :value="c.id">{{ c.name }}</option>
            </select>
            <span class="field-hint">{{ t('settingsServers.sudoCredentialHint') }}</span>
          </label>

          <!-- SSH jump chain (multi-hop): rows are in connection order — the platform
               connects to hop 1 first and reaches the server through the last hop.
               Every hop carries its own independent SSH credential reference. -->
          <div class="field">
            <span class="field-label">{{ t('settingsServers.jumpTitle') }}</span>
            <div v-for="(row, idx) in form.jumps" :key="row.rid" class="jump-row">
              <span class="jump-order" aria-hidden="true">{{ idx + 1 }}</span>
              <div class="jump-fields">
                <div class="jump-line">
                  <input
                    v-model="row.host"
                    class="field-input jump-host"
                    type="text"
                    :placeholder="t('settingsServers.jumpHostPlaceholder')"
                    :aria-label="t('settingsServers.jumpHost')"
                    autocomplete="off"
                  />
                  <input
                    v-model.number="row.port"
                    class="field-input jump-port"
                    type="number"
                    min="1"
                    max="65535"
                    :aria-label="t('settingsServers.fieldPort')"
                  />
                  <input
                    v-model="row.user"
                    class="field-input jump-user"
                    type="text"
                    :placeholder="t('settingsServers.fieldUser')"
                    :aria-label="t('settingsServers.fieldUser')"
                    autocomplete="off"
                  />
                  <select
                    v-model="row.credentialId"
                    class="field-input jump-cred"
                    :aria-label="t('settingsServers.fieldCredential')"
                  >
                    <option value="" disabled>{{ t('settingsServers.selectCredential') }}</option>
                    <option v-for="c in sshCredentials" :key="c.id" :value="c.id">{{ c.name }}</option>
                  </select>
                  <button
                    type="button"
                    class="jump-del"
                    :aria-label="t('settingsServers.jumpRemove')"
                    @click="removeJump(idx)"
                  >
                    <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>
                  </button>
                </div>
                <div v-if="row.errors.host || row.errors.port || row.errors.user || row.errors.credentialId" class="jump-errors">
                  <span v-if="row.errors.host" class="field-error">{{ row.errors.host }}</span>
                  <span v-if="row.errors.port" class="field-error">{{ row.errors.port }}</span>
                  <span v-if="row.errors.user" class="field-error">{{ row.errors.user }}</span>
                  <span v-if="row.errors.credentialId" class="field-error">{{ row.errors.credentialId }}</span>
                </div>
              </div>
            </div>
            <button
              type="button"
              class="btn-ghost jump-add"
              :disabled="form.jumps.length >= MAX_JUMPS"
              :title="form.jumps.length >= MAX_JUMPS ? t('settingsServers.jumpMaxReached') : undefined"
              @click="addJump"
            >
              ＋ {{ t('settingsServers.jumpAdd') }}
            </button>
            <span class="field-hint">{{ t('settingsServers.jumpHint') }}</span>
          </div>

          <!-- build-pool fields (FR-8-19): labels make the machine schedulable; unlabeled machines never pick.
               标签从登记处 ∪ 机器实际标签里选(chips),也可 inline 新建(自动登记);不再手敲逗号串。 -->
          <div class="field">
            <span class="field-label">{{ t('settingsServers.fieldLabels') }}</span>
            <div class="labelchips" :aria-label="t('settingsServers.labelsEditorAria')">
              <span v-for="term in formLabelTerms" :key="term" class="labelchip">
                {{ term }}
                <button
                  type="button"
                  class="labelchip-del"
                  :aria-label="t('settingsServers.labelDelAria', { term })"
                  @click="removeFormLabel(term)"
                >
                  <svg width="9" height="9" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>
                </button>
              </span>
              <span v-if="!formLabelTerms.length" class="labelchips-empty">{{ t('settingsServers.labelsEmpty') }}</span>
            </div>
            <div class="labelchips-addrow">
              <select
                v-model="formAddPick"
                class="field-input labelchips-select"
                :aria-label="t('settingsServers.addLabelAria')"
                @change="addFormLabel(formAddPick)"
              >
                <option value="" disabled>{{ t('settingsServers.addLabelPick') }}</option>
                <option v-for="c in formLabelCandidates" :key="c" :value="c">{{ c }}</option>
              </select>
              <input
                v-model="inlineLabelName"
                class="field-input labelchips-new"
                type="text"
                :placeholder="t('settingsServers.newLabelPlaceholder')"
                :aria-label="t('settingsServers.newLabelAria')"
                autocomplete="off"
                @keydown.enter.prevent="createAndAttachLabel"
              />
              <button type="button" class="btn-ghost" :disabled="labelSubmitting || !inlineLabelName.trim()" @click="createAndAttachLabel">
                {{ t('settingsServers.newLabelCreate') }}
              </button>
            </div>
            <span class="field-hint">{{ t('settingsServers.labelsHint') }}</span>
            <span v-if="formErrors.labels" class="field-error">{{ formErrors.labels }}</span>
          </div>
          <div class="field-row">
            <label class="field field-maxbuilds">
              <span class="field-label">{{ t('settingsServers.fieldMaxBuilds') }}</span>
              <input v-model.number="form.maxBuilds" class="field-input" type="number" min="0" max="64" />
              <span class="field-hint">{{ t('settingsServers.maxBuildsHint') }}</span>
              <span v-if="formErrors.maxBuilds" class="field-error">{{ formErrors.maxBuilds }}</span>
            </label>
            <label class="field field-priority">
              <span class="field-label">{{ t('settingsServers.fieldPriority') }}</span>
              <input v-model.number="form.priority" class="field-input" type="number" min="0" max="100" />
              <span class="field-hint">{{ t('settingsServers.priorityHint') }}</span>
              <span v-if="formErrors.priority" class="field-error">{{ formErrors.priority }}</span>
            </label>
          </div>

          <div class="modal-actions">
            <button type="button" class="btn-ghost" :disabled="formSubmitting" @click="requestClose">{{ t('settingsServers.cancel') }}</button>
            <button type="submit" class="btn-primary" :disabled="formSubmitting">
              {{ formSubmitting ? t('settingsServers.saving') : t('settingsServers.save') }}
            </button>
          </div>
        </form>
      </div>
    </div>

    <!-- ─── delete confirm modal ──────────────────────────────────────────────── -->
    <div v-if="deleteModalOpen" class="modal-backdrop">
      <div class="modal" role="dialog" aria-modal="true" aria-labelledby="server-del-title">
        <h3 id="server-del-title" class="modal-title">{{ t('settingsServers.deleteServer') }}</h3>
        <div v-if="deleteBanner" class="banner banner--error" role="alert">{{ deleteBanner }}</div>
        <p class="modal-text">
          <i18n-t keypath="settingsServers.deleteConfirm" tag="span">
            <template #name><strong>{{ deletingServer?.name }}</strong></template>
          </i18n-t>
        </p>
        <div class="modal-actions">
          <button type="button" class="btn-ghost" :disabled="deleteSubmitting" @click="closeDeleteModal">{{ t('settingsServers.cancel') }}</button>
          <button type="button" class="btn-primary btn-danger" :disabled="deleteSubmitting" @click="confirmDelete">
            {{ deleteSubmitting ? t('settingsServers.deleting') : t('settingsServers.confirmDelete') }}
          </button>
        </div>
      </div>
    </div>

    <!-- ─── service logs modal (Story 6-2, FR-16) ─────────────────────────────── -->
    <div v-if="logsModalOpen" class="modal-backdrop">
      <div class="modal modal--wide" role="dialog" aria-modal="true" aria-labelledby="server-logs-title">
        <div class="logs-modal-head">
          <h3 id="server-logs-title" class="modal-title">
            {{ t('settingsServers.serviceLogsTitle', { name: logsServer?.name }) }}
          </h3>
          <button type="button" class="btn-ghost" :aria-label="t('settingsServers.close')" @click="closeLogsModal">{{ t('settingsServers.close') }}</button>
        </div>
        <p class="logs-modal-sub mono">
          {{ logsServer?.user }}@{{ logsServer?.host }}:{{ logsServer?.port }}
        </p>
        <ServiceLogViewer
          v-if="logsServer"
          :server-id="logsServer.id"
          :server-name="logsServer.name"
        />
      </div>
    </div>

    <!-- ─── service operations modal (Story 6-3, FR-17) ───────────────────────── -->
    <div v-if="opsModalOpen" class="modal-backdrop">
      <div class="modal" role="dialog" aria-modal="true" aria-labelledby="server-ops-title">
        <div class="logs-modal-head">
          <h3 id="server-ops-title" class="modal-title">
            {{ t('settingsServers.serviceOpsTitle', { name: opsServer?.name }) }}
          </h3>
          <button type="button" class="btn-ghost" :aria-label="t('settingsServers.close')" @click="closeOpsModal">{{ t('settingsServers.close') }}</button>
        </div>
        <p class="logs-modal-sub mono">
          {{ opsServer?.user }}@{{ opsServer?.host }}:{{ opsServer?.port }}
        </p>
        <ServiceOpsPanel
          v-if="opsServer"
          :server-id="opsServer.id"
          :server-name="opsServer.name"
        />
      </div>
    </div>
  </div>
</template>

<style scoped>
.servers-root {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

.section-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 24px;
}

.section-title {
  font-size: var(--text-heading);
  font-weight: 600;
  color: var(--color-text);
}

.section-desc {
  font-size: var(--text-label);
  color: var(--color-faint);
  margin-top: 6px;
  max-width: 60ch;
  line-height: 1.55;
}

.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 8px 14px;
  font-size: var(--text-label);
  font-weight: 600;
  color: #fff;
  background: var(--color-primary);
  border: none;
  border-radius: var(--radius-md);
  cursor: pointer;
  white-space: nowrap;
  transition: filter var(--duration-fast);
}
.btn-primary:hover:not(:disabled) {
  filter: brightness(1.08);
}
.btn-primary:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.btn-ghost {
  padding: 6px 12px;
  font-size: var(--text-label);
  font-weight: 500;
  color: var(--color-dim);
  background: transparent;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition:
    color var(--duration-fast),
    border-color var(--duration-fast),
    background var(--duration-fast);
}
.btn-ghost:hover:not(:disabled) {
  color: var(--color-text);
  border-color: var(--color-dim);
}
.btn-ghost:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.btn-danger {
  color: var(--color-danger, #d4503e);
}
.btn-danger:hover:not(:disabled) {
  border-color: var(--color-danger, #d4503e);
  color: var(--color-danger, #d4503e);
}
.btn-primary.btn-danger {
  color: #fff;
  background: var(--color-danger, #d4503e);
}

.banner {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  font-size: var(--text-label);
  border-radius: var(--radius-md);
}
.banner--error {
  color: var(--color-danger, #d4503e);
  background: color-mix(in oklch, var(--color-danger, #d4503e) 10%, transparent);
}
.banner--warn {
  color: var(--color-warn, #b88600);
  background: color-mix(in oklch, var(--color-warn, #b88600) 12%, transparent);
}
.banner-retry {
  margin-left: auto;
  background: none;
  border: none;
  color: inherit;
  font-weight: 600;
  cursor: pointer;
}

.panel {
  border: 1px solid var(--color-border);
  border-radius: var(--radius-lg);
  background: var(--color-surface);
  overflow: hidden;
}
.panel--loading {
  opacity: 0.7;
}
.panel-head {
  display: flex;
  justify-content: space-between;
  padding: 12px 18px;
  font-size: var(--text-label);
  font-weight: 600;
  color: var(--color-dim);
  border-bottom: 1px solid var(--color-border);
}
.panel-meta {
  color: var(--color-faint);
  font-weight: 500;
}

.skel-row {
  padding: 16px 18px;
}
.skel-bar {
  height: 16px;
  border-radius: 4px;
  background: linear-gradient(90deg, var(--color-border) 25%, transparent 50%, var(--color-border) 75%);
  background-size: 200% 100%;
  animation: skel 1.4s ease-in-out infinite;
}
@keyframes skel {
  0% {
    background-position: 200% 0;
  }
  100% {
    background-position: -200% 0;
  }
}

.empty-row {
  padding: 28px 18px;
  text-align: center;
  font-size: var(--text-label);
  color: var(--color-faint);
}

.server-list {
  list-style: none;
}
.server-row {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 16px;
  padding: 16px 18px;
  border-bottom: 1px solid var(--color-border);
}
.server-row:last-child {
  border-bottom: none;
}
.server-main {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
.server-name {
  font-size: var(--text-body);
  font-weight: 600;
  color: var(--color-text);
}
.server-addr {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px;
  font-size: var(--text-label);
  color: var(--color-faint);
}
.mono {
  font-family: var(--font-mono, ui-monospace, monospace);
}
.cred-tag {
  color: var(--color-dim);
}
.test-result {
  margin-top: 6px;
  font-size: var(--text-label);
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: baseline;
}
.test-result--ok {
  color: var(--color-success, #2e8b57);
}
.test-result--fail {
  color: var(--color-danger, #d4503e);
}
.uname {
  color: var(--color-faint);
  font-size: 0.85em;
  word-break: break-all;
}
.server-actions {
  display: flex;
  gap: 8px;
  flex-shrink: 0;
}

/* modal */
.modal-backdrop {
  position: fixed;
  inset: 0;
  background: color-mix(in oklch, var(--color-text) 40%, transparent);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
  z-index: 100;
}
.modal {
  width: 100%;
  max-width: 440px;
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-lg);
  padding: 24px;
  display: flex;
  flex-direction: column;
  gap: 16px;
  box-shadow: var(--shadow-lg, 0 24px 60px rgba(0, 0, 0, 0.24));
}
.modal--wide {
  max-width: min(960px, 92vw);
}
.logs-modal-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}
.logs-modal-sub {
  font-size: 0.78rem;
  color: var(--color-dim);
  margin-top: -8px;
}
.modal-title {
  font-size: var(--text-heading);
  font-weight: 600;
  color: var(--color-text);
}
.modal-text {
  font-size: var(--text-label);
  color: var(--color-dim);
  line-height: 1.5;
}
.modal-actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  margin-top: 4px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-bottom: 14px;
}
.field-row {
  display: flex;
  gap: 12px;
}
.field-grow {
  flex: 1;
}
.field-port {
  width: 96px;
}
.field-maxbuilds {
  width: 130px;
}
.field-priority {
  flex: 1;
}
.field-hint {
  font-size: var(--text-caption, 0.7rem);
  color: var(--color-faint);
  line-height: 1.4;
}
.cred-tag--pool {
  color: var(--color-primary);
  background: var(--color-primary-soft);
}
.cred-tag--jump {
  color: var(--color-warn, #b45309);
  background: var(--color-warn-soft, rgba(180, 83, 9, 0.12));
}
/* 跳板链编辑:每行 = 序号 + 主机/端口/用户/凭据 + 删除;行间留白表达连接顺序。 */
.jump-row {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 8px;
  border: 1px dashed var(--color-border);
  border-radius: var(--radius-sm);
  margin-bottom: 8px;
}
.jump-order {
  flex: none;
  width: 22px;
  height: 22px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  margin-top: 4px;
  font-size: var(--text-caption, 0.7rem);
  font-weight: 600;
  color: var(--color-dim);
  background: var(--color-border);
  border-radius: 50%;
}
.jump-fields {
  flex: 1;
  min-width: 0;
}
.jump-line {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.jump-host {
  flex: 2 1 140px;
  min-width: 120px;
}
.jump-port {
  flex: 0 0 84px;
}
.jump-user {
  flex: 1 1 90px;
  min-width: 80px;
}
.jump-cred {
  flex: 2 1 140px;
  min-width: 130px;
}
.jump-del {
  flex: none;
  width: 26px;
  height: 26px;
  margin-top: 4px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  color: var(--color-faint);
  background: transparent;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: color var(--duration-fast), border-color var(--duration-fast);
}
.jump-del:hover {
  color: var(--color-danger, #b91c1c);
  border-color: var(--color-border);
}
.jump-errors {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 12px;
  margin-top: 4px;
}
.jump-add {
  align-self: flex-start;
}
.field-label {
  font-size: var(--text-label);
  font-weight: 500;
  color: var(--color-dim);
}
.field-input {
  padding: 8px 12px;
  font-size: var(--text-body);
  color: var(--color-text);
  background: var(--color-bg, var(--color-surface));
  border: 1px solid var(--color-border);
  border-radius: var(--radius-sm);
  transition: border-color var(--duration-fast);
}
.field-input:focus {
  outline: none;
  border-color: var(--color-primary);
}
.field-error {
  font-size: var(--text-label);
  color: var(--color-danger, #d4503e);
}

/* ─── 标签 chips(表单标签选择器 + 登记处列表共用)────────────────────────── */
.labelchips {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  min-height: 34px;
  padding: 5px 8px;
  border: 1px solid var(--color-border);
  border-radius: 8px;
  background: var(--color-inset);
}
.labelchips-empty {
  font-size: 0.76rem;
  color: var(--color-faint);
  font-style: italic;
  align-self: center;
}
.labelchip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 24px;
  padding: 0 5px 0 9px;
  border-radius: 100px;
  border: 1px solid var(--color-border-strong);
  background: var(--color-card);
  font-family: var(--font-mono);
  font-size: 0.74rem;
  color: var(--color-text);
  white-space: nowrap;
}
.labelchip--static {
  padding-right: 9px;
  cursor: default;
}
.labelchip-del {
  width: 16px;
  height: 16px;
  display: grid;
  place-items: center;
  border: none;
  border-radius: 100px;
  background: transparent;
  color: var(--color-faint);
  cursor: pointer;
  padding: 0;
}
.labelchip-del:hover {
  color: var(--color-danger, #d4503e);
  background: var(--color-inset);
}
.labelchips-addrow {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 8px;
}
.labelchips-select {
  width: auto;
  min-width: 200px;
  flex: 0 1 auto;
}
.labelchips-new {
  flex: 1 1 180px;
  min-width: 160px;
  font-family: var(--font-mono);
}

/* ─── 机器标签登记处面板 ─────────────────────────────────────────────────── */
.labelreg-body {
  padding: 12px 16px 14px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.labelreg-create {
  display: flex;
  gap: 8px;
  max-width: 420px;
}
.labelreg-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
}
.labelreg-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 6px 0;
  border-bottom: 1px solid var(--color-border);
}
.labelreg-row:last-child {
  border-bottom: none;
}
.labelreg-usage {
  font-family: var(--font-sans);
  font-size: 0.7rem;
  color: var(--color-faint);
}
</style>
