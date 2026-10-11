/**
 * Containers.local.test.ts — /containers 页面「本机(控制节点)」卡牌的回归测试。
 *
 * 覆盖:本机容器分组置首且与登记服务器卡并存;本机请求失败只隐藏本机卡(不拖垮服务器卡);
 * 只有本机数据时不显示空态;本机卡的标题/主机文案走 i18n(localName/localHost)。
 *
 * ServerCard 会在挂载时预取镜像/Stacks/卷/网络/实时 stats,故一并 stub 成空结果,
 * 让测试保持离线确定。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'

const listServersMock = vi.fn()
const getAllContainersMock = vi.fn()
const getServerContainersMock = vi.fn()
const getServerContainerStatsMock = vi.fn()
const getServerImagesMock = vi.fn()
const getServerStacksMock = vi.fn()
const getServerVolumesMock = vi.fn()
const getServerNetworksMock = vi.fn()

vi.mock('vue-router', () => ({
  useRouter: () => ({ resolve: () => ({ href: '/servers/x/terminal' }) }),
}))

vi.mock('../api/containers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/containers')>()
  return {
    ...actual,
    getAllContainers: (...a: unknown[]) => getAllContainersMock(...a),
    getServerContainers: (...a: unknown[]) => getServerContainersMock(...a),
    getServerContainerStats: (...a: unknown[]) => getServerContainerStatsMock(...a),
    getServerImages: (...a: unknown[]) => getServerImagesMock(...a),
    getServerStacks: (...a: unknown[]) => getServerStacksMock(...a),
    getServerVolumes: (...a: unknown[]) => getServerVolumesMock(...a),
    getServerNetworks: (...a: unknown[]) => getServerNetworksMock(...a),
  }
})

vi.mock('../api/servers', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/servers')>()
  return {
    ...actual,
    listServers: (...a: unknown[]) => listServersMock(...a),
  }
})

import Containers from './Containers.vue'
import { LOCAL_HOST_ID } from '../api/servers'
import type { Server } from '../api/servers'
import type { ServerContainers } from '../api/containers'

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
    gpu: false,
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  }
}

function group(serverId: string, over: Partial<ServerContainers> = {}): ServerContainers {
  return {
    serverId,
    reachable: true,
    runtime: 'docker',
    error: '',
    containers: [
      {
        id: `${serverId}-0123456789ab`,
        names: `${serverId}-ctr`,
        image: 'nginx:latest',
        state: 'running',
        status: 'Up 2 hours',
        ports: '0.0.0.0:80->80/tcp',
        createdAt: '2026-05-01T00:00:00Z',
      },
    ],
    running: 1,
    total: 1,
    collectedAt: '2026-10-11T00:00:00Z',
    ...over,
  }
}

async function mountView(): Promise<VueWrapper> {
  const w = mount(Containers)
  await flushPromises()
  return w
}

function cardNames(w: VueWrapper): string[] {
  return w.findAll('.panel__name').map((n) => n.text())
}

beforeEach(() => {
  vi.clearAllMocks()
  listServersMock.mockResolvedValue([server('s1', 'web-1')])
  getAllContainersMock.mockResolvedValue([group('s1')])
  getServerContainersMock.mockResolvedValue(group(LOCAL_HOST_ID, { containers: [], running: 0, total: 0 }))
  getServerContainerStatsMock.mockResolvedValue({
    serverId: 'x',
    reachable: true,
    runtime: 'docker',
    error: '',
    stats: [],
    collectedAt: '2026-10-11T00:00:00Z',
  })
  getServerImagesMock.mockResolvedValue({ serverId: 'x', reachable: true, runtime: 'docker', error: '', images: [], collectedAt: '' })
  getServerStacksMock.mockResolvedValue({ serverId: 'x', reachable: true, runtime: 'docker', error: '', stacks: [], collectedAt: '' })
  getServerVolumesMock.mockResolvedValue({ serverId: 'x', reachable: true, runtime: 'docker', error: '', volumes: [], collectedAt: '' })
  getServerNetworksMock.mockResolvedValue({ serverId: 'x', reachable: true, runtime: 'docker', error: '', networks: [], collectedAt: '' })
})

describe('Containers · 本机(控制节点)卡牌', () => {
  it('本机分组置首,与登记服务器卡并存,且显示本机文案', async () => {
    // 本机带一个容器,便于断言它渲染的是本机那一份数据。
    getServerContainersMock.mockResolvedValue(group(LOCAL_HOST_ID))
    const w = await mountView()
    expect(getServerContainersMock).toHaveBeenCalledWith(LOCAL_HOST_ID)
    expect(cardNames(w)).toEqual(['本机(控制节点)', 'web-1'])
    expect(w.text()).toContain(`${LOCAL_HOST_ID}-ctr`)
    w.unmount()
  })

  it('本机请求失败时只隐藏本机卡,登记服务器卡照常展示', async () => {
    getServerContainersMock.mockRejectedValue(new Error('boom'))
    const w = await mountView()
    expect(cardNames(w)).toEqual(['web-1'])
    w.unmount()
  })

  it('只有本机数据时不显示空态', async () => {
    listServersMock.mockResolvedValue([])
    getAllContainersMock.mockResolvedValue([])
    const w = await mountView()
    expect(cardNames(w)).toEqual(['本机(控制节点)'])
    w.unmount()
  })

  it('本机被标记为不可达/无运行时也照常成卡(降级由卡片内提示处理)', async () => {
    getServerContainersMock.mockResolvedValue(
      group(LOCAL_HOST_ID, { reachable: false, runtime: '', containers: [], running: 0, total: 0, error: '本机无容器运行时' }),
    )
    const w = await mountView()
    expect(cardNames(w)).toEqual(['本机(控制节点)', 'web-1'])
    expect(w.text()).toContain('本机无容器运行时')
    w.unmount()
  })
})
