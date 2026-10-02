<script setup lang="ts">
/**
 * SettingsRegistry — 内置本地 Docker registry(镜像仓库)。
 *
 * 区块:
 *  1. 基础配置:启用开关 / 外部地址(带探测建议值)/ 缓存上游(预置 + 自定义)/ 端口 /
 *     制品与缓存存储目录(改制品目录→部署时自动迁移旧数据;改缓存目录→清空旧缓存)/
 *     镜像保留策略。保存 = PUT(只写库,**不触碰任何机器**)。
 *  2. 部署与状态:显式「部署/更新」按钮(compose up -d);栈状态卡(容器/探活/存储占用);
 *     「立即清理」按保留策略裁剪制品 tag。
 *  3. daemon.json 下发:勾选已登记服务器(或控制机本机)→ 危险确认(整文件覆盖 + 重启
 *     Docker)→ 逐台下发结果。**只作用于被勾选的机器**;未勾选的机器永不被触碰。
 */
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useConfirm } from '../../composables/useConfirm'
import {
  getRegistryHubConfig,
  saveRegistryHubConfig,
  deployRegistryStack,
  getRegistryStatus,
  pruneRegistry,
  applyRegistryDaemon,
  inspectRegistryDaemon,
  type RegistryHubConfig,
  type RegistryStatus,
  type DaemonApplyResult,
  type RegistryDaemonInspect,
} from '../../api/registryHub'
import { listServers, type Server } from '../../api/servers'
import { HttpError } from '../../api/http'
import AppButton from '../../components/ui/AppButton.vue'

const { t } = useI18n()
const confirm = useConfirm()

// ─── 区块 1:配置 ─────────────────────────────────────────────────────────────
const cfg = ref<RegistryHubConfig | null>(null)
const loading = ref(true)
const saving = ref(false)
const savedFlash = ref(false)
const saveError = ref('')
const upstreamCustom = ref('')

/** 缓存上游预置项(value 为 registry API 地址);当前值不在预置 → 自定义。 */
const UPSTREAM_PRESETS: Array<{ label: string; value: string }> = [
  { label: 'Docker Hub(registry-1.docker.io)', value: 'https://registry-1.docker.io' },
  { label: 'DaoCloud 镜像(docker.m.daocloud.io)', value: 'https://docker.m.daocloud.io' },
]

const upstreamPreset = computed({
  get: () => {
    const cur = cfg.value?.upstreamUrl ?? ''
    return UPSTREAM_PRESETS.some((p) => p.value === cur) ? cur : 'custom'
  },
  set: (v: string) => {
    if (!cfg.value) return
    if (v === 'custom') {
      cfg.value.upstreamUrl = upstreamCustom.value || 'https://'
      return
    }
    cfg.value.upstreamUrl = v
  },
})

const isCustomUpstream = computed(() => upstreamPreset.value === 'custom')

async function loadConfig(): Promise<void> {
  loading.value = true
  try {
    cfg.value = await getRegistryHubConfig()
    if (isCustomUpstream.value) upstreamCustom.value = cfg.value.upstreamUrl
  } catch {
    saveError.value = t('settingsRegistry.errLoad')
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

async function saveConfig(): Promise<void> {
  if (!cfg.value) return
  saving.value = true
  savedFlash.value = false
  saveError.value = ''
  try {
    cfg.value = await saveRegistryHubConfig({
      enabled: cfg.value.enabled,
      externalAddr: cfg.value.externalAddr.trim(),
      upstreamUrl: (isCustomUpstream.value ? upstreamCustom.value : cfg.value.upstreamUrl).trim(),
      artifactPort: Math.floor(Number(cfg.value.artifactPort) || 0),
      cachePort: Math.floor(Number(cfg.value.cachePort) || 0),
      artifactDataDir: cfg.value.artifactDataDir.trim(),
      cacheDataDir: cfg.value.cacheDataDir.trim(),
      keepPerProject: Math.max(0, Math.floor(Number(cfg.value.keepPerProject) || 0)),
      maxAgeDays: Math.max(0, Math.floor(Number(cfg.value.maxAgeDays) || 0)),
    })
    savedFlash.value = true
    window.setTimeout(() => { savedFlash.value = false }, 2000)
    void loadStatus()
  } catch (err) {
    saveError.value = err instanceof HttpError ? (err.apiError?.message ?? t('settingsRegistry.errSave')) : t('settingsRegistry.errSave')
  } finally {
    saving.value = false
  }
}

// ─── 区块 2:部署 / 状态 / 清理 ──────────────────────────────────────────────
const status = ref<RegistryStatus | null>(null)
const statusLoading = ref(false)
const deploying = ref(false)
const deployError = ref('')
const deployOutput = ref('')
const pruning = ref(false)
const pruneMsg = ref('')

async function loadStatus(): Promise<void> {
  statusLoading.value = true
  try {
    status.value = await getRegistryStatus()
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
    const res = await deployRegistryStack()
    if (res.ok) deployOutput.value = res.output
    else deployError.value = res.error || t('settingsRegistry.deployFailed')
    void loadStatus()
  } catch (err) {
    deployError.value = err instanceof HttpError ? (err.apiError?.message ?? t('settingsRegistry.deployFailed')) : t('settingsRegistry.deployFailed')
  } finally {
    deploying.value = false
  }
}

async function prune(): Promise<void> {
  pruning.value = true
  pruneMsg.value = ''
  try {
    const res = await pruneRegistry()
    pruneMsg.value = res.ok
      ? t('settingsRegistry.pruneDone', { n: res.deletedTags })
      : (res.error || t('settingsRegistry.pruneFailed'))
  } catch (err) {
    pruneMsg.value = err instanceof HttpError ? (err.apiError?.message ?? t('settingsRegistry.pruneFailed')) : t('settingsRegistry.pruneFailed')
  } finally {
    pruning.value = false
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

// ─── 区块 3:daemon.json 下发(仅显式勾选目标) ───────────────────────────────
const servers = ref<Server[]>([])
const serversError = ref('')
const checked = ref<Set<string>>(new Set())
const includeLocal = ref(false)
const applying = ref(false)
const applyResults = ref<DaemonApplyResult[]>([])
const inspecting = ref<string>('')
const inspect = ref<Record<string, RegistryDaemonInspect>>({})

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
    serversError.value = t('settingsRegistry.errServers')
  }
}

async function runInspect(id: string): Promise<void> {
  inspecting.value = id
  try {
    inspect.value[id] = await inspectRegistryDaemon(id)
  } catch {
    inspect.value[id] = { targetId: id, targetName: id, isLocal: id === 'local', mirrors: [], daemonJson: '', error: t('settingsRegistry.inspectFailed') }
  } finally {
    inspecting.value = ''
  }
}

async function apply(): Promise<void> {
  if (!cfg.value) return
  const names = [
    ...(includeLocal.value ? [t('settingsRegistry.localMachine')] : []),
    ...servers.value.filter((s) => checked.value.has(s.id)).map((s) => s.name),
  ]
  const ok = await confirm.open({
    title: t('settingsRegistry.applyConfirmTitle'),
    body: t('settingsRegistry.applyConfirmBody', { targets: names.join('、'), mirror: `http://${cfg.value.cacheAddr}` }),
    confirmLabel: t('settingsRegistry.applyConfirmLabel'),
    variant: 'danger',
  })
  if (!ok) return
  applying.value = true
  applyResults.value = []
  try {
    const res = await applyRegistryDaemon([...checked.value], includeLocal.value)
    applyResults.value = res.results
  } catch (err) {
    applyResults.value = [{
      targetId: '', targetName: '', isLocal: false, ok: false, backupPath: '',
      error: err instanceof HttpError ? (err.apiError?.message ?? t('settingsRegistry.applyFailed')) : t('settingsRegistry.applyFailed'),
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
})
</script>

<template>
  <section class="reg" aria-labelledby="reg-heading">
    <header class="reg-head">
      <h2 id="reg-heading" class="reg-title">{{ t('settingsRegistry.title') }}</h2>
      <p class="reg-sub">{{ t('settingsRegistry.subtitle') }}</p>
    </header>

    <!-- 区块 1:基础配置(保存只写库,不触碰机器) -->
    <article class="reg-panel">
      <span class="reg-accent" aria-hidden="true" />
      <div class="reg-head-row">
        <div class="reg-head-text">
          <h3 class="reg-h3">{{ t('settingsRegistry.cfgTitle') }}</h3>
          <p class="reg-hint">{{ t('settingsRegistry.cfgHint') }}</p>
        </div>
        <button
          type="button"
          class="reg-switch"
          :class="{ 'reg-switch--on': cfg?.enabled }"
          role="switch"
          :aria-checked="!!cfg?.enabled"
          :aria-label="t('settingsRegistry.cfgTitle')"
          :disabled="loading"
          @click="toggleEnabled"
        >
          <span class="reg-switch-knob" aria-hidden="true" />
        </button>
      </div>

      <div v-if="cfg" class="reg-grid" :class="{ 'reg-grid--off': !cfg.enabled }">
        <label class="reg-field reg-field--wide">
          <span class="reg-label">{{ t('settingsRegistry.externalAddr') }}</span>
          <span class="reg-inline">
            <input class="reg-input mono" v-model="cfg.externalAddr" :disabled="loading" :placeholder="cfg.suggestedAddr || '192.168.1.10'" />
            <button
              v-if="cfg.suggestedAddr && cfg.suggestedAddr !== cfg.externalAddr"
              type="button"
              class="reg-mini"
              @click="useSuggestedAddr"
            >{{ t('settingsRegistry.useSuggested') }}</button>
          </span>
          <span class="reg-hint">{{ t('settingsRegistry.externalAddrHint') }}</span>
        </label>

        <label class="reg-field reg-field--wide">
          <span class="reg-label">{{ t('settingsRegistry.upstream') }}</span>
          <select class="reg-input" v-model="upstreamPreset">
            <option v-for="p in UPSTREAM_PRESETS" :key="p.value" :value="p.value">{{ p.label }}</option>
            <option value="custom">{{ t('settingsRegistry.upstreamCustom') }}</option>
          </select>
          <input
            v-if="isCustomUpstream"
            class="reg-input mono reg-mt"
            v-model="upstreamCustom"
            placeholder="https://docker.example.com"
          />
          <span class="reg-hint">{{ t('settingsRegistry.upstreamHint') }}</span>
        </label>

        <label class="reg-field">
          <span class="reg-label">{{ t('settingsRegistry.artifactPort') }}</span>
          <input class="reg-input mono" type="number" min="1" max="65535" v-model.number="cfg.artifactPort" :disabled="loading" />
          <span class="reg-hint">{{ t('settingsRegistry.artifactPortHint', { addr: cfg.artifactAddr || '…' }) }}</span>
        </label>
        <label class="reg-field">
          <span class="reg-label">{{ t('settingsRegistry.cachePort') }}</span>
          <input class="reg-input mono" type="number" min="1" max="65535" v-model.number="cfg.cachePort" :disabled="loading" />
          <span class="reg-hint">{{ t('settingsRegistry.cachePortHint', { addr: cfg.cacheAddr || '…' }) }}</span>
        </label>

        <label class="reg-field">
          <span class="reg-label">{{ t('settingsRegistry.artifactDataDir') }}</span>
          <input class="reg-input mono" v-model="cfg.artifactDataDir" :disabled="loading" :placeholder="cfg.effectiveArtifactDataDir" spellcheck="false" />
          <span class="reg-hint">{{ t('settingsRegistry.artifactDataDirHint') }}</span>
        </label>
        <label class="reg-field">
          <span class="reg-label">{{ t('settingsRegistry.cacheDataDir') }}</span>
          <input class="reg-input mono" v-model="cfg.cacheDataDir" :disabled="loading" :placeholder="cfg.effectiveCacheDataDir" spellcheck="false" />
          <span class="reg-hint">{{ t('settingsRegistry.cacheDataDirHint') }}</span>
        </label>

        <label class="reg-field">
          <span class="reg-label">{{ t('settingsRegistry.keepPerProject') }}</span>
          <input class="reg-input mono" type="number" min="0" step="1" v-model.number="cfg.keepPerProject" :disabled="loading" />
          <span class="reg-hint">{{ t('settingsRegistry.keepPerProjectHint') }}</span>
        </label>
        <label class="reg-field">
          <span class="reg-label">{{ t('settingsRegistry.maxAgeDays') }}</span>
          <input class="reg-input mono" type="number" min="0" step="1" v-model.number="cfg.maxAgeDays" :disabled="loading" />
          <span class="reg-hint">{{ t('settingsRegistry.maxAgeDaysHint') }}</span>
        </label>
      </div>

      <div class="reg-actions">
        <span v-if="savedFlash" class="reg-ok" role="status">{{ t('settingsRegistry.saved') }}</span>
        <span v-if="saveError" class="reg-err" role="alert">{{ saveError }}</span>
        <AppButton variant="primary" :loading="saving" :disabled="loading" @click="saveConfig">
          {{ t('settingsRegistry.save') }}
        </AppButton>
      </div>
    </article>

    <!-- 区块 2:部署与状态 -->
    <article class="reg-panel">
      <span class="reg-accent" aria-hidden="true" />
      <div class="reg-head-row">
        <div class="reg-head-text">
          <h3 class="reg-h3">{{ t('settingsRegistry.stackTitle') }}</h3>
          <p class="reg-hint">{{ t('settingsRegistry.stackHint') }}</p>
        </div>
        <div class="reg-head-btns">
          <AppButton variant="primary" :loading="deploying" :disabled="!cfg?.enabled" @click="deploy">
            {{ t('settingsRegistry.deploy') }}
          </AppButton>
          <AppButton :loading="pruning" :disabled="!cfg?.enabled" @click="prune">
            {{ t('settingsRegistry.prune') }}
          </AppButton>
        </div>
      </div>

      <p v-if="!cfg?.enabled" class="reg-warn">{{ t('settingsRegistry.disabledHint') }}</p>
      <p v-if="deployError" class="reg-err" role="alert">{{ deployError }}</p>
      <p v-else-if="deployOutput" class="reg-ok">{{ t('settingsRegistry.deployDone') }}</p>
      <p v-if="pruneMsg" class="reg-muted">{{ pruneMsg }}</p>

      <dl v-if="status" class="reg-status">
        <div class="reg-status-item">
          <dt>{{ t('settingsRegistry.stDeployed') }}</dt>
          <dd><span class="reg-dot" :class="status.deployed ? 'on' : 'off'" />{{ status.deployed ? t('settingsRegistry.stYes') : t('settingsRegistry.stNo') }}</dd>
        </div>
        <div class="reg-status-item">
          <dt>{{ t('settingsRegistry.stArtifact') }}</dt>
          <dd><span class="reg-dot" :class="status.artifactRunning && status.artifactReachable ? 'on' : status.artifactRunning ? 'warn' : 'off'" />{{ status.artifactRunning ? `:${cfg?.artifactPort ?? 5000} · ${t('settingsRegistry.stRunning')}` : t('settingsRegistry.stStopped') }}</dd>
        </div>
        <div class="reg-status-item">
          <dt>{{ t('settingsRegistry.stCache') }}</dt>
          <dd><span class="reg-dot" :class="status.cacheRunning && status.cacheReachable ? 'on' : status.cacheRunning ? 'warn' : 'off'" />{{ status.cacheRunning ? `:${cfg?.cachePort ?? 5001} · ${t('settingsRegistry.stRunning')}` : t('settingsRegistry.stStopped') }}</dd>
        </div>
        <div class="reg-status-item">
          <dt>{{ t('settingsRegistry.stData') }}</dt>
          <dd class="mono">{{ fmtBytes(status.dataDirBytes) }}</dd>
        </div>
        <div class="reg-status-item">
          <dt>{{ t('settingsRegistry.stCacheDisk') }}</dt>
          <dd class="mono">{{ fmtBytes(status.cacheDirBytes) }}</dd>
        </div>
        <div class="reg-status-item">
          <dt>{{ t('settingsRegistry.stImage') }}</dt>
          <dd class="mono">{{ status.image }}</dd>
        </div>
      </dl>
      <p v-else-if="statusLoading" class="reg-muted">{{ t('settingsRegistry.stLoading') }}</p>
    </article>

    <!-- 区块 3:daemon.json 下发(仅显式勾选目标) -->
    <article class="reg-panel">
      <span class="reg-accent reg-accent--danger" aria-hidden="true" />
      <div class="reg-head-row">
        <div class="reg-head-text">
          <h3 class="reg-h3">{{ t('settingsRegistry.applyTitle') }}</h3>
          <p class="reg-hint">{{ t('settingsRegistry.applyHint') }}</p>
        </div>
        <AppButton variant="primary" :loading="applying" :disabled="!canApply" @click="apply">
          {{ t('settingsRegistry.applyBtn', { n: checkedCount }) }}
        </AppButton>
      </div>

      <p v-if="cfg?.enabled" class="reg-warn reg-warn--danger">{{ t('settingsRegistry.applyWarn', { mirror: `http://${cfg.cacheAddr}` }) }}</p>
      <p v-else class="reg-warn">{{ t('settingsRegistry.disabledHint') }}</p>
      <p v-if="serversError" class="reg-err" role="alert">{{ serversError }}</p>

      <ul class="reg-targets">
        <li class="reg-target">
          <label class="reg-target-row">
            <input type="checkbox" v-model="includeLocal" />
            <span class="reg-target-name">{{ t('settingsRegistry.localMachine') }}</span>
            <span class="reg-target-meta mono">/etc/docker/daemon.json</span>
            <button type="button" class="reg-mini" :disabled="inspecting === 'local'" @click="runInspect('local')">
              {{ inspecting === 'local' ? '…' : t('settingsRegistry.inspect') }}
            </button>
          </label>
          <p v-if="inspect['local']" class="reg-inspect mono">
            {{ inspect['local'].error || (inspect['local'].mirrors.length ? inspect['local'].mirrors.join(', ') : t('settingsRegistry.noMirror')) }}
          </p>
        </li>
        <li v-for="s in servers" :key="s.id" class="reg-target">
          <label class="reg-target-row">
            <input type="checkbox" :checked="checked.has(s.id)" @change="toggleServer(s.id)" />
            <span class="reg-target-name">{{ s.name }}</span>
            <span class="reg-target-meta mono">{{ s.host }}:{{ s.port }}</span>
            <button type="button" class="reg-mini" :disabled="inspecting === s.id" @click="runInspect(s.id)">
              {{ inspecting === s.id ? '…' : t('settingsRegistry.inspect') }}
            </button>
          </label>
          <p v-if="inspect[s.id]" class="reg-inspect mono">
            {{ inspect[s.id]!.error || (inspect[s.id]!.mirrors.length ? inspect[s.id]!.mirrors.join(', ') : t('settingsRegistry.noMirror')) }}
          </p>
        </li>
        <li v-if="!servers.length && !serversError" class="reg-target reg-target--empty">
          {{ t('settingsRegistry.noServers') }}
        </li>
      </ul>

      <table v-if="applyResults.length" class="reg-results">
        <thead>
          <tr>
            <th>{{ t('settingsRegistry.resTarget') }}</th>
            <th>{{ t('settingsRegistry.resStatus') }}</th>
            <th>{{ t('settingsRegistry.resBackup') }}</th>
            <th>{{ t('settingsRegistry.resError') }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(r, i) in applyResults" :key="i">
            <td>{{ r.isLocal ? t('settingsRegistry.localMachine') : (r.targetName || r.targetId) }}</td>
            <td><span class="reg-dot" :class="r.ok ? 'on' : 'off'" />{{ r.ok ? t('settingsRegistry.resOk') : t('settingsRegistry.resFail') }}</td>
            <td class="mono reg-td-quiet">{{ r.backupPath || '—' }}</td>
            <td class="reg-td-quiet">{{ r.error || '—' }}</td>
          </tr>
        </tbody>
      </table>
    </article>
  </section>
</template>

<style scoped>
.reg {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  max-width: 760px;
}
.reg-head { margin-bottom: var(--space-1); }
.reg-title { font-size: var(--text-h2); font-weight: 700; letter-spacing: -0.01em; color: var(--color-text); }
.reg-sub { font-size: var(--text-body); color: var(--color-faint); margin-top: 2px; }

.reg-panel {
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
.reg-accent {
  position: absolute; inset: 0 0 auto 0; height: 3px;
  background: linear-gradient(90deg, var(--color-primary), var(--color-primary-press) 60%, transparent);
}
.reg-accent--danger {
  background: linear-gradient(90deg, var(--color-danger), var(--color-amber) 60%, transparent);
}

.reg-head-row { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.reg-head-text { max-width: 60ch; }
.reg-h3 { font-size: 1rem; font-weight: 600; color: var(--color-text); margin: 0 0 4px; }
.reg-hint { font-size: 0.8rem; color: var(--color-faint); margin: 0; }
.reg-head-btns { display: flex; gap: 10px; flex: none; }

.reg-switch {
  position: relative; flex: none; width: 40px; height: 22px; border-radius: 999px; border: none;
  cursor: pointer; background: var(--color-border); transition: background var(--duration-fast);
}
.reg-switch--on { background: var(--color-primary); }
.reg-switch:disabled { opacity: 0.55; cursor: default; }
.reg-switch:focus-visible { outline: 2px solid var(--color-primary); outline-offset: 2px; }
.reg-switch-knob {
  position: absolute; top: 3px; left: 3px; width: 16px; height: 16px; border-radius: 50%;
  background: #fff; transition: transform var(--duration-fast);
}
.reg-switch--on .reg-switch-knob { transform: translateX(18px); }

.reg-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px 24px; transition: opacity var(--duration-fast); }
.reg-grid--off { opacity: 0.55; }
.reg-field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.reg-field--wide { grid-column: 1 / -1; }
.reg-label { font-size: 0.82rem; font-weight: 500; color: var(--color-dim); }
.reg-input {
  width: 100%; padding: 7px 10px; border: 1px solid var(--color-border); border-radius: 8px;
  background: var(--color-surface); color: var(--color-text); font-size: 0.9rem;
}
.reg-input:focus { outline: 2px solid var(--color-primary); outline-offset: -1px; }
.reg-input:disabled { opacity: 0.6; }
.reg-inline { display: flex; gap: 8px; align-items: center; }
.reg-inline .reg-input { flex: 1; }
.reg-mt { margin-top: 8px; }
.reg-mini {
  flex: none; padding: 4px 10px; font-size: 0.74rem; font-weight: 600; color: var(--color-primary);
  background: var(--color-card); border: 1px solid var(--color-border); border-radius: var(--radius-sm);
  cursor: pointer; transition: background var(--duration-fast), border-color var(--duration-fast);
}
.reg-mini:hover { border-color: var(--color-primary); background: var(--color-primary-soft); }
.reg-mini:disabled { opacity: 0.5; cursor: default; }

.reg-actions { display: flex; align-items: center; justify-content: flex-end; gap: 12px; }
.reg-ok { font-size: 0.82rem; color: var(--color-ok, #18a058); }
.reg-err { font-size: 0.82rem; color: var(--color-danger, #e5484d); }
.reg-muted { font-size: 0.82rem; color: var(--color-faint); }
.reg-warn {
  margin: 0; font-size: 0.78rem; color: var(--color-faint);
  border-left: 2px solid var(--color-border); padding-left: 10px;
}
.reg-warn--danger { border-left-color: var(--color-amber, #f5a623); color: var(--color-amber, #b8860b); }

.reg-status {
  display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px 24px; margin: 0;
  padding-top: var(--space-3); border-top: 1px solid var(--color-border);
}
.reg-status-item { display: flex; flex-direction: column; gap: 3px; min-width: 0; }
.reg-status-item dt { font-size: var(--text-micro); color: var(--color-faint); text-transform: uppercase; letter-spacing: 0.07em; }
.reg-status-item dd { margin: 0; font-size: var(--text-caption); color: var(--color-text-soft); display: flex; align-items: center; gap: 6px; }
.reg-dot { width: 8px; height: 8px; border-radius: 50%; flex: none; display: inline-block; }
.reg-dot.on { background: var(--color-success, #18a058); }
.reg-dot.warn { background: var(--color-amber, #f5a623); }
.reg-dot.off { background: var(--color-border); }

.reg-targets { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 6px; }
.reg-target { border: 1px solid var(--color-border); border-radius: var(--radius-md); padding: 8px 12px; }
.reg-target--empty { border-style: dashed; color: var(--color-faint); font-size: 0.82rem; }
.reg-target-row { display: flex; align-items: center; gap: 10px; min-width: 0; }
.reg-target-name { font-size: 0.88rem; font-weight: 600; color: var(--color-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.reg-target-meta { flex: 1; font-size: 0.76rem; color: var(--color-faint); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.reg-inspect { margin: 6px 0 0 26px; font-size: 0.76rem; color: var(--color-dim); word-break: break-all; }

.reg-results { width: 100%; border-collapse: collapse; font-size: 0.82rem; }
.reg-results th {
  text-align: left; font-size: 0.74rem; text-transform: uppercase; letter-spacing: 0.06em;
  color: var(--color-faint); padding: 6px 10px; border-bottom: 1px solid var(--color-border);
}
.reg-results td { padding: 8px 10px; border-bottom: 1px solid var(--color-border); color: var(--color-text-soft); vertical-align: top; }
.reg-td-quiet { font-size: 0.76rem; word-break: break-all; }
.mono { font-family: var(--font-mono); }
</style>
