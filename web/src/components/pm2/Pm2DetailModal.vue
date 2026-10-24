<!--
  Pm2DetailModal.vue — pm2 进程详情弹窗(仿 ContainerInspectModal)。
  直接展示行数据(jlist 已带全量字段,无额外请求):基本信息 / 运行配置 / 日志文件路径。
  Escape 关闭;scrim z-index 500(与现有弹窗一致)。
-->
<script setup lang="ts">
import { onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Pm2Process } from '../../api/pm2'
import { pm2StateMeta, pm2UptimeLabel } from '../../lib/pm2State'

defineProps<{
  process: Pm2Process
}>()

const emit = defineEmits<{ (e: 'close'): void }>()

const { t } = useI18n()

function onKeydown(e: KeyboardEvent): void {
  if (e.key === 'Escape') emit('close')
}

onMounted(() => {
  window.addEventListener('keydown', onKeydown)
})
onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown)
})

function tsLabel(ms: number): string {
  return ms ? new Date(ms).toLocaleString() : '—'
}
</script>

<template>
  <div class="pd-backdrop">
    <div class="pd-modal" role="dialog" aria-modal="true" aria-labelledby="pd-title">
      <header class="pd-head">
        <div class="pd-head-text">
          <h3 id="pd-title" class="pd-title">{{ t('pm2.detailTitleFull') }}</h3>
          <span class="pd-sub mono">{{ process.name }} · pm_id {{ process.id }}</span>
        </div>
        <button type="button" class="pd-close" :aria-label="t('pm2.detailClose')" @click="emit('close')">×</button>
      </header>

      <div class="pd-body">
        <!-- 基本信息 -->
        <section class="pd-section">
          <h4 class="pd-section-title">{{ t('pm2.detailBasic') }}</h4>
          <dl class="pd-kv">
            <dt>{{ t('pm2.detailName') }}</dt>
            <dd class="mono">{{ process.name }}</dd>
            <dt>pm_id</dt>
            <dd class="mono">{{ process.id }}</dd>
            <dt>PID</dt>
            <dd class="mono">{{ process.pid || '—' }}</dd>
            <dt>{{ t('pm2.detailStatus') }}</dt>
            <dd><span :class="`txt--${pm2StateMeta(process.status).tone}`">{{ pm2StateMeta(process.status).label }}</span></dd>
            <dt>{{ t('pm2.detailUptime') }}</dt>
            <dd class="mono">{{ pm2UptimeLabel(process.uptime) || '—' }}</dd>
            <dt>{{ t('pm2.detailRestarts') }}</dt>
            <dd class="mono">{{ process.restarts }}</dd>
            <dt>{{ t('pm2.detailCreatedAt') }}</dt>
            <dd class="mono">{{ tsLabel(process.createdAt) }}</dd>
          </dl>
        </section>

        <!-- 运行配置 -->
        <section class="pd-section">
          <h4 class="pd-section-title">{{ t('pm2.detailRuntime') }}</h4>
          <dl class="pd-kv">
            <dt>{{ t('pm2.detailScript') }}</dt>
            <dd class="mono">{{ process.script || '—' }}</dd>
            <dt>{{ t('pm2.detailCwd') }}</dt>
            <dd class="mono">{{ process.cwd || '—' }}</dd>
            <dt>{{ t('pm2.detailExecMode') }}</dt>
            <dd class="mono">{{ process.execMode || '—' }}</dd>
            <dt>{{ t('pm2.detailInstances') }}</dt>
            <dd class="mono">{{ process.instances || '—' }}</dd>
            <dt>{{ t('pm2.detailInterpreter') }}</dt>
            <dd class="mono">{{ process.interpreter || '—' }}</dd>
          </dl>
        </section>

        <!-- 资源与日志文件 -->
        <section class="pd-section">
          <h4 class="pd-section-title">{{ t('pm2.detailLogsSection') }}</h4>
          <dl class="pd-kv">
            <dt>CPU</dt>
            <dd class="mono">{{ process.cpu.toFixed(1) }}%</dd>
            <dt>{{ t('pm2.detailMemory') }}</dt>
            <dd class="mono">{{ process.memory }} B</dd>
            <dt>{{ t('pm2.detailOutLog') }}</dt>
            <dd class="mono pd-path">{{ process.outLog || '—' }}</dd>
            <dt>{{ t('pm2.detailErrLog') }}</dt>
            <dd class="mono pd-path">{{ process.errLog || '—' }}</dd>
          </dl>
        </section>
      </div>
    </div>
  </div>
</template>

<style scoped>
.pd-backdrop {
  position: fixed;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  background: rgba(15, 23, 42, 0.55);
  z-index: 500;
  padding: 24px;
}

.pd-modal {
  width: min(720px, 100%);
  max-height: min(82vh, 760px);
  display: flex;
  flex-direction: column;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-lg, 12px);
  box-shadow: var(--shadow-modal);
  overflow: hidden;
}

.pd-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 14px 18px;
  border-bottom: 1px solid var(--color-border);
  background: var(--color-card-2);
}
.pd-head-text {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.pd-title {
  margin: 0;
  font-size: var(--text-section);
  font-weight: 700;
  color: var(--color-text);
}
.pd-sub {
  font-size: var(--text-micro);
  color: var(--color-faint);
}
.pd-close {
  flex-shrink: 0;
  width: 28px;
  height: 28px;
  border-radius: var(--rounded-sm);
  border: 1px solid var(--color-border);
  background: transparent;
  color: var(--color-dim);
  font-size: 16px;
  line-height: 1;
  cursor: pointer;
  transition: all var(--duration-fast) var(--ease-out-expo);
}
.pd-close:hover {
  color: var(--color-text);
  border-color: var(--color-text);
}

.pd-body {
  overflow: auto;
  padding: 4px 0 14px;
}
.pd-section {
  padding: 12px 18px 0;
}
.pd-section-title {
  margin: 0 0 10px;
  font-size: var(--text-label);
  font-weight: 700;
  color: var(--color-dim);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.pd-kv {
  display: grid;
  grid-template-columns: minmax(96px, auto) minmax(0, 1fr);
  gap: 7px 16px;
  margin: 0;
}
.pd-kv dt {
  font-size: var(--text-label);
  color: var(--color-faint);
  white-space: nowrap;
}
.pd-kv dd {
  margin: 0;
  font-size: var(--text-label);
  color: var(--color-text);
  min-width: 0;
  overflow-wrap: anywhere;
}
.pd-path {
  color: var(--color-dim);
}
.txt--green { color: var(--color-green); }
.txt--amber { color: var(--color-amber); }
.txt--red { color: var(--color-red); }
.txt--faint { color: var(--color-faint); }
</style>
