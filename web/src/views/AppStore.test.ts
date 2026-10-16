/**
 * AppStore.test.ts — 应用商店页。
 *
 * 覆盖:Gitea 部署对话框的 ssh_domain 宿主地址预填联动——单机自动选中即预填、
 * 切换目标机未手改时跟随刷新、手改后的自定义值不被覆盖、清空后恢复跟随。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'

const h = vi.hoisted(() => ({
  listAppTemplates: vi.fn(),
  createAppTemplate: vi.fn(),
  deleteAppTemplate: vi.fn(),
  deployApp: vi.fn(),
  listServers: vi.fn(),
  listDomains: vi.fn(),
  getGateway: vi.fn(),
  ensureService: vi.fn(),
  listInstances: vi.fn(),
  addInstance: vi.fn(),
  removeInstance: vi.fn(),
  confirmOpen: vi.fn(),
  push: vi.fn(),
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warn: vi.fn() },
}))

vi.mock('../api/apps', () => ({
  listAppTemplates: (...a: unknown[]) => h.listAppTemplates(...a),
  createAppTemplate: (...a: unknown[]) => h.createAppTemplate(...a),
  deleteAppTemplate: (...a: unknown[]) => h.deleteAppTemplate(...a),
  deployApp: (...a: unknown[]) => h.deployApp(...a),
}))
vi.mock('../api/servers', () => ({ listServers: (...a: unknown[]) => h.listServers(...a) }))
vi.mock('../api/servicereg', () => ({
  listDomains: (...a: unknown[]) => h.listDomains(...a),
  getGateway: (...a: unknown[]) => h.getGateway(...a),
  ensureService: (...a: unknown[]) => h.ensureService(...a),
  listInstances: (...a: unknown[]) => h.listInstances(...a),
  addInstance: (...a: unknown[]) => h.addInstance(...a),
  removeInstance: (...a: unknown[]) => h.removeInstance(...a),
}))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: h.push }) }))
vi.mock('../composables/useToast', () => ({
  useToast: () => ({ ...h.toast, toasts: { value: [] }, dismiss: vi.fn(), clear: vi.fn() }),
}))
vi.mock('../composables/useConfirm', () => ({
  useConfirm: () => ({ open: h.confirmOpen, pending: { value: null }, _resolve: vi.fn() }),
}))

import AppStore from './AppStore.vue'
import AppSelect from '../components/ui/AppSelect.vue'
import type { AppTemplate } from '../api/apps'
import type { Server } from '../api/servers'

const giteaTpl: AppTemplate = {
  id: 't1',
  name: 'gitea',
  displayName: 'Gitea',
  description: '',
  icon: '🍵',
  composeYaml: '',
  builtin: true,
  createdAt: '',
  updatedAt: '',
  params: [
    { name: 'version', label: '镜像版本', type: 'string', default: '28.1.0', required: false },
    { name: 'port', label: 'Web 端口', type: 'int', default: '3000', required: false },
    { name: 'ssh_port', label: 'SSH 端口', type: 'int', default: '2222', required: false },
    { name: 'ssh_domain', label: 'SSH 克隆域名', type: 'string', required: true },
  ],
}

function mkServer(id: string, name: string, host: string): Server {
  return { id, name, host, port: 22, user: 'root', credentialId: 'c1', credentialName: 'k' } as unknown as Server
}

beforeEach(() => {
  vi.clearAllMocks()
  h.listAppTemplates.mockResolvedValue({ items: [giteaTpl] })
  h.listServers.mockResolvedValue([mkServer('s1', '机器A', '10.0.0.1'), mkServer('s2', '机器B', '10.0.0.2')])
  h.listDomains.mockResolvedValue({ items: [] })
  h.getGateway.mockResolvedValue({ configured: false, servers: [] })
})

// openGitea 挂载页面并点开 Gitea 部署对话框。
async function openGitea() {
  const w = mount(AppStore)
  await flushPromises()
  const btn = w.findAll('button').find((b) => b.text().includes('部署'))!
  await btn.trigger('click')
  await nextTick()
  return w
}

// sshDomainInput 定位 SSH 克隆域名参数的输入框。
function sshDomainInput(w: ReturnType<typeof mount>) {
  const label = w.findAll('label.field').find((l) => l.text().includes('SSH 克隆域名'))!
  const input = label.find('input')
  return { value: () => (input.element as HTMLInputElement).value, set: (v: string) => input.setValue(v) }
}

// selectServer 模拟在目标机下拉里选中一台机器(AppSelect 声明的事件名为 update:modelValue)。
async function selectServer(w: ReturnType<typeof mount>, id: string) {
  const sel = w.findAllComponents(AppSelect)[0]
  ;(sel.vm as unknown as { $emit: (e: string, v: string) => void }).$emit('update:modelValue', id)
  await nextTick()
}

describe('AppStore ssh_domain 预填联动', () => {
  it('未选机器时为空;选机预填该机地址,切换目标机自动跟随刷新', async () => {
    const w = await openGitea()
    const field = sshDomainInput(w)
    expect(field.value()).toBe('')

    await selectServer(w, 's1')
    expect(field.value()).toBe('10.0.0.1')

    await selectServer(w, 's2')
    expect(field.value()).toBe('10.0.0.2')

    await selectServer(w, 's1')
    expect(field.value()).toBe('10.0.0.1')
  })

  it('手改 ssh_domain 后切换机器不覆盖自定义值;清空后恢复跟随', async () => {
    const w = await openGitea()
    const field = sshDomainInput(w)
    await selectServer(w, 's1')
    expect(field.value()).toBe('10.0.0.1')

    await field.set('git.example.com')
    await selectServer(w, 's2')
    expect(field.value()).toBe('git.example.com')

    await field.set('')
    await selectServer(w, 's1')
    expect(field.value()).toBe('10.0.0.1')
  })

  it('仅一台机器时打开对话框即预填该机地址', async () => {
    h.listServers.mockResolvedValue([mkServer('only', '唯一', '192.168.1.9')])
    const w = await openGitea()
    expect(sshDomainInput(w).value()).toBe('192.168.1.9')
  })
})
