<script setup lang="ts">
/**
 * Users — 管理员用户体系(/users)。
 *
 * 三个区块:
 *   1. 用户列表(GET /api/users)
 *   2. 邀请链接:生成(展示完整注册链接 + 复制)、列表、撤销
 *   3. 项目成员:选择项目 → 成员列表 → 添加用户 / 移出成员
 *
 * 仅管理员可访问(路由 meta.roles + 后端 requireAdmin 兜底)。
 */
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppButton from '../components/ui/AppButton.vue'
import AppSelect from '../components/ui/AppSelect.vue'
import EmptyState from '../components/ui/EmptyState.vue'
import SkeletonBlock from '../components/ui/SkeletonBlock.vue'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import { listProjects, type Project } from '../api/projects'
import {
  listUsers,
  createInvitation,
  listInvitations,
  revokeInvitation,
  listProjectMembers,
  addProjectMember,
  removeProjectMember,
  type UserDTO,
  type InvitationDTO,
} from '../api/users'
import { adminResetPasskeys, adminResetTOTP } from '../api/mfa'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

// ─── 用户列表 ────────────────────────────────────────────────────────────────
const users = ref<UserDTO[]>([])
const usersLoading = ref(true)
const usersError = ref('')

async function loadUsers(): Promise<void> {
  usersLoading.value = true
  usersError.value = ''
  try {
    users.value = await listUsers()
  } catch (err) {
    usersError.value = err instanceof Error ? err.message : t('users.errLoadUsers')
  } finally {
    usersLoading.value = false
  }
}

// ─── 两步验证管理(成员被锁死时的恢复通道)───────────────────────────────────
const mfaBusy = ref('')

async function onResetTOTP(u: UserDTO): Promise<void> {
  if (mfaBusy.value) return
  const ok = await confirm.open({
    title: t('mfa.adminResetTotp'),
    body: t('mfa.adminResetTotpConfirm', { username: u.username }),
    confirmLabel: t('mfa.adminResetTotp'),
    variant: 'danger',
  })
  if (!ok) return
  mfaBusy.value = u.id
  try {
    await adminResetTOTP(u.id)
    toast.success(t('mfa.adminResetDone'))
    await loadUsers()
  } catch {
    toast.error(t('mfa.errCode'))
  } finally {
    mfaBusy.value = ''
  }
}

async function onResetPasskeys(u: UserDTO): Promise<void> {
  if (mfaBusy.value) return
  const ok = await confirm.open({
    title: t('mfa.adminResetPasskeys'),
    body: t('mfa.adminResetPasskeysConfirm', { username: u.username }),
    confirmLabel: t('mfa.adminResetPasskeys'),
    variant: 'danger',
  })
  if (!ok) return
  mfaBusy.value = u.id
  try {
    await adminResetPasskeys(u.id)
    toast.success(t('mfa.adminResetDone'))
    await loadUsers()
  } catch {
    toast.error(t('mfa.errCode'))
  } finally {
    mfaBusy.value = ''
  }
}

// ─── 邀请 ────────────────────────────────────────────────────────────────────
const invitations = ref<InvitationDTO[]>([])
const invitesLoading = ref(true)
const invitesError = ref('')
const creatingInvite = ref(false)
/** 刚创建的邀请链接(展示完整链接 + 复制按钮)。 */
const createdLink = ref('')

const linkBase = computed(() => (typeof window !== 'undefined' ? window.location.origin : ''))
function inviteUrl(token: string): string {
  return `${linkBase.value}/register?token=${token}`
}

async function loadInvitations(): Promise<void> {
  invitesLoading.value = true
  invitesError.value = ''
  try {
    invitations.value = await listInvitations()
  } catch (err) {
    invitesError.value = err instanceof Error ? err.message : t('users.errLoadInvites')
  } finally {
    invitesLoading.value = false
  }
}

async function onCreateInvite(): Promise<void> {
  if (creatingInvite.value) return
  creatingInvite.value = true
  try {
    const inv = await createInvitation()
    createdLink.value = inviteUrl(inv.token)
    await loadInvitations()
    toast.success(t('users.toastInviteCreated'), { detail: t('users.inviteCreatedDetail') })
  } catch (err) {
    toast.error(t('users.errGeneric'), { detail: err instanceof Error ? err.message : '' })
  } finally {
    creatingInvite.value = false
  }
}

async function copyLink(): Promise<void> {
  if (!createdLink.value) return
  try {
    await navigator.clipboard.writeText(createdLink.value)
    toast.success(t('users.copied'))
  } catch {
    toast.error(t('users.copyFailed'))
  }
}

async function onRevokeInvite(inv: InvitationDTO): Promise<void> {
  const ok = await confirm.open({
    title: t('users.revokeConfirmTitle'),
    body: t('users.revokeConfirmBody'),
    confirmLabel: t('users.revokeConfirmOk'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await revokeInvitation(inv.id)
    if (createdLink.value === inviteUrl(inv.token)) createdLink.value = ''
    await loadInvitations()
    toast.success(t('users.toastInviteRevoked'))
  } catch (err) {
    toast.error(t('users.errGeneric'), { detail: err instanceof Error ? err.message : '' })
  }
}

function inviteStateLabel(state: string): string {
  switch (state) {
    case 'pending': return t('users.inviteStatePending')
    case 'used': return t('users.inviteStateUsed')
    case 'expired': return t('users.inviteStateExpired')
    default: return state
  }
}

// ─── 项目成员 ────────────────────────────────────────────────────────────────
const projects = ref<Project[]>([])
const selectedProjectId = ref('')
const members = ref<UserDTO[]>([])
const membersLoading = ref(false)
const membersError = ref('')
const addingMember = ref(false)
const selectedUserId = ref('')

const projectOptions = computed(() => projects.value.map((p) => ({ value: p.id, label: p.name })))

// 仅展示尚未加入该项目的用户(避免重复添加)。
const availableUsers = computed(() => {
  const memberIds = new Set(members.value.map((m) => m.id))
  return users.value.filter((u) => !memberIds.has(u.id))
})
const availableUserOptions = computed(() => availableUsers.value.map((u) => ({ value: u.id, label: u.username })))

async function loadProjects(): Promise<void> {
  try {
    projects.value = await listProjects()
  } catch (err) {
    toast.error(t('users.errLoadProjects'), { detail: err instanceof Error ? err.message : '' })
  }
}

async function onSelectProject(id: string): Promise<void> {
  selectedProjectId.value = id
  selectedUserId.value = ''
  if (!id) {
    members.value = []
    return
  }
  await loadMembers()
}

async function loadMembers(): Promise<void> {
  if (!selectedProjectId.value) return
  membersLoading.value = true
  membersError.value = ''
  try {
    members.value = await listProjectMembers(selectedProjectId.value)
  } catch (err) {
    membersError.value = err instanceof Error ? err.message : t('users.errLoadMembers')
  } finally {
    membersLoading.value = false
  }
}

async function onAddMember(): Promise<void> {
  if (!selectedUserId.value || !selectedProjectId.value || addingMember.value) return
  addingMember.value = true
  try {
    await addProjectMember(selectedProjectId.value, selectedUserId.value)
    selectedUserId.value = ''
    await loadMembers()
    toast.success(t('users.toastMemberAdded'))
  } catch (err) {
    toast.error(t('users.errGeneric'), { detail: err instanceof Error ? err.message : '' })
  } finally {
    addingMember.value = false
  }
}

async function onRemoveMember(m: UserDTO): Promise<void> {
  const ok = await confirm.open({
    title: t('users.removeConfirmTitle'),
    body: t('users.removeConfirmBody'),
    confirmLabel: t('users.removeConfirmOk'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await removeProjectMember(selectedProjectId.value, m.id)
    await loadMembers()
    toast.success(t('users.toastMemberRemoved'))
  } catch (err) {
    toast.error(t('users.errGeneric'), { detail: err instanceof Error ? err.message : '' })
  }
}

function formatTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString('zh-CN', {
    year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit',
  })
}

onMounted(async () => {
  await Promise.all([loadUsers(), loadInvitations(), loadProjects()])
})
</script>

<template>
  <div class="users-page">
    <header class="page-head">
      <h1 class="page-title">{{ t('users.title') }}</h1>
      <p class="page-sub">{{ t('users.subtitle') }}</p>
    </header>

    <!-- 1. 用户列表 -->
    <section class="card" aria-labelledby="users-list-h">
      <header class="card__h">
        <h2 id="users-list-h" class="card__title">{{ t('users.usersTitle') }}</h2>
        <p class="card__sub">{{ t('users.usersSub') }}</p>
      </header>

      <div v-if="usersLoading" class="stack" aria-busy="true">
        <SkeletonBlock :height="46" />
        <SkeletonBlock :height="46" />
      </div>
      <p v-else-if="usersError" class="load-error" role="alert">
        {{ usersError }}
        <button type="button" class="link" @click="loadUsers">{{ t('users.retry') }}</button>
      </p>
      <EmptyState v-else-if="users.length === 0" :title="t('users.usersEmpty')" />
      <div v-else class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th scope="col">{{ t('users.colUsername') }}</th>
              <th scope="col">{{ t('users.colRole') }}</th>
              <th scope="col">{{ t('users.colStatus') }}</th>
              <th scope="col">{{ t('mfa.adminColTotp') }}</th>
              <th scope="col">{{ t('mfa.adminColPasskeys') }}</th>
              <th scope="col">{{ t('users.colCreatedAt') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="u in users" :key="u.id">
              <td class="cell-strong">{{ u.username }}</td>
              <td><span class="pill">{{ u.role === 'member' ? t('users.roleMember') : t('users.roleAdmin') }}</span></td>
              <td><span class="pill pill--ok">{{ t('users.statusActive') }}</span></td>
              <td>
                <span class="pill" :class="u.totpEnabled ? 'pill--ok' : 'pill--muted'">
                  {{ u.totpEnabled ? t('mfa.totpOn') : t('mfa.totpOff') }}
                </span>
              </td>
              <td class="cell-muted users-mfa-cell">
                <span class="users-mfa-count">{{ u.passkeyCount }}</span>
                <span
                  v-if="u.totpEnabled || u.passkeyCount > 0"
                  class="users-mfa-actions"
                >
                  <button
                    v-if="u.totpEnabled"
                    type="button"
                    class="link"
                    :disabled="mfaBusy === u.id"
                    @click="onResetTOTP(u)"
                  >{{ t('mfa.adminResetTotp') }}</button>
                  <button
                    v-if="u.passkeyCount > 0"
                    type="button"
                    class="link users-mfa-reset"
                    :disabled="mfaBusy === u.id"
                    @click="onResetPasskeys(u)"
                  >{{ t('mfa.adminResetPasskeys') }}</button>
                </span>
              </td>
              <td class="cell-muted">{{ formatTime(u.createdAt) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- 2. 邀请链接 -->
    <section class="card" aria-labelledby="users-invites-h">
      <header class="card__h card__h--row">
        <div>
          <h2 id="users-invites-h" class="card__title">{{ t('users.invitesTitle') }}</h2>
          <p class="card__sub">{{ t('users.invitesSub') }}</p>
        </div>
        <AppButton variant="primary" :loading="creatingInvite" @click="onCreateInvite">
          {{ t('users.createInvite') }}
        </AppButton>
      </header>

      <!-- 刚创建的链接 -->
      <div v-if="createdLink" class="invite-new" role="status">
        <div class="invite-new__label">{{ t('users.inviteCreatedTitle') }}</div>
        <div class="invite-new__row">
          <input class="invite-new__input mono" :value="createdLink" readonly @focus="($event.target as HTMLInputElement).select()" />
          <AppButton variant="default" @click="copyLink">{{ t('users.copyInvite') }}</AppButton>
        </div>
        <p class="invite-new__hint">{{ t('users.inviteCreatedDetail') }}</p>
      </div>

      <div v-if="invitesLoading" class="stack" aria-busy="true">
        <SkeletonBlock :height="46" />
      </div>
      <p v-else-if="invitesError" class="load-error" role="alert">
        {{ invitesError }}
        <button type="button" class="link" @click="loadInvitations">{{ t('users.retry') }}</button>
      </p>
      <EmptyState v-else-if="invitations.length === 0" :title="t('users.invitesEmpty')" />
      <div v-else class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th scope="col">{{ t('users.colState') }}</th>
              <th scope="col">{{ t('users.colExpiresAt') }}</th>
              <th scope="col">{{ t('users.colUsedAt') }}</th>
              <th scope="col" class="col-act"><span class="sr-only">{{ t('users.revokeInvite') }}</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="inv in invitations" :key="inv.id">
              <td><span class="pill" :class="{ 'pill--ok': inv.state === 'used' }">{{ inviteStateLabel(inv.state) }}</span></td>
              <td class="cell-muted">{{ formatTime(inv.expiresAt) }}</td>
              <td class="cell-muted">{{ inv.usedAt ? formatTime(inv.usedAt) : '—' }}</td>
              <td class="col-act">
                <AppButton v-if="inv.state === 'pending'" variant="danger" @click="onRevokeInvite(inv)">
                  {{ t('users.revokeInvite') }}
                </AppButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- 3. 项目成员 -->
    <section class="card" aria-labelledby="users-members-h">
      <header class="card__h">
        <h2 id="users-members-h" class="card__title">{{ t('users.membersTitle') }}</h2>
        <p class="card__sub">{{ t('users.membersSub') }}</p>
      </header>

      <div class="member-toolbar">
        <AppSelect
          v-model="selectedProjectId"
          :options="projectOptions"
          :placeholder="t('users.selectProjectPlaceholder')"
          :aria-label="t('users.selectProject')"
          min-width="240px"
          @update:model-value="onSelectProject"
        />
      </div>

      <template v-if="selectedProjectId">
        <div v-if="membersLoading" class="stack" aria-busy="true">
          <SkeletonBlock :height="46" />
        </div>
        <p v-else-if="membersError" class="load-error" role="alert">
          {{ membersError }}
          <button type="button" class="link" @click="loadMembers">{{ t('users.retry') }}</button>
        </p>
        <EmptyState v-else-if="members.length === 0" :title="t('users.membersEmpty')" />

        <div v-else class="member-list">
          <div v-for="m in members" :key="m.id" class="member-row">
            <span class="member-name">{{ m.username }}</span>
            <AppButton variant="danger" @click="onRemoveMember(m)">{{ t('users.removeMember') }}</AppButton>
          </div>
        </div>

        <div class="member-add">
          <AppSelect
            v-model="selectedUserId"
            :options="availableUserOptions"
            :placeholder="t('users.addMemberPlaceholder')"
            :aria-label="t('users.addMember')"
            min-width="240px"
          />
          <AppButton
            variant="primary"
            :disabled="!selectedUserId"
            :loading="addingMember"
            @click="onAddMember"
          >{{ t('users.addMember') }}</AppButton>
        </div>
      </template>
    </section>
  </div>
</template>

<style scoped>
.users-page {
  display: flex;
  flex-direction: column;
  gap: 18px;
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

.card {
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  box-shadow: var(--shadow);
  padding: 20px 22px;
}
.card__h {
  margin-bottom: 16px;
}
.card__h--row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
}
.card__title {
  font-size: 1rem;
  font-weight: 600;
  color: var(--color-text);
}
.card__sub {
  font-size: 0.79rem;
  color: var(--color-faint);
  margin-top: 4px;
  line-height: 1.5;
}

.stack {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.load-error {
  font-size: 0.86rem;
  color: var(--color-red);
}
.link {
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

.table-wrap {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-lg);
  overflow-x: auto;
}
.table {
  width: 100%;
  border-collapse: collapse;
  font-size: 0.85rem;
}
.table th {
  text-align: left;
  font-weight: 600;
  color: var(--color-faint);
  font-size: 0.75rem;
  padding: 11px 14px;
  border-bottom: 1px solid var(--color-border);
  white-space: nowrap;
}
.table td {
  padding: 11px 14px;
  border-bottom: 1px solid var(--color-border);
  color: var(--color-text);
}
.table tbody tr:last-child td {
  border-bottom: none;
}
.cell-strong {
  font-weight: 600;
}
.cell-muted {
  color: var(--color-dim);
  font-size: 0.8rem;
}
.col-act {
  text-align: right;
  width: 1%;
  white-space: nowrap;
}
.pill {
  display: inline-block;
  font-size: 0.72rem;
  font-weight: 600;
  padding: 2px 9px;
  border-radius: var(--rounded-sm);
  background: var(--color-card-2);
  border: 1px solid var(--color-border-strong);
  color: var(--color-dim);
}
.pill--muted {
  background: rgba(107, 114, 128, 0.12);
  color: #6b7280;
}
.pill--ok {
  color: var(--color-green);
  background: var(--color-green-soft);
  border-color: var(--color-green-line);
}

.invite-new {
  border: 1px solid var(--color-primary);
  background: var(--color-primary-soft);
  border-radius: var(--rounded-lg);
  padding: 14px 16px;
  margin-bottom: 16px;
}
.invite-new__label {
  font-size: 0.82rem;
  font-weight: 600;
  color: var(--color-text);
}
.invite-new__row {
  display: flex;
  gap: 10px;
  margin-top: 9px;
}
.invite-new__input {
  flex: 1;
  height: 36px;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  padding: 0 12px;
  color: var(--color-text);
  font-size: 0.82rem;
}
.invite-new__hint {
  font-size: 0.75rem;
  color: var(--color-faint);
  margin-top: 8px;
}

.member-toolbar {
  margin-bottom: 14px;
}
.member-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.member-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-lg);
  background: var(--color-inset);
}
.member-name {
  font-size: 0.86rem;
  font-weight: 500;
  color: var(--color-text);
}
.member-add {
  display: flex;
  gap: 10px;
  margin-top: 14px;
  flex-wrap: wrap;
}
.sr-only {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}
.users-mfa-cell {
  white-space: nowrap;
}
.users-mfa-actions {
  display: inline-flex;
  gap: 10px;
  margin-left: 8px;
}
.users-mfa-reset {
  color: #ef4444;
}
</style>
