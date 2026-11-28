import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import AiOpsServerPicker from './AiOpsServerPicker.vue'
import type { Server } from '../../api/servers'

const servers: Server[] = [
  {
    id: 'a',
    name: 'A',
    host: 'host',
    port: 22,
    user: 'fixture',
    credentialId: 'c',
    credentialName: 'fixture',
    // fork 的 Server 含必填的 sudo/跳板链/构建池/GPU/服务器池字段(上游无)。
    sudoCredentialId: '',
    sudoCredentialName: '',
    jumps: [],
    labels: '',
    poolId: 'pool-1',
    poolName: 'default',
    kind: 'standard',
    maxBuilds: 0,
    priority: 0,
    gpu: false, sharedBuild: false,
    createdAt: '',
    updatedAt: '',
  },
]
describe('server picker opening', () => {
  it('waits for the restored selection instead of opening during loading', async () => {
    const w = mount(AiOpsServerPicker, {
      props: { servers, selected: [], disabled: true },
    })
    expect(w.get('details').attributes('open')).toBeUndefined()
    await w.setProps({ selected: ['a'], disabled: false })
    expect(w.get('details').attributes('open')).toBeUndefined()
    w.unmount()
  })
  it('opens an empty selection and stays open after the first choice', async () => {
    const w = mount(AiOpsServerPicker, {
      props: { servers, selected: [], disabled: true },
    })
    await w.setProps({ disabled: false })
    expect(w.get('details').attributes('open')).toBeDefined()
    await w.setProps({ selected: ['a'] })
    expect(w.get('details').attributes('open')).toBeDefined()
    w.unmount()
  })
  it('omits the port for the control-node entry (port 0 is not a display address)', async () => {
    const local = { ...servers[0]!, id: '__local__', name: '本机(控制节点)', host: 'localhost', port: 0 }
    const w = mount(AiOpsServerPicker, {
      props: { servers: [...servers, local], selected: ['__local__'], disabled: false },
    })
    const text = w.text()
    expect(text).toContain('localhost')
    expect(text).not.toContain('localhost:0')
    // 登记服务器仍带端口。
    expect(text).toContain('host:22')
    w.unmount()
  })
})
