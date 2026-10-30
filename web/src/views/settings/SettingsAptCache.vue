<script setup lang="ts">
/**
 * SettingsAptCache — 内置 apt 包缓存(apt-cacher-ng)。
 *
 * 区块:
 *  1. 基础配置:启用开关 / 外部地址(带探测建议值)/ 端口 / 缓存存储目录(改目录→部署时
 *     清空旧缓存)/ TLS 证书(选证书 → nginx stream 前端 TLS 终止,代理片段变 https://)。
 *     保存 = PUT(只写库,**不触碰任何机器**)。
 *  2. 部署与状态:显式「部署/更新」按钮(compose up -d);栈状态卡(容器/探活/缓存占用);
 *     「清空缓存」停容器删目录后重建(缓存可再生)。
 *  3. apt 代理下发:勾选已登记服务器(或控制机本机)→ 危险确认(覆盖代理片段文件,
 *     自动备份)→ 逐台下发结果。**只作用于被勾选的机器**;未勾选的机器永不被触碰。
 */
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useConfirm } from '../../composables/useConfirm'
import {
  getAptCacheConfig,
  saveAptCacheConfig,
  deployAptCacheStack,
  upgradeAptCacheImage,
  getAptCacheStatus,
  clearAptCache,
  applyAptProxy,
  inspectAptProxy,
  type AptCacheConfig,
  type AptCacheStatus,
  type AptProxyApplyResult,
  type AptProxyInspect,
} from '../../api/aptCache'
import { listServers, type Server } from '../../api/servers'
import { listCerts, type Cert } from '../../api/certMgmt'
import { HttpError } from '../../api/http'
import AppButton from '../../components/ui/AppButton.vue'

const { t } = useI18n()
const confirm = useConfirm()

// ─── 区块 1:配置 ─────────────────────────────────────────────────────────────
const cfg = ref<AptCacheConfig | null>(null)
const loading = ref(true)
const saving = ref(false)
const savedFlash = ref(false)
const saveError = ref('')

async function loadConfig(): Promise<void> {
  loading.value = true
  try {
    cfg.value = await getAptCacheConfig()
  } catch {
    saveError.value = t('settingsAptCache.errLoad')
  } finally {
    loading.value = false
  }
}

/** 采用后端探测的出网 IP 作为外部地址(UI 预填建议)。 */
function useSuggestedAddr(): void {
  if (cfg.value && cfg.value.suggestedAddr) cfg.value.externalAddr = cfg.value.suggestedAddr
}

function toggleEnabled(): void {
  if (cfg.value) cfg.value.enabled = !cfg.value.enabled
}

/** 部署相关字段的生效值快照(存储目录用服务端解析后的生效值,避免默认值物化造成假 diff)。 */
function deployRelevantSnapshot(c: AptCacheConfig): string {
  return [c.port, c.effectiveDataDir, c.tlsCertId].join('\u0000')
}

// ─── TLS 证书(证书管理已签发的公共可信证书;空 = 明文 HTTP) ──────────────────
const certs = ref<Cert[]>([])
const certsError = ref(false)

/** 只列已签发(pending/failed 无可用 PEM,选了也部署不了)。 */
const issuedCerts = computed(() => certs.value.filter((c) => c.status === 'issued'))

function certLabel(c: Cert): string {
  const until = c.notAfter ? ` · ${c.notAfter.slice(0, 10)}` : ''
  return `${c.primaryDomain}${until}`
}

const proxyScheme = computed(() => (cfg.value?.tlsCertId ? 'https' : 'http'))

async function loadCerts(): Promise<void> {
  try {
    certs.value = (await listCerts()).items
    certsError.value = false
  } catch {
    certsError.value = true
  }
}

async function saveConfig(): Promise<void> {
  if (!cfg.value) return
  saving.value = true
  savedFlash.value = false
  saveError.value = ''
  const before = deployRelevantSnapshot(cfg.value)
  try {
    cfg.value = await saveAptCacheConfig({
      enabled: cfg.value.enabled,
      externalAddr: cfg.value.externalAddr.trim(),
      port: Math.floor(Number(cfg.value.port) || 0),
      dataDir: cfg.value.dataDir.trim(),
      tlsCertId: cfg.value.tlsCertId.trim(),
    })
    savedFlash.value = true
    window.setTimeout(() => { savedFlash.value = false }, 2000)
    void loadStatus()
    // 部署相关配置(端口/目录/TLS 证书)有变更且已启用 → 询问是否立即重新部署
    // (保存本身仍只写库,不悄悄重建容器;但也不能让用户漏掉生效这一步)。
    if (cfg.value.enabled && deployRelevantSnapshot(cfg.value) !== before) {
      const ok = await confirm.open({
        title: t('settingsAptCache.redeployTitle'),
        body: t('settingsAptCache.redeployBody'),
        confirmLabel: t('settingsAptCache.redeployConfirm'),
        variant: 'primary',
      })
      if (ok) await deploy()
    }
  } catch (err) {
    saveError.value = err instanceof HttpError ? (err.apiError?.message ?? t('settingsAptCache.errSave')) : t('settingsAptCache.errSave')
  } finally {
    saving.value = false
  }
}

// ─── 区块 2:部署 / 状态 / 清空缓存 ──────────────────────────────────────────
const status = ref<AptCacheStatus | null>(null)
const statusLoading = ref(false)
const deploying = ref(false)
const deployError = ref('')
const deployOutput = ref('')
const clearing = ref(false)
const clearMsg = ref('')
const upgrading = ref(false)
const upgradeMsg = ref('')
const upgradeError = ref(false)

/** 升级栈镜像(apt-cacher-ng;TLS 态含 nginx 前端):pull → 变了才重建容器(缓存在宿主卷,不丢)。 */
async function upgradeImage(): Promise<void> {
  upgrading.value = true
  upgradeMsg.value = ''
  upgradeError.value = false
  try {
    const res = await upgradeAptCacheImage()
    if (res.ok && res.changed) {
      upgradeMsg.value = t('settingsAptCache.upgradeImageDone', { before: res.before || '—', after: res.after || '—' })
    } else if (res.ok) {
      upgradeMsg.value = t('settingsAptCache.upgradeImageLatest', { image: res.image })
    } else {
      upgradeMsg.value = res.error || t('settingsAptCache.upgradeImageFailed')
      upgradeError.value = true
    }
    void loadStatus()
  } catch (err) {
    upgradeMsg.value = err instanceof HttpError ? (err.apiError?.message ?? t('settingsAptCache.upgradeImageFailed')) : t('settingsAptCache.upgradeImageFailed')
    upgradeError.value = true
  } finally {
    upgrading.value = false
  }
}

async function loadStatus(): Promise<void> {
  statusLoading.value = true
  try {
    status.value = await getAptCacheStatus()
  } catch {
    status.value = null
  } finally {
    statusLoading.value = false
  }
}

async function deploy(): Promise<void> {
  deploying.value = true
  deployError.value = ''
  deployOutput.value = ''
  try {
    const res = await deployAptCacheStack()
    if (res.ok) deployOutput.value = res.output
    else deployError.value = res.error || t('settingsAptCache.deployFailed')
    void loadStatus()
  } catch (err) {
    deployError.value = err instanceof HttpError ? (err.apiError?.message ?? t('settingsAptCache.deployFailed')) : t('settingsAptCache.deployFailed')
  } finally {
    deploying.value = false
  }
}

async function clearCache(): Promise<void> {
  const ok = await confirm.open({
    title: t('settingsAptCache.clearConfirmTitle'),
    body: t('settingsAptCache.clearConfirmBody'),
    confirmLabel: t('settingsAptCache.clear'),
    variant: 'danger',
  })
  if (!ok) return
  clearing.value = true
  clearMsg.value = ''
  try {
    const res = await clearAptCache()
    clearMsg.value = res.ok ? t('settingsAptCache.clearDone') : (res.error || t('settingsAptCache.clearFailed'))
    void loadStatus()
  } catch (err) {
    clearMsg.value = err instanceof HttpError ? (err.apiError?.message ?? t('settingsAptCache.clearFailed')) : t('settingsAptCache.clearFailed')
  } finally {
    clearing.value = false
  }
}

function fmtBytes(n: number): string {
  if (!n) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++ }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`
}

// ─── 区块 3:apt 代理片段下发(仅显式勾选目标) ───────────────────────────────
const servers = ref<Server[]>([])
const serversError = ref('')
const checked = ref<Set<string>>(new Set())
const includeLocal = ref(false)
const applying = ref(false)
const applyResults = ref<AptProxyApplyResult[]>([])
const inspecting = ref<string>('')
const inspect = ref<Record<string, AptProxyInspect>>({})

const checkedCount = computed(() => checked.value.size + (includeLocal.value ? 1 : 0))
const canApply = computed(() => !applying.value && checkedCount.value > 0 && !!cfg.value?.enabled && !!cfg.value.externalAddr)

function toggleServer(id: string): void {
  const next = new Set(checked.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  checked.value = next
}

async function loadServers(): Promise<void> {
  try {
    servers.value = await listServers()
  } catch {
    serversError.value = t('settingsAptCache.errServers')
  }
}

function inspectText(ins: AptProxyInspect): string {
  if (ins.error) return ins.error
  if (ins.proxyHttp || ins.proxyHttps) {
    return `http: ${ins.proxyHttp || '—'} · https: ${ins.proxyHttps || '—'}`
  }
  return t('settingsAptCache.noProxy')
}

async function runInspect(id: string): Promise<void> {
  inspecting.value = id
  try {
    inspect.value[id] = await inspectAptProxy(id)
  } catch {
    inspect.value[id] = { targetId: id, targetName: id, isLocal: id === 'local', proxyHttp: '', proxyHttps: '', confFile: '', error: t('settingsAptCache.inspectFailed') }
  } finally {
    inspecting.value = ''
  }
}

async function apply(): Promise<void> {
  if (!cfg.value) return
  const names = [
    ...(includeLocal.value ? [t('settingsAptCache.localMachine')] : []),
    ...servers.value.filter((s) => checked.value.has(s.id)).map((s) => s.name),
  ]
  const ok = await confirm.open({
    title: t('settingsAptCache.applyConfirmTitle'),
    body: t('settingsAptCache.applyConfirmBody', { targets: names.join('、'), proxy: `${proxyScheme.value}://${cfg.value.addr}` }),
    confirmLabel: t('settingsAptCache.applyConfirmLabel'),
    variant: 'danger',
  })
  if (!ok) return
  applying.value = true
  applyResults.value = []
  try {
    const res = await applyAptProxy([...checked.value], includeLocal.value)
    applyResults.value = res.results
  } catch (err) {
    applyResults.value = [{
      targetId: '', targetName: '', isLocal: false, ok: false, backupPath: '',
      error: err instanceof HttpError ? (err.apiError?.message ?? t('settingsAptCache.applyFailed')) : t('settingsAptCache.applyFailed'),
      durationMs: 0,
    }]
  } finally {
    applying.value = false
  }
}

onMounted(() => {
  void loadConfig()
  void loadStatus()
  void loadServers()
  void loadCerts()
})
</script>

<template>
  <section class="ac" aria-labelledby="ac-heading">
    <header class="ac-head">
      <h2 id="ac-heading" class="ac-title">{{ t('settingsAptCache.title') }}</h2>
      <p class="ac-sub">{{ t('settingsAptCache.subtitle') }}</p>
    </header>

    <!-- 区块 1:基础配置(保存只写库,不触碰机器) -->
    <article class="ac-panel">
      <span class="ac-accent" aria-hidden="true" />
      <div class="ac-head-row">
        <div class="ac-head-text">
          <h3 class="ac-h3">{{ t('settingsAptCache.cfgTitle') }}</h3>
          <p class="ac-hint">{{ t('settingsAptCache.cfgHint') }}</p>
        </div>
        <button
          type="button"
          class="ac-switch"
          :class="{ 'ac-switch--on': cfg?.enabled }"
          role="switch"
          :aria-checked="!!cfg?.enabled"
          :aria-label="t('settingsAptCache.cfgTitle')"
          :disabled="loading"
          @click="toggleEnabled"
        >
          <span class="ac-switch-knob" aria-hidden="true" />
        </button>
      </div>

      <div v-if="cfg" class="ac-grid" :class="{ 'ac-grid--off': !cfg.enabled }">
        <label class="ac-field ac-field--wide">
          <span class="ac-label">{{ t('settingsAptCache.externalAddr') }}</span>
          <span class="ac-inline">
            <input class="ac-input mono" v-model="cfg.externalAddr" :disabled="loading" :placeholder="cfg.suggestedAddr || '192.168.1.10'" />
            <button
              v-if="cfg.suggestedAddr && cfg.suggestedAddr !== cfg.externalAddr"
              type="button"
              class="ac-mini"
              @click="useSuggestedAddr"
            >{{ t('settingsAptCache.useSuggested') }}</button>
          </span>
          <span class="ac-hint">{{ t('settingsAptCache.externalAddrHint') }}</span>
        </label>

        <label class="ac-field">
          <span class="ac-label">{{ t('settingsAptCache.port') }}</span>
          <input class="ac-input mono" type="number" min="1" max="65535" v-model.number="cfg.port" :disabled="loading" />
          <span class="ac-hint">{{ t('settingsAptCache.portHint', { addr: cfg.addr || '…' }) }}</span>
        </label>

        <label class="ac-field">
          <span class="ac-label">{{ t('settingsAptCache.dataDir') }}</span>
          <input class="ac-input mono" v-model="cfg.dataDir" :disabled="loading" :placeholder="cfg.effectiveDataDir" spellcheck="false" />
          <span class="ac-hint">{{ t('settingsAptCache.dataDirHint') }}</span>
        </label>

        <label class="ac-field">
          <span class="ac-label">{{ t('settingsAptCache.tlsCert') }}</span>
          <select class="ac-input" v-model="cfg.tlsCertId" :disabled="loading">
            <option value="">{{ t('settingsAptCache.tlsCertOff') }}</option>
            <option v-for="c in issuedCerts" :key="c.id" :value="c.id">{{ certLabel(c) }}</option>
          </select>
          <span class="ac-hint">
            {{ issuedCerts.length ? t('settingsAptCache.tlsCertHint') : t('settingsAptCache.tlsCertNone') }}
            <span v-if="certsError">{{ t('settingsAptCache.tlsCertErr') }}</span>
          </span>
        </label>
      </div>

      <div class="ac-actions">
        <span v-if="savedFlash" class="ac-ok" role="status">{{ t('settingsAptCache.saved') }}</span>
        <span v-if="saveError" class="ac-err" role="alert">{{ saveError }}</span>
        <AppButton variant="primary" :loading="saving" :disabled="loading" @click="saveConfig">
          {{ t('settingsAptCache.save') }}
        </AppButton>
      </div>
    </article>

    <!-- 区块 2:部署与状态 -->
    <article class="ac-panel">
      <span class="ac-accent" aria-hidden="true" />
      <div class="ac-head-row">
        <div class="ac-head-text">
          <h3 class="ac-h3">{{ t('settingsAptCache.stackTitle') }}</h3>
          <p class="ac-hint">{{ t('settingsAptCache.stackHint') }}</p>
        </div>
        <div class="ac-head-btns">
          <AppButton variant="primary" :loading="deploying" :disabled="!cfg?.enabled" @click="deploy">
            {{ t('settingsAptCache.deploy') }}
          </AppButton>
          <AppButton :loading="upgrading" :disabled="!cfg?.enabled || !status?.deployed" @click="upgradeImage">
            {{ t('settingsAptCache.upgradeImage') }}
          </AppButton>
          <AppButton :loading="clearing" :disabled="!cfg?.enabled" @click="clearCache">
            {{ t('settingsAptCache.clear') }}
          </AppButton>
        </div>
      </div>

      <p v-if="!cfg?.enabled" class="ac-warn">{{ t('settingsAptCache.disabledHint') }}</p>
      <p v-if="deployError" class="ac-err" role="alert">{{ deployError }}</p>
      <p v-else-if="deployOutput" class="ac-ok">{{ t('settingsAptCache.deployDone') }}</p>
      <p v-if="upgradeMsg" :class="upgradeError ? 'ac-err' : 'ac-muted'" :role="upgradeError ? 'alert' : 'status'">{{ upgradeMsg }}</p>
      <p v-if="clearMsg" class="ac-muted">{{ clearMsg }}</p>

      <dl v-if="status" class="ac-status">
        <div class="ac-status-item">
          <dt>{{ t('settingsAptCache.stDeployed') }}</dt>
          <dd><span class="ac-dot" :class="status.deployed ? 'on' : 'off'" />{{ status.deployed ? t('settingsAptCache.stYes') : t('settingsAptCache.stNo') }}</dd>
        </div>
        <div class="ac-status-item">
          <dt>{{ t('settingsAptCache.stCache') }}</dt>
          <dd><span class="ac-dot" :class="status.running && status.reachable ? 'on' : status.running ? 'warn' : 'off'" />{{ status.running ? `:${cfg?.port ?? 3142} · ${t('settingsAptCache.stRunning')}` : t('settingsAptCache.stStopped') }}</dd>
        </div>
        <div v-if="status.tlsEnabled" class="ac-status-item">
          <dt>{{ t('settingsAptCache.stTlsFront') }}</dt>
          <dd><span class="ac-dot" :class="status.tlsRunning ? 'on' : 'off'" />{{ status.tlsRunning ? t('settingsAptCache.stRunning') : t('settingsAptCache.stStopped') }}</dd>
        </div>
        <div class="ac-status-item">
          <dt>{{ t('settingsAptCache.stCacheDisk') }}</dt>
          <dd class="mono">{{ fmtBytes(status.cacheBytes) }}</dd>
        </div>
        <div class="ac-status-item">
          <dt>{{ t('settingsAptCache.stImage') }}</dt>
          <dd class="mono">{{ status.image }}</dd>
        </div>
      </dl>
      <p v-else-if="statusLoading" class="ac-muted">{{ t('settingsAptCache.stLoading') }}</p>
    </article>

    <!-- 区块 3:apt 代理片段下发(仅显式勾选目标) -->
    <article class="ac-panel">
      <span class="ac-accent ac-accent--danger" aria-hidden="true" />
      <div class="ac-head-row">
        <div class="ac-head-text">
          <h3 class="ac-h3">{{ t('settingsAptCache.applyTitle') }}</h3>
          <p class="ac-hint">{{ t('settingsAptCache.applyHint') }}</p>
        </div>
        <AppButton variant="primary" :loading="applying" :disabled="!canApply" @click="apply">
          {{ t('settingsAptCache.applyBtn', { n: checkedCount }) }}
        </AppButton>
      </div>

      <p v-if="cfg?.enabled && cfg?.tlsCertId" class="ac-warn ac-warn--danger">{{ t('settingsAptCache.applyWarnTls', { proxy: `${proxyScheme}://${cfg.addr}` }) }}</p>
      <p v-else-if="cfg?.enabled" class="ac-warn ac-warn--danger">{{ t('settingsAptCache.applyWarn', { proxy: `${proxyScheme}://${cfg.addr}` }) }}</p>
      <p v-else class="ac-warn">{{ t('settingsAptCache.disabledHint') }}</p>
      <p v-if="serversError" class="ac-err" role="alert">{{ serversError }}</p>

      <ul class="ac-targets">
        <li class="ac-target">
          <label class="ac-target-row">
            <input type="checkbox" v-model="includeLocal" />
            <span class="ac-target-name">{{ t('settingsAptCache.localMachine') }}</span>
            <span class="ac-target-meta mono">/etc/apt/apt.conf.d/95pipewright-proxy</span>
            <button type="button" class="ac-mini" :disabled="inspecting === 'local'" @click="runInspect('local')">
              {{ inspecting === 'local' ? '…' : t('settingsAptCache.inspect') }}
            </button>
          </label>
          <p v-if="inspect['local']" class="ac-inspect mono">{{ inspectText(inspect['local']) }}</p>
        </li>
        <li v-for="s in servers" :key="s.id" class="ac-target">
          <label class="ac-target-row">
            <input type="checkbox" :checked="checked.has(s.id)" @change="toggleServer(s.id)" />
            <span class="ac-target-name">{{ s.name }}</span>
            <span class="ac-target-meta mono">{{ s.host }}:{{ s.port }}</span>
            <button type="button" class="ac-mini" :disabled="inspecting === s.id" @click="runInspect(s.id)">
              {{ inspecting === s.id ? '…' : t('settingsAptCache.inspect') }}
            </button>
          </label>
          <p v-if="inspect[s.id]" class="ac-inspect mono">{{ inspectText(inspect[s.id]!) }}</p>
        </li>
        <li v-if="!servers.length && !serversError" class="ac-target ac-target--empty">
          {{ t('settingsAptCache.noServers') }}
        </li>
      </ul>

      <table v-if="applyResults.length" class="ac-results">
        <thead>
          <tr>
            <th>{{ t('settingsAptCache.resTarget') }}</th>
            <th>{{ t('settingsAptCache.resStatus') }}</th>
            <th>{{ t('settingsAptCache.resBackup') }}</th>
            <th>{{ t('settingsAptCache.resError') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(r, i) in applyResults" :key="i">
            <td>{{ r.isLocal ? t('settingsAptCache.localMachine') : (r.targetName || r.targetId) }}</td>
            <td><span class="ac-dot" :class="r.ok ? 'on' : 'off'" />{{ r.ok ? t('settingsAptCache.resOk') : t('settingsAptCache.resFail') }}</td>
            <td class="mono ac-td-quiet">{{ r.backupPath || '—' }}</td>
            <td class="ac-td-quiet">{{ r.error || '—' }}</td>
          </tr>
        </tbody>
      </table>
    </article>
  </section>
</template>

<style scoped>
.ac {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  max-width: 760px;
}
.ac-head { margin-bottom: var(--space-1); }
.ac-title { font-size: var(--text-h2); font-weight: 700; letter-spacing: -0.01em; color: var(--color-text); }
.ac-sub { font-size: var(--text-body); color: var(--color-faint); margin-top: 2px; }

.ac-panel {
  position: relative;
  overflow: hidden;
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-lg);
  padding: var(--space-5);
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  box-shadow: 0 1px 2px rgba(0, 0, 0, 0.04), 0 8px 24px -16px rgba(0, 0, 0, 0.18);
}
.ac-accent {
  position: absolute; inset: 0 0 auto 0; height: 3px;
  background: linear-gradient(90deg, var(--color-primary), var(--color-primary-press) 60%, transparent);
}
.ac-accent--danger {
  background: linear-gradient(90deg, var(--color-danger), var(--color-amber) 60%, transparent);
}

.ac-head-row { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.ac-head-text { max-width: 60ch; }
.ac-h3 { font-size: 1rem; font-weight: 600; color: var(--color-text); margin: 0 0 4px; }
.ac-hint { font-size: 0.8rem; color: var(--color-faint); margin: 0; }
.ac-head-btns { display: flex; gap: 10px; flex: none; }

.ac-switch {
  position: relative; flex: none; width: 40px; height: 22px; border-radius: 999px; border: none;
  cursor: pointer; background: var(--color-border); transition: background var(--duration-fast);
}
.ac-switch--on { background: var(--color-primary); }
.ac-switch:disabled { opacity: 0.55; cursor: default; }
.ac-switch:focus-visible { outline: 2px solid var(--color-primary); outline-offset: 2px; }
.ac-switch-knob {
  position: absolute; top: 3px; left: 3px; width: 16px; height: 16px; border-radius: 50%;
  background: #fff; transition: transform var(--duration-fast);
}
.ac-switch--on .ac-switch-knob { transform: translateX(18px); }

.ac-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px 24px; transition: opacity var(--duration-fast); }
.ac-grid--off { opacity: 0.55; }
.ac-field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.ac-field--wide { grid-column: 1 / -1; }
.ac-label { font-size: 0.82rem; font-weight: 500; color: var(--color-dim); }
.ac-input {
  width: 100%; padding: 7px 10px; border: 1px solid var(--color-border); border-radius: 8px;
  background: var(--color-surface); color: var(--color-text); font-size: 0.9rem;
}
.ac-input:focus { outline: 2px solid var(--color-primary); outline-offset: -1px; }
.ac-input:disabled { opacity: 0.6; }
.ac-inline { display: flex; gap: 8px; align-items: center; }
.ac-inline .ac-input { flex: 1; }
.ac-mini {
  flex: none; padding: 4px 10px; font-size: 0.74rem; font-weight: 600; color: var(--color-primary);
  background: var(--color-card); border: 1px solid var(--color-border); border-radius: var(--radius-sm);
  cursor: pointer; transition: background var(--duration-fast), border-color var(--duration-fast);
}
.ac-mini:hover { border-color: var(--color-primary); background: var(--color-primary-soft); }
.ac-mini:disabled { opacity: 0.5; cursor: default; }

.ac-actions { display: flex; align-items: center; justify-content: flex-end; gap: 12px; }
.ac-ok { font-size: 0.82rem; color: var(--color-ok); }
.ac-err { font-size: 0.82rem; color: var(--color-danger, #e5484d); }
.ac-muted { font-size: 0.82rem; color: var(--color-faint); }
.ac-warn {
  margin: 0; font-size: 0.78rem; color: var(--color-faint);
  border-left: 2px solid var(--color-border); padding-left: 10px;
}
.ac-warn--danger { border-left-color: var(--color-amber, #f5a623); color: var(--color-amber, #b8860b); }

.ac-status {
  display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px 24px; margin: 0;
  padding-top: var(--space-3); border-top: 1px solid var(--color-border);
}
.ac-status-item { display: flex; flex-direction: column; gap: 3px; min-width: 0; }
.ac-status-item dt { font-size: var(--text-micro); color: var(--color-faint); text-transform: uppercase; letter-spacing: 0.07em; }
.ac-status-item dd { margin: 0; font-size: var(--text-caption); color: var(--color-text-soft); display: flex; align-items: center; gap: 6px; }
.ac-dot { width: 8px; height: 8px; border-radius: 50%; flex: none; display: inline-block; }
.ac-dot.on { background: var(--color-success, #18a058); }
.ac-dot.warn { background: var(--color-amber, #f5a623); }
.ac-dot.off { background: var(--color-border); }

.ac-targets { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 6px; }
.ac-target { border: 1px solid var(--color-border); border-radius: var(--radius-md); padding: 8px 12px; }
.ac-target--empty { border-style: dashed; color: var(--color-faint); font-size: 0.82rem; }
.ac-target-row { display: flex; align-items: center; gap: 10px; min-width: 0; }
.ac-target-name { font-size: 0.88rem; font-weight: 600; color: var(--color-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ac-target-meta { flex: 1; font-size: 0.76rem; color: var(--color-faint); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ac-inspect { margin: 6px 0 0 26px; font-size: 0.76rem; color: var(--color-dim); word-break: break-all; }

.ac-results { width: 100%; border-collapse: collapse; font-size: 0.82rem; }
.ac-results th {
  text-align: left; font-size: 0.74rem; text-transform: uppercase; letter-spacing: 0.06em;
  color: var(--color-faint); padding: 6px 10px; border-bottom: 1px solid var(--color-border);
}
.ac-results td { padding: 8px 10px; border-bottom: 1px solid var(--color-border); color: var(--color-text-soft); vertical-align: top; }
.ac-td-quiet { font-size: 0.76rem; word-break: break-all; }
.mono { font-family: var(--font-mono); }
</style>
