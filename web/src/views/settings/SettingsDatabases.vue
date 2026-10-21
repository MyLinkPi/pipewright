<script setup lang="ts">
/*
  SettingsDatabases.vue — 数据库实例注册(MySQL / PostgreSQL)。

  一条实例 = 一个外部数据库接入;密码只写不读(vault 密文,绝不回显,可在编辑中轮换)。
  实例默认只读:管理操作(行编辑/DDL/导入/用户管理)须在「数据库」页显式解锁写模式。
  应用商店部署 mysql/postgres 模板成功后自动登记(source=appstore,同地址重部署原地轮换密码)。

  - 列表:引擎徽章、名称(host:port / 用户名)、来源(手动/应用市场)、只读状态;
    每行「编辑」「测试连接」「管理」(跳 /databases)+ 删除。
  - 添加弹窗:引擎 + 名称 + 主机 + 端口 + 用户名 + 密码 + 加密选项;支持先「测试连接」再保存。
  - 编辑弹窗:改名称/主机/端口/用户名,轮换密码(留空不改动)。
  - 删除仅注销登记,不影响数据库本身。
*/
import { ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useDirtyGuard } from '../../composables/useDirtyGuard'
import { NIcon } from 'naive-ui'
import { Database, Plus, Trash, PlugConnected, Pencil, ExternalLink } from '@vicons/tabler'
import {
  listDBInstances,
  createDBInstance,
  updateDBInstance,
  deleteDBInstance,
  testDBInstance,
  testDBConnectionDirect,
  type DBInstance,
  type DBEngine,
  type CreateDBInstanceInput,
} from '../../api/dbAdmin'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'

const { t } = useI18n()
const { confirmDiscard } = useDirtyGuard()
const toast = useToast()

// ─── 列表加载 ──────────────────────────────────────────────────────────────────
type LoadState = 'idle' | 'loading' | 'error'
const loadState = ref<LoadState>('idle')
const loadError = ref('')
const instances = ref<DBInstance[]>([])

const engineLabels = (engine: DBEngine): string =>
  engine === 'mysql' ? 'MySQL' : 'PostgreSQL'

async function load(): Promise<void> {
  loadState.value = 'loading'
  loadError.value = ''
  try {
    const res = await listDBInstances()
    instances.value = res.items
    loadState.value = 'idle'
  } catch (err) {
    loadState.value = 'error'
    loadError.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('settingsDatabases.errLoad', { status: err.status }))
        : t('settingsDatabases.errNetwork')
  }
}

onMounted(load)

// ─── 添加弹窗 ──────────────────────────────────────────────────────────────────
const modalOpen = ref(false)
const submitting = ref(false)
const testingForm = ref(false)
const formBanner = ref('')
const form = ref<CreateDBInstanceInput & { tlsEnabled: boolean }>({
  engine: 'mysql',
  name: '',
  host: '',
  port: 3306,
  username: 'root',
  password: '',
  tls: '',
  tlsEnabled: false,
})
const errors = ref<Record<string, string>>({})
const formSnapshot = ref('')

function openAdd(): void {
  form.value = { engine: 'mysql', name: '', host: '', port: 3306, username: 'root', password: '', tls: '', tlsEnabled: false }
  errors.value = {}
  formBanner.value = ''
  modalOpen.value = true
  formSnapshot.value = JSON.stringify(form.value)
}

function closeModal(): void {
  if (submitting.value) return
  modalOpen.value = false
  form.value.password = ''
}

async function requestClose(): Promise<void> {
  if (JSON.stringify(form.value) !== formSnapshot.value && !(await confirmDiscard())) return
  closeModal()
}

function validate(): boolean {
  errors.value = {}
  let ok = true
  if (!form.value.name.trim()) {
    errors.value.name = t('settingsDatabases.valNameRequired')
    ok = false
  }
  if (!form.value.host.trim()) {
    errors.value.host = t('settingsDatabases.valHostRequired')
    ok = false
  }
  const port = Number(form.value.port)
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    errors.value.port = t('settingsDatabases.valPortRange')
    ok = false
  }
  if (!form.value.username.trim()) {
    errors.value.username = t('settingsDatabases.valUsernameRequired')
    ok = false
  }
  if (!form.value.password) {
    errors.value.password = t('settingsDatabases.valPasswordRequired')
    ok = false
  }
  return ok
}

/** 表单里的引擎切换时联动默认端口与用户名(用户未显式改过时)。 */
function switchEngine(engine: DBEngine): void {
  const wasMySQL = form.value.engine === 'mysql'
  const defaults: Record<DBEngine, { port: number; username: string }> = {
    mysql: { port: 3306, username: 'root' },
    postgres: { port: 5432, username: 'postgres' },
  }
  const prev = defaults[form.value.engine]
  if (form.value.port === prev.port && form.value.username === prev.username) {
    form.value.port = defaults[engine].port
    form.value.username = defaults[engine].username
  }
  form.value.engine = engine
}

/** 直连测试未保存的表单凭据(密码用完即弃)。 */
async function testForm(): Promise<void> {
  if (!validate() || testingForm.value) return
  testingForm.value = true
  try {
    const res = await testDBConnectionDirect({
      engine: form.value.engine,
      name: form.value.name.trim(),
      host: form.value.host.trim(),
      port: Number(form.value.port),
      username: form.value.username.trim(),
      password: form.value.password,
      tls: form.value.tlsEnabled ? 'skip-verify' : '',
    })
    if (res.ok) {
      toast.success(t('settingsDatabases.testOk'), { detail: `${res.version} · ${res.latencyMs}ms` })
    } else {
      toast.error(t('settingsDatabases.testFail'), { detail: res.error })
    }
  } catch (err) {
    toast.error(t('settingsDatabases.testFail'), {
      detail: err instanceof HttpError ? (err.apiError?.message ?? t('settingsDatabases.errNetwork')) : t('settingsDatabases.errNetwork'),
    })
  } finally {
    testingForm.value = false
  }
}

async function submit(): Promise<void> {
  if (!validate()) return
  submitting.value = true
  formBanner.value = ''
  try {
    const created = await createDBInstance({
      engine: form.value.engine,
      name: form.value.name.trim(),
      host: form.value.host.trim(),
      port: Number(form.value.port),
      username: form.value.username.trim(),
      password: form.value.password,
      tls: form.value.tlsEnabled ? 'skip-verify' : '',
    })
    instances.value = [created, ...instances.value]
    form.value.password = ''
    modalOpen.value = false
  } catch (err) {
    formBanner.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('settingsDatabases.errSave', { status: err.status }))
        : t('settingsDatabases.errSaveRetry')
  } finally {
    submitting.value = false
  }
}

// ─── 编辑弹窗 ──────────────────────────────────────────────────────────────────
const editOpen = ref(false)
const editSubmitting = ref(false)
const editBanner = ref('')
const editing = ref<DBInstance | null>(null)
const editForm = ref<{ name: string; host: string; port: number; username: string; password: string; tlsEnabled: boolean }>({
  name: '', host: '', port: 3306, username: '', password: '', tlsEnabled: false,
})
const editErrors = ref<Record<string, string>>({})
const editSnapshot = ref('')

function openEdit(s: DBInstance): void {
  editing.value = s
  editForm.value = {
    name: s.name,
    host: s.host,
    port: s.port,
    username: s.username,
    password: '',
    tlsEnabled: s.tls === 'skip-verify',
  }
  editErrors.value = {}
  editSnapshot.value = JSON.stringify(editForm.value)
  editBanner.value = ''
  editOpen.value = true
}

function closeEdit(): void {
  if (editSubmitting.value) return
  editOpen.value = false
  editing.value = null
  editForm.value.password = ''
}

async function requestCloseEdit(): Promise<void> {
  if (JSON.stringify(editForm.value) !== editSnapshot.value && !(await confirmDiscard())) return
  closeEdit()
}

async function submitEdit(): Promise<void> {
  if (!editing.value) return
  editErrors.value = {}
  if (!editForm.value.name.trim()) editErrors.value.name = t('settingsDatabases.valNameRequired')
  if (!editForm.value.host.trim()) editErrors.value.host = t('settingsDatabases.valHostRequired')
  if (!editForm.value.username.trim()) editErrors.value.username = t('settingsDatabases.valUsernameRequired')
  if (Object.keys(editErrors.value).length) return
  editSubmitting.value = true
  editBanner.value = ''
  const id = editing.value.id
  try {
    const updated = await updateDBInstance(id, {
      name: editForm.value.name.trim(),
      host: editForm.value.host.trim(),
      port: Number(editForm.value.port),
      username: editForm.value.username.trim(),
      tls: editForm.value.tlsEnabled ? 'skip-verify' : '',
      // 留空 = 不轮换;填新值 = 原地轮换。
      ...(editForm.value.password ? { password: editForm.value.password } : {}),
    })
    const idx = instances.value.findIndex((p) => p.id === id)
    if (idx >= 0) instances.value[idx] = updated
    editForm.value.password = ''
    editOpen.value = false
    editing.value = null
  } catch (err) {
    editBanner.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('settingsDatabases.errSave', { status: err.status }))
        : t('settingsDatabases.errSaveRetry')
  } finally {
    editSubmitting.value = false
  }
}

// ─── 已存实例测试 ──────────────────────────────────────────────────────────────
const testingId = ref<string | null>(null)

async function testConnection(s: DBInstance): Promise<void> {
  if (testingId.value) return
  testingId.value = s.id
  try {
    const res = await testDBInstance(s.id)
    if (res.ok) {
      toast.success(t('settingsDatabases.testOk'), { detail: `${res.version} · ${res.latencyMs}ms` })
    } else {
      toast.error(t('settingsDatabases.testFail'), { detail: res.error })
    }
  } catch (err) {
    toast.error(t('settingsDatabases.testFail'), {
      detail: err instanceof HttpError ? (err.apiError?.message ?? t('settingsDatabases.errNetwork')) : t('settingsDatabases.errNetwork'),
    })
  } finally {
    testingId.value = null
  }
}

// ─── 删除 ──────────────────────────────────────────────────────────────────────
const deleteOpen = ref(false)
const deleting = ref<DBInstance | null>(null)
const deleteSubmitting = ref(false)
const deleteBanner = ref('')

function openDelete(s: DBInstance): void {
  deleting.value = s
  deleteBanner.value = ''
  deleteOpen.value = true
}
function closeDelete(): void {
  if (deleteSubmitting.value) return
  deleteOpen.value = false
  deleting.value = null
}
async function confirmDelete(): Promise<void> {
  if (!deleting.value) return
  deleteSubmitting.value = true
  deleteBanner.value = ''
  const id = deleting.value.id
  try {
    await deleteDBInstance(id)
    instances.value = instances.value.filter((p) => p.id !== id)
    deleteOpen.value = false
    deleting.value = null
  } catch (err) {
    deleteBanner.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('settingsDatabases.errDelete', { status: err.status }))
        : t('settingsDatabases.errNetwork')
  } finally {
    deleteSubmitting.value = false
  }
}
</script>

<template>
  <div class="db-root">
    <div class="section-head">
      <div class="section-head-text">
        <h2 class="section-title">{{ t('settingsDatabases.title') }}</h2>
        <p class="section-desc">{{ t('settingsDatabases.desc') }}</p>
      </div>
      <button class="btn-primary" :disabled="loadState === 'loading'" @click="openAdd">
        <NIcon :size="14"><Plus /></NIcon>
        {{ t('settingsDatabases.addInstance') }}
      </button>
    </div>

    <div v-if="loadState === 'error'" class="banner banner--error" role="alert">
      <span>⚠ {{ loadError }}</span>
      <button class="banner-retry" @click="load">↻ {{ t('settingsDatabases.retry') }}</button>
    </div>

    <div class="panel">
      <div class="panel-head">
        <span>{{ t('settingsDatabases.panelTitle') }}</span>
        <span v-if="loadState === 'idle'" class="panel-meta">{{ t('settingsDatabases.countLabel', { n: instances.length }) }}</span>
      </div>

      <div v-if="loadState === 'loading'" class="state-msg">{{ t('common.refresh') }}…</div>

      <div v-else-if="loadState === 'idle' && instances.length === 0" class="empty-state">
        <div class="empty-icon" aria-hidden="true"><NIcon :size="22"><Database /></NIcon></div>
        <p class="empty-label">{{ t('settingsDatabases.emptyLabel') }}</p>
        <p class="empty-hint">{{ t('settingsDatabases.emptyHint') }}</p>
        <button class="btn-primary" @click="openAdd">+ {{ t('settingsDatabases.addFirst') }}</button>
      </div>

      <template v-else-if="loadState === 'idle'">
        <div class="db-row db-row--head" aria-hidden="true">
          <span>{{ t('settingsDatabases.colEngine') }}</span>
          <span>{{ t('settingsDatabases.colName') }}</span>
          <span>{{ t('settingsDatabases.colSource') }}</span>
          <span>{{ t('settingsDatabases.colMode') }}</span>
          <span />
        </div>
        <div v-for="s in instances" :key="s.id" class="db-row">
          <span class="type-badge" :class="s.engine === 'mysql' ? 'type-badge--mysql' : 'type-badge--pg'">{{ engineLabels(s.engine) }}</span>
          <div class="db-name-cell">
            <strong class="db-name">{{ s.name }}</strong>
            <span class="db-ep mono">{{ s.host }}:{{ s.port }} · {{ s.username }}</span>
          </div>
          <span class="src-badge" :class="s.source === 'appstore' ? 'src-badge--app' : ''">
            {{ s.source === 'appstore' ? t('settingsDatabases.srcAppstore') : t('settingsDatabases.srcManual') }}
          </span>
          <span class="mode-state" :class="s.readonly ? 'mode-state--ro' : 'mode-state--rw'">
            {{ s.readonly ? t('settingsDatabases.modeReadonly') : t('settingsDatabases.modeWritable') }}
          </span>
          <span class="db-ops">
            <button class="op-btn" :title="t('settingsDatabases.editTitle', { name: s.name })" @click="openEdit(s)">
              <NIcon :size="13"><Pencil /></NIcon>
              <span class="op-btn-txt">{{ t('settingsDatabases.editBtn') }}</span>
            </button>
            <button class="op-btn" :disabled="testingId === s.id" :title="t('settingsDatabases.test')" @click="testConnection(s)">
              <NIcon :size="13"><PlugConnected /></NIcon>
              <span class="op-btn-txt">{{ testingId === s.id ? t('settingsDatabases.testing') : t('settingsDatabases.test') }}</span>
            </button>
            <router-link class="op-btn" :to="`/databases?instance=${encodeURIComponent(s.id)}`" :title="t('settingsDatabases.manage')">
              <NIcon :size="13"><ExternalLink /></NIcon>
              <span class="op-btn-txt">{{ t('settingsDatabases.manage') }}</span>
            </router-link>
            <button class="op-btn op-btn--danger" :title="t('settingsDatabases.deleteTitle', { name: s.name })" @click="openDelete(s)">
              <NIcon :size="13"><Trash /></NIcon>
            </button>
          </span>
        </div>
      </template>
    </div>
  </div>

  <!-- add modal -->
  <Teleport to="body">
    <div v-if="modalOpen" class="modal-scrim" role="dialog" :aria-label="t('settingsDatabases.addTitle')" aria-modal="true" @keydown.esc="requestClose">
      <div class="modal">
        <div class="modal-head">
          <div class="modal-icon" aria-hidden="true"><NIcon :size="18"><Database /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('settingsDatabases.addTitle') }}</h3>
            <p class="modal-sub">{{ t('settingsDatabases.addSub') }}</p>
          </div>
          <button class="modal-close" :aria-label="t('settingsDatabases.closeDialog')" :disabled="submitting" @click="requestClose">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6 6 18M6 6l12 12"/></svg>
          </button>
        </div>

        <div v-if="formBanner" class="banner banner--error modal-banner" role="alert">{{ formBanner }}</div>

        <form class="modal-form" novalidate @submit.prevent="submit">
          <div class="field">
            <label class="field-label">{{ t('settingsDatabases.fieldEngine') }}</label>
            <div class="segmented" role="group">
              <button type="button" class="seg-item" :class="{ 'seg-item--active': form.engine === 'mysql' }" :disabled="submitting" @click="switchEngine('mysql')">MySQL</button>
              <button type="button" class="seg-item" :class="{ 'seg-item--active': form.engine === 'postgres' }" :disabled="submitting" @click="switchEngine('postgres')">PostgreSQL</button>
            </div>
          </div>

          <div class="field">
            <label class="field-label" for="db-name">{{ t('settingsDatabases.fieldName') }}</label>
            <input id="db-name" v-model="form.name" class="field-input" :class="{ 'field-input--error': errors.name }" type="text" :placeholder="t('settingsDatabases.namePlaceholder')" :disabled="submitting" autocomplete="off" @input="errors.name = ''" />
            <span v-if="errors.name" class="field-error" role="alert">{{ errors.name }}</span>
          </div>

          <div class="field-grid">
            <div class="field">
              <label class="field-label" for="db-host">{{ t('settingsDatabases.fieldHost') }}</label>
              <input id="db-host" v-model="form.host" class="field-input field-input--mono" :class="{ 'field-input--error': errors.host }" type="text" placeholder="10.0.0.8" :disabled="submitting" autocomplete="off" spellcheck="false" @input="errors.host = ''" />
              <span v-if="errors.host" class="field-error" role="alert">{{ errors.host }}</span>
            </div>
            <div class="field">
              <label class="field-label" for="db-port">{{ t('settingsDatabases.fieldPort') }}</label>
              <input id="db-port" v-model="form.port" class="field-input field-input--mono" :class="{ 'field-input--error': errors.port }" type="number" min="1" max="65535" :disabled="submitting" @input="errors.port = ''" />
              <span v-if="errors.port" class="field-error" role="alert">{{ errors.port }}</span>
            </div>
          </div>

          <div class="field-grid">
            <div class="field">
              <label class="field-label" for="db-username">{{ t('settingsDatabases.fieldUsername') }}</label>
              <input id="db-username" v-model="form.username" class="field-input field-input--mono" :class="{ 'field-input--error': errors.username }" type="text" :disabled="submitting" autocomplete="off" spellcheck="false" @input="errors.username = ''" />
              <span v-if="errors.username" class="field-error" role="alert">{{ errors.username }}</span>
            </div>
            <div class="field">
              <label class="field-label" for="db-password">{{ t('settingsDatabases.fieldPassword') }}</label>
              <input id="db-password" v-model="form.password" class="field-input field-input--mono" :class="{ 'field-input--error': errors.password }" type="password" :placeholder="t('settingsDatabases.passwordPlaceholder')" :disabled="submitting" autocomplete="new-password" @input="errors.password = ''" />
              <span v-if="errors.password" class="field-error" role="alert">{{ errors.password }}</span>
            </div>
          </div>

          <label class="check-field">
            <input v-model="form.tlsEnabled" type="checkbox" :disabled="submitting" />
            <span>{{ t('settingsDatabases.tlsOption') }}</span>
          </label>

          <div class="modal-footer">
            <button type="button" class="btn-secondary" :disabled="testingForm" @click="testForm">
              <span v-if="testingForm" class="spinner spinner--ink" aria-hidden="true" />
              {{ testingForm ? t('settingsDatabases.testing') : t('settingsDatabases.testFirst') }}
            </button>
            <span class="footer-spacer" />
            <button type="button" class="btn-secondary" :disabled="submitting" @click="requestClose">{{ t('settingsDatabases.cancel') }}</button>
            <button type="submit" class="btn-primary" :disabled="submitting" :aria-busy="submitting">
              <span v-if="submitting" class="spinner" aria-hidden="true" />
              {{ submitting ? t('settingsDatabases.saving') : t('settingsDatabases.create') }}
            </button>
          </div>
        </form>
      </div>
    </div>
  </Teleport>

  <!-- edit modal -->
  <Teleport to="body">
    <div v-if="editOpen && editing" class="modal-scrim" role="dialog" :aria-label="t('settingsDatabases.editTitle', { name: editing.name })" aria-modal="true" @keydown.esc="requestCloseEdit">
      <div class="modal">
        <div class="modal-head">
          <div class="modal-icon" aria-hidden="true"><NIcon :size="18"><Pencil /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('settingsDatabases.editTitle', { name: editing.name }) }}</h3>
            <p class="modal-sub">{{ t('settingsDatabases.editSub') }}</p>
          </div>
          <button class="modal-close" :aria-label="t('settingsDatabases.closeDialog')" :disabled="editSubmitting" @click="requestCloseEdit">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6 6 18M6 6l12 12"/></svg>
          </button>
        </div>

        <div v-if="editBanner" class="banner banner--error modal-banner" role="alert">{{ editBanner }}</div>

        <form class="modal-form" novalidate @submit.prevent="submitEdit">
          <div class="field">
            <label class="field-label" for="edit-db-name">{{ t('settingsDatabases.fieldName') }}</label>
            <input id="edit-db-name" v-model="editForm.name" class="field-input" :class="{ 'field-input--error': editErrors.name }" type="text" :disabled="editSubmitting" autocomplete="off" @input="editErrors.name = ''" />
            <span v-if="editErrors.name" class="field-error" role="alert">{{ editErrors.name }}</span>
          </div>

          <div class="field-grid">
            <div class="field">
              <label class="field-label" for="edit-db-host">{{ t('settingsDatabases.fieldHost') }}</label>
              <input id="edit-db-host" v-model="editForm.host" class="field-input field-input--mono" :class="{ 'field-input--error': editErrors.host }" type="text" :disabled="editSubmitting" autocomplete="off" spellcheck="false" @input="editErrors.host = ''" />
              <span v-if="editErrors.host" class="field-error" role="alert">{{ editErrors.host }}</span>
            </div>
            <div class="field">
              <label class="field-label" for="edit-db-port">{{ t('settingsDatabases.fieldPort') }}</label>
              <input id="edit-db-port" v-model="editForm.port" class="field-input field-input--mono" type="number" min="1" max="65535" :disabled="editSubmitting" />
            </div>
          </div>

          <div class="field">
            <label class="field-label" for="edit-db-username">{{ t('settingsDatabases.fieldUsername') }}</label>
            <input id="edit-db-username" v-model="editForm.username" class="field-input field-input--mono" :class="{ 'field-input--error': editErrors.username }" type="text" :disabled="editSubmitting" autocomplete="off" spellcheck="false" @input="editErrors.username = ''" />
            <span v-if="editErrors.username" class="field-error" role="alert">{{ editErrors.username }}</span>
          </div>

          <div class="field">
            <label class="field-label" for="edit-db-password">{{ t('settingsDatabases.fieldPasswordRotate') }}</label>
            <input id="edit-db-password" v-model="editForm.password" class="field-input field-input--mono" type="password" :placeholder="t('settingsDatabases.passwordRotatePlaceholder')" :disabled="editSubmitting" autocomplete="new-password" />
            <span class="field-hint">{{ t('settingsDatabases.passwordRotateHint') }}</span>
          </div>

          <label class="check-field">
            <input v-model="editForm.tlsEnabled" type="checkbox" :disabled="editSubmitting" />
            <span>{{ t('settingsDatabases.tlsOption') }}</span>
          </label>

          <div class="modal-footer">
            <button type="button" class="btn-secondary" :disabled="editSubmitting" @click="requestCloseEdit">{{ t('settingsDatabases.cancel') }}</button>
            <button type="submit" class="btn-primary" :disabled="editSubmitting" :aria-busy="editSubmitting">
              <span v-if="editSubmitting" class="spinner" aria-hidden="true" />
              {{ editSubmitting ? t('settingsDatabases.saving') : t('settingsDatabases.save') }}
            </button>
          </div>
        </form>
      </div>
    </div>
  </Teleport>

  <!-- delete modal -->
  <Teleport to="body">
    <div v-if="deleteOpen && deleting" class="modal-scrim" role="dialog" :aria-label="t('settingsDatabases.deleteConfirmTitle')" aria-modal="true" @keydown.esc="closeDelete">
      <div class="modal modal--sm">
        <div class="modal-head">
          <div class="modal-icon modal-icon--danger" aria-hidden="true"><NIcon :size="18"><Trash /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('settingsDatabases.deleteConfirmTitle') }}</h3>
            <p class="modal-sub">{{ t('settingsDatabases.deleteHint') }}</p>
          </div>
          <button class="modal-close" :aria-label="t('settingsDatabases.closeDialog')" :disabled="deleteSubmitting" @click="closeDelete">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6 6 18M6 6l12 12"/></svg>
          </button>
        </div>
        <div class="modal-body">
          <p class="delete-text">
            {{ t('settingsDatabases.deleteConfirmPrefix') }}
            <strong class="delete-name">{{ deleting.name }}</strong>
            {{ t('settingsDatabases.deleteConfirmSuffix') }}
          </p>
          <div v-if="deleteBanner" class="banner banner--error modal-banner" role="alert">{{ deleteBanner }}</div>
        </div>
        <div class="modal-footer modal-footer--standalone">
          <button type="button" class="btn-secondary" :disabled="deleteSubmitting" @click="closeDelete">{{ t('settingsDatabases.cancel') }}</button>
          <button type="button" class="btn-danger" :disabled="deleteSubmitting" :aria-busy="deleteSubmitting" @click="confirmDelete">
            <span v-if="deleteSubmitting" class="spinner spinner--red" aria-hidden="true" />
            {{ deleteSubmitting ? t('settingsDatabases.deleting') : t('settingsDatabases.confirmDelete') }}
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.db-root {
  display: flex;
  flex-direction: column;
  gap: var(--card-gap);
}

.section-head {
  display: flex;
  align-items: flex-start;
  gap: 16px;
}
.section-head-text {
  flex: 1;
}
.section-title {
  font-size: 1.12rem;
  font-weight: 600;
  letter-spacing: -0.01em;
  color: var(--color-text);
}
.section-desc {
  font-size: 0.82rem;
  color: var(--color-faint);
  margin-top: 4px;
  max-width: 68ch;
  line-height: 1.55;
}

.panel {
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  box-shadow: var(--shadow);
  overflow: hidden;
  animation: panel-in 0.45s var(--ease-out-expo) both;
}
@keyframes panel-in {
  from { opacity: 0; transform: translateY(13px); }
  to { opacity: 1; transform: none; }
}
.panel-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 13px 18px;
  border-bottom: 1px solid var(--color-border);
  font-size: 0.86rem;
  font-weight: 600;
  color: var(--color-text);
}
.panel-meta {
  margin-left: auto;
  font-size: 0.74rem;
  color: var(--color-faint);
  font-weight: 400;
}
.state-msg {
  padding: 28px 18px;
  font-size: 0.84rem;
  color: var(--color-faint);
  text-align: center;
}

.empty-state {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 48px 32px;
  text-align: center;
}
.empty-icon {
  width: 52px;
  height: 52px;
  border-radius: var(--rounded-xl);
  background: var(--color-inset);
  border: 1.5px dashed var(--color-border-strong);
  display: grid;
  place-items: center;
  color: var(--color-dim);
  margin-bottom: 4px;
}
.empty-label {
  font-size: 0.92rem;
  font-weight: 600;
  color: var(--color-text);
}
.empty-hint {
  font-size: 0.8rem;
  color: var(--color-faint);
  max-width: 52ch;
  line-height: 1.55;
  margin-bottom: 6px;
}

.db-row {
  display: grid;
  grid-template-columns: 100px minmax(140px, 1.5fr) 110px 100px auto;
  align-items: center;
  gap: 14px;
  padding: 13px 18px;
  border-bottom: 1px solid var(--color-border);
  transition: background-color var(--duration-fast);
}
.db-row:last-child {
  border-bottom: none;
}
.db-row:not(.db-row--head):hover {
  background: var(--color-inset);
}
.db-row--head {
  height: 34px;
  padding-top: 0;
  padding-bottom: 0;
  font-size: 0.71rem;
  color: var(--color-faint);
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.04em;
  background: var(--color-card-2);
  pointer-events: none;
}

.type-badge {
  display: inline-flex;
  align-items: center;
  justify-self: start;
  font-size: 0.72rem;
  font-weight: 600;
  padding: 3px 9px;
  border-radius: var(--rounded-full);
  border: 1px solid var(--color-border-strong);
  background: var(--color-inset);
  color: var(--color-dim);
  white-space: nowrap;
}
.type-badge--mysql {
  border-color: var(--color-amber-line);
  background: var(--color-amber-soft);
  color: var(--color-amber);
}
.type-badge--pg {
  border-color: var(--color-primary-soft);
  background: var(--color-primary-soft);
  color: var(--color-primary);
}

.db-name-cell {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.db-name {
  font-size: 0.85rem;
  font-weight: 500;
  color: var(--color-text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.db-ep {
  font-size: 0.72rem;
  color: var(--color-faint);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.mono {
  font-family: var(--font-mono);
}

.src-badge {
  justify-self: start;
  font-size: 0.72rem;
  font-weight: 500;
  padding: 3px 9px;
  border-radius: var(--rounded-full);
  border: 1px solid var(--color-border-strong);
  color: var(--color-faint);
  white-space: nowrap;
}
.src-badge--app {
  border-color: var(--color-amber-line);
  background: var(--color-amber-soft);
  color: var(--color-amber);
}

.mode-state {
  font-size: 0.74rem;
  font-weight: 600;
  white-space: nowrap;
}
.mode-state--ro {
  color: var(--color-faint);
}
.mode-state--rw {
  color: var(--color-green);
}

.db-ops {
  display: flex;
  justify-content: flex-end;
  gap: 6px;
}
.op-btn {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  height: 28px;
  padding: 0 10px;
  border: 1px solid var(--color-border);
  background: transparent;
  color: var(--color-dim);
  border-radius: var(--rounded-md);
  cursor: pointer;
  font-size: 0.74rem;
  font-weight: 500;
  text-decoration: none;
  transition:
    color var(--duration-fast),
    border-color var(--duration-fast),
    background-color var(--duration-fast);
}
.op-btn:hover:not(:disabled) {
  color: var(--color-primary);
  border-color: var(--color-primary);
}
.op-btn:disabled {
  opacity: 0.55;
  cursor: progress;
}
.op-btn--danger {
  padding: 0 8px;
}
.op-btn--danger:hover:not(:disabled) {
  color: var(--color-red);
  border-color: var(--color-red-line);
  background: var(--color-red-soft);
}
.op-btn:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}

.banner {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 11px 14px;
  border-radius: var(--rounded);
  font-size: 0.83rem;
  line-height: 1.5;
}
.banner--error {
  background: var(--color-red-soft);
  border: 1px solid var(--color-red-line);
  color: var(--color-red);
}
.banner-retry {
  margin-left: auto;
  flex-shrink: 0;
  background: none;
  border: none;
  color: var(--color-red);
  font-size: 0.83rem;
  font-weight: 600;
  cursor: pointer;
  padding: 0;
  text-decoration: underline;
  text-underline-offset: 2px;
}

.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 34px;
  padding: 0 15px;
  border: none;
  background: var(--color-primary);
  color: #fff;
  font-family: var(--font-sans);
  font-size: 0.83rem;
  font-weight: 600;
  border-radius: var(--rounded);
  cursor: pointer;
  box-shadow: 0 5px 16px var(--color-primary-soft);
  transition:
    background-color var(--duration-fast),
    transform var(--duration-fast);
  white-space: nowrap;
  flex-shrink: 0;
}
.btn-primary:hover:not(:disabled) {
  background: var(--color-primary-press);
  transform: translateY(-1px);
}
.btn-primary:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 3px;
}
.btn-primary:disabled {
  opacity: 0.45;
  cursor: not-allowed;
  transform: none;
  box-shadow: none;
}
.btn-secondary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 34px;
  padding: 0 15px;
  background: var(--color-card-2);
  color: var(--color-text);
  border: 1px solid var(--color-border-strong);
  font-family: var(--font-sans);
  font-size: 0.83rem;
  font-weight: 500;
  border-radius: var(--rounded);
  cursor: pointer;
  transition: border-color var(--duration-fast);
}
.btn-secondary:hover:not(:disabled) {
  border-color: var(--color-faint);
}
.btn-secondary:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.btn-danger {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 34px;
  padding: 0 15px;
  background: var(--color-red-soft);
  color: var(--color-red);
  border: 1px solid var(--color-red-line);
  font-family: var(--font-sans);
  font-size: 0.83rem;
  font-weight: 600;
  border-radius: var(--rounded);
  cursor: pointer;
  transition: background-color var(--duration-fast), transform var(--duration-fast);
}
.btn-danger:hover:not(:disabled) {
  background: oklch(62% 0.18 22 / 0.25);
  transform: translateY(-1px);
}
.btn-danger:disabled {
  opacity: 0.5;
  cursor: not-allowed;
  transform: none;
}

.modal-scrim {
  position: fixed;
  inset: 0;
  background: oklch(0% 0 0 / 0.62);
  display: grid;
  place-items: center;
  z-index: 100;
  padding: 24px;
  animation: scrim-in var(--duration-fast) ease both;
}
@keyframes scrim-in {
  from { opacity: 0; }
  to { opacity: 1; }
}
.modal {
  width: 100%;
  max-width: 620px;
  max-height: min(90vh, calc(100dvh - 48px));
  display: flex;
  flex-direction: column;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-xl);
  box-shadow: var(--shadow-modal);
  overflow: hidden;
  animation: modal-in 0.35s var(--ease-out-expo) both;
}
.modal--sm {
  max-width: 520px;
}
@keyframes modal-in {
  from { opacity: 0; transform: translateY(14px) scale(0.98); }
  to { opacity: 1; transform: none; }
}
.modal-head {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 20px 20px 16px;
  border-bottom: 1px solid var(--color-border);
}
.modal-icon {
  width: 36px;
  height: 36px;
  border-radius: var(--rounded-lg);
  background: var(--color-primary-soft);
  color: var(--color-primary);
  display: grid;
  place-items: center;
  flex-shrink: 0;
}
.modal-icon--danger {
  background: var(--color-red-soft);
  color: var(--color-red);
}
.modal-title {
  font-size: 1rem;
  font-weight: 600;
  color: var(--color-text);
  margin-top: 2px;
  letter-spacing: -0.01em;
}
.modal-sub {
  font-size: 0.78rem;
  color: var(--color-faint);
  margin-top: 3px;
  line-height: 1.4;
}
.modal-close {
  margin-left: auto;
  flex-shrink: 0;
  width: 30px;
  height: 30px;
  border-radius: var(--rounded-md);
  border: none;
  background: transparent;
  color: var(--color-faint);
  cursor: pointer;
  display: grid;
  place-items: center;
  transition: color var(--duration-fast), background-color var(--duration-fast);
}
.modal-close:hover {
  color: var(--color-text);
  background: var(--color-inset);
}
.modal-close:disabled {
  opacity: 0.4;
  cursor: not-allowed;
}
.modal-banner {
  margin: 16px 20px 0;
  border-radius: var(--rounded);
}
.modal-form {
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 16px;
  min-height: 0;
  max-height: min(66vh, 560px);
  overflow-y: auto;
}
.modal-body {
  padding: 20px;
}
.modal-footer {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  padding-top: 4px;
}
/* footer 在滚动表单内:钉在滚动区底部,保存按钮始终可见。 */
.modal-form > .modal-footer {
  position: sticky;
  bottom: 0;
  padding-top: 12px;
  background: var(--color-card);
  border-top: 1px solid var(--color-border);
}
.modal-footer--standalone {
  padding: 4px 20px 20px;
}
.footer-spacer {
  flex: 1;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.field-grid {
  display: grid;
  grid-template-columns: 1.6fr 1fr;
  gap: 14px;
}
.field-label {
  font-size: 0.78rem;
  font-weight: 500;
  color: var(--color-dim);
}
.field-input {
  width: 100%;
  height: 38px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  padding: 0 12px;
  color: var(--color-text);
  font-family: var(--font-sans);
  font-size: 0.86rem;
  transition: border-color var(--duration-fast), box-shadow var(--duration-fast);
  box-sizing: border-box;
}
.field-input::placeholder {
  color: var(--color-faint);
}
.field-input:focus {
  outline: none;
  border-color: var(--color-primary);
  box-shadow: 0 0 0 3px var(--color-primary-soft);
}
.field-input--error {
  border-color: var(--color-red);
}
.field-input--error:focus {
  border-color: var(--color-red);
  box-shadow: 0 0 0 3px var(--color-red-soft);
}
.field-input:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.field-input--mono {
  font-family: var(--font-mono);
  font-size: 0.8rem;
}
.field-error {
  font-size: 0.76rem;
  color: var(--color-red);
  line-height: 1.4;
}
.field-hint {
  font-size: 0.74rem;
  color: var(--color-faint);
  line-height: 1.4;
}

.segmented {
  display: inline-flex;
  background: var(--color-inset);
  border-radius: var(--rounded);
  padding: 3px;
  gap: 2px;
  width: 100%;
  box-sizing: border-box;
}
.seg-item {
  flex: 1;
  height: 30px;
  border: none;
  background: transparent;
  color: var(--color-dim);
  font-family: var(--font-sans);
  font-size: 0.8rem;
  font-weight: 500;
  border-radius: var(--rounded-md);
  cursor: pointer;
  transition: color var(--duration-fast), background-color var(--duration-fast), box-shadow var(--duration-fast);
}
.seg-item:hover:not(:disabled) {
  color: var(--color-text);
  background: oklch(100% 0 0 / 0.04);
}
.seg-item--active {
  background: var(--color-card);
  color: var(--color-text);
  box-shadow: var(--shadow);
}
.seg-item:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.check-field {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 0.8rem;
  color: var(--color-dim);
  cursor: pointer;
}
.check-field input[type='checkbox'] {
  accent-color: var(--color-primary);
}

.delete-text {
  font-size: 0.86rem;
  color: var(--color-dim);
  line-height: 1.55;
}
.delete-name {
  color: var(--color-text);
  font-weight: 600;
}

.spinner {
  display: inline-block;
  width: 13px;
  height: 13px;
  border: 2px solid rgba(255, 255, 255, 0.35);
  border-top-color: #fff;
  border-radius: var(--rounded-full);
  animation: spin 0.7s linear infinite;
  flex-shrink: 0;
}
.spinner--red {
  border-color: oklch(69% 0.17 22 / 0.3);
  border-top-color: var(--color-red);
}
.spinner--ink {
  border-color: oklch(0% 0 0 / 0.15);
  border-top-color: var(--color-dim);
}
@keyframes spin {
  to { transform: rotate(360deg); }
}

@media (max-width: 640px) {
  .db-row {
    grid-template-columns: 1fr 1fr;
    gap: 8px 12px;
  }
  .db-row--head {
    display: none;
  }
  .db-ops {
    grid-column: 1 / -1;
    justify-content: flex-start;
  }
  .field-grid {
    grid-template-columns: 1fr;
  }
}

@media (prefers-reduced-motion: reduce) {
  *, *::before, *::after {
    animation-duration: 0.01ms !important;
    animation-iteration-count: 1 !important;
    transition-duration: 0.01ms !important;
  }
}
</style>
