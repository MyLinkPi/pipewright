<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { NAlert, NButton, useMessage } from 'naive-ui'
import { useStatusStore } from '../stores/status'
import { useConfigStore } from '../stores/config'
import { useLocalStore } from '../stores/local'
import { hasToken } from '../api/http'

const router = useRouter()
const message = useMessage()
const status = useStatusStore()
const config = useConfigStore()
const local = useLocalStore()

const acting = ref(false)
const hasAuth = hasToken()

const nav = [
  { to: '/overview', label: '概览' },
  { to: '/peers', label: '对端' },
  { to: '/routes', label: '网段路由' },
  { to: '/proxy', label: '本地代理' },
  { to: '/config', label: '配置总览' },
  { to: '/config/connection', label: '连接参数' },
  { to: '/aliyun', label: '阿里云安全组' },
  { to: '/logs', label: '日志' },
  { to: '/settings', label: '设置' },
]

async function onPrimary(): Promise<void> {
  if (acting.value) return
  acting.value = true
  try {
    switch (status.action.kind) {
      case 'install-service':
        await local.installService()
        message.success('已请求安装服务（如弹出 UAC 请确认）')
        break
      case 'import':
        await router.push('/config/connection')
        break
      case 'connect':
        await config.save({ enabled: true })
        await status.refresh()
        message.success('已请求连接')
        break
      default:
        await config.save({ enabled: false })
        await status.refresh()
        message.success('已断开')
        break
    }
  } catch (err) {
    message.error(err instanceof Error ? err.message : String(err))
  } finally {
    acting.value = false
  }
}

async function installFromBanner(): Promise<void> {
  try {
    await local.installService()
    message.success('已请求安装服务（如弹出 UAC 请确认）')
  } catch (err) {
    message.error(err instanceof Error ? err.message : String(err))
  }
}
</script>

<template>
  <div v-if="!hasAuth" class="unauth">
    <h2>未授权</h2>
    <p class="muted">请从 pipewright 客户端（托盘图标或主窗口）打开本界面。</p>
  </div>

  <div v-else class="shell">
    <aside class="sidebar">
      <div class="brand">
        <span class="brand-mark"></span>
        <div>
          <strong>pipewright</strong>
          <div class="faint brand-sub">集群客户端</div>
        </div>
      </div>
      <nav class="nav">
        <RouterLink v-for="item in nav" :key="item.to" :to="item.to" class="nav-item">
          {{ item.label }}
        </RouterLink>
      </nav>
      <div class="sidebar-foot faint">
        v{{ local.info?.version ?? '—' }}
      </div>
    </aside>

    <div class="main">
      <header class="topbar">
        <span class="dot" :class="status.meta.tone"></span>
        <div class="status-block">
          <div class="status-line">
            <strong>{{ status.meta.label }}</strong>
            <span v-if="status.snapshot?.join.networkName" class="muted">
              · {{ status.snapshot.join.networkName }}
            </span>
            <span v-if="status.snapshot?.easyTier?.selfIp" class="muted mono">
              · {{ status.snapshot.easyTier.selfIp }}
            </span>
          </div>
          <div class="faint status-hint">{{ status.meta.hint }}</div>
        </div>
        <div class="topbar-actions">
          <n-button size="small" :loading="status.loading" @click="status.refresh()">刷新</n-button>
          <n-button
            size="small"
            type="primary"
            :loading="acting"
            @click="onPrimary"
          >
            {{ status.action.label }}
          </n-button>
        </div>
      </header>

      <n-alert
        v-if="status.offline"
        class="banner"
        type="warning"
        :bordered="false"
        title="后台服务未运行"
      >
        <div class="banner-body">
          <span>组网与配置由后台服务提供。安装并启动后本页会自动恢复。</span>
          <n-button
            v-if="local.canInstall"
            size="small"
            type="primary"
            ghost
            @click="installFromBanner"
          >
            安装并启动服务
          </n-button>
        </div>
      </n-alert>

      <main class="content">
        <RouterView />
      </main>
    </div>
  </div>
</template>

<style scoped>
.unauth {
  height: 100%;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
}

.shell {
  display: flex;
  height: 100%;
}

.sidebar {
  width: 208px;
  flex: 0 0 208px;
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 18px 12px;
  background: var(--color-canvas);
  border-right: 1px solid var(--color-border);
}

.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 0 8px;
}

.brand-mark {
  width: 10px;
  height: 22px;
  border-radius: 3px;
  background: var(--color-primary);
  box-shadow: 0 0 12px var(--color-primary-soft);
}

.brand-sub {
  font-size: 12px;
}

.nav {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.nav-item {
  padding: 8px 10px;
  border-radius: 8px;
  color: var(--color-dim);
  font-size: 14px;
  transition: background 0.12s ease;
}

.nav-item:hover {
  background: var(--color-card);
  color: var(--color-text);
}

.nav-item.router-link-active {
  background: var(--color-primary-soft);
  color: var(--color-text);
}

.sidebar-foot {
  margin-top: auto;
  padding: 0 8px;
  font-size: 12px;
}

.main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.topbar {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 20px;
  border-bottom: 1px solid var(--color-border);
  background: var(--color-bg);
}

.status-block {
  min-width: 0;
}

.status-line {
  display: flex;
  gap: 4px;
  align-items: baseline;
  font-size: 14px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.status-hint {
  font-size: 12px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.topbar-actions {
  margin-left: auto;
  display: flex;
  gap: 8px;
}

.dot {
  width: 9px;
  height: 9px;
  border-radius: 50%;
  flex: 0 0 9px;
  background: var(--color-faint);
}

.dot.ok {
  background: var(--color-success);
  box-shadow: 0 0 8px var(--color-success-soft);
}

.dot.warn {
  background: var(--color-warning);
}

.banner {
  margin: 12px 20px 0;
}

.banner-body {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.content {
  flex: 1;
  overflow: auto;
  padding: 20px;
}
</style>
