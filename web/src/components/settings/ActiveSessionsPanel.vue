<script setup lang="ts">
/**
 * ActiveSessionsPanel — 活跃会话列表 + 单条撤销(管理员与普通用户共用)。
 *
 * 自包含:加载 GET /api/account/sessions → 列表(current 标记)→ 撤销(ConfirmDialog 二次确认)。
 * 撤销「当前会话」→ 清本地会话缓存并回登录页;撤销其它会话 → toast + 刷新列表。
 * 通过 defineExpose 暴露 reload(),供父组件在改口令后(服务端吊销其它会话)刷新。
 *
 * 文案复用 `settingsAccount.*` 词条(8 语言已齐备)。
 */
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppButton from '../ui/AppButton.vue'
import EmptyState from '../ui/EmptyState.vue'
import SkeletonBlock from '../ui/SkeletonBlock.vue'
import { useToast } from '../../composables/useToast'
import { useConfirm } from '../../composables/useConfirm'
import { useSessionStore } from '../../stores/session'
import { listSessions, revokeSession, type Session } from '../../api/account'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()
const router = useRouter()
const sessionStore = useSessionStore()

const sessions = ref<Session[]>([])
const sessionsLoading = ref(true)
const sessionsError = ref('')

async function loadSessions(): Promise<void> {
  sessionsLoading.value = true
  sessionsError.value = ''
  try {
    sessions.value = await listSessions()
  } catch (err) {
    sessionsError.value = err instanceof Error ? err.message : t('settingsAccount.errLoadSessions')
  } finally {
    sessionsLoading.value = false
  }
}

async function onRevoke(s: Session): Promise<void> {
  const ok = await confirm.open({
    title: s.current ? t('settingsAccount.revokeCurrentTitle') : t('settingsAccount.revokeTitle'),
    body: s.current
      ? t('settingsAccount.revokeCurrentBody')
      : t('settingsAccount.revokeBody'),
    confirmLabel: t('settingsAccount.revokeConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await revokeSession(s.id)
    if (s.current) {
      // Revoking the current session — backend 204; next call will 401 → login.
      toast.info(t('settingsAccount.toastCurrentRevoked'), { detail: t('settingsAccount.toastCurrentRevokedDetail') })
      sessionStore.clearSession()
      router.push('/login')
      return
    }
    toast.success(t('settingsAccount.toastSessionRevoked'))
    await loadSessions()
  } catch (err) {
    toast.error(t('settingsAccount.toastRevokeFailed'), {
      detail: err instanceof Error ? err.message : t('settingsAccount.unknownError'),
    })
  }
}

function formatTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

onMounted(loadSessions)

// 供父组件在改口令后刷新(服务端已吊销其它会话)。
defineExpose({ reload: loadSessions })
</script>

<template>
  <section class="acct-card" aria-labelledby="acct-sessions-h">
    <header class="acct-card__h">
      <h2 id="acct-sessions-h" class="acct-card__title">{{ t('settingsAccount.sessionsTitle') }}</h2>
      <p class="acct-card__sub">{{ t('settingsAccount.sessionsSub') }}</p>
    </header>

    <div v-if="sessionsLoading" class="acct-sessions__loading" aria-busy="true">
      <SkeletonBlock :height="52" />
      <SkeletonBlock :height="52" />
    </div>

    <p v-else-if="sessionsError" class="acct-sessions__error" role="alert">
      {{ sessionsError }}
      <button type="button" class="acct-link" @click="loadSessions">{{ t('settingsAccount.retry') }}</button>
    </p>

    <EmptyState
      v-else-if="sessions.length === 0"
      :title="t('settingsAccount.emptyTitle')"
      :description="t('settingsAccount.emptyDesc')"
    />

    <ul v-else class="acct-sessions">
      <li v-for="s in sessions" :key="s.id" class="acct-session">
        <div class="acct-session__icon" aria-hidden="true">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">
            <rect x="2" y="4" width="20" height="13" rx="2" />
            <path d="M8 21h8M12 17v4" />
          </svg>
        </div>
        <div class="acct-session__bd">
          <div class="acct-session__top">
            <span class="acct-session__id mono">{{ s.id }}</span>
            <span v-if="s.current" class="acct-session__current">{{ t('settingsAccount.currentSession') }}</span>
          </div>
          <div class="acct-session__meta">
            {{ t('settingsAccount.sessionMeta', { created: formatTime(s.createdAt), lastSeen: formatTime(s.lastSeenAt) }) }}
          </div>
        </div>
        <div class="acct-session__act">
          <AppButton variant="danger" @click="onRevoke(s)">{{ t('settingsAccount.revokeBtn') }}</AppButton>
        </div>
      </li>
    </ul>
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
.acct-sessions {
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.acct-sessions__loading {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.acct-sessions__error {
  font-size: 0.83rem;
  color: var(--color-red);
}
.acct-session {
  display: flex;
  align-items: center;
  gap: 13px;
  padding: 12px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-lg);
  background: var(--color-inset);
}
.acct-session__icon {
  width: 34px;
  height: 34px;
  border-radius: var(--rounded);
  background: var(--color-card);
  border: 1px solid var(--color-border);
  display: grid;
  place-items: center;
  color: var(--color-dim);
  flex: none;
}
.acct-session__icon svg {
  width: 17px;
  height: 17px;
}
.acct-session__bd {
  flex: 1;
  min-width: 0;
}
.acct-session__top {
  display: flex;
  align-items: center;
  gap: 9px;
}
.acct-session__id {
  font-size: 0.82rem;
  color: var(--color-text);
  font-weight: 500;
}
.acct-session__current {
  font-size: 0.68rem;
  color: var(--color-green);
  background: var(--color-green-soft);
  border: 1px solid var(--color-green-line);
  border-radius: var(--rounded-sm);
  padding: 1px 7px;
  font-weight: 600;
}
.acct-session__meta {
  font-size: 0.75rem;
  color: var(--color-faint);
  margin-top: 3px;
}
.acct-session__act {
  flex: none;
}
.acct-link {
  background: none;
  border: none;
  color: var(--color-primary);
  cursor: pointer;
  font: inherit;
  text-decoration: underline;
  text-underline-offset: 2px;
  padding: 0;
  margin-left: 6px;
}
</style>
