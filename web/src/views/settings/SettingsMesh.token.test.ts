import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import SettingsMesh from './SettingsMesh.vue'

// 回归:API 令牌明文只在创建响应返回一次(库内只存哈希)。此前 tokenResult 已写入
// 却从未在模板渲染 → UI 创建的令牌永远拿不到。本组用例把「创建 → 一次性弹窗展示
// → 复制 → 未复制关闭守卫」钉成硬要求。

const api = vi.hoisted(() => ({
  settings: vi.fn(),
  acl: vi.fn(),
  peers: vi.fn(),
  overview: vi.fn(),
  firewall: vi.fn(),
  dnsRecords: vi.fn(),
  dnsProviders: vi.fn(),
  switchStatus: vi.fn(),
  credentials: vi.fn(),
  tokens: vi.fn(),
  createToken: vi.fn(),
}))
const confirmOpen = vi.hoisted(() => vi.fn())
const clipboardWrite = vi.hoisted(() => vi.fn())

vi.mock('../../api/mesh', () => ({
  getMeshSettings: api.settings,
  getMeshACL: api.acl,
  listMeshPeers: api.peers,
  getMeshOverview: api.overview,
  listMeshDnsRecords: api.dnsRecords,
  getMeshSecureModeStatus: api.switchStatus,
  listMeshCredentials: api.credentials,
  listMeshApiTokens: api.tokens,
  createMeshApiToken: api.createToken,
}))
vi.mock('../../api/dnsProviders', () => ({ listDnsProviders: api.dnsProviders }))
vi.mock('../../api/firewall', () => ({ getFirewallStatus: api.firewall }))
vi.mock('../../composables/useConfirm', () => ({
  useConfirm: () => ({ open: confirmOpen }),
}))

const PLAINTEXT = 'pwmc_' + 'A'.repeat(43)

let wrapper: VueWrapper | undefined
beforeEach(() => {
  vi.resetAllMocks()
  api.settings.mockResolvedValue({
    name: 'node-a',
    enabled: false,
    easytierMode: 'managed',
    easytierRpc: '',
    networkName: 'net',
    virtualIp: '',
    peers: [],
    secureMode: false,
  })
  api.acl.mockResolvedValue({})
  api.peers.mockResolvedValue({ items: [] })
  api.overview.mockResolvedValue({
    easytier: { selfIp: '', err: '' },
    settings: { enabled: false, networkName: 'net' },
    peers: [],
    supervisor: null,
    binaries: null,
    externalMode: false,
  })
  api.firewall.mockResolvedValue({
    ports: [],
    extraCidrs: [],
    detect: { supported: true, reason: '' },
    phase: 'off',
  })
  api.dnsRecords.mockResolvedValue({ items: [] })
  api.dnsProviders.mockResolvedValue([])
  api.switchStatus.mockResolvedValue({
    secureMode: false,
    pending: null,
    nodes: [],
    skipped: [],
  })
  api.credentials.mockResolvedValue({ items: [] })
  api.tokens.mockResolvedValue({ items: [] })
  api.createToken.mockResolvedValue({
    name: 'ci-runner',
    token: PLAINTEXT,
    createdAt: '2026-11-11T00:00:00Z',
  })
  confirmOpen.mockResolvedValue(true)
  clipboardWrite.mockResolvedValue(undefined)
  // jsdom 无 clipboard 且 isSecureContext 为 false → 两者都 stub,让 copyText 走
  // Clipboard API 主路径而非 execCommand 兜底。
  Object.defineProperty(window, 'isSecureContext', { value: true, configurable: true })
  Object.defineProperty(navigator, 'clipboard', {
    value: { writeText: clipboardWrite },
    configurable: true,
  })
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

async function setup() {
  wrapper = mount(SettingsMesh, {
    global: { stubs: { Teleport: true, 'router-link': true } },
  })
  await flushPromises()
  return wrapper
}

function tokenSection(w: VueWrapper) {
  return w
    .findAll('section.block')
    .find((s) => s.find('h2').text() === '凭据 API 令牌')!
}

async function createToken(w: VueWrapper) {
  const section = tokenSection(w)
  await section.get('input.inp').setValue('ci-runner')
  const btn = section.findAll('button').find((b) => b.text() === '创建令牌')!
  await btn.trigger('click')
  await flushPromises()
}

function dialogButton(w: VueWrapper, label: string) {
  return w
    .get('[role="dialog"]')
    .findAll('button')
    .find((b) => b.text() === label)!
}

describe('API 令牌一次性展示', () => {
  it('创建后弹窗展示明文令牌(唯一一次可获取)', async () => {
    const w = await setup()
    expect(w.find('[role="dialog"]').exists()).toBe(false)
    await createToken(w)
    expect(api.createToken).toHaveBeenCalledExactlyOnceWith('ci-runner')
    const dialog = w.get('[role="dialog"]')
    expect((dialog.get('.token-text').element as HTMLTextAreaElement).value).toBe(
      PLAINTEXT,
    )
    expect(dialog.text()).toContain('ci-runner')
    expect(dialog.text()).toContain('仅此一次显示')
    // 名字输入框已清空,令牌列表已刷新。
    expect(
      (tokenSection(w).get('input.inp').element as HTMLInputElement).value,
    ).toBe('')
    expect(api.tokens).toHaveBeenCalledTimes(2)
  })

  it('复制按钮把明文写入剪贴板并给出已复制反馈', async () => {
    const w = await setup()
    await createToken(w)
    await dialogButton(w, '复制令牌').trigger('click')
    await flushPromises()
    expect(clipboardWrite).toHaveBeenCalledExactlyOnceWith(PLAINTEXT)
    expect(w.get('[role="dialog"]').text()).toContain('已复制')
  })

  it('未复制就关闭需二次确认:取消保留弹窗,确认后丢弃', async () => {
    const w = await setup()
    await createToken(w)
    confirmOpen.mockResolvedValueOnce(false)
    await dialogButton(w, '我已保存,关闭').trigger('click')
    await flushPromises()
    expect(confirmOpen).toHaveBeenCalledTimes(1)
    expect(w.find('[role="dialog"]').exists()).toBe(true)
    await dialogButton(w, '我已保存,关闭').trigger('click')
    await flushPromises()
    expect(w.find('[role="dialog"]').exists()).toBe(false)
  })

  it('已成功复制后关闭不再询问', async () => {
    const w = await setup()
    await createToken(w)
    await dialogButton(w, '复制令牌').trigger('click')
    await flushPromises()
    await dialogButton(w, '我已保存,关闭').trigger('click')
    await flushPromises()
    expect(confirmOpen).not.toHaveBeenCalled()
    expect(w.find('[role="dialog"]').exists()).toBe(false)
  })
})
