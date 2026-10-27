<script setup lang="ts">
/**
 * ServiceRegBinding — 部署节点「服务注册」区块(一键生成)。
 *
 * 选基域 + 填服务名(可选同基域额外子域名/别名;反代可填子路径;非容器反代再填端口;
 * 静态发布无端口)→ 一键生成:幂等注册服务并回填 regServiceId 绑定。绑定后显示路由预览,
 * 可解绑。配置键(写进 job.config,由后端部署联动消费):
 *   regDomainId / regServiceName / regServiceAliases / regPath / regPort / regStripPath / regServiceId(绑定结果)
 * 实例无需人工维护 —— 部署时按目标机自动注册(容器 = 自动分配宿主端口 + 扩缩容轮转;
 * 非容器 = 摘 → 升 → 挂回;静态发布无实例,内容由 deploy_static 节点下发到全部网关)。
 */
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { listDomains, ensureService, type Domain } from '../../api/servicereg'

const props = defineProps<{
  /** 是否容器部署(容器:端口由 ports 配置推导;非容器:用户填服务端口)。 */
  containerMode: boolean
  /** 是否网关静态发布(deploy_static:注册静态托管服务,无端口,内容发布到全部网关)。 */
  staticMode: boolean
  /** 当前 config 值(regDomainId / regServiceName / regPath / regPort / regStripPath / regServiceId)。 */
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
const regAliases = computed(() => (props.config.regServiceAliases ?? '').trim())
const regPath = computed(() => (props.config.regPath ?? '').trim())
const regPort = computed(() => (props.config.regPort ?? '').trim())
const stripPath = computed(() => (props.config.regStripPath ?? '').trim() === '1')
const boundId = computed(() => (props.config.regServiceId ?? '').trim())

const boundDomain = computed(
  () => domains.value.find((d) => d.id === domainId.value) ?? null,
)

/** 归一化子路径展示:空 = 根路径 /;确保以 / 开头。 */
function displayPath(p: string): string {
  const v = p.trim()
  if (!v) return '/'
  return v.startsWith('/') ? v : `/${v}`
}

/** 解析别名输入(逗号/分号/空白分隔 → trim → 小写 → 去重);空项静默丢弃。 */
function parseAliases(raw: string): string[] {
  const seen = new Set<string>()
  for (const part of raw.split(/[,;\s]+/)) {
    const v = part.trim().toLowerCase()
    if (v) seen.add(v)
  }
  return [...seen]
}

/** 路由预览 FQDN+path(已绑定时按绑定参数,未绑定时按当前输入);别名域名附加展示。 */
const fqdnPreview = computed(() => {
  if (!serviceName.value) return ''
  const base = boundDomain.value?.baseDomain ?? ''
  if (!base) return ''
  const main = `${serviceName.value}.${base}${displayPath(regPath.value)}`
  const aliases = parseAliases(regAliases.value)
  if (!aliases.length) return main
  return `${main} · ${aliases.map((a) => `${a}.${base}`).join(', ')}`
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
  // 端口仅反代形态需要(容器从 ports 推导;静态托管无端口)。
  if (!props.containerMode && !props.staticMode && !regPort.value) {
    genError.value = t('pipelineJob.regNeedPort')
    return
  }
  generating.value = true
  try {
    const aliases = parseAliases(regAliases.value)
    if (props.staticMode) {
      // 静态托管:无端口/上游,内容由本节点发布到全部网关主机。
      const res = await ensureService({
        domainId: domainId.value,
        name: serviceName.value,
        aliases,
        path: regPath.value,
        protocol: 'http',
        upstreamKind: 'static',
        upstream: '',
        upstreamPort: 0,
      })
      set('regServiceId', res.id)
      return
    }
    // 容器部署:服务端口从 ports 配置推导(首个容器端口),此处不重复填。
    const upstreamPort = props.containerMode
      ? deriveContainerPort()
      : Number.parseInt(regPort.value, 10)
    const res = await ensureService({
      domainId: domainId.value,
      name: serviceName.value,
      aliases,
      path: regPath.value,
      protocol: 'http',
      upstreamKind: props.containerMode ? 'container' : 'address',
      upstream: '',
      upstreamPort,
      stripPathPrefix: stripPath.value,
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
          :placeholder="staticMode ? 'app' : containerMode ? 'shop' : 'api'"
          :aria-label="t('pipelineJob.fieldRegServiceNameLabel')"
          @input="set('regServiceName', ($event.target as HTMLInputElement).value)"
        />
        <p v-if="fqdnPreview" class="field-hint">{{ fqdnPreview }}</p>
      </div>

      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineJob.fieldRegAliasesLabel') }}</div>
        <input
          class="drawer-input is-mono"
          type="text"
          :value="regAliases"
          placeholder="api-v2, api-old"
          :aria-label="t('pipelineJob.fieldRegAliasesLabel')"
          @input="set('regServiceAliases', ($event.target as HTMLInputElement).value)"
        />
        <p class="field-hint">{{ t('pipelineJob.fieldRegAliasesHint') }}</p>
      </div>

      <div class="drawer-field">
        <div class="drawer-field-label">{{ t('pipelineJob.fieldRegPathLabel') }}</div>
        <input
          class="drawer-input is-mono"
          type="text"
          :value="regPath"
          :placeholder="staticMode ? '/' : '/api'"
          :aria-label="t('pipelineJob.fieldRegPathLabel')"
          @input="set('regPath', ($event.target as HTMLInputElement).value)"
        />
        <p class="field-hint">
          {{ staticMode ? t('pipelineJob.fieldRegPathStaticHint') : t('pipelineJob.fieldRegPathHint') }}
        </p>
      </div>

      <div v-if="!containerMode && !staticMode" class="drawer-field">
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

      <label v-if="!containerMode && !staticMode && regPath" class="reg-strip">
        <input
          type="checkbox"
          :checked="stripPath"
          @change="set('regStripPath', ($event.target as HTMLInputElement).checked ? '1' : '')"
        />
        <span>{{ t('pipelineJob.fieldRegStripLabel') }}</span>
      </label>

      <button class="reg-generate" :disabled="generating" @click="generate">
        {{ generating ? t('pipelineJob.regGenerating') : t('pipelineJob.regGenerate') }}
      </button>
      <p class="field-hint">
        {{ staticMode ? t('pipelineJob.regGenerateStaticHint') : t('pipelineJob.regGenerateHint') }}
      </p>
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
            staticMode
              ? t('pipelineJob.regBoundHintStatic')
              : containerMode
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

.reg-strip {
  display: flex;
  align-items: center;
  gap: 7px;
  margin-top: 6px;
  font-size: 0.76rem;
  color: var(--color-dim);
  cursor: pointer;
}

.reg-strip input {
  accent-color: var(--color-primary);
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
