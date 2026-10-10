<script setup lang="ts">
/*
  Certificates.vue — 证书管理(原「证书总览」的继任者,数据源从旧 Caddy 反代路由切到 certmgmt)。

  平台证书的统一管理页:
  - acme.sh 自动签发(DNS-01,凭据复用 DNS 提供商集成;支持泛域名)—— acme.sh 以内嵌脚本
    跑在控制机本地(DB 同级 acme/,签发不依赖网关);平台后台调度自动续期。
  - 手动导入现成证书(PEM 配对校验,SAN 自动解析)。
  - 签发/续期结果自动同步到被 SAN 覆盖的网关基域(nginx -t + reload)。

  页面结构:引擎状态卡 → 摘要卡(总数/已签发/处理中/最近到期)→ 表格(状态三态徽章 +
  到期高亮 + 自动续期开关 + 立即续期/删除)→ 签发/导入两个模态。存在 pending 证书时 3s 轮询。
*/
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { Certificate as CertIcon, Lock, LockOpen, AlertTriangle, Refresh, Plus, Download, Trash, Rotate, Rocket } from '@vicons/tabler'
import {
  listCerts,
  createCert,
  importCert,
  renewCert,
  deleteCert,
  setCertAutoRenew,
  getCertEngine,
  deployCertEngine,
  type Cert,
  type CertEngine,
  type CertCA,
  type CertKeyType,
} from '../api/certMgmt'
import { listDnsProviders, type DnsProvider } from '../api/dnsProviders'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'
import { useConfirm } from '../composables/useConfirm'
import EmptyState from '../components/ui/EmptyState.vue'
import ErrorState from '../components/ui/ErrorState.vue'
import SkeletonBlock from '../components/ui/SkeletonBlock.vue'
import AppButton from '../components/ui/AppButton.vue'
import AppSelect from '../components/ui/AppSelect.vue'

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

type LoadState = 'idle' | 'loading' | 'loaded' | 'error'
const loadState = ref<LoadState>('idle')
const loadError = ref('')
const certs = ref<Cert[]>([])
const engine = ref<CertEngine | null>(null)
const dnsProviders = ref<DnsProvider[]>([])
const refreshing = ref(false)

function errMsg(err: unknown, fallback: string): string {
  return err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback
}

async function load(): Promise<void> {
  loadState.value = certs.value.length === 0 ? 'loading' : 'loaded'
  refreshing.value = true
  loadError.value = ''
  try {
    const [cs, eng] = await Promise.all([listCerts(), getCertEngine()])
    certs.value = cs.items
    engine.value = eng
    loadState.value = 'loaded'
  } catch (err) {
    loadState.value = certs.value.length === 0 ? 'error' : 'loaded'
    loadError.value = errMsg(err, t('certMgmt.errNetwork'))
  } finally {
    refreshing.value = false
  }
}

async function loadProviders(): Promise<void> {
  try {
    dnsProviders.value = await listDnsProviders()
  } catch {
    dnsProviders.value = []
  }
}

onMounted(() => {
  void load()
  void loadProviders()
})

// ─── pending 轮询:有任何证书在签发/续期时每 3s 刷列表 ──────────────────────────
let pollTimer: ReturnType<typeof setInterval> | null = null
const hasPending = computed(() => certs.value.some((c) => c.status === 'pending'))
function syncPolling(): void {
  const want = hasPending.value && loadState.value !== 'error'
  if (want && pollTimer === null) {
    pollTimer = setInterval(() => {
      if (!hasPending.value) {
        syncPolling()
        return
      }
      listCerts()
        .then((res) => (certs.value = res.items))
        .catch(() => undefined)
    }, 3000)
  } else if (!want && pollTimer !== null) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}
// 轮询由 hasPending 驱动;在 load 后与卸载时同步。
async function loadAndSync(): Promise<void> {
  await load()
  syncPolling()
}
onUnmounted(() => {
  if (pollTimer !== null) clearInterval(pollTimer)
})

// ─── 派生:到期天数 / 摘要 ─────────────────────────────────────────────────────
interface Row {
  cert: Cert
  days: number | null
  expiring: boolean
}
const DAY_MS = 86_400_000
const EXPIRING_DAYS = 30
const rows = computed<Row[]>(() =>
  certs.value.map((cert) => {
    let days: number | null = null
    if (cert.notAfter) {
      const ms = new Date(cert.notAfter).getTime() - Date.now()
      days = Number.isNaN(ms) ? null : Math.ceil(ms / DAY_MS)
    }
    return { cert, days, expiring: days !== null && days <= EXPIRING_DAYS }
  }),
)
const total = computed(() => certs.value.length)
const issuedCount = computed(() => certs.value.filter((c) => c.status === 'issued').length)
const pendingCount = computed(() => certs.value.filter((c) => c.status === 'pending').length)
const failedCount = computed(() => certs.value.filter((c) => c.status === 'failed').length)
const soonest = computed<Row | undefined>(() => {
  const withDays = rows.value.filter((r) => r.days !== null)
  if (withDays.length === 0) return undefined
  return withDays.reduce((a, b) => ((a.days ?? 0) <= (b.days ?? 0) ? a : b))
})

const providerName = computed(() => {
  const m = new Map<string, string>()
  for (const p of dnsProviders.value) m.set(p.id, p.name || p.id)
  return (id: string): string => (id ? (m.get(id) ?? id) : '—')
})

function fmtDate(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  return Number.isNaN(d.getTime()) ? '—' : d.toISOString().slice(0, 10)
}
function expiryText(r: Row): string {
  if (r.days === null) return '—'
  if (r.days < 0) return t('certMgmt.expiredAgo', { n: Math.abs(r.days) })
  if (r.days === 0) return t('certMgmt.expiresToday')
  return t('certMgmt.daysLeft', { n: r.days })
}

// ─── 行操作:续期 / 重新下发 / 自动续期开关 / 删除 ──────────────────────────────
const busy = ref<Set<string>>(new Set())
function isBusy(id: string): boolean {
  return busy.value.has(id)
}
function markBusy(id: string, on: boolean): void {
  const next = new Set(busy.value)
  if (on) next.add(id)
  else next.delete(id)
  busy.value = next
}

async function renewRow(r: Row): Promise<void> {
  const c = r.cert
  if (isBusy(c.id)) return
  markBusy(c.id, true)
  try {
    if (c.source === 'acme') {
      await renewCert(c.id)
      toast.success(t('certMgmt.renewing'), { detail: c.primaryDomain })
      syncPolling()
    } else {
      await renewCert(c.id)
      toast.success(t('certMgmt.redeployed'), { detail: c.primaryDomain })
    }
    await loadAndSync()
  } catch (err) {
    toast.error(errMsg(err, t('certMgmt.renewFail')), { detail: c.primaryDomain })
  } finally {
    markBusy(c.id, false)
  }
}

async function toggleAutoRenew(r: Row): Promise<void> {
  const c = r.cert
  if (isBusy(c.id)) return
  markBusy(c.id, true)
  try {
    await setCertAutoRenew(c.id, !c.autoRenew)
    toast.success(t('certMgmt.autoRenewToggled'), { detail: c.primaryDomain })
    await loadAndSync()
  } catch (err) {
    toast.error(errMsg(err, t('certMgmt.autoRenewFail')))
  } finally {
    markBusy(c.id, false)
  }
}

async function deleteRow(r: Row): Promise<void> {
  const c = r.cert
  const ok = await confirm.open({
    title: t('certMgmt.delTitle'),
    body: t('certMgmt.delBody', { domain: c.primaryDomain }),
    confirmLabel: t('certMgmt.delConfirm'),
    variant: 'danger',
  })
  if (!ok) return
  try {
    await deleteCert(c.id)
    toast.success(t('certMgmt.deleted'), { detail: c.primaryDomain })
    await loadAndSync()
  } catch (err) {
    toast.error(errMsg(err, t('certMgmt.deleteFail')))
  }
}

// ─── 引擎卡 ────────────────────────────────────────────────────────────────────
const engineBusy = ref(false)
async function doDeployEngine(): Promise<void> {
  engineBusy.value = true
  try {
    engine.value = await deployCertEngine()
    toast.success(t('certMgmt.engine.deployed'))
  } catch (err) {
    toast.error(errMsg(err, t('certMgmt.engine.deployFail')))
  } finally {
    engineBusy.value = false
  }
}
// ─── 签发证书模态 ──────────────────────────────────────────────────────────────
const showCreate = ref(false)
const creating = ref(false)
// ca/keyType 用宽 string 表单类型(AppSelect 的 v-model 回写是 string),提交时收窄。
const createForm = ref<{
  primaryDomain: string
  sansText: string
  dnsProviderId: string
  ca: string
  keyType: string
  autoRenew: boolean
}>({
  primaryDomain: '',
  sansText: '',
  dnsProviderId: '',
  ca: 'letsencrypt',
  keyType: 'ec-256',
  autoRenew: true,
})
const providerOptions = computed(() =>
  dnsProviders.value.map((p) => ({
    value: p.id,
    label: `${p.name}(${p.type}${p.zones.map((z) => ' ' + z.baseDomain).join(',')})`,
  })),
)
const caOptions = computed(() => [
  { value: 'letsencrypt', label: "Let's Encrypt" },
  { value: 'zerossl', label: 'ZeroSSL' },
  { value: 'buypass', label: 'BuyPass' },
])
const keyTypeOptions = computed(() => [
  { value: 'ec-256', label: 'ECDSA P-256(推荐)' },
  { value: 'ec-384', label: 'ECDSA P-384' },
  { value: 'rsa-2048', label: 'RSA 2048' },
])
function openCreate(): void {
  createForm.value = {
    primaryDomain: '',
    sansText: '',
    dnsProviderId: dnsProviders.value[0]?.id ?? '',
    ca: 'letsencrypt',
    keyType: 'ec-256',
    autoRenew: true,
  }
  if (dnsProviders.value.length === 0) void loadProviders()
  showCreate.value = true
}
async function submitCreate(): Promise<void> {
  const f = createForm.value
  if (!f.primaryDomain.trim() || !f.dnsProviderId) {
    toast.error(t('certMgmt.create.errNoProvider'))
    return
  }
  creating.value = true
  try {
    const domains = f.sansText
      .split('\n')
      .map((s) => s.trim())
      .filter((s) => s.length > 0)
    const created = await createCert({
      primaryDomain: f.primaryDomain.trim(),
      domains,
      dnsProviderId: f.dnsProviderId,
      ca: f.ca as CertCA | '',
      keyType: f.keyType as CertKeyType,
      autoRenew: f.autoRenew,
    })
    showCreate.value = false
    toast.success(t('certMgmt.create.success'), { detail: created.primaryDomain })
    await loadAndSync()
  } catch (err) {
    toast.error(errMsg(err, t('certMgmt.create.fail')))
  } finally {
    creating.value = false
  }
}

// ─── 导入证书模态 ──────────────────────────────────────────────────────────────
const showImport = ref(false)
const importing = ref(false)
const importCertPem = ref('')
const importKeyPem = ref('')
function openImport(): void {
  importCertPem.value = ''
  importKeyPem.value = ''
  showImport.value = true
}
function onPemPicked(e: Event, which: 'cert' | 'key'): void {
  const f = (e.target as HTMLInputElement).files?.[0]
  if (!f) return
  const reader = new FileReader()
  reader.onload = () => {
    if (which === 'cert') importCertPem.value = String(reader.result ?? '')
    else importKeyPem.value = String(reader.result ?? '')
  }
  reader.readAsText(f)
}
async function submitImport(): Promise<void> {
  if (!importCertPem.value.trim() || !importKeyPem.value.trim()) return
  importing.value = true
  try {
    const c = await importCert({ certPem: importCertPem.value, keyPem: importKeyPem.value })
    showImport.value = false
    toast.success(t('certMgmt.import.success'), { detail: c.primaryDomain })
    await loadAndSync()
  } catch (err) {
    toast.error(errMsg(err, t('certMgmt.import.fail')))
  } finally {
    importing.value = false
  }
}
</script>

<template>
  <div class="certs">
    <header class="certs__header">
      <div class="certs__title-wrap">
        <span class="certs__eyebrow">
          <NIcon :size="13"><CertIcon /></NIcon>
          {{ t('certMgmt.eyebrow') }}
        </span>
        <h1 class="certs__title">{{ t('certMgmt.title') }}</h1>
        <p class="certs__sub">{{ t('certMgmt.subtitle') }}</p>
      </div>
      <div class="certs__actions">
        <AppButton variant="default" :loading="refreshing" @click="loadAndSync">
          <NIcon :size="14"><Refresh /></NIcon>&nbsp;{{ refreshing ? t('certMgmt.refreshingAll') : t('common.refresh') }}
        </AppButton>
        <AppButton variant="primary" @click="openCreate">
          <NIcon :size="14"><Plus /></NIcon>&nbsp;{{ t('certMgmt.btn.create') }}
        </AppButton>
        <AppButton variant="default" @click="openImport">
          <NIcon :size="14"><Download /></NIcon>&nbsp;{{ t('certMgmt.btn.import') }}
        </AppButton>
      </div>
    </header>

    <!-- 首屏骨架 -->
    <div v-if="loadState === 'loading'" class="certs__skel" :aria-label="t('certMgmt.loadingAria')" aria-busy="true">
      <div class="certs__skel-cards">
        <SkeletonBlock v-for="n in 4" :key="n" :height="84" width="100%" />
      </div>
      <SkeletonBlock :height="240" width="100%" />
    </div>

    <ErrorState v-else-if="loadState === 'error'" :title="t('certMgmt.errTitle')" :description="loadError" @retry="loadAndSync" />

    <EmptyState
      v-else-if="total === 0"
      :title="t('certMgmt.empty.title')"
      :description="t('certMgmt.empty.desc')"
    />

    <template v-else>
      <!-- 签发引擎卡 -->
      <section class="engine" :aria-label="t('certMgmt.engine.title')">
        <div class="engine__main">
          <span class="engine__ic"><NIcon :size="16"><Rocket /></NIcon></span>
          <div>
            <div class="engine__title">{{ t('certMgmt.engine.title') }}</div>
            <div class="engine__meta">
              <span class="engine__pill" :class="engine?.ready ? 'engine__pill--ok' : 'engine__pill--warn'">
                {{ !engine?.installed ? t('certMgmt.engine.notInstalled') : (engine.ready ? t('certMgmt.engine.ready') : t('certMgmt.engine.missingDeps')) }}
              </span>
              <span v-if="engine?.version" class="engine__kv mono">acme.sh {{ engine.version }}</span>
            </div>
            <p class="engine__hint">{{ t('certMgmt.engine.hint') }}</p>
          </div>
        </div>
        <div class="engine__actions">
          <AppButton variant="default" :loading="engineBusy" @click="doDeployEngine">
            {{ t('certMgmt.engine.deploy') }}
          </AppButton>
        </div>
      </section>

      <!-- 摘要卡 -->
      <section class="cards" :aria-label="t('certMgmt.summaryAria')">
        <article class="card card--total">
          <span class="card__num">{{ total }}</span>
          <span class="card__label">{{ t('certMgmt.cardTotal') }}</span>
        </article>
        <article class="card card--issued">
          <span class="card__num">{{ issuedCount }}</span>
          <span class="card__label">{{ t('certMgmt.cardIssued') }}</span>
        </article>
        <article class="card card--warn">
          <span class="card__num">{{ pendingCount + failedCount }}</span>
          <span class="card__label">{{ t('certMgmt.cardPendingFailed') }}</span>
          <span class="card__break">{{ t('certMgmt.cardPendingFailedBreak', { pending: pendingCount, failed: failedCount }) }}</span>
        </article>
        <article class="card card--expiry" :class="{ 'card--expiry-hot': soonest && soonest.expiring }">
          <span v-if="soonest" class="card__num">
            {{ (soonest.days ?? 0) < 0 ? '!' : soonest.days }}<span v-if="(soonest.days ?? 0) >= 0" class="card__unit">{{ t('certMgmt.dayUnit') }}</span>
          </span>
          <span v-else class="card__num card__num--none">—</span>
          <span class="card__label">{{ t('certMgmt.cardSoonest') }}</span>
          <span v-if="soonest" class="card__break mono" :title="soonest.cert.primaryDomain">{{ soonest.cert.primaryDomain }}</span>
          <span v-else class="card__break">{{ t('certMgmt.cardSoonestNone') }}</span>
        </article>
      </section>

      <!-- 处理中提示 -->
      <div v-if="hasPending" class="certs__polling">
        <NIcon :size="14" class="spin"><Refresh /></NIcon>
        {{ t('certMgmt.pollHint') }}
      </div>

      <!-- 表格 -->
      <div class="tablewrap">
        <table class="tbl">
          <thead>
            <tr>
              <th>{{ t('certMgmt.colDomains') }}</th>
              <th>{{ t('certMgmt.colSource') }}</th>
              <th>{{ t('certMgmt.colProvider') }}</th>
              <th>{{ t('certMgmt.colStatus') }}</th>
              <th>{{ t('certMgmt.colExpiry') }}</th>
              <th>{{ t('certMgmt.colAutoRenew') }}</th>
              <th class="tbl__act-h">{{ t('certMgmt.colActions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="r in rows"
              :key="r.cert.id"
              class="datarow"
              :class="{ 'datarow--busy': isBusy(r.cert.id), 'datarow--expiring': r.expiring }"
            >
              <td class="cell-domain">
                <div class="dom mono">{{ r.cert.primaryDomain }}</div>
                <div v-if="r.cert.domains.filter((d) => d !== r.cert.primaryDomain).length" class="dom__aliases">
                  <span v-for="a in r.cert.domains.filter((d) => d !== r.cert.primaryDomain)" :key="a" class="dom__alias mono">{{ a }}</span>
                </div>
                <div v-if="r.cert.statusDetail" class="dom__detail" :title="r.cert.statusDetail">{{ r.cert.statusDetail }}</div>
              </td>
              <td class="cell-src">
                <span class="tag" :class="r.cert.source === 'acme' ? 'tag--acme' : 'tag--manual'">
                  {{ r.cert.source === 'acme' ? t('certMgmt.sourceAcme') : t('certMgmt.sourceManual') }}
                </span>
                <span v-if="r.cert.ca" class="tag tag--ca">{{ r.cert.ca }}</span>
                <span class="tag tag--key mono">{{ r.cert.keyType }}</span>
              </td>
              <td class="cell-provider">{{ r.cert.validation === 'dns' ? providerName(r.cert.dnsProviderId) : '—' }}</td>
              <td>
                <span class="badge" :class="`badge--${r.cert.status}`">
                  <NIcon :size="13">
                    <Lock v-if="r.cert.status === 'issued'" />
                    <AlertTriangle v-else-if="r.cert.status === 'failed'" />
                    <LockOpen v-else />
                  </NIcon>
                  {{ r.cert.status === 'issued' ? t('certMgmt.statusIssued') : r.cert.status === 'failed' ? t('certMgmt.statusFailed') : t('certMgmt.statusPending') }}
                </span>
              </td>
              <td class="cell-expiry" :class="{ 'cell-expiry--hot': r.expiring }">
                <div class="mono">{{ fmtDate(r.cert.notBefore) }} → {{ fmtDate(r.cert.notAfter) }}</div>
                <div>{{ r.days === null ? '—' : expiryText(r) }}</div>
              </td>
              <td>
                <button
                  v-if="r.cert.source === 'acme'"
                  class="autobtn"
                  :class="{ 'autobtn--on': r.cert.autoRenew }"
                  :disabled="isBusy(r.cert.id)"
                  :title="t('certMgmt.autoRenewTitle')"
                  @click="toggleAutoRenew(r)"
                >
                  {{ r.cert.autoRenew ? t('certMgmt.autoOn') : t('certMgmt.autoOff') }}
                </button>
                <span v-else class="cell-dim">—</span>
              </td>
              <td class="cell-act">
                <button
                  class="rowbtn"
                  :disabled="isBusy(r.cert.id) || r.cert.status === 'pending'"
                  :title="r.cert.source === 'acme' ? t('certMgmt.renewTitle') : t('certMgmt.redeployTitle')"
                  @click="renewRow(r)"
                >
                  <NIcon :size="14"><Rotate /></NIcon>
                </button>
                <button class="rowbtn rowbtn--danger" :disabled="isBusy(r.cert.id)" :title="t('certMgmt.deleteTitle')" @click="deleteRow(r)">
                  <NIcon :size="14"><Trash /></NIcon>
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <p class="certs__foot">
        <NIcon :size="13" class="certs__foot-ic"><Lock /></NIcon>
        {{ t('certMgmt.footNote') }}
      </p>
    </template>

    <!-- 签发证书模态 -->
    <div v-if="showCreate" class="scrim" @click.self="showCreate = false">
      <div class="modal" role="dialog" :aria-label="t('certMgmt.create.title')">
        <header class="modal__head">
          <h3 class="modal__title">{{ t('certMgmt.create.title') }}</h3>
          <button class="modal__close" :aria-label="t('certMgmt.close')" @click="showCreate = false">✕</button>
        </header>
        <div class="modal__body">
          <label class="field">
            <span class="field__lbl">{{ t('certMgmt.create.primary') }}</span>
            <input v-model="createForm.primaryDomain" class="field__in mono" type="text" :placeholder="t('certMgmt.create.primaryPh')" />
          </label>
          <label class="field">
            <span class="field__lbl">{{ t('certMgmt.create.sans') }}</span>
            <textarea v-model="createForm.sansText" class="field__in mono" rows="3" :placeholder="t('certMgmt.create.sansPh')"></textarea>
          </label>
          <label class="field">
            <span class="field__lbl">{{ t('certMgmt.create.provider') }}</span>
            <AppSelect v-model="createForm.dnsProviderId" :options="providerOptions" />
            <span v-if="dnsProviders.length === 0" class="field__hint">{{ t('certMgmt.create.errNoProvider') }}</span>
          </label>
          <div class="modal__row">
            <label class="field">
              <span class="field__lbl">{{ t('certMgmt.create.ca') }}</span>
              <AppSelect v-model="createForm.ca" :options="caOptions" />
            </label>
            <label class="field">
              <span class="field__lbl">{{ t('certMgmt.create.keyType') }}</span>
              <AppSelect v-model="createForm.keyType" :options="keyTypeOptions" />
            </label>
          </div>
          <label class="check">
            <input v-model="createForm.autoRenew" type="checkbox" />
            <span>{{ t('certMgmt.create.autoRenew') }}</span>
          </label>
          <p class="modal__hint">{{ t('certMgmt.create.hint') }}</p>
        </div>
        <footer class="modal__foot">
          <AppButton variant="default" :disabled="creating" @click="showCreate = false">{{ t('certMgmt.cancel') }}</AppButton>
          <AppButton variant="primary" :loading="creating" :disabled="!createForm.primaryDomain.trim() || !createForm.dnsProviderId" @click="submitCreate">
            {{ creating ? t('certMgmt.create.creating') : t('certMgmt.create.submit') }}
          </AppButton>
        </footer>
      </div>
    </div>

    <!-- 导入证书模态 -->
    <div v-if="showImport" class="scrim" @click.self="showImport = false">
      <div class="modal" role="dialog" :aria-label="t('certMgmt.import.title')">
        <header class="modal__head">
          <h3 class="modal__title">{{ t('certMgmt.import.title') }}</h3>
          <button class="modal__close" :aria-label="t('certMgmt.close')" @click="showImport = false">✕</button>
        </header>
        <div class="modal__body">
          <label class="field">
            <span class="field__lbl">{{ t('certMgmt.import.certFile') }}</span>
            <input class="field__in" type="file" accept=".pem,.crt,.txt" @change="onPemPicked($event, 'cert')" />
            <textarea v-model="importCertPem" class="field__in mono" rows="4" placeholder="-----BEGIN CERTIFICATE-----"></textarea>
          </label>
          <label class="field">
            <span class="field__lbl">{{ t('certMgmt.import.keyFile') }}</span>
            <input class="field__in" type="file" accept=".pem,.key,.txt" @change="onPemPicked($event, 'key')" />
            <textarea v-model="importKeyPem" class="field__in mono" rows="4" placeholder="-----BEGIN PRIVATE KEY-----"></textarea>
          </label>
          <p class="modal__hint">{{ t('certMgmt.import.hint') }}</p>
        </div>
        <footer class="modal__foot">
          <AppButton variant="default" :disabled="importing" @click="showImport = false">{{ t('certMgmt.cancel') }}</AppButton>
          <AppButton variant="primary" :loading="importing" :disabled="!importCertPem.trim() || !importKeyPem.trim()" @click="submitImport">
            {{ importing ? t('certMgmt.import.importing') : t('certMgmt.import.submit') }}
          </AppButton>
        </footer>
      </div>
    </div>
  </div>
</template>

<style scoped>
.certs {
  padding: 28px clamp(16px, 4vw, 40px) 40px;
  max-width: 1180px;
  margin: 0 auto;
}

/* 头部 */
.certs__header {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 18px;
  flex-wrap: wrap;
  margin-bottom: 22px;
}
.certs__eyebrow {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: var(--text-micro);
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.09em;
  color: var(--color-primary);
}
.certs__title {
  margin: 6px 0 4px;
  font-size: var(--text-display);
  font-weight: 700;
  letter-spacing: -0.01em;
  color: var(--color-text);
}
.certs__sub {
  margin: 0;
  font-size: var(--text-label);
  color: var(--color-dim);
  max-width: 62ch;
  line-height: 1.55;
}
.certs__actions {
  display: inline-flex;
  gap: 8px;
  flex-wrap: wrap;
}

/* 骨架 */
.certs__skel {
  display: flex;
  flex-direction: column;
  gap: 18px;
}
.certs__skel-cards {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 14px;
}

/* 引擎卡 */
.engine {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
  padding: 14px 18px;
  margin-bottom: 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  background: var(--color-card);
  box-shadow: var(--shadow);
}
.engine__main {
  display: flex;
  gap: 12px;
  align-items: flex-start;
}
.engine__ic {
  display: inline-grid;
  place-items: center;
  width: 34px;
  height: 34px;
  border-radius: var(--rounded-md);
  background: var(--color-primary-soft);
  color: var(--color-primary);
  flex-shrink: 0;
  margin-top: 2px;
}
.engine__title {
  font-size: var(--text-label);
  font-weight: 700;
  color: var(--color-text);
}
.engine__meta {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin-top: 4px;
}
.engine__pill {
  font-size: var(--text-micro);
  font-weight: 600;
  padding: 2px 10px;
  border-radius: var(--rounded-full);
  white-space: nowrap;
}
.engine__pill--ok {
  color: var(--color-green);
  background: var(--color-green-soft);
}
.engine__pill--warn {
  color: var(--color-amber);
  background: var(--color-amber-soft);
}
.engine__kv {
  font-size: var(--text-micro);
  color: var(--color-dim);
}
.engine__hint {
  margin: 4px 0 0;
  font-size: var(--text-micro);
  color: var(--color-faint);
  max-width: 72ch;
}
.engine__actions {
  display: inline-flex;
  gap: 8px;
}

/* 摘要卡 —— bento 式,左侧色条标识语义 */
.cards {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 14px;
  margin-bottom: 16px;
}
.card {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 3px;
  padding: 18px 18px 16px 22px;
  border-radius: var(--rounded-card);
  border: 1px solid var(--color-border);
  background: var(--color-card);
  box-shadow: var(--shadow);
  overflow: hidden;
}
.card::before {
  content: '';
  position: absolute;
  inset: 0 auto 0 0;
  width: 4px;
  background: var(--color-faint);
}
.card--total::before {
  background: var(--color-primary);
}
.card--issued::before {
  background: var(--color-green);
}
.card--warn::before {
  background: var(--color-amber);
}
.card--expiry::before {
  background: var(--color-cyan);
}
.card--expiry-hot::before {
  background: var(--color-red);
}
.card--expiry-hot {
  border-color: var(--color-red-line);
}
.card__num {
  font-size: var(--text-kpi);
  font-weight: 700;
  line-height: 1.05;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}
.card__num--none {
  color: var(--color-faint);
}
.card__unit {
  font-size: var(--text-label);
  font-weight: 600;
  color: var(--color-dim);
  margin-left: 2px;
}
.card--issued .card__num {
  color: var(--color-green);
}
.card--warn .card__num {
  color: var(--color-amber);
}
.card--expiry-hot .card__num {
  color: var(--color-red);
}
.card__label {
  font-size: var(--text-micro);
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  color: var(--color-faint);
}
.card__break {
  margin-top: 3px;
  font-size: var(--text-micro);
  color: var(--color-dim);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* 处理中提示 */
.certs__polling {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
  padding: 7px 12px;
  border-radius: var(--rounded-md);
  font-size: var(--text-micro);
  font-weight: 600;
  color: var(--color-primary);
  background: var(--color-primary-soft);
  width: fit-content;
}
.spin {
  animation: certspin 1.2s linear infinite;
}
@keyframes certspin {
  to {
    transform: rotate(360deg);
  }
}

/* 表格 */
.tablewrap {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  background: var(--color-card);
  box-shadow: var(--shadow);
  overflow: hidden;
}
.tbl {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-label);
}
.tbl thead th {
  text-align: left;
  font-size: var(--text-micro);
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  color: var(--color-faint);
  padding: 11px 16px;
  background: var(--color-card-2);
  border-bottom: 1px solid var(--color-border);
  white-space: nowrap;
}
.tbl__act-h {
  text-align: right;
}
.datarow {
  border-bottom: 1px solid var(--color-border);
  transition: background var(--duration-fast, 150ms) ease;
}
.datarow:last-child {
  border-bottom: none;
}
.datarow:hover {
  background: var(--color-card-2);
}
.datarow--busy {
  opacity: 0.55;
}
.datarow--expiring {
  background: var(--color-amber-soft);
}
.datarow--expiring:hover {
  background: var(--color-amber-soft);
}
.tbl td {
  padding: 12px 16px;
  vertical-align: top;
  color: var(--color-dim);
}
.cell-domain {
  min-width: 200px;
}
.dom {
  font-weight: 600;
  color: var(--color-text);
  word-break: break-all;
}
.dom__aliases {
  margin-top: 4px;
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.dom__alias {
  font-size: var(--text-micro);
  padding: 1px 7px;
  border-radius: var(--rounded-full);
  background: var(--color-primary-soft);
  color: var(--color-primary);
}
.dom__detail {
  margin-top: 4px;
  font-size: var(--text-micro);
  color: var(--color-faint);
  max-width: 420px;
  /* 错误详情须完整可读(排障信息),换行展示而非省略号截断 */
  white-space: pre-wrap;
  word-break: break-all;
}
.cell-src {
  white-space: nowrap;
}
.tag {
  display: inline-block;
  font-size: var(--text-micro);
  font-weight: 600;
  padding: 1px 8px;
  border-radius: var(--rounded-full);
  margin-right: 4px;
}
.tag--acme {
  color: var(--color-primary);
  background: var(--color-primary-soft);
}
.tag--manual {
  color: var(--color-dim);
  background: var(--color-inset);
}
.tag--ca {
  color: var(--color-cyan);
  background: var(--color-inset);
}
.tag--key {
  color: var(--color-faint);
  background: var(--color-inset);
}
.cell-provider {
  white-space: nowrap;
  color: var(--color-text);
}
.cell-expiry {
  white-space: nowrap;
  font-variant-numeric: tabular-nums;
}
.cell-expiry--hot {
  color: var(--color-amber);
  font-weight: 600;
}
.cell-dim {
  color: var(--color-faint);
}
.badge {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: var(--text-micro);
  font-weight: 600;
  padding: 4px 10px;
  border-radius: var(--rounded-full);
  white-space: nowrap;
}
.badge--issued {
  color: var(--color-green);
  background: var(--color-green-soft);
}
.badge--pending {
  color: var(--color-amber);
  background: var(--color-amber-soft);
}
.badge--failed {
  color: var(--color-red);
  background: var(--color-red-soft);
}
.autobtn {
  font-size: var(--text-micro);
  font-weight: 600;
  padding: 3px 10px;
  border-radius: var(--rounded-full);
  border: 1px solid var(--color-border);
  background: var(--color-inset);
  color: var(--color-faint);
  cursor: pointer;
  transition: all var(--duration-fast, 150ms) ease;
}
.autobtn--on {
  color: var(--color-green);
  border-color: var(--color-green-line, var(--color-border));
  background: var(--color-green-soft);
}
.autobtn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.cell-act {
  text-align: right;
  white-space: nowrap;
}
.rowbtn {
  display: inline-grid;
  place-items: center;
  width: 30px;
  height: 30px;
  margin-left: 4px;
  border-radius: var(--rounded-md);
  border: 1px solid var(--color-border);
  background: var(--color-card-2);
  color: var(--color-faint);
  cursor: pointer;
  transition: all var(--duration-fast, 150ms) ease;
}
.rowbtn:hover:not(:disabled) {
  color: var(--color-primary);
  border-color: var(--color-primary);
}
.rowbtn--danger:hover:not(:disabled) {
  color: var(--color-red);
  border-color: var(--color-red);
}
.rowbtn:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}

/* 页脚 */
.certs__foot {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 16px 2px 0;
  font-size: var(--text-micro);
  color: var(--color-faint);
  line-height: 1.5;
}
.certs__foot-ic {
  color: var(--color-green);
  flex-shrink: 0;
}

/* 模态 */
.scrim {
  position: fixed;
  inset: 0;
  z-index: 60;
  display: grid;
  place-items: center;
  padding: 20px;
  background: color-mix(in srgb, #000 45%, transparent);
}
.modal {
  width: min(720px, 100%);
  max-height: min(86vh, 720px);
  display: flex;
  flex-direction: column;
  border-radius: var(--rounded-card);
  border: 1px solid var(--color-border);
  background: var(--color-card);
  box-shadow: var(--shadow-lg, var(--shadow));
  overflow: hidden;
}
.modal__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 14px 18px;
  border-bottom: 1px solid var(--color-border);
  background: var(--color-card-2);
}
.modal__title {
  margin: 0;
  font-size: var(--text-label);
  font-weight: 700;
  color: var(--color-text);
}
.modal__close {
  border: none;
  background: transparent;
  color: var(--color-dim);
  font-size: 14px;
  cursor: pointer;
  padding: 2px 6px;
  border-radius: 6px;
}
.modal__close:hover {
  color: var(--color-text);
  background: var(--color-inset);
}
.modal__body {
  padding: 16px 18px;
  display: flex;
  flex-direction: column;
  gap: 12px;
  overflow-y: auto;
}
.modal__row {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 12px;
}
.modal__foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  padding: 12px 18px;
  border-top: 1px solid var(--color-border);
  background: var(--color-card-2);
}
.modal__hint {
  margin: 0;
  font-size: var(--text-micro);
  color: var(--color-faint);
  line-height: 1.5;
}
.field {
  display: flex;
  flex-direction: column;
  gap: 5px;
}
.field__lbl {
  font-size: var(--text-micro);
  font-weight: 600;
  color: var(--color-dim);
}
.field__in {
  font: inherit;
  font-size: var(--text-label);
  padding: 8px 10px;
  border-radius: var(--rounded-md);
  border: 1px solid var(--color-border-strong);
  background: var(--color-inset);
  color: var(--color-text);
  width: 100%;
  box-sizing: border-box;
}
.field__in:focus {
  outline: none;
  border-color: var(--color-primary);
}
.field__hint {
  font-size: var(--text-micro);
  color: var(--color-amber);
}
.check {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: var(--text-label);
  color: var(--color-dim);
  cursor: pointer;
}

@media (max-width: 880px) {
  .cards,
  .certs__skel-cards {
    grid-template-columns: repeat(2, 1fr);
  }
  .tablewrap {
    overflow-x: auto;
  }
  .tbl {
    min-width: 780px;
  }
  .modal__row {
    grid-template-columns: 1fr;
  }
}
</style>
