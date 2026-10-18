<script setup lang="ts">
/*
  SettingsRedis.vue — Redis 实例注册（设置 → Redis）。

  为「Redis」管理页提供实例登记:host:port + 可选密码(内网无密码实例合法)+ 默认逻辑库。
  密码经保险库密文存储,绝不回显——编辑时留空不改动;应用市场部署 redis 模板会自动
  登记实例(source=appstore),重新部署同地址原地轮换密码。
*/
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useDirtyGuard } from '../../composables/useDirtyGuard'
import { NIcon } from 'naive-ui'
import { Bolt, Pencil, Trash, PlugConnected, CircleCheck, CircleX } from '@vicons/tabler'
import {
  listRedisInstances, createRedisInstance, updateRedisInstance, deleteRedisInstance,
  testRedisInstance, testRedisConnectionDirect,
  type RedisInstance,
} from '../../api/redisAdmin'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'
import { useConfirm } from '../../composables/useConfirm'

const { t } = useI18n()
const { confirmDiscard } = useDirtyGuard()
const toast = useToast()
const confirm = useConfirm()

const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 列表加载 ──────────────────────────────────────────────────────────────────
type LoadState = 'idle' | 'loading' | 'error'
const loadState = ref<LoadState>('idle')
const loadError = ref('')
const instances = ref<RedisInstance[]>([])

async function load(): Promise<void> {
  loadState.value = 'loading'
  try {
    instances.value = await listRedisInstances()
    loadState.value = 'idle'
  } catch (err) {
    loadState.value = 'error'
    loadError.value = errMsg(err, t('redisAdmin.errNetwork'))
  }
}

// ─── 新增表单 ──────────────────────────────────────────────────────────────────
const form = ref({
  name: '',
  host: '',
  port: 6379,
  dbNum: 0,
  password: '',
})
const formDirty = computed(() =>
  form.value.name.trim() !== '' || form.value.host.trim() !== '' || form.value.password !== '')
const submitting = ref(false)
const testingForm = ref(false)
const formTested = ref(false)

function markDirty(): void { formTested.value = false }

async function submit(): Promise<void> {
  if (submitting.value) return
  if (!form.value.name.trim() || !form.value.host.trim()) {
    toast.error(t('redisSettings.errFormRequired'))
    return
  }
  submitting.value = true
  try {
    await createRedisInstance({
      name: form.value.name.trim(),
      host: form.value.host.trim(),
      port: Number(form.value.port) || 6379,
      dbNum: Number(form.value.dbNum) || 0,
      password: form.value.password,
    })
    toast.success(t('redisSettings.created'))
    form.value = { name: '', host: '', port: 6379, dbNum: 0, password: '' }
    formTested.value = false
    await load()
  } catch (err) {
    toast.error(t('redisSettings.errCreate'), { detail: errMsg(err, t('redisAdmin.errNetwork')) })
  } finally {
    submitting.value = false
  }
}

async function testForm(): Promise<void> {
  if (testingForm.value) return
  if (!form.value.host.trim()) {
    toast.error(t('redisSettings.errFormRequired'))
    return
  }
  testingForm.value = true
  try {
    const res = await testRedisConnectionDirect({
      name: form.value.name.trim(),
      host: form.value.host.trim(),
      port: Number(form.value.port) || 6379,
      dbNum: Number(form.value.dbNum) || 0,
      password: form.value.password,
    })
    if (res.ok) {
      formTested.value = true
      toast.success(t('redisAdmin.testOk', { latency: res.latencyMs, version: res.version || '?' }))
    } else {
      formTested.value = false
      toast.error(t('redisAdmin.testFail'), { detail: res.error ?? '' })
    }
  } catch (err) {
    toast.error(t('redisAdmin.testFail'), { detail: errMsg(err, t('redisAdmin.errNetwork')) })
  } finally {
    testingForm.value = false
  }
}

// ─── 行操作:测试 / 编辑 / 删除 ────────────────────────────────────────────────
const testingId = ref('')

async function testRow(inst: RedisInstance): Promise<void> {
  if (testingId.value) return
  testingId.value = inst.id
  try {
    const res = await testRedisInstance(inst.id)
    if (res.ok) {
      toast.success(t('redisAdmin.testOk', { latency: res.latencyMs, version: res.version || '?' }))
    } else {
      toast.error(t('redisAdmin.testFail'), { detail: res.error ?? '' })
    }
  } finally {
    testingId.value = ''
  }
}

// 编辑弹窗(密码留空 = 不改动)。
const editOpen = ref(false)
const editSubmitting = ref(false)
const editTarget = ref<RedisInstance | null>(null)
const editForm = ref({ name: '', host: '', port: 6379, dbNum: 0, password: '' })

function openEdit(inst: RedisInstance): void {
  editTarget.value = inst
  editForm.value = { name: inst.name, host: inst.host, port: inst.port, dbNum: inst.dbNum, password: '' }
  editOpen.value = true
}

async function submitEdit(): Promise<void> {
  if (!editTarget.value || editSubmitting.value) return
  if (!editForm.value.name.trim() || !editForm.value.host.trim()) {
    toast.error(t('redisSettings.errFormRequired'))
    return
  }
  editSubmitting.value = true
  try {
    await updateRedisInstance(editTarget.value.id, {
      name: editForm.value.name.trim(),
      host: editForm.value.host.trim(),
      port: Number(editForm.value.port) || 6379,
      dbNum: Number(editForm.value.dbNum) || 0,
      ...(editForm.value.password !== '' ? { password: editForm.value.password } : {}),
    })
    toast.success(t('redisSettings.updated'))
    editOpen.value = false
    await load()
  } catch (err) {
    toast.error(t('redisSettings.errUpdate'), { detail: errMsg(err, t('redisAdmin.errNetwork')) })
  } finally {
    editSubmitting.value = false
  }
}

async function askDelete(inst: RedisInstance): Promise<void> {
  const ok = await confirm.open({
    title: t('redisSettings.deleteTitle', { name: inst.name }),
    body: t('redisSettings.deleteBody'),
    confirmLabel: t('redisSettings.deleteConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteRedisInstance(inst.id)
    toast.success(t('redisSettings.deleted'))
    await load()
  } catch (err) {
    toast.error(t('redisSettings.errDelete'), { detail: errMsg(err, t('redisAdmin.errNetwork')) })
  }
}

async function requestLeave(): Promise<boolean> {
  if (!formDirty.value) return true
  return confirmDiscard()
}

onMounted(load)
</script>

<template>
  <div class="settings-redis">
    <header class="page-head">
      <div>
        <h2 class="page-title">{{ t('redisSettings.title') }}</h2>
        <p class="page-desc">{{ t('redisSettings.desc') }}</p>
      </div>
      <router-link to="/redis" class="link-manage">{{ t('redisSettings.goManage') }} →</router-link>
    </header>

    <!-- 新增表单 -->
    <form class="add-form" @submit.prevent="requestLeave().then((ok) => { if (ok) void submit() })">
      <label class="field">
        <span class="field-label">{{ t('redisSettings.fieldName') }}</span>
        <input v-model="form.name" class="field-input" type="text" :placeholder="t('redisSettings.namePlaceholder')" @input="markDirty" />
      </label>
      <label class="field">
        <span class="field-label">{{ t('redisSettings.fieldHost') }}</span>
        <input v-model="form.host" class="field-input mono" type="text" placeholder="10.0.0.8" @input="markDirty" />
      </label>
      <label class="field field--num">
        <span class="field-label">{{ t('redisSettings.fieldPort') }}</span>
        <input v-model="form.port" class="field-input mono" type="number" min="1" max="65535" @input="markDirty" />
      </label>
      <label class="field field--num">
        <span class="field-label">{{ t('redisSettings.fieldDb') }}</span>
        <input v-model="form.dbNum" class="field-input mono" type="number" min="0" max="15" @input="markDirty" />
      </label>
      <label class="field">
        <span class="field-label">{{ t('redisSettings.fieldPassword') }}</span>
        <input v-model="form.password" class="field-input mono" type="password" :placeholder="t('redisSettings.passwordPlaceholder')" autocomplete="new-password" @input="markDirty" />
      </label>
      <div class="form-actions">
        <button type="button" class="btn-secondary" :disabled="testingForm" @click="testForm">
          <NIcon :size="13"><PlugConnected /></NIcon>
          {{ testingForm ? t('redisAdmin.testing') : t('redisSettings.testForm') }}
        </button>
        <button type="submit" class="btn-primary" :disabled="submitting">
          {{ submitting ? t('redisSettings.submitting') : `+ ${t('redisSettings.add')}` }}
        </button>
      </div>
    </form>

    <!-- 实例列表 -->
    <div v-if="loadState === 'loading'" class="load-state">{{ t('redisAdmin.loading') }}</div>
    <div v-else-if="loadState === 'error'" class="load-state">
      <p>{{ loadError }}</p>
      <button class="btn-secondary" @click="load">{{ t('redisAdmin.retry') }}</button>
    </div>
    <div v-else-if="instances.length === 0" class="empty-state">
      <div class="empty-icon" aria-hidden="true"><NIcon :size="22"><Bolt /></NIcon></div>
      <p class="empty-label">{{ t('redisSettings.emptyLabel') }}</p>
      <p class="empty-hint">{{ t('redisSettings.emptyHint') }}</p>
    </div>
    <template v-else>
      <div class="list-head">
        <span class="list-title">{{ t('redisSettings.panelTitle') }}</span>
        <span class="list-count">{{ t('redisSettings.countLabel', { n: instances.length }) }}</span>
      </div>
      <div class="inst-row inst-row--head" aria-hidden="true">
        <span>{{ t('redisSettings.colName') }}</span>
        <span>{{ t('redisSettings.colEndpoint') }}</span>
        <span>{{ t('redisSettings.colDb') }}</span>
        <span>{{ t('redisSettings.colPassword') }}</span>
        <span />
      </div>
      <div v-for="inst in instances" :key="inst.id" class="inst-row">
        <div class="name-cell">
          <strong class="inst-name">{{ inst.name }}</strong>
          <span v-if="inst.source === 'appstore'" class="src-badge">{{ t('redisSettings.srcAppstore') }}</span>
        </div>
        <span class="mono inst-endpoint">{{ inst.endpoint }}</span>
        <span class="mono">db{{ inst.dbNum }}</span>
        <span class="cred-state" :class="inst.passwordConfigured ? 'cred-state--ok' : 'cred-state--missing'">
          <NIcon :size="13"><CircleCheck v-if="inst.passwordConfigured" /><CircleX v-else /></NIcon>
          {{ inst.passwordConfigured ? t('redisSettings.pwConfigured') : t('redisSettings.pwNone') }}
        </span>
        <span class="row-ops">
          <button class="op-btn" :title="t('redisAdmin.test')" :disabled="testingId === inst.id" @click="testRow(inst)">
            <NIcon :size="13"><PlugConnected /></NIcon>
            <span class="op-btn-txt">{{ testingId === inst.id ? t('redisAdmin.testing') : t('redisAdmin.test') }}</span>
          </button>
          <button class="op-btn" :title="t('redisSettings.edit')" @click="openEdit(inst)">
            <NIcon :size="13"><Pencil /></NIcon>
            <span class="op-btn-txt">{{ t('redisSettings.edit') }}</span>
          </button>
          <button class="op-btn op-btn--danger" :title="t('redisSettings.deleteConfirm')" @click="askDelete(inst)">
            <NIcon :size="13"><Trash /></NIcon>
          </button>
        </span>
      </div>
    </template>

    <!-- 编辑弹窗 -->
    <Teleport to="body">
      <div
        v-if="editOpen"
        class="modal-scrim"
        role="dialog"
        :aria-label="t('redisSettings.editTitle')"
        aria-modal="true"
        @click.self="editOpen = false"
        @keydown.esc="editOpen = false"
      >
        <form class="modal" @submit.prevent="submitEdit">
          <div class="modal-head">
            <span class="modal-icon"><NIcon :size="17"><Bolt /></NIcon></span>
            <div>
              <h2 class="modal-title">{{ t('redisSettings.editTitle') }}</h2>
              <p class="modal-sub">{{ t('redisSettings.editSub') }}</p>
            </div>
          </div>
          <div class="modal-body">
            <label class="field">
              <span class="field-label">{{ t('redisSettings.fieldName') }}</span>
              <input v-model="editForm.name" class="field-input" type="text" />
            </label>
            <div class="field-row">
              <label class="field">
                <span class="field-label">{{ t('redisSettings.fieldHost') }}</span>
                <input v-model="editForm.host" class="field-input mono" type="text" />
              </label>
              <label class="field field--num">
                <span class="field-label">{{ t('redisSettings.fieldPort') }}</span>
                <input v-model="editForm.port" class="field-input mono" type="number" min="1" max="65535" />
              </label>
              <label class="field field--num">
                <span class="field-label">{{ t('redisSettings.fieldDb') }}</span>
                <input v-model="editForm.dbNum" class="field-input mono" type="number" min="0" max="15" />
              </label>
            </div>
            <label class="field">
              <span class="field-label">{{ t('redisSettings.fieldPasswordRotate') }}</span>
              <input v-model="editForm.password" class="field-input mono" type="password" :placeholder="t('redisSettings.passwordRotatePlaceholder')" autocomplete="new-password" />
            </label>
          </div>
          <div class="modal-foot">
            <button type="button" class="btn-secondary" :disabled="editSubmitting" @click="editOpen = false">
              {{ t('objectStorage.cancel') }}
            </button>
            <button type="submit" class="btn-primary" :disabled="editSubmitting" :aria-busy="editSubmitting">
              {{ editSubmitting ? t('redisSettings.submitting') : t('redisSettings.save') }}
            </button>
          </div>
        </form>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.settings-redis {
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

.add-form {
  display: grid;
  grid-template-columns: 1.2fr 1.4fr 0.6fr 0.6fr 1.2fr auto;
  gap: 12px;
  align-items: end;
  padding: 16px;
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
}
.field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.field-row { display: grid; grid-template-columns: 1.6fr 0.8fr 0.8fr; gap: 12px; }
.field-label {
  font-size: 0.76rem;
  font-weight: 500;
  color: var(--color-dim);
}
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
.form-actions { display: flex; gap: 8px; }

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
.empty-hint { font-size: 0.78rem; color: var(--color-faint); max-width: 52ch; }

.list-head {
  display: flex;
  align-items: baseline;
  gap: 10px;
}
.list-title { font-size: 0.9rem; font-weight: 600; color: var(--color-text); }
.list-count { font-size: 0.74rem; color: var(--color-faint); }

.inst-row {
  display: grid;
  grid-template-columns: 1.4fr 1.3fr 0.5fr 0.9fr auto;
  gap: 10px;
  align-items: center;
  padding: 12px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-card);
  font-size: 0.82rem;
}
.inst-row + .inst-row { margin-top: 8px; }
.inst-row--head {
  background: none;
  border: none;
  padding-bottom: 0;
  font-size: 0.7rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.inst-row--head + .inst-row { margin-top: 0; }
.name-cell { display: flex; align-items: center; gap: 8px; min-width: 0; }
.inst-name { color: var(--color-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.src-badge {
  flex-shrink: 0;
  font-size: 0.66rem;
  padding: 2px 7px;
  border-radius: var(--rounded-full);
  background: var(--color-primary-soft);
  color: var(--color-primary);
  font-weight: 600;
}
.inst-endpoint { color: var(--color-dim); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.cred-state {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 0.74rem;
}
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
.modal-body {
  padding: 18px 20px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.modal-foot {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  padding: 14px 20px 18px;
  border-top: 1px solid var(--color-border);
}

@media (max-width: 960px) {
  .add-form { grid-template-columns: 1fr 1fr; }
  .inst-row { grid-template-columns: 1fr 1fr; row-gap: 6px; }
}
</style>
