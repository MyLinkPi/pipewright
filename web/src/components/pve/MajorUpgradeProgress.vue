<script setup lang="ts">
/*
  MajorUpgradeProgress.vue — 大版本 8→9 滚动升级进度(轮询 major-upgrade-status)。

  展示:任务状态徽标 + 逐节点时间线(状态/错误/起止时间)+ 日志尾部。
  done(非 running)即停轮询并 emit done 一次;paused/interrupted 可续跑,
  failed 节点可单独重试——受理后重置 doneEmitted 并重启轮询(状态回到 running)。
  中止在当前步骤结束后生效(绝不中断 dist-upgrade),受理后继续轮询等暂停。
*/
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { ArrowBarUp, Check, AlertTriangle, PlayerStop, RotateClockwise, Power } from '@vicons/tabler'
import {
  getPVEMajorUpgradeStatus, abortPVEMajorUpgrade, resumePVEMajorUpgrade, retryPVEMajorNode,
  confirmPVEMajorReboot, type PVEMajorUpgradeStatus,
} from '../../api/pveUpgrade'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'
import { useConfirm } from '../../composables/useConfirm'

const props = defineProps<{ clusterId: string; clusterName: string }>()
const emit = defineEmits<{ close: []; done: [] }>()

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()
const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

const status = ref<PVEMajorUpgradeStatus | null>(null)
const loadErr = ref('')
const actionBusy = ref(false)
let timer: ReturnType<typeof setTimeout> | null = null
let statusController: AbortController | null = null
let disposed = false
let doneEmitted = false
let tries = 0
let pollSeq = 0

const nodeStates = ['pending', 'guests', 'sources', 'upgrading', 'awaiting-reboot', 'rebooting', 'waiting', 'verifying', 'done', 'failed']
function nodeStateLabel(state: string): string {
  return nodeStates.includes(state) ? t(`pveUpgrade.nodeState_${state}`) : state
}
function nodeStateClass(state: string): string {
  if (state === 'done') return 'ok'
  if (state === 'failed') return 'err'
  if (state === 'awaiting-reboot' || state === 'upgrading' || state === 'rebooting' || state === 'waiting') return 'warn'
  if (state === 'pending') return 'dim'
  return 'run'
}

/** 头部状态徽标:running / done / paused / interrupted。 */
const badge = computed(() => {
  const st = status.value
  if (!st || (!st.jobId && !st.running && !st.done)) return null
  if (st.running) return { label: t('pveUpgrade.majorRunning'), cls: 'run', icon: ArrowBarUp }
  if (st.status === 'done') return { label: t('pveUpgrade.majorDone'), cls: 'ok', icon: Check }
  if (st.status === 'interrupted') return { label: t('pveUpgrade.majorInterrupted'), cls: 'bad', icon: AlertTriangle }
  return { label: t('pveUpgrade.majorPaused'), cls: 'warn', icon: AlertTriangle }
})

const canResume = computed(() => {
  const st = status.value
  return !!st && !st.running && (st.status === 'paused' || st.status === 'interrupted')
})

function fmtTime(s?: string): string {
  return s ? new Date(s).toLocaleString() : '—'
}

async function poll(): Promise<void> {
  if (disposed) return
  timer = null
  const seq = pollSeq
  const controller = new AbortController()
  statusController = controller
  try {
    const st = await getPVEMajorUpgradeStatus(props.clusterId, controller.signal)
    if (disposed || seq !== pollSeq) return
    status.value = st
    loadErr.value = ''
    tries = 0
    if (st.done) {
      if (!doneEmitted) {
        doneEmitted = true
        if (st.status === 'done') toast.success(t('pveUpgrade.majorDone'))
        else if (st.status === 'interrupted') toast.warn(t('pveUpgrade.majorInterrupted'), { detail: st.error })
        else toast.warn(t('pveUpgrade.majorPaused'), { detail: st.error })
        emit('done')
      }
      return
    }
    if (!st.jobId && !st.running) {
      // 无任务:不轮询,显示空态。
      return
    }
  } catch (err) {
    if (disposed || seq !== pollSeq || (err instanceof Error && err.name === 'AbortError')) return
    tries += 1
    loadErr.value = err instanceof HttpError ? (err.apiError?.message ?? t('pveUpgrade.errNetwork')) : t('pveUpgrade.errNetwork')
    if (tries >= 30) return
  } finally {
    if (statusController === controller) statusController = null
  }
  if (disposed || seq !== pollSeq) return
  timer = setTimeout(() => { void poll() }, 2000)
}

/** 续跑/重试受理后:状态回到 running,重置一次性守卫并重启轮询。 */
function restartPoll(): void {
  pollSeq += 1
  doneEmitted = false
  tries = 0
  if (timer) { clearTimeout(timer); timer = null }
  statusController?.abort()
  statusController = null
  void poll()
}

async function onAbort(): Promise<void> {
  if (actionBusy.value) return
  const ok = await confirm.open({
    title: t('pveUpgrade.abortConfirmTitle'),
    body: t('pveUpgrade.abortConfirmBody'),
    confirmLabel: t('pveUpgrade.abort'),
    variant: 'danger',
  })
  if (!ok) return
  actionBusy.value = true
  try {
    await abortPVEMajorUpgrade(props.clusterId)
    toast.success(t('pveUpgrade.abortAccepted'))
    // 保持轮询:中止在当前步骤结束后生效,等状态转 paused。
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  } finally {
    actionBusy.value = false
  }
}

async function onResume(): Promise<void> {
  if (actionBusy.value) return
  const ok = await confirm.open({
    title: t('pveUpgrade.resumeConfirmTitle'),
    body: t('pveUpgrade.resumeConfirmBody', { name: props.clusterName }),
    confirmText: props.clusterName,
    confirmLabel: t('pveUpgrade.resume'),
    variant: 'danger',
  })
  if (!ok) return
  actionBusy.value = true
  try {
    await resumePVEMajorUpgrade(props.clusterId, props.clusterName)
    toast.success(t('pveUpgrade.majorStarted'))
    restartPoll()
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  } finally {
    actionBusy.value = false
  }
}

async function onRetryNode(node: string): Promise<void> {
  if (actionBusy.value) return
  const ok = await confirm.open({
    title: t('pveUpgrade.retryConfirmTitle', { node }),
    body: t('pveUpgrade.retryConfirmBody', { name: props.clusterName }),
    confirmText: props.clusterName,
    confirmLabel: t('pveUpgrade.retryNode'),
    variant: 'danger',
  })
  if (!ok) return
  actionBusy.value = true
  try {
    await retryPVEMajorNode(props.clusterId, node, props.clusterName)
    toast.success(t('pveUpgrade.majorStarted'))
    restartPoll()
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  } finally {
    actionBusy.value = false
  }
}

/** 人工确认重启宿主节点(awaiting-reboot 态):控制端随节点下线,回来后人工续跑收尾。 */
const awaitingNode = computed(() => {
  const st = status.value
  if (!st || st.running) return ''
  return (st.nodes ?? []).find((n) => n.state === 'awaiting-reboot')?.node ?? ''
})

async function onConfirmReboot(): Promise<void> {
  if (actionBusy.value || !awaitingNode.value) return
  const ok = await confirm.open({
    title: t('pveUpgrade.confirmRebootTitle', { node: awaitingNode.value }),
    body: t('pveUpgrade.confirmRebootBody', { node: awaitingNode.value, name: props.clusterName }),
    confirmText: props.clusterName,
    confirmLabel: t('pveUpgrade.confirmRebootBtn'),
    variant: 'danger',
  })
  if (!ok) return
  actionBusy.value = true
  try {
    await confirmPVEMajorReboot(props.clusterId, props.clusterName)
    toast.success(t('pveUpgrade.confirmRebootSent'))
    restartPoll()
  } catch (err) {
    toast.error(errMsg(err, t('pveUpgrade.errNetwork')))
  } finally {
    actionBusy.value = false
  }
}

onMounted(() => { void poll() })
onUnmounted(() => {
  disposed = true
  pollSeq += 1
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
      :aria-label="t('pveUpgrade.majorProgressTitle')"
      aria-modal="true"
      @click.self="emit('close')"
      @keydown.esc="emit('close')"
    >
      <div class="modal modal--wide">
        <div class="modal-head">
          <span class="modal-icon"><NIcon :size="17"><ArrowBarUp /></NIcon></span>
          <div>
            <h2 class="modal-title">{{ t('pveUpgrade.majorProgressTitle') }} · {{ clusterName }}</h2>
            <p class="modal-sub">
              <template v-if="status && status.jobId">{{ status.step }} — {{ status.message }}</template>
              <template v-else>{{ t('pveUpgrade.majorIdle') }}</template>
            </p>
          </div>
          <span v-if="badge" class="done-badge" :class="badge.cls">
            <NIcon :size="13"><component :is="badge.icon" /></NIcon>
            {{ badge.label }}
          </span>
        </div>

        <div class="modal-body">
          <p v-if="loadErr" class="banner">{{ loadErr }}</p>
          <p v-if="status?.error" class="banner">{{ status.error }}</p>
          <p v-if="status?.running && status.abortRequested" class="banner banner--warn">{{ t('pveUpgrade.abortAccepted') }}</p>

          <!-- 逐节点时间线 -->
          <template v-if="status && status.nodes && status.nodes.length > 0">
            <div class="node-row node-row--head" aria-hidden="true">
              <span>{{ t('pveUpgrade.node') }}</span>
              <span>{{ t('pveUpgrade.colState') }}</span>
              <span>{{ t('pveUpgrade.colTime') }}</span>
              <span />
            </div>
            <div v-for="n in status.nodes" :key="n.node" class="node-row">
              <strong class="node-name">{{ n.node }}</strong>
              <span>
                <span class="tag" :class="nodeStateClass(n.state)" :title="n.error">{{ nodeStateLabel(n.state) }}</span>
              </span>
              <span class="mono dim time-cell">
                <template v-if="n.startedAt || n.finishedAt">{{ fmtTime(n.startedAt) }} → {{ fmtTime(n.finishedAt) }}</template>
                <template v-else>—</template>
              </span>
              <span class="row-ops">
                <button
                  v-if="n.state === 'failed' && !status.running"
                  type="button"
                  class="link-btn"
                  :disabled="actionBusy"
                  @click="onRetryNode(n.node)"
                >
                  <NIcon :size="12"><RotateClockwise /></NIcon>{{ t('pveUpgrade.retryNode') }}
                </button>
              </span>
            </div>
          </template>

          <!-- 日志尾部 -->
          <template v-if="status?.log && status.log.length > 0">
            <span class="log-title">{{ t('pveUpgrade.majorLogTitle') }}</span>
            <pre class="log">{{ status.log.slice(-40).join('\n') }}</pre>
          </template>
        </div>

        <div v-if="awaitingNode" class="banner banner--warn awaiting-banner">
          <span>{{ t('pveUpgrade.confirmRebootBanner', { node: awaitingNode }) }}</span>
          <button type="button" class="btn-danger" :disabled="actionBusy" @click="onConfirmReboot">
            <NIcon :size="13"><Power /></NIcon>{{ t('pveUpgrade.confirmRebootBtn') }}
          </button>
        </div>

        <div class="modal-foot">
          <button v-if="status?.running" type="button" class="btn-danger" :disabled="actionBusy" @click="onAbort">
            <NIcon :size="13"><PlayerStop /></NIcon>{{ t('pveUpgrade.abort') }}
          </button>
          <button v-if="canResume" type="button" class="btn-primary" :disabled="actionBusy" @click="onResume">
            <NIcon :size="13"><RotateClockwise /></NIcon>{{ t('pveUpgrade.resume') }}
          </button>
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
.done-badge.warn { background: var(--color-amber-soft, oklch(95% 0.06 80)); color: var(--color-amber, oklch(75% 0.15 80)); }
.done-badge.run { background: var(--color-primary-soft); color: var(--color-primary); }
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
.banner--warn {
  background: var(--color-amber-soft, oklch(95% 0.06 80));
  color: var(--color-amber, oklch(75% 0.15 80));
}
.awaiting-banner {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.node-row {
  display: grid;
  grid-template-columns: 0.9fr 0.9fr 1.6fr auto;
  gap: 10px;
  align-items: center;
  padding: 8px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-card);
  font-size: 0.8rem;
}
.node-row--head {
  background: none;
  border: none;
  padding-bottom: 0;
  font-size: 0.68rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.node-name { color: var(--color-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.time-cell { font-size: 0.68rem; overflow-wrap: anywhere; }
.row-ops { display: flex; justify-content: flex-end; }
.tag {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 2px 9px;
  border-radius: var(--rounded-full);
  font-size: 0.7rem;
  font-weight: 600;
  background: var(--color-inset);
  color: var(--color-dim);
  white-space: nowrap;
}
.tag.ok { background: var(--color-green-soft, oklch(95% 0.04 150)); color: var(--color-green); }
.tag.warn { background: var(--color-amber-soft, oklch(95% 0.06 80)); color: var(--color-amber, oklch(75% 0.15 80)); }
.tag.err { background: var(--color-red-soft, oklch(95% 0.03 25)); color: var(--color-red); }
.tag.run { background: var(--color-primary-soft); color: var(--color-primary); }
.tag.dim { background: var(--color-inset); color: var(--color-faint); }
.dim { color: var(--color-dim); }
.mono { font-family: var(--font-mono); font-size: 0.76rem; }
.log-title { font-size: 0.7rem; font-weight: 600; color: var(--color-faint); text-transform: uppercase; letter-spacing: 0.04em; }
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
.link-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  background: none;
  border: none;
  padding: 0;
  color: var(--color-primary);
  font-size: 0.74rem;
  cursor: pointer;
}
.link-btn:disabled { opacity: 0.5; cursor: not-allowed; }
.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 36px;
  padding: 0 16px;
  background: var(--color-primary);
  color: var(--color-primary-contrast, #fff);
  border: none;
  border-radius: var(--rounded);
  font-size: 0.82rem;
  font-weight: 600;
  cursor: pointer;
}
.btn-primary:disabled { opacity: 0.55; cursor: not-allowed; }
.btn-danger {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 36px;
  padding: 0 12px;
  background: var(--color-red-soft, oklch(95% 0.03 25));
  color: var(--color-red);
  border: 1px solid var(--color-red);
  border-radius: var(--rounded);
  font-size: 0.8rem;
  font-weight: 600;
  cursor: pointer;
}
.btn-danger:disabled { opacity: 0.55; cursor: not-allowed; }
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
  .node-row { grid-template-columns: 0.9fr 0.9fr auto; }
  .node-row > :nth-child(3) { display: none; }
}
</style>
