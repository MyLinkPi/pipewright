<script setup lang="ts">
/**
 * ChangePasswordPanel — 修改口令表单(管理员与普通用户共用)。
 *
 * 自包含:校验(必填/长度/确认一致)→ POST /api/account/password →
 * 错误内联映射(invalid_current_password / weak_password)→ 成功 toast。
 * 成功后服务端会吊销「除当前会话外」的全部会话,故 emit('updated')
 * 通知父组件刷新会话列表。
 *
 * 文案复用 `settingsAccount.*` 词条(8 语言已齐备)。
 */
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import FormField from '../ui/FormField.vue'
import AppButton from '../ui/AppButton.vue'
import { useToast } from '../../composables/useToast'
import { HttpError } from '../../api/http'
import { changePassword } from '../../api/account'

const { t } = useI18n()
const toast = useToast()

const emit = defineEmits<{ updated: [] }>()

const currentPassword = ref('')
const newPassword = ref('')
const confirmPassword = ref('')
const currentError = ref('')
const newError = ref('')
const confirmError = ref('')
const submitting = ref(false)

function clearPwErrors(): void {
  currentError.value = ''
  newError.value = ''
  confirmError.value = ''
}

function validatePassword(): boolean {
  clearPwErrors()
  let ok = true
  if (!currentPassword.value) {
    currentError.value = t('settingsAccount.errCurrentRequired')
    ok = false
  }
  if (newPassword.value.length < 8) {
    newError.value = t('settingsAccount.errNewTooShort')
    ok = false
  }
  if (confirmPassword.value !== newPassword.value) {
    confirmError.value = t('settingsAccount.errConfirmMismatch')
    ok = false
  }
  return ok
}

async function onSubmit(): Promise<void> {
  if (!validatePassword()) return
  submitting.value = true
  try {
    await changePassword({
      currentPassword: currentPassword.value,
      newPassword: newPassword.value,
    })
    currentPassword.value = ''
    newPassword.value = ''
    confirmPassword.value = ''
    clearPwErrors()
    toast.success(t('settingsAccount.toastPwUpdated'), { detail: t('settingsAccount.toastPwUpdatedDetail') })
    // 服务端已吊销其它会话 → 通知父组件刷新可见会话列表。
    emit('updated')
  } catch (err) {
    if (err instanceof HttpError) {
      if (err.apiError?.code === 'invalid_current_password') {
        currentError.value = t('settingsAccount.errCurrentWrong')
        return
      }
      if (err.apiError?.code === 'weak_password') {
        newError.value = t('settingsAccount.errNewTooShort')
        return
      }
    }
    toast.error(t('settingsAccount.toastPwFailed'), {
      detail: err instanceof Error ? err.message : t('settingsAccount.unknownError'),
    })
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section class="acct-card" aria-labelledby="acct-pw-h">
    <header class="acct-card__h">
      <h2 id="acct-pw-h" class="acct-card__title">{{ t('settingsAccount.changePwTitle') }}</h2>
      <p class="acct-card__sub">{{ t('settingsAccount.changePwSub') }}</p>
    </header>
    <form class="acct-form" @submit.prevent="onSubmit">
      <FormField :label="t('settingsAccount.currentPwLabel')" field-id="acct-current" :error="currentError" required>
        <template #default="{ fieldId, ariaDescribedby }">
          <input
            :id="fieldId"
            v-model="currentPassword"
            class="field-input"
            :class="{ 'field-input--error': currentError }"
            type="password"
            autocomplete="current-password"
            :aria-invalid="!!currentError || undefined"
            :aria-describedby="ariaDescribedby"
          />
        </template>
      </FormField>

      <FormField
        :label="t('settingsAccount.newPwLabel')"
        field-id="acct-new"
        :error="newError"
        :hint="t('settingsAccount.newPwHint')"
        required
      >
        <template #default="{ fieldId, ariaDescribedby }">
          <input
            :id="fieldId"
            v-model="newPassword"
            class="field-input"
            :class="{ 'field-input--error': newError }"
            type="password"
            autocomplete="new-password"
            :aria-invalid="!!newError || undefined"
            :aria-describedby="ariaDescribedby"
          />
        </template>
      </FormField>

      <FormField :label="t('settingsAccount.confirmPwLabel')" field-id="acct-confirm" :error="confirmError" required>
        <template #default="{ fieldId, ariaDescribedby }">
          <input
            :id="fieldId"
            v-model="confirmPassword"
            class="field-input"
            :class="{ 'field-input--error': confirmError }"
            type="password"
            autocomplete="new-password"
            :aria-invalid="!!confirmError || undefined"
            :aria-describedby="ariaDescribedby"
          />
        </template>
      </FormField>

      <div class="acct-form__actions">
        <AppButton variant="primary" type="submit" :loading="submitting">
          {{ t('settingsAccount.changePwBtn') }}
        </AppButton>
      </div>
    </form>
  </section>
</template>

<style scoped>
.acct-card {
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  box-shadow: var(--shadow);
  padding: 20px 22px;
}
.acct-card__h {
  margin-bottom: 16px;
}
.acct-card__title {
  font-size: 1rem;
  font-weight: 600;
  color: var(--color-text);
}
.acct-card__sub {
  font-size: 0.79rem;
  color: var(--color-faint);
  margin-top: 4px;
  line-height: 1.5;
}
.acct-form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.acct-form__actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
.field-input {
  width: 100%;
  height: 40px;
  background: var(--color-inset);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  padding: 0 13px;
  color: var(--color-text);
  font-family: var(--font-sans);
  font-size: 0.9rem;
  transition:
    border-color var(--duration-fast, 150ms),
    box-shadow var(--duration-fast, 150ms);
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
</style>
