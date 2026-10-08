<script setup lang="ts">
/**
 * ServiceRegBinding — 部署节点「服务注册」区块(一键生成)。
 *
 * 选基域 + 填服务名(非容器再填端口)→ 一键生成:幂等注册服务并回填 regServiceId 绑定。
 * 绑定后显示 FQDN 预览,可解绑。配置键(写进 job.config,由后端部署联动消费):
 *   regDomainId / regServiceName / regPort / regServiceId(绑定结果)
 * 实例无需人工维护 —— 部署时按目标机自动注册(容器 = 自动分配宿主端口 + 扩缩容轮转;
 * 非容器 = 摘 → 升 → 挂回)。
 */
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { listDomains, ensureService, type Domain } from '../../api/servicereg'

const props = defineProps<{
  /** 是否容器部署(容器:端口由 ports 配置推导;非容器:用户填服务端口)。 */
  containerMode: boolean
  /** 当前 config 值(regDomainId / regServiceName / regPort / regServiceId)。 */
  config: Record<string, string>
}>()

const emit = defineEmits<{
  /** 就地改一个键(空值 = 删除键)。 */
  (e: 'set', key: string, value: string): void
}>()

const { t } = useI18n()

const domains = ref<Domain[]>([])
const domainsError = ref('')
const generating = ref(false)
const genError = ref('')

const domainId = computed(() => (props.config.regDomainId ?? '').trim())
const serviceName = computed(() => (props.config.regServiceName ?? '').trim())
const regPort = computed(() => (props.config.regPort ?? '').trim())
const boundId = computed(() => (props.config.regServiceId ?? '').trim())

const boundDomain = computed(
  () => domains.value.find((d) => d.id === domainId.value) ?? null,
)
/** FQDN 预览(已绑定时按绑定参数,未绑定时按当前输入)。 */
const fqdnPreview = computed(() => {
  if (!serviceName.value) return ''
  const base = boundDomain.value?.baseDomain ?? ''
  return base ? `${serviceName.value}.${base}` : ''
})

onMounted(async () => {
  try {
    const res = await listDomains()
    domains.value = res.items ?? []
  } catch (err) {
    domainsError.value =
      err instanceof Error && err.message ? err.message : t('pipelineJob.regDomainsLoadFailed')
  }
})

function set(key: string, value: string): void {
  emit('set', key, value.trim())
}

async function generate(): Promise<void> {
  genError.value = ''
  if (!domainId.value || !serviceName.value) {
    genError.value = t('pipelineJob.regNeedDomainAndName')
    return
  }
  if (!props.containerMode && !regPort.value) {
    genError.value = t('pipelineJob.regNeedPort')
    return
  }
  generating.value = true
  try {
    // 容器部署:服务端口从 ports 配置推导(首个容器端口),此处不重复填。
    const upstreamPort = props.containerMode
      ? deriveContainerPort()
      : Number.parseInt(regPort.value, 10)
    const res = await ensureService({
      domainId: domainId.value,
      name: serviceName.value,
      protocol: 'http',
      upstreamKind: props.containerMode ? 'container' : 'address',
      upstream: '',
      upstreamPort,
    })
    set('regServiceId', res.id)
  } catch (err) {
    genError.value =
      err instanceof Error && err.message ? err.message : t('pipelineJob.regGenerateFailed')
  } finally {
    generating.value = false
  }
}

/** 从 ports 配置推导容器服务端口(如 "8080:80, 9000" → 80)。 */
function deriveContainerPort(): number {
  const first = (props.config.ports ?? '')
    .split(/[,;\s]+/)
    .map((s) => s.trim())
    .filter(Boolean)[0] ?? ''
  const containerPart = first.includes(':') ? first.split(':').pop() ?? '' : first
  const n = Number.parseInt(containerPart.replace(/^auto:/i, ''), 10)
  return Number.isFinite(n) && n > 0 ? n : 80
}

function unbind(): void {
  emit('set', 'regServiceId', '')
}
</script>

<template>
  <div class="reg-binding">
    <template v-if="!boundId">
      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineJob.fieldRegDomainLabel') }}</div>
        <select
          class="drawer-select"
          :value="domainId"
          :aria-label="t('pipelineJob.fieldRegDomainLabel')"
          @change="set('regDomainId', ($event.target as HTMLSelectElement).value)"
        >
          <option value="">{{ t('pipelineJob.regDomainUnselected') }}</option>
          <option v-for="d in domains" :key="d.id" :value="d.id">{{ d.baseDomain }}</option>
        </select>
        <p v-if="domainsError" class="reg-error" role="alert">{{ domainsError }}</p>
      </div>

      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineJob.fieldRegServiceNameLabel') }}</div>
        <input
          class="drawer-input is-mono"
          type="text"
          :value="serviceName"
          :placeholder="containerMode ? 'shop' : 'api'"
          :aria-label="t('pipelineJob.fieldRegServiceNameLabel')"
          @input="set('regServiceName', ($event.target as HTMLInputElement).value)"
        />
        <p v-if="fqdnPreview" class="field-hint">{{ fqdnPreview }}</p>
      </div>

      <div v-if="!containerMode" class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineJob.fieldRegPortLabel') }}</div>
        <input
          class="drawer-input is-mono"
          type="number"
          :value="regPort"
          placeholder="8080"
          :aria-label="t('pipelineJob.fieldRegPortLabel')"
          @input="set('regPort', ($event.target as HTMLInputElement).value)"
        />
        <p class="field-hint">{{ t('pipelineJob.fieldRegPortHint') }}</p>
      </div>

      <button class="reg-generate" :disabled="generating" @click="generate">
        {{ generating ? t('pipelineJob.regGenerating') : t('pipelineJob.regGenerate') }}
      </button>
      <p class="field-hint">{{ t('pipelineJob.regGenerateHint') }}</p>
      <p v-if="genError" class="reg-error" role="alert">{{ genError }}</p>
    </template>

    <template v-else>
      <div class="reg-bound">
        <div class="reg-bound-row">
          <span class="reg-bound-fqdn is-mono">{{ fqdnPreview || serviceName }}</span>
          <button class="reg-unbind" @click="unbind">
            {{ t('pipelineJob.regUnbind') }}
          </button>
        </div>
        <p class="field-hint">
          {{
            containerMode
              ? t('pipelineJob.regBoundHintContainer')
              : t('pipelineJob.regBoundHintHost')
          }}
        </p>
      </div>
    </template>
  </div>
</template>

<style scoped>
.reg-binding {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.reg-generate {
  align-self: flex-start;
  margin-top: 4px;
  padding: 7px 14px;
  background: var(--color-primary);
  border: 1px solid var(--color-primary);
  border-radius: var(--rounded);
  color: #fff;
  font: inherit;
  font-size: 0.8rem;
  font-weight: 600;
  cursor: pointer;
  transition: opacity var(--duration-fast);
}

.reg-generate:hover:not(:disabled) {
  opacity: 0.9;
}

.reg-generate:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}

.reg-error {
  margin: 5px 0 0;
  font-size: 0.74rem;
  color: var(--color-red, #d33);
}

.reg-bound {
  padding: 10px 12px;
  background: var(--color-inset);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
}

.reg-bound-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}

.reg-bound-fqdn {
  font-size: 0.82rem;
  font-weight: 650;
  color: var(--color-primary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.reg-unbind {
  flex-shrink: 0;
  padding: 4px 10px;
  background: none;
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  color: var(--color-dim);
  font: inherit;
  font-size: 0.74rem;
  font-weight: 600;
  cursor: pointer;
}

.reg-unbind:hover {
  color: var(--color-text);
  border-color: var(--color-text);
}

.field-hint {
  margin: 5px 0 0;
  font-size: 0.72rem;
  color: var(--color-faint);
  line-height: 1.4;
}

.is-mono {
  font-family: var(--font-mono);
  font-size: 0.8rem;
}
</style>
