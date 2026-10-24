/**
 * Pm2.local.test.ts — /pm2 页面「本机(控制节点)」卡牌的回归测试(仿 Containers.local.test.ts)。
 *
 * 覆盖:本机 pm2 分组置首且与登记服务器卡并存;本机请求失败只隐藏本机卡(不拖垮服务器卡);
 * 只有本机数据时不显示空态;本机卡的标题/主机文案走 i18n(localName/localHost)。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'

const listServersMock = vi.fn()
const getAllPm2Mock = vi.fn()
const getServerPm2Mock = vi.fn()

vi.mock('../api/servers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/servers')>()
  return {
    ...actual,
    listServers: (...a: unknown[]) => listServersMock(...a),
  }
})

vi.mock('../api/pm2', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/pm2')>()
  return {
    ...actual,
    getAllPm2: (...a: unknown[]) => getAllPm2Mock(...a),
    getServerPm2: (...a: unknown[]) => getServerPm2Mock(...a),
  }
})

import Pm2 from './Pm2.vue'
import { LOCAL_HOST_ID } from '../api/servers'
import type { Server } from '../api/servers'
import type { ServerPm2 } from '../api/pm2'

function server(id: string, name: string): Server {
  return {
    id,
    name,
    host: '10.0.0.1',
    port: 22,
    user: 'root',
    credentialId: 'c-1',
    credentialName: 'cred',
    sudoCredentialId: '',
    sudoCredentialName: '',
    jumps: [],
    labels: '',
    maxBuilds: 0,
    priority: 0,
    gpu: false, sharedBuild: false,
    poolId: 'default',
    poolName: 'default',
    kind: 'standard',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  }
}

function proc(over: Partial<ServerPm2['processes'][number]> = {}): ServerPm2['processes'][number] {
  return {
    id: 0,
    name: 'api',
    pid: 111,
    status: 'online',
    restarts: 3,
    uptime: Date.now() - 60_000,
    cpu: 1.5,
    memory: 104857600,
    execMode: 'cluster_mode',
    instances: '2',
    interpreter: 'node',
    script: 'server.js',
    cwd: '/srv/api',
    createdAt: 1729000000000,
    outLog: '/home/u/.pm2/logs/api-out.log',
    errLog: '/home/u/.pm2/logs/api-error.log',
    ...over,
  }
}

function group(serverId: string, over: Partial<ServerPm2> = {}): ServerPm2 {
  return {
    serverId,
    reachable: true,
    runtime: 'pm2',
    error: '',
    processes: [proc()],
    online: 1,
    total: 1,
    collectedAt: '2026-10-11T00:00:00Z',
    ...over,
  }
}

async function mountView(): Promise<VueWrapper> {
  const w = mount(Pm2)
  await flushPromises()
  return w
}

function cardNames(w: VueWrapper): string[] {
  return w.findAll('.panel__name').map((n) => n.text())
}

beforeEach(() => {
  vi.clearAllMocks()
  listServersMock.mockResolvedValue([server('s1', 'web-1')])
  getAllPm2Mock.mockResolvedValue([group('s1')])
  getServerPm2Mock.mockResolvedValue(group(LOCAL_HOST_ID, { processes: [], online: 0, total: 0 }))
})

describe('Pm2 · 本机(控制节点)卡牌', () => {
  it('本机分组置首,与登记服务器卡并存,且显示本机数据', async () => {
    getServerPm2Mock.mockResolvedValue(group(LOCAL_HOST_ID))
    const w = await mountView()
    expect(getServerPm2Mock).toHaveBeenCalledWith(LOCAL_HOST_ID)
    expect(cardNames(w)).toEqual(['本机(控制节点)', 'web-1'])
    // 进程行渲染:名称 + 脚本 + pm_id。
    expect(w.text()).toContain('api')
    expect(w.text()).toContain('pm_id 0')
    w.unmount()
  })

  it('本机请求失败时只隐藏本机卡,登记服务器卡照常展示', async () => {
    getServerPm2Mock.mockRejectedValue(new Error('boom'))
    const w = await mountView()
    expect(cardNames(w)).toEqual(['web-1'])
    w.unmount()
  })

  it('只有本机数据时不显示空态', async () => {
    listServersMock.mockResolvedValue([])
    getAllPm2Mock.mockResolvedValue([])
    const w = await mountView()
    expect(cardNames(w)).toEqual(['本机(控制节点)'])
    w.unmount()
  })

  it('本机被标记为不可达/无 pm2 也照常成卡(降级由卡片内提示处理)', async () => {
    getServerPm2Mock.mockResolvedValue(
      group(LOCAL_HOST_ID, { reachable: false, runtime: '', processes: [], online: 0, total: 0, error: '本机无 pm2' }),
    )
    const w = await mountView()
    expect(cardNames(w)).toEqual(['本机(控制节点)', 'web-1'])
    expect(w.text()).toContain('本机无 pm2')
    w.unmount()
  })

  it('KPI 与状态筛选计数按三桶归并', async () => {
    getAllPm2Mock.mockResolvedValue([
      group('s1', {
        processes: [proc(), proc({ id: 1, status: 'errored' }), proc({ id: 2, status: 'stopped', pid: 0, uptime: 0 })],
        online: 1,
        total: 3,
      }),
    ])
    getServerPm2Mock.mockResolvedValue(group(LOCAL_HOST_ID, { processes: [], online: 0, total: 0 }))
    const w = await mountView()
    const kpis = w.findAll('.kpi__num').map((n) => n.text())
    // 总数 3 / 在线 1 / 停止 1 / 异常 1 / 覆盖 1+1。
    expect(kpis[0]).toBe('3')
    expect(kpis[1]).toBe('1')
    expect(kpis[2]).toBe('1')
    expect(kpis[3]).toBe('1')
    w.unmount()
  })
})
