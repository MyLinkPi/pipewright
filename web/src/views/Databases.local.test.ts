/**
 * Databases.local.test.ts — 只读模式下写操作入口守卫的回归测试。
 *
 * 背景:实例默认只读,此前建库/插行/建号等写入口照常可点,用户填完表单提交才收到
 * 403「实例处于只读模式,请先解锁写模式」。现在写入口统一走 guardWrite:
 * 只读 → 立刻弹解锁确认框(注明解锁后将继续的动作),确认解锁后自动继续原操作;
 * 取消则什么都不发生。SQL 控制台不受影响(SELECT 在只读下仍可执行)。
 * 另覆盖 MySQL 授权/回收操作区(与 PG 对齐):库下拉、只读守卫与调用参数。
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'

const listInstancesMock = vi.fn()
const getServerInfoMock = vi.fn()
const listDatabasesMock = vi.fn()
const setReadonlyMock = vi.fn()
const listUsersMock = vi.fn()
const getDBUserGrantsMock = vi.fn()
const grantDBUserMock = vi.fn()

vi.mock('vue-router', () => ({
  useRoute: () => ({ query: {} }),
  RouterLink: { props: ['to'], template: '<a><slot /></a>' },
}))

vi.mock('../api/dbAdmin', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api/dbAdmin')>()
  return {
    ...actual,
    listDBInstances: (...a: unknown[]) => listInstancesMock(...a),
    getDBServerInfo: (...a: unknown[]) => getServerInfoMock(...a),
    listDBDatabases: (...a: unknown[]) => listDatabasesMock(...a),
    setDBInstanceReadonly: (...a: unknown[]) => setReadonlyMock(...a),
    listDBUsers: (...a: unknown[]) => listUsersMock(...a),
    getDBUserGrants: (...a: unknown[]) => getDBUserGrantsMock(...a),
    grantDBUser: (...a: unknown[]) => grantDBUserMock(...a),
  }
})

import Databases from './Databases.vue'
import type { DBInstance } from '../api/dbAdmin'

function instance(over: Partial<DBInstance> = {}): DBInstance {
  return {
    id: 'db-1',
    name: 'mysql-main',
    engine: 'mysql',
    host: '10.0.0.8',
    port: 3306,
    username: 'root',
    tls: '',
    readonly: true,
    source: 'manual',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
    ...over,
  }
}

async function mountView(): Promise<VueWrapper> {
  const w = mount(Databases)
  await flushPromises()
  return w
}

/** 解锁/建库等对话框都 Teleport 到 body,须从 document 上找。 */
function bodyModal(): Element | null {
  return document.body.querySelector('.modal-scrim')
}

function bodyText(): string {
  return bodyModal()?.textContent ?? ''
}

/** 建库弹窗以输入框 placeholder(new_database)识别,placeholder 不进 textContent。 */
function createDbInputPlaceholder(): string {
  return bodyModal()?.querySelector('input')?.getAttribute('placeholder') ?? ''
}

beforeEach(() => {
  document.body.innerHTML = ''
  vi.clearAllMocks()
  listInstancesMock.mockResolvedValue({ items: [instance()] })
  getServerInfoMock.mockResolvedValue({ engine: 'mysql', version: '8.0', versionFull: '8.0.36', uptimeSeconds: 100 })
  listDatabasesMock.mockResolvedValue({ items: [{ name: 'app', tablesCount: 1, sizeBytes: 1024 }] })
  setReadonlyMock.mockImplementation(async (_id: string, readonly: boolean) => instance({ readonly }))
  listUsersMock.mockResolvedValue({ items: [] })
})

describe('Databases · 只读写入口守卫', () => {
  it('只读时点建库:立刻弹解锁框(而非建库表单),并注明解锁后将继续的动作', async () => {
    const w = await mountView()
    await w.find('.sidebar-head .icon-btn').trigger('click')

    expect(bodyText()).toContain('解锁写模式')
    expect(bodyText()).toContain('解锁后将直接继续:新建数据库')
    expect(createDbInputPlaceholder()).toBe('')
    expect(setReadonlyMock).not.toHaveBeenCalled()
    w.unmount()
  })

  it('确认解锁:调用解锁接口后自动继续原操作(建库表单直接打开)', async () => {
    const w = await mountView()
    await w.find('.sidebar-head .icon-btn').trigger('click')

    const confirm = bodyModal()?.querySelector('.btn-danger') as HTMLButtonElement
    confirm.click()
    await flushPromises()

    expect(setReadonlyMock).toHaveBeenCalledWith('db-1', false)
    expect(createDbInputPlaceholder()).toBe('new_database')
    w.unmount()
  })

  it('取消解锁:不调接口,也不继续原操作', async () => {
    const w = await mountView()
    await w.find('.sidebar-head .icon-btn').trigger('click')

    const cancel = bodyModal()?.querySelector('.btn-secondary') as HTMLButtonElement
    cancel.click()
    await flushPromises()

    expect(setReadonlyMock).not.toHaveBeenCalled()
    expect(bodyModal()).toBeNull()
    w.unmount()
  })

  it('用户页签的建号入口同样被守卫', async () => {
    const w = await mountView()
    const usersTab = w.findAll('.dba-tab').find((b) => b.text().includes('用户'))
    await usersTab!.trigger('click')
    await flushPromises()

    const create = w.findAll('button').find((b) => b.text() === '新建用户')
    await create!.trigger('click')

    expect(bodyText()).toContain('解锁后将直接继续:新建用户')
    expect(listUsersMock).toHaveBeenCalled()
    w.unmount()
  })

  it('写模式下入口直接放行:点建库不再弹解锁框', async () => {
    listInstancesMock.mockResolvedValue({ items: [instance({ readonly: false })] })
    const w = await mountView()
    await w.find('.sidebar-head .icon-btn').trigger('click')

    expect(createDbInputPlaceholder()).toBe('new_database')
    expect(bodyText()).not.toContain('解锁写模式')
    w.unmount()
  })

  it('SQL 控制台不被守卫误伤:只读下执行按钮仍可用(SELECT 合法)', async () => {
    const w = await mountView()
    const sqlTab = w.findAll('.dba-tab').find((b) => b.text().includes('SQL'))
    await sqlTab!.trigger('click')

    const textarea = w.find('textarea.sql-input')
    await textarea.setValue('SELECT 1')
    const run = w.find('.sql-actions .btn-primary')
    expect((run.element as HTMLButtonElement).disabled).toBe(false)
    w.unmount()
  })
})

describe('Databases · MySQL 授权/回收', () => {
  /** 进入用户页签并选中账号 app@'%(触发 SHOW GRANTS 加载与目标库默认值)。 */
  async function selectAppUser(w: VueWrapper): Promise<void> {
    const usersTab = w.findAll('.dba-tab').find((b) => b.text().includes('用户'))
    await usersTab!.trigger('click')
    await flushPromises()
    const row = w.findAll('tr.row-click').find((r) => r.text().includes('app'))
    await row!.trigger('click')
    await flushPromises()
  }

  beforeEach(() => {
    listUsersMock.mockResolvedValue({ items: [{ name: 'app', host: '%' }] })
    getDBUserGrantsMock.mockResolvedValue({ engine: 'mysql', lines: ["GRANT SELECT ON `app`.* TO 'app'@'%'"] })
    grantDBUserMock.mockResolvedValue({ ok: true })
  })

  it('授权面板:SHOW GRANTS 原文 + 库下拉(来自库列表,默认选中)+ 模板 + 授权/回收按钮', async () => {
    const w = await mountView()
    await selectAppUser(w)

    const panel = w.find('.grants-panel')
    expect(panel.text()).toContain('GRANT SELECT')
    const [dbSelect, templateSelect] = panel.findAll('select')
    expect(dbSelect.findAll('option').map((o) => o.text())).toEqual(['app'])
    expect((dbSelect.element as HTMLSelectElement).value).toBe('app')
    expect(templateSelect.findAll('option').map((o) => o.text())).toEqual(['全部权限', '只读', '增删改查'])
    const texts = panel.findAll('button').map((b) => b.text())
    expect(texts).toContain('授权')
    expect(texts).toContain('回收')
    w.unmount()
  })

  it('只读时点授权:立刻弹解锁框(注明将继续:授权),不调授权接口', async () => {
    const w = await mountView()
    await selectAppUser(w)

    const grant = w.findAll('button').find((b) => b.text() === '授权')
    await grant!.trigger('click')

    expect(bodyText()).toContain('解锁写模式')
    expect(bodyText()).toContain('解锁后将直接继续:授权')
    expect(grantDBUserMock).not.toHaveBeenCalled()
    expect(setReadonlyMock).not.toHaveBeenCalled()
    w.unmount()
  })

  it('写模式下点授权:带 账号/host/库/模板 调用 grantDBUser', async () => {
    listInstancesMock.mockResolvedValue({ items: [instance({ readonly: false })] })
    const w = await mountView()
    await selectAppUser(w)

    const grant = w.findAll('button').find((b) => b.text() === '授权')
    await grant!.trigger('click')
    await flushPromises()

    expect(grantDBUserMock).toHaveBeenCalledWith('db-1', {
      name: 'app', host: '%', database: 'app', template: 'read',
    })
    w.unmount()
  })
})
