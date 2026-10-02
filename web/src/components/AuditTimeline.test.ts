import { describe, expect, it, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

// listAudit 在模块加载时被组件引用 → 用 vi.mock 换成可控 stub。
const listAuditMock = vi.fn()
vi.mock('../api/audit', () => ({
  listAudit: (...args: unknown[]) => listAuditMock(...args),
}))

import AuditTimeline from './AuditTimeline.vue'
import type { AuditEntry } from '../api/audit'

function entry(partial: Partial<AuditEntry>): AuditEntry {
  return {
    id: 'a1', timestamp: new Date().toISOString(), actor: 'admin', action: 'credential_create',
    targetType: 'credential', targetId: 'cred-1', detail: {}, ip: '203.0.113.7',
    ...partial,
  }
}

async function mountWith(entries: AuditEntry[]) {
  listAuditMock.mockResolvedValue({ entries, nextBefore: null })
  const wrapper = mount(AuditTimeline)
  await flushPromises()
  return wrapper
}

describe('AuditTimeline action 文案', () => {
  beforeEach(() => listAuditMock.mockReset())

  it('已知 action 渲染为「动词+名词+可读对象」,不再裸奔 action id', async () => {
    const w = await mountWith([
      entry({ id: 'e1', action: 'credential_reveal', targetId: 'cred-9', detail: { name: '部署密钥' } }),
      entry({ id: 'e2', action: 'platformhttps.settings.save', targetId: 'pipe.example.com', detail: { ok: true } }),
      entry({ id: 'e3', action: 'certmgmt.cert.renew', targetId: 'cert-1', detail: { primaryDomain: 'example.com' } }),
      entry({ id: 'e4', action: 'environment.rollback', targetId: 'proj-1', detail: { environment: 'staging' } }),
    ])
    const txt = w.text()
    expect(txt).toContain('查看凭据 部署密钥')
    expect(txt).toContain('修改平台 HTTPS pipe.example.com')
    expect(txt).toContain('续期证书 example.com')
    expect(txt).toContain('回滚环境 staging')
    expect(txt).not.toContain('credential_reveal')
    expect(txt).not.toContain('platformhttps.settings.save')
  })

  it('开关型 action 按 detail 布尔值选动词(停用自动续期)', async () => {
    const w = await mountWith([
      entry({ id: 'e1', action: 'certmgmt.cert.autorenew', targetId: 'cert-1', detail: { enabled: false, primaryDomain: 'example.com' } }),
      entry({ id: 'e2', action: 'certmgmt.cert.autorenew', targetId: 'cert-1', detail: { enabled: true, primaryDomain: 'example.com' } }),
    ])
    const txt = w.text()
    expect(txt).toContain('停用自动续期 example.com')
    expect(txt).toContain('启用自动续期 example.com')
  })

  it('无名词短语与伪目标 ID:批量命令只带命令本身,不出现占位符', async () => {
    const w = await mountWith([
      entry({ id: 'e1', action: 'server_command', targetType: 'server', targetId: 'run-1', detail: { command: 'uname -a' } }),
      entry({ id: 'e2', action: 'platformhttps.disable', targetId: '-', detail: { ok: true } }),
    ])
    const txt = w.text()
    expect(txt).toContain('在多台服务器执行命令 uname -a')
    // 伪目标 '-' 不渲染为对象:仅 server_command 一行有 .obj。
    const objs = w.findAll('.obj').map((n) => n.text())
    expect(objs).toEqual(['uname -a'])
  })

  it('未知 action 退化为「操作 <action id>」并仍显示目标', async () => {
    const w = await mountWith([
      entry({ id: 'e1', action: 'some_future_action', targetId: 'obj-1', detail: {} }),
    ])
    expect(w.text()).toContain('操作 some_future_action')
    expect(w.text()).toContain('obj-1')
  })

  it('历史数据无 detail 时对象回退到 targetId', async () => {
    const w = await mountWith([
      entry({ id: 'e1', action: 'credential_reveal', targetId: '8b793d81-cf52-4675-b4a0-de070b82b372', detail: {} }),
    ])
    expect(w.text()).toContain('查看凭据 8b793d81-cf52-4675-b4a0-de070b82b372')
  })

  it('who 行展示操作来源 IP', async () => {
    const w = await mountWith([entry({ id: 'e1', ip: '198.51.100.23' })])
    expect(w.text()).toContain('198.51.100.23')
  })
})
