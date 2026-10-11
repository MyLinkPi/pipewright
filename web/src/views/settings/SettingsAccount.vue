<script setup lang="ts">
/**
 * SettingsAccount (Story 1.7) — 管理员账户设置。
 *
 * 组合共享面板(与普通用户 MemberSettings 复用同一套账户逻辑):
 *   - 改密面板 ChangePasswordPanel(emit updated → 刷新会话列表)
 *   - 活跃会话面板 ActiveSessionsPanel(暴露 reload)
 * 另含管理员专属:身份展示 + 「重新引导」按钮。
 */
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppButton from '../../components/ui/AppButton.vue'
import ChangePasswordPanel from '../../components/settings/ChangePasswordPanel.vue'
import ActiveSessionsPanel from '../../components/settings/ActiveSessionsPanel.vue'
import { useToast } from '../../composables/useToast'
import { resetOnboarding } from '../../composables/useOnboarding'
import { useSessionStore } from '../../stores/session'

const { t } = useI18n()
const toast = useToast()
const router = useRouter()
const sessionStore = useSessionStore()

const username = computed(() => sessionStore.user?.username ?? 'admin')

// 改口令成功后服务端吊销其它会话 → 刷新可见会话列表。
const sessionsPanel = ref<InstanceType<typeof ActiveSessionsPanel> | null>(null)
function onPasswordUpdated(): void {
  sessionsPanel.value?.reload()
}

// ——— 重新引导 ———
function onReopenOnboarding(): void {
  resetOnboarding()
  toast.info(t('settingsAccount.toastOnboardingReset'), { detail: t('settingsAccount.toastOnboardingResetDetail') })
  router.push('/onboarding')
}
</script>

<template>
  <div class="account">
    <!-- Identity -->
    <section class="acct-card" aria-labelledby="acct-identity-h">
      <header class="acct-card__h">
        <h2 id="acct-identity-h" class="acct-card__title">{{ t('settingsAccount.identityTitle') }}</h2>
        <p class="acct-card__sub">{{ t('settingsAccount.identitySub') }}</p>
      </header>
      <div class="acct-identity">
        <div class="acct-avatar mono" aria-hidden="true">{{ username.slice(0, 1).toUpperCase() }}</div>
        <div>
          <div class="acct-identity__name">{{ username }}</div>
          <div class="acct-identity__role">{{ t('settingsAccount.roleAdmin') }}</div>
        </div>
      </div>
    </section>

    <!-- 共享改密面板 -->
    <ChangePasswordPanel @updated="onPasswordUpdated" />

    <!-- 共享活跃会话面板 -->
    <ActiveSessionsPanel ref="sessionsPanel" />

    <!-- Re-run onboarding -->
    <section class="acct-card" aria-labelledby="acct-onboarding-h">
      <header class="acct-card__h">
        <h2 id="acct-onboarding-h" class="acct-card__title">{{ t('settingsAccount.onboardingTitle') }}</h2>
        <p class="acct-card__sub">{{ t('settingsAccount.onboardingSub') }}</p>
      </header>
      <div class="acct-form__actions">
        <AppButton variant="default" @click="onReopenOnboarding">{{ t('settingsAccount.onboardingBtn') }}</AppButton>
      </div>
    </section>
  </div>
</template>

<style scoped>
.account {
  display: flex;
  flex-direction: column;
  gap: 18px;
  max-width: 720px;
}

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

/* identity */
.acct-identity {
  display: flex;
  align-items: center;
  gap: 14px;
}
.acct-avatar {
  width: 44px;
  height: 44px;
  border-radius: var(--rounded-lg);
  background: var(--color-primary-soft);
  color: var(--color-primary);
  display: grid;
  place-items: center;
  font-weight: 700;
  font-size: 1.1rem;
  flex: none;
}
.acct-identity__name {
  font-size: 0.95rem;
  font-weight: 600;
  color: var(--color-text);
}
.acct-identity__role {
  font-size: 0.78rem;
  color: var(--color-faint);
  margin-top: 2px;
}

.acct-form__actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
</style>
