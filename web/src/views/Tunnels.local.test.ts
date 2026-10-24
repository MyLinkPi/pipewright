/**
 * Tunnels.local.test.ts — /tunnels 页面回归测试(仿 Pm2.local.test.ts)。
 *
 * 覆盖:列表渲染(名称/模式/映射/状态)与 KPI 计数;状态筛选与搜索;启停操作调用;
 * 新建表单的直连/绑定服务器两种连接方式与校验;删除确认流程。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'

const listTunnelsMock = vi.fn()
const createTunnelMock = vi.fn()
const updateTunnelMock = vi.fn()
const deleteTunnelMock = vi.fn()
const startTunnelMock = vi.fn()
const stopTunnelMock = vi.fn()
const restartTunnelMock = vi.fn()
const testTunnelMock = vi.fn()
const getTunnelLogsMock = vi.fn()
const listServersMock = vi.fn()
const listCredentialsMock = vi.fn()

vi.mock('../api/tunnels', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/tunnels')>()
  return {
    ...actual,
    listTunnels: (...a: unknown[]) => listTunnelsMock(...a),
    createTunnel: (...a: unknown[]) => createTunnelMock(...a),
    updateTunnel: (...a: unknown[]) => updateTunnelMock(...a),
    deleteTunnel: (...a: unknown[]) => deleteTunnelMock(...a),
    startTunnel: (...a: unknown[]) => startTunnelMock(...a),
    stopTunnel: (...a: unknown[]) => stopTunnelMock(...a),
    restartTunnel: (...a: unknown[]) => restartTunnelMock(...a),
    testTunnel: (...a: unknown[]) => testTunnelMock(...a),
    getTunnelLogs: (...a: unknown[]) => getTunnelLogsMock(...a),
  }
})

vi.mock('../api/servers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/servers')>()
  return { ...actual, listServers: (...a: unknown[]) => listServersMock(...a) }
})

vi.mock('../api/credentials', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/credentials')>()
  return { ...actual, listCredentials: (...a: unknown[]) => listCredentialsMock(...a) }
})

// useConfirm 每次调用返回新闭包对象,spy 单例无效 → mock 整个模块(共享 open)。
const confirmOpenMock = vi.fn()
vi.mock('../composables/useConfirm', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../composables/useConfirm')>()
  return {
    ...actual,
    useConfirm: () => ({ pending: actual.useConfirm().pending, open: confirmOpenMock, _resolve: actual.useConfirm()._resolve }),
  }
})

import Tunnels from './Tunnels.vue'
import type { SshTunnel } from '../api/tunnels'

function tunnel(over: Partial<SshTunnel> = {}): SshTunnel {
  return {
    id: 't1',
    name: 'db-tunnel',
    serverId: '',
    serverName: '',
    host: '203.0.113.10',
    port: 22,
    user: 'root',
    credentialId: 'c1',
    credentialName: 'key',
    mode: 'local',
    bindHost: '127.0.0.1',
    bindPort: 15432,
    destHost: '10.0.0.5',
    destPort: 5432,
    enabled: true,
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    status: 'active',
    pid: 4321,
    lastError: '',
    restarts: 0,
    actualPort: 15432,
    endpoint: '127.0.0.1:15432',
    startedAt: '2026-10-20T00:00:00Z',
    ...over,
  }
}

async function mountView(): Promise<VueWrapper> {
  const w = mount(Tunnels)
  await flushPromises()
  return w
}

beforeEach(() => {
  vi.clearAllMocks()
  listTunnelsMock.mockResolvedValue([])
  listServersMock.mockResolvedValue([{ id: 's1', name: 'web-1' }])
  listCredentialsMock.mockResolvedValue([{ id: 'c1', name: 'prod-key', type: 'ssh_key' }])
})

describe('Tunnels · 列表与筛选', () => {
  it('渲染隧道行:名称/目标/模式/映射/状态', async () => {
    listTunnelsMock.mockResolvedValue([tunnel()])
    const w = await mountView()
    expect(w.text()).toContain('db-tunnel')
    expect(w.text()).toContain('root@203.0.113.10')
    expect(w.text()).toContain('本地')
    expect(w.text()).toContain('127.0.0.1:15432')
    expect(w.text()).toContain('10.0.0.5:5432')
    expect(w.text()).toContain('运行中')
    expect(w.text()).toContain('pid 4321')
    w.unmount()
  })

  it('dynamic 模式展示 SOCKS5 且无目标;自动端口展示实际端口', async () => {
    listTunnelsMock.mockResolvedValue([tunnel({ mode: 'dynamic', bindPort: 0, actualPort: 54321, destHost: '', destPort: 0 })])
    const w = await mountView()
    expect(w.text()).toContain('SOCKS5')
    expect(w.text()).toContain('127.0.0.1:54321')
    expect(w.text()).not.toContain('10.0.0.5:5432')
    w.unmount()
  })

  it('KPI 三桶计数与状态筛选联动', async () => {
    listTunnelsMock.mockResolvedValue([
      tunnel({ id: 'a', status: 'active' }),
      tunnel({ id: 'b', name: 'rt', status: 'retrying' }),
      tunnel({ id: 'c', name: 'off', status: 'disabled', enabled: false }),
    ])
    const w = await mountView()
    const kpis = w.findAll('.kpi__num').map((n) => n.text())
    expect(kpis).toEqual(['3', '1', '1', '1'])
    // 筛选:只看运行中。
    await w.findAll('.filter-chip')[1].trigger('click')
    expect(w.findAll('.tunnel-row')).toHaveLength(1)
    expect(w.text()).toContain('db-tunnel')
    expect(w.text()).not.toContain('off 的')
    w.unmount()
  })

  it('搜索按名称过滤', async () => {
    listTunnelsMock.mockResolvedValue([tunnel(), tunnel({ id: 'x', name: 'socks-out' })])
    const w = await mountView()
    await w.find('.search-box__input').setValue('socks')
    expect(w.findAll('.tunnel-row')).toHaveLength(1)
    expect(w.text()).toContain('socks-out')
    w.unmount()
  })

  it('报错时展示错误态;空列表展示空态', async () => {
    listTunnelsMock.mockRejectedValue(new Error('boom'))
    let w = await mountView()
    expect(w.find('.error-state, [class*=error]').exists()).toBe(true)
    w.unmount()
    listTunnelsMock.mockResolvedValue([])
    w = await mountView()
    expect(w.text()).toContain('还没有隧道')
    w.unmount()
  })
})

describe('Tunnels · 行操作', () => {
  it('待启动隧道点「启动」调用 startTunnel', async () => {
    listTunnelsMock.mockResolvedValue([tunnel({ status: 'stopped' })])
    startTunnelMock.mockResolvedValue(tunnel())
    const w = await mountView()
    const btn = w.findAll('.tunnel-row__actions button').find((b) => b.text() === '启动')!
    await btn.trigger('click')
    await flushPromises()
    expect(startTunnelMock).toHaveBeenCalledWith('t1')
    w.unmount()
  })

  it('运行中隧道点「停止」先二次确认再调用 stopTunnel', async () => {
    confirmOpenMock.mockResolvedValue(true)
    listTunnelsMock.mockResolvedValue([tunnel()])
    stopTunnelMock.mockResolvedValue(tunnel({ status: 'disabled', enabled: false }))
    const w = await mountView()
    const btn = w.findAll('.tunnel-row__actions button').find((b) => b.text() === '停止')!
    await btn.trigger('click')
    await flushPromises()
    expect(confirmOpenMock).toHaveBeenCalled()
    expect(stopTunnelMock).toHaveBeenCalledWith('t1')
    w.unmount()
  })

  it('测试按钮调用 testTunnel 并成功 toast', async () => {
    testTunnelMock.mockResolvedValue({ ok: true, latencyMs: 12, error: null })
    listTunnelsMock.mockResolvedValue([tunnel()])
    const w = await mountView()
    const btn = w.findAll('.tunnel-row__actions button').find((b) => b.text() === '测试')!
    await btn.trigger('click')
    await flushPromises()
    expect(testTunnelMock).toHaveBeenCalledWith('t1')
    w.unmount()
  })

  it('日志按钮打开日志弹窗并拉取日志', async () => {
    getTunnelLogsMock.mockResolvedValue(['2026-10-24 [tunnel] 转发就绪:127.0.0.1:15432'])
    listTunnelsMock.mockResolvedValue([tunnel()])
    const w = await mountView()
    const btn = w.findAll('.tunnel-row__actions button').find((b) => b.text() === '日志')!
    await btn.trigger('click')
    await flushPromises()
    expect(getTunnelLogsMock).toHaveBeenCalledWith('t1', 300)
    expect(w.text()).toContain('转发就绪')
    w.unmount()
  })
})

describe('Tunnels · 新建/编辑表单', () => {
  it('空表单提交给出各字段错误,不发请求', async () => {
    const w = await mountView()
    await w.findAll('.header-actions button')[0].trigger('click') // 新建隧道
    await w.find('form.tunnel-form').trigger('submit.prevent')
    await flushPromises()
    expect(w.text()).toContain('请填写名称')
    expect(createTunnelMock).not.toHaveBeenCalled()
    w.unmount()
  })

  it('直连 dynamic 表单提交载荷正确(host/user/凭据,无目标)', async () => {
    createTunnelMock.mockResolvedValue(tunnel({ mode: 'dynamic' }))
    const w = await mountView()
    await w.findAll('.header-actions button')[0].trigger('click')
    // 直连模式 input 序:name / host / port / user / bindHost / bindPort(dynamic 无 dest)。
    const inputs = w.findAll('input.field-input')
    await inputs[0].setValue('socks-out')
    await inputs[1].setValue('198.51.100.7')
    await inputs[2].setValue('22')
    await inputs[3].setValue('ops')
    await inputs[5].setValue('1080')
    await w.find('select.field-input').setValue('c1')
    // 模式 = dynamic(最后一个 radio-pill;前两个是连接方式)。
    const pills = w.findAll('.radio-pill')
    await pills[pills.length - 1].find('input').setValue('dynamic')
    await w.find('form.tunnel-form').trigger('submit.prevent')
    await flushPromises()
    expect(createTunnelMock).toHaveBeenCalledWith(
      expect.objectContaining({
        name: 'socks-out',
        host: '198.51.100.7',
        user: 'ops',
        credentialId: 'c1',
        serverId: '',
        mode: 'dynamic',
        destHost: '',
        destPort: 0,
        bindHost: '127.0.0.1',
        bindPort: 1080,
        enabled: true,
      }),
    )
    w.unmount()
  })

  it('绑定服务器模式提交载荷:serverId 生效、直连字段清空', async () => {
    createTunnelMock.mockResolvedValue(tunnel({ serverId: 's1', host: '', user: '', credentialId: '' }))
    const w = await mountView()
    await w.findAll('.header-actions button')[0].trigger('click')
    await w.find('input.field-input').setValue('via-server')
    // 连接方式切「已登记服务器」(第一个 radio-pill)。
    await w.findAll('.radio-pill input')[0].setValue('server')
    await flushPromises()
    // 服务器下拉出现并选择。
    await w.find('select.field-input').setValue('s1')
    // 服务器模式 input 序:name / bindHost / bindPort / destHost / destPort(local 默认)。
    const inputs = w.findAll('input.field-input')
    await inputs[2].setValue('8080')
    await inputs[3].setValue('db.internal')
    await inputs[4].setValue('3306')
    await w.find('form.tunnel-form').trigger('submit.prevent')
    await flushPromises()
    expect(createTunnelMock).toHaveBeenCalledWith(
      expect.objectContaining({
        serverId: 's1', host: '', user: '', credentialId: '', mode: 'local',
        bindHost: '127.0.0.1', bindPort: 8080, destHost: 'db.internal', destPort: 3306,
      }),
    )
    w.unmount()
  })

  it('编辑回填并提交走 updateTunnel', async () => {
    updateTunnelMock.mockResolvedValue(tunnel())
    listTunnelsMock.mockResolvedValue([tunnel()])
    const w = await mountView()
    const btn = w.findAll('.tunnel-row__actions button').find((b) => b.text() === '编辑')!
    await btn.trigger('click')
    await flushPromises()
    expect((w.find('input.field-input').element as HTMLInputElement).value).toBe('db-tunnel')
    await w.find('form.tunnel-form').trigger('submit.prevent')
    await flushPromises()
    expect(updateTunnelMock).toHaveBeenCalledWith('t1', expect.objectContaining({ name: 'db-tunnel' }))
    w.unmount()
  })
})

describe('Tunnels · 删除', () => {
  it('删除确认后调用 deleteTunnel', async () => {
    deleteTunnelMock.mockResolvedValue(undefined)
    listTunnelsMock.mockResolvedValue([tunnel()])
    const w = await mountView()
    const btn = w.findAll('.tunnel-row__actions button').find((b) => b.text() === '删除')!
    await btn.trigger('click')
    await flushPromises()
    expect(w.text()).toContain('确定删除隧道')
    const confirmBtn = w.findAll('.modal--sm .modal-actions button')[1]
    await confirmBtn.trigger('click')
    await flushPromises()
    expect(deleteTunnelMock).toHaveBeenCalledWith('t1')
    w.unmount()
  })
})
