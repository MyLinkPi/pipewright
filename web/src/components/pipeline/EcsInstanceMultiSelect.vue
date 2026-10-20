<script setup lang="ts">
/*
  EcsInstanceMultiSelect.vue — ECS 开关机节点的实例多选(JobDrawer ecsInstances 字段控件)。

  候选随所选阿里云账户联动拉取(GET /api/aliyun/accounts/{id}/instances);选中集序列化为
  逗号串单键(modelValue = "i-1,i-2",遵循流水线 config Record<string,string> 冻结合同,
  同 mesh_push items 先例)。未选账户时提示先选账户;拉取失败可重试。
  快照语义:选中项保存的是配置时实例 ID;实例释放后运行时由后端逐台报「实例不存在」。
*/
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { listAliyunInstances, type AliyunEcsInstance } from '../../api/aliyunEcs'
import { HttpError } from '../../api/http'

const props = defineProps<{
  /** 阿里云账户 id(空 = 未选账户,提示先选)。 */
  accountId: string
  /** 逗号串选中的实例 ID。 */
  modelValue: string
}>()

const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

const { t } = useI18n()
const instances = ref<AliyunEcsInstance[]>([])
const state = ref<'idle' | 'loading' | 'error' | 'ready'>('idle')
const error = ref('')

const selected = computed<Set<string>>(
  () =>
    new Set(
      props.modelValue
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean),
    ),
)

async function load(): Promise<void> {
  if (!props.accountId) return
  state.value = 'loading'
  error.value = ''
  try {
    instances.value = await listAliyunInstances(props.accountId)
    state.value = 'ready'
  } catch (err) {
    state.value = 'error'
    error.value = err instanceof HttpError ? (err.apiError?.message ?? '') : ''
  }
}

watch(
  () => props.accountId,
  () => {
    instances.value = []
    state.value = 'idle'
    void load()
  },
  { immediate: true },
)

function emitNext(next: Set<string>): void {
  const ids = instances.value.map((i) => i.instanceId).filter((id) => next.has(id))
  // 账户切换后可能残留快照 ID(不在当前列表):保留用户显式选中的、丢弃列表外的旧值,
  // 保证值与列表一致(后端运行时仍会校验存在性)。
  emit('update:modelValue', ids.join(','))
}

function toggle(id: string): void {
  const next = new Set(selected.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  emitNext(next)
}

const allSelected = computed(
  () => instances.value.length > 0 && selected.value.size >= instances.value.length,
)

function selectAll(): void {
  emitNext(new Set(instances.value.map((i) => i.instanceId)))
}

function clearAll(): void {
  emit('update:modelValue', '')
}
</script>

<template>
  <div class="ecs-multi" :aria-label="t('pipelineJob.fieldEcsInstancesLabel')">
    <p v-if="!accountId" class="ecs-multi-hint">{{ t('pipelineJob.ecsNeedAccount') }}</p>
    <template v-else>
      <div class="ecs-multi-bar">
        <span class="ecs-multi-count">{{ t('pipelineJob.ecsSelectedCount', { n: selected.size, total: instances.length }) }}</span>
        <button type="button" class="ecs-link" :disabled="allSelected" @click="selectAll">
          {{ t('pipelineJob.ecsSelectAll') }}
        </button>
        <button type="button" class="ecs-link" :disabled="selected.size === 0" @click="clearAll">
          {{ t('pipelineJob.ecsClearSelection') }}
        </button>
      </div>
      <p v-if="state === 'loading'" class="ecs-multi-hint">{{ t('pipelineJob.ecsInstancesLoading') }}</p>
      <p v-else-if="state === 'error'" class="ecs-multi-hint ecs-multi-err">
        {{ error || t('pipelineJob.ecsInstancesErr') }}
        <button type="button" class="ecs-link" @click="load">{{ t('common.refresh') }}</button>
      </p>
      <p v-else-if="state === 'ready' && instances.length === 0" class="ecs-multi-hint">
        {{ t('pipelineJob.ecsInstancesEmpty') }}
      </p>
      <div v-else-if="state === 'ready'" class="ecs-multi-list">
        <label v-for="inst in instances" :key="inst.instanceId" class="ecs-row" :class="{ 'ecs-row--sel': selected.has(inst.instanceId) }">
          <input
            type="checkbox"
            :checked="selected.has(inst.instanceId)"
            :aria-label="inst.name || inst.instanceId"
            @change="toggle(inst.instanceId)"
          />
          <span class="ecs-name">{{ inst.name || inst.instanceId }}</span>
          <span class="ecs-id">{{ inst.instanceId }}</span>
          <span class="ecs-ip">{{ inst.privateIps[0] || inst.publicIps[0] || '—' }}</span>
          <span class="ecs-status" :class="inst.status === 'Running' ? 'ecs-status--run' : ''">{{ inst.status }}</span>
        </label>
      </div>
    </template>
  </div>
</template>

<style scoped>
.ecs-multi {
  display: flex;
  flex-direction: column;
  gap: 8px;
  width: 100%;
}
.ecs-multi-bar {
  display: flex;
  align-items: center;
  gap: 12px;
}
.ecs-multi-count {
  font-size: 0.76rem;
  color: var(--color-faint);
}
.ecs-multi-hint {
  font-size: 0.76rem;
  color: var(--color-faint);
}
.ecs-multi-err {
  color: var(--color-red);
}
.ecs-link {
  background: none;
  border: none;
  padding: 0;
  font-size: 0.74rem;
  color: var(--color-primary);
  cursor: pointer;
}
.ecs-link:disabled {
  color: var(--color-faint);
  cursor: not-allowed;
}
.ecs-multi-list {
  max-height: 220px;
  overflow-y: auto;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
}
.ecs-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  font-size: 0.76rem;
  cursor: pointer;
  border-top: 1px solid var(--color-border);
}
.ecs-row:first-child {
  border-top: none;
}
.ecs-row--sel {
  background: var(--color-primary-soft);
}
.ecs-row input {
  accent-color: var(--color-primary);
  flex-shrink: 0;
}
.ecs-name {
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ecs-id,
.ecs-ip {
  color: var(--color-faint);
  font-family: var(--font-mono);
  font-size: 0.7rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ecs-ip {
  margin-left: auto;
}
.ecs-status {
  flex-shrink: 0;
  color: var(--color-faint);
  font-size: 0.68rem;
}
.ecs-status--run {
  color: var(--color-green);
}
</style>
