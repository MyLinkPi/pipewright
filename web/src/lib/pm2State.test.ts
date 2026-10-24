/**
 * pm2State.test.ts — 状态归并三桶 / 行 key / 内存格式化 的纯函数回归。
 */
import { describe, it, expect } from 'vitest'
import { pm2Bucket, pm2RowKey, formatPm2Bytes, pm2ActionsFor } from './pm2State'

describe('pm2Bucket', () => {
  it('online 含 launching;stopped 含 stopping;其余归 errored', () => {
    expect(pm2Bucket('online')).toBe('online')
    expect(pm2Bucket('launching')).toBe('online')
    expect(pm2Bucket('stopping')).toBe('stopped')
    expect(pm2Bucket('stopped')).toBe('stopped')
    expect(pm2Bucket('errored')).toBe('errored')
  })
})

describe('pm2RowKey', () => {
  it('cluster 同名多实例按 name#pm_id 唯一', () => {
    expect(pm2RowKey({ id: 0, name: 'api' })).toBe('api#0')
    expect(pm2RowKey({ id: 1, name: 'api' })).not.toBe(pm2RowKey({ id: 0, name: 'api' }))
  })
})

describe('formatPm2Bytes', () => {
  it('常用量级人读', () => {
    expect(formatPm2Bytes(0)).toBe('0 B')
    expect(formatPm2Bytes(512)).toBe('512 B')
    expect(formatPm2Bytes(1024)).toBe('1.0 KiB')
    expect(formatPm2Bytes(104857600)).toBe('100.0 MiB')
  })
})

describe('pm2ActionsFor', () => {
  it('online → 重启/重载/停止/删除;stopped/errored → 启动/删除', () => {
    const online = pm2ActionsFor('online').map((a) => a.action)
    expect(online).toEqual(['restart', 'reload', 'stop', 'delete'])
    const stopped = pm2ActionsFor('stopped').map((a) => a.action)
    expect(stopped).toEqual(['start', 'delete'])
    const errored = pm2ActionsFor('errored').map((a) => a.action)
    expect(errored).toEqual(['start', 'delete'])
  })
})
