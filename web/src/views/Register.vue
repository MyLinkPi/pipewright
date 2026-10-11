<script setup lang="ts">
/**
 * Register — 邀请注册页(/register?token=...),公开路由。
 *
 * 流程:读 query.token → GET /api/auth/invitations/{token} 展示状态
 *   · pending → 显示注册表单(用户名 / 口令 / 确认)→ POST /api/auth/register
 *     → 后端自动登录(下发会话 cookie)→ 写入本地会话 → 跳 /my-projects
 *   · used / expired / 不存在 → 展示对应提示与「返回登录」
 *
 * 视觉复用登录页的沉浸式深色外壳(同套品牌色变量,不新增视觉体系)。
 */
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { register, fetchInvitation, type InvitationState } from '../api/auth'
import { HttpError } from '../api/http'
import { useSessionStore } from '../stores/session'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const sessionStore = useSessionStore()

type PageState = 'loading' | 'form' | 'used' | 'expired' | 'invalid'

const token = computed(() => {
  const raw = route.query.token
  return typeof raw === 'string' ? raw : ''
})

const pageState = ref<PageState>('loading')

const username = ref('')
const password = ref('')
const confirmPassword = ref('')
const usernameError = ref('')
const passwordError = ref('')
const confirmError = ref('')
const bannerError = ref('')
const submitting = ref(false)

async function loadInvitation(): Promise<void> {
  if (!token.value) {
    pageState.value = 'invalid'
    return
  }
  try {
    const info = await fetchInvitation(token.value)
    pageState.value = fromInvitationState(info.state)
  } catch (err) {
    if (err instanceof HttpError && err.status === 404) {
      pageState.value = 'invalid'
      return
    }
    pageState.value = 'invalid'
  }
}

function fromInvitationState(state: InvitationState): PageState {
  switch (state) {
    case 'pending': return 'form'
    case 'used': return 'used'
    case 'expired': return 'expired'
    default: return 'invalid'
  }
}

function clearErrors(): void {
  usernameError.value = ''
  passwordError.value = ''
  confirmError.value = ''
  bannerError.value = ''
}

function validate(): boolean {
  clearErrors()
  let ok = true
  const u = username.value.trim()
  if (!u) {
    usernameError.value = t('register.errUsernameRequired')
    ok = false
  } else if (!/^[A-Za-z0-9_.-]{3,32}$/.test(u)) {
    usernameError.value = t('register.errUsernameInvalid')
    ok = false
  }
  if (!password.value) {
    passwordError.value = t('register.errPasswordRequired')
    ok = false
  } else if (password.value.length < 8) {
    passwordError.value = t('register.errPasswordTooShort')
    ok = false
  }
  if (confirmPassword.value !== password.value) {
    confirmError.value = t('register.errPasswordMismatch')
    ok = false
  }
  return ok
}

async function handleSubmit(): Promise<void> {
  if (!validate()) return
  submitting.value = true
  try {
    const user = await register(token.value, username.value.trim(), password.value)
    sessionStore.setUser(user)
    await router.replace('/my-projects')
  } catch (err) {
    applyRegisterError(err)
  } finally {
    submitting.value = false
  }
}

function applyRegisterError(err: unknown): void {
  if (!(err instanceof HttpError)) {
    bannerError.value = t('register.errUnknown')
    return
  }
  const code = err.apiError?.code
  switch (code) {
    case 'invitation_invalid':
      pageState.value = 'invalid'
      return
    case 'invitation_used':
      pageState.value = 'used'
      return
    case 'invitation_expired':
      pageState.value = 'expired'
      return
    case 'username_taken':
      usernameError.value = t('register.errUsernameTaken')
      return
    case 'invalid_username':
      usernameError.value = t('register.errUsernameInvalid')
      return
    case 'weak_password':
      passwordError.value = t('register.errPasswordTooShort')
      return
    default:
      if (err.status === 0) {
        bannerError.value = t('register.errNetwork')
      } else {
        bannerError.value = err.apiError?.message ?? t('register.errGeneric', { status: err.status })
      }
  }
}

/** 非表单状态(已用 / 过期 / 无效)的标题文案。 */
const stateTitle = computed(() => {
  switch (pageState.value) {
    case 'used':
      return t('register.usedTitle')
    case 'expired':
      return t('register.expiredTitle')
    default:
      return t('register.invalidTitle')
  }
})

/** 非表单状态(已用 / 过期 / 无效)的说明文案。 */
const stateDesc = computed(() => {
  switch (pageState.value) {
    case 'used':
      return t('register.usedDesc')
    case 'expired':
      return t('register.expiredDesc')
    default:
      return t('register.invalidDesc')
  }
})

function goLogin(): void {
  void router.replace('/login')
}

onMounted(loadInvitation)
</script>

<template>
  <div class="register-page">
    <div class="aurora" aria-hidden="true">
      <span class="blob b1" />
      <span class="blob b2" />
      <span class="blob b3" />
    </div>

    <div class="brand">
      <div class="brand-logo mono" aria-hidden="true">p&gt;</div>
      <div class="brand-name">pipe<span class="brand-name__w">wright</span></div>
    </div>

    <div class="wrap">
      <div class="card">
        <span class="eyebrow"><span class="eyebrow__dot" aria-hidden="true" />{{ t('register.eyebrow') }}</span>

        <!-- loading -->
        <template v-if="pageState === 'loading'">
          <h1 class="headline">{{ t('register.title') }}</h1>
          <p class="lede">{{ t('register.loading') }}</p>
        </template>

        <!-- form -->
        <template v-else-if="pageState === 'form'">
          <h1 class="headline">{{ t('register.title') }}</h1>
          <p class="lede">{{ t('register.subtitle') }}</p>

          <form class="reg-form" novalidate @submit.prevent="handleSubmit">
            <div v-if="bannerError" class="banner" role="alert">
              <span class="banner__icon" aria-hidden="true">⚠</span>
              <span>{{ bannerError }}</span>
            </div>

            <div class="field">
              <label class="field__label" for="reg-username">{{ t('register.usernameLabel') }}</label>
              <input
                id="reg-username"
                v-model="username"
                class="field__input"
                type="text"
                autocomplete="username"
                autocorrect="off"
                autocapitalize="off"
                spellcheck="false"
                :placeholder="t('register.usernamePlaceholder')"
                :disabled="submitting"
                :aria-invalid="usernameError ? 'true' : undefined"
                @input="usernameError = ''"
              />
              <span v-if="usernameError" class="field__error" role="alert">{{ usernameError }}</span>
            </div>

            <div class="field">
              <label class="field__label" for="reg-password">{{ t('register.passwordLabel') }}</label>
              <input
                id="reg-password"
                v-model="password"
                class="field__input"
                type="password"
                autocomplete="new-password"
                :placeholder="t('register.passwordPlaceholder')"
                :disabled="submitting"
                :aria-invalid="passwordError ? 'true' : undefined"
                @input="passwordError = ''"
              />
              <span v-if="passwordError" class="field__error" role="alert">{{ passwordError }}</span>
            </div>

            <div class="field">
              <label class="field__label" for="reg-confirm">{{ t('register.confirmLabel') }}</label>
              <input
                id="reg-confirm"
                v-model="confirmPassword"
                class="field__input"
                type="password"
                autocomplete="new-password"
                :placeholder="t('register.confirmPlaceholder')"
                :disabled="submitting"
                :aria-invalid="confirmError ? 'true' : undefined"
                @input="confirmError = ''"
              />
              <span v-if="confirmError" class="field__error" role="alert">{{ confirmError }}</span>
            </div>

            <button type="submit" class="submit" :disabled="submitting" :aria-busy="submitting">
              <span v-if="submitting" class="spinner" aria-hidden="true" />
              <span>{{ submitting ? t('register.submitting') : t('register.submit') }}</span>
            </button>
          </form>
        </template>

        <!-- invalid / used / expired -->
        <template v-else>
          <h1 class="headline">{{ stateTitle }}</h1>
          <p class="lede">{{ stateDesc }}</p>
          <button type="button" class="submit submit--ghost" @click="goLogin">
            {{ t('register.backToLogin') }}
          </button>
        </template>

        <p v-if="pageState !== 'loading'" class="foot">
          <button type="button" class="foot-link" @click="goLogin">{{ t('register.backToLogin') }}</button>
        </p>
      </div>
    </div>
  </div>
</template>

<style scoped>
.register-page {
  --lp-bg: #060a14;
  --lp-blue: #3b82f6;
  --lp-cyan: #22d3ee;
  --lp-indigo: #6366f1;
  --lp-text: #eef4ff;
  --lp-dim: #aebbd6;
  --lp-faint: #74809c;
  --lp-brd: rgba(255, 255, 255, 0.12);

  position: fixed;
  inset: 0;
  overflow: auto;
  background: var(--lp-bg);
  color: var(--lp-text);
  font-family: var(--font-sans);
  font-size: 14px;
  line-height: 1.5;
}
.mono {
  font-family: var(--font-mono, ui-monospace, monospace);
}

.aurora {
  position: absolute;
  inset: 0;
  z-index: 0;
  overflow: hidden;
  filter: saturate(1.2);
}
.blob {
  position: absolute;
  border-radius: 50%;
  filter: blur(70px);
  opacity: 0.8;
  mix-blend-mode: screen;
}
.b1 {
  width: 540px;
  height: 540px;
  background: radial-gradient(circle, var(--lp-blue), transparent 65%);
  top: -150px;
  left: -110px;
}
.b2 {
  width: 500px;
  height: 500px;
  background: radial-gradient(circle, var(--lp-cyan), transparent 65%);
  bottom: -170px;
  right: -90px;
}
.b3 {
  width: 420px;
  height: 420px;
  background: radial-gradient(circle, var(--lp-indigo), transparent 65%);
  top: 40%;
  right: 18%;
  opacity: 0.6;
}

.brand {
  position: absolute;
  top: 30px;
  left: 34px;
  z-index: 6;
  display: flex;
  align-items: center;
  gap: 11px;
}
.brand-logo {
  width: 38px;
  height: 38px;
  border-radius: 11px;
  display: grid;
  place-items: center;
  color: #fff;
  font-weight: 700;
  font-size: 15px;
  background: linear-gradient(135deg, var(--lp-blue), var(--lp-cyan));
  box-shadow: 0 6px 22px rgba(59, 130, 246, 0.5);
}
.brand-name {
  font-weight: 700;
  font-size: 17px;
}
.brand-name__w {
  background: linear-gradient(90deg, var(--lp-blue), var(--lp-cyan));
  -webkit-background-clip: text;
  background-clip: text;
  -webkit-text-fill-color: transparent;
}

.wrap {
  position: relative;
  z-index: 5;
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 64px 40px 40px;
}
.card {
  position: relative;
  width: 100%;
  max-width: 440px;
  border-radius: 26px;
  padding: 40px 38px 34px;
  background: linear-gradient(160deg, rgba(255, 255, 255, 0.08), rgba(255, 255, 255, 0.03));
  backdrop-filter: blur(34px) saturate(1.3);
  border: 1px solid var(--lp-brd);
  box-shadow:
    0 30px 80px -20px rgba(0, 0, 0, 0.7),
    inset 0 1px 0 rgba(255, 255, 255, 0.18);
}

.eyebrow {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-family: var(--font-mono, monospace);
  font-size: 11px;
  letter-spacing: 0.1em;
  text-transform: uppercase;
  color: var(--lp-dim);
  padding: 5px 11px;
  border-radius: 999px;
  border: 1px solid var(--lp-brd);
  background: rgba(255, 255, 255, 0.04);
  margin-bottom: 20px;
}
.eyebrow__dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: #34d399;
  box-shadow: 0 0 10px 1px rgba(52, 211, 153, 0.8);
}
.headline {
  font-weight: 700;
  font-size: 28px;
  letter-spacing: -0.025em;
  line-height: 1.16;
}
.lede {
  color: var(--lp-dim);
  font-size: 14.5px;
  margin-top: 10px;
}

.reg-form {
  margin-top: 24px;
}
.banner {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 11px 14px;
  border-radius: 13px;
  background: rgba(244, 63, 94, 0.13);
  border: 1px solid rgba(244, 63, 94, 0.4);
  color: #fda4af;
  font-size: 13px;
  margin-bottom: 18px;
}
.banner__icon {
  flex-shrink: 0;
}
.field {
  margin-bottom: 15px;
}
.field__label {
  display: block;
  font-size: 12.5px;
  font-weight: 600;
  color: var(--lp-dim);
  margin-bottom: 8px;
}
.field__input {
  width: 100%;
  padding: 14px 15px;
  border-radius: 14px;
  background: rgba(10, 9, 20, 0.5);
  border: 1px solid var(--lp-brd);
  color: var(--lp-text);
  font-family: var(--font-sans);
  font-size: 14.5px;
  outline: none;
  transition:
    border-color 0.2s,
    box-shadow 0.2s;
}
.field__input::placeholder {
  color: var(--lp-faint);
}
.field__input:focus {
  border-color: var(--lp-blue);
  box-shadow: 0 0 0 1.5px var(--lp-blue), 0 0 26px rgba(59, 130, 246, 0.35);
}
.field__input:disabled {
  opacity: 0.55;
}
.field__error {
  display: block;
  font-size: 12px;
  color: #fda4af;
  margin-top: 7px;
}

.submit {
  width: 100%;
  margin-top: 7px;
  padding: 16px;
  border-radius: 15px;
  border: none;
  cursor: pointer;
  font-family: var(--font-sans);
  font-size: 15px;
  font-weight: 700;
  color: #fff;
  background: linear-gradient(100deg, var(--lp-blue), var(--lp-indigo), var(--lp-cyan));
  box-shadow: 0 12px 34px -8px rgba(59, 130, 246, 0.7);
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
}
.submit:hover:not(:disabled) {
  transform: translateY(-1px);
}
.submit:disabled {
  opacity: 0.65;
  cursor: not-allowed;
}
.submit--ghost {
  margin-top: 22px;
  background: rgba(255, 255, 255, 0.08);
  border: 1px solid var(--lp-brd);
  box-shadow: none;
}
.spinner {
  display: inline-block;
  width: 15px;
  height: 15px;
  border: 2px solid rgba(255, 255, 255, 0.35);
  border-top-color: #fff;
  border-radius: 50%;
  animation: spin 0.7s linear infinite;
}
@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}

.foot {
  margin-top: 18px;
  text-align: center;
}
.foot-link {
  background: none;
  border: none;
  color: var(--lp-dim);
  cursor: pointer;
  font: inherit;
  font-size: 13px;
  text-decoration: underline;
  text-underline-offset: 3px;
}
.foot-link:hover {
  color: var(--lp-text);
}
</style>
