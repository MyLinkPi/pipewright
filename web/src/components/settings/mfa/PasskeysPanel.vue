<script setup lang="ts">
/**
 * PasskeysPanel — 通行密钥管理面板(管理员与普通用户共用)。
 *
 * 列表(名称/创建时间/最近使用)+ 添加(命名 → navigator.credentials.create →
 * finish)→ 删除(确认)。浏览器不支持 / 非 HTTPS 环境给出对应提示。
 */
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import FormField from '../../ui/FormField.vue'
import AppButton from '../../ui/AppButton.vue'
import { HttpError } from '../../../api/http'
import { useToast } from '../../../composables/useToast'
import { useConfirm } from '../../../composables/useConfirm'
import {
  beginPasskeyRegistration,
  deletePasskey,
  fetchMFAStatus,
  finishPasskeyRegistration,
  type PasskeyInfo,
} from '../../../api/mfa'
import {
  buildRegistrationResult,
  createCredential,
  isPasskeySupported,
  prepareCredentialCreationOptions,
} from '../../../utils/webauthn'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

const loading = ref(true)
const passkeys = ref<PasskeyInfo[]>([])
const supported = ref(true)

const adding = ref(false)
const newName = ref('')
const addSubmitting = ref(false)
const addError = ref('')

async function reload(): Promise<void> {
  loading.value = true
  try {
    const st = await fetchMFAStatus()
    passkeys.value = st.passkeys
  } catch {
    // 状态加载失败保持现状。
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  supported.value = await isPasskeySupported()
  await reload()
})

function errText(err: unknown, fallback: string): string {
  if (err instanceof HttpError) {
    if (err.apiError?.code === 'insecure_context') return t('mfa.errInsecure')
    return err.apiError?.message ?? fallback
  }
  return fallback
}

async function register(): Promise<void> {
  if (addSubmitting.value) return
  addSubmitting.value = true
  addError.value = ''
  try {
    const begin = await beginPasskeyRegistration(newName.value.trim())
    const credential = await createCredential(prepareCredentialCreationOptions(begin.options))
    const payload = buildRegistrationResult(credential as PublicKeyCredential | null)
    if (!payload) {
      addError.value = t('mfa.errPasskeyFailed')
      return
    }
    await finishPasskeyRegistration(payload)
    toast.success(t('mfa.passkeyAdded'))
    adding.value = false
    newName.value = ''
    await reload()
  } catch (err) {
    addError.value = errText(err, t('mfa.errPasskeyFailed'))
  } finally {
    addSubmitting.value = false
  }
}

async function remove(pk: PasskeyInfo): Promise<void> {
  const ok = await confirm.open({
    title: t('mfa.remove'),
    body: t('mfa.removeConfirm'),
    confirmLabel: t('mfa.remove'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deletePasskey(pk.id)
    toast.success(t('mfa.passkeyRemoved'))
    await reload()
  } catch (err) {
    toast.error(errText(err, t('mfa.errPasskeyFailed')))
  }
}
</script>

<template>
  <section class="panel-card" aria-labelledby="mfa-passkey-h">
    <header class="panel-card__h">
      <h2 id="mfa-passkey-h" class="panel-card__title">{{ t('mfa.passkeyCardTitle') }}</h2>
      <p class="panel-card__sub">{{ t('mfa.passkeyCardDesc') }}</p>
    </header>

    <p v-if="!supported" class="pk-warn">{{ t('mfa.errPasskeyUnsupported') }}</p>

    <!-- 添加表单 -->
    <div v-if="adding" class="pk-add">
      <FormField :label="t('mfa.passkeyNameLabel')" field-id="mfa-passkey-name" :error="addError">
        <template #default="{ fieldId }">
          <input
            :id="fieldId"
            v-model="newName"
            class="field-input"
            :class="{ 'field-input--error': addError }"
            type="text"
            :placeholder="t('mfa.passkeyNamePlaceholder')"
            maxlength="64"
          />
        </template>
      </FormField>
      <div class="pk-add__nav">
        <AppButton variant="default" size="sm" @click="adding = false; addError = ''">
          {{ t('mfa.cancel') }}
        </AppButton>
        <AppButton
          variant="primary"
          size="sm"
          :disabled="addSubmitting || !supported"
          @click="register"
        >
          {{ addSubmitting ? t('login.mfaVerifying') : t('mfa.confirm') }}
        </AppButton>
      </div>
    </div>

    <div v-else class="pk-toolbar">
      <AppButton variant="primary" size="sm" :disabled="!supported" @click="adding = true">
        {{ t('mfa.passkeyAdd') }}
      </AppButton>
    </div>

    <!-- 列表 -->
    <ul v-if="passkeys.length" class="pk-list">
      <li v-for="pk in passkeys" :key="pk.id" class="pk-item">
        <div class="pk-item__main">
          <span class="pk-item__name">{{ pk.name || `#${pk.id}` }}</span>
          <span class="pk-item__meta">
            {{ pk.createdAt }} · {{ t('mfa.passkeyLastUsed') }}: {{ pk.lastUsedAt || t('mfa.passkeyNever') }}
          </span>
        </div>
        <AppButton variant="danger" size="sm" @click="remove(pk)">{{ t('mfa.remove') }}</AppButton>
      </li>
    </ul>
    <p v-else-if="!loading" class="pk-empty">{{ t('mfa.passkeyEmpty') }}</p>
  </section>
</template>

<style scoped>
.field-input {
  width: 100%;
  padding: 9px 12px;
  border-radius: 9px;
  border: 1px solid var(--color-border, #e5e7eb);
  background: var(--color-surface, #fff);
  font-size: 13.5px;
  font-family: inherit;
  outline: none;
}
.field-input:focus {
  border-color: var(--color-primary, #3b82f6);
}
.field-input--error {
  border-color: #ef4444;
}
.panel-card {
  background: var(--color-surface, #fff);
  border: 1px solid var(--color-border, #e5e7eb);
  border-radius: 14px;
  padding: 20px;
}
.panel-card__h {
  margin-bottom: 14px;
}
.panel-card__title {
  font-size: 15px;
  font-weight: 700;
  margin: 0 0 4px;
}
.panel-card__sub {
  font-size: 12.5px;
  color: var(--color-text-dim);
  margin: 0;
}
.pk-warn {
  font-size: 12px;
  color: #b45309;
  background: rgba(245, 158, 11, 0.1);
  border-radius: 8px;
  padding: 8px 10px;
}
.pk-toolbar {
  display: flex;
}
.pk-add {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.pk-add__nav {
  display: flex;
  gap: 8px;
}
.pk-list {
  list-style: none;
  margin: 14px 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.pk-item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 12px;
  border: 1px solid var(--color-border, #e5e7eb);
  border-radius: 10px;
}
.pk-item__main {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.pk-item__name {
  font-size: 13.5px;
  font-weight: 600;
}
.pk-item__meta {
  font-size: 12px;
  color: var(--color-text-dim);
}
.pk-item > :last-child {
  margin-left: auto;
}
.pk-empty {
  font-size: 12.5px;
  color: var(--color-text-dim);
  margin: 12px 0 0;
}
</style>
