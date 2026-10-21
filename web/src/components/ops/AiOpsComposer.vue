<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { Send, PlayerStop, Refresh } from '@vicons/tabler'
import { utf8Bytes } from '../../api/opsChat'
const props = defineProps<{
  draft: string
  draftStatus: string
  disabled: boolean
  active: boolean
  modelAvailable: boolean
  hasTargets: boolean
  pending: boolean
}>()
const emit = defineEmits<{
  edit: [value: string]
  send: []
  stop: []
  save: []
  remote: []
  local: []
  resend: []
}>()
const { t } = useI18n()
const canSend = computed(
  () =>
    !props.disabled &&
    !props.active &&
    !props.pending &&
    props.modelAvailable &&
    props.hasTargets &&
    !!props.draft.trim() &&
    utf8Bytes(props.draft) <= 8192,
)
// 回车发送;Ctrl/Cmd+回车换行(Shift+回车保留原生换行)。
function onKeydown(event: KeyboardEvent): void {
  if (event.key !== 'Enter' || event.shiftKey) return
  event.preventDefault()
  if (event.ctrlKey || event.metaKey) {
    const el = event.target as HTMLTextAreaElement
    const at = el.selectionStart
    const next = props.draft.slice(0, at) + "\n" + props.draft.slice(el.selectionEnd)
    el.value = next
    emit('edit', next)
    requestAnimationFrame(() => {
      el.selectionStart = el.selectionEnd = at + 1
    })
    return
  }
  if (canSend.value) emit('send')
}
</script>
<template>
  <section class="composer">
    <p v-if="!modelAvailable" class="ops-muted">
      {{ t('opsChat.modelUnavailable') }}
    </p>
    <p v-if="!hasTargets" class="ops-muted" role="status">
      {{ t('opsChat.noTargets') }}
    </p>
    <div class="input-row">
      <textarea
        class="ops-input draft"
        rows="3"
        :value="draft"
        :placeholder="t('opsChat.message')"
        :aria-label="t('opsChat.message')"
        :disabled="disabled"
        @input="emit('edit', ($event.target as HTMLTextAreaElement).value)"
        @keydown="onKeydown"
      />
      <button
        class="ops-btn ops-icon ops-primary"
        :disabled="!canSend"
        :title="t(hasTargets ? 'opsChat.send' : 'opsChat.noTargets')"
        :aria-label="t('opsChat.send')"
        @click="emit('send')"
      >
        <Send />
      </button>
    </div>
    <div class="ops-row">
      <span class="ops-muted ops-grow" role="status"
        >{{ t('opsChat.draft.' + draftStatus) }} · {{ utf8Bytes(draft) }}/8192
        B</span
      >
      <button
        v-if="draftStatus === 'failed'"
        class="ops-btn"
        @click="emit('save')"
      >
        {{ t('opsChat.save') }}
      </button>
      <button
        v-if="active"
        class="ops-btn ops-danger"
        :disabled="disabled"
        @click="emit('stop')"
      >
        <PlayerStop />{{ t('opsChat.stop') }}
      </button>
    </div>
    <div v-if="draftStatus === 'conflict'" class="ops-row">
      <button class="ops-btn" @click="emit('remote')">
        {{ t('opsChat.useRemote') }}
      </button>
      <button class="ops-btn" @click="emit('local')">
        {{ t('opsChat.keepLocal') }}
      </button>
    </div>
    <div v-if="pending" class="ops-row">
      <span class="ops-muted ops-grow">{{ t('opsChat.ambiguous') }}</span>
      <button class="ops-btn" :disabled="disabled" @click="emit('resend')">
        <Refresh />{{ t('opsChat.retrySubmission') }}
      </button>
    </div>
  </section>
</template>
<style scoped src="./opsChat.css"></style>
<style scoped>
.composer {
  padding: 12px 16px;
  border-top: 1px solid var(--color-border);
  flex: none;
  max-height: 55%;
  overflow: auto;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.input-row {
  display: flex;
  align-items: flex-end;
  gap: 8px;
  min-width: 0;
}
.draft {
  resize: vertical;
  min-height: 72px;
  max-height: 140px;
  flex: 1;
  line-height: 1.5;
}
@media (max-width: 600px) {
  .composer input,
  .composer textarea {
    font-size: 16px;
  }
}
.composer p {
  margin: 0;
}
</style>
