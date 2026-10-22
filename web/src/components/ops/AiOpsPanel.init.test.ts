import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import AiOpsPanel from './AiOpsPanel.vue'
import {
  opsApi,
  streamOps,
  type Snapshot,
  type Session,
  type StreamEvent,
} from '../../api/opsChat'
import { resetOpsAuth } from '../../utils/opsAuth'

vi.mock('vue-router', () => ({
  useRouter: () => ({ resolve: () => ({ href: '/terminal' }) }),
  onBeforeRouteLeave: vi.fn(),
}))
vi.mock('../../api/servers', () => ({
  listServers: vi.fn().mockResolvedValue(
    Array.from({ length: 6 }, (_, i) => ({
      id: `srv-${i}`,
      name: `strata0${i}`,
      host: `10.0.0.${i}`,
      port: 22,
    })),
  ),
}))
vi.mock('../../api/opsChat', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/opsChat')>()
  return {
    ...actual,
    streamOps: vi.fn(),
    opsApi: Object.fromEntries(
      Object.keys(actual.opsApi).map((key) => [key, vi.fn()]),
    ),
  }
})

const stamp = '2026-10-22T00:00:00Z'
function session(id: string, serverIds: string[]): Session {
  return {
    id,
    revision: 1,
    title: id,
    draft: '',
    serverIds,
    activeRunId: '',
    watermark: 0,
    createdAt: stamp,
    updatedAt: stamp,
  }
}
function snap(s: Session): Snapshot {
  return {
    session: s,
    runs: [],
    calls: [],
    confirmations: [],
    entries: { entries: [], cursor: '', watermark: 0, reset: false },
    watermark: 0,
  }
}
const events: ((event: StreamEvent) => void)[] = []

let wrapper: VueWrapper | undefined
beforeEach(() => {
  vi.useFakeTimers()
  vi.clearAllMocks()
  resetOpsAuth()
  events.length = 0
  vi.mocked(opsApi.capabilities).mockResolvedValue({
    available: true,
    modelAvailable: false,
    provider: { provider: '', model: '', configHash: '' },
    limits: { targets: 8 },
  })
  vi.mocked(opsApi.tools).mockResolvedValue([])
  vi.mocked(opsApi.list).mockResolvedValue({
    sessions: [session('ov', [])],
    activeSessionId: 'ov',
    cursor: '',
  })
  vi.mocked(opsApi.activate).mockResolvedValue({ ok: true })
  vi.mocked(opsApi.snapshot).mockResolvedValue(snap(session('ov', [])))
  vi.mocked(streamOps).mockImplementation(
    async (_id, _after, signal, deliver) => {
      events.push(deliver)
      await new Promise<void>((resolve) =>
        signal.addEventListener('abort', () => resolve(), { once: true }),
      )
    },
  )
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  resetOpsAuth()
  vi.useRealTimers()
})

async function mountPanel(initialServerIds: string[] = []): Promise<void> {
  wrapper = mount(AiOpsPanel, {
    attachTo: document.body,
    props: { context: 'overview', initialServerIds },
    global: {
      stubs: {
        AiOpsMessageList: true,
        AiOpsConfirmation: true,
        AiOpsSessionList: true,
      },
    },
  })
  await flushPromises()
}

describe('AiOpsPanel initialization', () => {
  it('lists all registered servers (context=overview, preselected empty, AI unconfigured)', async () => {
    await mountPanel()
    const text = wrapper!.text()
    expect(text).not.toContain('暂无登记服务器')
    for (let i = 0; i < 6; i++) expect(text).toContain(`strata0${i}`)
    expect(text).toContain('已选 0 台 / 最多 8 台')
    expect(text).toContain('AI 未配置，仍可使用运维工具')
  })

  it('surfaces an error banner instead of a blank picker when the init requests hang', async () => {
    vi.mocked(opsApi.capabilities).mockReturnValue(
      new Promise(() => undefined),
    )
    await mountPanel()
    expect(wrapper!.text()).toContain('暂无登记服务器')
    expect(wrapper!.find('[role="alert"]').exists()).toBe(false)
    await vi.advanceTimersByTimeAsync(15_001)
    await flushPromises()
    const alert = wrapper!.find('[role="alert"]')
    expect(alert.exists()).toBe(true)
    expect(alert.text()).toContain('网络请求失败')
  })
})

describe('AiOpsPanel entry preselection', () => {
  function bindSession(serverIds: string[]): void {
    const s = session('ov', serverIds)
    vi.mocked(opsApi.snapshot).mockImplementation(async (id) =>
      snap(id === 'ov' ? s : session(id, serverIds)),
    )
    vi.mocked(opsApi.list).mockResolvedValue({
      sessions: [s],
      activeSessionId: 'ov',
      cursor: '',
    })
  }

  it('starts a new session with the card-clicked server when the reused session targets differ', async () => {
    bindSession([])
    vi.mocked(opsApi.create).mockImplementation(async (ids) => {
      const s = session('new', ids as string[])
      vi.mocked(opsApi.snapshot).mockResolvedValue(snap(s))
      return s
    })
    await mountPanel(['srv-3'])
    expect(opsApi.create).toHaveBeenCalledWith(['srv-3'], 'overview', '', '')
    const text = wrapper!.text()
    expect(text).toContain('已选 1 台 / 最多 8 台')
    const checked = wrapper!
      .findAll('.server-choice input')
      .filter((c) => (c.element as HTMLInputElement).checked)
    expect(checked).toHaveLength(1)
    expect(
      (checked[0]!.element as HTMLInputElement).closest('label')?.textContent,
    ).toContain('strata03')
  })

  it('reuses the bound session without creating one when targets already match', async () => {
    bindSession(['srv-3'])
    await mountPanel(['srv-3'])
    expect(opsApi.create).not.toHaveBeenCalled()
    expect(wrapper!.text()).toContain('已选 1 台 / 最多 8 台')
  })
})
