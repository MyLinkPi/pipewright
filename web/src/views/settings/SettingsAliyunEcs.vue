<script setup lang="ts">
/*
  SettingsAliyunEcs.vue — 阿里云 ECS 批量导入(设置 → 阿里云 ECS)。

  上半页:多套账户配置(名称 + AccessKeyId + AccessKeySecret 只写 + Region 下拉,密文
  长期保存,留空 secret = 不修改),支持测连。
  下半页:导入向导 —— 选账户 → 拉取实例(手选机器范围,已登记的机器标「将更新」)→
  连接设置(默认内网 IP / 可切公网;SSH 用户 root / 端口 22)→ 每批统一的认证方式
  (选已有凭据 或 内联新建密码/私钥)→ 逐台结果(created/updated/failed 独立容错)。
*/
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { Cloud, Trash, PlugConnected, CircleCheck, CircleX, Refresh } from '@vicons/tabler'
import AppSelect from '../../components/ui/AppSelect.vue'
import {
  ALIYUN_REGIONS, aliyunRegionLabel,
  listAliyunAccounts, createAliyunAccount, updateAliyunAccount, deleteAliyunAccount,
  testAliyunAccount, listAliyunInstances, importAliyunInstances,
  type AliyunEcsAccount, type AliyunEcsInstance, type AliyunIpPreference,
  type AliyunInlineSecretKind, type AliyunImportItemResult,
} from '../../api/aliyunEcs'
import { listServers } from '../../api/servers'
import { listCredentials, type Credential } from '../../api/credentials'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'
import { useConfirm } from '../../composables/useConfirm'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 账户列表 ──────────────────────────────────────────────────────────────────
type LoadState = 'idle' | 'loading' | 'error'
const loadState = ref<LoadState>('idle')
const loadError = ref('')
const accounts = ref<AliyunEcsAccount[]>([])

async function load(): Promise<void> {
  loadState.value = 'loading'
  try {
    accounts.value = await listAliyunAccounts()
    if (selectedAccountId.value && !accounts.value.some((a) => a.id === selectedAccountId.value)) {
      selectedAccountId.value = ''
      instances.value = []
      selected.value = new Set()
    }
    loadState.value = 'idle'
  } catch (err) {
    loadState.value = 'error'
    loadError.value = errMsg(err, t('settingsAliyunEcs.errNetwork'))
  }
}

const testingId = ref('')

async function testRow(acc: AliyunEcsAccount): Promise<void> {
  if (testingId.value) return
  testingId.value = acc.id
  try {
    await testAliyunAccount(acc.id)
    toast.success(t('settingsAliyunEcs.testOk'))
  } catch (err) {
    toast.error(t('settingsAliyunEcs.testFail'), { detail: errMsg(err, t('settingsAliyunEcs.errNetwork')) })
  } finally {
    testingId.value = ''
  }
}

async function askDelete(acc: AliyunEcsAccount): Promise<void> {
  const ok = await confirm.open({
    title: t('settingsAliyunEcs.deleteTitle', { name: acc.name }),
    body: t('settingsAliyunEcs.deleteBody'),
    confirmLabel: t('settingsAliyunEcs.deleteConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteAliyunAccount(acc.id)
    toast.success(t('settingsAliyunEcs.deleted'))
    await load()
  } catch (err) {
    toast.error(t('settingsAliyunEcs.errDelete'), { detail: errMsg(err, t('settingsAliyunEcs.errNetwork')) })
  }
}

// ─── 账户编辑弹窗(secret 只写:编辑留空 = 不修改) ─────────────────────────────
const accountOpen = ref(false)
const accountSubmitting = ref(false)
const editingAccount = ref<AliyunEcsAccount | null>(null)
const accountForm = ref({
  name: '',
  accessKeyId: '',
  secret: '',
  region: 'cn-hangzhou',
})

function openCreate(): void {
  editingAccount.value = null
  accountForm.value = { name: '', accessKeyId: '', secret: '', region: 'cn-hangzhou' }
  accountOpen.value = true
}

function openEdit(acc: AliyunEcsAccount): void {
  editingAccount.value = acc
  accountForm.value = { name: acc.name, accessKeyId: acc.accessKeyId, secret: '', region: acc.region }
  accountOpen.value = true
}

async function submitAccount(): Promise<void> {
  if (accountSubmitting.value) return
  const f = accountForm.value
  if (!f.name.trim() || !f.accessKeyId.trim() || !f.region) {
    toast.error(t('settingsAliyunEcs.errAccountRequired'))
    return
  }
  if (!editingAccount.value && !f.secret.trim()) {
    toast.error(t('settingsAliyunEcs.errSecretRequired'))
    return
  }
  accountSubmitting.value = true
  try {
    if (editingAccount.value) {
      const body: Record<string, string> = {
        name: f.name.trim(),
        accessKeyId: f.accessKeyId.trim(),
        region: f.region,
      }
      if (f.secret.trim()) body.secret = f.secret.trim()
      await updateAliyunAccount(editingAccount.value.id, body)
      toast.success(t('settingsAliyunEcs.accountSaved'))
    } else {
      await createAliyunAccount({
        name: f.name.trim(),
        accessKeyId: f.accessKeyId.trim(),
        secret: f.secret.trim(),
        region: f.region,
      })
      toast.success(t('settingsAliyunEcs.accountCreated'))
    }
    accountOpen.value = false
    await load()
  } catch (err) {
    toast.error(t('settingsAliyunEcs.errAccountSave'), { detail: errMsg(err, t('settingsAliyunEcs.errNetwork')) })
  } finally {
    accountSubmitting.value = false
  }
}

// ─── 导入向导:账户/IP 偏好 + 实例列表 ────────────────────────────────────────
const selectedAccountId = ref('')
const ipPreference = ref<AliyunIpPreference>('private')
const instancesState = ref<LoadState>('idle')
const instancesError = ref('')
const instances = ref<AliyunEcsInstance[]>([])
const selected = ref<Set<string>>(new Set())

const accountOptions = computed(() =>
  accounts.value.map((a) => ({
    value: a.id,
    label: `${a.name} · ${aliyunRegionLabel(a.region)}`,
    hint: a.accessKeyId,
  })),
)

/** 已登记机器的 host 集合(用于「将更新」徽标;随导入成功刷新)。 */
const registeredHosts = ref<Set<string>>(new Set())

async function refreshRegisteredHosts(): Promise<void> {
  try {
    const servers = await listServers()
    registeredHosts.value = new Set(servers.map((s) => s.host))
  } catch {
    registeredHosts.value = new Set()
  }
}

/** 实例按当前 IP 偏好的连接地址(空 = 该实例无此类地址,导入将逐台报错)。 */
function targetHost(inst: AliyunEcsInstance): string {
  const list = ipPreference.value === 'public' ? inst.publicIps : inst.privateIps
  return list.length > 0 ? list[0] : ''
}

function willUpdate(inst: AliyunEcsInstance): boolean {
  const host = targetHost(inst)
  return host !== '' && registeredHosts.value.has(host)
}

async function loadInstances(): Promise<void> {
  if (!selectedAccountId.value || instancesState.value === 'loading') return
  instancesState.value = 'loading'
  selected.value = new Set()
  importResult.value = null
  try {
    instances.value = await listAliyunInstances(selectedAccountId.value)
    await refreshRegisteredHosts()
    instancesState.value = 'idle'
  } catch (err) {
    instancesState.value = 'error'
    instancesError.value = errMsg(err, t('settingsAliyunEcs.errNetwork'))
  }
}

function toggleSelect(id: string): void {
  const next = new Set(selected.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  selected.value = next
}

const allSelected = computed(
  () => instances.value.length > 0 && selected.value.size === instances.value.length,
)

function selectAll(): void {
  selected.value = new Set(instances.value.map((i) => i.instanceId))
}

function clearSelection(): void {
  selected.value = new Set()
}

// ─── 导入向导:连接与认证 ──────────────────────────────────────────────────────
const sshUser = ref('root')
const sshPort = ref(22)
const authMode = ref<'existing' | 'inline'>('existing')
const credentialId = ref('')
const inlineKind = ref<AliyunInlineSecretKind>('password')
const inlineName = ref('')
const inlineSecret = ref('')
const credentials = ref<Credential[]>([])

const sshCredentials = computed(() =>
  credentials.value.filter((c) => c.type === 'ssh_key' || c.type === 'ssh_password'),
)

const credentialOptions = computed(() =>
  sshCredentials.value.map((c) => ({
    value: c.id,
    label: c.name || c.maskedValue || c.id,
    hint: c.type,
  })),
)

async function loadCredentials(): Promise<void> {
  try {
    credentials.value = await listCredentials()
  } catch {
    credentials.value = []
  }
}

const importing = ref(false)
const importResult = ref<{ items: AliyunImportItemResult[]; total: number; ok: number; failed: number } | null>(null)

const canSubmit = computed(() => {
  if (!selectedAccountId.value || selected.value.size === 0 || importing.value) return false
  if (!sshUser.value.trim()) return false
  if (authMode.value === 'existing') return credentialId.value !== ''
  return inlineSecret.value.trim() !== ''
})

async function submitImport(): Promise<void> {
  if (!canSubmit.value || instancesState.value === 'loading') return
  if (selected.value.size > 100) {
    toast.error(t('settingsAliyunEcs.errTooMany'))
    return
  }
  importing.value = true
  importResult.value = null
  try {
    const res = await importAliyunInstances({
      accountId: selectedAccountId.value,
      instanceIds: [...selected.value],
      ipPreference: ipPreference.value,
      ...(authMode.value === 'existing'
        ? { credentialId: credentialId.value }
        : {
            inlineSecret: {
              kind: inlineKind.value,
              ...(inlineName.value.trim() ? { name: inlineName.value.trim() } : {}),
              secret: inlineSecret.value.trim(),
            },
          }),
      sshUser: sshUser.value.trim(),
      sshPort: Number(sshPort.value) || 22,
    })
    importResult.value = res
    if (res.failed === 0) {
      toast.success(t('settingsAliyunEcs.importAllOk', { n: res.ok }))
    } else {
      toast.error(t('settingsAliyunEcs.importPartial', { ok: res.ok, fail: res.failed }))
    }
    // 已登记集合刷新:「将更新」徽标与刚导入的结果对齐;凭据列表可能新增(内联建凭据)。
    await refreshRegisteredHosts()
    await loadCredentials()
  } catch (err) {
    toast.error(t('settingsAliyunEcs.errImport'), { detail: errMsg(err, t('settingsAliyunEcs.errNetwork')) })
  } finally {
    importing.value = false
  }
}

onMounted(() => {
  void load()
  void loadCredentials()
})
</script>

<template>
  <div class="settings-aliyun">
    <header class="page-head">
      <div>
        <h2 class="page-title">{{ t('settingsAliyunEcs.title') }}</h2>
        <p class="page-desc">{{ t('settingsAliyunEcs.desc') }}</p>
      </div>
      <button class="btn-primary" @click="openCreate">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" aria-hidden="true">
          <path d="M12 5v14M5 12h14" />
        </svg>
        {{ t('settingsAliyunEcs.addAccount') }}
      </button>
    </header>

    <!-- 账户列表 -->
    <div v-if="loadState === 'loading'" class="load-state">{{ t('settingsAliyunEcs.loading') }}</div>
    <div v-else-if="loadState === 'error'" class="load-state">
      <p>{{ loadError }}</p>
      <button class="btn-secondary" @click="load">{{ t('settingsAliyunEcs.retry') }}</button>
    </div>
    <div v-else-if="accounts.length === 0" class="empty-state">
      <div class="empty-icon" aria-hidden="true"><NIcon :size="22"><Cloud /></NIcon></div>
      <p class="empty-label">{{ t('settingsAliyunEcs.emptyLabel') }}</p>
      <p class="empty-hint">{{ t('settingsAliyunEcs.emptyHint') }}</p>
    </div>
    <template v-else>
      <div class="list-head">
        <span class="list-title">{{ t('settingsAliyunEcs.panelAccounts') }}</span>
        <span class="list-count">{{ t('settingsAliyunEcs.countLabel', { n: accounts.length }) }}</span>
      </div>
      <div class="acc-row acc-row--head" aria-hidden="true">
        <span>{{ t('settingsAliyunEcs.colName') }}</span>
        <span>{{ t('settingsAliyunEcs.colRegion') }}</span>
        <span>{{ t('settingsAliyunEcs.colAccessKeyId') }}</span>
        <span />
      </div>
      <div v-for="acc in accounts" :key="acc.id" class="acc-row">
        <strong class="acc-name">{{ acc.name }}</strong>
        <span class="mono">{{ acc.region }}<template v-if="!ALIYUN_REGIONS.some(r => r.value === acc.region)"> ⚠</template></span>
        <span class="mono ak">{{ acc.accessKeyId }}</span>
        <span class="row-ops">
          <button class="op-btn" :disabled="testingId === acc.id" @click="testRow(acc)">
            <NIcon :size="13"><PlugConnected /></NIcon>
            <span class="op-btn-txt">{{ testingId === acc.id ? t('settingsAliyunEcs.testing') : t('settingsAliyunEcs.test') }}</span>
          </button>
          <button class="op-btn" @click="openEdit(acc)">
            <NIcon :size="13"><Refresh /></NIcon>
            <span class="op-btn-txt">{{ t('settingsAliyunEcs.edit') }}</span>
          </button>
          <button class="op-btn op-btn--danger" :title="t('settingsAliyunEcs.deleteConfirm')" @click="askDelete(acc)">
            <NIcon :size="13"><Trash /></NIcon>
          </button>
        </span>
      </div>
    </template>

    <!-- 账户编辑弹窗 -->
    <Teleport to="body">
      <div
        v-if="accountOpen"
        class="modal-scrim"
        role="dialog"
        :aria-label="editingAccount ? t('settingsAliyunEcs.editTitle') : t('settingsAliyunEcs.createTitle')"
        aria-modal="true"
        @click.self="accountOpen = false"
        @keydown.esc="accountOpen = false"
      >
        <form class="modal" @submit.prevent="submitAccount">
          <div class="modal-head">
            <span class="modal-icon"><NIcon :size="17"><Cloud /></NIcon></span>
            <div>
              <h2 class="modal-title">{{ editingAccount ? t('settingsAliyunEcs.editTitle') : t('settingsAliyunEcs.createTitle') }}</h2>
              <p class="modal-sub">{{ t('settingsAliyunEcs.accountSub') }}</p>
            </div>
          </div>
          <div class="modal-body">
            <label class="field">
              <span class="field-label">{{ t('settingsAliyunEcs.fieldName') }}</span>
              <input v-model="accountForm.name" class="field-input" type="text" :placeholder="t('settingsAliyunEcs.namePlaceholder')" />
            </label>
            <div class="field-row">
              <label class="field">
                <span class="field-label">{{ t('settingsAliyunEcs.fieldAccessKeyId') }}</span>
                <input v-model="accountForm.accessKeyId" class="field-input mono" type="text" placeholder="LTAI5t…" autocomplete="off" />
              </label>
              <label class="field">
                <span class="field-label">{{ t('settingsAliyunEcs.fieldSecret') }}</span>
                <input
                  v-model="accountForm.secret"
                  class="field-input mono"
                  type="password"
                  :placeholder="editingAccount ? t('settingsAliyunEcs.secretKeep') : t('settingsAliyunEcs.secretPlaceholder')"
                  autocomplete="new-password"
                />
              </label>
            </div>
            <label class="field">
              <span class="field-label">{{ t('settingsAliyunEcs.fieldRegion') }}</span>
              <AppSelect
                v-model="accountForm.region"
                :options="[...ALIYUN_REGIONS]"
                :searchable="true"
                :search-placeholder="t('settingsAliyunEcs.regionSearch')"
                :height="'36px'"
                :portal="true"
              />
            </label>
            <p class="hint">{{ t('settingsAliyunEcs.secretHint') }}</p>
          </div>
          <div class="modal-foot">
            <button type="button" class="btn-secondary" :disabled="accountSubmitting" @click="accountOpen = false">
              {{ t('settingsAliyunEcs.cancel') }}
            </button>
            <button type="submit" class="btn-primary" :disabled="accountSubmitting" :aria-busy="accountSubmitting">
              {{ accountSubmitting ? t('settingsAliyunEcs.saving') : t('settingsAliyunEcs.save') }}
            </button>
          </div>
        </form>
      </div>
    </Teleport>

    <!-- ─── 导入向导 ──────────────────────────────────────────────────────────── -->
    <div class="wizard-head">
      <h3 class="section-title">{{ t('settingsAliyunEcs.importTitle') }}</h3>
      <p class="section-desc">{{ t('settingsAliyunEcs.importDesc') }}</p>
    </div>

    <div v-if="accounts.length === 0" class="banner" role="status">
      {{ t('settingsAliyunEcs.needAccountFirst') }}
    </div>
    <template v-else>
      <!-- 步骤 1:账户 + IP 偏好 + 拉取 -->
      <div class="wiz-panel">
        <div class="wiz-panel-title">{{ t('settingsAliyunEcs.stepPick') }}</div>
        <div class="wiz-row">
          <label class="field field--grow">
            <span class="field-label">{{ t('settingsAliyunEcs.fieldAccount') }}</span>
            <AppSelect
              v-model="selectedAccountId"
              :options="accountOptions"
              :placeholder="t('settingsAliyunEcs.accountPlaceholder')"
              :height="'36px'"
              :portal="true"
            />
          </label>
          <div class="field">
            <span class="field-label">{{ t('settingsAliyunEcs.fieldIpPreference') }}</span>
            <div class="radio-group" role="radiogroup" :aria-label="t('settingsAliyunEcs.fieldIpPreference')">
              <label class="radio">
                <input v-model="ipPreference" type="radio" value="private" />
                {{ t('settingsAliyunEcs.ipPrivate') }}
              </label>
              <label class="radio">
                <input v-model="ipPreference" type="radio" value="public" />
                {{ t('settingsAliyunEcs.ipPublic') }}
              </label>
            </div>
          </div>
          <button class="btn-secondary btn-load" :disabled="!selectedAccountId || instancesState === 'loading'" @click="loadInstances">
            <NIcon :size="13"><Refresh /></NIcon>
            {{ instancesState === 'loading' ? t('settingsAliyunEcs.loadingInstances') : t('settingsAliyunEcs.loadInstances') }}
          </button>
        </div>
        <p class="hint">{{ ipPreference === 'private' ? t('settingsAliyunEcs.ipPrivateHint') : t('settingsAliyunEcs.ipPublicHint') }}</p>
      </div>

      <!-- 步骤 2:实例列表(手选机器范围) -->
      <div v-if="instancesState === 'error'" class="banner banner--warn" role="status">
        {{ instancesError }}
      </div>
      <div v-else-if="instances.length > 0" class="wiz-panel">
        <div class="wiz-panel-title-row">
          <span class="wiz-panel-title">{{ t('settingsAliyunEcs.stepSelect') }}</span>
          <span class="sel-count">{{ t('settingsAliyunEcs.selectedCount', { n: selected.size, total: instances.length }) }}</span>
          <button class="link-btn" :disabled="allSelected" @click="selectAll">{{ t('settingsAliyunEcs.selectAll') }}</button>
          <button class="link-btn" :disabled="selected.size === 0" @click="clearSelection">{{ t('settingsAliyunEcs.clearSelection') }}</button>
        </div>
        <div class="inst-table" role="table">
          <div class="inst-row inst-row--head" role="row">
            <span class="cell cell--check" />
            <span class="cell">{{ t('settingsAliyunEcs.colInstance') }}</span>
            <span class="cell">{{ t('settingsAliyunEcs.colPrivateIps') }}</span>
            <span class="cell">{{ t('settingsAliyunEcs.colPublicIps') }}</span>
            <span class="cell">{{ t('settingsAliyunEcs.colOsStatus') }}</span>
          </div>
          <label
            v-for="inst in instances"
            :key="inst.instanceId"
            class="inst-row"
            :class="{ 'inst-row--sel': selected.has(inst.instanceId) }"
            role="row"
          >
            <span class="cell cell--check">
              <input
                type="checkbox"
                :checked="selected.has(inst.instanceId)"
                :aria-label="t('settingsAliyunEcs.selectInstance', { name: inst.name || inst.instanceId })"
                @change="toggleSelect(inst.instanceId)"
              />
            </span>
            <span class="cell">
              <span class="inst-name">{{ inst.name || inst.instanceId }}</span>
              <span class="mono inst-id">{{ inst.instanceId }}</span>
              <span v-if="willUpdate(inst)" class="badge badge--update">{{ t('settingsAliyunEcs.willUpdate') }}</span>
            </span>
            <span class="cell mono">{{ inst.privateIps.length ? inst.privateIps.join(', ') : '—' }}</span>
            <span class="cell mono">{{ inst.publicIps.length ? inst.publicIps.join(', ') : '—' }}</span>
            <span class="cell">
              <span class="os" :class="inst.osType === 'windows' ? 'os--win' : 'os--linux'">{{ inst.osType || '?' }}</span>
              <span class="inst-status" :class="inst.status === 'Running' ? 'st--run' : 'st--stop'">
                {{ inst.status === 'Running' ? t('settingsAliyunEcs.statusRunning') : inst.status }}
              </span>
            </span>
          </label>
        </div>
      </div>

      <!-- 步骤 3:连接 + 认证(每批统一) -->
      <div v-if="instances.length > 0" class="wiz-panel">
        <div class="wiz-panel-title">{{ t('settingsAliyunEcs.stepAuth') }}</div>
        <div class="wiz-row">
          <label class="field">
            <span class="field-label">{{ t('settingsAliyunEcs.fieldSshUser') }}</span>
            <input v-model="sshUser" class="field-input mono" type="text" placeholder="root" />
          </label>
          <label class="field">
            <span class="field-label">{{ t('settingsAliyunEcs.fieldSshPort') }}</span>
            <input v-model.number="sshPort" class="field-input" type="number" min="1" max="65535" />
          </label>
        </div>
        <div class="field">
          <span class="field-label">{{ t('settingsAliyunEcs.fieldAuthMode') }}</span>
          <div class="radio-group" role="radiogroup" :aria-label="t('settingsAliyunEcs.fieldAuthMode')">
            <label class="radio">
              <input v-model="authMode" type="radio" value="existing" />
              {{ t('settingsAliyunEcs.authExisting') }}
            </label>
            <label class="radio">
              <input v-model="authMode" type="radio" value="inline" />
              {{ t('settingsAliyunEcs.authInline') }}
            </label>
          </div>
        </div>

        <div v-if="authMode === 'existing'" class="auth-existing">
          <label class="field">
            <span class="field-label">{{ t('settingsAliyunEcs.fieldCredential') }}</span>
            <AppSelect
              v-model="credentialId"
              :options="credentialOptions"
              :placeholder="t('settingsAliyunEcs.credentialPlaceholder')"
              :height="'36px'"
              :portal="true"
            />
          </label>
          <p v-if="credentialOptions.length === 0" class="hint hint--warn">
            {{ t('settingsAliyunEcs.noCredentials') }}
          </p>
          <p class="hint">{{ t('settingsAliyunEcs.authExistingHint') }}</p>
        </div>

        <div v-else class="auth-inline">
          <div class="wiz-row">
            <div class="field">
              <span class="field-label">{{ t('settingsAliyunEcs.fieldInlineKind') }}</span>
              <div class="radio-group" role="radiogroup" :aria-label="t('settingsAliyunEcs.fieldInlineKind')">
                <label class="radio">
                  <input v-model="inlineKind" type="radio" value="password" />
                  {{ t('settingsAliyunEcs.inlinePassword') }}
                </label>
                <label class="radio">
                  <input v-model="inlineKind" type="radio" value="key" />
                  {{ t('settingsAliyunEcs.inlineKey') }}
                </label>
              </div>
            </div>
            <label class="field field--grow">
              <span class="field-label">{{ t('settingsAliyunEcs.fieldInlineName') }}</span>
              <input v-model="inlineName" class="field-input" type="text" :placeholder="t('settingsAliyunEcs.inlineNamePlaceholder')" />
            </label>
          </div>
          <label class="field">
            <span class="field-label">
              {{ inlineKind === 'password' ? t('settingsAliyunEcs.fieldInlinePassword') : t('settingsAliyunEcs.fieldInlineKey') }}
            </span>
            <textarea
              v-if="inlineKind === 'key'"
              v-model="inlineSecret"
              class="field-input field-textarea mono"
              rows="4"
              placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
              autocomplete="off"
            />
            <input
              v-else
              v-model="inlineSecret"
              class="field-input mono"
              type="password"
              placeholder="••••••••"
              autocomplete="new-password"
            />
          </label>
          <p class="hint">{{ t('settingsAliyunEcs.authInlineHint') }}</p>
        </div>

        <div class="import-foot">
          <button class="btn-primary" :disabled="!canSubmit" :aria-busy="importing" @click="submitImport">
            {{ importing ? t('settingsAliyunEcs.importing') : t('settingsAliyunEcs.importButton', { n: selected.size }) }}
          </button>
        </div>
      </div>

      <!-- 导入结果:逐台 + 汇总 -->
      <div v-if="importResult" class="wiz-panel">
        <div class="wiz-panel-title-row">
          <span class="wiz-panel-title">{{ t('settingsAliyunEcs.resultTitle') }}</span>
          <span class="sel-count" :class="importResult.failed > 0 ? 'st--stop' : 'st--run'">
            {{ t('settingsAliyunEcs.resultSummary', { ok: importResult.ok, total: importResult.total }) }}
          </span>
        </div>
        <div class="res-list">
          <div v-for="item in importResult.items" :key="item.instanceId" class="res-row" :class="{ 'res-row--fail': !item.ok }">
            <NIcon :size="14" class="res-ico">
              <CircleCheck v-if="item.ok" />
              <CircleX v-else />
            </NIcon>
            <span class="inst-name">{{ item.name || item.instanceId }}</span>
            <span class="mono inst-id">{{ item.instanceId }}</span>
            <span v-if="item.ok" class="badge" :class="item.action === 'updated' ? 'badge--update' : 'badge--new'">
              {{ item.action === 'updated' ? t('settingsAliyunEcs.actionUpdated') : t('settingsAliyunEcs.actionCreated') }}
            </span>
            <span v-if="!item.ok" class="res-err">{{ item.error }}</span>
          </div>
        </div>
        <p class="hint">
          <router-link to="/settings/servers" class="link-inline">{{ t('settingsAliyunEcs.goServers') }} →</router-link>
        </p>
      </div>
    </template>
  </div>
</template>

<style scoped>
.settings-aliyun {
  display: flex;
  flex-direction: column;
  gap: 20px;
  max-width: 980px;
}
.page-head {
  display: flex;
  align-items: flex-start;
  gap: 16px;
}
.page-title {
  font-size: 1.15rem;
  font-weight: 700;
  color: var(--color-text);
}
.page-desc {
  font-size: 0.82rem;
  color: var(--color-faint);
  margin-top: 4px;
  max-width: 68ch;
  line-height: 1.5;
}
.load-state {
  padding: 32px 20px;
  text-align: center;
  color: var(--color-faint);
  font-size: 0.82rem;
  display: flex;
  flex-direction: column;
  gap: 10px;
  align-items: center;
}
.empty-state {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 40px 24px;
  text-align: center;
  border: 1.5px dashed var(--color-border-strong);
  border-radius: var(--rounded-card);
  color: var(--color-dim);
}
.empty-icon {
  width: 40px;
  height: 40px;
  display: grid;
  place-items: center;
  border-radius: var(--rounded-lg);
  background: var(--color-primary-soft);
  color: var(--color-primary);
}
.empty-label { font-size: 0.92rem; font-weight: 600; color: var(--color-text); }
.empty-hint { font-size: 0.78rem; color: var(--color-faint); max-width: 60ch; }

.list-head { display: flex; align-items: baseline; gap: 10px; }
.list-title { font-size: 0.9rem; font-weight: 600; color: var(--color-text); }
.list-count { font-size: 0.74rem; color: var(--color-faint); }

.acc-row {
  display: grid;
  grid-template-columns: 1.2fr 0.9fr 1.4fr auto;
  gap: 10px;
  align-items: center;
  padding: 12px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-card);
  font-size: 0.82rem;
}
.acc-row + .acc-row { margin-top: 8px; }
.acc-row--head {
  background: none;
  border: none;
  padding-bottom: 0;
  font-size: 0.7rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.acc-row--head + .acc-row { margin-top: 0; }
.acc-name { color: var(--color-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ak { color: var(--color-dim); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.row-ops { display: flex; align-items: center; gap: 6px; justify-content: flex-end; }
.op-btn {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  height: 28px;
  padding: 0 9px;
  background: var(--color-card);
  color: var(--color-dim);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  font-size: 0.74rem;
  cursor: pointer;
}
.op-btn:hover:not(:disabled) { color: var(--color-text); border-color: var(--color-border-strong); }
.op-btn:disabled { opacity: 0.5; cursor: not-allowed; }
.op-btn--danger { color: var(--color-red); }

.modal-scrim {
  position: fixed;
  inset: 0;
  background: oklch(0% 0 0 / 0.62);
  display: grid;
  place-items: center;
  z-index: 100;
  padding: 24px;
}
.modal {
  width: 100%;
  max-width: 560px;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-xl);
  box-shadow: var(--shadow-modal);
  overflow: hidden;
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
.modal-title { font-size: 1rem; font-weight: 600; color: var(--color-text); margin-top: 2px; }
.modal-sub { font-size: 0.78rem; color: var(--color-faint); margin-top: 3px; }
.modal-body { padding: 18px 20px; display: flex; flex-direction: column; gap: 14px; }
.modal-foot {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  padding: 14px 20px 18px;
  border-top: 1px solid var(--color-border);
}
.field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.field--grow { flex: 1; }
.field-row { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.field-label { font-size: 0.76rem; font-weight: 500; color: var(--color-dim); }
.field-input {
  width: 100%;
  height: 36px;
  box-sizing: border-box;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  padding: 0 10px;
  color: var(--color-text);
  font-family: var(--font-sans);
  font-size: 0.84rem;
}
.field-input.mono { font-family: var(--font-mono); font-size: 0.8rem; }
.field-textarea {
  height: auto;
  padding: 8px 10px;
  resize: vertical;
  line-height: 1.45;
}
.field-input:focus {
  outline: none;
  border-color: var(--color-primary);
  box-shadow: 0 0 0 3px var(--color-primary-soft);
}
.hint { font-size: 0.74rem; color: var(--color-faint); line-height: 1.5; }
.hint--warn { color: var(--color-red); }

.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 36px;
  padding: 0 16px;
  background: var(--color-primary);
  color: var(--color-primary-contrast, #fff);
  border: none;
  border-radius: var(--rounded);
  font-size: 0.82rem;
  font-weight: 600;
  cursor: pointer;
  flex-shrink: 0;
}
.btn-primary:disabled { opacity: 0.55; cursor: not-allowed; }
.btn-secondary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 36px;
  padding: 0 12px;
  background: var(--color-card);
  color: var(--color-text);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  font-size: 0.8rem;
  font-weight: 500;
  cursor: pointer;
  flex-shrink: 0;
}
.btn-secondary:disabled { opacity: 0.55; cursor: not-allowed; }
.mono { font-family: var(--font-mono); }

.wizard-head { margin-top: 12px; }
.section-title { font-size: 0.98rem; font-weight: 700; color: var(--color-text); }
.section-desc { font-size: 0.8rem; color: var(--color-faint); margin-top: 4px; max-width: 72ch; line-height: 1.5; }
.banner {
  padding: 12px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-inset);
  color: var(--color-dim);
  font-size: 0.8rem;
}
.banner--warn { border-color: var(--color-red); color: var(--color-red); }

.wiz-panel {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  background: var(--color-card);
}
.wiz-panel-title { font-size: 0.84rem; font-weight: 600; color: var(--color-text); }
.wiz-panel-title-row { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
.wiz-row { display: flex; gap: 12px; align-items: flex-end; flex-wrap: wrap; }
.btn-load { margin-left: auto; }
.sel-count { font-size: 0.74rem; color: var(--color-faint); }
.link-btn {
  background: none;
  border: none;
  padding: 0;
  font-size: 0.74rem;
  color: var(--color-primary);
  cursor: pointer;
}
.link-btn:disabled { color: var(--color-faint); cursor: not-allowed; }
.link-inline { color: var(--color-primary); }

.radio-group { display: flex; gap: 14px; height: 36px; align-items: center; }
.radio {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 0.8rem;
  color: var(--color-dim);
  cursor: pointer;
}
.radio input { accent-color: var(--color-primary); }

.inst-table {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  overflow: hidden;
}
.inst-row {
  display: grid;
  grid-template-columns: 34px 1.5fr 1fr 1fr 0.9fr;
  gap: 8px;
  align-items: center;
  padding: 9px 12px;
  font-size: 0.78rem;
  border-top: 1px solid var(--color-border);
  cursor: pointer;
}
.inst-row:first-child { border-top: none; }
.inst-row--head {
  background: var(--color-inset);
  font-size: 0.68rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  cursor: default;
}
.inst-row--sel { background: var(--color-primary-soft); }
.cell { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--color-dim); }
.cell--check { display: grid; place-items: center; }
.cell--check input { accent-color: var(--color-primary); }
.inst-name { color: var(--color-text); font-weight: 500; margin-right: 6px; }
.inst-id { color: var(--color-faint); font-size: 0.7rem; margin-right: 6px; }
.badge {
  display: inline-block;
  font-size: 0.64rem;
  padding: 1px 7px;
  border-radius: var(--rounded-full);
  background: var(--color-inset);
  color: var(--color-dim);
  font-weight: 600;
  vertical-align: middle;
}
.badge--update { background: var(--color-primary-soft); color: var(--color-primary); }
.badge--new { background: var(--color-primary-soft); color: var(--color-primary); }
.os {
  display: inline-block;
  font-size: 0.66rem;
  padding: 1px 6px;
  border-radius: var(--rounded-full);
  font-weight: 600;
  margin-right: 6px;
}
.os--linux { background: var(--color-inset); color: var(--color-dim); }
.os--win { background: var(--color-inset); color: var(--color-dim); }
.inst-status { font-size: 0.72rem; }
.st--run { color: var(--color-green); font-weight: 600; }
.st--stop { color: var(--color-faint); }

.auth-existing, .auth-inline { display: flex; flex-direction: column; gap: 12px; }
.import-foot { display: flex; justify-content: flex-end; }

.res-list { display: flex; flex-direction: column; gap: 6px; }
.res-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  font-size: 0.78rem;
  min-width: 0;
}
.res-row--fail { border-color: var(--color-red); }
.res-ico { flex-shrink: 0; color: var(--color-green); }
.res-row--fail .res-ico { color: var(--color-red); }
.res-err {
  margin-left: auto;
  color: var(--color-red);
  font-size: 0.72rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

@media (max-width: 960px) {
  .acc-row { grid-template-columns: 1fr 1fr; row-gap: 6px; }
  .inst-row { grid-template-columns: 30px 1.6fr 1fr 0.9fr; }
  .inst-row .cell:nth-child(4) { display: none; }
}
</style>
