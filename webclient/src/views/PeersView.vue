<script setup lang="ts">
import { computed } from 'vue'
import { NCard, NDataTable, NEmpty } from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import { useStatusStore } from '../stores/status'
import { peerRows, type PeerRow } from '../lib/derive'

const status = useStatusStore()
const rows = computed(() => peerRows(status.snapshot?.easytier?.links))

const columns: DataTableColumns<PeerRow> = [
  { title: '虚拟 IP', key: 'virtualIp', width: 150, render: (row) => row.virtualIp },
  { title: '主机名', key: 'hostname', render: (row) => row.hostname },
  { title: '延迟', key: 'latency', width: 100, render: (row) => row.latency },
  { title: '链路', key: 'kind', width: 200, render: (row) => row.kind },
  { title: '版本', key: 'version', width: 120, render: (row) => row.version },
]
</script>

<template>
  <n-card title="在线对端" size="small">
    <template #header-extra>
      <span class="faint">{{ rows.length }} 个</span>
    </template>
    <n-data-table
      v-if="rows.length > 0"
      :columns="columns"
      :data="rows"
      :row-key="(row: PeerRow) => row.key"
      :bordered="false"
      size="small"
    />
    <n-empty v-else description="暂无在线对端（组网未连接或集群无其他节点）" />
  </n-card>
</template>
