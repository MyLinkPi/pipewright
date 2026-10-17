<!--
  Cluster.vue — 集群(对等节点 + EasyTier 自组网)总览。

  无中心:每个节点平等,本页展示「本节点视角」的集群状态:
    · EasyTier 概览卡:本机虚拟 IP / 网络 / 托管进程运行态 / 状态查询错误提示;
    · 节点表:已知对端(发现登记 + 手工补录)× 链路状态(p2p 直连 / 经 X 中继)、延迟、在线;
    · 节点展开:经本节点代理查询对端的「服务器指标」与「项目/流水线状态」——是否可见由
      **对端**的授权单决定(默认全拒绝;未授权显示占位徽标,不报错)。

  数据源:GET /api/mesh/overview(15s 轮询)+ 按需 GET /api/mesh/peers/{id}/{servers,projects}。
-->
<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { Refresh } from '@vicons/tabler'
import {
  getMeshOverview,
  fetchPeerServers,
  fetchPeerProjects,
  type MeshOverview,
  type MeshPeerProject,
  type MeshProxyDegraded,
} from '../api/mesh'
import type { ServerMetrics } from '../api/servers'
import AppButton from '../components/ui/AppButton.vue'
import EmptyState from '../components/ui/EmptyState.vue'
import ErrorState from '../components/ui/ErrorState.vue'

const { t } = useI18n()

type LoadState = 'idle' | 'loading' | 'error'

const state = ref<LoadState>('idle')
const errorMsg = ref('')
const overview = ref<MeshOverview | null>(null)
const expanded = ref<string>('')
const peerServers = ref<Record<string, ServerMetrics[] | MeshProxyDegraded | 'loading'>>({})
const peerProjects = ref<Record<string, MeshPeerProject[] | MeshProxyDegraded | 'loading'>>({})
let timer: number | undefined

async function load(): Promise<void> {
  try {
    overview.value = await getMeshOverview()
    state.value = 'idle'
  } catch (err) {
    errorMsg.value = err instanceof Error ? err.message : String(err)
    state.value = 'error'
  }
}

/** 代理查询结果的降级原因('' = 非 degraded;模板里避免索引访问的窄化问题)。 */
function degradedReason(v: unknown): string {
  if (typeof v === 'object' && v !== null && (v as MeshProxyDegraded).available === false) {
    return (v as MeshProxyDegraded).reason
  }
  return ''
}

async function toggleExpand(nodeId: string): Promise<void> {
  if (expanded.value === nodeId) {
    expanded.value = ''
    return
  }
  expanded.value = nodeId
  if (peerServers.value[nodeId]) return
  peerServers.value[nodeId] = 'loading'
  peerProjects.value[nodeId] = 'loading'
  try {
    const [servers, projects] = await Promise.all([
      fetchPeerServers(nodeId),
      fetchPeerProjects(nodeId),
    ])
    peerServers.value[nodeId] = servers as ServerMetrics[] | MeshProxyDegraded
    peerProjects.value[nodeId] = projects as MeshPeerProject[] | MeshProxyDegraded
  } catch {
    peerServers.value[nodeId] = { available: false, reason: 'unreachable' }
    peerProjects.value[nodeId] = { available: false, reason: 'unreachable' }
  }
}

function fmtBytes(n: number | null | undefined): string {
  if (!n || n <= 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`
}

function memPct(m: ServerMetrics): string {
  if (!m.memory || !m.memory.totalBytes) return '—'
  return `${Math.round((m.memory.usedBytes / m.memory.totalBytes) * 100)}%`
}

function diskPct(m: ServerMetrics): string {
  if (!m.disk || !m.disk.totalBytes) return '—'
  return `${Math.round((m.disk.usedBytes / m.disk.totalBytes) * 100)}%`
}

function peerName(id: string): string {
  const p = overview.value?.peers.find((x) => x.nodeId === id)
  return p?.name || p?.nodeId.slice(0, 8) || id
}

const easyTierOk = computed(() => overview.value?.easytier?.ok ?? false)
const supervisor = computed(() => overview.value?.supervisor)

onMounted(() => {
  void load()
  timer = window.setInterval(() => void load(), 15_000)
})
onUnmounted(() => {
  if (timer !== undefined) window.clearInterval(timer)
})
</script>

<template>
  <div class="cluster-page">
    <header class="page-head">
      <div>
        <h1>{{ t('cluster.title') }}</h1>
        <p class="sub">{{ t('cluster.subtitle') }}</p>
      </div>
      <AppButton variant="default" size="sm" @click="load">
        < Refresh class="btn-ico" />
        <span>{{ t('common.refresh') }}</span>
      </AppButton>
    </header>

    <ErrorState v-if="state === 'error'" :description="errorMsg" show-retry @retry="load" />

    <template v-if="overview">
      <!-- EasyTier 概览卡 -->
      <section class="et-card" :class="{ off: !easyTierOk }">
        <div class="et-grid">
          <div class="et-item">
            <span class="et-label">{{ t('cluster.et.selfIp') }}</span>
            <span class="et-value">{{ overview.easytier.selfIp || '—' }}</span>
          </div>
          <div class="et-item">
            <span class="et-label">{{ t('cluster.et.network') }}</span>
            <span class="et-value">{{ overview.settings.networkName || '—' }}</span>
          </div>
          <div class="et-item">
            <span class="et-label">{{ t('cluster.et.mode') }}</span>
            <span class="et-value">
              {{ overview.externalMode ? t('cluster.et.modeExternal') : t('cluster.et.modeManaged') }}
            </span>
          </div>
          <div class="et-item">
            <span class="et-label">{{ t('cluster.et.process') }}</span>
            <span class="et-value" :class="supervisor?.running ? 'ok-text' : 'warn-text'">
              <template v-if="overview.externalMode">{{ t('cluster.et.readonly') }}</template>
              <template v-else-if="supervisor?.running">{{ t('cluster.et.running') }}</template>
              <template v-else>{{ t('cluster.et.stopped') }}</template>
            </span>
          </div>
          <div class="et-item">
            <span class="et-label">{{ t('cluster.et.peers') }}</span>
            <span class="et-value">{{ overview.peers.filter((p) => p.online).length }}/{{ overview.peers.length }}</span>
          </div>
        </div>
        <p v-if="!overview.settings.enabled" class="et-hint">{{ t('cluster.et.disabledHint') }}</p>
        <p v-else-if="overview.easytier.err" class="et-hint warn-text">{{ overview.easytier.err }}</p>
      </section>

      <!-- 节点表 -->
      <section v-if="overview.peers.length === 0" class="empty-wrap">
        <EmptyState :title="t('cluster.empty.title')" :description="t('cluster.empty.hint')" />
      </section>
      <section v-else class="peer-table" role="table">
        <div class="peer-row head" role="row">
          <span class="c-name">{{ t('cluster.peers.name') }}</span>
          <span class="c-vip">{{ t('cluster.peers.virtualIp') }}</span>
          <span class="c-link">{{ t('cluster.peers.link') }}</span>
          <span class="c-lat">{{ t('cluster.peers.latency') }}</span>
          <span class="c-seen">{{ t('cluster.peers.lastSeen') }}</span>
          <span class="c-act"></span>
        </div>
        <template v-for="p in overview.peers" :key="p.nodeId">
          <div class="peer-row" role="row" :class="{ offline: !p.online }">
            <span class="c-name">
              <span class="dot" :class="p.online ? 'on' : 'off'"></span>
              {{ p.name || p.nodeId.slice(0, 8) }}
            </span>
            <span class="c-vip mono">{{ p.virtualIp || '—' }}</span>
            <span class="c-link">
              <template v-if="!p.online">—</template>
              <span v-else-if="p.linkType === 'p2p'" class="tag ok">{{ t('cluster.link.p2p') }}</span>
              <span v-else class="tag warn">{{ t('cluster.link.relay', { via: p.relayVia || '?' }) }}</span>
            </span>
            <span class="c-lat">{{ p.online ? `${Math.round(p.latencyMs)} ms` : '—' }}</span>
            <span class="c-seen">{{ p.lastSeen ? new Date(p.lastSeen).toLocaleString() : '—' }}</span>
            <span class="c-act">
              <button class="link-btn" @click="toggleExpand(p.nodeId)">
                {{ expanded === p.nodeId ? t('cluster.collapse') : t('cluster.expand') }}
              </button>
              <a v-if="p.publicUrl" class="link-btn" :href="p.publicUrl" target="_blank" rel="noreferrer">
                {{ t('cluster.openPeer') }}
              </a>
            </span>
          </div>

          <!-- 展开:对端服务器指标 + 项目(可见性由对端授权单决定) -->
          <div v-if="expanded === p.nodeId" class="peer-detail">
            <div class="detail-block">
              <h3>{{ t('cluster.detail.servers') }}</h3>
              <template v-if="peerServers[p.nodeId] === 'loading'"><span class="placeholder">…</span></template>
              <p v-else-if="degradedReason(peerServers[p.nodeId])" class="placeholder">
                {{ degradedReason(peerServers[p.nodeId]) === 'forbidden'
                  ? t('cluster.detail.forbidden')
                  : t('cluster.detail.unreachable') }}
              </p>
              <template v-else>
                <div v-if="(peerServers[p.nodeId] as ServerMetrics[]).length === 0" class="placeholder">
                  {{ t('cluster.detail.noServers') }}
                </div>
                <div v-for="m in (peerServers[p.nodeId] as ServerMetrics[])" :key="m.serverId" class="srv-line">
                  <span class="srv-name">{{ m.serverId }}</span>
                  <span :class="m.reachable ? 'ok-text' : 'warn-text'">
                    {{ m.reachable
                      ? `CPU ${m.cpu?.loadavg1 ?? '—'}/${m.cpu?.cores ?? '—'} · ${t('cluster.detail.mem')} ${memPct(m)} · ${t('cluster.detail.disk')} ${diskPct(m)}`
                      : t('cluster.detail.unreachableHost') }}
                  </span>
                </div>
              </template>
            </div>
            <div class="detail-block">
              <h3>{{ t('cluster.detail.projects') }}</h3>
              <template v-if="peerProjects[p.nodeId] === 'loading'"><span class="placeholder">…</span></template>
              <p v-else-if="degradedReason(peerProjects[p.nodeId])" class="placeholder">
                {{ degradedReason(peerProjects[p.nodeId]) === 'forbidden'
                  ? t('cluster.detail.forbidden')
                  : t('cluster.detail.unreachable') }}
              </p>
              <template v-else>
                <div v-if="(peerProjects[p.nodeId] as MeshPeerProject[]).length === 0" class="placeholder">
                  {{ t('cluster.detail.noProjects') }}
                </div>
                <div
                  v-for="pr in (peerProjects[p.nodeId] as MeshPeerProject[])"
                  :key="pr.id"
                  class="srv-line"
                >
                  <span class="srv-name">{{ pr.name }}</span>
                  <span class="muted">
                    {{ pr.sourceType === 'pushed' ? t('cluster.detail.pushedSource') : 'git' }}
                    · {{ pr.hasPipeline ? t('cluster.detail.pipelineReady') : t('cluster.detail.pipelineDraft') }}
                  </span>
                </div>
              </template>
            </div>
          </div>
        </template>
      </section>
    </template>
  </div>
</template>

<style scoped>
.cluster-page { display: flex; flex-direction: column; gap: 16px; }
.page-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; }
.page-head h1 { margin: 0; font-size: 20px; }
.sub { margin: 4px 0 0; color: var(--color-text-muted, #888); font-size: 13px; }
.btn-ico { width: 14px; height: 14px; }

.et-card { border: 1px solid var(--color-border, #333); border-radius: 10px; padding: 14px 16px; }
.et-card.off { opacity: 0.85; }
.et-grid { display: flex; flex-wrap: wrap; gap: 24px; }
.et-item { display: flex; flex-direction: column; gap: 2px; }
.et-label { font-size: 12px; color: var(--color-text-muted, #888); }
.et-value { font-size: 14px; font-weight: 600; }
.et-hint { margin: 10px 0 0; font-size: 13px; color: var(--color-text-muted, #888); }

.peer-table { border: 1px solid var(--color-border, #333); border-radius: 10px; overflow: hidden; }
.peer-row { display: grid; grid-template-columns: 1.4fr 1fr 1.4fr 0.7fr 1.2fr auto; gap: 8px; padding: 10px 16px; align-items: center; border-top: 1px solid var(--color-border, #2a2a2a); font-size: 13px; }
.peer-row.head { border-top: none; color: var(--color-text-muted, #888); font-size: 12px; background: var(--color-bg-soft, #1c1c1c); }
.peer-row.offline { opacity: 0.55; }
.c-name { display: flex; align-items: center; gap: 8px; font-weight: 600; }
.dot { width: 8px; height: 8px; border-radius: 50%; background: var(--color-text-muted, #666); }
.dot.on { background: var(--color-green, #4caf50); }
.mono { font-family: var(--font-mono, monospace); }
.tag { display: inline-block; padding: 1px 8px; border-radius: 999px; font-size: 12px; }
.tag.ok { background: var(--color-green-soft, rgba(76, 175, 80, 0.15)); color: var(--color-green, #4caf50); }
.tag.warn { background: var(--color-amber-soft, rgba(255, 193, 7, 0.15)); color: var(--color-amber, #ffc107); }
.link-btn { background: none; border: none; color: var(--color-primary, #4c6ef5); cursor: pointer; font-size: 13px; padding: 0 4px; text-decoration: none; }
.c-act { display: flex; gap: 4px; white-space: nowrap; }

.peer-detail { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; padding: 12px 16px 16px; border-top: 1px dashed var(--color-border, #2a2a2a); background: var(--color-bg-soft, #161616); }
.detail-block h3 { margin: 4px 0 8px; font-size: 13px; color: var(--color-text-muted, #999); }
.srv-line { display: flex; justify-content: space-between; gap: 12px; padding: 4px 0; font-size: 13px; }
.srv-name { font-weight: 600; }
.placeholder { color: var(--color-text-muted, #888); font-size: 13px; margin: 4px 0; }
.muted { color: var(--color-text-muted, #888); }
.ok-text { color: var(--color-green, #4caf50); }
.warn-text { color: var(--color-amber, #ffc107); }
.empty-wrap { border: 1px dashed var(--color-border, #333); border-radius: 10px; padding: 24px; }
</style>
