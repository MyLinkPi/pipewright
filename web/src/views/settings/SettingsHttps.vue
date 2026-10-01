<script setup lang="ts">
/**
 * SettingsHttps — 平台 HTTPS 访问(宿主 nginx 自动配置)。
 *
 * 当 nginx 所在机器(通常即平台自身所在主机,登记为目标服务器)装有宿主 nginx 时,
 * 把平台 Web 页面发布为 HTTPS:
 *   - 选服务器(可探测:nginx 版本 / root·免密 sudo / conf.d 加载情况)
 *   - 选访问域名 + 复用「证书管理」的已签发证书(SAN 须覆盖域名)
 *   - 可选 80→443 跳转(默认开)、高级项自定义反代上游(默认 127.0.0.1:平台端口)
 *   - 保存并应用 = 下发证书 + 写 /etc/nginx/conf.d vhost + nginx -t(失败回滚)+ reload
 *   - 证书续期后由平台自动重新下发;可禁用并清理远端配置
 *
 * Reuses: AppButton / AppSelect tokens + settings 页模式。No new UI libraries.
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppButton from '../../components/ui/AppButton.vue'
import AppSelect from '../../components/ui/AppSelect.vue'
import { useToast } from '../../composables/useToast'
import { useConfirm } from '../../composables/useConfirm'
import { HttpError } from '../../api/http'
import { listServers, type Server } from '../../api/servers'
import { listCerts, type Cert } from '../../api/certMgmt'
import { getSystemConfig, saveSystemConfig } from '../../api/systemConfig'
import {
  getPlatformHttpsSettings,
  savePlatformHttpsSettings,
  detectPlatformHttps,
  applyPlatformHttps,
  disablePlatformHttps,
  certCoversDomain,
  type PlatformHttpsSettings,
  type PlatformHttpsDetect,
} from '../../api/platformHttps'

type LoadState = 'loading' | 'ready' | 'error'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

// ─── state ────────────────────────────────────────────────────────────────────

const loadState = ref<LoadState>('loading')
const loadError = ref('')
const servers = ref<Server[]>([])
const certs = ref<Cert[]>([])
const saved = ref<PlatformHttpsSettings | null>(null)
const detect = ref<PlatformHttpsDetect | null>(null)
const detecting = ref(false)
const showAdvanced = ref(false)
const saving = ref(false)
const applying = ref(false)
const disabling = ref(false)

const form = reactive({
  enabled: false,
  serverId: '',
  domain: '',
  certId: '',
  upstreamHost: '127.0.0.1',
  upstreamPort: 8080,
  httpRedirect: true,
})

// 外部访问地址(系统配置):通知审批链接 / PR 回写链接的前缀,运行时改即时生效。
const publicUrl = ref('')
const publicUrlSaving = ref(false)

// ─── load ─────────────────────────────────────────────────────────────────────

async function load(): Promise<void> {
  loadState.value = 'loading'
  try {
    const [st, sv, cs, sys] = await Promise.all([
      getPlatformHttpsSettings(),
      listServers(),
      listCerts(),
      getSystemConfig(),
    ])
    saved.value = st
    servers.value = sv
    certs.value = cs.items
    publicUrl.value = sys.publicUrl
    form.enabled = st.enabled
    form.serverId = st.serverId
    form.domain = st.domain
    form.certId = st.certId
    form.upstreamHost = st.upstreamHost || '127.0.0.1'
    form.upstreamPort = st.upstreamPort > 0 ? st.upstreamPort : defaultPortOf(st.effectiveUpstream)
    form.httpRedirect = st.httpRedirect
    // 已启用或自定义过上游 → 展开高级区,让用户看见当前生效值。
    if (st.enabled && (st.upstreamHost !== '127.0.0.1' || st.upstreamPort > 0)) showAdvanced.value = true
    loadState.value = 'ready'
  } catch (err) {
    loadState.value = 'error'
    loadError.value = httpMessage(err, t('platformHttps.errLoad'))
  }
}

function defaultPortOf(effective: string): number {
  const p = Number.parseInt(effective.split(':').pop() ?? '', 10)
  return Number.isFinite(p) && p > 0 ? p : 8080
}

onMounted(load)

// ─── selectors / detect ───────────────────────────────────────────────────────

const serverOptions = computed(() =>
  servers.value.map((s) => ({ value: s.id, label: `${s.name}(${s.host})` })),
)

/** 覆盖当前域名的已签发证书(下拉过滤;规则与服务端一致)。 */
const eligibleCerts = computed(() =>
  certs.value.filter((c) => c.status === 'issued' && certCoversDomain(c, form.domain)),
)

const certOptions = computed(() =>
  eligibleCerts.value.map((c) => ({
    value: c.id,
    label: `${c.primaryDomain}${c.domains.length > 1 ? ` (+${c.domains.length - 1})` : ''}`,
  })),
)

function certOf(id: string): Cert | undefined {
  return certs.value.find((c) => c.id === id)
}

async function runDetect(): Promise<void> {
  if (!form.serverId || detecting.value) return
  detecting.value = true
  detect.value = null
  try {
    detect.value = await detectPlatformHttps(form.serverId)
  } catch (err) {
    toast.error(t('platformHttps.detectFailed'), { detail: httpMessage(err, t('platformHttps.errLoad')) })
  } finally {
    detecting.value = false
  }
}

function onServerChange(): void {
  detect.value = null // 换机后旧探测失效
}

// ─── actions ──────────────────────────────────────────────────────────────────

function buildPayload(apply: boolean): Record<string, unknown> {
  return {
    enabled: form.enabled,
    serverId: form.serverId,
    domain: form.domain,
    certId: form.certId,
    upstreamHost: showAdvanced.value ? form.upstreamHost : '',
    upstreamPort: showAdvanced.value ? form.upstreamPort : 0,
    httpRedirect: form.httpRedirect,
    apply,
  }
}

async function handleSave(apply: boolean): Promise<void> {
  if (saving.value) return
  saving.value = true
  try {
    const res = await savePlatformHttpsSettings(buildPayload(apply) as never)
    saved.value = res.settings
    if (res.applyError) {
      toast.error(t('platformHttps.toastApplyFailed'), { detail: res.applyError })
    } else if (apply && res.settings.enabled) {
      toast.success(t('platformHttps.toastApplied'))
    } else {
      toast.success(t('platformHttps.toastSaved'))
    }
  } catch (err) {
    toast.error(t('platformHttps.toastSaveFailed'), { detail: httpMessage(err, t('platformHttps.errLoad')) })
  } finally {
    saving.value = false
  }
}

async function handleApply(): Promise<void> {
  if (applying.value) return
  applying.value = true
  try {
    saved.value = await applyPlatformHttps()
    toast.success(t('platformHttps.toastApplied'))
  } catch (err) {
    toast.error(t('platformHttps.toastApplyFailed'), { detail: httpMessage(err, t('platformHttps.errLoad')) })
    // 失败状态已回填设置行,刷新展示。
    try {
      saved.value = await getPlatformHttpsSettings()
    } catch {
      /* 保持旧值 */
    }
  } finally {
    applying.value = false
  }
}

async function handleDisable(): Promise<void> {
  if (disabling.value) return
  if (
    !(await confirm.open({
      title: t('platformHttps.disableTitle'),
      body: t('platformHttps.disableBody'),
      confirmLabel: t('platformHttps.disableBtn'),
    }))
  ) {
    return
  }
  disabling.value = true
  try {
    saved.value = await disablePlatformHttps()
    form.enabled = false
    detect.value = null
    toast.success(t('platformHttps.toastDisabled'))
  } catch (err) {
    toast.error(t('platformHttps.toastDisableFailed'), { detail: httpMessage(err, t('platformHttps.errLoad')) })
  } finally {
    disabling.value = false
  }
}

// ─── helpers ──────────────────────────────────────────────────────────────────

/** 建议的外部地址:HTTPS 已启用并有域名时,推荐 https://<域名> 一键填入。 */
const suggestUrl = computed(() =>
  saved.value?.enabled && saved.value.domain ? `https://${saved.value.domain}` : '',
)

async function handleSavePublicUrl(): Promise<void> {
  if (publicUrlSaving.value) return
  publicUrlSaving.value = true
  try {
    const c = await saveSystemConfig(publicUrl.value.trim())
    publicUrl.value = c.publicUrl
    toast.success(t('platformHttps.publicUrlSaved'))
  } catch (err) {
    toast.error(t('platformHttps.publicUrlSaveFailed'), { detail: httpMessage(err, t('platformHttps.errLoad')) })
  } finally {
    publicUrlSaving.value = false
  }
}

function httpMessage(err: unknown, fallback: string): string {
  if (err instanceof HttpError) {
    if (err.status === 0) return t('platformHttps.errNoServer')
    return err.apiError?.message ?? `HTTP ${err.status}`
  }
  return err instanceof Error ? err.message : fallback
}

function fmtDate(iso: string): string {
  if (!iso) return t('platformHttps.never')
  return new Date(iso).toLocaleString()
}

const statusMeta = computed(() => {
  if (!saved.value?.enabled) return { key: 'stateDisabled', cls: 'idle' }
  if (saved.value.status === 'active') return { key: 'stateActive', cls: 'ok' }
  if (saved.value.status === 'failed') return { key: 'stateFailed', cls: 'bad' }
  return { key: 'stateEnabledPending', cls: 'idle' }
})

const managedFilesHint = computed(() => t('platformHttps.managedFiles'))
</script>

<template>
  <div class="https-settings">
    <!-- 状态卡:当前生效情况 + 重新应用/禁用 -->
    <section v-if="saved" class="panel status-panel">
      <div class="status-head">
        <span class="status-chip" :class="`status-chip--${statusMeta.cls}`">
          <span class="status-dot" :class="`status-dot--${statusMeta.cls}`" aria-hidden="true" />
          {{ t(`platformHttps.${statusMeta.key}`) }}
        </span>
        <span v-if="saved.domain" class="status-url mono">https://{{ saved.domain }}</span>
        <div class="status-actions">
          <AppButton
            variant="default"
            :disabled="!saved.enabled || applying || saving"
            :loading="applying"
            @click="handleApply"
          >
            {{ t('platformHttps.applyBtn') }}
          </AppButton>
          <AppButton
            variant="danger"
            :disabled="!saved.enabled || disabling"
            :loading="disabling"
            @click="handleDisable"
          >
            {{ t('platformHttps.disableBtn') }}
          </AppButton>
        </div>
      </div>
      <dl class="status-meta">
        <div>
          <dt>{{ t('platformHttps.effectiveUpstreamLabel') }}</dt>
          <dd class="mono">{{ saved.effectiveUpstream }}</dd>
        </div>
        <div>
          <dt>{{ t('platformHttps.lastApplied') }}</dt>
          <dd>{{ fmtDate(saved.lastAppliedAt) }}</dd>
        </div>
      </dl>
      <p v-if="saved.enabled && saved.status === 'failed' && saved.statusDetail" class="status-error" role="alert">
        {{ saved.statusDetail }}
      </p>
      <p v-if="saved.enabled && saved.status === 'active' && !publicUrl" class="status-hint warn-hint">
        {{ t('platformHttps.publicUrlMissing') }}
      </p>
    </section>

    <!-- 外部访问地址(系统配置):通知审批链接 / PR 回写链接前缀,运行时改即时生效 -->
    <section class="panel">
      <div class="pub-head">
        <label class="field-label" for="https-public-url">{{ t('platformHttps.publicUrlLabel') }}</label>
        <button
          v-if="suggestUrl && publicUrl !== suggestUrl"
          type="button"
          class="suggest-btn"
          @click="publicUrl = suggestUrl"
        >
          {{ t('platformHttps.publicUrlSuggest', { url: suggestUrl }) }}
        </button>
      </div>
      <div class="pub-row">
        <input
          id="https-public-url"
          v-model="publicUrl"
          type="url"
          class="field-input field-input--mono"
          :placeholder="t('platformHttps.publicUrlPlaceholder')"
          autocomplete="off"
          spellcheck="false"
          :disabled="publicUrlSaving"
        />
        <AppButton variant="primary" :loading="publicUrlSaving" :disabled="publicUrlSaving" @click="handleSavePublicUrl">
          {{ t('platformHttps.publicUrlSave') }}
        </AppButton>
      </div>
      <span class="field-hint">{{ t('platformHttps.publicUrlHint') }}</span>
    </section>

    <!-- 配置表单 -->
    <section v-if="loadState === 'loading'" class="panel loading-panel">
      {{ t('platformHttps.loading') }}
    </section>
    <section v-else-if="loadState === 'error'" class="panel loading-panel error" role="alert">
      {{ loadError }}
      <AppButton variant="default" @click="load">{{ t('platformHttps.retry') }}</AppButton>
    </section>

    <form v-else class="panel config-panel" novalidate @submit.prevent="handleSave(true)">
      <!-- 服务器 + 探测 -->
      <div class="field">
        <label class="field-label" for="https-server">{{ t('platformHttps.serverLabel') }}</label>
        <div class="server-row">
          <AppSelect
            input-id="https-server"
            v-model="form.serverId"
            :options="serverOptions"
            :placeholder="t('platformHttps.serverPlaceholder')"
            min-width="260px"
            @update:model-value="onServerChange"
          />
          <AppButton variant="default" :disabled="!form.serverId" :loading="detecting" @click="runDetect">
            {{ detecting ? t('platformHttps.detecting') : t('platformHttps.detectBtn') }}
          </AppButton>
        </div>
        <span class="field-hint">{{ t('platformHttps.serverHint') }}</span>

        <div v-if="detect" class="detect-badges">
          <span class="badge" :class="detect.installed ? 'badge--ok' : 'badge--bad'">
            {{ detect.installed
              ? t('platformHttps.detectInstalled', { version: detect.version || '?' })
              : t('platformHttps.detectNotInstalled') }}
          </span>
          <span class="badge" :class="detect.isRoot || detect.sudoOk ? 'badge--ok' : 'badge--bad'">
            {{ detect.isRoot ? t('platformHttps.detectRoot') : detect.sudoOk ? t('platformHttps.detectSudo') : t('platformHttps.detectNoPriv') }}
          </span>
          <span class="badge" :class="detect.confDIncluded ? 'badge--ok' : 'badge--warn'">
            {{ detect.confDIncluded ? t('platformHttps.detectConfD') : t('platformHttps.detectConfDMissing') }}
          </span>
          <span v-if="detect.managedConf" class="badge badge--info">{{ t('platformHttps.detectManaged') }}</span>
        </div>
      </div>

      <!-- 域名 -->
      <div class="field">
        <label class="field-label" for="https-domain">{{ t('platformHttps.domainLabel') }}</label>
        <input
          id="https-domain"
          v-model="form.domain"
          type="text"
          class="field-input field-input--mono"
          :placeholder="t('platformHttps.domainPlaceholder')"
          autocomplete="off"
          spellcheck="false"
        />
      </div>

      <!-- 证书 -->
      <div class="field">
        <label class="field-label" for="https-cert">{{ t('platformHttps.certLabel') }}</label>
        <AppSelect
          input-id="https-cert"
          v-model="form.certId"
          :options="certOptions"
          :placeholder="t('platformHttps.certPlaceholder')"
          min-width="260px"
          :disabled="!form.domain"
        />
        <span v-if="form.domain && eligibleCerts.length === 0" class="field-hint warn">
          {{ t('platformHttps.certNone') }}
        </span>
        <span v-else-if="form.certId && certOf(form.certId)" class="field-hint">
          {{ t('platformHttps.certExpires', {
            date: fmtDate(certOf(form.certId)!.notAfter),
            sans: certOf(form.certId)!.domains.join(', '),
          }) }}
        </span>
      </div>

      <!-- 80→443 跳转 -->
      <div class="toggle-row">
        <button
          type="button"
          class="toggle-track"
          :class="{ 'toggle-track--on': form.httpRedirect }"
          role="switch"
          :aria-checked="form.httpRedirect"
          :aria-label="t('platformHttps.redirectTitle')"
          @click="form.httpRedirect = !form.httpRedirect"
        >
          <span class="toggle-thumb" />
        </button>
        <div class="toggle-label">
          <strong>{{ t('platformHttps.redirectTitle') }}</strong>
          <span>{{ t('platformHttps.redirectDesc') }}</span>
        </div>
      </div>

      <!-- 高级:反代上游 -->
      <div class="advanced">
        <button type="button" class="advanced-toggle" @click="showAdvanced = !showAdvanced">
          {{ t('platformHttps.advanced') }} {{ showAdvanced ? '▴' : '▾' }}
        </button>
        <template v-if="showAdvanced">
          <span class="field-hint">{{ t('platformHttps.advancedHint') }}</span>
          <div class="advanced-row">
            <div class="field">
              <label class="field-label" for="https-upstream-host">{{ t('platformHttps.upstreamHost') }}</label>
              <input
                id="https-upstream-host"
                v-model="form.upstreamHost"
                type="text"
                class="field-input field-input--mono"
                placeholder="127.0.0.1"
                autocomplete="off"
                spellcheck="false"
              />
            </div>
            <div class="field">
              <label class="field-label" for="https-upstream-port">{{ t('platformHttps.upstreamPort') }}</label>
              <input
                id="https-upstream-port"
                v-model.number="form.upstreamPort"
                type="number"
                min="1"
                max="65535"
                class="field-input field-input--mono"
              />
            </div>
          </div>
        </template>
      </div>

      <!-- 启用 + 保存 -->
      <div class="card-footer">
        <div class="toggle-row">
          <button
            type="button"
            class="toggle-track"
            :class="{ 'toggle-track--on': form.enabled }"
            role="switch"
            :aria-checked="form.enabled"
            :aria-label="t('platformHttps.enableTitle')"
            @click="form.enabled = !form.enabled"
          >
            <span class="toggle-thumb" />
          </button>
          <div class="toggle-label">
            <strong>{{ t('platformHttps.enableTitle') }}</strong>
            <span>{{ t('platformHttps.enableDesc') }}</span>
          </div>
        </div>

        <div class="card-actions">
          <AppButton variant="default" type="button" :disabled="saving" @click="handleSave(false)">
            {{ t('platformHttps.saveBtn') }}
          </AppButton>
          <AppButton variant="primary" type="submit" :loading="saving">
            {{ t('platformHttps.saveApplyBtn') }}
          </AppButton>
        </div>
      </div>

      <p class="managed-note">{{ managedFilesHint }}</p>
    </form>
  </div>
</template>

<style scoped>
.https-settings {
  display: flex;
  flex-direction: column;
  gap: 20px;
  max-width: 720px;
}

.panel {
  border: 1px solid var(--color-border);
  border-radius: var(--radius-card, 10px);
  background: var(--color-surface);
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

/* ── 状态卡 ── */
.status-head {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.status-url {
  font-weight: 600;
}

.status-actions {
  margin-left: auto;
  display: flex;
  gap: 8px;
}

.status-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 10px;
  border-radius: 999px;
  font-size: var(--text-label);
  border: 1px solid var(--color-border);
  color: var(--color-dim);
}

.status-chip--ok {
  color: var(--color-success, #16a34a);
  border-color: currentColor;
}

.status-chip--bad {
  color: var(--color-danger, #dc2626);
  border-color: currentColor;
}

.status-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--color-faint);
}

.status-dot--ok {
  background: var(--color-success, #16a34a);
}

.status-dot--bad {
  background: var(--color-danger, #dc2626);
}

.status-meta {
  display: flex;
  gap: 32px;
  margin: 0;
}

.status-meta dt {
  font-size: var(--text-label);
  color: var(--color-faint);
  margin-bottom: 2px;
}

.status-meta dd {
  margin: 0;
  font-size: var(--text-body);
}

.status-error {
  margin: 0;
  padding: 10px 12px;
  border-radius: 8px;
  background: var(--color-danger-soft, rgba(220, 38, 38, 0.08));
  color: var(--color-danger, #dc2626);
  font-size: var(--text-body);
  white-space: pre-wrap;
  word-break: break-all;
}

.status-hint {
  margin: 0;
  color: var(--color-faint);
  font-size: var(--text-label);
}

.warn-hint {
  color: var(--color-warning, #d97706);
}

/* ── 外部访问地址 ── */
.pub-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.suggest-btn {
  background: none;
  border: 1px dashed var(--color-primary);
  color: var(--color-primary);
  border-radius: 999px;
  padding: 2px 10px;
  font-size: var(--text-label);
  cursor: pointer;
}

.suggest-btn:hover {
  background: var(--color-primary);
  color: #fff;
}

.pub-row {
  display: flex;
  gap: 8px;
}

.pub-row .field-input {
  flex: 1;
}

/* ── 表单 ── */
.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.field-label {
  font-size: var(--text-label);
  font-weight: 600;
  color: var(--color-dim);
}

.field-input {
  height: 36px;
  padding: 0 10px;
  border: 1px solid var(--color-border);
  border-radius: 8px;
  background: var(--color-surface);
  color: var(--color-text);
  font-size: var(--text-body);
}

.field-input:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: -1px;
}

.mono {
  font-family: var(--font-mono, monospace);
}

.field-hint {
  font-size: var(--text-label);
  color: var(--color-faint);
}

.field-hint.warn {
  color: var(--color-warning, #d97706);
}

.server-row {
  display: flex;
  gap: 8px;
  align-items: center;
}

.detect-badges {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 4px;
}

.badge {
  padding: 2px 10px;
  border-radius: 999px;
  font-size: var(--text-label);
  border: 1px solid var(--color-border);
  color: var(--color-dim);
}

.badge--ok {
  color: var(--color-success, #16a34a);
  border-color: currentColor;
}

.badge--bad {
  color: var(--color-danger, #dc2626);
  border-color: currentColor;
}

.badge--warn {
  color: var(--color-warning, #d97706);
  border-color: currentColor;
}

.badge--info {
  color: var(--color-primary);
  border-color: currentColor;
}

/* ── 开关(照 settings 页惯例) ── */
.toggle-row {
  display: flex;
  align-items: flex-start;
  gap: 12px;
}

.toggle-track {
  position: relative;
  width: 40px;
  height: 22px;
  border-radius: 999px;
  border: 1px solid var(--color-border);
  background: var(--color-surface);
  cursor: pointer;
  flex-shrink: 0;
  transition: background var(--duration-fast);
}

.toggle-track--on {
  background: var(--color-primary);
  border-color: var(--color-primary);
}

.toggle-thumb {
  position: absolute;
  top: 2px;
  left: 2px;
  width: 16px;
  height: 16px;
  border-radius: 50%;
  background: #fff;
  transition: transform var(--duration-fast);
}

.toggle-track--on .toggle-thumb {
  transform: translateX(18px);
}

.toggle-label {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.toggle-label strong {
  font-size: var(--text-body);
}

.toggle-label span {
  font-size: var(--text-label);
  color: var(--color-faint);
}

/* ── 高级 ── */
.advanced {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.advanced-toggle {
  align-self: flex-start;
  background: none;
  border: none;
  color: var(--color-primary);
  font-size: var(--text-label);
  cursor: pointer;
  padding: 0;
}

.advanced-row {
  display: grid;
  grid-template-columns: 1fr 140px;
  gap: 12px;
}

/* ── 底部 ── */
.card-footer {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  border-top: 1px solid var(--color-border);
  padding-top: 16px;
}

.card-actions {
  display: flex;
  gap: 8px;
}

.managed-note {
  margin: 0;
  font-size: var(--text-label);
  color: var(--color-faint);
}

.loading-panel {
  color: var(--color-faint);
  align-items: flex-start;
  gap: 12px;
}

.loading-panel.error {
  color: var(--color-danger, #dc2626);
}
</style>
