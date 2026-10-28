<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  NButton,
  NCard,
  NEmpty,
  NInputNumber,
  NSelect,
  NSwitch,
  NTag,
  useMessage,
} from 'naive-ui'
import { useConfigStore } from '../stores/config'
import { useStatusStore } from '../stores/status'
import { buildSocksPatch, socksDraft, type SocksDraft } from '../lib/configDraft'
import { validateProxies } from '../lib/validate'
import type { ProxyConfig } from '../api/types'
import { shortError } from '../lib/format'

const config = useConfigStore()
const status = useStatusStore()
const message = useMessage()

const enabled = ref(false)
const port = ref(1080)
const exitNodes = ref<string[]>([])
const proxies = ref<ProxyConfig[]>([])
const base = ref<SocksDraft>({ enabled: false, port: 0, exitNodes: [] })
const baseProxies = ref<ProxyConfig[]>([])
const saving = ref(false)

function syncDraft(): void {
  const view = config.view
  if (!view) return
  const socks = socksDraft(view.socks5)
  base.value = { enabled: socks.enabled, port: socks.port, exitNodes: [...socks.exitNodes] }
  enabled.value = socks.enabled
  port.value = socks.port > 0 ? socks.port : 1080
  exitNodes.value = [...socks.exitNodes]
  baseProxies.value = (view.proxies ?? []).map((p) => ({ port: p.port, exitNodes: [...p.exitNodes] }))
  proxies.value = (view.proxies ?? []).map((p) => ({ port: p.port, exitNodes: [...p.exitNodes] }))
}

onMounted(async () => {
  if (!config.view) await config.load()
  syncDraft()
})

const exitOptions = computed(() => {
  const options = new Map<string, string>()
  for (const link of status.snapshot?.easyTier?.links ?? []) {
    options.set(link.virtualIp, `${link.virtualIp}${link.hostname ? ` · ${link.hostname}` : ''}`)
  }
  for (const ip of exitNodes.value) {
    if (ip && !options.has(ip)) options.set(ip, `${ip}（离线）`)
  }
  for (const p of proxies.value) {
    for (const ip of p.exitNodes) {
      if (ip && !options.has(ip)) options.set(ip, `${ip}（离线）`)
    }
  }
  return [...options].map(([value, label]) => ({ value, label }))
})

const dirty = computed(
  () =>
    Object.keys(
      buildSocksPatch(
        { enabled: enabled.value, port: port.value, exitNodes: exitNodes.value },
        proxies.value,
        base.value,
        baseProxies.value,
      ),
    ).length > 0,
)

function proxyState(proxy: ProxyConfig): { text: string; type: 'default' | 'success' | 'error' } {
  const live = status.snapshot?.proxies.find((p) => p.port === proxy.port)
  if (!live) return { text: '启动中', type: 'default' }
  if (live.lastErr) return { text: `错误：${shortError(live.lastErr)}`, type: 'error' }
  return live.running ? { text: '运行中', type: 'success' } : { text: '启动中', type: 'default' }
}

function nextFreePort(): number {
  const used = new Set(proxies.value.map((p) => p.port))
  if (enabled.value) used.add(port.value)
  let candidate = 1081
  while (used.has(candidate)) candidate += 1
  return candidate
}

function addProxy(): void {
  proxies.value = [...proxies.value, { port: nextFreePort(), exitNodes: [] }]
}

function removeProxy(index: number): void {
  proxies.value = proxies.value.filter((_, i) => i !== index)
}

async function copyAddress(): Promise<void> {
  try {
    await navigator.clipboard.writeText(`127.0.0.1:${port.value}`)
    message.success('已复制代理地址')
  } catch {
    message.error('复制失败（浏览器未授予剪贴板权限）')
  }
}

async function save(): Promise<void> {
  const err = validateProxies(enabled.value, port.value, proxies.value)
  if (err) {
    message.error(err)
    return
  }
  saving.value = true
  try {
    await config.save(
      buildSocksPatch(
        { enabled: enabled.value, port: port.value, exitNodes: exitNodes.value },
        proxies.value,
        base.value,
        baseProxies.value,
      ),
    )
    syncDraft()
    await status.refresh()
    message.success('已保存并应用（组网可能短暂重连）')
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page">
    <n-card title="本地 SOCKS5 代理" size="small">
      <template #header-extra>
        <n-tag v-if="dirty" size="small" type="warning">有未保存修改</n-tag>
      </template>
      <p class="faint hint">
        应用（浏览器/终端等）把代理指向 127.0.0.1:端口 即可经集群访问；可达范围 = 网段路由内配置的网段。
        无认证，仅限本机使用；域名解析走本机 DNS。
      </p>
      <div class="row">
        <n-switch v-model:value="enabled" />
        <span class="muted">启用</span>
        <span class="faint">端口</span>
        <n-input-number v-model:value="port" :min="1" :max="65535" :show-button="false" class="port" />
        <n-button size="small" :disabled="!enabled" @click="copyAddress">复制代理地址</n-button>
      </div>
      <div class="row">
        <span class="faint label">出口节点</span>
        <n-select
          v-model:value="exitNodes"
          multiple
          filterable
          clearable
          :options="exitOptions"
          placeholder="不使用出口节点（仅访问虚拟网/通告网段）"
          class="grow"
        />
      </div>
    </n-card>

    <n-card title="多出口代理" size="small">
      <template #header-extra>
        <n-button size="small" @click="addProxy">添加代理</n-button>
      </template>
      <p class="faint hint">
        每条 = 一个独立本地 SOCKS5 端口 + 出口节点（按序优先）。每条会拉起一个轻量组网实例，
        集群节点列表会多出「主机名-p端口」条目。
      </p>
      <div v-if="proxies.length > 0" class="proxy-list">
        <div v-for="(proxy, index) in proxies" :key="index" class="proxy-row">
          <span class="faint">端口</span>
          <n-input-number
            :value="proxy.port"
            :min="1"
            :max="65535"
            :show-button="false"
            class="port"
            @update:value="(v: number | null) => (proxy.port = v ?? 0)"
          />
          <n-select
            :value="proxy.exitNodes"
            multiple
            filterable
            clearable
            :options="exitOptions"
            placeholder="无出口（仅虚拟网/通告网段）"
            class="grow"
            @update:value="(v: string[]) => (proxy.exitNodes = v)"
          />
          <n-tag size="small" :type="proxyState(proxy).type">{{ proxyState(proxy).text }}</n-tag>
          <n-button size="tiny" quaternary @click="removeProxy(index)">删除</n-button>
        </div>
      </div>
      <n-empty v-else description="未配置多出口代理" />
      <template #action>
        <div class="actions">
          <n-button size="small" @click="syncDraft">重置</n-button>
          <n-button size="small" type="primary" :loading="saving" :disabled="!dirty" @click="save">
            保存并应用
          </n-button>
        </div>
      </template>
    </n-card>
  </div>
</template>

<style scoped>
.page {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.hint {
  margin: 0 0 12px;
  font-size: 13px;
}

.row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 6px 0;
}

.label {
  min-width: 56px;
}

.port {
  width: 120px;
}

.grow {
  flex: 1;
  min-width: 220px;
}

.proxy-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.proxy-row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 10px;
  border-radius: 8px;
  background: var(--color-inset);
}

.actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
</style>
