<script setup lang="ts">
/*
  ObjectStorage.vue — 对象存储浏览(只读浏览 + 新建桶;顶部导航「对象存储」)。

  实例在 设置 → 对象存储 注册;此处浏览其桶内目录树:面包屑导航 + 子目录/对象列表。
  桶内对象可能巨多:S3/OSS 协议没有随机跳页能力,采用 marker 游标分页 —— 每次请求
  单页返回,「上一页」用 marker 历史栈回放,绝不聚合全量。
  浏览根锚定在配置级 pathPrefix 之下(与流水线 oss_upload 节点的写入位置一致)。
  「新建桶」在当前实例的账户下建桶并自动登记为新配置(凭据由后端独立重封)。
*/
import { ref, computed, watch, onMounted, nextTick } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { CloudUpload, Folder, File as FileIcon, Refresh, ChevronLeft, ChevronRight, Plus } from '@vicons/tabler'
import {
  listObjectStores, browseObjectStore, createObjectStoreBucket,
  type ObjectStore, type BrowseObjectStoreResult,
} from '../api/objectStores'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'

const { t } = useI18n()
const toast = useToast()
const route = useRoute()

const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 实例 ──────────────────────────────────────────────────────────────────────
const stores = ref<ObjectStore[]>([])
const currentId = ref('')
const loadingStores = ref(true)
const current = computed<ObjectStore | null>(() =>
  stores.value.find((s) => s.id === currentId.value) ?? null)

async function loadStores(): Promise<void> {
  loadingStores.value = true
  try {
    stores.value = await listObjectStores()
    // 支持 ?store=<id> 预选(设置页「浏览」按钮跳入)。
    const wanted = String(route.query.store ?? '')
    currentId.value = stores.value.some((s) => s.id === wanted) ? wanted : (stores.value[0]?.id ?? '')
  } catch (err) {
    toast.error(t('objectStorage.errLoadStores'), { detail: errMsg(err, t('objectStorage.errNetwork')) })
  } finally {
    loadingStores.value = false
  }
}

// ─── 目录与 marker 游标分页 ────────────────────────────────────────────────────
const prefix = ref('') // 当前目录(相对 pathPrefix;空 = 前缀根)
const PAGE_SIZES = [50, 100, 200]
const pageSize = ref(100)
const pageMarkers = ref<string[]>(['']) // 各页起始 marker(第 0 页 = 空)
const pageIdx = ref(0)
const result = ref<BrowseObjectStoreResult | null>(null)
const loading = ref(false)
const error = ref('')

// 配置级 pathPrefix 归一化后的完整形态(无首尾斜杠;空 = 未配置)。
const prefixBase = computed(() => (current.value?.pathPrefix ?? '').replace(/^\/+|\/+$/g, ''))

// 当前目录的桶内完整前缀(pathPrefix + 当前目录,以 / 结尾;根为空)。
const fullPrefix = computed(() => {
  const parts = [prefixBase.value, prefix.value]
    .map((p) => p.replace(/^\/+|\/+$/g, ''))
    .filter((p) => p !== '')
  const joined = parts.join('/')
  return joined === '' ? '' : joined + '/'
})

// 面包屑:bucket(根) + pathPrefix 段 + 目录段;每层可点击跳回,rel 为相对 pathPrefix 的目录。
const crumbs = computed(() => {
  const segs = fullPrefix.value === '' ? [] : fullPrefix.value.slice(0, -1).split('/')
  const baseCount = prefixBase.value === '' ? 0 : prefixBase.value.split('/').length
  const out = [{ name: current.value?.bucket ?? '', rel: '' }]
  segs.forEach((name, i) => {
    out.push({ name, rel: segs.slice(baseCount, i + 1).join('/') })
  })
  return out
})

// 桶内完整键 → 相对 pathPrefix 的形式(去掉配置前缀段)。
function relOf(fullKey: string): string {
  const base = prefixBase.value === '' ? '' : prefixBase.value + '/'
  return fullKey.startsWith(base) ? fullKey.slice(base.length) : fullKey
}

// 行显示名:去掉当前目录前缀后的段名(目录键以 / 结尾)。
function displayName(key: string): string {
  return key.slice(fullPrefix.value.length).replace(/\/$/, '')
}

function formatBytes(n: number): string {
  if (!n) return '—'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MiB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GiB`
}

function formatTime(iso: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

async function loadPage(): Promise<void> {
  if (!current.value) return
  loading.value = true
  error.value = ''
  try {
    result.value = await browseObjectStore(current.value.id, {
      prefix: prefix.value,
      marker: pageMarkers.value[pageIdx.value] ?? '',
      maxKeys: pageSize.value,
    })
  } catch (err) {
    result.value = null
    error.value = errMsg(err, t('objectStorage.errNetwork'))
  } finally {
    loading.value = false
  }
}

// 下一页:nextMarker 压栈(截断旧前进尾巴,翻页路径始终线性)。
function nextPage(): void {
  const nm = result.value?.nextMarker ?? ''
  if (!nm || loading.value) return
  pageMarkers.value = [...pageMarkers.value.slice(0, pageIdx.value + 1), nm]
  pageIdx.value++
  void loadPage()
}

function prevPage(): void {
  if (pageIdx.value === 0 || loading.value) return
  pageIdx.value--
  void loadPage()
}

// 进入目录(p 为桶内完整目录键,以 / 结尾)或面包屑层(rel 为相对 pathPrefix 目录)。
function enterDir(dir: string): void {
  prefix.value = dir.replace(/^\/+|\/+$/g, '')
  pageMarkers.value = ['']
  pageIdx.value = 0
  void loadPage()
}

function enterPrefix(p: string): void {
  enterDir(relOf(p))
}

function onPageSizeChange(): void {
  pageMarkers.value = ['']
  pageIdx.value = 0
  void loadPage()
}

// 切实例:回到该实例的前缀根并重置分页。
watch(currentId, () => {
  prefix.value = ''
  pageMarkers.value = ['']
  pageIdx.value = 0
  result.value = null
  error.value = ''
  if (currentId.value) void loadPage()
})

onMounted(loadStores)

// ─── 新建桶(当前账户下建桶 + 自动登记新配置)───────────────────────────────────
const bucketModalOpen = ref(false)
const bucketName = ref('')
const bucketDisplayName = ref('')
const creating = ref(false)
const createError = ref('')
const bucketInput = ref<HTMLInputElement | null>(null)

// 与后端 bucketNameRe 同规则:3-63 位小写字母/数字/连字符,两端为字母或数字。
const BUCKET_NAME_RE = /^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$/
const bucketNameValid = computed(() => BUCKET_NAME_RE.test(bucketName.value.trim().toLowerCase()))

async function openBucketModal(): Promise<void> {
  bucketName.value = ''
  bucketDisplayName.value = ''
  createError.value = ''
  bucketModalOpen.value = true
  await nextTick()
  bucketInput.value?.focus()
}

async function submitCreateBucket(): Promise<void> {
  if (!current.value || creating.value || !bucketNameValid.value) return
  creating.value = true
  createError.value = ''
  try {
    const created = await createObjectStoreBucket(current.value.id, {
      bucket: bucketName.value.trim().toLowerCase(),
      ...(bucketDisplayName.value.trim() ? { name: bucketDisplayName.value.trim() } : {}),
    })
    bucketModalOpen.value = false
    toast.success(t('objectStorage.createBucketOk', { bucket: created.bucket }))
    // 重拉列表并切到新配置(触发 currentId watch:回根目录 + 重载第一页)。
    stores.value = await listObjectStores()
    currentId.value = created.id
  } catch (err) {
    createError.value = errMsg(err, t('objectStorage.errNetwork'))
  } finally {
    creating.value = false
  }
}
</script>

<template>
  <div class="obs-view">
    <header class="view-header">
      <div class="header-main">
        <h1 class="view-title">{{ t('objectStorage.title') }}</h1>
        <p class="view-sub">{{ t('objectStorage.subtitle') }}</p>
      </div>
      <div v-if="stores.length > 0" class="header-tools">
        <select v-model="currentId" class="store-select" :aria-label="t('objectStorage.pickStore')">
          <option v-for="s in stores" :key="s.id" :value="s.id">
            {{ s.name }} · {{ s.bucket }}{{ s.pathPrefix ? ` /${s.pathPrefix}` : '' }}
          </option>
        </select>
        <button class="btn-secondary" @click="openBucketModal">
          <NIcon :size="13"><Plus /></NIcon>{{ t('objectStorage.createBucket') }}
        </button>
        <button class="btn-secondary" :disabled="loading || !current" @click="loadPage">
          <NIcon :size="13"><Refresh /></NIcon>{{ t('common.refresh') }}
        </button>
      </div>
    </header>

    <div v-if="loadingStores" class="obs-state">{{ t('objectStorage.loading') }}</div>

    <!-- 无实例:引导去设置页注册 -->
    <div v-else-if="stores.length === 0" class="empty-panel">
      <NIcon :size="26"><CloudUpload /></NIcon>
      <p class="empty-title">{{ t('objectStorage.noStores') }}</p>
      <p class="empty-desc">{{ t('objectStorage.noStoresDesc') }}</p>
      <router-link to="/settings/object-storage" class="btn-primary">
        {{ t('objectStorage.goToSettings') }}
      </router-link>
    </div>

    <template v-else>
      <!-- 面包屑:bucket / pathPrefix 段 / 目录段 -->
      <nav class="crumbs" :aria-label="t('objectStorage.breadcrumb')">
        <template v-for="(c, i) in crumbs" :key="`${i}-${c.name}`">
          <span v-if="i > 0" class="crumb-sep">/</span>
          <button v-if="i < crumbs.length - 1" class="crumb-link" @click="enterDir(c.rel)">{{ c.name }}</button>
          <span v-else class="crumb-current" :title="c.name">{{ c.name }}</span>
        </template>
      </nav>

      <div class="obs-toolbar">
        <span class="row-count">
          {{ t('objectStorage.entryCount', {
            dirs: result?.prefixes.length ?? 0,
            files: result?.objects.length ?? 0,
          }) }}
        </span>
        <select v-model="pageSize" class="page-size" :aria-label="t('objectStorage.pageSize')" @change="onPageSizeChange">
          <option v-for="n in PAGE_SIZES" :key="n" :value="n">{{ n }} / {{ t('objectStorage.pageUnit') }}</option>
        </select>
      </div>

      <div v-if="loading" class="obs-state">{{ t('objectStorage.loading') }}</div>
      <div v-else-if="error" class="obs-error">
        <p>{{ error }}</p>
        <button class="btn-secondary" @click="loadPage">{{ t('objectStorage.retry') }}</button>
      </div>
      <div v-else-if="!result || (result.prefixes.length === 0 && result.objects.length === 0)" class="empty-panel empty-panel--slim">
        <NIcon :size="22"><Folder /></NIcon>
        <p class="empty-title">{{ t('objectStorage.emptyDir') }}</p>
      </div>
      <div v-else class="grid-scroll">
        <table class="data-table">
          <thead>
            <tr>
              <th>{{ t('objectStorage.colName') }}</th>
              <th class="col-num">{{ t('objectStorage.colSize') }}</th>
              <th>{{ t('objectStorage.colModified') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="p in result.prefixes" :key="p" class="row-click" @click="enterPrefix(p)">
              <td class="cell-strong name-cell">
                <NIcon :size="14" class="icon-dir"><Folder /></NIcon>
                <span class="mono">{{ displayName(p) }}/</span>
              </td>
              <td class="col-num cell-dim">—</td>
              <td class="cell-dim">—</td>
            </tr>
            <tr v-for="o in result.objects" :key="o.key">
              <td class="name-cell">
                <NIcon :size="14" class="icon-file"><FileIcon /></NIcon>
                <span class="mono" :title="o.key">{{ displayName(o.key) }}</span>
              </td>
              <td class="col-num">{{ formatBytes(o.size) }}</td>
              <td class="cell-dim">{{ formatTime(o.lastModified) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- marker 游标分页:只有顺序翻页;下一页由 nextMarker 驱动,上一页回放历史栈 -->
      <div class="pager">
        <button class="icon-btn" :disabled="pageIdx === 0 || loading" :aria-label="t('objectStorage.prevPage')" @click="prevPage">
          <NIcon :size="13"><ChevronLeft /></NIcon>
        </button>
        <span class="pager-info">{{ t('objectStorage.pageOf', { n: pageIdx + 1 }) }}</span>
        <button
          class="icon-btn"
          :disabled="!result?.nextMarker || loading"
          :aria-label="t('objectStorage.nextPage')"
          @click="nextPage"
        >
          <NIcon :size="13"><ChevronRight /></NIcon>
        </button>
      </div>
    </template>

    <!-- 新建桶:当前账户下建桶 + 自动登记新配置 -->
    <Teleport to="body">
      <div
        v-if="bucketModalOpen"
        class="modal-scrim"
        role="dialog"
        :aria-label="t('objectStorage.createBucketTitle')"
        aria-modal="true"
        @click.self="bucketModalOpen = false"
        @keydown.esc="bucketModalOpen = false"
      >
        <form class="modal" @submit.prevent="submitCreateBucket">
          <div class="modal-head">
            <span class="modal-icon"><NIcon :size="17"><Plus /></NIcon></span>
            <div>
              <h2 class="modal-title">{{ t('objectStorage.createBucketTitle') }}</h2>
              <p class="modal-sub">{{ t('objectStorage.createBucketSub', { account: current?.name ?? '' }) }}</p>
            </div>
          </div>
          <div class="modal-body">
            <label class="field">
              <span class="field-label">{{ t('objectStorage.bucketNameField') }}</span>
              <input
                ref="bucketInput"
                v-model="bucketName"
                class="field-input mono"
                type="text"
                :placeholder="t('objectStorage.bucketNamePlaceholder')"
                autocomplete="off"
                spellcheck="false"
              />
              <span v-if="bucketName !== '' && !bucketNameValid" class="field-hint field-hint--error">
                {{ t('objectStorage.bucketNameHint') }}
              </span>
            </label>
            <label class="field">
              <span class="field-label">{{ t('objectStorage.displayNameField') }}</span>
              <input
                v-model="bucketDisplayName"
                class="field-input"
                type="text"
                :placeholder="bucketName.trim().toLowerCase() || t('objectStorage.displayNamePlaceholder')"
                autocomplete="off"
              />
            </label>
            <p v-if="createError" class="modal-error">{{ createError }}</p>
          </div>
          <div class="modal-foot">
            <button type="button" class="btn-secondary" :disabled="creating" @click="bucketModalOpen = false">
              {{ t('objectStorage.cancel') }}
            </button>
            <button type="submit" class="btn-primary" :disabled="creating || !bucketNameValid" :aria-busy="creating">
              {{ creating ? t('objectStorage.creating') : t('objectStorage.createBucketConfirm') }}
            </button>
          </div>
        </form>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.obs-view {
  display: flex;
  flex-direction: column;
  gap: 18px;
}

.view-header {
  display: flex;
  align-items: flex-start;
  gap: 16px;
  padding-bottom: 18px;
  border-bottom: 1px solid var(--color-border);
}
.header-main {
  flex: 1;
}
.view-title {
  font-size: var(--text-display);
  font-weight: 700;
  letter-spacing: -0.02em;
  color: var(--color-text);
}
.view-sub {
  font-size: var(--text-body);
  color: var(--color-faint);
  margin-top: 2px;
}
.header-tools {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.store-select {
  height: 34px;
  padding: 0 10px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-family: var(--font-sans);
  font-size: 0.84rem;
  max-width: 420px;
}

.mono {
  font-family: var(--font-mono);
}

.empty-panel {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 72px 32px;
  text-align: center;
  border: 1.5px dashed var(--color-border-strong);
  border-radius: var(--rounded-card);
  color: var(--color-dim);
}
.empty-panel--slim {
  padding: 40px 32px;
}
.empty-title {
  font-size: 1rem;
  font-weight: 600;
  color: var(--color-text);
}
.empty-desc {
  font-size: 0.82rem;
  color: var(--color-faint);
  max-width: 48ch;
}

.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 34px;
  padding: 0 16px;
  background: var(--color-primary);
  color: var(--color-primary-contrast, #fff);
  border: none;
  border-radius: var(--rounded);
  font-size: 0.82rem;
  font-weight: 600;
  cursor: pointer;
  text-decoration: none;
}
.btn-secondary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 32px;
  padding: 0 12px;
  background: var(--color-card);
  color: var(--color-text);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  font-size: 0.8rem;
  font-weight: 500;
  cursor: pointer;
}
.btn-secondary:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}

.crumbs {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  font-size: 0.84rem;
}
.crumb-link {
  border: none;
  background: none;
  padding: 0;
  color: var(--color-primary);
  font-size: 0.84rem;
  font-weight: 500;
  cursor: pointer;
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.crumb-link:hover {
  text-decoration: underline;
}
.crumb-current {
  color: var(--color-text);
  font-weight: 600;
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.crumb-sep {
  color: var(--color-faint);
}

.obs-toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
}
.row-count {
  font-size: 0.74rem;
  color: var(--color-faint);
}
.page-size {
  height: 32px;
  padding: 0 8px;
  margin-left: auto;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-size: 0.78rem;
}

.obs-state {
  padding: 48px 24px;
  text-align: center;
  color: var(--color-faint);
  font-size: 0.84rem;
}
.obs-error {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
  padding: 40px 24px;
  text-align: center;
  border: 1px solid var(--color-red-line);
  background: var(--color-red-soft);
  border-radius: var(--rounded);
  color: var(--color-red);
  font-size: 0.82rem;
  word-break: break-all;
}

.grid-scroll {
  overflow-x: auto;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
}
.data-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 0.78rem;
}
.data-table th {
  text-align: left;
  padding: 8px 10px;
  font-size: 0.7rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  border-bottom: 1px solid var(--color-border);
  background: var(--color-card-2);
  white-space: nowrap;
}
.data-table td {
  padding: 8px 10px;
  border-bottom: 1px solid var(--color-border);
  color: var(--color-dim);
  vertical-align: top;
}
.data-table tbody tr:last-child td {
  border-bottom: none;
}
.row-click {
  cursor: pointer;
  transition: background-color var(--duration-fast);
}
.row-click:hover {
  background: var(--color-inset);
}
.cell-strong {
  color: var(--color-text);
  font-weight: 500;
}
.cell-dim {
  color: var(--color-faint);
}
.col-num {
  text-align: right;
  white-space: nowrap;
}
.name-cell {
  max-width: 520px;
}
.name-cell .mono {
  display: inline-block;
  vertical-align: middle;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 460px;
}
.icon-dir {
  color: var(--color-primary);
  vertical-align: middle;
  margin-right: 7px;
}
.icon-file {
  color: var(--color-faint);
  vertical-align: middle;
  margin-right: 7px;
}

.pager {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
}
.pager-info {
  font-size: 0.78rem;
  color: var(--color-dim);
}
.icon-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 30px;
  height: 30px;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  color: var(--color-dim);
  cursor: pointer;
}
.icon-btn:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.icon-btn:hover:not(:disabled) {
  color: var(--color-text);
}

/* ——— 新建桶弹窗(样式体系与设置页一致)——— */
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
  max-width: 480px;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-xl);
  box-shadow: var(--shadow-modal);
  overflow: hidden;
  animation: modal-in 0.35s var(--ease-out-expo) both;
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
.modal-body {
  padding: 18px 20px;
  display: flex;
  flex-direction: column;
  gap: 14px;
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
  box-sizing: border-box;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  padding: 0 12px;
  color: var(--color-text);
  font-family: var(--font-sans);
  font-size: 0.86rem;
  transition: border-color var(--duration-fast), box-shadow var(--duration-fast);
}
.field-input.mono {
  font-family: var(--font-mono);
  font-size: 0.8rem;
}
.field-input::placeholder {
  color: var(--color-faint);
}
.field-input:focus {
  outline: none;
  border-color: var(--color-primary);
  box-shadow: 0 0 0 3px var(--color-primary-soft);
}
.field-hint {
  font-size: 0.72rem;
  color: var(--color-faint);
  line-height: 1.4;
}
.field-hint--error {
  color: var(--color-red);
}
.modal-error {
  padding: 10px 12px;
  background: var(--color-red-soft);
  border: 1px solid var(--color-red-line);
  color: var(--color-red);
  border-radius: var(--rounded);
  font-size: 0.78rem;
  white-space: pre-wrap;
  word-break: break-all;
  margin: 0;
}
.modal-foot {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  padding: 14px 20px 18px;
  border-top: 1px solid var(--color-border);
}
</style>
