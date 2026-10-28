<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NButton, NCard, NProgress, NTag, useMessage } from 'naive-ui'
import { http } from '../api/http'
import type { UpdateInfo, UpgradeStatus } from '../api/types'
import { useStatusStore } from '../stores/status'
import { useLocalStore } from '../stores/local'
import { formatTime, orDash, shortError } from '../lib/format'

const status = useStatusStore()
const local = useLocalStore()
const message = useMessage()

const update = ref<UpdateInfo | null>(null)
const checking = ref(false)
const upgrading = ref(false)
const upgradeStatus = ref<UpgradeStatus>({
  phase: '',
  current: '',
  target: '',
  message: '',
  time: '',
})

const snap = computed(() => status.snapshot)
const peerCount = computed(() => snap.value?.easyTier?.links?.length ?? 0)
const routeCount = computed(() => snap.value?.easyTier?.proxyCidrs?.length ?? 0)
const avgLatency = computed(() => {
  const links = snap.value?.easyTier?.links ?? []
  if (links.length === 0) return '—'
  const total = links.reduce((sum, link) => sum + (link.latencyMs || 0), 0)
  return `${Math.round(total / links.length)}ms`
})
const serviceText = computed(() => {
  const runningAs = snap.value?.service.runningAs ?? ''
  if (runningAs === 'service') return 'Windows 服务'
  if (runningAs === 'console') return '前台进程'
  return '—'
})
const binaryText = computed(() => {
  const bins = snap.value?.binaries
  if (!bins) return '—'
  if (bins.downloading) return '正在下载…'
  if (bins.corePath === '') return '未就位'
  return orDash(bins.version)
})
const upgradePhase = computed(() => phaseLabel(upgradeStatus.value.phase))
const upgradeRunning = computed(() =>
  ['downloading', 'applying', 'restarting'].includes(upgradeStatus.value.phase),
)

let timer: number | undefined

function phaseLabel(phase: string): string {
  switch (phase) {
    case 'downloading':
      return '下载升级包'
    case 'applying':
      return '替换程序文件'
    case 'restarting':
      return '重启服务'
    case 'done':
      return '已完成'
    case 'skipped':
      return '无需升级'
    case 'error':
      return '失败'
    default:
      return ''
  }
}

async function loadUpgradeStatus(): Promise<void> {
  try {
    upgradeStatus.value = await http.get<UpgradeStatus>('/api/upgrade/status')
  } catch {
    // 服务未运行时忽略;由离线横幅统一提示。
  }
}

async function checkUpdate(): Promise<void> {
  if (checking.value) return
  checking.value = true
  try {
    update.value = await http.get<UpdateInfo>('/api/upgrade/check')
    if (update.value.checkError) {
      message.error(`检查更新失败：${update.value.checkError}`)
    } else if (update.value.updateAvailable) {
      message.success(`发现新版本 ${update.value.latest}`)
    } else {
      message.info(`已是最新版本（${update.value.current}）`)
    }
  } catch (err) {
    message.error(err instanceof Error ? err.message : String(err))
  } finally {
    checking.value = false
  }
}

async function startUpgrade(): Promise<void> {
  const latest = update.value?.latest
  if (!latest) return
  upgrading.value = true
  try {
    await local.upgrade(latest)
    message.success('已请求升级（如弹出 UAC 请确认）')
    await loadUpgradeStatus()
  } catch (err) {
    message.error(err instanceof Error ? err.message : String(err))
  } finally {
    upgrading.value = false
  }
}

onMounted(() => {
  void loadUpgradeStatus()
  timer = window.setInterval(loadUpgradeStatus, 3000)
})

onBeforeUnmount(() => {
  if (timer !== undefined) window.clearInterval(timer)
})
</script>

<template>
  <div class="page">
    <section class="hero">
      <div class="hero-main">
        <div class="hero-state">
          <span class="dot" :class="status.meta.tone"></span>
          <strong>{{ status.meta.label }}</strong>
        </div>
        <p class="muted hero-hint">{{ status.meta.hint }}</p>
        <div class="metrics">
          <div class="metric">
            <span class="faint">虚拟 IP</span>
            <span class="mono">{{ orDash(snap?.easyTier?.selfIp ?? '') }}</span>
          </div>
          <div class="metric">
            <span class="faint">网络</span>
            <span>{{ orDash(snap?.join.networkName ?? '') }}</span>
          </div>
          <div class="metric">
            <span class="faint">在线对端</span>
            <span>{{ peerCount }}</span>
          </div>
          <div class="metric">
            <span class="faint">平均延迟</span>
            <span class="mono">{{ avgLatency }}</span>
          </div>
          <div class="metric">
            <span class="faint">通告网段</span>
            <span>{{ routeCount }}</span>
          </div>
        </div>
      </div>
    </section>

    <div class="grid">
      <n-card title="后台服务" size="small">
        <div class="kv">
          <span class="faint">形态</span><span>{{ serviceText }}</span>
        </div>
        <div class="kv">
          <span class="faint">PID</span><span class="mono">{{ snap?.service.pid || '—' }}</span>
        </div>
        <div class="kv">
          <span class="faint">客户端版本</span><span>{{ orDash(snap?.clientVersion ?? '') }}</span>
        </div>
        <div class="kv">
          <span class="faint">入网认证</span>
          <span>
            {{
              snap?.join.hasCredential
                ? '临时凭据（Secure Mode）'
                : snap?.join.hasSecret
                  ? '长期密钥（Secure Mode）'
                  : '未配置'
            }}
          </span>
        </div>
      </n-card>

      <n-card title="EasyTier 二进制" size="small">
        <div class="kv">
          <span class="faint">版本</span><span>{{ binaryText }}</span>
        </div>
        <div class="kv">
          <span class="faint">路径</span>
          <span class="mono path">{{ orDash(snap?.binaries.corePath ?? '') }}</span>
        </div>
        <div v-if="snap?.binaries.lastErr" class="err">{{ shortError(snap.binaries.lastErr) }}</div>
      </n-card>

      <n-card title="更新" size="small">
        <template #header-extra>
          <n-tag v-if="upgradeStatus.phase === 'done'" type="success" size="small">已完成</n-tag>
          <n-tag v-else-if="upgradeStatus.phase === 'error'" type="error" size="small">失败</n-tag>
        </template>
        <div class="kv">
          <span class="faint">当前版本</span><span>{{ orDash(snap?.clientVersion ?? '') }}</span>
        </div>
        <div v-if="update?.updateAvailable" class="kv">
          <span class="faint">可升级到</span><span>{{ update.latest }}</span>
        </div>
        <div v-if="upgradeRunning" class="upgrade-progress">
          <n-progress type="line" :percentage="0" :processing="true" :show-indicator="false" />
          <span class="faint">
            正在升级…{{ upgradePhase ? `（${upgradePhase}）` : '' }}
            {{ upgradeStatus.time ? formatTime(upgradeStatus.time) : '' }}
          </span>
        </div>
        <div v-else-if="upgradeStatus.phase === 'error'" class="err">
          {{ shortError(upgradeStatus.error ?? '升级失败') }}
        </div>
        <div class="actions">
          <n-button size="small" :loading="checking" @click="checkUpdate">检查更新</n-button>
          <n-button
            v-if="update?.updateAvailable && update.latest"
            size="small"
            type="primary"
            :loading="upgrading"
            @click="startUpgrade"
          >
            升级到 {{ update.latest }}
          </n-button>
        </div>
      </n-card>
    </div>
  </div>
</template>

<style scoped>
.page {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.hero {
  position: relative;
  overflow: hidden;
  border-radius: 14px;
  border: 1px solid var(--color-border);
  background:
    radial-gradient(520px 180px at 88% -30%, var(--color-primary-soft), transparent 60%),
    var(--color-card);
  padding: 22px 24px;
}

.hero-state {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 20px;
}

.hero-hint {
  margin: 4px 0 0;
  font-size: 13px;
}

.metrics {
  display: flex;
  flex-wrap: wrap;
  gap: 28px;
  margin-top: 18px;
}

.metric {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: 14px;
}

.dot {
  width: 10px;
  height: 10px;
  border-radius: 50%;
  background: var(--color-faint);
}

.dot.ok {
  background: var(--color-success);
  box-shadow: 0 0 8px var(--color-success-soft);
}

.dot.warn {
  background: var(--color-warning);
}

.grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
  gap: 16px;
}

.kv {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  padding: 4px 0;
  font-size: 14px;
}

.path {
  max-width: 240px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.err {
  margin-top: 8px;
  color: var(--color-danger);
  font-size: 13px;
}

.actions {
  display: flex;
  gap: 8px;
  margin-top: 14px;
}

.upgrade-progress {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-top: 12px;
  font-size: 12px;
}
</style>
