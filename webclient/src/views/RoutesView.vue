<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { NButton, NCard, NEmpty, NInput, NSwitch, NTag, useMessage } from 'naive-ui'
import { useConfigStore } from '../stores/config'
import { useStatusStore } from '../stores/status'
import { routeRows } from '../lib/derive'
import { buildRoutesPatch } from '../lib/configDraft'
import { validateRoutes } from '../lib/validate'

const config = useConfigStore()
const status = useStatusStore()
const message = useMessage()

const draft = ref<string[]>([])
const baseline = ref<string[]>([])
const manual = ref('')
const saving = ref(false)

function cleaned(list: string[]): string[] {
  return list.map((v) => v.trim()).filter((v) => v !== '')
}

function syncDraft(): void {
  const routes = config.view?.routes ?? []
  baseline.value = [...routes]
  draft.value = [...routes]
}

onMounted(async () => {
  if (!config.view) await config.load()
  syncDraft()
})

const available = computed(() => routeRows(status.snapshot))
const dirty = computed(
  () => JSON.stringify(cleaned(draft.value)) !== JSON.stringify(cleaned(baseline.value)),
)

function isEnabled(cidr: string): boolean {
  return draft.value.includes(cidr)
}

function toggle(cidr: string, on: boolean): void {
  if (on) {
    if (!draft.value.includes(cidr)) draft.value = [...draft.value, cidr]
  } else {
    draft.value = draft.value.filter((v) => v !== cidr)
  }
}

function addManual(): void {
  const cidr = manual.value.trim()
  if (!cidr) return
  const err = validateRoutes([cidr])
  if (err) {
    message.error(err)
    return
  }
  if (draft.value.includes(cidr)) {
    message.info('该网段已在列表中')
    return
  }
  draft.value = [...draft.value, cidr]
  manual.value = ''
}

function removeManual(cidr: string): void {
  draft.value = draft.value.filter((v) => v !== cidr)
}

async function save(): Promise<void> {
  const err = validateRoutes(draft.value)
  if (err) {
    message.error(err)
    return
  }
  saving.value = true
  try {
    await config.save(buildRoutesPatch(draft.value, baseline.value))
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
    <n-card title="集群通告的网段" size="small">
      <template #header-extra>
        <span class="faint">勾选 = 本机经集群隧道路由该网段</span>
      </template>
      <div v-if="available.length > 0" class="rows">
        <div v-for="row in available" :key="row.cidr" class="row">
          <span class="mono cidr">{{ row.cidr }}</span>
          <span class="faint gateway">网关 {{ row.gateway }}</span>
          <n-tag v-if="row.backups > 0" size="small" type="info">+{{ row.backups }} 备</n-tag>
          <n-switch :value="isEnabled(row.cidr)" @update:value="(v: boolean) => toggle(row.cidr, v)" />
        </div>
      </div>
      <n-empty v-else description="集群尚未通告任何网段" />
    </n-card>

    <n-card title="本机路由的网段" size="small">
      <template #header-extra>
        <n-tag v-if="dirty" size="small" type="warning">有未保存修改</n-tag>
      </template>
      <p class="faint hint">
        留空 = 不路由任何通告网段（仅虚拟网互通）。也可手动添加通告列表之外的 CIDR。
      </p>
      <div v-if="draft.length > 0" class="rows">
        <div v-for="cidr in draft" :key="cidr" class="row">
          <span class="mono cidr">{{ cidr }}</span>
          <n-button size="tiny" quaternary @click="removeManual(cidr)">移除</n-button>
        </div>
      </div>
      <n-empty v-else description="当前不路由任何通告网段" />
      <div class="add">
        <n-input
          v-model:value="manual"
          placeholder="手动添加 CIDR，如 192.168.30.0/24"
          @keyup.enter="addManual"
        />
        <n-button @click="addManual">添加</n-button>
      </div>
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

.rows {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 8px 10px;
  border-radius: 8px;
  background: var(--color-inset);
}

.cidr {
  min-width: 170px;
}

.gateway {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.hint {
  margin: 0 0 10px;
  font-size: 13px;
}

.add {
  display: flex;
  gap: 8px;
  margin-top: 12px;
}

.actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
</style>
