<script setup lang="ts">
/**
 * TotpPanel — 验证器 App(TOTP)两步验证面板(管理员与普通用户共用)。
 *
 * 三态:未配置(发起开启向导)/ 已开启(关闭、备份码重生成)/ 向导进行中。
 * 开启向导:扫码(QRCodeSVG 渲染 otpauth URL,自托管无外部请求)→
 * 备份码一次性展示(可下载)→ 6 位码验证启用。
 * 关闭与备份码重生成都要求输入当前动态码/备份码(后端锁定节流)。
 */
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import QRCode from 'qrcode.vue'
import FormField from '../../ui/FormField.vue'
import AppButton from '../../ui/AppButton.vue'
import { HttpError } from '../../../api/http'
import { useToast } from '../../../composables/useToast'
import {
  disableTOTP,
  enableTOTP,
  fetchMFAStatus,
  regenerateBackupCodes,
  setupTOTP,
  type TOTPSetup,
} from '../../../api/mfa'

const { t } = useI18n()
const toast = useToast()

const loading = ref(true)
const enabled = ref(false)
const setupPending = ref(false)
const backupRemaining = ref(0)

// 向导状态
const wizard = ref<'closed' | 'scan' | 'backup' | 'verify'>('closed')
const wizardData = ref<TOTPSetup | null>(null)
const verifyCode = ref('')
const verifyError = ref('')
const submitting = ref(false)

// 关闭 / 重生成对话框
const dialog = ref<'none' | 'disable' | 'regen'>('none')
const dialogCode = ref('')
const dialogError = ref('')
const dialogSubmitting = ref(false)
const regenCodes = ref<string[] | null>(null)

async function reload(): Promise<void> {
  loading.value = true
  try {
    const st = await fetchMFAStatus()
    enabled.value = st.totpEnabled
    setupPending.value = st.totpSetupPending
    backupRemaining.value = st.backupCodesRemaining
    if (!st.totpEnabled && !st.totpSetupPending) {
      wizard.value = 'closed'
      wizardData.value = null
    }
  } catch {
    // 载入失败保持现状;操作时的错误各自有提示。
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void reload()
})

function errText(err: unknown, fallback: string): string {
  if (err instanceof HttpError) {
    if (err.status === 429) return t('mfa.errLocked')
    return err.apiError?.message ?? fallback
  }
  return fallback
}

async function startWizard(): Promise<void> {
  submitting.value = true
  try {
    wizardData.value = await setupTOTP()
    wizard.value = 'scan'
    verifyCode.value = ''
    verifyError.value = ''
  } catch (err) {
    toast.error(errText(err, t('mfa.errCode')))
  } finally {
    submitting.value = false
  }
}

/** 继续未完成的 setup(库内已有 enabled=0 的待验证配置)。 */
async function resumeWizard(): Promise<void> {
  await startWizard()
}

async function confirmEnable(): Promise<void> {
  const code = verifyCode.value.trim()
  if (!code || submitting.value) return
  submitting.value = true
  verifyError.value = ''
  try {
    await enableTOTP(code)
    wizard.value = 'closed'
    wizardData.value = null
    toast.success(t('mfa.enabled'))
    await reload()
  } catch (err) {
    verifyError.value = errText(err, t('mfa.errCode'))
  } finally {
    submitting.value = false
  }
}

async function confirmDisable(): Promise<void> {
  const code = dialogCode.value.trim()
  if (!code || dialogSubmitting.value) return
  dialogSubmitting.value = true
  dialogError.value = ''
  try {
    await disableTOTP(code)
    dialog.value = 'none'
    dialogCode.value = ''
    toast.success(t('mfa.disabled'))
    await reload()
  } catch (err) {
    dialogError.value = errText(err, t('mfa.errCode'))
  } finally {
    dialogSubmitting.value = false
  }
}

async function confirmRegen(): Promise<void> {
  const code = dialogCode.value.trim()
  if (!code || dialogSubmitting.value) return
  dialogSubmitting.value = true
  dialogError.value = ''
  try {
    const res = await regenerateBackupCodes(code)
    regenCodes.value = res.backupCodes
    dialogCode.value = ''
    await reload()
  } catch (err) {
    dialogError.value = errText(err, t('mfa.errCode'))
  } finally {
    dialogSubmitting.value = false
  }
}

function downloadCodes(codes: string[]): void {
  const text =
    'Pipewright backup codes\n' + codes.map((c) => c).join('\n') + '\n'
  const blob = new Blob([text], { type: 'text/plain' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = 'pipewright-backup-codes.txt'
  a.click()
  URL.revokeObjectURL(url)
}
</script>

<template>
  <section class="panel-card" aria-labelledby="mfa-totp-h">
    <header class="panel-card__h">
      <h2 id="mfa-totp-h" class="panel-card__title">{{ t('mfa.totpCardTitle') }}</h2>
      <p class="panel-card__sub">{{ t('mfa.totpCardDesc') }}</p>
    </header>

    <!-- 状态行 + 操作 -->
    <div v-if="wizard === 'closed'" class="totp-state">
      <span class="totp-badge" :class="{ 'totp-badge--on': enabled }">
        {{ enabled ? t('mfa.totpOn') : t('mfa.totpOff') }}
      </span>
      <span v-if="enabled" class="totp-remaining">{{ t('mfa.backupRemaining', { n: backupRemaining }) }}</span>
      <span class="totp-actions">
        <template v-if="!enabled">
          <AppButton v-if="setupPending" variant="default" size="sm" :disabled="loading" @click="resumeWizard">
            {{ t('mfa.wizardTitle') }}
          </AppButton>
          <AppButton v-else variant="primary" size="sm" :disabled="loading || submitting" @click="startWizard">
            {{ t('mfa.wizardTitle') }}
          </AppButton>
        </template>
        <template v-else>
          <AppButton variant="default" size="sm" @click="dialog = 'regen'; regenCodes = null; dialogCode = ''; dialogError = ''">
            {{ t('mfa.regenerateBackup') }}
          </AppButton>
          <AppButton variant="danger" size="sm" @click="dialog = 'disable'; dialogCode = ''; dialogError = ''">
            {{ t('mfa.disable') }}
          </AppButton>
        </template>
      </span>
    </div>

    <!-- 开启向导 -->
    <div v-else class="totp-wizard">
      <!-- 第 1 步:扫码 -->
      <template v-if="wizard === 'scan'">
        <h3 class="totp-wizard__title">{{ t('mfa.stepScan') }}</h3>
        <p class="totp-wizard__desc">{{ t('mfa.stepScanDesc') }}</p>
        <div class="totp-qr">
          <QRCode v-if="wizardData" :value="wizardData.otpauthUrl" :size="180" level="M" />
        </div>
        <div v-if="wizardData" class="totp-secret mono">{{ wizardData.secret }}</div>
        <div class="totp-wizard__nav">
          <AppButton variant="default" size="sm" @click="wizard = 'closed'">
            {{ t('mfa.cancel') }}
          </AppButton>
          <AppButton variant="primary" size="sm" @click="wizard = 'backup'">
            {{ t('mfa.next') }}
          </AppButton>
        </div>
      </template>

      <!-- 第 2 步:备份码 -->
      <template v-else-if="wizard === 'backup'">
        <h3 class="totp-wizard__title">{{ t('mfa.stepBackup') }}</h3>
        <p class="totp-wizard__desc">{{ t('mfa.stepBackupDesc') }}</p>
        <p class="totp-warn">{{ t('mfa.stepBackupWarning') }}</p>
        <div v-if="wizardData" class="totp-codes">
          <code v-for="c in wizardData.backupCodes" :key="c" class="mono">{{ c }}</code>
        </div>
        <div class="totp-wizard__nav">
          <AppButton variant="default" size="sm" @click="wizard = 'scan'">{{ t('mfa.back') }}</AppButton>
          <span class="totp-wizard__spacer" />
          <AppButton variant="default" size="sm" @click="wizardData && downloadCodes(wizardData.backupCodes)">
            {{ t('mfa.backupDownload') }}
          </AppButton>
          <AppButton variant="primary" size="sm" @click="wizard = 'verify'">{{ t('mfa.next') }}</AppButton>
        </div>
      </template>

      <!-- 第 3 步:验证启用 -->
      <template v-else>
        <h3 class="totp-wizard__title">{{ t('mfa.stepVerify') }}</h3>
        <p class="totp-wizard__desc">{{ t('mfa.stepVerifyDesc') }}</p>
        <form class="totp-verify" novalidate @submit.prevent="confirmEnable">
          <FormField :label="t('login.mfaCodeLabel')" field-id="mfa-verify-code" :error="verifyError">
            <template #default="{ fieldId }">
              <input
                :id="fieldId"
                v-model="verifyCode"
                class="field-input"
                :class="{ 'field-input--error': verifyError }"
                type="tel"
                inputmode="numeric"
                :placeholder="t('login.mfaCodePlaceholder')"
                maxlength="6"
                autocomplete="one-time-code"
              />
            </template>
          </FormField>
          <div class="totp-wizard__nav">
            <AppButton variant="default" size="sm" type="button" @click="wizard = 'backup'">
              {{ t('mfa.back') }}
            </AppButton>
            <AppButton variant="primary" size="sm" type="submit" :disabled="submitting || !verifyCode.trim()">
              {{ t('mfa.confirmEnable') }}
            </AppButton>
          </div>
        </form>
      </template>
    </div>

    <!-- 关闭 / 重生成 对话 -->
    <div v-if="dialog !== 'none'" class="totp-dialog" role="dialog" aria-modal="false">
      <template v-if="dialog === 'disable'">
        <h3 class="totp-wizard__title">{{ t('mfa.disableTitle') }}</h3>
        <p class="totp-wizard__desc">{{ t('mfa.disableConfirm') }}</p>
      </template>
      <template v-else>
        <h3 class="totp-wizard__title">
          {{ regenCodes ? t('mfa.backupRegenerated') : t('mfa.regenerateTitle') }}
        </h3>
        <p v-if="!regenCodes" class="totp-wizard__desc">{{ t('mfa.regenerateDesc') }}</p>
      </template>

      <div v-if="regenCodes" class="totp-codes">
        <code v-for="c in regenCodes" :key="c" class="mono">{{ c }}</code>
      </div>

      <form
        v-else
        class="totp-verify"
        novalidate
        @submit.prevent="dialog === 'disable' ? confirmDisable() : confirmRegen()"
      >
        <FormField :label="t('mfa.codeLabel')" field-id="mfa-dialog-code" :error="dialogError">
          <template #default="{ fieldId }">
            <input
              :id="fieldId"
              v-model="dialogCode"
              class="field-input"
              :class="{ 'field-input--error': dialogError }"
              type="text"
              :placeholder="t('login.mfaBackupPlaceholder')"
              autocomplete="off"
            />
          </template>
        </FormField>
        <div class="totp-wizard__nav">
          <AppButton variant="default" size="sm" type="button" @click="dialog = 'none'; regenCodes = null">
            {{ regenCodes ? t('common.refresh') : t('mfa.cancel') }}
          </AppButton>
          <AppButton
            variant="primary"
            size="sm"
            type="submit"
            :disabled="dialogSubmitting || !dialogCode.trim()"
          >
            {{ t('mfa.confirm') }}
          </AppButton>
        </div>
      </form>
      <div v-if="regenCodes" class="totp-wizard__nav">
        <AppButton variant="default" size="sm" @click="dialog = 'none'; regenCodes = null">
          {{ t('mfa.confirm') }}
        </AppButton>
        <AppButton variant="default" size="sm" @click="downloadCodes(regenCodes)">
          {{ t('mfa.backupDownload') }}
        </AppButton>
      </div>
    </div>
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
.totp-state {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.totp-badge {
  font-size: 12px;
  font-weight: 600;
  padding: 3px 10px;
  border-radius: 999px;
  background: rgba(107, 114, 128, 0.12);
  color: #6b7280;
}
.totp-badge--on {
  background: rgba(16, 185, 129, 0.14);
  color: #059669;
}
.totp-remaining {
  font-size: 12.5px;
  color: var(--color-text-dim);
}
.totp-actions {
  margin-left: auto;
  display: flex;
  gap: 8px;
}
.totp-wizard__title {
  font-size: 14px;
  font-weight: 700;
  margin: 4px 0 4px;
}
.totp-wizard__desc {
  font-size: 12.5px;
  color: var(--color-text-dim);
  margin: 0 0 12px;
}
.totp-warn {
  font-size: 12px;
  color: #b45309;
  background: rgba(245, 158, 11, 0.1);
  border-radius: 8px;
  padding: 8px 10px;
  margin: 0 0 12px;
}
.totp-qr {
  display: flex;
  justify-content: center;
  padding: 12px;
  background: #fff;
  border-radius: 10px;
  width: max-content;
  margin: 0 auto 10px;
}
.totp-secret {
  text-align: center;
  font-size: 12px;
  word-break: break-all;
  color: var(--color-text-dim);
  margin-bottom: 12px;
}
.totp-codes {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
  gap: 6px;
  margin-bottom: 12px;
}
.totp-codes code {
  text-align: center;
  font-size: 13px;
  padding: 6px 4px;
  border-radius: 8px;
  background: rgba(59, 130, 246, 0.08);
}
.totp-verify {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.totp-wizard__nav {
  display: flex;
  gap: 8px;
  align-items: center;
}
.totp-wizard__spacer {
  flex: 1;
}
.totp-dialog {
  margin-top: 14px;
  border-top: 1px dashed var(--color-border, #e5e7eb);
  padding-top: 14px;
}
</style>
