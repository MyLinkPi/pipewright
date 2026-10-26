import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import VarsCacheTab from './VarsCacheTab.vue'
import type { BuildConfig, PacVarsConfig, BuildVar } from '../../api/pipelineSettings'

function buildFixture(): BuildConfig {
  return {
    model: 'dockerfile',
    dockerfilePath: 'Dockerfile',
    toolchain: { language: 'go', version: '1.22' },
    artifactType: 'image',
    vars: [],
    cache: { enabled: false, paths: [] },
  }
}

function pacFixture(): PacVarsConfig {
  return {
    defs: [
      { name: 'DB_CONN', description: '生产数据库连接字符串', required: true, secret: true, default: '' },
      { name: 'BASE_DOMAIN', description: '服务注册基域', required: false, secret: false, default: 'internal.example.com' },
    ],
    values: [
      { id: 'pv1', key: 'BASE_DOMAIN', secret: false, value: 'corp.example.com' },
    ],
    ref: 'main',
    file: '.pipewright.yml',
    syncedAt: '2026-01-01T00:00:00Z',
  }
}

function mountTab(build: BuildConfig, pacVars?: PacVarsConfig | null) {
  return mount(VarsCacheTab, { props: { build, credentials: [], pacVars } })
}

describe('VarsCacheTab 流水线变量(仓库声明)', () => {
  it('按声明渲染只读行:名称/徽标/描述/默认值,已填值回显', () => {
    const w = mountTab(buildFixture(), pacFixture())
    const rows = w.findAll('.pacrow')
    expect(rows).toHaveLength(2)

    const secretRow = rows[0]
    expect(secretRow.find('code').text()).toBe('DB_CONN')
    expect(secretRow.find('.pacbadge--req').exists()).toBe(true)
    expect(secretRow.find('.pacbadge--sec').exists()).toBe(true)
    expect(secretRow.find('select.vsel').exists()).toBe(true) // secret → 凭据下拉

    const plainRow = rows[1]
    expect(plainRow.find('input.vv').element instanceof HTMLInputElement).toBe(true)
    expect((plainRow.find('input.vv').element as HTMLInputElement).value).toBe('corp.example.com')
    expect(plainRow.find('.pac-default').text()).toContain('internal.example.com')
  })

  it('编辑明文值 → updatePacVars 只上送已填行(未触碰行保持未填写)', async () => {
    const w = mountTab(buildFixture(), pacFixture())
    await nextTick()
    // BASE_DOMAIN 已有值,改它;DB_CONN(secret)未选凭据 → 不上送。
    await w.findAll('.pacrow')[1].find('input.vv').setValue('new.example.com')
    await nextTick()

    const emitted = w.emitted('updatePacVars')
    expect(emitted).toBeTruthy()
    const vals = emitted!.at(-1)![0] as BuildVar[]
    expect(vals).toHaveLength(1)
    expect(vals[0].key).toBe('BASE_DOMAIN')
    expect(vals[0].value).toBe('new.example.com')
    expect(vals[0].secret).toBe(false)
  })

  it('父组件回写相同内容不再重复 emit(防双向回环)', async () => {
    const w = mountTab(buildFixture(), pacFixture())
    await nextTick()
    await w.findAll('.pacrow')[1].find('input.vv').setValue('new.example.com')
    await nextTick()
    const count = w.emitted('updatePacVars')!.length

    // 模拟父组件按 emit 内容回写 :pac-vars(保存 round-trip 的回声)。
    const echoed = JSON.parse(JSON.stringify(pacFixture())) as PacVarsConfig
    echoed.values = [{ id: 'pv1', key: 'BASE_DOMAIN', secret: false, value: 'new.example.com' }]
    await w.setProps({ pacVars: echoed })
    await nextTick()
    await nextTick()
    expect(w.emitted('updatePacVars')!.length).toBe(count)

    // 输入框不被重建(值仍在)。
    const input = w.findAll('.pacrow')[1].find('input.vv').element as HTMLInputElement
    expect(input.value).toBe('new.example.com')
  })

  it('预览同步改变声明(defs)后重建行并保留按名匹配的已填值', async () => {
    const w = mountTab(buildFixture(), pacFixture())
    await nextTick()
    const synced = JSON.parse(JSON.stringify(pacFixture())) as PacVarsConfig
    synced.defs = [
      { name: 'BASE_DOMAIN', description: '基域', required: false, secret: false, default: '' },
      { name: 'APP_SUB', description: '子域名', required: false, secret: false, default: '' },
    ]
    // 服务端同步保留了 BASE_DOMAIN 的值。
    synced.values = [{ id: 'pv1', key: 'BASE_DOMAIN', secret: false, value: 'corp.example.com' }]
    await w.setProps({ pacVars: synced })
    await nextTick()
    await nextTick()

    const rows = w.findAll('.pacrow')
    expect(rows).toHaveLength(2)
    expect(rows[0].find('code').text()).toBe('BASE_DOMAIN')
    expect((rows[0].find('input.vv').element as HTMLInputElement).value).toBe('corp.example.com')
  })

  it('未同步声明时显示空提示', () => {
    const w = mountTab(buildFixture(), null)
    expect(w.find('.pacrow').exists()).toBe(false)
    // 第一块 .vempty 是构建变量的空提示;流水线变量卡片的空提示在其后。
    const empties = w.findAll('.vempty')
    expect(empties.length).toBeGreaterThanOrEqual(2)
    expect(empties.at(-1)!.text()).toContain('尚未同步仓库变量声明')
  })
})
