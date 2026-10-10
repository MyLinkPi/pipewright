import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import LabelSelectorEditor from './LabelSelectorEditor.vue'
import type { Server } from '../../api/servers'

function serverFixture(labels: string): Server {
  return {
    id: 'srv-1',
    name: 'web-1',
    host: '10.0.0.1',
    port: 22,
    user: 'deploy',
    credentialId: 'cred-1',
    credentialName: 'key',
    sudoCredentialId: '',
    sudoCredentialName: '',
    jumps: [],
    labels,
    maxBuilds: 0,
    priority: 0,
    gpu: false,
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  }
}

function mountEditor(modelValue: string, labels = 'web,env=prod,env=stage') {
  return mount(LabelSelectorEditor, {
    props: { modelValue, servers: [serverFixture(labels)] },
  })
}

describe('LabelSelectorEditor 回显(重新打开已保存的选择器)', () => {
  it('挂载时 modelValue 已有值 → 反解出全部条件行(k=v 与纯标记)', async () => {
    const w = mountEditor('web,env=prod')
    await nextTick()

    const keys = w.findAll('select.selector-key')
    expect(keys.length).toBe(2)
    expect((keys[0].element as HTMLSelectElement).value).toBe('web')
    expect((keys[1].element as HTMLSelectElement).value).toBe('env')

    // web 是纯标记 → 无 value 下拉;env 是 k=v → value 选中 prod。
    const values = w.findAll('select.selector-value')
    expect(values.length).toBe(1)
    expect((values[0].element as HTMLSelectElement).value).toBe('prod')
  })

  it('挂载空值 → 无条件行(只有「添加」按钮)', async () => {
    const w = mountEditor('')
    await nextTick()
    expect(w.findAll('select.selector-key').length).toBe(0)
    expect(w.find('button.selector-add').exists()).toBe(true)
  })

  it('自身 emit 的等价回写(回声)不重建行;外部真变更才重解', async () => {
    const w = mountEditor('')
    await nextTick()

    // 用户点「添加标签条件」→ 立即 emit 首个标签项。
    await w.find('button.selector-add').trigger('click')
    const emitted = w.emitted('update:modelValue')?.at(-1)?.[0] as string
    expect(emitted).toBe('env=prod')

    // 父级以相同值回写(回声)→ 行 DOM 不重建。
    const before = w.find('select.selector-key').element
    await w.setProps({ modelValue: emitted })
    await nextTick()
    expect(w.find('select.selector-key').element).toBe(before)

    // 外部真变更(切 job / 保存回填别的值)→ 全量重解。
    await w.setProps({ modelValue: 'web' })
    await nextTick()
    const keys = w.findAll('select.selector-key')
    expect(keys.length).toBe(1)
    expect((keys[0].element as HTMLSelectElement).value).toBe('web')
    expect(w.findAll('select.selector-value').length).toBe(0) // 纯标记无 value 下拉
  })
})

describe('LabelSelectorEditor 陈旧值回显(标签后来被删)', () => {
  it('row.v 不在候选 → 补原始值 option,下拉选中态与 model 一致(不显示分叉)', async () => {
    // 候选只有 env=prod / env=stage,已保存值 env=legacy 已被删除。
    const w = mountEditor('env=legacy')
    await nextTick()

    const sel = w.find('select.selector-value')
    expect(sel.exists()).toBe(true)
    const el = sel.element as HTMLSelectElement
    // 修复前:下拉显示第一个候选(prod)但 model 仍是 legacy —— 显示与保存值分叉。
    expect(el.value).toBe('legacy')
    expect(Array.from(el.options).map((o) => o.value)).toContain('legacy')
  })

  it('row.k 不在候选 → 补原始 key option', async () => {
    const w = mountEditor('oldzone=1')
    await nextTick()

    const sel = w.find('select.selector-key')
    expect(sel.exists()).toBe(true)
    const el = sel.element as HTMLSelectElement
    expect(el.value).toBe('oldzone')
    expect(Array.from(el.options).map((o) => o.value)).toContain('oldzone')
  })

  it('候选齐全时不产生多余兜底 option', async () => {
    const w = mountEditor('env=prod')
    await nextTick()
    const el = w.find('select.selector-value').element as HTMLSelectElement
    expect(Array.from(el.options).map((o) => o.value)).toEqual(['prod', 'stage'])
  })
})
