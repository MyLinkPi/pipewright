<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { listAudit } from '../api/audit'
import type { AuditEntry } from '../api/audit'
import { HttpError } from '../api/http'

const { t } = useI18n()

// ─── state ──────────────────────────────────────────────────────────────────

type LoadState = 'idle' | 'loading' | 'error'

const PAGE_SIZE = 8

const loadState = ref<LoadState>('idle')
const loadError = ref('')
const entries = ref<AuditEntry[]>([])
const nextBefore = ref<string | null>(null)
const loadingMore = ref(false)

// ─── action → presentation mapping ───────────────────────────────────────────

type DotKind = 'add' | 'del' | 'use' | 'cfg'

interface ActionMeta {
  dot: DotKind
  verb: string
  // 名词可省略:省略时句子只含动词短语(如「在多台服务器执行命令 <cmd>」)。
  noun?: string
  // 开关型操作(启用/停用、绑定/解绑):按 detail 里该字段的布尔值在 verb/offVerb
  // 之间二选一(缺省视为开启)。
  toggle?: 'enabled' | 'attached'
  offVerb?: string
}

// Maps each audit action to a verb/noun i18n key + a dot category that drives
// semantic coloring (green=create, red=delete, primary=run/use, cyan=config
// change). verb/noun hold i18n keys (under misc.audit.*); meta() resolves them
// via t(). Must cover every action the backend writes (internal/audit + httpapi);
// unknown actions fall back to the raw action id.
const ACTION_META: Record<string, ActionMeta> = {
  // 凭据保险库
  credential_create: { dot: 'add', verb: 'verbCreate', noun: 'nounCredential' },
  credential_update: { dot: 'cfg', verb: 'verbUpdate', noun: 'nounCredential' },
  credential_delete: { dot: 'del', verb: 'verbDelete', noun: 'nounCredential' },
  credential_reveal: { dot: 'use', verb: 'verbReveal', noun: 'nounCredential' },
  trigger_secret_reset: { dot: 'cfg', verb: 'verbReset', noun: 'nounWebhookSecret' },
  // 项目 / 运行 / 环境
  project_create: { dot: 'add', verb: 'verbCreate', noun: 'nounProject' },
  project_update: { dot: 'cfg', verb: 'verbUpdate', noun: 'nounProject' },
  project_delete: { dot: 'del', verb: 'verbDelete', noun: 'nounProject' },
  'project.environments.save': { dot: 'cfg', verb: 'verbUpdate', noun: 'nounEnvironments' },
  run_trigger_manual: { dot: 'use', verb: 'verbTrigger', noun: 'nounRun' },
  'run.promote': { dot: 'use', verb: 'verbPromote' },
  'run.promote.reject': { dot: 'cfg', verb: 'verbReject' },
  'environment.rollback': { dot: 'cfg', verb: 'verbRollback', noun: 'nounEnvironment' },
  // 账号 / 会话
  password_change: { dot: 'cfg', verb: 'verbUpdate', noun: 'nounPassword' },
  session_revoke: { dot: 'del', verb: 'verbRevoke', noun: 'nounSession' },
  // 流水线模板 / 变量组 / 自定义节点
  template_create: { dot: 'add', verb: 'verbCreate', noun: 'nounTemplate' },
  template_delete: { dot: 'del', verb: 'verbDelete', noun: 'nounTemplate' },
  template_apply: { dot: 'use', verb: 'verbApply', noun: 'nounTemplate' },
  variable_group_create: { dot: 'add', verb: 'verbCreate', noun: 'nounVarGroup' },
  variable_group_update: { dot: 'cfg', verb: 'verbUpdate', noun: 'nounVarGroup' },
  variable_group_delete: { dot: 'del', verb: 'verbDelete', noun: 'nounVarGroup' },
  custom_node_create: { dot: 'add', verb: 'verbCreate', noun: 'nounCustomNode' },
  custom_node_update: { dot: 'cfg', verb: 'verbUpdate', noun: 'nounCustomNode' },
  custom_node_delete: { dot: 'del', verb: 'verbDelete', noun: 'nounCustomNode' },
  // 服务器运维
  service_op: { dot: 'cfg', verb: 'verbDefault', noun: 'nounService' },
  container_create: { dot: 'add', verb: 'verbCreate', noun: 'nounContainer' },
  container_terminal: { dot: 'use', verb: 'verbTerminal', noun: 'nounContainerTerminal' },
  server_terminal: { dot: 'use', verb: 'verbTerminal', noun: 'nounServerTerminal' },
  server_command: { dot: 'use', verb: 'verbCommand' },
  image_op: { dot: 'cfg', verb: 'verbDefault', noun: 'nounImage' },
  stack_op: { dot: 'cfg', verb: 'verbDefault', noun: 'nounStack' },
  volume_op: { dot: 'cfg', verb: 'verbDefault', noun: 'nounVolume' },
  network_op: { dot: 'cfg', verb: 'verbDefault', noun: 'nounNetwork' },
  system_prune: { dot: 'del', verb: 'verbPrune', noun: 'nounDocker' },
  // 应用商店
  'appstore.deploy': { dot: 'use', verb: 'verbDeploy', noun: 'nounApp' },
  'appstore.template.create': { dot: 'add', verb: 'verbCreate', noun: 'nounAppTemplate' },
  'appstore.template.update': { dot: 'cfg', verb: 'verbUpdate', noun: 'nounAppTemplate' },
  'appstore.template.delete': { dot: 'del', verb: 'verbDelete', noun: 'nounAppTemplate' },
  // 证书管理
  'certmgmt.cert.create': { dot: 'add', verb: 'verbCreate', noun: 'nounCert' },
  'certmgmt.cert.import': { dot: 'add', verb: 'verbImport', noun: 'nounCert' },
  'certmgmt.cert.renew': { dot: 'use', verb: 'verbRenew', noun: 'nounCert' },
  'certmgmt.cert.delete': { dot: 'del', verb: 'verbDelete', noun: 'nounCert' },
  'certmgmt.cert.autorenew': { dot: 'cfg', verb: 'verbEnable', offVerb: 'verbDisable', toggle: 'enabled', noun: 'nounAutoRenew' },
  'certmgmt.engine.deploy': { dot: 'cfg', verb: 'verbDeploy', noun: 'nounCertEngine' },
  // DNS 提供商
  'dns.provider.create': { dot: 'add', verb: 'verbAdd', noun: 'nounDNSProvider' },
  'dns.provider.update': { dot: 'cfg', verb: 'verbUpdate', noun: 'nounDNSProvider' },
  'dns.provider.delete': { dot: 'del', verb: 'verbDelete', noun: 'nounDNSProvider' },
  'dns.provider.verify': { dot: 'use', verb: 'verbVerify', noun: 'nounDNSProvider' },
  'dns.provider.zone.add': { dot: 'add', verb: 'verbCreate', noun: 'nounDNSZone' },
  'dns.provider.zone.remove': { dot: 'del', verb: 'verbDelete', noun: 'nounDNSZone' },
  // 平台 HTTPS
  'platformhttps.settings.save': { dot: 'cfg', verb: 'verbUpdate', noun: 'nounPlatformHTTPS' },
  'platformhttps.apply': { dot: 'use', verb: 'verbApply', noun: 'nounPlatformHTTPS' },
  'platformhttps.disable': { dot: 'del', verb: 'verbDisable', noun: 'nounPlatformHTTPS' },
  // PR 预览环境
  'preview.config.update': { dot: 'cfg', verb: 'verbEnable', offVerb: 'verbDisable', toggle: 'enabled', noun: 'nounPreviewEnv' },
  'preview.env.reclaim': { dot: 'del', verb: 'verbReclaim', noun: 'nounPreviewEnv' },
  // 服务注册
  'servicereg.apply': { dot: 'use', verb: 'verbApply', noun: 'nounServiceReg' },
  'servicereg.domain.create': { dot: 'add', verb: 'verbCreate', noun: 'nounSRBaseDomain' },
  'servicereg.domain.delete': { dot: 'del', verb: 'verbDelete', noun: 'nounSRBaseDomain' },
  'servicereg.gateway.deploy': { dot: 'use', verb: 'verbDeploy', noun: 'nounSRGateway' },
  'servicereg.gateway.remove': { dot: 'del', verb: 'verbDelete', noun: 'nounSRGateway' },
  'servicereg.instance.add': { dot: 'add', verb: 'verbCreate', noun: 'nounSRInstance' },
  'servicereg.instance.remove': { dot: 'del', verb: 'verbDelete', noun: 'nounSRInstance' },
  'servicereg.instance.attach': { dot: 'cfg', verb: 'verbAttach', offVerb: 'verbDetach', toggle: 'attached', noun: 'nounSRInstance' },
  'servicereg.service.create': { dot: 'add', verb: 'verbCreate', noun: 'nounSRService' },
  'servicereg.service.update': { dot: 'cfg', verb: 'verbUpdate', noun: 'nounSRService' },
  'servicereg.service.delete': { dot: 'del', verb: 'verbDelete', noun: 'nounSRService' },
  'servicereg.service.enable': { dot: 'cfg', verb: 'verbEnable', offVerb: 'verbDisable', toggle: 'enabled', noun: 'nounSRService' },
  'servicereg.settings.update': { dot: 'cfg', verb: 'verbUpdate', noun: 'nounServiceReg' },
  // 本地镜像仓库
  registry_deploy: { dot: 'use', verb: 'verbDeploy', noun: 'nounRegistry' },
  registry_daemon_apply: { dot: 'cfg', verb: 'verbApply', noun: 'nounRegistryConfig' },
  registry_prune: { dot: 'del', verb: 'verbPrune', noun: 'nounRegistry' },
  // 系统设置
  'systemconfig.set': { dot: 'cfg', verb: 'verbUpdate', noun: 'nounSystemConfig' },
}

// CJK(Han/kana/Hangul)判断,用于决定动词与名词之间是否补空格。
function isCJK(ch: string): boolean {
  const c = ch.codePointAt(0) ?? 0
  return (c >= 0x3040 && c <= 0x30ff) || (c >= 0x3400 && c <= 0x4dbf) ||
    (c >= 0x4e00 && c <= 0x9fff) || (c >= 0xf900 && c <= 0xfaff) ||
    (c >= 0xac00 && c <= 0xd7af)
}

// 中中相接不加空格(新增凭据),中西/西西相接补空格(Created credential / 启用 PR 预览环境)。
function joinPhrase(verb: string, noun: string): string {
  if (!verb || !noun) return verb + noun
  const a = verb[verb.length - 1]!
  const b = noun[0]!
  return isCJK(a) && isCJK(b) ? verb + noun : `${verb} ${noun}`
}

// Returns the dot category plus localized verb/noun for an action. Unknown
// actions fall back to a generic verb and surface the raw action id as the noun.
function meta(e: AuditEntry): { dot: DotKind; label: string } {
  const d = e.detail ?? {}
  const m = ACTION_META[e.action]
  if (!m) return { dot: 'cfg', label: joinPhrase(t('misc.audit.verbDefault'), e.action) }
  const on = m.toggle ? d[m.toggle] !== false : true
  const verb = t(`misc.audit.${on ? m.verb : m.offVerb ?? m.verb}`)
  const noun = m.noun ? t(`misc.audit.${m.noun}`) : ''
  return { dot: m.dot, label: joinPhrase(verb, noun) }
}

// 目标伪 ID:后端对「无单一目标对象」的操作填的占位符,不值得展示。
const PSEUDO_TARGETS = new Set(['-', 'admin', 'settings', 'gateway', 'daemon', 'local', 'default', 'stack', 'prune'])

// Best-effort human label for the affected object: prefer readable detail
// fields (name / domain / environment / image / …), else the target id.
// detail is already masked server-side.
const OBJ_DETAIL_KEYS = [
  'name', 'primaryDomain', 'baseDomain', 'branch', 'targetEnvironment', 'environment',
  'template', 'image', 'container', 'containerId', 'network', 'publicUrl', 'target', 'scope', 'command',
] as const

function objectLabel(e: AuditEntry): string {
  const d = e.detail ?? {}
  for (const k of OBJ_DETAIL_KEYS) {
    const v = d[k]
    if (typeof v === 'string' && v) return v
  }
  // 环境链保存:chain 是环境名数组,用箭头串起展示。
  if (Array.isArray(d['chain'])) {
    const names = (d['chain'] as unknown[]).filter((x): x is string => typeof x === 'string' && !!x)
    if (names.length) return names.join(' → ')
  }
  if (!e.targetId || PSEUDO_TARGETS.has(e.targetId)) return ''
  return e.targetId
}

function actorLabel(actor: string): string {
  return actor === 'admin' ? t('misc.audit.actorYou') : actor
}

// ─── rendered rows ───────────────────────────────────────────────────────────

interface Row {
  id: string
  dot: DotKind
  actor: string
  label: string
  obj: string
  ip: string
  timestamp: string
}

const rows = computed<Row[]>(() =>
  entries.value.map((e) => {
    const m = meta(e)
    return {
      id: e.id,
      dot: m.dot,
      actor: actorLabel(e.actor),
      label: m.label,
      obj: objectLabel(e),
      ip: e.ip,
      timestamp: e.timestamp,
    }
  }),
)

// ─── relative time ────────────────────────────────────────────────────────────

function relativeTime(isoStr: string): string {
  const diff = Date.now() - new Date(isoStr).getTime()
  const s = Math.floor(diff / 1000)
  if (s < 60) return t('time.justNow')
  const m = Math.floor(s / 60)
  if (m < 60) return t('time.minAgo', { n: m })
  const h = Math.floor(m / 60)
  if (h < 24) return t('time.hourAgo', { n: h })
  const day = Math.floor(h / 24)
  if (day < 30) return t('time.dayAgo', { n: day })
  return new Date(isoStr).toLocaleDateString()
}

// ─── data loading ─────────────────────────────────────────────────────────────

function describeError(err: unknown): string {
  if (err instanceof HttpError) {
    if (err.status === 0) return t('misc.audit.errConnect')
    return err.apiError?.message ?? t('misc.audit.errLoad', { status: err.status })
  }
  return t('misc.audit.errLoadRetry')
}

async function loadFirst(): Promise<void> {
  loadState.value = 'loading'
  loadError.value = ''
  try {
    const res = await listAudit({ limit: PAGE_SIZE })
    entries.value = res.entries
    nextBefore.value = res.nextBefore
    loadState.value = 'idle'
  } catch (err) {
    loadError.value = describeError(err)
    loadState.value = 'error'
  }
}

async function loadMore(): Promise<void> {
  if (!nextBefore.value || loadingMore.value) return
  loadingMore.value = true
  try {
    const res = await listAudit({ limit: PAGE_SIZE, before: nextBefore.value })
    entries.value = [...entries.value, ...res.entries]
    nextBefore.value = res.nextBefore
  } catch (err) {
    loadError.value = describeError(err)
  } finally {
    loadingMore.value = false
  }
}

onMounted(loadFirst)
</script>

<template>
  <div class="panel audit-panel">
    <div class="panel-head">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" aria-hidden="true">
        <path d="M12 8v4l3 2" /><circle cx="12" cy="12" r="9" />
      </svg>
      {{ t('misc.audit.title') }}
      <span class="panel-sub">{{ t('misc.audit.sub') }}</span>
    </div>

    <!-- Error -->
    <div v-if="loadState === 'error' && entries.length === 0" class="audit-error" role="alert">
      <span>{{ loadError }}</span>
      <button class="audit-retry" @click="loadFirst">↻ {{ t('misc.error.retry') }}</button>
    </div>

    <!-- Loading skeleton -->
    <template v-else-if="loadState === 'loading'">
      <div class="aev aev--skel" v-for="i in 3" :key="i" aria-hidden="true">
        <span class="skel adot-skel" />
        <span class="skel line-skel" />
        <span class="skel time-skel" />
      </div>
    </template>

    <!-- Empty -->
    <template v-else-if="loadState === 'idle' && entries.length === 0">
      <div class="audit-empty">
        <p class="audit-empty-label">{{ t('misc.audit.emptyLabel') }}</p>
        <p class="audit-empty-hint">{{ t('misc.audit.emptyHint') }}</p>
      </div>
    </template>

    <!-- Timeline -->
    <template v-else>
      <ol class="audit" :aria-label="t('misc.audit.treeAria')">
        <li v-for="r in rows" :key="r.id" class="aev">
          <span class="adot" :class="`adot--${r.dot}`" aria-hidden="true">
            <!-- add -->
            <svg v-if="r.dot === 'add'" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
            <!-- del -->
            <svg v-else-if="r.dot === 'del'" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M5 12h14" /></svg>
            <!-- use (run) -->
            <svg v-else-if="r.dot === 'use'" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"><path d="M5 3v18M5 7h10l-2 3 2 3H5" /></svg>
            <!-- cfg -->
            <svg v-else viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"><circle cx="12" cy="12" r="3" /><path d="M12 2v3M12 19v3M2 12h3M19 12h3" /></svg>
          </span>

          <div class="atx">
            <span class="atx-line">
              <b>{{ r.actor }}</b>
              {{ r.label }}
              <span v-if="r.obj" class="obj">{{ r.obj }}</span>
            </span>
            <span class="who">
              {{ r.actor }} · {{ t('misc.audit.via') }}<template v-if="r.ip"> · <span class="ip">{{ r.ip }}</span></template>
            </span>
          </div>

          <time class="atm" :datetime="r.timestamp" :title="r.timestamp">{{ relativeTime(r.timestamp) }}</time>
        </li>
      </ol>

      <button
        v-if="nextBefore"
        class="viewall"
        :disabled="loadingMore"
        @click="loadMore"
      >
        <span v-if="loadingMore" class="spinner" aria-hidden="true" />
        {{ loadingMore ? t('misc.loadingShort') : t('misc.audit.loadMore') }}
      </button>
    </template>
  </div>
</template>

<style scoped>
.audit-panel {
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  box-shadow: var(--shadow);
  overflow: hidden;
  animation: panel-in 0.45s var(--ease-out-expo) both;
}

@keyframes panel-in {
  from { opacity: 0; transform: translateY(13px); }
  to   { opacity: 1; transform: none; }
}

.panel-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 13px 18px;
  border-bottom: 1px solid var(--color-border);
  font-size: 0.86rem;
  font-weight: 600;
  color: var(--color-text);
}

.panel-sub {
  margin-left: auto;
  font-size: 0.74rem;
  font-weight: 400;
  color: var(--color-faint);
}

/* ─── timeline ──────────────────────────────────────────────────────────────── */
.audit {
  list-style: none;
  margin: 0;
  padding: 6px 0;
}

.aev {
  display: grid;
  grid-template-columns: 34px 1fr auto;
  gap: 13px;
  padding: 11px 18px;
  align-items: start;
  position: relative;
}

/* Connecting spine between dots. */
.aev::before {
  content: '';
  position: absolute;
  left: 33px;
  top: 30px;
  bottom: -11px;
  width: 1.5px;
  background: var(--color-border);
}

.aev:last-child::before {
  display: none;
}

.adot {
  width: 34px;
  height: 34px;
  border-radius: var(--rounded-lg);
  display: grid;
  place-items: center;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  z-index: 1;
  color: var(--color-dim);
}

.adot svg {
  width: 15px;
  height: 15px;
}

.adot--add {
  background: var(--color-green-soft);
  border-color: transparent;
  color: var(--color-green);
}

.adot--del {
  background: var(--color-red-soft);
  border-color: transparent;
  color: var(--color-red);
}

.adot--use {
  background: var(--color-primary-soft);
  border-color: transparent;
  color: var(--color-primary);
}

.adot--cfg {
  background: var(--color-cyan-soft);
  border-color: transparent;
  color: var(--color-cyan);
}

.atx {
  font-size: 0.83rem;
  line-height: 1.5;
  min-width: 0;
}

.atx-line b {
  font-weight: 600;
  color: var(--color-text);
}

.obj {
  font-family: var(--font-mono);
  color: var(--color-dim);
  font-size: 0.92em;
  word-break: break-all;
}

.who {
  display: block;
  color: var(--color-faint);
  font-size: 0.75rem;
  margin-top: 2px;
}

.who .ip {
  font-family: var(--font-mono);
}

.atm {
  font-size: 0.74rem;
  color: var(--color-faint);
  white-space: nowrap;
  font-family: var(--font-mono);
}

/* ─── load more ─────────────────────────────────────────────────────────────── */
.viewall {
  width: 100%;
  padding: 13px 18px;
  text-align: center;
  font-size: 0.79rem;
  color: var(--color-primary);
  font-weight: 500;
  cursor: pointer;
  border: none;
  border-top: 1px solid var(--color-border);
  background: transparent;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 7px;
  transition: background-color var(--duration-fast);
}

.viewall:hover:not(:disabled) {
  background: var(--color-inset);
}

.viewall:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: -2px;
}

.viewall:disabled {
  opacity: 0.6;
  cursor: progress;
}

/* ─── empty / error ─────────────────────────────────────────────────────────── */
.audit-empty {
  padding: 36px 32px;
  text-align: center;
}

.audit-empty-label {
  font-size: 0.86rem;
  font-weight: 600;
  color: var(--color-dim);
}

.audit-empty-hint {
  font-size: 0.78rem;
  color: var(--color-faint);
  max-width: 52ch;
  margin: 6px auto 0;
  line-height: 1.55;
}

.audit-error {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 16px 18px;
  font-size: 0.82rem;
  color: var(--color-red);
}

.audit-retry {
  margin-left: auto;
  background: none;
  border: none;
  color: var(--color-red);
  font-weight: 600;
  cursor: pointer;
  text-decoration: underline;
  text-underline-offset: 2px;
}

/* ─── skeleton ──────────────────────────────────────────────────────────────── */
.aev--skel {
  align-items: center;
}

.skel {
  display: block;
  background: linear-gradient(
    90deg,
    var(--color-inset) 0%,
    oklch(100% 0 0 / 0.06) 50%,
    var(--color-inset) 100%
  );
  background-size: 200% 100%;
  border-radius: var(--rounded-md);
  animation: shimmer 1.4s ease-in-out infinite;
}

@keyframes shimmer {
  0%   { background-position: 200% center; }
  100% { background-position: -200% center; }
}

.adot-skel { width: 34px; height: 34px; border-radius: var(--rounded-lg); }
.line-skel { height: 13px; width: 72%; }
.time-skel { height: 11px; width: 48px; }

/* ─── spinner ───────────────────────────────────────────────────────────────── */
.spinner {
  display: inline-block;
  width: 12px;
  height: 12px;
  border: 2px solid var(--color-primary-soft);
  border-top-color: var(--color-primary);
  border-radius: var(--rounded-full);
  animation: spin 0.7s linear infinite;
}

@keyframes spin {
  to { transform: rotate(360deg); }
}

@media (prefers-reduced-motion: reduce) {
  *,
  *::before,
  *::after {
    animation-duration: 0.01ms !important;
    animation-iteration-count: 1 !important;
    transition-duration: 0.01ms !important;
  }
}
</style>
