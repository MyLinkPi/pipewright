<script setup lang="ts">
/*
  SettingsObjectStorage.vue — 对象存储管理（阿里云 OSS / S3 兼容的 RustFS）。

  为流水线 `oss_upload` 节点提供上传目标：一份配置 = 一个对象存储账户（Endpoint + Bucket）。
  凭据按机密性拆开：AccessKeyId 非机密（明文可回显可编辑）；AccessKeySecret 只写不读 ——
  存入保险库，列表只显示「已配置 / 未配置」，绝不回显（可在编辑中轮换）。
  RustFS 配置也可由应用商店部署 RustFS 成功后自动登记（尽力而为，含默认建桶）。

  - 列表：类型徽章、名称（附 Endpoint + 协议标签）、Bucket、凭据状态；每行「编辑」+「测试连接」+ 删除。
  - 添加弹窗：类型 + 名称 + Endpoint + HTTPS 开关 + Bucket（可拉取候选下拉）+ AccessKeyId + AccessKeySecret（必填）。
  - 编辑弹窗：改名称 / Endpoint / HTTPS 开关 / Bucket / AccessKeyId，轮换 Secret（留空不改动）。
  - 删除二次确认（连带删除其保险库凭据）。
  数据来自 GET /api/object-stores（只读聚合，从不含 Secret）。
*/
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useDirtyGuard } from '../../composables/useDirtyGuard'
import { NIcon } from 'naive-ui'
import { CloudUpload, Plus, Trash, CircleCheck, CircleX, PlugConnected, Pencil, Folder } from '@vicons/tabler'
import {
  listObjectStores,
  createObjectStore,
  updateObjectStore,
  deleteObjectStore,
  testObjectStore,
  listObjectStoreBuckets,
  probeObjectStoreBuckets,
  type ObjectStore,
  type ObjectStoreType,
  type ObjectStoreBucket,
  type CreateObjectStoreInput,
} from '../../api/objectStores'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'

const { t } = useI18n()
const { confirmDiscard } = useDirtyGuard()
const toast = useToast()
const router = useRouter()

// 浏览桶内容:跳顶级浏览页并预选该实例(只读,marker 游标分页)。
function browseStore(s: ObjectStore): void {
  void router.push({ path: '/object-storage', query: { store: s.id } })
}

// ─── 列表加载 ──────────────────────────────────────────────────────────────────
type LoadState = 'idle' | 'loading' | 'error'
const loadState = ref<LoadState>('idle')
const loadError = ref('')
const stores = ref<ObjectStore[]>([])

// 类型分段选择器:阿里云 OSS + S3 兼容的 RustFS(自建,应用商店部署后自动登记)。
const STORE_TYPES: ObjectStoreType[] = ['aliyun_oss', 'rustfs']
const typeLabels = computed<Record<ObjectStoreType, string>>(() => ({
  aliyun_oss: t('settingsObjectStorage.typeAliyunOss'),
  rustfs: t('settingsObjectStorage.typeRustfs'),
}))

/** Endpoint 占位符按类型切换（RustFS 是 host:port 形式的自建接入点）。 */
function endpointPlaceholderFor(type: ObjectStoreType | undefined): string {
  return type === 'rustfs'
    ? t('settingsObjectStorage.endpointPlaceholderRustfs')
    : t('settingsObjectStorage.endpointPlaceholder')
}

/** Endpoint 提示按类型切换。 */
function endpointHintFor(type: ObjectStoreType | undefined): string {
  return type === 'rustfs'
    ? t('settingsObjectStorage.endpointHintRustfs')
    : t('settingsObjectStorage.endpointHint')
}

/** HTTPS 开关的类型默认：公云 OSS 必走 TLS；RustFS 自建内网默认明文（可手动开启）。 */
function defaultUseSSLFor(type: ObjectStoreType | undefined): boolean {
  return type !== 'rustfs'
}

async function load(): Promise<void> {
  loadState.value = 'loading'
  loadError.value = ''
  try {
    stores.value = await listObjectStores()
    loadState.value = 'idle'
  } catch (err) {
    loadState.value = 'error'
    loadError.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('settingsObjectStorage.errLoad', { status: err.status }))
        : t('settingsObjectStorage.errNetwork')
  }
}

onMounted(load)

// ─── 添加弹窗 ──────────────────────────────────────────────────────────────────
const modalOpen = ref(false)
const submitting = ref(false)
const formBanner = ref('')
const form = ref<{
  type: ObjectStoreType
  name: string
  endpoint: string
  useSSL: boolean
  bucket: string
  pathPrefix: string
  accessKeyId: string
  secret: string
}>({ type: 'aliyun_oss', name: '', endpoint: '', useSSL: true, bucket: '', pathPrefix: '', accessKeyId: '', secret: '' })
const errors = ref<{ name: string; endpoint: string; bucket: string; pathPrefix: string; accessKeyId: string; secret: string }>({
  name: '',
  endpoint: '',
  bucket: '',
  pathPrefix: '',
  accessKeyId: '',
  secret: '',
})
const formSnapshot = ref('')

/** 类型切换时重置该类型的默认传输协议（OSS→HTTPS / RustFS→HTTP），并清空过时的桶候选。 */
function switchType(opt: ObjectStoreType): void {
  if (form.value.type === opt) return
  form.value.type = opt
  form.value.useSSL = defaultUseSSLFor(opt)
  bucketOptions.value = []
}

function openAdd(): void {
  form.value = { type: 'aliyun_oss', name: '', endpoint: '', useSSL: true, bucket: '', pathPrefix: '', accessKeyId: '', secret: '' }
  errors.value = { name: '', endpoint: '', bucket: '', pathPrefix: '', accessKeyId: '', secret: '' }
  formBanner.value = ''
  bucketOptions.value = []
  modalOpen.value = true
  formSnapshot.value = JSON.stringify(form.value)
}

function closeModal(): void {
  if (submitting.value) return
  modalOpen.value = false
  form.value.secret = ''
}

/** Shared close path for ✕ / 取消 / ESC: confirm before discarding a dirty form. */
async function requestClose(): Promise<void> {
  if (JSON.stringify(form.value) !== formSnapshot.value && !(await confirmDiscard())) return
  closeModal()
}

function validate(): boolean {
  errors.value = { name: '', endpoint: '', bucket: '', pathPrefix: '', accessKeyId: '', secret: '' }
  let ok = true
  if (!form.value.name.trim()) {
    errors.value.name = t('settingsObjectStorage.valNameRequired')
    ok = false
  }
  if (!form.value.endpoint.trim()) {
    errors.value.endpoint = t('settingsObjectStorage.valEndpointRequired')
    ok = false
  }
  if (!form.value.bucket.trim()) {
    errors.value.bucket = t('settingsObjectStorage.valBucketRequired')
    ok = false
  }
  if (!form.value.accessKeyId.trim()) {
    errors.value.accessKeyId = t('settingsObjectStorage.valAccessKeyIdRequired')
    ok = false
  }
  if (!form.value.secret) {
    errors.value.secret = t('settingsObjectStorage.valSecretRequired')
    ok = false
  }
  return ok
}

async function submit(): Promise<void> {
  if (!validate()) return
  submitting.value = true
  formBanner.value = ''
  try {
    const payload: CreateObjectStoreInput = {
      type: form.value.type,
      name: form.value.name.trim(),
      endpoint: form.value.endpoint.trim(),
      useSSL: form.value.useSSL,
      bucket: form.value.bucket.trim(),
      pathPrefix: form.value.pathPrefix.trim(),
      accessKeyId: form.value.accessKeyId.trim(),
      secret: form.value.secret,
    }
    const created = await createObjectStore(payload)
    stores.value = [created, ...stores.value]
    form.value.secret = ''
    modalOpen.value = false
  } catch (err) {
    formBanner.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('settingsObjectStorage.errSave', { status: err.status }))
        : t('settingsObjectStorage.errSaveRetry')
  } finally {
    submitting.value = false
  }
}

// ─── 编辑弹窗(名称 / Endpoint / HTTPS / Bucket / AccessKeyId + 轮换 Secret)────
const editOpen = ref(false)
const editSubmitting = ref(false)
const editBanner = ref('')
const editing = ref<ObjectStore | null>(null)
const editForm = ref<{ name: string; endpoint: string; useSSL: boolean; bucket: string; pathPrefix: string; accessKeyId: string; secret: string }>({
  name: '',
  endpoint: '',
  useSSL: false,
  bucket: '',
  pathPrefix: '',
  accessKeyId: '',
  secret: '',
})
const editErrors = ref<{ name: string; endpoint: string; bucket: string; accessKeyId: string }>({
  name: '',
  endpoint: '',
  bucket: '',
  accessKeyId: '',
})
const editSnapshot = ref('')

function openEdit(s: ObjectStore): void {
  editing.value = s
  editForm.value = {
    name: s.name,
    endpoint: s.endpoint,
    useSSL: s.useSSL,
    bucket: s.bucket,
    pathPrefix: s.pathPrefix,
    accessKeyId: s.accessKeyId,
    secret: '',
  }
  editErrors.value = { name: '', endpoint: '', bucket: '', accessKeyId: '' }
  editSnapshot.value = JSON.stringify(editForm.value)
  editBanner.value = ''
  editBucketOptions.value = []
  editOpen.value = true
}

function closeEdit(): void {
  if (editSubmitting.value) return
  editOpen.value = false
  editing.value = null
  editForm.value.secret = ''
}

async function requestCloseEdit(): Promise<void> {
  if (JSON.stringify(editForm.value) !== editSnapshot.value && !(await confirmDiscard())) return
  closeEdit()
}

function validateEdit(): boolean {
  editErrors.value = { name: '', endpoint: '', bucket: '', accessKeyId: '' }
  let ok = true
  if (!editForm.value.name.trim()) {
    editErrors.value.name = t('settingsObjectStorage.valNameRequired')
    ok = false
  }
  if (!editForm.value.endpoint.trim()) {
    editErrors.value.endpoint = t('settingsObjectStorage.valEndpointRequired')
    ok = false
  }
  if (!editForm.value.bucket.trim()) {
    editErrors.value.bucket = t('settingsObjectStorage.valBucketRequired')
    ok = false
  }
  if (!editForm.value.accessKeyId.trim()) {
    editErrors.value.accessKeyId = t('settingsObjectStorage.valAccessKeyIdRequired')
    ok = false
  }
  return ok
}

async function submitEdit(): Promise<void> {
  if (!editing.value || !validateEdit()) return
  editSubmitting.value = true
  editBanner.value = ''
  const id = editing.value.id
  try {
    const updated = await updateObjectStore(id, {
      name: editForm.value.name.trim(),
      endpoint: editForm.value.endpoint.trim(),
      useSSL: editForm.value.useSSL,
      bucket: editForm.value.bucket.trim(),
      // 空串 = 清空前缀(可选字段,允许显式清空)。
      pathPrefix: editForm.value.pathPrefix.trim(),
      accessKeyId: editForm.value.accessKeyId.trim(),
      // 留空 = 不轮换;填新值 = 原地轮换(credentialId 不变)。
      ...(editForm.value.secret ? { secret: editForm.value.secret } : {}),
    })
    const idx = stores.value.findIndex((p) => p.id === id)
    if (idx >= 0) stores.value[idx] = updated
    editForm.value.secret = ''
    editOpen.value = false
    editing.value = null
  } catch (err) {
    editBanner.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('settingsObjectStorage.errSave', { status: err.status }))
        : t('settingsObjectStorage.errSaveRetry')
  } finally {
    editSubmitting.value = false
  }
}

// ─── 测试连接 ──────────────────────────────────────────────────────────────────
const testingId = ref<string | null>(null)

// ─── 桶候选列表（datalist 下拉建议；仍可手输）──────────────────────────────────
const bucketOptions = ref<ObjectStoreBucket[]>([])
const fetchingBuckets = ref(false)
const editBucketOptions = ref<ObjectStoreBucket[]>([])
const editFetchingBuckets = ref(false)

/** 添加弹窗的拉取条件：endpoint/accessKeyId/secret 齐全（探测端点需要完整凭据）。 */
const canFetchBuckets = computed(
  () =>
    !fetchingBuckets.value &&
    !submitting.value &&
    form.value.endpoint.trim() !== '' &&
    form.value.accessKeyId.trim() !== '' &&
    form.value.secret !== '',
)

/** 编辑弹窗的拉取条件：凭据未动（endpoint/AK 未改、未填新 secret）→ 服务端已存凭据即可；
    改过则与添加同规则 —— 需填新 secret 才能拼出完整凭据。 */
const canFetchEditBuckets = computed(() => {
  if (editFetchingBuckets.value || editSubmitting.value || !editing.value) return false
  const unchanged =
    editForm.value.endpoint.trim() === editing.value.endpoint &&
    editForm.value.accessKeyId.trim() === editing.value.accessKeyId &&
    editForm.value.secret === ''
  if (unchanged) return editForm.value.endpoint.trim() !== ''
  return editForm.value.endpoint.trim() !== '' && editForm.value.accessKeyId.trim() !== '' && editForm.value.secret !== ''
})

/** 拉取失败的人话提示（toast）；字段仍可手输，不阻断表单。 */
function bucketFetchError(err: unknown): string {
  return err instanceof HttpError
    ? (err.apiError?.message ?? t('settingsObjectStorage.errFetchBuckets'))
    : t('settingsObjectStorage.errFetchBuckets')
}

/** 添加弹窗：未落库明文凭据直连探测列桶（不产生任何持久化副作用）。 */
async function fetchBucketOptions(): Promise<void> {
  if (!canFetchBuckets.value) return
  fetchingBuckets.value = true
  try {
    const res = await probeObjectStoreBuckets({
      type: form.value.type,
      endpoint: form.value.endpoint.trim(),
      useSSL: form.value.useSSL,
      accessKeyId: form.value.accessKeyId.trim(),
      secret: form.value.secret,
    })
    bucketOptions.value = res.buckets
  } catch (err) {
    bucketOptions.value = []
    toast.error(bucketFetchError(err))
  } finally {
    fetchingBuckets.value = false
  }
}

/** 编辑弹窗：凭据未动走已存配置的列桶；改过则按当前表单值直连探测。 */
async function fetchEditBucketOptions(): Promise<void> {
  if (!canFetchEditBuckets.value || !editing.value) return
  editFetchingBuckets.value = true
  try {
    const unchanged =
      editForm.value.endpoint.trim() === editing.value.endpoint &&
      editForm.value.accessKeyId.trim() === editing.value.accessKeyId &&
      editForm.value.secret === ''
    const res = unchanged
      ? await listObjectStoreBuckets(editing.value.id)
      : await probeObjectStoreBuckets({
          type: editing.value.type,
          endpoint: editForm.value.endpoint.trim(),
          useSSL: editForm.value.useSSL,
          accessKeyId: editForm.value.accessKeyId.trim(),
          secret: editForm.value.secret,
        })
    editBucketOptions.value = res.buckets
  } catch (err) {
    editBucketOptions.value = []
    toast.error(bucketFetchError(err))
  } finally {
    editFetchingBuckets.value = false
  }
}

async function testConnection(s: ObjectStore): Promise<void> {
  if (testingId.value) return
  testingId.value = s.id
  try {
    const res = await testObjectStore(s.id)
    if (res.ok) {
      toast.success(t('settingsObjectStorage.testOk'), { detail: res.detail || s.bucket })
    } else {
      toast.error(t('settingsObjectStorage.testFail'), { detail: res.error ?? res.detail })
    }
  } catch (err) {
    toast.error(t('settingsObjectStorage.testFail'), {
      detail:
        err instanceof HttpError
          ? (err.apiError?.message ?? t('settingsObjectStorage.errNetwork'))
          : t('settingsObjectStorage.errNetwork'),
    })
  } finally {
    testingId.value = null
  }
}

// ─── 删除 ──────────────────────────────────────────────────────────────────────
const deleteOpen = ref(false)
const deleting = ref<ObjectStore | null>(null)
const deleteSubmitting = ref(false)
const deleteBanner = ref('')

function openDelete(s: ObjectStore): void {
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
    const res = await deleteObjectStore(id)
    if (res.ok) {
      stores.value = stores.value.filter((p) => p.id !== id)
      deleteOpen.value = false
      deleting.value = null
    } else {
      deleteBanner.value = t('settingsObjectStorage.errDelete', { status: 0 })
    }
  } catch (err) {
    deleteBanner.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('settingsObjectStorage.errDelete', { status: err.status }))
        : t('settingsObjectStorage.errNetwork')
  } finally {
    deleteSubmitting.value = false
  }
}
</script>

<template>
  <div class="obj-root">
    <!-- section header -->
    <div class="section-head">
      <div class="section-head-text">
        <h2 class="section-title">{{ t('settingsObjectStorage.title') }}</h2>
        <p class="section-desc">{{ t('settingsObjectStorage.desc') }}</p>
      </div>
      <button class="btn-primary" :disabled="loadState === 'loading'" @click="openAdd">
        <NIcon :size="14"><Plus /></NIcon>
        {{ t('settingsObjectStorage.addStore') }}
      </button>
    </div>

    <!-- load error -->
    <div v-if="loadState === 'error'" class="banner banner--error" role="alert">
      <span>⚠ {{ loadError }}</span>
      <button class="banner-retry" @click="load">↻ {{ t('settingsObjectStorage.retry') }}</button>
    </div>

    <!-- panel -->
    <div class="panel">
      <div class="panel-head">
        <span>{{ t('settingsObjectStorage.panelTitle') }}</span>
        <span v-if="loadState === 'idle'" class="panel-meta">{{ t('settingsObjectStorage.countLabel', { n: stores.length }) }}</span>
      </div>

      <div v-if="loadState === 'loading'" class="state-msg">{{ t('common.refresh') }}…</div>

      <div v-else-if="loadState === 'idle' && stores.length === 0" class="empty-state">
        <div class="empty-icon" aria-hidden="true"><NIcon :size="22"><CloudUpload /></NIcon></div>
        <p class="empty-label">{{ t('settingsObjectStorage.emptyLabel') }}</p>
        <p class="empty-hint">{{ t('settingsObjectStorage.emptyHint') }}</p>
        <button class="btn-primary" @click="openAdd">+ {{ t('settingsObjectStorage.addFirstStore') }}</button>
      </div>

      <template v-else-if="loadState === 'idle'">
        <div class="obj-row obj-row--head" aria-hidden="true">
          <span>{{ t('settingsObjectStorage.colType') }}</span>
          <span>{{ t('settingsObjectStorage.colName') }}</span>
          <span>{{ t('settingsObjectStorage.colBucket') }}</span>
          <span>{{ t('settingsObjectStorage.colCredential') }}</span>
          <span />
        </div>
        <div v-for="s in stores" :key="s.id" class="obj-row">
          <span class="type-badge">{{ typeLabels[s.type] }}</span>
          <div class="obj-name-cell">
            <strong class="obj-name">{{ s.name }}</strong>
            <span v-if="s.endpoint" class="obj-ep mono">{{ s.endpoint }}<span class="proto-tag" :class="s.useSSL ? 'proto-tag--tls' : ''">{{ s.useSSL ? 'https' : 'http' }}</span></span>
          </div>
          <span class="obj-bucket mono">{{ s.bucket }}<span v-if="s.pathPrefix" class="obj-prefix">/{{ s.pathPrefix }}/</span></span>
          <span class="cred-state" :class="s.credentialConfigured ? 'cred-state--ok' : 'cred-state--missing'">
            <NIcon :size="13"><CircleCheck v-if="s.credentialConfigured" /><CircleX v-else /></NIcon>
            {{ s.credentialConfigured ? t('settingsObjectStorage.credConfigured') : t('settingsObjectStorage.credMissing') }}
          </span>
          <span class="obj-ops">
            <button
              class="op-btn"
              :title="t('settingsObjectStorage.browse')"
              :aria-label="t('settingsObjectStorage.browse')"
              @click="browseStore(s)"
            >
              <NIcon :size="13"><Folder /></NIcon>
              <span class="op-btn-txt">{{ t('settingsObjectStorage.browse') }}</span>
            </button>
            <button
              class="op-btn"
              :title="t('settingsObjectStorage.editTitle', { name: s.name })"
              :aria-label="t('settingsObjectStorage.editTitle', { name: s.name })"
              @click="openEdit(s)"
            >
              <NIcon :size="13"><Pencil /></NIcon>
              <span class="op-btn-txt">{{ t('settingsObjectStorage.editBtn') }}</span>
            </button>
            <button
              class="op-btn"
              :disabled="testingId === s.id"
              :title="t('settingsObjectStorage.test')"
              @click="testConnection(s)"
            >
              <NIcon :size="13"><PlugConnected /></NIcon>
              <span class="op-btn-txt">{{ testingId === s.id ? t('settingsObjectStorage.testing') : t('settingsObjectStorage.test') }}</span>
            </button>
            <button
              class="op-btn op-btn--danger"
              :title="t('settingsObjectStorage.deleteTitle', { name: s.name })"
              :aria-label="t('settingsObjectStorage.deleteTitle', { name: s.name })"
              @click="openDelete(s)"
            >
              <NIcon :size="13"><Trash /></NIcon>
            </button>
          </span>
        </div>
      </template>
    </div>
  </div>

  <!-- add modal -->
  <Teleport to="body">
    <div
      v-if="modalOpen"
      class="modal-scrim"
      role="dialog"
      :aria-label="t('settingsObjectStorage.addTitle')"
      aria-modal="true"
      @keydown.esc="requestClose"
    >
      <div class="modal">
        <div class="modal-head">
          <div class="modal-icon" aria-hidden="true"><NIcon :size="18"><CloudUpload /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('settingsObjectStorage.addTitle') }}</h3>
            <p class="modal-sub">{{ t('settingsObjectStorage.addSub') }}</p>
          </div>
          <button class="modal-close" :aria-label="t('settingsObjectStorage.closeDialog')" :disabled="submitting" @click="requestClose">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6 6 18M6 6l12 12"/></svg>
          </button>
        </div>

        <div v-if="formBanner" class="banner banner--error modal-banner" role="alert">{{ formBanner }}</div>

        <form class="modal-form" novalidate @submit.prevent="submit">
          <div class="field">
            <label class="field-label">{{ t('settingsObjectStorage.fieldType') }}</label>
            <div class="segmented" role="group">
              <button
                v-for="opt in STORE_TYPES"
                :key="opt"
                type="button"
                class="seg-item"
                :class="{ 'seg-item--active': form.type === opt }"
                :disabled="submitting"
                @click="switchType(opt)"
              >{{ typeLabels[opt] }}</button>
            </div>
          </div>

          <div class="field">
            <label class="field-label" for="obj-name">{{ t('settingsObjectStorage.fieldName') }}</label>
            <input
              id="obj-name"
              v-model="form.name"
              class="field-input"
              :class="{ 'field-input--error': errors.name }"
              type="text"
              :placeholder="t('settingsObjectStorage.namePlaceholder')"
              :disabled="submitting"
              autocomplete="off"
              @input="errors.name = ''"
            />
            <span v-if="errors.name" class="field-error" role="alert">{{ errors.name }}</span>
          </div>

          <div class="field">
            <label class="field-label" for="obj-endpoint">{{ t('settingsObjectStorage.fieldEndpoint') }}</label>
            <input
              id="obj-endpoint"
              v-model="form.endpoint"
              class="field-input field-input--mono"
              :class="{ 'field-input--error': errors.endpoint }"
              type="text"
              :placeholder="endpointPlaceholderFor(form.type)"
              :disabled="submitting"
              autocomplete="off"
              spellcheck="false"
              @input="errors.endpoint = ''"
            />
            <span v-if="errors.endpoint" class="field-error" role="alert">{{ errors.endpoint }}</span>
            <span class="field-hint">{{ endpointHintFor(form.type) }}</span>
          </div>

          <label class="check-field" :class="{ 'check-field--disabled': submitting }">
            <input v-model="form.useSSL" type="checkbox" :disabled="submitting" />
            <span>{{ t('settingsObjectStorage.fieldUseSSL') }}</span>
          </label>
          <span class="field-hint check-field-hint">{{ t('settingsObjectStorage.useSSLHint') }}</span>

          <div class="field">
            <div class="field-label-row">
              <label class="field-label" for="obj-bucket">{{ t('settingsObjectStorage.fieldBucket') }}</label>
              <button
                type="button"
                class="link-btn"
                :disabled="!canFetchBuckets"
                @click="fetchBucketOptions"
              >{{ fetchingBuckets ? t('settingsObjectStorage.fetchingBuckets') : t('settingsObjectStorage.fetchBuckets') }}</button>
            </div>
            <input
              id="obj-bucket"
              v-model="form.bucket"
              class="field-input field-input--mono"
              :class="{ 'field-input--error': errors.bucket }"
              type="text"
              list="obj-bucket-options"
              :placeholder="t('settingsObjectStorage.bucketPlaceholder')"
              :disabled="submitting"
              autocomplete="off"
              spellcheck="false"
              @input="errors.bucket = ''"
            />
            <datalist id="obj-bucket-options">
              <option v-for="b in bucketOptions" :key="b.name" :value="b.name" />
            </datalist>
            <span v-if="errors.bucket" class="field-error" role="alert">{{ errors.bucket }}</span>
            <span class="field-hint">{{ t('settingsObjectStorage.bucketHint') }}</span>
          </div>

          <div class="field">
            <label class="field-label" for="obj-prefix">{{ t('settingsObjectStorage.fieldPathPrefix') }}</label>
            <input
              id="obj-prefix"
              v-model="form.pathPrefix"
              class="field-input field-input--mono"
              type="text"
              :placeholder="t('settingsObjectStorage.pathPrefixPlaceholder')"
              :disabled="submitting"
              autocomplete="off"
              spellcheck="false"
            />
            <span class="field-hint">{{ t('settingsObjectStorage.pathPrefixHint') }}</span>
          </div>

          <div class="field">
            <label class="field-label" for="obj-ak">{{ t('settingsObjectStorage.fieldAccessKeyId') }}</label>
            <input
              id="obj-ak"
              v-model="form.accessKeyId"
              class="field-input field-input--mono"
              :class="{ 'field-input--error': errors.accessKeyId }"
              type="text"
              :placeholder="t('settingsObjectStorage.accessKeyIdPlaceholder')"
              :disabled="submitting"
              autocomplete="off"
              spellcheck="false"
              @input="errors.accessKeyId = ''"
            />
            <span v-if="errors.accessKeyId" class="field-error" role="alert">{{ errors.accessKeyId }}</span>
            <span class="field-hint">{{ t('settingsObjectStorage.accessKeyIdHint') }}</span>
          </div>

          <div class="field">
            <label class="field-label" for="obj-secret">{{ t('settingsObjectStorage.fieldSecret') }}</label>
            <input
              id="obj-secret"
              v-model="form.secret"
              class="field-input field-input--mono"
              :class="{ 'field-input--error': errors.secret }"
              type="password"
              :placeholder="t('settingsObjectStorage.secretPlaceholder')"
              :disabled="submitting"
              autocomplete="new-password"
              @input="errors.secret = ''"
            />
            <span v-if="errors.secret" class="field-error" role="alert">{{ errors.secret }}</span>
            <span class="field-hint">{{ t('settingsObjectStorage.secretHint') }}</span>
          </div>

          <div class="modal-footer">
            <button type="button" class="btn-secondary" :disabled="submitting" @click="requestClose">{{ t('settingsObjectStorage.cancel') }}</button>
            <button type="submit" class="btn-primary" :disabled="submitting" :aria-busy="submitting">
              <span v-if="submitting" class="spinner" aria-hidden="true" />
              {{ submitting ? t('settingsObjectStorage.saving') : t('settingsObjectStorage.create') }}
            </button>
          </div>
        </form>
      </div>
    </div>
  </Teleport>

  <!-- edit modal -->
  <Teleport to="body">
    <div
      v-if="editOpen && editing"
      class="modal-scrim"
      role="dialog"
      :aria-label="t('settingsObjectStorage.editTitle', { name: editing.name })"
      aria-modal="true"
      @keydown.esc="requestCloseEdit"
    >
      <div class="modal">
        <div class="modal-head">
          <div class="modal-icon" aria-hidden="true"><NIcon :size="18"><Pencil /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('settingsObjectStorage.editTitle', { name: editing.name }) }}</h3>
            <p class="modal-sub">{{ t('settingsObjectStorage.editSub') }}</p>
          </div>
          <button class="modal-close" :aria-label="t('settingsObjectStorage.closeDialog')" :disabled="editSubmitting" @click="requestCloseEdit">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6 6 18M6 6l12 12"/></svg>
          </button>
        </div>

        <div v-if="editBanner" class="banner banner--error modal-banner" role="alert">{{ editBanner }}</div>

        <form class="modal-form" novalidate @submit.prevent="submitEdit">
          <div class="field">
            <label class="field-label" for="edit-obj-name">{{ t('settingsObjectStorage.fieldName') }}</label>
            <input
              id="edit-obj-name"
              v-model="editForm.name"
              class="field-input"
              :class="{ 'field-input--error': editErrors.name }"
              type="text"
              :disabled="editSubmitting"
              autocomplete="off"
              @input="editErrors.name = ''"
            />
            <span v-if="editErrors.name" class="field-error" role="alert">{{ editErrors.name }}</span>
          </div>

          <div class="field">
            <label class="field-label" for="edit-obj-endpoint">{{ t('settingsObjectStorage.fieldEndpoint') }}</label>
            <input
              id="edit-obj-endpoint"
              v-model="editForm.endpoint"
              class="field-input field-input--mono"
              :class="{ 'field-input--error': editErrors.endpoint }"
              type="text"
              :disabled="editSubmitting"
              autocomplete="off"
              spellcheck="false"
              @input="editErrors.endpoint = ''"
            />
            <span v-if="editErrors.endpoint" class="field-error" role="alert">{{ editErrors.endpoint }}</span>
          </div>

          <label class="check-field" :class="{ 'check-field--disabled': editSubmitting }">
            <input v-model="editForm.useSSL" type="checkbox" :disabled="editSubmitting" />
            <span>{{ t('settingsObjectStorage.fieldUseSSL') }}</span>
          </label>
          <span class="field-hint check-field-hint">{{ t('settingsObjectStorage.useSSLHint') }}</span>

          <div class="field">
            <div class="field-label-row">
              <label class="field-label" for="edit-obj-bucket">{{ t('settingsObjectStorage.fieldBucket') }}</label>
              <button
                type="button"
                class="link-btn"
                :disabled="!canFetchEditBuckets"
                @click="fetchEditBucketOptions"
              >{{ editFetchingBuckets ? t('settingsObjectStorage.fetchingBuckets') : t('settingsObjectStorage.fetchBuckets') }}</button>
            </div>
            <input
              id="edit-obj-bucket"
              v-model="editForm.bucket"
              class="field-input field-input--mono"
              :class="{ 'field-input--error': editErrors.bucket }"
              type="text"
              list="edit-obj-bucket-options"
              :disabled="editSubmitting"
              autocomplete="off"
              spellcheck="false"
              @input="editErrors.bucket = ''"
            />
            <datalist id="edit-obj-bucket-options">
              <option v-for="b in editBucketOptions" :key="b.name" :value="b.name" />
            </datalist>
            <span v-if="editErrors.bucket" class="field-error" role="alert">{{ editErrors.bucket }}</span>
          </div>

          <div class="field">
            <label class="field-label" for="edit-obj-prefix">{{ t('settingsObjectStorage.fieldPathPrefix') }}</label>
            <input
              id="edit-obj-prefix"
              v-model="editForm.pathPrefix"
              class="field-input field-input--mono"
              type="text"
              :placeholder="t('settingsObjectStorage.pathPrefixPlaceholder')"
              :disabled="editSubmitting"
              autocomplete="off"
              spellcheck="false"
            />
            <span class="field-hint">{{ t('settingsObjectStorage.pathPrefixHint') }}</span>
          </div>

          <div class="field">
            <label class="field-label" for="edit-obj-ak">{{ t('settingsObjectStorage.fieldAccessKeyId') }}</label>
            <input
              id="edit-obj-ak"
              v-model="editForm.accessKeyId"
              class="field-input field-input--mono"
              :class="{ 'field-input--error': editErrors.accessKeyId }"
              type="text"
              :disabled="editSubmitting"
              autocomplete="off"
              spellcheck="false"
              @input="editErrors.accessKeyId = ''"
            />
            <span v-if="editErrors.accessKeyId" class="field-error" role="alert">{{ editErrors.accessKeyId }}</span>
          </div>

          <div class="field">
            <label class="field-label" for="edit-obj-secret">{{ t('settingsObjectStorage.fieldSecretRotate') }}</label>
            <input
              id="edit-obj-secret"
              v-model="editForm.secret"
              class="field-input field-input--mono"
              type="password"
              :placeholder="t('settingsObjectStorage.secretRotatePlaceholder')"
              :disabled="editSubmitting"
              autocomplete="new-password"
            />
            <span class="field-hint">{{ t('settingsObjectStorage.secretRotateHint') }}</span>
          </div>

          <div class="modal-footer">
            <button type="button" class="btn-secondary" :disabled="editSubmitting" @click="requestCloseEdit">{{ t('settingsObjectStorage.cancel') }}</button>
            <button type="submit" class="btn-primary" :disabled="editSubmitting" :aria-busy="editSubmitting">
              <span v-if="editSubmitting" class="spinner" aria-hidden="true" />
              {{ editSubmitting ? t('settingsObjectStorage.saving') : t('settingsObjectStorage.save') }}
            </button>
          </div>
        </form>
      </div>
    </div>
  </Teleport>

  <!-- delete modal -->
  <Teleport to="body">
    <div
      v-if="deleteOpen && deleting"
      class="modal-scrim"
      role="dialog"
      :aria-label="t('settingsObjectStorage.deleteConfirmTitle')"
      aria-modal="true"
      @keydown.esc="closeDelete"
    >
      <div class="modal modal--sm">
        <div class="modal-head">
          <div class="modal-icon modal-icon--danger" aria-hidden="true"><NIcon :size="18"><Trash /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('settingsObjectStorage.deleteConfirmTitle') }}</h3>
            <p class="modal-sub">{{ t('settingsObjectStorage.deleteIrreversible') }}</p>
          </div>
          <button class="modal-close" :aria-label="t('settingsObjectStorage.closeDialog')" :disabled="deleteSubmitting" @click="closeDelete">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6 6 18M6 6l12 12"/></svg>
          </button>
        </div>
        <div class="modal-body">
          <p class="delete-text">
            {{ t('settingsObjectStorage.deleteConfirmPrefix') }}
            <strong class="delete-name">{{ deleting.name }}</strong>
            {{ t('settingsObjectStorage.deleteConfirmSuffix') }}
          </p>
          <div v-if="deleteBanner" class="banner banner--error modal-banner" role="alert">{{ deleteBanner }}</div>
        </div>
        <div class="modal-footer modal-footer--standalone">
          <button type="button" class="btn-secondary" :disabled="deleteSubmitting" @click="closeDelete">{{ t('settingsObjectStorage.cancel') }}</button>
          <button type="button" class="btn-danger" :disabled="deleteSubmitting" :aria-busy="deleteSubmitting" @click="confirmDelete">
            <span v-if="deleteSubmitting" class="spinner spinner--red" aria-hidden="true" />
            {{ deleteSubmitting ? t('settingsObjectStorage.deleting') : t('settingsObjectStorage.confirmDelete') }}
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.obj-root {
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
  max-width: 46ch;
  line-height: 1.55;
  margin-bottom: 6px;
}

.obj-row {
  display: grid;
  grid-template-columns: 110px minmax(130px, 1.4fr) minmax(120px, 1fr) 120px auto;
  align-items: center;
  gap: 14px;
  padding: 13px 18px;
  border-bottom: 1px solid var(--color-border);
  transition: background-color var(--duration-fast);
}
.obj-row:last-child {
  border-bottom: none;
}
.obj-row:not(.obj-row--head):hover {
  background: var(--color-inset);
}
.obj-row--head {
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
  border: 1px solid var(--color-amber-line);
  background: var(--color-amber-soft);
  color: var(--color-amber);
  white-space: nowrap;
}

.obj-name-cell {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.obj-name {
  font-size: 0.85rem;
  font-weight: 500;
  color: var(--color-text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.obj-ep {
  font-size: 0.72rem;
  color: var(--color-faint);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.obj-bucket {
  font-size: 0.78rem;
  color: var(--color-dim);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.obj-prefix {
  color: var(--color-faint);
}
.mono {
  font-family: var(--font-mono);
}

.cred-state {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 0.74rem;
  font-weight: 600;
  white-space: nowrap;
}
.cred-state--ok {
  color: var(--color-green);
}
.cred-state--missing {
  color: var(--color-faint);
}

.obj-ops {
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
  max-width: 680px;
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
.modal-footer--standalone {
  padding: 4px 20px 20px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
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
.field-label-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}
.link-btn {
  background: none;
  border: none;
  padding: 0;
  font-size: 0.74rem;
  color: var(--color-primary);
  cursor: pointer;
}
.link-btn:hover:not(:disabled) {
  text-decoration: underline;
}
.link-btn:disabled {
  color: var(--color-faint);
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
.check-field--disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.check-field-hint {
  margin-top: -4px;
}
.proto-tag {
  margin-left: 6px;
  padding: 0 5px;
  border-radius: 4px;
  font-size: 0.62rem;
  font-weight: 600;
  line-height: 1.5;
  color: var(--color-faint);
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  text-transform: uppercase;
}
.proto-tag--tls {
  color: var(--color-green);
  border-color: var(--color-green-soft);
}

.segmented {
  display: inline-flex;
  background: var(--color-inset);
  border-radius: var(--rounded);
  padding: 3px;
  gap: 2px;
  width: 100%;
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
@keyframes spin {
  to { transform: rotate(360deg); }
}

@media (max-width: 640px) {
  .obj-row {
    grid-template-columns: 1fr 1fr;
    gap: 8px 12px;
  }
  .obj-row--head {
    display: none;
  }
  .obj-ops {
    grid-column: 1 / -1;
    justify-content: flex-start;
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
