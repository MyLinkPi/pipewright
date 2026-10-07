import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import VarsCacheTab from './VarsCacheTab.vue'
import type { BuildConfig } from '../../api/pipelineSettings'

function buildFixture(): BuildConfig {
  return {
    model: 'dockerfile',
    dockerfilePath: 'Dockerfile',
    toolchain: { language: 'go', version: '1.22' },
    artifactType: 'image',
    vars: [{ id: '', key: '', secret: false, value: '' }],
    cache: { enabled: false, paths: [] },
  }
}

/** 模拟 ProjectPipeline 的回写:取最近一次 emit 的值原样写回 :build。 */
async function echoUpdate(wrapper: Awaited<ReturnType<typeof mountTab>>) {
  const last = wrapper.emitted('update')?.at(-1)?.[0] as BuildConfig | undefined
  if (last) await wrapper.setProps({ build: last })
  await nextTick()
  await nextTick()
}

function mountTab(build: BuildConfig) {
  // attachTo:VTU 默认挂游离节点,focus() 不生效;挂到 document 才能断言焦点行为。
  // i18n 已由 src/test/setup.ts 全局安装,这里不再重复传 global.plugins。
  return mount(VarsCacheTab, {
    props: { build, credentials: [] },
    attachTo: document.body,
  })
}

describe('VarsCacheTab 输入焦点保持', () => {
  it('变量 KEY 每敲一个字符,输入框 DOM 不被重建(不失焦)', async () => {
    const w = mountTab(buildFixture())
    await nextTick()
    const first = w.find('input.vk').element as HTMLInputElement
    first.focus()

    let text = ''
    for (const ch of ['T', 'E', 'S', 'T']) {
      text += ch
      await w.find('input.vk').setValue(text)
      await echoUpdate(w)
      const now = w.find('input.vk').element as HTMLInputElement
      expect(now).toBe(first)
      expect(document.activeElement).toBe(now)
    }
    expect(first.value).toBe('TEST')
  })

  it('外部真变更(如保存后回填)仍会重建并同步显示', async () => {
    const w = mountTab(buildFixture())
    await nextTick()
    const changed = buildFixture()
    changed.vars = [{ id: 'v1', key: 'GOPROXY', secret: false, value: 'https://goproxy.io' }]
    await w.setProps({ build: changed })
    await nextTick()
    await nextTick()
    const input = w.find('input.vk').element as HTMLInputElement
    expect(input.value).toBe('GOPROXY')
  })
})
