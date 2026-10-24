/**
 * AppStore.test.ts — 应用商店页。
 *
 * 覆盖:Gitea 部署对话框的 ssh_domain 宿主地址预填联动(单机自动选中即预填、切机跟随、
 * 手改不覆盖、清空恢复);实例名维度(重名提醒出现/消失、默认名已存在也提醒、拉取失败
 * 静默、非法实例名禁用提交、实例随部署提交);已部署实例区(行渲染/徽标/不可达占位、
 * 点「升级」直达升级模式并按实例记录提交 upgrade)。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'

const h = vi.hoisted(() => ({
  listAppTemplates: vi.fn(),
  createAppTemplate: vi.fn(),
  deleteAppTemplate: vi.fn(),
  deployApp: vi.fn(),
  upgradeApp: vi.fn(),
  uninstallApp: vi.fn(),
  getAppUpgradeContext: vi.fn(),
  listAppInstances: vi.fn(),
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
  upgradeApp: (...a: unknown[]) => h.upgradeApp(...a),
  uninstallApp: (...a: unknown[]) => h.uninstallApp(...a),
  getAppUpgradeContext: (...a: unknown[]) => h.getAppUpgradeContext(...a),
  listAppInstances: (...a: unknown[]) => h.listAppInstances(...a),
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
import type { AppTemplate, AppInstance } from '../api/apps'
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
  // 已部署实例区默认为空(隐藏),不影响既有断言。
  h.listAppInstances.mockResolvedValue({ items: [] })
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

// mkInstance 构造「已部署实例」条目(默认 = 机器B 上的 gitea-2,模板完好)。
function mkInstance(over: Partial<AppInstance> = {}): AppInstance {
  return {
    serverId: 's2',
    serverName: '机器B',
    reachable: true,
    error: '',
    stackName: 'gitea-2',
    templateId: 't1',
    templateName: 'gitea',
    displayName: 'Gitea',
    icon: '🍵',
    instance: '2',
    cpus: '1',
    memory: '512m',
    deployedAt: '2026-01-01T00:00:00Z',
    templateGone: false,
    upgradeAvailable: false,
    legacy: false,
    ...over,
  }
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

describe('AppStore 已部署实例区', () => {
  it('渲染实例行:栈名/机器/「模板有更新」徽标;模板已删禁用升级按钮;不可达机器占位提示', async () => {
    h.listAppInstances.mockResolvedValue({
      items: [
        mkInstance({ upgradeAvailable: true }),
        mkInstance({
          serverId: 's1', serverName: '机器A', stackName: 'old-app', templateId: 'gone',
          templateName: '', displayName: '', icon: '', instance: '', cpus: '', memory: '',
          deployedAt: '', templateGone: true,
        }),
        mkInstance({
          serverId: 's3', serverName: '机器C', reachable: false, stackName: '', templateId: '',
          templateName: '', displayName: '', icon: '', instance: '', cpus: '', memory: '',
          deployedAt: '',
        }),
      ],
    })
    const w = mount(AppStore)
    await flushPromises()

    expect(w.text()).toContain('已部署实例')
    expect(w.text()).toContain('gitea-2')
    expect(w.text()).toContain('机器B')
    expect(w.text()).toContain('模板有更新')
    expect(w.text()).toContain('模板已删除')
    expect(w.text()).toContain('机器C 不可达')

    // 模板完好的行:升级/卸载按钮都可点;模板已删的行:升级禁用、卸载仍可用(卸载不依赖模板)。
    const btns = w.findAll('.apps__inst button')
    expect(btns.length).toBe(4)
    const upEnabled = btns.filter((b) => b.text().includes('升级') && !(b.element as HTMLButtonElement).disabled)
    expect(upEnabled.length).toBe(1)
    const unEnabled = btns.filter((b) => b.text().includes('卸载') && !(b.element as HTMLButtonElement).disabled)
    expect(unEnabled.length).toBe(2)
  })

  it('实例区常显:空列表显示空态提示而非隐藏;加载失败显示错误与刷新入口', async () => {
    const w = mount(AppStore)
    await flushPromises()
    expect(w.text()).toContain('已部署实例')
    expect(w.text()).toContain('暂无实例')

    h.listAppInstances.mockRejectedValue(new Error('down'))
    await w.findAll('button').find((b) => b.text().includes('刷新'))!.trigger('click')
    await flushPromises()
    expect(w.text()).toContain('已部署实例加载失败')
  })

  it('点「升级」→ 打开弹窗直接进入升级模式:预填边车参数与实例名,提交走 upgrade 端点', async () => {
    h.listAppInstances.mockResolvedValue({ items: [mkInstance()] })
    h.getAppUpgradeContext.mockResolvedValue({
      found: true,
      stackName: 'gitea-2',
      params: { version: '28.2.0', port: '3000', ssh_port: '2222', ssh_domain: '10.0.0.2' },
      secrets: {},
      cpus: '1',
      memory: '512m',
    })
    h.upgradeApp.mockResolvedValue({ serverId: 's2', name: 'gitea-2', ok: true, output: '', error: '' })

    const w = mount(AppStore)
    await flushPromises()
    const rowBtn = w.findAll('button').find((b) => b.text().includes('升级'))!
    await rowBtn.trigger('click')
    await flushPromises()

    // 弹窗打开且处于升级模式(徽标 + 升级提示),实例名已按实例记录填入并被禁用。
    expect(w.text()).toContain('升级模式')
    expect(w.text()).toContain('gitea-2')
    expect(instanceInput(w).value()).toBe('2')
    // 参数预填来自 upgrade-context(28.2.0)而非模板默认值(28.1.0)。
    const verLabel = w.findAll('label.field').find((l) => l.text().includes('镜像版本'))!
    expect((verLabel.find('input').element as HTMLInputElement).value).toBe('28.2.0')

    // 提交走 upgrade 端点,机器与实例名取自实例记录。
    await w.find('form').trigger('submit')
    await flushPromises()
    expect(h.upgradeApp).toHaveBeenCalledTimes(1)
    const args = h.upgradeApp.mock.calls[0]
    expect(args[0]).toBe('s2')
    expect(args[1]).toBe('t1')
    expect(args[4]).toBe('2')
    expect(w.text()).toContain('升级成功')
  })

  it('升级模式 secret:现值预填 + 显示切换可查明文,placeholder 说明保持/覆盖语义', async () => {
    h.listAppInstances.mockResolvedValue({
      items: [mkInstance({ templateId: 't-sec', stackName: 'mysql', templateName: 'mysql', instance: '' })],
    })
    // gitea 模板无 secret 参数——借用升级上下文断言 secret 预填需模板带 secret;此处直接
    // 给 giteaTpl 追加一个 secret 参数的副本模板。
    const mysqlTpl = { ...giteaTpl, id: 't-sec', name: 'mysql', params: [...giteaTpl.params, { name: 'password', label: '数据库口令', type: 'secret' as const, required: true, autoGenerate: true }] }
    h.listAppTemplates.mockResolvedValue({ items: [mysqlTpl] })
    h.getAppUpgradeContext.mockResolvedValue({
      found: true,
      stackName: 'mysql',
      params: {},
      secrets: { password: '0ld-p4ss' },
      cpus: '',
      memory: '',
    })
    const w = mount(AppStore)
    await flushPromises()
    await w.findAll('button').find((b) => b.text().includes('升级'))!.trigger('click')
    await flushPromises()

    expect(w.text()).toContain('已预填当前口令')
    const secLabel = w.findAll('label.field').find((l) => l.text().includes('数据库口令'))!
    const secInput = secLabel.find('input')
    // 现值默认明文(值已预填在表单,打码只是视觉障碍);placeholder 说明保持/覆盖语义;
    // 点「隐藏」后切回 password。
    expect((secInput.element as HTMLInputElement).placeholder).toContain('不动 = 保持现有口令')
    expect((secInput.element as HTMLInputElement).type).toBe('text')
    expect((secInput.element as HTMLInputElement).value).toBe('0ld-p4ss')
    await secLabel.findAll('button').find((b) => b.text().includes('隐藏'))!.trigger('click')
    await nextTick()
    expect((secInput.element as HTMLInputElement).type).toBe('password')
  })

  it('旧版部署(legacy)行:显示「旧版部署」徽标与「重新部署」按钮,点击打开部署弹窗(非升级模式)', async () => {
    h.listAppInstances.mockResolvedValue({ items: [mkInstance({ legacy: true, deployedAt: '', upgradeAvailable: false })] })
    const w = mount(AppStore)
    await flushPromises()

    expect(w.text()).toContain('旧版部署')
    const rowBtn = w.findAll('button').find((b) => b.text().includes('重新部署'))!
    expect(rowBtn).toBeTruthy()
    await rowBtn.trigger('click')
    await flushPromises()

    // 打开的是全新部署弹窗:无升级模式徽标,提交按钮为「部署」,实例名按记录预填。
    expect(w.text()).not.toContain('升级模式')
    expect(w.find('form').findAll('button').some((b) => b.text().includes('部署'))).toBe(true)
    expect(instanceInput(w).value()).toBe('2')
  })

  it('单实例 legacy 行(instance 键被 omitempty 省略):点击重新部署不报错,实例名为空', async () => {
    // 后端对空实例名省略 instance 键(undefined),不能让 .trim() 崩掉栈名 computed。
    h.listAppInstances.mockResolvedValue({
      items: [mkInstance({ legacy: true, instance: undefined, stackName: 'gitea', deployedAt: '' })],
    })
    const w = mount(AppStore)
    await flushPromises()
    const rowBtn = w.findAll('button').find((b) => b.text().includes('重新部署'))!
    await rowBtn.trigger('click')
    await flushPromises()
    expect(w.text()).not.toContain('升级模式')
    expect(instanceInput(w).value()).toBe('')
  })

  it('legacy 重新部署:旧 compose 反推值预填参数(含 secret 明文);反推不到的 ssh_domain 回落目标机地址', async () => {
    h.listAppInstances.mockResolvedValue({ items: [mkInstance({ legacy: true, deployedAt: '' })] })
    // 模板带 secret 参数(legacy 反推保持原口令的场景)。
    h.listAppTemplates.mockResolvedValue({
      items: [{ ...giteaTpl, params: [...giteaTpl.params, { name: 'password', label: '数据库口令', type: 'secret' as const, required: true, autoGenerate: true }] }],
    })
    // 反推上下文:version 反推出 28.2.0;ssh_domain 未反推出(留空);password 反推出旧口令。
    h.getAppUpgradeContext.mockResolvedValue({
      found: false,
      legacy: true,
      stackName: 'gitea-2',
      params: { version: '28.2.0', password: '0ld-p4ss' },
      secrets: {},
      cpus: '',
      memory: '',
    })
    const w = mount(AppStore)
    await flushPromises()
    await w.findAll('button').find((b) => b.text().includes('重新部署'))!.trigger('click')
    await flushPromises()

    // 反推值优先;ssh_domain 两头都没有 → prefillHostParams 回落目标机(机器B 10.0.0.2)。
    const verLabel = w.findAll('label.field').find((l) => l.text().includes('镜像版本'))!
    expect((verLabel.find('input').element as HTMLInputElement).value).toBe('28.2.0')
    expect(sshDomainInput(w).value()).toBe('10.0.0.2')
    // secret 反推值明文预填(重新部署保持原口令,不必重新生成),可隐藏。
    const secLabel = w.findAll('label.field').find((l) => l.text().includes('数据库口令'))!
    const secInput = secLabel.find('input')
    expect((secInput.element as HTMLInputElement).type).toBe('text')
    expect((secInput.element as HTMLInputElement).value).toBe('0ld-p4ss')
    await secLabel.findAll('button').find((b) => b.text().includes('隐藏'))!.trigger('click')
    await nextTick()
    expect((secInput.element as HTMLInputElement).type).toBe('password')
  })
})

describe('AppStore 卸载', () => {
  it('点「卸载」→ 弹窗默认保留数据卷;勾选删卷出现警示,提交带 stackName 与 deleteVolumes,成功后刷新实例区', async () => {
    h.listAppInstances.mockResolvedValue({ items: [mkInstance()] })
    h.uninstallApp.mockResolvedValue({
      serverId: 's2', name: 'gitea-2', ok: true, output: '已删除容器 1 个;已清理受管目录', error: '',
    })
    const w = mount(AppStore)
    await flushPromises()

    await w.findAll('button').find((b) => b.text().includes('卸载'))!.trigger('click')
    await nextTick()

    // 弹窗打开:栈名/机器名可见;删卷默认不勾选 → 不可恢复警示不出现。
    expect(w.text()).toContain('gitea-2')
    expect(w.text()).toContain('机器B')
    const cb = w.find('input[type="checkbox"]')
    expect((cb.element as HTMLInputElement).checked).toBe(false)
    expect(w.text()).not.toContain('不可恢复')

    // 勾选删卷 → 警示出现;提交走 uninstall(serverId, stackName, true)。
    await cb.setValue(true)
    await nextTick()
    expect(w.text()).toContain('不可恢复')
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(h.uninstallApp).toHaveBeenCalledTimes(1)
    expect(h.uninstallApp.mock.calls[0]).toEqual(['s2', 'gitea-2', true])
    expect(h.toast.success).toHaveBeenCalledWith('已卸载')
    // 成功 → 弹窗关闭 + 实例区刷新(再次拉取)。
    expect(w.text()).not.toContain('确认卸载')
    expect(h.listAppInstances).toHaveBeenCalledTimes(2)
  })

  it('卸载失败 → 弹窗保留并 toast 后端人读原因', async () => {
    h.listAppInstances.mockResolvedValue({ items: [mkInstance()] })
    h.uninstallApp.mockResolvedValue({
      serverId: 's2', name: 'gitea-2', ok: false, output: '', error: '删除容器失败:no such container',
    })
    const w = mount(AppStore)
    await flushPromises()
    await w.findAll('button').find((b) => b.text().includes('卸载'))!.trigger('click')
    await nextTick()
    await w.find('form').trigger('submit')
    await flushPromises()

    expect(h.toast.error).toHaveBeenCalledWith('删除容器失败:no such container')
    expect(w.text()).toContain('确认卸载') // 弹窗仍在,可重试
  })
})
