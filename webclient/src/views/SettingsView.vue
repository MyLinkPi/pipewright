<script setup lang="ts">
import { computed } from 'vue'
import {
  NButton,
  NCard,
  NRadioButton,
  NRadioGroup,
  NSwitch,
  NTag,
  useDialog,
  useMessage,
} from 'naive-ui'
import { useLocalStore } from '../stores/local'
import { useStatusStore } from '../stores/status'
import { useUiStore, type ThemeName } from '../stores/ui'
import { orDash } from '../lib/format'

const local = useLocalStore()
const status = useStatusStore()
const ui = useUiStore()
const dialog = useDialog()
const message = useMessage()

const service = computed(() => local.info?.service ?? null)

const serviceLabel = computed(() => {
  switch (service.value?.state) {
    case 'RUNNING':
      return '运行中'
    case 'STARTING':
      return '启动中'
    case 'STOPPING':
      return '停止中'
    case 'STOPPED':
      return '已停止'
    case 'NOT_INSTALLED':
      return '未安装'
    case 'UNSUPPORTED':
      return '当前平台不支持'
    default:
      return '未知'
  }
})

const serviceType = computed<'success' | 'warning' | 'error' | 'default'>(() => {
  if (service.value?.state === 'RUNNING') return 'success'
  if (service.value?.state === 'NOT_INSTALLED' || service.value?.state === 'UNSUPPORTED') {
    return 'default'
  }
  return 'warning'
})

const platformLabel = computed(() => {
  const platform = local.info?.platform ?? ''
  if (platform === 'windows') return 'Windows'
  if (platform === 'darwin') return 'macOS'
  if (platform === 'linux') return 'Linux'
  return orDash(platform)
})

function onTheme(value: string | number | boolean): void {
  ui.setTheme(String(value) as ThemeName)
}

async function toggleAutoStart(value: boolean): Promise<void> {
  try {
    await local.setAutoStart(value)
    message.success(value ? '已开启登录自启' : '已关闭登录自启')
  } catch (err) {
    message.error(err instanceof Error ? err.message : String(err))
  }
}

async function runService(action: 'install' | 'start' | 'stop'): Promise<void> {
  try {
    if (action === 'install') await local.installService()
    else await local.serviceAction(action)
    message.success(action === 'install' ? '已请求安装服务（如弹出 UAC 请确认）' : '已请求' + serviceLabel.value)
  } catch (err) {
    message.error(err instanceof Error ? err.message : String(err))
  }
}

function confirmUninstall(): void {
  dialog.warning({
    title: '卸载后台服务',
    content: '卸载后本机将停止组网（配置保留，可随时重新安装）。',
    positiveText: '卸载',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await local.serviceAction('uninstall')
        message.success('已请求卸载服务')
      } catch (err) {
        message.error(err instanceof Error ? err.message : String(err))
      }
    },
  })
}

function confirmQuit(): void {
  dialog.warning({
    title: '退出客户端',
    content: '仅关闭界面与托盘图标，不影响后台组网服务。',
    positiveText: '退出',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await local.quit()
      } catch (err) {
        message.error(err instanceof Error ? err.message : String(err))
      }
    },
  })
}
</script>

<template>
  <div class="page">
    <n-card title="本机与版本" size="small">
      <div class="kv">
        <span class="faint">平台</span><span>{{ platformLabel }}</span>
      </div>
      <div class="kv">
        <span class="faint">客户端版本</span><span>{{ orDash(status.snapshot?.clientVersion ?? '') }}</span>
      </div>
      <div class="kv">
        <span class="faint">界面版本</span><span>{{ orDash(local.info?.version ?? '') }}</span>
      </div>
      <div class="kv kv-path">
        <div class="kv-head">
          <span class="faint">数据目录</span>
          <n-button size="tiny" quaternary @click="local.revealDataDir()">打开</n-button>
        </div>
        <span class="mono path" :title="local.info?.dataDir ?? ''">{{ orDash(local.info?.dataDir ?? '') }}</span>
      </div>
      <div class="kv kv-path">
        <span class="faint">配置文件</span>
        <span class="mono path" :title="local.info?.configFile ?? ''">{{ orDash(local.info?.configFile ?? '') }}</span>
      </div>
      <div class="kv kv-path">
        <div class="kv-head">
          <span class="faint">日志文件</span>
          <n-button size="tiny" quaternary @click="local.openLog()">打开</n-button>
        </div>
        <span class="mono path" :title="local.info?.logFile ?? ''">{{ orDash(local.info?.logFile ?? '') }}</span>
      </div>
    </n-card>

    <n-card v-if="local.canInstall" title="后台服务" size="small">
      <template #header-extra>
        <n-tag size="small" :type="serviceType">{{ serviceLabel }}</n-tag>
      </template>
      <p class="faint hint">
        后台服务以系统级运行，开机即组网（无需登录后再启动托盘）。安装/卸载需要管理员权限。
      </p>
      <div class="actions">
        <n-button
          v-if="service?.state === 'NOT_INSTALLED'"
          size="small"
          type="primary"
          :loading="local.busy === 'install'"
          @click="runService('install')"
        >
          安装并启动服务
        </n-button>
        <template v-else>
          <n-button
            size="small"
            :disabled="service?.state === 'RUNNING'"
            :loading="local.busy === 'start'"
            @click="runService('start')"
          >
            启动
          </n-button>
          <n-button
            size="small"
            :disabled="service?.state !== 'RUNNING'"
            :loading="local.busy === 'stop'"
            @click="runService('stop')"
          >
            停止
          </n-button>
          <n-button size="small" type="error" ghost @click="confirmUninstall">卸载</n-button>
        </template>
      </div>
      <div v-if="local.error" class="err">{{ local.error }}</div>
    </n-card>

    <n-card title="外观与启动" size="small">
      <div class="kv">
        <span class="faint">主题</span>
        <n-radio-group :value="ui.theme" size="small" @update:value="onTheme">
          <n-radio-button value="dark">深色</n-radio-button>
          <n-radio-button value="light">浅色</n-radio-button>
        </n-radio-group>
      </div>
      <div v-if="local.isWindows" class="kv">
        <span class="faint">登录自启（托盘）</span>
        <n-switch
          :value="local.info?.autoStart ?? false"
          :loading="local.busy === 'autostart'"
          @update:value="toggleAutoStart"
        />
      </div>
    </n-card>

    <n-card title="退出" size="small">
      <p class="faint hint">关闭界面与托盘图标不会影响后台组网；下次双击客户端可重新打开。</p>
      <div class="actions">
        <n-button size="small" @click="confirmQuit">退出客户端</n-button>
      </div>
    </n-card>
  </div>
</template>

<style scoped>
.page {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.kv {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 5px 0;
  font-size: 14px;
}

.kv-path {
  flex-direction: column;
  align-items: stretch;
  gap: 4px;
}

.kv-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.hint {
  margin: 0 0 12px;
  font-size: 13px;
}

.actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}

.err {
  margin-top: 10px;
  color: var(--color-danger);
  font-size: 13px;
}
</style>
