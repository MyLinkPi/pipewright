import { describe, it, expect } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import JobDrawer from './JobDrawer.vue'
import LabelSelectorEditor from './LabelSelectorEditor.vue'
import type { PipelineJob, PipelineStage } from '../../api/pipeline'
import type { Server } from '../../api/servers'

const stage: PipelineStage = { id: 's1', name: '部署', kind: 'deploy', jobs: [] }

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
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  }
}

/** deploy_ssh:schema 里带 labelSelector 字段(key = 'selector');非脚本类 → 无构建机区块。 */
function deployJob(config: Record<string, string>): PipelineJob {
  return { id: 'j1', name: '部署', type: 'deploy_ssh', summary: '', config }
}

describe('JobDrawer — labelSelector 字段绑定走 field.key', () => {
  it('回显读 field.key 对应键,编辑写回同一键', async () => {
    const w = mount(JobDrawer, {
      props: {
        job: deployJob({ selector: 'env=prod' }),
        stage,
        servers: [serverFixture('web,env=prod,env=stage')],
      },
    })
    await nextTick()

    const editor = w.findComponent(LabelSelectorEditor)
    expect(editor.exists()).toBe(true)
    expect(editor.props('modelValue')).toBe('env=prod')

    editor.vm.$emit('update:modelValue', 'env=stage')
    await nextTick()

    const last = w.emitted('update')!.at(-1)![0] as Partial<PipelineJob>
    expect(last.config!.selector).toBe('env=stage')
  })
})
