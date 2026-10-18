<script setup lang="ts">
/*
  SettingsPVE.vue — PVE 集群 Token 配置(设置 → PVE)。

  集群记录由**服务器登记自动探测生成**(在「设置 → 服务器」登记 PVE 物理机 → 自动建
  pve-<集群名> 池并登记全部节点,kind=pve_node,不可作为构建/部署目标)。本页只做:
  API Token 配置(密文存储,绝不回显)、测连、只读开关(解锁电源操作)、删除。
*/
import { ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { Server, Trash, PlugConnected, CircleCheck, CircleX, ShieldLock, ShieldOff } from '@vicons/tabler'
import {
  listPVEClusters, setPVEToken, testPVECluster, setPVEReadonly, deletePVECluster,
  type PVECluster,
} from '../../api/pveAdmin'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'
import { useConfirm } from '../../composables/useConfirm'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 列表加载 ──────────────────────────────────────────────────────────────────
type LoadState = 'idle' | 'loading' | 'error'
const loadState = ref<LoadState>('idle')
const loadError = ref('')
const clusters = ref<PVECluster[]>([])

async function load(): Promise<void> {
  loadState.value = 'loading'
  try {
    clusters.value = await listPVEClusters()
    loadState.value = 'idle'
  } catch (err) {
    loadState.value = 'error'
    loadError.value = errMsg(err, t('pveAdmin.errNetwork'))
  }
}

// ─── 行操作:测连 / 只读开关 / 删除 ────────────────────────────────────────────
const testingId = ref('')
const togglingId = ref('')

async function testRow(cl: PVECluster): Promise<void> {
  if (testingId.value) return
  testingId.value = cl.id
  try {
    await testPVECluster(cl.id)
    toast.success(t('pveAdmin.testOk'))
    await load()
  } catch (err) {
    toast.error(t('pveAdmin.testFail'), { detail: errMsg(err, t('pveAdmin.errNetwork')) })
  } finally {
    testingId.value = ''
  }
}

async function toggleReadonly(cl: PVECluster): Promise<void> {
  if (togglingId.value) return
  if (cl.readonly) {
    const ok = await confirm.open({
      title: t('pveSettings.unlockTitle', { name: cl.name }),
      body: t('pveSettings.unlockBody'),
      confirmLabel: t('pveSettings.unlockConfirm'),
      variant: 'danger',
    })
    if (!ok) return
  }
  togglingId.value = cl.id
  try {
    await setPVEReadonly(cl.id, !cl.readonly)
    toast.success(cl.readonly ? t('pveSettings.unlocked') : t('pveSettings.locked'))
    await load()
  } catch (err) {
    toast.error(t('pveSettings.errToggle'), { detail: errMsg(err, t('pveAdmin.errNetwork')) })
  } finally {
    togglingId.value = ''
  }
}

async function askDelete(cl: PVECluster): Promise<void> {
  const ok = await confirm.open({
    title: t('pveSettings.deleteTitle', { name: cl.name }),
    body: t('pveSettings.deleteBody'),
    confirmLabel: t('pveSettings.deleteConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deletePVECluster(cl.id)
    toast.success(t('pveSettings.deleted'))
    await load()
  } catch (err) {
    toast.error(t('pveSettings.errDelete'), { detail: errMsg(err, t('pveAdmin.errNetwork')) })
  }
}

// ─── Token 配置弹窗(密文存储,留空 secret = 不改动) ────────────────────────────
const tokenOpen = ref(false)
const tokenSubmitting = ref(false)
const tokenTarget = ref<PVECluster | null>(null)
const tokenForm = ref({
  apiUrl: '',
  tokenName: '',
  tokenSecret: '',
  tlsMode: 'insecure' as 'verify' | 'insecure' | 'fingerprint',
  tlsFingerprint: '',
})

function openToken(cl: PVECluster): void {
  tokenTarget.value = cl
  tokenForm.value = {
    apiUrl: cl.apiUrl,
    tokenName: cl.tokenName,
    tokenSecret: '',
    tlsMode: cl.tlsMode,
    tlsFingerprint: cl.tlsFingerprint,
  }
  tokenOpen.value = true
}

async function submitToken(): Promise<void> {
  if (!tokenTarget.value || tokenSubmitting.value) return
  const f = tokenForm.value
  if (!f.apiUrl.trim() || !f.tokenName.trim() || !f.tokenSecret.trim()) {
    toast.error(t('pveSettings.errTokenRequired'))
    return
  }
  if (f.tlsMode === 'fingerprint' && !f.tlsFingerprint.trim()) {
    toast.error(t('pveSettings.errFingerprintRequired'))
    return
  }
  tokenSubmitting.value = true
  try {
    await setPVEToken(tokenTarget.value.id, {
      apiUrl: f.apiUrl.trim(),
      tokenName: f.tokenName.trim(),
      tokenSecret: f.tokenSecret.trim(),
      tlsMode: f.tlsMode,
      ...(f.tlsMode === 'fingerprint' ? { tlsFingerprint: f.tlsFingerprint.trim() } : {}),
    })
    toast.success(t('pveSettings.tokenSaved'))
    tokenOpen.value = false
    await load()
  } catch (err) {
    // 测连失败时 Token 已保存(state 保持 created)——给出可读提示而非静默。
    toast.error(t('pveSettings.errTokenSave'), { detail: errMsg(err, t('pveAdmin.errNetwork')) })
    tokenOpen.value = false
    await load()
  } finally {
    tokenSubmitting.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="settings-pve">
    <header class="page-head">
      <div>
        <h2 class="page-title">{{ t('pveSettings.title') }}</h2>
        <p class="page-desc">{{ t('pveSettings.desc') }}</p>
      </div>
      <router-link to="/pve" class="link-manage">{{ t('pveSettings.goManage') }} →</router-link>
    </header>

    <!-- 集群列表 -->
    <div v-if="loadState === 'loading'" class="load-state">{{ t('pveAdmin.loading') }}</div>
    <div v-else-if="loadState === 'error'" class="load-state">
      <p>{{ loadError }}</p>
      <button class="btn-secondary" @click="load">{{ t('pveAdmin.retry') }}</button>
    </div>
    <div v-else-if="clusters.length === 0" class="empty-state">
      <div class="empty-icon" aria-hidden="true"><NIcon :size="22"><Server /></NIcon></div>
      <p class="empty-label">{{ t('pveSettings.emptyLabel') }}</p>
      <p class="empty-hint">{{ t('pveSettings.emptyHint') }}</p>
    </div>
    <template v-else>
      <div class="list-head">
        <span class="list-title">{{ t('pveSettings.panelTitle') }}</span>
        <span class="list-count">{{ t('pveSettings.countLabel', { n: clusters.length }) }}</span>
      </div>
      <div class="cl-row cl-row--head" aria-hidden="true">
        <span>{{ t('pveSettings.colName') }}</span>
        <span>{{ t('pveSettings.colPool') }}</span>
        <span>{{ t('pveSettings.colState') }}</span>
        <span>{{ t('pveSettings.colToken') }}</span>
        <span />
      </div>
      <div v-for="cl in clusters" :key="cl.id" class="cl-row">
        <div class="name-cell">
          <strong class="cl-name">{{ cl.name }}</strong>
          <span v-if="cl.state === 'ready'" class="badge badge--ok">{{ t('pveSettings.stateReady') }}</span>
          <span v-else class="badge">{{ t('pveSettings.stateCreated') }}</span>
        </div>
        <div class="pool-cell">
          <span class="mono pool-name">{{ cl.poolName || '—' }}</span>
          <span class="pool-nodes">{{ t('pveSettings.nodeCount', { n: cl.nodeCount }) }}</span>
        </div>
        <span class="ro-state" :class="cl.readonly ? 'ro-state--on' : 'ro-state--off'">
          <NIcon :size="13"><ShieldLock v-if="cl.readonly" /><ShieldOff v-else /></NIcon>
          {{ cl.readonly ? t('pveSettings.readonlyOn') : t('pveSettings.readonlyOff') }}
        </span>
        <span class="cred-state" :class="cl.hasToken ? 'cred-state--ok' : 'cred-state--missing'">
          <NIcon :size="13"><CircleCheck v-if="cl.hasToken" /><CircleX v-else /></NIcon>
          {{ cl.hasToken ? t('pveSettings.tokenConfigured') : t('pveSettings.tokenNone') }}
        </span>
        <span class="row-ops">
          <button class="op-btn" @click="openToken(cl)">
            <NIcon :size="13"><PlugConnected /></NIcon>
            <span class="op-btn-txt">{{ t('pveSettings.tokenAction') }}</span>
          </button>
          <button
            class="op-btn"
            :title="t('pveAdmin.test')"
            :disabled="testingId === cl.id || !cl.hasToken"
            @click="testRow(cl)"
          >
            <NIcon :size="13"><PlugConnected /></NIcon>
            <span class="op-btn-txt">{{ testingId === cl.id ? t('pveAdmin.testing') : t('pveAdmin.test') }}</span>
          </button>
          <button
            class="op-btn"
            :class="{ 'op-btn--danger': !cl.readonly }"
            :disabled="togglingId === cl.id"
            @click="toggleReadonly(cl)"
          >
            <NIcon :size="13"><ShieldLock v-if="cl.readonly" /><ShieldOff v-else /></NIcon>
            <span class="op-btn-txt">{{ cl.readonly ? t('pveSettings.unlock') : t('pveSettings.lock') }}</span>
          </button>
          <button class="op-btn op-btn--danger" :title="t('pveSettings.deleteConfirm')" @click="askDelete(cl)">
            <NIcon :size="13"><Trash /></NIcon>
          </button>
        </span>
      </div>
    </template>

    <!-- Token 配置弹窗 -->
    <Teleport to="body">
      <div
        v-if="tokenOpen"
        class="modal-scrim"
        role="dialog"
        :aria-label="t('pveSettings.tokenTitle')"
        aria-modal="true"
        @click.self="tokenOpen = false"
        @keydown.esc="tokenOpen = false"
      >
        <form class="modal" @submit.prevent="submitToken">
          <div class="modal-head">
            <span class="modal-icon"><NIcon :size="17"><Server /></NIcon></span>
            <div>
              <h2 class="modal-title">{{ t('pveSettings.tokenTitle') }} · {{ tokenTarget?.name }}</h2>
              <p class="modal-sub">{{ t('pveSettings.tokenSub') }}</p>
            </div>
          </div>
          <div class="modal-body">
            <label class="field">
              <span class="field-label">{{ t('pveSettings.fieldApiUrl') }}</span>
              <input v-model="tokenForm.apiUrl" class="field-input mono" type="text" placeholder="https://10.0.0.11:8006" />
            </label>
            <div class="field-row">
              <label class="field">
                <span class="field-label">{{ t('pveSettings.fieldTokenName') }}</span>
                <input v-model="tokenForm.tokenName" class="field-input mono" type="text" placeholder="root@pam!pipewright" />
              </label>
              <label class="field">
                <span class="field-label">{{ t('pveSettings.fieldTokenSecret') }}</span>
                <input v-model="tokenForm.tokenSecret" class="field-input mono" type="password" :placeholder="tokenTarget?.hasToken ? t('pveSettings.secretKeep') : t('pveSettings.secretPlaceholder')" autocomplete="new-password" />
              </label>
            </div>
            <label class="field">
              <span class="field-label">{{ t('pveSettings.fieldTlsMode') }}</span>
              <select v-model="tokenForm.tlsMode" class="field-input">
                <option value="insecure">{{ t('pveSettings.tlsInsecure') }}</option>
                <option value="fingerprint">{{ t('pveSettings.tlsFingerprint') }}</option>
                <option value="verify">{{ t('pveSettings.tlsVerify') }}</option>
              </select>
            </label>
            <label v-if="tokenForm.tlsMode === 'fingerprint'" class="field">
              <span class="field-label">{{ t('pveSettings.fieldFingerprint') }}</span>
              <input v-model="tokenForm.tlsFingerprint" class="field-input mono" type="text" placeholder="AA:BB:…(SHA256)" />
            </label>
            <p class="hint">{{ t('pveSettings.tokenHint') }}</p>
          </div>
          <div class="modal-foot">
            <button type="button" class="btn-secondary" :disabled="tokenSubmitting" @click="tokenOpen = false">
              {{ t('objectStorage.cancel') }}
            </button>
            <button type="submit" class="btn-primary" :disabled="tokenSubmitting" :aria-busy="tokenSubmitting">
              {{ tokenSubmitting ? t('pveSettings.tokenSubmitting') : t('pveSettings.tokenSave') }}
            </button>
          </div>
        </form>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.settings-pve {
  display: flex;
  flex-direction: column;
  gap: 20px;
  max-width: 960px;
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
.link-manage {
  margin-left: auto;
  font-size: 0.8rem;
  color: var(--color-primary);
  text-decoration: none;
  white-space: nowrap;
}
.link-manage:hover { text-decoration: underline; }

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
.empty-hint { font-size: 0.78rem; color: var(--color-faint); max-width: 56ch; }

.list-head { display: flex; align-items: baseline; gap: 10px; }
.list-title { font-size: 0.9rem; font-weight: 600; color: var(--color-text); }
.list-count { font-size: 0.74rem; color: var(--color-faint); }

.cl-row {
  display: grid;
  grid-template-columns: 1.3fr 1.1fr 0.9fr 0.9fr auto;
  gap: 10px;
  align-items: center;
  padding: 12px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-card);
  font-size: 0.82rem;
}
.cl-row + .cl-row { margin-top: 8px; }
.cl-row--head {
  background: none;
  border: none;
  padding-bottom: 0;
  font-size: 0.7rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.cl-row--head + .cl-row { margin-top: 0; }
.name-cell { display: flex; align-items: center; gap: 8px; min-width: 0; }
.cl-name { color: var(--color-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.badge {
  flex-shrink: 0;
  font-size: 0.66rem;
  padding: 2px 7px;
  border-radius: var(--rounded-full);
  background: var(--color-inset);
  color: var(--color-dim);
  font-weight: 600;
}
.badge--ok { background: var(--color-primary-soft); color: var(--color-primary); }
.pool-cell { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
.pool-name { color: var(--color-dim); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.pool-nodes { font-size: 0.72rem; color: var(--color-faint); }
.ro-state { display: inline-flex; align-items: center; gap: 5px; font-size: 0.74rem; }
.ro-state--on { color: var(--color-green); }
.ro-state--off { color: var(--color-red); }
.cred-state { display: inline-flex; align-items: center; gap: 5px; font-size: 0.74rem; }
.cred-state--ok { color: var(--color-green); }
.cred-state--missing { color: var(--color-faint); }
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
  max-width: 540px;
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
.field-input:focus {
  outline: none;
  border-color: var(--color-primary);
  box-shadow: 0 0 0 3px var(--color-primary-soft);
}
.hint { font-size: 0.74rem; color: var(--color-faint); line-height: 1.5; }
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
}
.btn-secondary:disabled { opacity: 0.55; cursor: not-allowed; }
.mono { font-family: var(--font-mono); }

@media (max-width: 960px) {
  .cl-row { grid-template-columns: 1fr 1fr; row-gap: 6px; }
}
</style>
