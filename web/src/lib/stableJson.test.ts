import { describe, it, expect } from 'vitest'
import { stableStringify } from './stableJson'

describe('stableStringify(键序无关序列化 · 脏检查快照)', () => {
  it('内容等价但键序不同的对象产出同一字符串(false dirty 回归)', () => {
    const fromServer = { name: '构建', config: { image: 'node:20', commands: 'npm ci' } }
    // JobDrawer 以 {...extras, ...typed} 重建 config → 键序不同、内容等价。
    const rebuilt = { name: '构建', config: { commands: 'npm ci', image: 'node:20' } }
    expect(JSON.stringify(fromServer)).not.toBe(JSON.stringify(rebuilt))
    expect(stableStringify(fromServer)).toBe(stableStringify(rebuilt))
  })

  it('嵌套对象/数组内的对象键也递归排序', () => {
    const a = { stages: [{ id: 's1', jobs: [{ id: 'j1', config: { b: '2', a: '1' } }] }] }
    const b = { stages: [{ jobs: [{ config: { a: '1', b: '2' }, id: 'j1' }], id: 's1' }] }
    expect(stableStringify(a)).toBe(stableStringify(b))
  })

  it('数组顺序仍然敏感(顺序即语义)', () => {
    expect(stableStringify([1, 2])).not.toBe(stableStringify([2, 1]))
  })

  it('内容真变更仍判出差异', () => {
    const a = { config: { image: 'node:20' } }
    const b = { config: { image: 'node:22' } }
    expect(stableStringify(a)).not.toBe(stableStringify(b))
  })

  it('原始值与 null/undefined 安全处理', () => {
    expect(stableStringify(null)).toBe('null')
    expect(stableStringify(undefined)).toBe(undefined)
    expect(stableStringify('x')).toBe('"x"')
  })
})
