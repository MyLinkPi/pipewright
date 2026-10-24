import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import SettingsVault from './SettingsVault.vue'

const api = vi.hoisted(() => ({
  list: vi.fn().mockResolvedValue([]),
  create: vi.fn(),
  update: vi.fn(),
  generate: vi.fn(),
}))
const toastFns = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }))
vi.mock('../../api/credentials', () => ({
  listCredentials: api.list,
  createCredential: api.create,
  updateCredential: api.update,
  deleteCredential: vi.fn(),
  revealCredential: vi.fn(),
  generateSshKeyPair: api.generate,
  isSystemManagedType: () => false,
}))
vi.mock('../../api/oauth', () => ({
  getOAuthApps: vi.fn().mockResolvedValue([]),
}))
vi.mock('vue-router', () => ({
  useRoute: () => ({ query: {} }),
  useRouter: () => ({ replace: vi.fn() }),
}))
vi.mock('../../components/AuditTimeline.vue', () => ({
  default: { render: () => null },
}))
vi.mock('../../composables/useToast', () => ({
  useToast: () => toastFns,
}))
let w: VueWrapper | undefined
afterEach(() => {
  w?.unmount()
  vi.clearAllMocks()
})

const PRIV = '-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----'
const PUB = 'ssh-ed25519 AAAAC3Nzatestkey'

/** Open the add modal and switch to the SSH 私钥 type. */
async function openSshKeyForm(wrapper: VueWrapper): Promise<void> {
  const add = wrapper.findAll('button').find((b) => b.text() === '添加凭据')!
  await add.trigger('click')
  const ssh = wrapper.findAll('.seg-item').find((b) => b.text() === 'SSH 私钥')!
  await ssh.trigger('click')
}

describe('ssh keypair generation', () => {
  it('fills the private key, shows the public key, and passes the chosen algorithm', async () => {
    api.generate.mockResolvedValue({ privateKey: PRIV, publicKey: PUB })
    w = mount(SettingsVault, {
      global: { stubs: { Teleport: true, 'router-link': true } },
    })
    await flushPromises()
    await openSshKeyForm(w)
    await w.get('#cred-ssh-algo').setValue('rsa2048')
    await w.get('.ssh-keygen-btn').trigger('click')
    await flushPromises()
    expect(api.generate).toHaveBeenCalledWith('rsa2048')
    expect((w.get('#cred-secret').element as HTMLTextAreaElement).value).toBe(PRIV)
    expect(w.get('[data-testid="ssh-public-key"]').text()).toContain(PUB)
  })

  it('defaults to ed25519 and keeps the controls hidden for non-ssh types', async () => {
    api.generate.mockResolvedValue({ privateKey: PRIV, publicKey: PUB })
    w = mount(SettingsVault, {
      global: { stubs: { Teleport: true, 'router-link': true } },
    })
    await flushPromises()
    // Default type is git_token: no algorithm select / generate button / pubkey box.
    expect(w.find('.ssh-keygen-row').exists()).toBe(false)
    expect(w.find('[data-testid="ssh-public-key"]').exists()).toBe(false)
    await openSshKeyForm(w)
    await w.get('.ssh-keygen-btn').trigger('click')
    await flushPromises()
    expect(api.generate).toHaveBeenCalledWith('ed25519')
  })

  it('toasts and leaves the secret untouched when generation fails', async () => {
    api.generate.mockRejectedValue(new Error('boom'))
    w = mount(SettingsVault, {
      global: { stubs: { Teleport: true, 'router-link': true } },
    })
    await flushPromises()
    await openSshKeyForm(w)
    await w.get('#cred-secret').setValue('existing pasted key')
    await w.get('.ssh-keygen-btn').trigger('click')
    await flushPromises()
    expect(toastFns.error).toHaveBeenCalled()
    expect((w.get('#cred-secret').element as HTMLTextAreaElement).value).toBe('existing pasted key')
    expect(w.find('[data-testid="ssh-public-key"]').exists()).toBe(false)
  })
})
