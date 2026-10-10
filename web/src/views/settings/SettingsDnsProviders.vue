<script setup lang="ts">
/*
  SettingsDnsProviders.vue — DNS 提供商管理(R3 / E3.1 · 零 DNS 体验)。

  挂接 Cloudflare / DNSPod / 阿里云 DNS 账户(一个账户托管多个根区),解锁两件事:
  ① 路由走 DNS-01 验证 → 通配符证书;② 一键分配子域名(app-xxxx.<根区> + 自动 A 记录 + 路由)。
  凭据按机密性拆分:API ID(DNSPod SecretId / 阿里云 AccessKeyId)非机密,明文可回显可编辑;
  API Secret 只写不读 —— 存入保险库,列表只显示「已配置 / 未配置」,绝不回显(可在编辑中轮换)。

  - 列表:类型徽章、名称、根区标签(多个)、凭据状态;每行「编辑」+「验证」(逐根区探测)+ 删除。
  - 添加弹窗:类型(三选一)+ 名称 + API ID(cloudflare 无)+ API Secret(必填,password)
    + 根区(动态多值,≥1)。
  - 编辑弹窗:改名称 / API ID、轮换 Secret(留空不改动);根区就地增删(Secret 只写一次,
    不必删了重建)。
  - 删除二次确认。验证结果逐根区以 toast 反馈。
  数据来自 GET /api/dns/providers(只读聚合,从不含 Secret)。
*/
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useDirtyGuard } from '../../composables/useDirtyGuard'
import { NIcon } from 'naive-ui'
import { World, Plus, Trash, CircleCheck, CircleX, ShieldCheck, Pencil } from '@vicons/tabler'
import {
  listDnsProviders,
  createDnsProvider,
  updateDnsProvider,
  deleteDnsProvider,
  verifyDnsProvider,
  addDnsZone,
  removeDnsZone,
  type DnsProvider,
  type DnsProviderType,
  type CreateDnsProviderInput,
  type ZoneVerifyResult,
} from '../../api/dnsProviders'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'

const { t } = useI18n()
const { confirmDiscard } = useDirtyGuard()
const formSnapshot = ref('')
const toast = useToast()

// ─── 列表加载 ──────────────────────────────────────────────────────────────────
type LoadState = 'idle' | 'loading' | 'error'
const loadState = ref<LoadState>('idle')
const loadError = ref('')
const providers = ref<DnsProvider[]>([])

const PROVIDER_TYPES: DnsProviderType[] = ['cloudflare', 'dnspod', 'alidns']
const typeLabels = computed<Record<DnsProviderType, string>>(() => ({
  cloudflare: t('dnsProviders.typeCloudflare'),
  dnspod: t('dnsProviders.typeDnspod'),
  alidns: t('dnsProviders.typeAlidns'),
}))

// 仅 dnspod / alidns 有 API ID 半段(cloudflare 单 Token)。
const needsApiId = (type: DnsProviderType): boolean => type !== 'cloudflare'

// Secret(机密,只写)与 API ID(非机密)分字段;标签/占位/提示按提供商类型切换。
const apiIdPlaceholders = computed<Record<'dnspod' | 'alidns', string>>(() => ({
  dnspod: t('dnsProviders.apiIdPlaceholderDnspod'),
  alidns: t('dnsProviders.apiIdPlaceholderAlidns'),
}))
const secretPlaceholders = computed<Record<DnsProviderType, string>>(() => ({
  cloudflare: t('dnsProviders.secretPlaceholderCloudflare'),
  dnspod: t('dnsProviders.secretPlaceholderDnspod'),
  alidns: t('dnsProviders.secretPlaceholderAlidns'),
}))
const secretHints = computed<Record<DnsProviderType, string>>(() => ({
  cloudflare: t('dnsProviders.secretHintCloudflare'),
  dnspod: t('dnsProviders.secretHintDnspod'),
  alidns: t('dnsProviders.secretHintAlidns'),
}))

async function load(): Promise<void> {
  loadState.value = 'loading'
  loadError.value = ''
  try {
    providers.value = await listDnsProviders()
    loadState.value = 'idle'
  } catch (err) {
    loadState.value = 'error'
    loadError.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('dnsProviders.errLoad', { status: err.status }))
        : t('dnsProviders.errNetwork')
  }
}

onMounted(load)

// 根域:与路由域名相同的 FQDN 校验(根域本身不带通配符)。
const FQDN_RE = /^(?=.{1,253}$)([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$/i

// ─── 添加弹窗 ──────────────────────────────────────────────────────────────────
const modalOpen = ref(false)
const submitting = ref(false)
const formBanner = ref('')
const form = ref<{
  type: DnsProviderType
  name: string
  apiId: string
  secret: string
  baseDomains: string[]
}>({ type: 'cloudflare', name: '', apiId: '', secret: '', baseDomains: [''] })
const errors = ref<{ name: string; apiId: string; secret: string; zones: string }>({
  name: '',
  apiId: '',
  secret: '',
  zones: '',
})

function openAdd(): void {
  form.value = { type: 'cloudflare', name: '', apiId: '', secret: '', baseDomains: [''] }
  errors.value = { name: '', apiId: '', secret: '', zones: '' }
  formBanner.value = ''
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

/** 切换提供商类型时清掉不适用的 API ID(cloudflare 无此半段)。 */
function onTypeChange(type: DnsProviderType): void {
  form.value.type = type
  if (!needsApiId(type)) form.value.apiId = ''
  errors.value.apiId = ''
}

function addZoneInput(): void {
  form.value.baseDomains.push('')
}

function removeZoneInput(i: number): void {
  form.value.baseDomains.splice(i, 1)
  if (form.value.baseDomains.length === 0) form.value.baseDomains.push('')
}

function validate(): boolean {
  errors.value = { name: '', apiId: '', secret: '', zones: '' }
  let ok = true
  if (!form.value.name.trim()) {
    errors.value.name = t('dnsProviders.valNameRequired')
    ok = false
  }
  if (needsApiId(form.value.type) && !form.value.apiId.trim()) {
    errors.value.apiId = t('dnsProviders.valApiIdRequired')
    ok = false
  }
  const domains = form.value.baseDomains.map((d) => d.trim().toLowerCase()).filter(Boolean)
  if (domains.length === 0) {
    errors.value.zones = t('dnsProviders.valZonesRequired')
    ok = false
  } else if (domains.some((d) => !FQDN_RE.test(d))) {
    errors.value.zones = t('dnsProviders.valBaseDomainInvalid')
    ok = false
  }
  if (!form.value.secret) {
    errors.value.secret = t('dnsProviders.valSecretRequired')
    ok = false
  }
  return ok
}

async function submit(): Promise<void> {
  if (!validate()) return
  submitting.value = true
  formBanner.value = ''
  try {
    const payload: CreateDnsProviderInput = {
      type: form.value.type,
      name: form.value.name.trim(),
      apiId: needsApiId(form.value.type) ? form.value.apiId.trim() : '',
      secret: form.value.secret,
      baseDomains: form.value.baseDomains.map((d) => d.trim().toLowerCase()).filter(Boolean),
    }
    const created = await createDnsProvider(payload)
    providers.value = [created, ...providers.value]
    form.value.secret = ''
    modalOpen.value = false
  } catch (err) {
    formBanner.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('dnsProviders.errSave', { status: err.status }))
        : t('dnsProviders.errSaveRetry')
  } finally {
    submitting.value = false
  }
}

// ─── 编辑弹窗(名称 / API ID / 轮换 Secret + 根区就地增删)────────────────────
const editOpen = ref(false)
const editSubmitting = ref(false)
const editBanner = ref('')
const editing = ref<DnsProvider | null>(null)
const editForm = ref<{ name: string; apiId: string; secret: string }>({ name: '', apiId: '', secret: '' })
const editErrors = ref<{ name: string; apiId: string }>({ name: '', apiId: '' })
const editSnapshot = ref('')
// 根区增删(立即生效,不等「保存」;Secret 只写一次,追加根区不必删了重建)。
const zoneInput = ref('')
const zoneInputError = ref('')
const zoneBusy = ref(false)

function openEdit(p: DnsProvider): void {
  editing.value = p
  editForm.value = { name: p.name, apiId: p.apiId, secret: '' }
  editErrors.value = { name: '', apiId: '' }
  editSnapshot.value = JSON.stringify(editForm.value)
  editBanner.value = ''
  zoneInput.value = ''
  zoneInputError.value = ''
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
  editErrors.value = { name: '', apiId: '' }
  let ok = true
  if (!editForm.value.name.trim()) {
    editErrors.value.name = t('dnsProviders.valNameRequired')
    ok = false
  }
  if (editing.value && needsApiId(editing.value.type) && !editForm.value.apiId.trim()) {
    editErrors.value.apiId = t('dnsProviders.valApiIdRequired')
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
    const payload: Record<string, string> = {
      name: editForm.value.name.trim(),
      apiId: needsApiId(editing.value.type) ? editForm.value.apiId.trim() : '',
    }
    if (editForm.value.secret) payload.secret = editForm.value.secret
    const updated = await updateDnsProvider(id, payload)
    const idx = providers.value.findIndex((p) => p.id === id)
    if (idx >= 0) providers.value[idx] = updated
    editForm.value.secret = ''
    editOpen.value = false
    editing.value = null
  } catch (err) {
    editBanner.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('dnsProviders.errSave', { status: err.status }))
        : t('dnsProviders.errSaveRetry')
  } finally {
    editSubmitting.value = false
  }
}

async function onAddZone(): Promise<void> {
  if (!editing.value || zoneBusy.value) return
  const domain = zoneInput.value.trim().toLowerCase()
  if (!domain) {
    zoneInputError.value = t('dnsProviders.valBaseDomainRequired')
    return
  }
  if (!FQDN_RE.test(domain)) {
    zoneInputError.value = t('dnsProviders.valBaseDomainInvalid')
    return
  }
  zoneBusy.value = true
  zoneInputError.value = ''
  const providerId = editing.value.id
  try {
    const zone = await addDnsZone(providerId, domain)
    const p = providers.value.find((x) => x.id === providerId)
    if (p) {
      p.zones = [...p.zones, zone].sort((a, b) => a.baseDomain.localeCompare(b.baseDomain))
      editing.value = p
    }
    zoneInput.value = ''
  } catch (err) {
    zoneInputError.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('dnsProviders.errSaveRetry'))
        : t('dnsProviders.errSaveRetry')
  } finally {
    zoneBusy.value = false
  }
}

async function onRemoveZone(zoneId: string): Promise<void> {
  if (!editing.value || zoneBusy.value) return
  zoneBusy.value = true
  zoneInputError.value = ''
  const providerId = editing.value.id
  try {
    await removeDnsZone(providerId, zoneId)
    const p = providers.value.find((x) => x.id === providerId)
    if (p) {
      p.zones = p.zones.filter((z) => z.id !== zoneId)
      editing.value = p
    }
  } catch (err) {
    zoneInputError.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('dnsProviders.errDelete', { status: err.status }))
        : t('dnsProviders.errNetwork')
  } finally {
    zoneBusy.value = false
  }
}

// ─── 验证(逐根区结果)────────────────────────────────────────────────────────
const verifyingId = ref<string | null>(null)

function verifyDetail(zones: ZoneVerifyResult[]): string {
  return zones.map((z) => (z.ok ? z.baseDomain : `${z.baseDomain}: ${z.error ?? ''}`)).join(' · ')
}

async function verify(p: DnsProvider): Promise<void> {
  if (verifyingId.value) return
  verifyingId.value = p.id
  try {
    const res = await verifyDnsProvider(p.id)
    const failed = res.zones.filter((z) => !z.ok).length
    if (res.ok) {
      toast.success(t('dnsProviders.verifyOk'), { detail: verifyDetail(res.zones) })
    } else {
      toast.error(
        failed === res.zones.length
          ? t('dnsProviders.verifyFail')
          : t('dnsProviders.verifyPartial', { failed, total: res.zones.length }),
        { detail: verifyDetail(res.zones) },
      )
    }
  } catch (err) {
    toast.error(t('dnsProviders.verifyFail'), {
      detail:
        err instanceof HttpError
          ? (err.apiError?.message ?? t('dnsProviders.errNetwork'))
          : t('dnsProviders.errNetwork'),
    })
  } finally {
    verifyingId.value = null
  }
}

// ─── 删除 ──────────────────────────────────────────────────────────────────────
const deleteOpen = ref(false)
const deleting = ref<DnsProvider | null>(null)
const deleteSubmitting = ref(false)
const deleteBanner = ref('')

function openDelete(p: DnsProvider): void {
  deleting.value = p
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
    const res = await deleteDnsProvider(id)
    if (res.ok) {
      providers.value = providers.value.filter((p) => p.id !== id)
      deleteOpen.value = false
      deleting.value = null
    } else {
      deleteBanner.value = t('dnsProviders.errDelete', { status: 0 })
    }
  } catch (err) {
    deleteBanner.value =
      err instanceof HttpError
        ? (err.apiError?.message ?? t('dnsProviders.errDelete', { status: err.status }))
        : t('dnsProviders.errNetwork')
  } finally {
    deleteSubmitting.value = false
  }
}
</script>

<template>
  <div class="dns-root">
    <!-- section header -->
    <div class="section-head">
      <div class="section-head-text">
        <h2 class="section-title">{{ t('dnsProviders.title') }}</h2>
        <p class="section-desc">{{ t('dnsProviders.desc') }}</p>
      </div>
      <button class="btn-primary" :disabled="loadState === 'loading'" @click="openAdd">
        <NIcon :size="14"><Plus /></NIcon>
        {{ t('dnsProviders.addProvider') }}
      </button>
    </div>

    <!-- load error -->
    <div v-if="loadState === 'error'" class="banner banner--error" role="alert">
      <span>⚠ {{ loadError }}</span>
      <button class="banner-retry" @click="load">↻ {{ t('dnsProviders.retry') }}</button>
    </div>

    <!-- panel -->
    <div class="panel">
      <div class="panel-head">
        <span>{{ t('dnsProviders.panelTitle') }}</span>
        <span v-if="loadState === 'idle'" class="panel-meta">{{ t('dnsProviders.countLabel', { n: providers.length }) }}</span>
      </div>

      <!-- loading -->
      <div v-if="loadState === 'loading'" class="state-msg">{{ t('common.refresh') }}…</div>

      <!-- empty -->
      <div v-else-if="loadState === 'idle' && providers.length === 0" class="empty-state">
        <div class="empty-icon" aria-hidden="true"><NIcon :size="22"><World /></NIcon></div>
        <p class="empty-label">{{ t('dnsProviders.emptyLabel') }}</p>
        <p class="empty-hint">{{ t('dnsProviders.emptyHint') }}</p>
        <button class="btn-primary" @click="openAdd">+ {{ t('dnsProviders.addFirstProvider') }}</button>
      </div>

      <!-- list -->
      <template v-else-if="loadState === 'idle'">
        <div class="dns-row dns-row--head" aria-hidden="true">
          <span>{{ t('dnsProviders.colType') }}</span>
          <span>{{ t('dnsProviders.colName') }}</span>
          <span>{{ t('dnsProviders.colZones') }}</span>
          <span>{{ t('dnsProviders.colCredential') }}</span>
          <span />
        </div>
        <div v-for="p in providers" :key="p.id" class="dns-row">
          <span class="type-badge" :class="`type-badge--${p.type}`">{{ typeLabels[p.type] }}</span>
          <div class="dns-name-cell">
            <strong class="dns-name">{{ p.name }}</strong>
            <span v-if="p.apiId" class="dns-apiid mono">{{ p.apiId }}</span>
          </div>
          <span v-if="p.zones.length" class="zone-tags">
            <span v-for="z in p.zones" :key="z.id" class="zone-tag mono">{{ z.baseDomain }}</span>
          </span>
          <span v-else class="zone-tags zone-tags--empty">—</span>
          <span class="cred-state" :class="p.credentialConfigured ? 'cred-state--ok' : 'cred-state--missing'">
            <NIcon :size="13"><CircleCheck v-if="p.credentialConfigured" /><CircleX v-else /></NIcon>
            {{ p.credentialConfigured ? t('dnsProviders.credConfigured') : t('dnsProviders.credMissing') }}
          </span>
          <span class="dns-ops">
            <button
              class="op-btn"
              :title="t('dnsProviders.editTitle', { name: p.name })"
              :aria-label="t('dnsProviders.editTitle', { name: p.name })"
              @click="openEdit(p)"
            >
              <NIcon :size="13"><Pencil /></NIcon>
              <span class="op-btn-txt">{{ t('dnsProviders.editBtn') }}</span>
            </button>
            <button
              class="op-btn"
              :disabled="verifyingId === p.id"
              :title="t('dnsProviders.verify')"
              @click="verify(p)"
            >
              <NIcon :size="13"><ShieldCheck /></NIcon>
              <span class="op-btn-txt">{{ verifyingId === p.id ? t('dnsProviders.verifying') : t('dnsProviders.verify') }}</span>
            </button>
            <button
              class="op-btn op-btn--danger"
              :title="t('dnsProviders.deleteTitle', { name: p.name })"
              :aria-label="t('dnsProviders.deleteAria', { name: p.name })"
              @click="openDelete(p)"
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
      :aria-label="t('dnsProviders.addTitle')"
      aria-modal="true"
      @keydown.esc="requestClose"
    >
      <div class="modal">
        <div class="modal-head">
          <div class="modal-icon" aria-hidden="true"><NIcon :size="18"><World /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('dnsProviders.addTitle') }}</h3>
            <p class="modal-sub">{{ t('dnsProviders.modalSub') }}</p>
          </div>
          <button class="modal-close" :aria-label="t('dnsProviders.closeDialog')" :disabled="submitting" @click="requestClose">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6 6 18M6 6l12 12"/></svg>
          </button>
        </div>

        <div v-if="formBanner" class="banner banner--error modal-banner" role="alert">{{ formBanner }}</div>

        <form class="modal-form" novalidate @submit.prevent="submit">
          <!-- type -->
          <div class="field">
            <label class="field-label">{{ t('dnsProviders.fieldType') }}</label>
            <div class="segmented" role="group">
              <button
                v-for="opt in PROVIDER_TYPES"
                :key="opt"
                type="button"
                class="seg-item"
                :class="{ 'seg-item--active': form.type === opt }"
                :disabled="submitting"
                @click="onTypeChange(opt)"
              >{{ typeLabels[opt] }}</button>
            </div>
          </div>

          <!-- name -->
          <div class="field">
            <label class="field-label" for="dns-name">{{ t('dnsProviders.fieldName') }}</label>
            <input
              id="dns-name"
              v-model="form.name"
              class="field-input"
              :class="{ 'field-input--error': errors.name }"
              type="text"
              :placeholder="t('dnsProviders.namePlaceholder')"
              :disabled="submitting"
              autocomplete="off"
              @input="errors.name = ''"
            />
            <span v-if="errors.name" class="field-error" role="alert">{{ errors.name }}</span>
          </div>

          <!-- API ID (non-secret; dnspod / alidns only) -->
          <div v-if="needsApiId(form.type)" class="field">
            <label class="field-label" for="dns-apiid">{{ t('dnsProviders.fieldApiId') }}</label>
            <input
              id="dns-apiid"
              v-model="form.apiId"
              class="field-input field-input--mono"
              :class="{ 'field-input--error': errors.apiId }"
              type="text"
              :placeholder="apiIdPlaceholders[form.type as 'dnspod' | 'alidns']"
              :disabled="submitting"
              autocomplete="off"
              spellcheck="false"
              @input="errors.apiId = ''"
            />
            <span v-if="errors.apiId" class="field-error" role="alert">{{ errors.apiId }}</span>
            <span class="field-hint">{{ t('dnsProviders.apiIdHint') }}</span>
          </div>

          <!-- secret (write-only) -->
          <div class="field">
            <label class="field-label" for="dns-secret">{{ t('dnsProviders.fieldSecret') }}</label>
            <input
              id="dns-secret"
              v-model="form.secret"
              class="field-input field-input--mono"
              :class="{ 'field-input--error': errors.secret }"
              type="password"
              :placeholder="secretPlaceholders[form.type]"
              :disabled="submitting"
              autocomplete="new-password"
              @input="errors.secret = ''"
            />
            <span v-if="errors.secret" class="field-error" role="alert">{{ errors.secret }}</span>
            <span class="field-hint">{{ secretHints[form.type] }}</span>
          </div>

          <!-- base domains (multi) -->
          <div class="field">
            <label class="field-label">{{ t('dnsProviders.fieldBaseDomains') }}</label>
            <div
              v-for="(_, i) in form.baseDomains"
              :key="i"
              class="zone-input-row"
            >
              <input
                v-model="form.baseDomains[i]"
                class="field-input field-input--mono"
                type="text"
                :placeholder="t('dnsProviders.baseDomainPlaceholder')"
                :disabled="submitting"
                :aria-label="`${t('dnsProviders.fieldBaseDomains')} ${i + 1}`"
                autocomplete="off"
                spellcheck="false"
                @input="errors.zones = ''"
              />
              <button
                v-if="form.baseDomains.length > 1"
                type="button"
                class="zone-rm"
                :aria-label="t('dnsProviders.removeZoneAria', { domain: form.baseDomains[i] || i + 1 })"
                :disabled="submitting"
                @click="removeZoneInput(i)"
              >✕</button>
            </div>
            <span v-if="errors.zones" class="field-error" role="alert">{{ errors.zones }}</span>
            <button type="button" class="zone-add" :disabled="submitting" @click="addZoneInput">
              + {{ t('dnsProviders.addZoneField') }}
            </button>
          </div>

          <div class="modal-footer">
            <button type="button" class="btn-secondary" :disabled="submitting" @click="requestClose">{{ t('dnsProviders.cancel') }}</button>
            <button type="submit" class="btn-primary" :disabled="submitting" :aria-busy="submitting">
              <span v-if="submitting" class="spinner" aria-hidden="true" />
              {{ submitting ? t('dnsProviders.saving') : t('dnsProviders.create') }}
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
      :aria-label="t('dnsProviders.editTitle', { name: editing.name })"
      aria-modal="true"
      @keydown.esc="requestCloseEdit"
    >
      <div class="modal">
        <div class="modal-head">
          <div class="modal-icon" aria-hidden="true"><NIcon :size="18"><Pencil /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('dnsProviders.editTitle', { name: editing.name }) }}</h3>
            <p class="modal-sub">{{ t('dnsProviders.editSub') }}</p>
          </div>
          <button class="modal-close" :aria-label="t('dnsProviders.closeDialog')" :disabled="editSubmitting" @click="requestCloseEdit">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6 6 18M6 6l12 12"/></svg>
          </button>
        </div>

        <div v-if="editBanner" class="banner banner--error modal-banner" role="alert">{{ editBanner }}</div>

        <form class="modal-form" novalidate @submit.prevent="submitEdit">
          <!-- name -->
          <div class="field">
            <label class="field-label" for="edit-dns-name">{{ t('dnsProviders.fieldName') }}</label>
            <input
              id="edit-dns-name"
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

          <!-- API ID (non-secret; dnspod / alidns only) -->
          <div v-if="needsApiId(editing.type)" class="field">
            <label class="field-label" for="edit-dns-apiid">{{ t('dnsProviders.fieldApiId') }}</label>
            <input
              id="edit-dns-apiid"
              v-model="editForm.apiId"
              class="field-input field-input--mono"
              :class="{ 'field-input--error': editErrors.apiId }"
              type="text"
              :placeholder="apiIdPlaceholders[editing.type as 'dnspod' | 'alidns']"
              :disabled="editSubmitting"
              autocomplete="off"
              spellcheck="false"
              @input="editErrors.apiId = ''"
            />
            <span v-if="editErrors.apiId" class="field-error" role="alert">{{ editErrors.apiId }}</span>
            <span class="field-hint">{{ t('dnsProviders.apiIdHint') }}</span>
          </div>

          <!-- secret rotation (optional) -->
          <div class="field">
            <label class="field-label" for="edit-dns-secret">{{ t('dnsProviders.fieldSecretRotate') }}</label>
            <input
              id="edit-dns-secret"
              v-model="editForm.secret"
              class="field-input field-input--mono"
              type="password"
              :placeholder="t('dnsProviders.secretRotatePlaceholder')"
              :disabled="editSubmitting"
              autocomplete="new-password"
            />
          </div>

          <!-- zone management (immediate effect) -->
          <div class="field">
            <label class="field-label">{{ t('dnsProviders.zonesSection') }}</label>
            <div class="zone-manage">
              <span v-for="z in editing.zones" :key="z.id" class="zone-tag zone-tag--managed mono">
                {{ z.baseDomain }}
                <button
                  type="button"
                  class="zone-rm zone-rm--inline"
                  :aria-label="t('dnsProviders.removeZoneTitle', { domain: z.baseDomain })"
                  :disabled="zoneBusy"
                  @click="onRemoveZone(z.id)"
                >✕</button>
              </span>
            </div>
            <div class="zone-input-row">
              <input
                v-model="zoneInput"
                class="field-input field-input--mono"
                type="text"
                :placeholder="t('dnsProviders.addZonePlaceholder')"
                :disabled="zoneBusy"
                aria-label="new zone"
                autocomplete="off"
                spellcheck="false"
                @keydown.enter.prevent="onAddZone"
                @input="zoneInputError = ''"
              />
              <button type="button" class="zone-add-btn" :disabled="zoneBusy" @click="onAddZone">
                {{ t('dnsProviders.addZoneBtn') }}
              </button>
            </div>
            <span v-if="zoneInputError" class="field-error" role="alert">{{ zoneInputError }}</span>
          </div>

          <div class="modal-footer">
            <button type="button" class="btn-secondary" :disabled="editSubmitting" @click="requestCloseEdit">{{ t('dnsProviders.cancel') }}</button>
            <button type="submit" class="btn-primary" :disabled="editSubmitting" :aria-busy="editSubmitting">
              <span v-if="editSubmitting" class="spinner" aria-hidden="true" />
              {{ editSubmitting ? t('dnsProviders.saving') : t('dnsProviders.save') }}
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
      :aria-label="t('dnsProviders.deleteConfirmTitle')"
      aria-modal="true"
      @keydown.esc="closeDelete"
    >
      <div class="modal modal--sm">
        <div class="modal-head">
          <div class="modal-icon modal-icon--danger" aria-hidden="true"><NIcon :size="18"><Trash /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('dnsProviders.deleteConfirmTitle') }}</h3>
            <p class="modal-sub">{{ t('dnsProviders.deleteIrreversible') }}</p>
          </div>
          <button class="modal-close" :aria-label="t('dnsProviders.closeDialog')" :disabled="deleteSubmitting" @click="closeDelete">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6 6 18M6 6l12 12"/></svg>
          </button>
        </div>
        <div class="modal-body">
          <p class="delete-text">
            {{ t('dnsProviders.deleteConfirmPrefix') }}
            <strong class="delete-name">{{ deleting.name }}</strong>
            {{ t('dnsProviders.deleteConfirmSuffix') }}
          </p>
          <div v-if="deleteBanner" class="banner banner--error modal-banner" role="alert">{{ deleteBanner }}</div>
        </div>
        <div class="modal-footer modal-footer--standalone">
          <button type="button" class="btn-secondary" :disabled="deleteSubmitting" @click="closeDelete">{{ t('dnsProviders.cancel') }}</button>
          <button type="button" class="btn-danger" :disabled="deleteSubmitting" :aria-busy="deleteSubmitting" @click="confirmDelete">
            <span v-if="deleteSubmitting" class="spinner spinner--red" aria-hidden="true" />
            {{ deleteSubmitting ? t('dnsProviders.deleting') : t('dnsProviders.confirmDelete') }}
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.dns-root {
  display: flex;
  flex-direction: column;
  gap: var(--card-gap);
}

/* section head */
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

/* panel */
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

/* empty */
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

/* table */
.dns-row {
  display: grid;
  grid-template-columns: 110px minmax(130px, 1.1fr) minmax(160px, 1.6fr) 120px auto;
  align-items: center;
  gap: 14px;
  padding: 13px 18px;
  border-bottom: 1px solid var(--color-border);
  transition: background-color var(--duration-fast);
}
.dns-row:last-child {
  border-bottom: none;
}
.dns-row:not(.dns-row--head):hover {
  background: var(--color-inset);
}
.dns-row--head {
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
.type-badge--cloudflare {
  color: #f38020;
  border-color: oklch(72% 0.16 56 / 0.4);
  background: oklch(72% 0.16 56 / 0.1);
}
.type-badge--dnspod {
  color: var(--color-primary);
  border-color: var(--color-primary-soft);
  background: var(--color-primary-soft);
}
.type-badge--alidns {
  color: var(--color-amber);
  border-color: var(--color-amber-line);
  background: var(--color-amber-soft);
}

.dns-name-cell {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.dns-name {
  font-size: 0.85rem;
  font-weight: 500;
  color: var(--color-text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.dns-apiid {
  font-size: 0.72rem;
  color: var(--color-faint);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* zone tags */
.zone-tags {
  display: flex;
  flex-wrap: wrap;
  gap: 5px;
  min-width: 0;
}
.zone-tags--empty {
  color: var(--color-faint);
  font-size: 0.78rem;
}
.zone-tag {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 0.72rem;
  padding: 2px 8px;
  border-radius: var(--rounded-full);
  border: 1px solid var(--color-border);
  background: var(--color-inset);
  color: var(--color-dim);
  white-space: nowrap;
}
.zone-manage {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
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

.dns-ops {
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

/* banner */
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

/* buttons */
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

/* modal */
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

/* fields */
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

/* zone multi-input rows */
.zone-input-row {
  display: flex;
  align-items: center;
  gap: 6px;
}
.zone-rm {
  flex-shrink: 0;
  width: 26px;
  height: 26px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-md);
  background: transparent;
  color: var(--color-faint);
  cursor: pointer;
  font-size: 0.74rem;
  line-height: 1;
  display: grid;
  place-items: center;
  transition: color var(--duration-fast), border-color var(--duration-fast), background-color var(--duration-fast);
}
.zone-rm:hover:not(:disabled) {
  color: var(--color-red);
  border-color: var(--color-red-line);
  background: var(--color-red-soft);
}
.zone-rm:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.zone-rm--inline {
  width: 18px;
  height: 18px;
  border: none;
  background: transparent;
}
.zone-add {
  align-self: flex-start;
  border: none;
  background: transparent;
  color: var(--color-primary);
  font-size: 0.78rem;
  font-weight: 600;
  cursor: pointer;
  padding: 2px 0;
}
.zone-add:hover:not(:disabled) {
  text-decoration: underline;
  text-underline-offset: 3px;
}
.zone-add:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.zone-add-btn {
  flex-shrink: 0;
  height: 38px;
  padding: 0 14px;
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  background: var(--color-card-2);
  color: var(--color-text);
  font-size: 0.8rem;
  font-weight: 500;
  cursor: pointer;
  transition: border-color var(--duration-fast);
}
.zone-add-btn:hover:not(:disabled) {
  border-color: var(--color-faint);
}
.zone-add-btn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

/* segmented */
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

/* delete body */
.delete-text {
  font-size: 0.86rem;
  color: var(--color-dim);
  line-height: 1.55;
}
.delete-name {
  color: var(--color-text);
  font-weight: 600;
}

/* spinner */
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
  .dns-row {
    grid-template-columns: 1fr 1fr;
    gap: 8px 12px;
  }
  .dns-row--head {
    display: none;
  }
  .dns-ops {
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
