import { describe, expect, it } from 'vitest'
import { isCIDR, isIP, isPeerAddress, validateProxies, validateRoutes, validateSecGroups, validateVirtualIP } from './validate'

describe('isIP', () => {
  it('接受合法 IPv4 与 IPv6', () => {
    expect(isIP('127.0.0.1')).toBe(true)
    expect(isIP('10.126.14.2')).toBe(true)
    expect(isIP('::1')).toBe(true)
  })

  it('拒绝越界/畸形地址', () => {
    expect(isIP('256.1.1.1')).toBe(false)
    expect(isIP('1.2.3')).toBe(false)
    expect(isIP('abc')).toBe(false)
    expect(isIP('')).toBe(false)
  })
})

describe('isCIDR', () => {
  it('接受合法 CIDR', () => {
    expect(isCIDR('192.168.30.0/24')).toBe(true)
    expect(isCIDR('10.0.0.0/8')).toBe(true)
    expect(isCIDR('fd00::/64')).toBe(true)
  })

  it('拒绝非法 CIDR', () => {
    expect(isCIDR('192.168.30.0')).toBe(false)
    expect(isCIDR('192.168.30.0/33')).toBe(false)
    expect(isCIDR('300.1.0.0/16')).toBe(false)
  })
})

describe('isPeerAddress', () => {
  it('接受协议前缀与裸 host:port', () => {
    expect(isPeerAddress('tcp://1.2.3.4:11010')).toBe(true)
    expect(isPeerAddress('ws://peer.example:11011')).toBe(true)
    expect(isPeerAddress('1.2.3.4:11010')).toBe(true)
  })

  it('拒绝空值与未知协议', () => {
    expect(isPeerAddress('')).toBe(false)
    expect(isPeerAddress('http://x')).toBe(false)
    expect(isPeerAddress('peer')).toBe(false)
  })
})

describe('validateVirtualIP', () => {
  it('空值表示自动分配', () => {
    expect(validateVirtualIP('')).toBeNull()
    expect(validateVirtualIP('10.126.14.9')).toBeNull()
    expect(validateVirtualIP('999.1.1.1')).toContain('不是合法地址')
  })
})

describe('validateRoutes', () => {
  it('跳过空行并校验每一项', () => {
    expect(validateRoutes(['', '  '])).toBeNull()
    expect(validateRoutes(['10.0.0.0/8'])).toBeNull()
    expect(validateRoutes(['10.0.0.0/8', 'bad'])).toContain('bad')
  })
})

describe('validateProxies', () => {
  it('端口必须唯一且范围合法', () => {
    expect(validateProxies(false, 1080, [{ port: 1081, exitNodes: [] }])).toBeNull()
    expect(validateProxies(false, 1080, [{ port: 0, exitNodes: [] }])).toContain('超出范围')
    expect(
      validateProxies(false, 1080, [
        { port: 1081, exitNodes: [] },
        { port: 1081, exitNodes: [] },
      ]),
    ).toContain('重复')
  })

  it('启用主代理时端口不得与其冲突', () => {
    expect(validateProxies(true, 1080, [{ port: 1080, exitNodes: [] }])).toContain('冲突')
    expect(validateProxies(false, 1080, [{ port: 1080, exitNodes: [] }])).toBeNull()
  })

  it('出口节点必须是合法 IP', () => {
    expect(validateProxies(false, 1080, [{ port: 1081, exitNodes: ['10.0.0.1'] }])).toBeNull()
    expect(validateProxies(false, 1080, [{ port: 1081, exitNodes: ['bad'] }])).toContain('不是合法 IP')
  })
})

describe('validateSecGroups', () => {
  it('三段必填', () => {
    expect(
      validateSecGroups([{ regionId: 'cn-hangzhou', securityGroupId: 'sg-1', description: 'pw' }]),
    ).toBeNull()
    expect(
      validateSecGroups([{ regionId: 'cn-hangzhou', securityGroupId: '', description: 'pw' }]),
    ).toContain('三段')
  })
})
