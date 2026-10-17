/**
 * AppStore.test.ts — 应用商店页。
 *
 * 覆盖:Gitea 部署对话框的 ssh_domain 宿主地址预填联动(单机自动选中即预填、切机跟随、
 * 手改不覆盖、清空恢复);实例名维度(重名提醒出现/消失、默认名已存在也提醒、拉取失败
 * 静默、非法实例名禁用提交、实例随部署提交)。
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
  getAllContainers: vi.fn(),
  getAllServerMetrics: vi.fn(),
  listDomains: vi.fn(),
  getGateway: vi.fn(),
  ensureService: vi.fn(),
  listInstances: vi.fn(),
  addInstance: vi.fn(),
  removeInstance: vi.fn(),
  getServerStacks: vi.fn(),
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
vi.mock('../api/servers', () => ({
  listServers: (...a: unknown[]) => h.listServers(...a),
  getAllServerMetrics: (...a: unknown[]) => h.getAllServerMetrics(...a),
}))
vi.mock('../api/containers', () => ({
  getServerStacks: (...a: unknown[]) => h.getServerStacks(...a),
  getAllContainers: (...a: unknown[]) => h.getAllContainers(...a),
}))
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
  // 容量摘要数据源(选机器下拉的 hint):默认无数据 → 选项不带 hint,不影响既有断言。
  h.getAllContainers.mockResolvedValue([])
  h.getAllServerMetrics.mockResolvedValue([])
  h.listDomains.mockResolvedValue({ items: [] })
  h.getGateway.mockResolvedValue({ configured: false, servers: [] })
  h.getServerStacks.mockResolvedValue(mkStacks([]))
})

// mkStacks 构造 getServerStacks 的响应(给定栈名列表)。
function mkStacks(names: string[]) {
  return {
    serverId: 's1',
    reachable: true,
    runtime: 'docker',
    error: '',
    stacks: names.map((name) => ({ name, status: '', configFiles: [] as string[] })),
    collectedAt: '',
  }
}

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

// instanceInput 定位实例名输入框。
function instanceInput(w: ReturnType<typeof mount>) {
  const label = w.findAll('label.field').find((l) => l.text().includes('实例名'))!
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

  it('容量数据到位后,目标机下拉选项带容器数/负载/内存/磁盘 hint', async () => {
    h.getAllContainers.mockResolvedValue([
      { serverId: 's1', reachable: true, runtime: 'docker', error: '', containers: [], running: 3, total: 5, collectedAt: '' },
    ])
    h.getAllServerMetrics.mockResolvedValue([
      {
        serverId: 's1', reachable: true, error: '',
        cpu: { loadavg1: 0.42, cores: 8 },
        memory: { usedBytes: 4.5 * 1024 ** 3, totalBytes: 16 * 1024 ** 3 },
        disk: { path: '/', usedBytes: 380, totalBytes: 1000 },
        collectedAt: '',
      },
    ])
    const w = await openGitea()
    const opts = w.findAllComponents(AppSelect)[0].props('options') as Array<{ value: string; hint?: string }>
    const s1 = opts.find((o) => o.value === 's1')!
    expect(s1.hint).toContain('3') // 容器 3/5
    expect(s1.hint).toContain('0.42')
    expect(s1.hint).toContain('4.5 / 16 GiB')
    expect(s1.hint).toContain('38')
    // 无数据的机器 hint 为空串(选项照常可选,容量只是参考信息)。
    expect((opts.find((o) => o.value === 's2')!).hint).toBe('')
  })
})

describe('AppStore 实例名与重名提醒', () => {
  it('实例名与现有栈重名 → 显示提醒;改名/清空后消失', async () => {
    h.getServerStacks.mockResolvedValue(mkStacks(['gitea-2', 'mysql']))
    const w = await openGitea()
    await selectServer(w, 's1')
    await flushPromises()
    expect(w.text()).not.toContain('同名栈')

    const field = instanceInput(w)
    await field.set('2')
    await nextTick()
    expect(w.text()).toContain('同名栈')
    expect(w.text()).toContain('gitea-2')

    await field.set('3')
    await nextTick()
    expect(w.text()).not.toContain('同名栈')

    await field.set('')
    await nextTick()
    expect(w.text()).not.toContain('同名栈')
  })

  it('实例留空但默认栈已存在 → 仍提醒(重新部署=更新)', async () => {
    h.getServerStacks.mockResolvedValue(mkStacks(['gitea']))
    const w = await openGitea()
    await selectServer(w, 's1')
    await flushPromises()
    expect(w.text()).toContain('同名栈')
  })

  it('stacks 拉取失败 → 静默跳过提醒,不报错', async () => {
    h.getServerStacks.mockRejectedValue(new Error('down'))
    const w = await openGitea()
    await selectServer(w, 's1')
    await flushPromises()
    expect(w.text()).not.toContain('同名栈')
  })

  it('非法实例名 → 提示并禁用提交;合法实例随部署提交', async () => {
    h.deployApp.mockResolvedValue({
      serverId: 's1', name: 'gitea-2', ok: true, output: '', error: '',
      params: { ssh_domain: '10.0.0.1' },
    })
    const w = await openGitea()
    await selectServer(w, 's1')
    await flushPromises()

    const field = instanceInput(w)
    await field.set('-bad!')
    await nextTick()
    expect(w.text()).toContain('实例名非法')
    const submit = w.find('form').findAll('button').find((b) => b.text().includes('部署'))!
    expect((submit.element as HTMLButtonElement).disabled).toBe(true)

    await field.set('2')
    await nextTick()
    expect(w.text()).not.toContain('实例名非法')
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(h.deployApp).toHaveBeenCalledTimes(1)
    const args = h.deployApp.mock.calls[0]
    expect(args[0]).toBe('s1')
    expect(args[4]).toBe('2')
    expect(w.text()).toContain('部署成功')
  })

  it('注册到网关的端口预填优先 HTTP 管理端口而非协议端口', async () => {
    h.listDomains.mockResolvedValue({ items: [{ id: 'd1', baseDomain: 'example.com', hasCert: false }] })
    // deployRound 跑一遍「打开对话框 → 选机 → 提交 → 展示结果」并返回注册卡预填的端口。
    const deployRound = async (params: Record<string, string>): Promise<string> => {
      h.deployApp.mockResolvedValue({ serverId: 's1', name: 'gitea', ok: true, output: '', error: '', params })
      const w = await openGitea()
      await selectServer(w, 's1')
      await flushPromises()
      await w.find('form').trigger('submit')
      await flushPromises()
      const label = w.findAll('label.field').find((l) => l.text().includes('网关反代端口'))!
      return (label.find('input').element as HTMLInputElement).value
    }
    // rabbitmq:mgmt(15672)优于 amqp(5672);redpanda:console(8080)优于 kafka(9092);
    // minio/rustfs:api(9000)保持不变;gitea:port 本体优先。
    expect(await deployRound({ amqp_port: '5672', mgmt_port: '15672' })).toBe('15672')
    expect(await deployRound({ kafka_port: '9092', schema_port: '8081', console_port: '8080' })).toBe('8080')
    expect(await deployRound({ api_port: '9000', console_port: '9001' })).toBe('9000')
    expect(await deployRound({ port: '3000', ssh_port: '2222' })).toBe('3000')
  })
})
