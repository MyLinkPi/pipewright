<!--
  BatchCommandModal.vue — 批量执行命令(服务器状态页)

  对勾选的一批已登记服务器同步执行同一条 shell 命令:
    · 已选机器 chips(可逐个移除);命令 textarea(mono);单机超时 30/60/120/300s
    · 执行前统一确认;命令命中危险特征(rm -rf / mkfs / dd 写盘 / 关机重启 / fork bomb …)
      时升级为「输入『执行』二字」的强确认(useConfirm.confirmText)
    · 同步等待全部机器返回(每台独立超时,一台失败不连累其它台),逐机展示
      成败徽标 / 退出码 / 耗时 / stdout / stderr(可折叠、可复制)
    · 完成后 toast 三分支(全成 / 全败 / 部分失败,同 Containers.runBulk)
  历史由后端落库(最近 200 次),经 CommandHistoryDrawer 回看/重跑。
-->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  batchServerCommand,
  type BatchCommandResult,
} from '../../api/servers'
import { HttpError } from '../../api/http'
import { useToast } from '../../composables/useToast'
import { useConfirm } from '../../composables/useConfirm'
import StatusBadge from '../ui/StatusBadge.vue'

interface ServerOption {
  id: string
  name: string
}

const props = defineProps<{
  /** 勾选的目标机(进入弹窗后仍可在 chips 里移除)。 */
  servers: ServerOption[]
  /** 预填命令(历史「重跑」入口);空 = 手输。 */
  initialCommand?: string
}>()
const emit = defineEmits<{
  (e: 'close'): void
  (e: 'executed'): void
}>()

const { t } = useI18n()
const toast = useToast()
const confirm = useConfirm()

// ─── 表单状态 ────────────────────────────────────────────────────────────────

const targets = ref<ServerOption[]>(props.servers.map((s) => ({ ...s })))
const command = ref(props.initialCommand ?? '')
const timeoutSec = ref('60')
const busy = ref(false)
const result = ref<BatchCommandResult | null>(null)

const TIMEOUT_OPTIONS = ['30', '60', '120', '300'].map((v) => ({
  value: v,
  label: t('batchCommand.timeoutOption', { n: v }),
}))

const canRun = computed(
  () => !busy.value && targets.value.length > 0 && command.value.trim() !== '',
)

// 弹窗打开时聚焦命令框;重跑入口预填后光标在末尾。
const commandInput = ref<HTMLTextAreaElement | null>(null)
watch(commandInput, (el) => el?.focus())

// ─── 危险命令启发式(前端强确认用;后端另有审计,拦截不靠这里) ────────────────

const DANGER_PATTERNS: RegExp[] = [
  /\brm\s+[^|;&]*-[a-z]*r[a-z]*f/i, // rm -rf(任意顺序组合 flag)
  /\bmkfs(\.\w+)?\b/i, // mkfs / mkfs.ext4
  /\bdd\b[^|;&]*of=\/dev\//i, // dd of=/dev/sd*
  />\s*\/dev\/(sd|nvme|vd|hd)/i, // 重定向直写块设备
  /\b(shutdown|reboot|halt|poweroff)\b/i, // 关机 / 重启
  /\binit\s+[06]\b/i,
  /:\(\)\s*\{.*\}\s*;\s*:/, // fork bomb
  /\b(wipefs|blkdiscard)\b/i,
  /\bchmod\s+-R\s+777\s+\/\s*$/i, // chmod -R 777 /(裸根)
]
const isDangerous = computed(() => DANGER_PATTERNS.some((re) => re.test(command.value)))

function removeTarget(id: string): void {
  targets.value = targets.value.filter((s) => s.id !== id)
}

// ─── 执行 ────────────────────────────────────────────────────────────────────

async function run(): Promise<void> {
  if (!canRun.value) return
  const n = targets.value.length
  const cmd = command.value.trim()

  // 统一确认;危险命令升级为输入「执行」二字的强确认。
  const ok = await confirm.open(
    isDangerous.value
      ? {
          title: t('batchCommand.dangerTitle'),
          body: t('batchCommand.dangerBody', { n }),
          confirmLabel: t('batchCommand.run'),
          confirmText: t('batchCommand.confirmWord'),
          variant: 'danger',
        }
      : {
          title: t('batchCommand.confirmTitle'),
          body: t('batchCommand.confirmBody', { n }),
          confirmLabel: t('batchCommand.run'),
          variant: 'primary',
        },
  )
  if (!ok) return

  busy.value = true
  result.value = null
  try {
    const res = await batchServerCommand({
      command: cmd,
      serverIds: targets.value.map((s) => s.id),
      timeoutSeconds: Number(timeoutSec.value),
    })
    result.value = res
    // toast 三分支(与 Containers.runBulk 同风格)。
    if (res.summary.failed === 0) {
      toast.success(t('batchCommand.toastAllOk', { n: res.summary.total }))
    } else if (res.summary.ok === 0) {
      toast.error(t('batchCommand.toastAllFail', { n: res.summary.failed }))
    } else {
      toast.warn(
        t('batchCommand.toastPartial', { ok: res.summary.ok, fail: res.summary.failed }),
      )
    }
    emit('executed')
  } catch (err) {
    if (err instanceof HttpError) {
      toast.error(t('batchCommand.execFailed'), {
        detail: err.status === 0 ? undefined : (err.apiError?.message ?? undefined),
      })
    } else {
      toast.error(t('batchCommand.execFailed'))
    }
  } finally {
    busy.value = false
  }
}

async function copyText(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(t('batchCommand.copied'))
  } catch {
    toast.error(t('batchCommand.copyFailed'))
  }
}
</script>

<template>
  <div class="modal-scrim">
    <div class="modal" role="dialog" :aria-label="t('batchCommand.title')">
      <header class="modal__head">
        <h3 class="modal__title">{{ t('batchCommand.title') }}</h3>
        <button class="modal__close" :aria-label="t('batchCommand.close')" @click="emit('close')">✕</button>
      </header>

      <div class="modal__body">
        <!-- 目标机 chips -->
        <div class="field">
          <span class="field__k">{{ t('batchCommand.selectedServers', { n: targets.length }) }}</span>
          <div v-if="targets.length" class="chips" role="list">
            <span v-for="s in targets" :key="s.id" class="chip" role="listitem">
              {{ s.name }}
              <button
                class="chip__x"
                :aria-label="t('batchCommand.removeServer', { name: s.name })"
                :disabled="busy"
                @click="removeTarget(s.id)"
              >✕</button>
            </span>
          </div>
          <p v-else class="field__hint">{{ t('batchCommand.noTargets') }}</p>
        </div>

        <!-- 命令 -->
        <label class="field">
          <span class="field__k">{{ t('batchCommand.command') }}</span>
          <textarea
            ref="commandInput"
            v-model="command"
            class="field__in mono"
            rows="3"
            spellcheck="false"
            :placeholder="t('batchCommand.commandPlaceholder')"
            :disabled="busy"
          />
          <span v-if="isDangerous" class="field__danger" role="alert">
            {{ t('batchCommand.dangerHint') }}
          </span>
        </label>

        <!-- 超时 -->
        <div class="field field--row">
          <span class="field__k">{{ t('batchCommand.timeout') }}</span>
          <select v-model="timeoutSec" class="field__in field__in--sm" :disabled="busy">
            <option v-for="o in TIMEOUT_OPTIONS" :key="o.value" :value="o.value">{{ o.label }}</option>
          </select>
        </div>

        <!-- 结果(执行后) -->
        <div v-if="result" class="results">
          <div class="results__summary">
            {{ t('batchCommand.resultsTitle') }} ·
            {{ t('batchCommand.summary', { ok: result.summary.ok, total: result.summary.total }) }}
          </div>
          <div v-for="it in result.items" :key="it.serverId" class="ritem" :class="{ 'ritem--fail': !it.ok }">
            <header class="ritem__head">
              <StatusBadge :status="it.ok ? 'success' : 'failed'" />
              <span class="ritem__name" :title="it.serverId">{{ it.name || it.serverId }}</span>
              <span class="ritem__meta mono">
                {{ t('batchCommand.exitCode', { n: it.exitCode }) }} ·
                {{ t('batchCommand.duration', { n: it.durationMs }) }}
              </span>
              <button
                v-if="it.stdout || it.stderr || it.error"
                class="ritem__copy"
                @click="copyText([it.error && `${t('batchCommand.error')}: ${it.error}`, it.stdout, it.stderr].filter(Boolean).join('\n'))"
              >{{ t('batchCommand.copy') }}</button>
            </header>
            <p v-if="it.error" class="ritem__error" role="alert">{{ it.error }}</p>
            <details v-if="it.stdout" class="ritem__out">
              <summary>{{ t('batchCommand.stdout') }}</summary>
              <pre class="mono">{{ it.stdout }}</pre>
            </details>
            <details v-if="it.stderr" class="ritem__out">
              <summary>{{ t('batchCommand.stderr') }}</summary>
              <pre class="mono">{{ it.stderr }}</pre>
            </details>
          </div>
        </div>
      </div>

      <footer class="modal__foot">
        <span class="foot-hint">{{ t('batchCommand.footHint') }}</span>
        <span class="grow" />
        <button class="btn btn--ghost" :disabled="busy" @click="emit('close')">
          {{ t('batchCommand.close') }}
        </button>
        <button class="btn btn--primary" :class="{ 'btn--danger': isDangerous }" :disabled="!canRun" @click="run">
          {{ busy ? t('batchCommand.running') : t('batchCommand.run') }}
        </button>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.modal-scrim {
  position: fixed;
  inset: 0;
  z-index: 500;
  background: oklch(0% 0 0 / 0.45);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
  animation: scrimIn var(--duration-fast) var(--ease-out-expo);
}
@keyframes scrimIn {
  from { opacity: 0; }
  to { opacity: 1; }
}
.modal {
  width: min(720px, 96vw);
  max-height: 90vh;
  display: flex;
  flex-direction: column;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-lg, 12px);
  box-shadow: var(--shadow-modal);
  animation: modalIn var(--duration-normal) var(--ease-out-expo);
}
@keyframes modalIn {
  from { transform: translateY(12px) scale(0.99); opacity: 0.5; }
  to { transform: translateY(0) scale(1); opacity: 1; }
}
.modal__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 16px 20px;
  border-bottom: 1px solid var(--color-border);
}
.modal__title {
  margin: 0;
  font-size: 1.05rem;
  font-weight: 700;
  color: var(--color-text);
}
.modal__close {
  border: 0;
  background: transparent;
  color: var(--color-faint);
  font-size: 1rem;
  cursor: pointer;
  padding: 4px 8px;
  border-radius: 6px;
}
.modal__close:hover { color: var(--color-text); background: var(--color-inset); }

.modal__body {
  padding: 18px 20px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.modal__foot {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 14px 20px;
  border-top: 1px solid var(--color-border);
}
.foot-hint { font-size: var(--text-label, 0.8rem); color: var(--color-faint); }
.grow { flex: 1; }

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.field--row {
  flex-direction: row;
  align-items: center;
  gap: 10px;
}
.field__k {
  font-size: var(--text-label, 0.8rem);
  font-weight: 600;
  color: var(--color-dim);
}
.field__hint {
  margin: 0;
  font-size: var(--text-label, 0.8rem);
  color: var(--color-faint);
}
.field__danger {
  font-size: var(--text-label, 0.8rem);
  font-weight: 600;
  color: var(--color-red);
}
.field__in {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded, 8px);
  background: var(--color-card-2, var(--color-inset));
  color: var(--color-text);
  font: inherit;
  padding: 8px 10px;
}
.field__in:focus-visible {
  outline: none;
  border-color: var(--color-primary);
  box-shadow: 0 0 0 3px var(--color-primary-soft);
}
.field__in--sm { width: 140px; }
.mono { font-family: var(--font-mono, ui-monospace, monospace); }

.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 10px;
  border-radius: 999px;
  border: 1px solid var(--color-border);
  background: var(--color-inset);
  font-size: var(--text-label, 0.8rem);
  color: var(--color-text);
}
.chip__x {
  border: 0;
  background: transparent;
  color: var(--color-faint);
  cursor: pointer;
  font-size: 0.7rem;
  padding: 0 2px;
  border-radius: 4px;
}
.chip__x:hover:not(:disabled) { color: var(--color-red); }
.chip__x:disabled { cursor: not-allowed; opacity: 0.5; }

/* ——— results ——— */
.results {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.results__summary {
  font-size: var(--text-label, 0.8rem);
  font-weight: 600;
  color: var(--color-dim);
}
.ritem {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded, 8px);
  background: var(--color-card-2, var(--color-inset));
}
.ritem--fail { border-color: var(--color-red-line, var(--color-border)); }
.ritem__head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.ritem__name {
  font-weight: 650;
  font-size: var(--text-body, 0.9rem);
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 40%;
}
.ritem__meta {
  font-size: var(--text-label, 0.8rem);
  color: var(--color-faint);
  font-variant-numeric: tabular-nums;
}
.ritem__copy {
  margin-left: auto;
  border: 1px solid var(--color-border);
  background: transparent;
  color: var(--color-dim);
  font-size: var(--text-label, 0.8rem);
  padding: 3px 10px;
  border-radius: 6px;
  cursor: pointer;
}
.ritem__copy:hover { color: var(--color-text); border-color: var(--color-border-strong); }
.ritem__error {
  margin: 0;
  font-size: var(--text-label, 0.8rem);
  color: var(--color-red);
  line-height: 1.5;
}
.ritem__out summary {
  cursor: pointer;
  font-size: var(--text-label, 0.8rem);
  font-weight: 600;
  color: var(--color-dim);
  user-select: none;
}
.ritem__out pre {
  margin: 6px 0 0;
  padding: 10px;
  border-radius: 6px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  font-size: 0.78rem;
  line-height: 1.5;
  color: var(--color-text);
  max-height: 240px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
}

/* ——— buttons(local idiom matches CreateContainerModal) ——— */
.btn {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded, 8px);
  padding: 7px 16px;
  font: inherit;
  font-size: var(--text-body, 0.9rem);
  cursor: pointer;
  background: var(--color-card-2, var(--color-inset));
  color: var(--color-text);
}
.btn--ghost { background: transparent; color: var(--color-dim); }
.btn--primary {
  background: var(--color-primary);
  border-color: var(--color-primary);
  color: var(--color-primary-contrast, #fff);
  font-weight: 600;
}
.btn--danger {
  background: var(--color-red);
  border-color: var(--color-red);
  color: #fff;
}
.btn:disabled { opacity: 0.55; cursor: not-allowed; }
</style>
