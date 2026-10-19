<!--
  SsarMonitor.vue — 机器监控·列表页(自维护 fork MyLinkPi/ssar)。

  两区:
    1. 平台安装包状态卡(GitHub Release 缓存 / 手动触发下载;进度轮询);
    2. 服务器列表(每台安装状态 / 采集器存活 / 最近采样;单台或批量「安装/升级」)。
  点机器行 / 「详情 →」跳转独立详情页(/ssar-monitor/:id:总览图表 + 机器侧细粒度
  按需查询)—— 列表与详情分离,机器多时无需翻到页底找数据。
  数据:GET /api/ssar/overview(20s 轮询);写动作 POST download / install(CSRF;
  审计在后端)。
-->
<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import {
  getSsarOverview,
  triggerSsarDownload,
  installSsar,
  type SsarOverview,
  type SsarServerStatus,
  type SsarInstallResult,
} from '../api/ssar'
import { HttpError } from '../api/http'
import AppButton from '../components/ui/AppButton.vue'
import ErrorState from '../components/ui/ErrorState.vue'
import SkeletonBlock from '../components/ui/SkeletonBlock.vue'

type LoadState = 'idle' | 'loading' | 'error'

const { t } = useI18n()
const router = useRouter()

const loadState = ref<LoadState>('idle')
const loadError = ref('')
const overview = ref<SsarOverview | null>(null)

// ─── 总览轮询(状态探测走后端 TTL 缓存;下载进度也在这看)────────────────────
const OVERVIEW_POLL_MS = 20_000
let overviewTimer: ReturnType<typeof setInterval> | null = null

async function loadOverview(): Promise<void> {
  try {
    const o = await getSsarOverview()
    // 防御性归一:后端 nil 切片可能序列化为 null(老版本/边缘路径),模板按数组消费。
    if (o?.binaries) {
      o.binaries.packages = Array.isArray(o.binaries.packages) ? o.binaries.packages : []
    }
    if (!Array.isArray(o?.items)) {
      o.items = []
    }
    overview.value = o
    loadState.value = loadState.value === 'error' ? 'idle' : loadState.value
  } catch (e) {
    if (loadState.value !== 'loading') return // 已有数据时静默,不打断页面
    loadState.value = 'error'
    loadError.value = e instanceof HttpError ? e.message : String(e)
  }
}

async function initialLoad(): Promise<void> {
  loadState.value = 'loading'
  await loadOverview()
  if (loadState.value === 'loading') loadState.value = 'idle'
}

// ─── 安装 / 下载动作 ──────────────────────────────────────────────────────────
const installing = ref(false)
const installBanner = ref('')
const downloading = ref(false)

async function downloadPackages(): Promise<void> {
  downloading.value = true
  try {
    await triggerSsarDownload()
  } catch {
    // 202 端点失败少见;轮询状态里会体现 lastErr。
  } finally {
    downloading.value = false
  }
}

async function runInstall(serverIds: string[]): Promise<void> {
  if (serverIds.length === 0 || installing.value) return
  installing.value = true
  installBanner.value = ''
  try {
    const results = await installSsar(serverIds)
    const ok = results.filter((r: SsarInstallResult) => r.ok).length
    installBanner.value =
      ok === results.length
        ? t('ssarMonitor.installAllOk', { n: ok })
        : t('ssarMonitor.installPartial', { ok, fail: results.length - ok })
    const failed = results.find((r: SsarInstallResult) => !r.ok && r.error)
    if (failed) installBanner.value += ` · ${failed.error}`
    await loadOverview()
  } catch (e) {
    installBanner.value = e instanceof HttpError ? e.message : t('ssarMonitor.installFailed')
  } finally {
    installing.value = false
  }
}

function installAll(): void {
  const ids = (overview.value?.items ?? []).map((s) => s.serverId)
  void runInstall(ids)
}

function installOne(s: SsarServerStatus): void {
  void runInstall([s.serverId])
}

// ─── 详情跳转 ────────────────────────────────────────────────────────────────
function goDetail(id: string): void {
  void router.push({ name: 'ssar-monitor-detail', params: { id } })
}

// ─── 展示格式化 ──────────────────────────────────────────────────────────────
function fmtTime(iso: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString()
}

function statusBadge(s: SsarServerStatus): { cls: string; text: string } {
  if (s.error) return { cls: 'badge--err', text: s.error }
  if (!s.installed) return { cls: 'badge--none', text: t('ssarMonitor.notInstalled') }
  const ver = `ssar v${s.version || '?'}`
  if (!s.collectorActive) return { cls: 'badge--warn', text: t('ssarMonitor.collectorStopped') + ' · ' + ver }
  return { cls: 'badge--ok', text: t('ssarMonitor.collectorActive') + ' · ' + ver }
}

const binLine = computed(() => {
  const b = overview.value?.binaries
  if (!b) return ''
  if (b.downloading) return t('ssarMonitor.binDownloading')
  if (b.version) return t('ssarMonitor.binVersion', { version: b.version, n: b.packages.length })
  if (b.lastErr) return t('ssarMonitor.binErr', { err: b.lastErr })
  return t('ssarMonitor.binEmpty')
})

onMounted(() => {
  void initialLoad()
  overviewTimer = setInterval(() => void loadOverview(), OVERVIEW_POLL_MS)
})
onUnmounted(() => {
  if (overviewTimer) clearInterval(overviewTimer)
})
</script>

<template>
  <div class="ssarmon">
    <header class="view-header">
      <div class="view-header__text">
        <h1 class="view-title">{{ t('ssarMonitor.title') }}</h1>
        <p class="view-sub">{{ t('ssarMonitor.subtitle') }}</p>
      </div>
      <div class="view-header__actions">
        <AppButton :loading="loadState === 'loading'" @click="initialLoad">
          {{ t('common.refresh') }}
        </AppButton>
      </div>
    </header>

    <ErrorState
      v-if="loadState === 'error'"
      :title="t('ssarMonitor.errLoadTitle')"
      :description="loadError"
      @retry="initialLoad"
    />
    <template v-else>
      <SkeletonBlock v-if="!overview" />

      <template v-else>
        <!-- ── 平台安装包状态 ─────────────────────────────────────────── -->
        <section class="panel bin-panel" aria-labelledby="ssar-bin-heading">
          <div class="bin-panel__main">
            <h2 id="ssar-bin-heading" class="panel-title">{{ t('ssarMonitor.binHeading') }}</h2>
            <p
              class="bin-panel__line"
              :class="{ 'bin-panel__line--err': !!overview.binaries.lastErr && !overview.binaries.version }"
            >
              {{ binLine }}
            </p>
            <p v-if="overview.binaries.packages.length" class="bin-panel__pkgs">
              {{ overview.binaries.packages.join(' · ') }}
            </p>
          </div>
          <div class="bin-panel__actions">
            <AppButton
              variant="primary"
              :loading="downloading || overview.binaries.downloading"
              :disabled="overview.binaries.downloading"
              @click="downloadPackages"
            >
              {{ overview.binaries.version ? t('ssarMonitor.binRedownload') : t('ssarMonitor.binDownload') }}
            </AppButton>
            <AppButton
              variant="primary"
              :loading="installing"
              :disabled="overview.items.length === 0"
              @click="installAll"
            >
              {{ t('ssarMonitor.installAll') }}
            </AppButton>
          </div>
        </section>

        <p v-if="installBanner" class="install-banner" role="status">{{ installBanner }}</p>

        <!-- ── 服务器列表(点行进详情)─────────────────────────────────── -->
        <section class="panel" aria-labelledby="ssar-servers-heading">
          <h2 id="ssar-servers-heading" class="panel-title">{{ t('ssarMonitor.serversHeading') }}</h2>
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th scope="col">{{ t('ssarMonitor.colServer') }}</th>
                  <th scope="col">{{ t('ssarMonitor.colStatus') }}</th>
                  <th scope="col">{{ t('ssarMonitor.colLastSample') }}</th>
                  <th scope="col" class="col-act">
                    <span class="sr-only">{{ t('ssarMonitor.colActions') }}</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="s in overview.items" :key="s.serverId" class="row--click" @click="goDetail(s.serverId)">
                  <td class="cell-name">{{ s.name }}</td>
                  <td><span class="badge" :class="statusBadge(s).cls">{{ statusBadge(s).text }}</span></td>
                  <td class="cell-mono">{{ fmtTime(s.lastSampleAt) }}</td>
                  <td class="col-act">
                    <a class="link-btn" href="#" @click.prevent.stop="goDetail(s.serverId)">
                      {{ t('common.detailArrow') }}
                    </a>
                    <AppButton :loading="installing" @click.stop="installOne(s)">
                      {{ s.installed ? t('ssarMonitor.upgrade') : t('ssarMonitor.install') }}
                    </AppButton>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>
      </template>
    </template>
  </div>
</template>

<style scoped>
.ssarmon {
  display: flex;
  flex-direction: column;
  gap: 18px;
}
.bin-panel {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}
.bin-panel__line {
  margin: 6px 0 0;
  font-size: var(--text-label);
  color: var(--color-dim);
}
.bin-panel__line--err {
  color: var(--color-danger, #d64545);
}
.bin-panel__pkgs {
  margin: 4px 0 0;
  font-family: var(--font-mono);
  font-size: 0.76rem;
  color: var(--color-faint, var(--color-dim));
  word-break: break-all;
}
.bin-panel__actions {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
}
.install-banner {
  margin: 0;
  padding: 10px 14px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  background: var(--color-card);
  font-size: var(--text-label);
  color: var(--color-text);
}
.badge {
  display: inline-block;
  padding: 3px 10px;
  border-radius: 999px;
  font-size: 0.76rem;
  font-weight: 600;
  white-space: nowrap;
}
.badge--ok {
  background: color-mix(in srgb, var(--color-success, #16a34a) 14%, transparent);
  color: var(--color-success, #16a34a);
}
.badge--warn {
  background: color-mix(in srgb, var(--color-warning, #e0a32e) 16%, transparent);
  color: var(--color-warning, #e0a32e);
}
.badge--err {
  background: color-mix(in srgb, var(--color-danger, #d64545) 14%, transparent);
  color: var(--color-danger, #d64545);
}
.badge--none {
  background: color-mix(in srgb, var(--color-dim) 14%, transparent);
  color: var(--color-dim);
}
.row--click {
  cursor: pointer;
}
.link-btn {
  color: var(--color-accent, #4f7cff);
  text-decoration: none;
  font-size: 0.82rem;
  margin-right: 12px;
}
.link-btn:hover {
  text-decoration: underline;
}
.col-act {
  white-space: nowrap;
}
.table-wrap {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  background: var(--color-card);
  box-shadow: var(--shadow);
  overflow-x: auto;
}
.table {
  width: 100%;
  border-collapse: collapse;
  font-size: 0.86rem;
}
.table th {
  text-align: left;
  font-weight: 600;
  color: var(--color-faint);
  font-size: 0.76rem;
  padding: 12px 14px;
  border-bottom: 1px solid var(--color-border);
  white-space: nowrap;
}
.table td {
  padding: 13px 14px;
  border-bottom: 1px solid var(--color-border);
  color: var(--color-text);
  vertical-align: middle;
}
.table tbody tr:last-child td {
  border-bottom: none;
}
.cell-name {
  font-weight: 600;
}
.cell-mono {
  font-family: var(--font-mono);
  font-size: 0.8rem;
  white-space: nowrap;
}
</style>
