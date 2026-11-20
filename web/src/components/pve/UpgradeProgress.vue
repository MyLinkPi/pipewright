<script setup lang="ts">
/*
  UpgradeProgress.vue — 日常 apt 升级进度(轮询 upgrade-status 至 done)。

  展示:步骤/消息 + 逐节点结果表(更新包数/需重启标记)+ 日志尾部。
  完成后 emit done(父组件刷新更新矩阵);独立轮询 timer,2s 一次,错误预算 30 次。
*/
import { ref, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { ArrowBarUp, Check, X, AlertTriangle } from '@vicons/tabler'
import { getPVEUpgradeStatus, type PVEUpgradeStatus } from '../../api/pveUpgrade'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'

const props = defineProps<{ clusterId: string; clusterName: string }>()
const emit = defineEmits<{ close: []; done: [] }>()

const { t } = useI18n()
const toast = useToast()

const status = ref<PVEUpgradeStatus | null>(null)
const loadErr = ref('')
let timer: ReturnType<typeof setTimeout> | null = null
let statusController: AbortController | null = null
let disposed = false
let doneEmitted = false
let tries = 0

const stepKeys = ['prepare', 'refresh', 'upgrade', 'check']
function stepLabel(step: string): string {
  return stepKeys.includes(step) ? t(`pveUpgrade.step_${step}`) : step
}

async function poll(): Promise<void> {
  if (disposed) return
  timer = null
  const controller = new AbortController()
  statusController = controller
  try {
    const st = await getPVEUpgradeStatus(props.clusterId, controller.signal)
    if (disposed) return
    status.value = st
    loadErr.value = ''
    tries = 0
    if (st.done) {
      if (!doneEmitted) {
        doneEmitted = true
        if (st.error) toast.error(t('pveUpgrade.progressFailed'), { detail: st.error })
        else if (st.failed > 0) toast.warn(t('pveUpgrade.progressPartial', { ok: st.ok, failed: st.failed }))
        else if (st.total > 0) toast.success(t('pveUpgrade.progressDone', { n: st.ok }))
        emit('done')
      }
      return
    }
    if (st.total === 0 && !st.running) {
      // 从未启动:不轮询,显示空态。
      return
    }
  } catch (err) {
    if (disposed || (err instanceof Error && err.name === 'AbortError')) return
    tries += 1
    loadErr.value = err instanceof HttpError ? (err.apiError?.message ?? t('pveUpgrade.errNetwork')) : t('pveUpgrade.errNetwork')
    if (tries >= 30) return
  } finally {
    if (statusController === controller) statusController = null
  }
  if (disposed) return
  timer = setTimeout(() => { void poll() }, 2000)
}

onMounted(() => { void poll() })
onUnmounted(() => {
  disposed = true
  if (timer) clearTimeout(timer)
  statusController?.abort()
  statusController = null
})
</script>

<template>
  <Teleport to="body">
    <div
      class="modal-scrim"
      role="dialog"
      :aria-label="t('pveUpgrade.progressTitle')"
      aria-modal="true"
      @click.self="emit('close')"
      @keydown.esc="emit('close')"
    >
      <div class="modal modal--wide">
        <div class="modal-head">
          <span class="modal-icon"><NIcon :size="17"><ArrowBarUp /></NIcon></span>
          <div>
            <h2 class="modal-title">{{ t('pveUpgrade.progressTitle') }} · {{ clusterName }}</h2>
            <p class="modal-sub">
              <template v-if="status && (status.running || status.done)">
                {{ stepLabel(status.step) }} — {{ status.message }}
              </template>
              <template v-else>{{ t('pveUpgrade.progressIdle') }}</template>
            </p>
          </div>
          <span v-if="status?.done" class="done-badge" :class="status.error || status.failed > 0 ? 'bad' : 'ok'">
            <NIcon :size="13"><component :is="status.error || status.failed > 0 ? AlertTriangle : Check" /></NIcon>
            {{ status.error ? t('pveUpgrade.progressFailed') : t('pveUpgrade.progressSummary', { ok: status.ok, failed: status.failed }) }}
          </span>
        </div>

        <div class="modal-body">
          <p v-if="loadErr" class="banner">{{ loadErr }}</p>
          <p v-if="status?.error" class="banner">{{ status.error }}</p>
          <p v-if="status && status.total === 0 && !status.running && !status.done" class="idle">{{ t('pveUpgrade.progressIdle') }}</p>

          <!-- 逐节点结果 -->
          <template v-if="status && status.items && status.items.length > 0">
            <div class="item-row item-row--head" aria-hidden="true">
              <span>{{ t('pveUpgrade.node') }}</span>
              <span>{{ t('pveUpgrade.colUpdated') }}</span>
              <span>{{ t('pveUpgrade.colResult') }}</span>
            </div>
            <div v-for="item in status.items" :key="item.node" class="item-row">
              <strong class="item-name">{{ item.node }}</strong>
              <span class="mono dim">{{ item.ok ? t('pveUpgrade.updatedN', { n: item.updated }) : '—' }}</span>
              <span class="item-result">
                <template v-if="item.ok">
                  <span class="ok"><NIcon :size="12"><Check /></NIcon>{{ t('pveUpgrade.itemOk') }}</span>
                  <span v-if="item.needsReboot" class="reboot-pill"><NIcon :size="11"><AlertTriangle /></NIcon>{{ t('pveUpgrade.needsReboot') }}</span>
                  <span v-if="item.note" class="dim sub" :title="item.note">{{ item.note }}</span>
                </template>
                <span v-else-if="item.err" class="bad" :title="item.err"><NIcon :size="12"><X /></NIcon>{{ t('pveUpgrade.itemFailed') }}</span>
                <span v-else class="dim">{{ t('pveUpgrade.itemPending') }}</span>
              </span>
            </div>
          </template>

          <!-- 日志尾部 -->
          <pre v-if="status?.log && status.log.length > 0" class="log">{{ status.log.slice(-40).join('\n') }}</pre>
        </div>

        <div class="modal-foot">
          <button type="button" class="btn-secondary" @click="emit('close')">
            {{ status?.done ? t('pveUpgrade.close') : t('pveUpgrade.runInBackground') }}
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.modal-scrim {
  position: fixed;
  inset: 0;
  background: oklch(0% 0 0 / 0.62);
  display: grid;
  place-items: center;
  z-index: 100;
  padding: 24px;
}
.modal {
  width: 100%;
  max-width: 540px;
  max-height: min(90vh, calc(100dvh - 48px));
  display: flex;
  flex-direction: column;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-xl);
  box-shadow: var(--shadow-modal);
  overflow: hidden;
}
.modal--wide { max-width: 680px; }
.modal-head {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 20px 20px 16px;
  border-bottom: 1px solid var(--color-border);
}
.modal-icon {
  width: 36px;
  height: 36px;
  border-radius: var(--rounded-lg);
  background: var(--color-primary-soft);
  color: var(--color-primary);
  display: grid;
  place-items: center;
  flex-shrink: 0;
}
.modal-title { font-size: 1rem; font-weight: 600; color: var(--color-text); margin-top: 2px; }
.modal-sub { font-size: 0.78rem; color: var(--color-faint); margin-top: 3px; }
.done-badge {
  margin-left: auto;
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 0.74rem;
  font-weight: 600;
  padding: 4px 10px;
  border-radius: var(--rounded-full);
  white-space: nowrap;
}
.done-badge.ok { background: var(--color-green-soft, oklch(95% 0.04 150)); color: var(--color-green); }
.done-badge.bad { background: var(--color-red-soft, oklch(95% 0.03 25)); color: var(--color-red); }
.modal-body { padding: 18px 20px; display: flex; flex-direction: column; gap: 10px; min-height: 0; overflow-y: auto; }
.modal-foot {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  padding: 14px 20px 18px;
  border-top: 1px solid var(--color-border);
}
.banner {
  padding: 9px 12px;
  border-radius: var(--rounded);
  background: var(--color-red-soft, oklch(95% 0.03 25));
  color: var(--color-red);
  font-size: 0.78rem;
  line-height: 1.5;
  overflow-wrap: anywhere;
}
.idle { padding: 20px 12px; text-align: center; color: var(--color-faint); font-size: 0.82rem; }
.item-row {
  display: grid;
  grid-template-columns: 1fr 0.9fr 1.6fr;
  gap: 10px;
  align-items: center;
  padding: 8px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-card);
  font-size: 0.8rem;
}
.item-row--head {
  background: none;
  border: none;
  padding-bottom: 0;
  font-size: 0.68rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.item-name { color: var(--color-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.item-result { display: flex; flex-direction: column; gap: 2px; }
.item-result .ok, .item-result .bad { display: inline-flex; align-items: center; gap: 4px; font-size: 0.76rem; }
.ok { color: var(--color-green); }
.bad { color: var(--color-red); }
.dim { color: var(--color-dim); }
.sub { font-size: 0.68rem; }
.mono { font-family: var(--font-mono); font-size: 0.76rem; }
.reboot-pill {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  align-self: flex-start;
  padding: 1px 7px;
  border-radius: var(--rounded-full);
  background: var(--color-amber-soft, oklch(95% 0.06 80));
  color: var(--color-amber, oklch(75% 0.15 80));
  font-size: 0.66rem;
  font-weight: 600;
}
.log {
  margin: 0;
  padding: 10px 12px;
  background: var(--color-inset);
  border-radius: var(--rounded);
  font-family: var(--font-mono);
  font-size: 0.7rem;
  line-height: 1.6;
  color: var(--color-dim);
  max-height: 220px;
  overflow-y: auto;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
.btn-secondary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 36px;
  padding: 0 12px;
  background: var(--color-card);
  color: var(--color-text);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  font-size: 0.8rem;
  font-weight: 500;
  cursor: pointer;
}
@media (max-width: 720px) {
  .item-row { grid-template-columns: 1fr 1.6fr; }
  .item-row > :nth-child(2) { display: none; }
}
</style>
