<script setup lang="ts">
import { computed, h, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { NButton, NCard, NDataTable, NEmpty, useMessage } from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import { useConfigStore } from '../stores/config'
import { useStatusStore } from '../stores/status'
import { useLocalStore } from '../stores/local'
import { describeConfig, type ConfigSummaryRow } from '../lib/configDraft'
import { orDash } from '../lib/format'

const router = useRouter()
const message = useMessage()
const config = useConfigStore()
const status = useStatusStore()
const local = useLocalStore()

const target: Record<string, string> = {
  enabled: '/overview',
  network: '/config/connection',
  auth: '/config/connection',
  virtualIp: '/config/connection',
  hostname: '/config/connection',
  peers: '/config/connection',
  easytier: '/config/connection',
  routes: '/routes',
  socks5: '/proxy',
  proxies: '/proxy',
  aliyun: '/aliyun',
}

const rows = computed(() => describeConfig(config.view, status.snapshot))

const columns: DataTableColumns<ConfigSummaryRow> = [
  { title: '项目', key: 'label', width: 130 },
  { title: '当前配置', key: 'configured' },
  { title: '实际状态', key: 'actual' },
  {
    title: '',
    key: 'key',
    width: 72,
    render: (row) =>
      h(
        NButton,
        {
          size: 'tiny',
          quaternary: true,
          onClick: () => router.push(target[row.key] ?? '/config/connection'),
        },
        () => '修改',
      ),
  },
]

onMounted(async () => {
  if (!config.view) await config.load()
})

async function copySummary(): Promise<void> {
  const text = rows.value
    .map((row) => `${row.label}\t${row.configured}\t${row.actual}`)
    .join('\n')
  try {
    await navigator.clipboard.writeText(text)
    message.success('已复制配置摘要（不含密钥）')
  } catch {
    message.error('复制失败（浏览器未授予剪贴板权限）')
  }
}
</script>

<template>
  <div class="page">
    <n-card title="当前配置" size="small">
      <template #header-extra>
        <n-button size="small" :disabled="rows.length === 0" @click="copySummary">
          复制摘要
        </n-button>
      </template>
      <n-data-table
        v-if="rows.length > 0"
        :columns="columns"
        :data="rows"
        :row-key="(row: ConfigSummaryRow) => row.key"
        :bordered="false"
        size="small"
      />
      <n-empty v-else description="服务未运行，暂时无法读取配置" />
    </n-card>

    <n-card title="本机路径" size="small">
      <div class="kv">
        <span class="faint">数据目录</span>
        <span class="mono path">{{ orDash(local.info?.dataDir ?? '') }}</span>
        <n-button size="tiny" quaternary @click="local.revealDataDir()">打开</n-button>
      </div>
      <div class="kv">
        <span class="faint">配置文件</span>
        <span class="mono path">{{ orDash(local.info?.configFile ?? '') }}</span>
      </div>
      <div class="kv">
        <span class="faint">日志文件</span>
        <span class="mono path">{{ orDash(local.info?.logFile ?? '') }}</span>
        <n-button size="tiny" quaternary @click="local.openLog()">打开</n-button>
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

.kv .path {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
