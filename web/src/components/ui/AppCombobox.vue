<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'

defineOptions({ inheritAttrs: false })

export interface ComboboxOption {
  value: string
  label: string
}

const props = withDefaults(
  defineProps<{
    inputId?: string
    modelValue: string
    options: ComboboxOption[]
    placeholder?: string
    disabled?: boolean
    ariaLabel?: string
    mono?: boolean
    portal?: boolean
  }>(),
  {
    inputId: undefined,
    placeholder: '',
    disabled: false,
    ariaLabel: undefined,
    mono: false,
    portal: false,
  },
)

const emit = defineEmits<{
  'update:modelValue': [value: string]
  change: []
}>()

const root = ref<HTMLElement | null>(null)
const input = ref<HTMLInputElement | null>(null)
const menu = ref<HTMLElement | null>(null)
const open = ref(false)
const activeIndex = ref(-1)
const menuStyle = ref<Record<string, string>>({})
// 查询词仅在键入期间存在:选中/重新展开时清空,避免按已提交值过滤隐藏其余选项。
const query = ref('')

// 输入即过滤:当前文本同时是查询词;未输入时展示全量。
const visibleOptions = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return props.options
  return props.options.filter(
    (option) =>
      option.label.toLowerCase().includes(q) ||
      option.value.toLowerCase().includes(q),
  )
})
watch(
  () => props.disabled,
  (disabled) => {
    if (disabled) close()
  },
  { flush: 'sync' },
)
watch(visibleOptions, (options) => {
  if (!open.value) return
  const selectedIndex = options.findIndex(
    (option) => option.value === props.modelValue,
  )
  activeIndex.value =
    selectedIndex >= 0
      ? selectedIndex
      : Math.min(Math.max(0, activeIndex.value), options.length - 1)
  positionMenu()
}, { flush: 'sync' })

function positionMenu(): void {
  if (!props.portal || !root.value) return
  const rect = root.value.getBoundingClientRect()
  const availableBelow = window.innerHeight - rect.bottom - 8
  const availableAbove = rect.top - 8
  const desiredHeight = Math.min(240, props.options.length * 38 + 12)
  const above =
    availableBelow < desiredHeight && availableAbove > availableBelow
  const maxHeight = Math.max(
    0,
    Math.min(240, above ? availableAbove : availableBelow),
  )
  menuStyle.value = {
    left: `${rect.left}px`,
    width: `${rect.width}px`,
    top: above ? 'auto' : `${rect.bottom + 6}px`,
    bottom: above ? `${window.innerHeight - rect.top + 6}px` : 'auto',
    maxHeight: `${maxHeight}px`,
  }
}

function close(): void {
  open.value = false
  activeIndex.value = -1
  query.value = ''
}

function toggle(): void {
  if (props.disabled) return
  if (open.value) {
    close()
    return
  }
  query.value = ''
  positionMenu()
  open.value = true
  activeIndex.value = Math.max(
    0,
    visibleOptions.value.findIndex(
      (option) => option.value === props.modelValue,
    ),
  )
  input.value?.focus()
}

function choose(option: ComboboxOption): void {
  if (props.disabled) return
  query.value = ''
  emit('update:modelValue', option.value)
  emit('change')
  close()
}

function onInput(event: Event): void {
  query.value = (event.target as HTMLInputElement).value
  emit('update:modelValue', query.value)
  emit('change')
  if (!props.options.length) {
    close()
    return
  }
  if (!open.value) {
    positionMenu()
    open.value = true
  }
  activeIndex.value = 0
}

// 点击输入框即展开:与切换按钮一致的入口(键入自动展开,点击也应展开)。
function onFieldClick(): void {
  if (props.disabled || open.value || !props.options.length) return
  query.value = ''
  positionMenu()
  open.value = true
  activeIndex.value = Math.max(
    0,
    visibleOptions.value.findIndex(
      (option) => option.value === props.modelValue,
    ),
  )
}

function moveActive(direction: 1 | -1): void {
  if (!visibleOptions.value.length) return
  activeIndex.value =
    (activeIndex.value + direction + visibleOptions.value.length) %
    visibleOptions.value.length
}

function handleKeydown(event: KeyboardEvent): void {
  if (props.disabled || event.isComposing || event.keyCode === 229) return
  if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
    event.preventDefault()
    if (!open.value) toggle()
    else moveActive(event.key === 'ArrowDown' ? 1 : -1)
  } else if (event.key === 'Enter' && open.value) {
    event.preventDefault()
    const option = visibleOptions.value[activeIndex.value]
    if (option) choose(option)
  } else if (event.key === 'Escape' && open.value) {
    event.preventDefault()
    close()
  }
}

function handlePointerdown(event: PointerEvent): void {
  if (
    open.value &&
    root.value &&
    !root.value.contains(event.target as Node) &&
    !menu.value?.contains(event.target as Node)
  )
    close()
}

onMounted(() => {
  document.addEventListener('pointerdown', handlePointerdown)
  window.addEventListener('resize', positionMenu)
  window.addEventListener('scroll', positionMenu, true)
})
onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', handlePointerdown)
  window.removeEventListener('resize', positionMenu)
  window.removeEventListener('scroll', positionMenu, true)
})
</script>

<template>
  <div
    ref="root"
    class="app-combobox"
    :class="{ 'app-combobox--mono': mono, 'app-combobox--open': open }"
  >
    <input
      v-bind="$attrs"
      :id="inputId"
      ref="input"
      class="app-combobox__input"
      type="text"
      role="combobox"
      :value="modelValue"
      :placeholder="placeholder"
      :aria-label="ariaLabel"
      :aria-expanded="open"
      :aria-controls="inputId ? `${inputId}-options` : undefined"
      aria-haspopup="listbox"
      aria-autocomplete="list"
      :disabled="disabled"
      autocomplete="off"
      @input="onInput"
      @click="onFieldClick"
      @keydown="handleKeydown"
    />
    <button
      type="button"
      class="app-combobox__toggle"
      :tabindex="-1"
      :disabled="disabled || !options.length"
      :aria-label="ariaLabel"
      @click="toggle"
    >
      <svg
        class="app-combobox__chevron"
        width="15"
        height="15"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        aria-hidden="true"
      >
        <path d="m6 9 6 6 6-6" />
      </svg>
    </button>
    <Teleport to="body" :disabled="!portal">
      <div
        v-if="open && visibleOptions.length"
        :id="inputId ? `${inputId}-options` : undefined"
        ref="menu"
        class="app-combobox__menu"
        :class="{ 'app-combobox__menu--portal': portal }"
        :style="portal ? menuStyle : undefined"
        role="listbox"
      >
        <button
          v-for="(option, index) in visibleOptions"
          :key="option.value"
          type="button"
          class="app-combobox__option"
          :class="{
            'app-combobox__option--active': index === activeIndex,
            'app-combobox__option--selected': option.value === modelValue,
          }"
          role="option"
          :aria-selected="option.value === modelValue"
          @mouseenter="activeIndex = index"
          @click="choose(option)"
        >
          <span class="app-combobox__option-label">{{ option.label }}</span>
          <svg
            v-if="option.value === modelValue"
            class="app-combobox__check"
            width="15"
            height="15"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2.5"
            aria-hidden="true"
          >
            <path d="m5 12 4 4L19 6" />
          </svg>
        </button>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.app-combobox {
  position: relative;
  width: 100%;
}
.app-combobox__input {
  width: 100%;
  height: 38px;
  box-sizing: border-box;
  padding: 0 34px 0 12px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  background: var(--color-card-2);
  color: var(--color-text);
  font: inherit;
  text-align: left;
  transition:
    border-color var(--duration-fast),
    box-shadow var(--duration-fast);
}
.app-combobox--mono .app-combobox__input {
  font-family: var(--font-mono);
  font-size: 0.8rem;
}
.app-combobox__input:hover:not(:disabled) {
  border-color: var(--color-border-strong);
  background: var(--color-inset);
}
.app-combobox--open .app-combobox__input,
.app-combobox__input:focus-visible {
  outline: none;
  border-color: var(--color-primary);
  box-shadow: 0 0 0 3px var(--color-primary-soft);
}
.app-combobox__input:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}
.app-combobox__input::placeholder {
  color: var(--color-faint);
}
.app-combobox__toggle {
  position: absolute;
  top: 0;
  right: 0;
  display: flex;
  align-items: center;
  height: 38px;
  padding: 0 11px 0 4px;
  border: 0;
  background: transparent;
  color: var(--color-faint);
  cursor: pointer;
}
.app-combobox__toggle:disabled {
  cursor: default;
  opacity: 0.55;
}
.app-combobox__chevron {
  transition:
    transform var(--duration-fast),
    color var(--duration-fast);
}
.app-combobox--open .app-combobox__chevron {
  transform: rotate(180deg);
  color: var(--color-primary);
}
.app-combobox__menu {
  box-sizing: border-box;
  position: absolute;
  z-index: 20;
  top: calc(100% + 6px);
  left: 0;
  width: 100%;
  max-height: 240px;
  overflow-y: auto;
  padding: 5px;
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded);
  background: var(--color-card-2);
  box-shadow: 0 12px 28px rgb(0 0 0 / 24%);
}
.app-combobox__menu--portal {
  position: fixed;
  z-index: 10000;
}
.app-combobox__option {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  width: 100%;
  min-height: 38px;
  padding: 8px 9px;
  border: 0;
  border-radius: calc(var(--rounded) - 2px);
  background: transparent;
  color: var(--color-text);
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.app-combobox__option:hover,
.app-combobox__option--active {
  background: var(--color-inset);
}
.app-combobox__option--selected,
.app-combobox__check {
  color: var(--color-primary);
}
.app-combobox__option-label {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
