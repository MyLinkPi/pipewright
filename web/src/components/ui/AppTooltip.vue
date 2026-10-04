<script setup lang="ts">
/**
 * AppTooltip — keyboard-accessible hover/focus tooltip.
 * Content shown above trigger by default.
 * Uses CSS-only for positioning; v-show for keyboard accessibility.
 * Escape key hides tooltip when focused.
 */
import { ref } from 'vue'

withDefaults(defineProps<{
  content: string
  placement?: 'top' | 'bottom'
  /** > 0 时气泡限宽换行(长句提示);默认 0 = 单行 nowrap。 */
  maxWidth?: number
}>(), {
  placement: 'top',
  maxWidth: 0,
})

const visible = ref(false)

function show() { visible.value = true }
function hide() { visible.value = false }

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape') hide()
}
</script>

<template>
  <span
    class="tooltip-wrap"
    :class="`tooltip-wrap--${placement}`"
    @mouseenter="show"
    @mouseleave="hide"
    @focusin="show"
    @focusout="hide"
    @keydown="onKeydown"
  >
    <!-- Tooltip bubble -->
    <span
      v-show="visible"
      class="tooltip-bubble"
      :class="{ 'tooltip-bubble--wrap': maxWidth > 0 }"
      :style="maxWidth > 0 ? { maxWidth: `${maxWidth}px` } : undefined"
      role="tooltip"
    >{{ content }}</span>

    <!-- Trigger element -->
    <slot />
  </span>
</template>

<style scoped>
.tooltip-wrap {
  position: relative;
  display: inline-flex;
}

.tooltip-bubble {
  position: absolute;
  left: 50%;
  translate: -50% 0;
  background: var(--color-term);
  color: oklch(92% 0.01 270);
  border: 1px solid var(--color-border-strong);
  font-size: 0.74rem;
  font-family: var(--font-sans);
  padding: 6px 10px;
  border-radius: 8px;
  white-space: nowrap;
  box-shadow: var(--shadow);
  pointer-events: none;
  z-index: 1000;
  /* Enter animation */
  animation: tt-in 0.12s var(--ease-out-expo) both;
}

/* Arrow */
.tooltip-bubble::after {
  content: '';
  position: absolute;
  left: 50%;
  translate: -50% 0;
  width: 8px;
  height: 8px;
  background: var(--color-term);
  border-right: 1px solid var(--color-border-strong);
  border-bottom: 1px solid var(--color-border-strong);
}

/* 限宽换行变体:长句提示(白空间从 nowrap 放开) */
.tooltip-bubble--wrap {
  white-space: normal;
  width: max-content;
  text-align: left;
  line-height: 1.55;
}

/* top placement */
.tooltip-wrap--top .tooltip-bubble {
  bottom: calc(100% + 8px);
}
.tooltip-wrap--top .tooltip-bubble::after {
  bottom: -5px;
  transform: translateX(-50%) rotate(45deg);
}

/* bottom placement */
.tooltip-wrap--bottom .tooltip-bubble {
  top: calc(100% + 8px);
}
.tooltip-wrap--bottom .tooltip-bubble::after {
  top: -5px;
  transform: translateX(-50%) rotate(-135deg);
}

@keyframes tt-in {
  from { opacity: 0; transform: translateY(4px); }
  to   { opacity: 1; transform: none; }
}

@media (prefers-reduced-motion: reduce) {
  .tooltip-bubble {
    animation: none;
  }
}
</style>
