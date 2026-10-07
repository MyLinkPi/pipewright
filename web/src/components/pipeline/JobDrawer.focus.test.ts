import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import JobDrawer from './JobDrawer.vue'
import type { PipelineJob, PipelineStage } from '../../api/pipeline'

function jobFixture(): PipelineJob {
  return {
    id: 'job_1',
    name: 'test-job',
    type: 'custom',
    summary: '',
    config: {
      image: 'node:20',
      commands: 'npm test',
      // schema 外的键 → 高级区 KV 行(custom 的 schema 字段 = image/commands/runner)
      MY_PARAM: 'v1',
      OTHER_PARAM: 'v2',
    },
  }
}

function stageFixture(): PipelineStage {
  return { id: 'stg_1', name: 'build', kind: 'build', jobs: [] }
}

async function mountDrawer(job: PipelineJob) {
  // attachTo:VTU 默认挂游离节点,focus() 不生效;挂到 document 才能断言焦点行为。
  const wrapper = mount(JobDrawer, {
    props: { job, stage: stageFixture() },
    attachTo: document.body,
  })
  await nextTick()
  await nextTick()
  return wrapper
}

/** 模拟 PipelineCanvas.updateJob 的回写:以新对象引用写回 :job。 */
async function echoUpdate(wrapper: Awaited<ReturnType<typeof mountDrawer>>, job: PipelineJob) {
  const patch = wrapper.emitted('update')?.at(-1)?.[0] as Partial<PipelineJob> | undefined
  if (patch) await wrapper.setProps({ job: { ...job, ...patch } })
  await nextTick()
  await nextTick()
}

describe('JobDrawer 高级区 KV 输入焦点保持', () => {
  it('blur 提交回写后,KV 行 DOM 不被重建', async () => {
    const job = jobFixture()
    const w = await mountDrawer(job)
    const before = w.findAll('input.kv-input').map((i) => i.element)

    await w.findAll('input.kv-input')[0].setValue('MY_KEY')
    await w.findAll('input.kv-input')[0].trigger('blur')
    await echoUpdate(w, job)

    const after = w.findAll('input.kv-input').map((i) => i.element)
    expect(after.length).toBe(before.length)
    for (let i = 0; i < after.length; i++) expect(after[i]).toBe(before[i])
    expect((after[0] as HTMLInputElement).value).toBe('MY_KEY')
  })

  it('blur 提交回写后,正要交互的另一行输入框焦点不丢', async () => {
    const job = jobFixture()
    const w = await mountDrawer(job)
    const inputs = w.findAll('input.kv-input')
    ;(inputs[2].element as HTMLInputElement).focus() // 用户点向第二行 KEY
    await inputs[0].trigger('blur') // 与此同时第一行 blur 提交
    await echoUpdate(w, job)
    expect(document.activeElement).toBe(inputs[2].element)
  })

  it('外部真变更(切换/换类型/保存回填)仍会全量重建', async () => {
    const job = jobFixture()
    const w = await mountDrawer(job)
    const before = w.findAll('input.kv-input').map((i) => i.element)

    const changed = jobFixture()
    changed.config = { ...changed.config, NEW_KEY: 'x' }
    await w.setProps({ job: changed })
    await nextTick()
    await nextTick()

    const rows = w.findAll('input.kv-input')
    expect(rows.length).toBe(6)
    expect(rows[0].element).not.toBe(before[0])
    expect((rows[4].element as HTMLInputElement).value).toBe('NEW_KEY')
  })

  it('本地名称被清空时外部改名,仍 hydrate 反映新名(空值回退用 prev)', async () => {
    const job = jobFixture()
    const w = await mountDrawer(job)
    const nameInput = w.find('input.drawer-input')

    // 用户清空名称框(v-model 即时生效,尚未 blur)。
    await nameInput.setValue('')
    // 外部仅改名到达(其余字段不变)。
    await w.setProps({ job: { ...job, name: 'renamed-job' } })
    await nextTick()
    await nextTick()

    expect((nameInput.element as HTMLInputElement).value).toBe('renamed-job')
  })
})
