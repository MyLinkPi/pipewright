<script setup lang="ts">
/**
 * MemberSettings — 普通用户专属设置页(/my-settings)。
 *
 * 独立页面,**不挂**管理员 Settings 外壳(避免暴露 12 项管理员子导航)。
 * 仅调用当前用户自身的账户端点(memberGate 白名单内,无越权风险):
 *   - GET    /api/account/sessions
 *   - POST   /api/account/password
 *   - DELETE /api/account/sessions/{id}
 *
 * 四个区块:个人资料 / 修改口令(共享面板)/ 活跃会话(共享面板)/ 退出登录。
 * 改密与会话文案复用 `settingsAccount.*`;本页新增词条在 `mySettings.*`。
 */
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import AppButton from '../components/ui/AppButton.vue'
import ChangePasswordPanel from '../components/settings/ChangePasswordPanel.vue'
import ActiveSessionsPanel from '../components/settings/ActiveSessionsPanel.vue'
import { useConfirm } from '../composables/useConfirm'
import { useToast } from '../composables/useToast'
import { useSessionStore } from '../stores/session'
import { logout } from '../api/auth'

const { t } = useI18n()
const router = useRouter()
const sessionStore = useSessionStore()
const confirm = useConfirm()
const toast = useToast()

const username = computed(() => sessionStore.user?.username ?? '')
const initial = computed(() => (username.value ? username.value.slice(0, 1).toUpperCase() : '?'))

const sessionsPanel = ref<InstanceType<typeof ActiveSessionsPanel> | null>(null)
function onPasswordUpdated(): void {
  sessionsPanel.value?.reload()
}

const loggingOut = ref(false)
async function handleLogout(): Promise<void> {
  if (loggingOut.value) return
  const ok = await confirm.open({
    title: t('shell.logoutTitle'),
    body: t('shell.logoutBody'),
    confirmLabel: t('nav.logout'),
    variant: 'danger',
  })
  if (!ok) return
  loggingOut.value = true
  try {
    await logout()
  } catch {
    // 后端不可达也要本地登出 —— 失败不阻塞跳转。
    toast.info(t('shell.logoutLocalTitle'), { detail: t('shell.logoutLocalDetail') })
  } finally {
    sessionStore.clearSession()
    loggingOut.value = false
    void router.push('/login')
  }
}
</script>

<template>
  <div class="member-settings">
    <header class="page-head">
      <h1 class="page-title">{{ t('mySettings.title') }}</h1>
      <p class="page-sub">{{ t('mySettings.subtitle') }}</p>
    </header>

    <!-- 个人资料 -->
    <section class="acct-card" aria-labelledby="ms-profile-h">
      <header class="acct-card__h">
        <h2 id="ms-profile-h" class="acct-card__title">{{ t('mySettings.profileTitle') }}</h2>
        <p class="acct-card__sub">{{ t('mySettings.profileSub') }}</p>
      </header>
      <div class="profile">
        <div class="profile__avatar mono" aria-hidden="true">{{ initial }}</div>
        <div class="profile__bd">
          <div class="profile__name">{{ username }}</div>
          <div class="profile__meta">
            <span class="profile__label">{{ t('mySettings.roleLabel') }}</span>
            <span class="profile__badge">{{ t('mySettings.roleMember') }}</span>
          </div>
        </div>
      </div>
    </section>

    <!-- 共享改密面板 -->
    <ChangePasswordPanel @updated="onPasswordUpdated" />

    <!-- 共享活跃会话面板 -->
    <ActiveSessionsPanel ref="sessionsPanel" />

    <!-- 退出登录 -->
    <section class="acct-card" aria-labelledby="ms-logout-h">
      <header class="acct-card__h">
        <h2 id="ms-logout-h" class="acct-card__title">{{ t('mySettings.logoutTitle') }}</h2>
        <p class="acct-card__sub">{{ t('mySettings.logoutSub') }}</p>
      </header>
      <div class="actions">
        <AppButton variant="danger" :loading="loggingOut" @click="handleLogout">
          {{ t('mySettings.logoutBtn') }}
        </AppButton>
      </div>
    </section>
  </div>
</template>

<style scoped>
.member-settings {
  display: flex;
  flex-direction: column;
  gap: 18px;
  max-width: 720px;
}

.page-head {
  margin-bottom: 2px;
}
.page-title {
  font-size: 1.25rem;
  font-weight: 700;
  color: var(--color-text);
}
.page-sub {
  font-size: 0.83rem;
  color: var(--color-faint);
  margin-top: 5px;
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

/* profile */
.profile {
  display: flex;
  align-items: center;
  gap: 14px;
}
.profile__avatar {
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
.profile__name {
  font-size: 0.95rem;
  font-weight: 600;
  color: var(--color-text);
}
.profile__meta {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 4px;
}
.profile__label {
  font-size: 0.78rem;
  color: var(--color-faint);
}
.profile__badge {
  font-size: 0.68rem;
  font-weight: 600;
  color: var(--color-primary);
  background: var(--color-primary-soft);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-sm);
  padding: 1px 7px;
}

.actions {
  display: flex;
  justify-content: flex-end;
}
</style>
