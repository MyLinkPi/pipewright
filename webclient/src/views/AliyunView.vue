<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import {
  NButton,
  NCard,
  NDataTable,
  NEmpty,
  NInput,
  NTag,
  useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import { useConfigStore } from '../stores/config'
import { useStatusStore } from '../stores/status'
import { aliyunDraft, buildAliyunPatch, type AliyunDraft } from '../lib/configDraft'
import { validateSecGroups } from '../lib/validate'
import type { SecGroupItemStatus } from '../api/types'
import { formatTime, orDash, shortError } from '../lib/format'

const config = useConfigStore()
const status = useStatusStore()
const message = useMessage()

const draft = ref<AliyunDraft>({ accessKeyId: '', accessKeySecret: '', groups: [] })
const base = ref<AliyunDraft>({ accessKeyId: '', accessKeySecret: '', groups: [] })
const saving = ref(false)

function cloneDraft(source: AliyunDraft): AliyunDraft {
  return {
    accessKeyId: source.accessKeyId,
    accessKeySecret: '',
    groups: source.groups.map((g) => ({ ...g })),
  }
}

function syncDraft(): void {
  const view = config.view
  if (!view) return
  const next = aliyunDraft(view.aliyun)
  draft.value = cloneDraft(next)
  base.value = cloneDraft(next)
}

onMounted(async () => {
  if (!config.view) await config.load()
  syncDraft()
})

const dirty = computed(
  () => Object.keys(buildAliyunPatch(draft.value, base.value)).length > 0,
)
const syncState = computed(() => status.snapshot?.securityGroup ?? null)
const results = computed<SecGroupItemStatus[]>(() => syncState.value?.results ?? [])

function addGroup(): void {
  draft.value.groups = [
    ...draft.value.groups,
    { regionId: '', securityGroupId: '', description: '' },
  ]
}

function removeGroup(index: number): void {
  draft.value.groups = draft.value.groups.filter((_, i) => i !== index)
}

function statusLabel(code: string): string {
  return (
    {
      update: '已更新',
      skip: '最新',
      warn: '无匹配规则',
      fail: '失败',
      pending: '待同步',
    }[code] ?? code
  )
}

function statusType(code: string): 'success' | 'warning' | 'error' | 'default' {
  if (code === 'update' || code === 'skip') return 'success'
  if (code === 'warn' || code === 'pending') return 'warning'
  if (code === 'fail') return 'error'
  return 'default'
}

const resultColumns: DataTableColumns<SecGroupItemStatus> = [
  { title: '地域', key: 'regionId', width: 130, render: (row) => row.regionId },
  { title: '安全组', key: 'securityGroupId', width: 160, render: (row) => row.securityGroupId },
  { title: '规则描述', key: 'description', render: (row) => row.description },
  {
    title: '结果',
    key: 'status',
    width: 110,
    render: (row) => h(NTag, { size: 'small', type: statusType(row.status) }, () => statusLabel(row.status)),
  },
  { title: '错误', key: 'lastErr', render: (row) => orDash(shortError(row.lastErr)) },
]

async function save(): Promise<void> {
  const err = validateSecGroups(draft.value.groups)
  if (err) {
    message.error(err)
    return
  }
  saving.value = true
  try {
    await config.save(buildAliyunPatch(draft.value, base.value))
    syncDraft()
    await status.refresh()
    message.success('已保存并应用')
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page">
    <n-card title="阿里云安全组（可选）" size="small">
      <template #header-extra>
        <n-tag v-if="dirty" size="small" type="warning">有未保存修改</n-tag>
      </template>
      <p class="faint hint">
        连接/重连前自动把本机公网 IPv4 更新到下列安全组的既有规则：按规则描述精确匹配，
        协议/端口沿用，先授权新 IP 再回收旧 IP；连接期间每 5 分钟复查自愈。
        须先在阿里云控制台按所填描述创建放行规则（该描述在组内唯一）。
      </p>

      <div class="form">
        <label class="field">
          <span class="faint">AccessKeyId</span>
          <n-input v-model:value="draft.accessKeyId" placeholder="LTAI..." />
        </label>
        <label class="field">
          <span class="faint">AccessKeySecret</span>
          <n-input
            v-model:value="draft.accessKeySecret"
            type="password"
            show-password-on="click"
            :placeholder="config.view?.aliyun.hasSecret ? '已配置，留空保持不变' : '未配置'"
          />
        </label>
      </div>

      <div class="field-head">
        <span class="faint">安全组列表（地域 / 安全组ID / 规则描述）</span>
        <n-button size="tiny" @click="addGroup">添加安全组</n-button>
      </div>
      <div v-if="draft.groups.length > 0" class="groups">
        <div v-for="(group, index) in draft.groups" :key="index" class="group-row">
          <n-input v-model:value="group.regionId" placeholder="cn-hangzhou" class="region" />
          <n-input v-model:value="group.securityGroupId" placeholder="sg-xxx" class="sg" />
          <n-input v-model:value="group.description" placeholder="规则描述（如 pwclient-home）" class="grow" />
          <n-button size="small" quaternary @click="removeGroup(index)">删除</n-button>
        </div>
      </div>
      <div v-else class="faint hint">未配置安全组</div>

      <template #action>
        <div class="actions">
          <n-button size="small" @click="syncDraft">重置</n-button>
          <n-button size="small" type="primary" :loading="saving" :disabled="!dirty" @click="save">
            保存并应用
          </n-button>
        </div>
      </template>
    </n-card>

    <n-card title="最近同步" size="small">
      <div class="kv">
        <span class="faint">状态</span>
        <span>{{ syncState?.configured ? '已生效' : '未生效（须同时配置 AK/Secret 且至少一组）' }}</span>
      </div>
      <div class="kv">
        <span class="faint">最近执行</span>
        <span>{{ syncState?.lastRun ? formatTime(syncState.lastRun) : '尚未同步' }}</span>
      </div>
      <div class="kv">
        <span class="faint">公网 IP</span>
        <span class="mono">{{ orDash(syncState?.lastIp ?? '') }}</span>
      </div>
      <div v-if="syncState?.lastErr" class="err">{{ shortError(syncState.lastErr) }}</div>
      <n-data-table
        v-if="results.length > 0"
        class="results"
        :columns="resultColumns"
        :data="results"
        :row-key="(row: SecGroupItemStatus) => `${row.regionId}-${row.securityGroupId}-${row.description}`"
        :bordered="false"
        size="small"
      />
      <n-empty v-else description="尚无同步结果" />
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
  margin: 0 0 10px;
  font-size: 13px;
}

.form {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 12px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
}

.field-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin: 14px 0 6px;
  font-size: 13px;
}

.groups {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.group-row {
  display: flex;
  gap: 8px;
  align-items: center;
}

.region {
  width: 140px;
  flex: 0 0 140px;
}

.sg {
  width: 160px;
  flex: 0 0 160px;
}

.grow {
  flex: 1;
}

.kv {
  display: flex;
  gap: 12px;
  padding: 4px 0;
  font-size: 14px;
}

.err {
  margin-top: 6px;
  color: var(--color-danger);
  font-size: 13px;
}

.results {
  margin-top: 12px;
}

.actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
</style>
