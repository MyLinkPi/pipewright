<script setup lang="ts">
/*
  MajorUpgradeWizard.vue — 大版本 8→9 升级向导(两步:预检报告确认 → 策略与强确认)。

  步 1:审阅预检报告(集群/逐节点检查 + pve8to9 摘要),逐条勾选 acksRequired 警告;
  步 2:guest 处理策略(warn/shutdown)+ 原样输入集群名强确认。
  提交后由父组件打开 MajorUpgradeProgress 轮询;本组件只负责收集入参。
  样式沿用 CreateWizard 模态框体系(Teleport + modal-scrim + steps + field)。
*/
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import { Server, ArrowBarUp } from '@vicons/tabler'
import AppSelect from '../ui/AppSelect.vue'
import {
  startPVEMajorUpgrade,
  type PVEMajorPreflightReport, type PVEMajorCheck,
} from '../../api/pveUpgrade'
import { HttpError } from '../../api/http'

const props = defineProps<{
  clusterId: string
  clusterName: string
  report: PVEMajorPreflightReport
}>()
const emit = defineEmits<{ close: []; started: [] }>()

const { t } = useI18n()
const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 步骤与表单 ────────────────────────────────────────────────────────────────
const step = ref(1)
const acks = ref(new Set<string>())
const guestPolicy = ref<'warn' | 'shutdown'>('warn')
const confirmText = ref('')
const formErrors = ref<Record<string, string>>({})
const formBanner = ref('')
const submitting = ref(false)

// 宿主节点(控制端所在):用户显式指定而非自动识别,避免误判;该节点最后升级、
// guest 不自动关停、重启需人工确认(不做自恢复)。
const hostNode = ref('')
const hostNodeOptions = computed(() => [
  { value: '', label: t('pveUpgrade.hostNodeNone') },
  ...props.report.nodes.filter((n) => n.online).map((n) => ({ value: n.node, label: n.node })),
])

// 升级节点勾选:默认全选在线节点。宿主机仅是标记(选中即走「最后升级 + 人工确认
// 重启」序列);不勾选它则本次完全不动(分批升级),按普通滚动执行。
const selectedNodes = ref<Set<string>>(new Set(props.report.nodes.filter((n) => n.online).map((n) => n.node)))
const selectedList = computed(() =>
  props.report.nodes.filter((n) => selectedNodes.value.has(n.node)).map((n) => n.node))

function toggleNode(node: string): void {
  if (selectedNodes.value.has(node)) selectedNodes.value.delete(node)
  else selectedNodes.value.add(node)
}

function stepLabel(n: number): string {
  return n === 1 ? t('pveUpgrade.preflightTitle') : t('pveUpgrade.majorConfirmTitle')
}

// 待确认警告:按 key 去重,聚合集群/各节点的同 key 警告文案。

function checkLabel(level: PVEMajorCheck['level']): string {
  if (level === 'pass') return t('pveUpgrade.checkPass')
  if (level === 'warn') return t('pveUpgrade.checkWarn')
  return t('pveUpgrade.checkFail')
}
function checkClass(level: PVEMajorCheck['level']): string {
  return level === 'pass' ? 'ok' : level === 'warn' ? 'warn' : 'err'
}

/** 待确认警告:按 key 去重,聚合集群/各节点的同 key 警告文案;节点级警告仅聚合
 * 本次所选节点的(未选节点的警告不拦本次升级,后端同口径过滤)。 */
const ackItems = computed(() => {
  const msgs = new Map<string, string[]>()
  const push = (key: string, msg: string): void => {
    const list = msgs.get(key) ?? []
    list.push(msg)
    msgs.set(key, list)
  }
  for (const ck of props.report.clusterChecks) {
    if (ck.level === 'warn') push(ck.key, ck.message)
  }
  for (const np of props.report.nodes) {
    if (!selectedNodes.value.has(np.node)) continue
    for (const ck of np.checks) {
      if (ck.level === 'warn') push(ck.key, `${np.node}: ${ck.message}`)
    }
  }
  const seen = new Set<string>()
  const items: Array<{ key: string; message: string }> = []
  for (const key of props.report.acksRequired) {
    const ci = key.lastIndexOf(':')
    if (ci >= 0 && !selectedNodes.value.has(key.slice(ci + 1))) continue
    if (seen.has(key)) continue
    seen.add(key)
    items.push({ key, message: (msgs.get(key) ?? [key]).join(';') })
  }
  return items
})

const allAcked = computed(() => ackItems.value.every((it) => acks.value.has(it.key)))

/** 阻断按本次所选节点收敛:集群级 FAIL + 选中节点的 FAIL 才拦本次;未选节点的
 * FAIL 不拦(它们本次不动,可稍后单独升级)。 */
const scopedBlocked = computed(() => {
  if (props.report.clusterChecks.some((ck) => ck.level === 'fail')) return true
  return props.report.nodes
    .filter((np) => selectedNodes.value.has(np.node))
    .some((np) => np.checks.some((ck) => ck.level === 'fail'))
})

function toggleAck(key: string): void {
  if (acks.value.has(key)) acks.value.delete(key)
  else acks.value.add(key)
}

// 全部勾选后清掉「请先确认全部警告」横幅。
watch(allAcked, (v) => {
  if (v && formBanner.value === t('pveUpgrade.ackMissing')) formBanner.value = ''
})

// ─── 校验与提交 ────────────────────────────────────────────────────────────────
function validateStep(n: number): boolean {
  const e: Record<string, string> = {}
  if (n === 1 && selectedNodes.value.size === 0) e.nodes = t('pveUpgrade.upgradeNodesEmpty')
  if (n === 1 && !allAcked.value) e.acks = t('pveUpgrade.ackMissing')
  if (n === 2 && confirmText.value !== props.clusterName) e.confirm = t('pveUpgrade.confirmMismatch')
  formErrors.value = e
  if (e.acks) formBanner.value = e.acks
  return Object.keys(e).length === 0
}

function nextStep(): void {
  if (submitting.value || scopedBlocked.value) return
  if (validateStep(1)) {
    formBanner.value = ''
    step.value = 2
  }
}

async function submit(): Promise<void> {
  if (submitting.value || scopedBlocked.value) return
  if (!validateStep(2)) return
  submitting.value = true
  formBanner.value = ''
  try {
    await startPVEMajorUpgrade(props.clusterId, {
      confirm: confirmText.value,
      acks: [...acks.value],
      guestPolicy: guestPolicy.value,
      nodes: selectedList.value,
      hostNode: hostNode.value || undefined,
    })
    emit('started')
  } catch (err) {
    formBanner.value = errMsg(err, t('pveUpgrade.errNetwork'))
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <Teleport to="body">
    <div
      class="modal-scrim"
      role="dialog"
      :aria-label="t('pveUpgrade.majorTitle')"
      aria-modal="true"
      @click.self="emit('close')"
      @keydown.esc="emit('close')"
    >
      <form class="modal modal--wide" @submit.prevent="step === 2 ? submit() : nextStep()">
        <div class="modal-head">
          <span class="modal-icon"><NIcon :size="17"><Server /></NIcon></span>
          <div>
            <h2 class="modal-title">{{ t('pveUpgrade.majorTitle') }} · {{ clusterName }}</h2>
            <p class="modal-sub">{{ t('pveUpgrade.majorSub') }}</p>
          </div>
          <ol class="steps">
            <li v-for="n in 2" :key="n" :class="{ active: step === n, done: step > n }">
              {{ stepLabel(n) }}
            </li>
          </ol>
        </div>

        <div class="modal-body">
          <p v-if="formBanner" class="banner">{{ formBanner }}</p>
          <p v-if="scopedBlocked" class="banner">{{ t('pveUpgrade.preflightBlocked') }}</p>

          <!-- 步 1:预检报告确认 -->
          <template v-if="step === 1">
            <div class="sec">
              <span class="sec-title">{{ t('pveUpgrade.upgradeNodesLabel') }}</span>
              <p class="hint">{{ t('pveUpgrade.upgradeNodesHint') }}</p>
              <div class="node-pick">
                <label
                  v-for="np in report.nodes"
                  :key="np.node"
                  class="check node-pick-item"
                >
                  <input
                    type="checkbox"
                    :checked="selectedNodes.has(np.node)"
                    :disabled="!np.online"
                    @change="toggleNode(np.node)"
                  />
                  <strong>{{ np.node }}</strong>
                  <span v-if="!np.online" class="tag err">{{ t('pveUpgrade.offline') }}</span>
                  <span v-if="np.node === hostNode" class="tag run">{{ t('pveUpgrade.hostNodeTag') }}</span>
                </label>
              </div>
              <span v-if="formErrors.nodes" class="field-error">{{ formErrors.nodes }}</span>
            </div>

            <div class="sec">
              <span class="sec-title">{{ t('pveUpgrade.clusterChecks') }}</span>
              <div v-for="ck in report.clusterChecks" :key="ck.key" class="check-row">
                <span class="tag" :class="checkClass(ck.level)">{{ checkLabel(ck.level) }}</span>
                <span class="check-msg">{{ ck.message }}</span>
              </div>
            </div>

            <div class="sec">
              <span class="sec-title">{{ t('pveUpgrade.nodeChecks') }}</span>
              <div v-for="np in report.nodes" :key="np.node" class="node-block">
                <div class="node-block-head">
                  <strong>{{ np.node }}</strong>
                  <span class="tag" :class="np.online ? 'ok' : 'err'">{{ np.online ? t('pveUpgrade.online') : t('pveUpgrade.offline') }}</span>
                  <span class="mono dim">{{ np.version || '—' }}</span>
                  <span v-if="np.codename" class="mono dim">{{ np.codename }}</span>
                  <span class="dim sub">{{ t('pveUpgrade.diskFree', { gb: np.diskFreeGb.toFixed(1) }) }}</span>
                  <span class="dim sub">{{ t('pveUpgrade.pendingN', { n: np.pending }) }}</span>
                </div>
                <p v-if="np.error" class="banner">{{ np.error }}</p>
                <div v-if="np.pve8to9" class="pve8to9">
                  <span class="sec-title">{{ t('pveUpgrade.pve8to9Title') }}</span>
                  <span class="dim sub">
                    {{ t('pveUpgrade.pve8to9Summary', { passed: np.pve8to9.passed, skipped: np.pve8to9.skipped, warnings: np.pve8to9.warnings, failures: np.pve8to9.failures }) }}
                  </span>
                </div>
                <div v-for="ck in np.checks" :key="ck.key" class="check-row">
                  <span class="tag" :class="checkClass(ck.level)">{{ checkLabel(ck.level) }}</span>
                  <span class="check-msg">{{ ck.message }}</span>
                </div>
              </div>
            </div>

            <div v-if="ackItems.length > 0" class="sec">
              <span class="sec-title">{{ t('pveUpgrade.ackRequired') }}({{ ackItems.length }})</span>
              <label v-for="item in ackItems" :key="item.key" class="check ack-check">
                <input type="checkbox" :checked="acks.has(item.key)" @change="toggleAck(item.key)" />
                <span class="ack-text">
                  <span>{{ item.message }}</span>
                  <span class="dim sub">{{ t('pveUpgrade.ackLabel') }}</span>
                </span>
              </label>
            </div>
          </template>

          <!-- 步 2:策略与强确认 -->
          <template v-if="step === 2">
            <p class="hint">{{ t('pveUpgrade.majorConfirmBody', { name: clusterName }) }}</p>

            <div class="field">
              <span class="field-label">{{ t('pveUpgrade.guestPolicy') }}</span>
              <label class="check">
                <input v-model="guestPolicy" type="radio" value="warn" />
                {{ t('pveUpgrade.guestWarn') }}
              </label>
              <label class="check">
                <input v-model="guestPolicy" type="radio" value="shutdown" />
                {{ t('pveUpgrade.guestShutdown') }}
              </label>
            </div>

            <div class="field">
              <span class="field-label">{{ t('pveUpgrade.hostNodeLabel') }}</span>
              <AppSelect
                v-model="hostNode"
                :options="hostNodeOptions"
                :placeholder="t('pveUpgrade.hostNodeNone')"
                :aria-label="t('pveUpgrade.hostNodeLabel')"
                min-width="220px"
                height="36px"
              />
              <span class="field-hint">{{ t('pveUpgrade.hostNodeHint') }}</span>
            </div>

            <label class="field">
              <span class="field-label">{{ t('pveUpgrade.majorConfirmTitle') }}</span>
              <input v-model="confirmText" class="field-input mono" type="text" :placeholder="clusterName" autocomplete="off" />
              <span v-if="formErrors.confirm" class="field-error">{{ formErrors.confirm }}</span>
            </label>

            <div class="summary">
              <div class="summary-row"><span>{{ t('pveUpgrade.cluster') }}</span><strong>{{ clusterName }}</strong></div>
              <div class="summary-row"><span>{{ t('pveUpgrade.guestPolicy') }}</span><strong>{{ guestPolicy === 'warn' ? t('pveUpgrade.guestWarn') : t('pveUpgrade.guestShutdown') }}</strong></div>
              <div class="summary-row"><span>{{ t('pveUpgrade.hostNodeLabel') }}</span><strong>{{ hostNode || t('pveUpgrade.hostNodeNone') }}</strong></div>
              <div class="summary-row"><span>{{ t('pveUpgrade.upgradeNodesLabel') }}</span><strong>{{ selectedList.length }} / {{ report.nodes.length }}</strong></div>
              <div class="summary-row"><span>{{ t('pveUpgrade.node') }}</span><strong>{{ report.nodes.length }}</strong></div>
            </div>
          </template>
        </div>

        <div class="modal-foot">
          <button type="button" class="btn-secondary" :disabled="submitting" @click="emit('close')">
            {{ t('pveUpgrade.cancel') }}
          </button>
          <button v-if="step > 1" type="button" class="btn-secondary" :disabled="submitting" @click="step -= 1">
            {{ t('pveCreate.back') }}
          </button>
          <button
            type="submit"
            class="btn-primary"
            :disabled="submitting || scopedBlocked || (step === 1 ? !allAcked : confirmText !== clusterName)"
            :aria-busy="submitting"
          >
            <NIcon v-if="step === 2" :size="13"><ArrowBarUp /></NIcon>
            {{ step === 2 ? (submitting ? t('pveCreate.submitting') : t('pveUpgrade.startMajor')) : t('pveCreate.next') }}
          </button>
        </div>
      </form>
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
.modal-sub { font-size: 0.78rem; color: var(--color-faint); margin-top: 3px; max-width: 44ch; line-height: 1.5; }
.steps { margin-left: auto; display: flex; gap: 6px; list-style: none; padding: 0; }
.steps li {
  font-size: 0.7rem;
  padding: 4px 9px;
  border-radius: var(--rounded-full);
  border: 1px solid var(--color-border);
  color: var(--color-faint);
  white-space: nowrap;
}
.steps li.active { background: var(--color-primary-soft); color: var(--color-primary); border-color: var(--color-primary); font-weight: 600; }
.steps li.done { color: var(--color-green); border-color: var(--color-green); }
.modal-body { padding: 18px 20px; display: flex; flex-direction: column; gap: 14px; min-height: 0; overflow-y: auto; }
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
.sec { display: flex; flex-direction: column; gap: 6px; }
.sec-title { font-size: 0.74rem; font-weight: 600; color: var(--color-dim); }
.check-row { display: flex; align-items: flex-start; gap: 8px; font-size: 0.78rem; }
.check-msg { color: var(--color-dim); line-height: 1.5; overflow-wrap: anywhere; }
.node-block {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-inset);
}
.node-block-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 0.8rem; }
.node-block-head strong { color: var(--color-text); }
.pve8to9 { display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; }
.tag {
  display: inline-flex;
  align-items: center;
  padding: 1px 8px;
  border-radius: var(--rounded-full);
  font-size: 0.68rem;
  font-weight: 600;
  background: var(--color-card);
  color: var(--color-dim);
  white-space: nowrap;
}
.tag.ok { background: var(--color-green-soft, oklch(95% 0.04 150)); color: var(--color-green); }
.tag.warn { background: var(--color-amber-soft, oklch(95% 0.06 80)); color: var(--color-amber, oklch(75% 0.15 80)); }
.tag.err { background: var(--color-red-soft, oklch(95% 0.03 25)); color: var(--color-red); }
.check { display: inline-flex; align-items: center; gap: 6px; font-size: 0.78rem; color: var(--color-dim); cursor: pointer; }
.ack-check { align-items: flex-start; }
.ack-check input { margin-top: 3px; }
.ack-text { display: flex; flex-direction: column; gap: 2px; line-height: 1.5; }
.field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.field-label { font-size: 0.76rem; font-weight: 500; color: var(--color-dim); }
.field-input {
  width: 100%;
  height: 36px;
  box-sizing: border-box;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  padding: 0 10px;
  color: var(--color-text);
  font-family: var(--font-sans);
  font-size: 0.84rem;
}
.field-input.mono { font-family: var(--font-mono); font-size: 0.8rem; }
.field-input:focus {
  outline: none;
  border-color: var(--color-primary);
  box-shadow: 0 0 0 3px var(--color-primary-soft);
}
.field-error { font-size: 0.72rem; color: var(--color-red); }
.hint { font-size: 0.74rem; color: var(--color-faint); line-height: 1.5; }
.node-pick { display: flex; flex-wrap: wrap; gap: 8px 16px; }
.node-pick-item { display: inline-flex; align-items: center; gap: 6px; font-size: 0.8rem; color: var(--color-text); cursor: pointer; }
.node-pick-item input:disabled { cursor: not-allowed; }
.field-hint { font-size: 0.7rem; color: var(--color-faint); line-height: 1.5; }
.dim { color: var(--color-dim); }
.sub { font-size: 0.68rem; }
.mono { font-family: var(--font-mono); font-size: 0.76rem; }
.summary {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 12px 14px;
  background: var(--color-inset);
  border-radius: var(--rounded);
}
.summary-row { display: flex; justify-content: space-between; gap: 12px; font-size: 0.78rem; }
.summary-row span { color: var(--color-faint); }
.summary-row strong { color: var(--color-text); font-weight: 600; overflow-wrap: anywhere; text-align: right; }
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
.btn-secondary:disabled { opacity: 0.55; cursor: not-allowed; }
@media (max-width: 720px) {
  .steps { display: none; }
}
</style>
