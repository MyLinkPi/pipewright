import { afterEach, describe, expect, it } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import AppCombobox from './AppCombobox.vue'

let wrapper: VueWrapper | undefined

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const options = [
  { value: 'gpt-4o', label: 'GPT-4o' },
  { value: 'manual/model:1', label: 'Manual Model' },
  { value: 'llama3', label: 'Llama 3' },
]

function setup(props: Record<string, unknown> = {}) {
  wrapper = mount(AppCombobox, {
    props: { modelValue: '', options, ...props },
  })
  return wrapper
}

describe('AppCombobox', () => {
  it('emits free-typed input and filters options while typing', async () => {
    const w = setup()
    const input = w.get('input[role="combobox"]')
    await input.setValue('llama')
    expect(w.emitted('update:modelValue')?.[0]).toEqual(['llama'])
    // 键入即自动展开并按查询词过滤。
    expect(
      w.findAll('[role="option"]').map((o) => o.text()),
    ).toEqual(['Llama 3'])
    await input.setValue('nomatch')
    expect(w.find('[role="listbox"]').exists()).toBe(false)
  })
  it('chooses an option via click and keyboard, then shows all options on reopen', async () => {
    const w = setup({ modelValue: 'llama3' })
    await w.get('.app-combobox__toggle').trigger('click')
    expect(w.findAll('[role="option"]')).toHaveLength(3)
    await w.get('[role="option"]').trigger('click')
    expect(w.emitted('update:modelValue')?.[0]).toEqual(['gpt-4o'])
    expect(w.find('[role="listbox"]').exists()).toBe(false)
    await w.get('.app-combobox__toggle').trigger('click')
    expect(w.findAll('[role="option"]')).toHaveLength(3)
    await w.get('input[role="combobox"]').trigger('keydown', {
      key: 'ArrowDown',
    })
    await w.get('input[role="combobox"]').trigger('keydown', { key: 'Enter' })
    expect(w.emitted('update:modelValue')?.at(-1)).toEqual(['gpt-4o'])
  })
  it('ignores IME confirmation and closes on outside pointerdown', async () => {
    const w = setup()
    await w.get('.app-combobox__toggle').trigger('click')
    const input = w.get('input[role="combobox"]')
    await input.trigger('keydown', { key: 'Enter', isComposing: true })
    expect(w.emitted('update:modelValue')).toBeUndefined()
    document.dispatchEvent(
      new PointerEvent('pointerdown', { bubbles: true }),
    )
    await Promise.resolve()
    expect(w.find('[role="listbox"]').exists()).toBe(false)
  })
  it('disables the toggle and drops the menu while disabled', async () => {
    const w = setup({ modelValue: 'llama3' })
    expect(w.get('.app-combobox__toggle').attributes('disabled')).toBeUndefined()
    await w.get('.app-combobox__toggle').trigger('click')
    await w.setProps({ disabled: true })
    expect(w.find('[role="listbox"]').exists()).toBe(false)
    expect(w.get('input[role="combobox"]').attributes('disabled')).toBeDefined()
    expect(
      w.get('.app-combobox__toggle').attributes('disabled'),
    ).toBeDefined()
  })
})
