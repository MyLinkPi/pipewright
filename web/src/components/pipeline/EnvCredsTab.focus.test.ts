import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick, ref } from 'vue'

// useLabels 在 onMounted 拉标签登记处 → 换成本地 stub,避免打 http。
vi.mock('../../api/labels', () => ({
  useLabels: () => ({ items: ref<{ name: string }[]>([]), load: () => Promise.resolve() }),
}))

import EnvCredsTab from './EnvCredsTab.vue'
import type { Environment } from '../../api/pipelineSettings'

function envFixture(): Environment {
  return {
    id: 'env-1',
    name: '',
    targetServerIds: [],
    envVars: [{ id: '', key: '', secret: false, value: '' }],
    imageRegistry: { type: '', url: '' },
  }
}

async function mountTab(environments: Environment[]) {
  // attachTo:VTU 默认挂游离节点,focus() 不生效;挂到 document 才能断言焦点行为。
  // i18n 已由 src/test/setup.ts 全局安装,这里不再重复传 global.plugins。
  const wrapper = mount(EnvCredsTab, {
    props: { environments, credentials: [], servers: [] },
    attachTo: document.body,
  })
  await nextTick()
  return wrapper
}

/** 模拟 ProjectPipeline 的回写:取最近一次 emit 的值原样写回 :environments。 */
async function echoUpdate(wrapper: Awaited<ReturnType<typeof mountTab>>) {
  const last = wrapper.emitted('update')?.at(-1)?.[0] as Environment[] | undefined
  if (last) await wrapper.setProps({ environments: last })
  await nextTick()
  await nextTick()
}

describe('EnvCredsTab 输入焦点保持', () => {
  it('环境名每敲一个字符,输入框 DOM 不被重建(不失焦)', async () => {
    const w = await mountTab([envFixture()])
    const first = w.find('input.env-name').element as HTMLInputElement
    first.focus()

    let text = ''
    for (const ch of ['P', 'r', 'o', 'd']) {
      text += ch
      await w.find('input.env-name').setValue(text)
      await echoUpdate(w)
      const now = w.find('input.env-name').element as HTMLInputElement
      expect(now).toBe(first)
      expect(document.activeElement).toBe(now)
    }
    expect(first.value).toBe('Prod')
  })

  it('环境变量 KEY 每敲一个字符,输入框 DOM 不被重建(不失焦)', async () => {
    const w = await mountTab([envFixture()])
    const first = w.find('input.ev-k').element as HTMLInputElement
    first.focus()

    let text = ''
    for (const ch of ['T', 'E', 'S', 'T']) {
      text += ch
      await w.find('input.ev-k').setValue(text)
      await echoUpdate(w)
      const now = w.find('input.ev-k').element as HTMLInputElement
      expect(now).toBe(first)
      expect(document.activeElement).toBe(now)
    }
    expect(first.value).toBe('TEST')
  })

  it('外部真变更(如保存后回填)仍会重建并同步显示', async () => {
    const w = await mountTab([envFixture()])
    await w.setProps({ environments: [{ ...envFixture(), name: '生产' }] })
    await nextTick()
    await nextTick()
    const input = w.find('input.env-name').element as HTMLInputElement
    expect(input.value).toBe('生产')
  })
})
