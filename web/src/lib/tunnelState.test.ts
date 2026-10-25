/**
 * tunnelState.test.ts — 隧道状态元数据纯函数的单测(仿 pm2State 的测试粒度)。
 */
import { describe, it, expect } from 'vitest'
import {
  tunnelBucket,
  tunnelStateMeta,
  tunnelModeMeta,
  tunnelBindLabel,
  tunnelDestLabel,
  tunnelTargetLabel,
  tunnelUptimeLabel,
  tunnelActionsFor,
} from './tunnelState'
import type { SshTunnel } from '../api/tunnels'

describe('tunnelBucket', () => {
  it('active → running;connecting/retrying → pending;stopped/disabled → off', () => {
    expect(tunnelBucket('active')).toBe('running')
    expect(tunnelBucket('connecting')).toBe('pending')
    expect(tunnelBucket('retrying')).toBe('pending')
    expect(tunnelBucket('stopped')).toBe('off')
    expect(tunnelBucket('disabled')).toBe('off')
  })
})

describe('tunnelStateMeta', () => {
  it('五态各有标签与色系;未知态回落 disabled', () => {
    expect(tunnelStateMeta('active').tone).toBe('green')
    expect(tunnelStateMeta('active').pulse).toBe(true)
    expect(tunnelStateMeta('retrying').tone).toBe('red')
    expect(tunnelStateMeta('disabled').label).toBe('已停用')
    expect(tunnelStateMeta('nope' as SshTunnel['status']).tone).toBe('faint')
  })
})

describe('tunnelModeMeta', () => {
  it('四模式映射 ssh 旗子', () => {
    expect(tunnelModeMeta('local').flag).toBe('-L')
    expect(tunnelModeMeta('remote').flag).toBe('-R')
    expect(tunnelModeMeta('dynamic').flag).toBe('-D')
    expect(tunnelModeMeta('vpn').flag).toBe('-w')
    expect(tunnelModeMeta('dynamic').label).toBe('动态')
    expect(tunnelModeMeta('vpn').label).toBe('VPN')
  })
})

describe('tunnelBindLabel / tunnelDestLabel', () => {
  it('bind_port>0 原样;0 用实际分配端口', () => {
    expect(tunnelBindLabel({ mode: 'local', bindHost: '127.0.0.1', bindPort: 8080, actualPort: 0 })).toBe('127.0.0.1:8080')
    expect(tunnelBindLabel({ mode: 'local', bindHost: '127.0.0.1', bindPort: 0, actualPort: 54321 })).toBe('127.0.0.1:54321')
    expect(tunnelBindLabel({ mode: 'local', bindHost: '0.0.0.0', bindPort: 0, actualPort: 0 })).toBe('0.0.0.0:0')
  })
  it('vpn 模式展示 IP + tun 序号', () => {
    expect(tunnelBindLabel({ mode: 'vpn', bindHost: '10.255.0.2', bindPort: 0, actualPort: 0 })).toBe('10.255.0.2(tun0)')
    expect(tunnelDestLabel({ mode: 'vpn', destHost: '10.255.0.1', destPort: 1 })).toBe('10.255.0.1(tun1)')
    expect(tunnelDestLabel({ mode: 'local', destHost: 'db', destPort: 5432 })).toBe('db:5432')
  })
})

describe('tunnelTargetLabel', () => {
  it('绑服务器显示服务器名;直连显示 user@host', () => {
    expect(tunnelTargetLabel({ serverId: 's1', serverName: 'web-1', host: '', user: '' })).toBe('web-1')
    expect(tunnelTargetLabel({ serverId: 's1', serverName: '', host: '', user: '' })).toBe('s1')
    expect(tunnelTargetLabel({ serverId: '', serverName: '', host: '1.2.3.4', user: 'root' })).toBe('root@1.2.3.4')
  })
})

describe('tunnelUptimeLabel', () => {
  it('空/未来值返回空串;过去值给相对时间', () => {
    expect(tunnelUptimeLabel('')).toBe('')
    expect(tunnelUptimeLabel('9999-01-01T00:00:00Z')).toBe('')
    expect(typeof tunnelUptimeLabel(new Date(Date.now() - 3 * 60_000).toISOString())).toBe('string')
  })
})

describe('tunnelActionsFor', () => {
  it('运行/连接/重连态 → 重启+停止;待启动/停用 → 启动', () => {
    const running = tunnelActionsFor('active').map((a) => a.action)
    expect(running).toEqual(['restart', 'stop'])
    expect(tunnelActionsFor('retrying').map((a) => a.action)).toEqual(['restart', 'stop'])
    expect(tunnelActionsFor('stopped').map((a) => a.action)).toEqual(['start'])
    expect(tunnelActionsFor('disabled').map((a) => a.action)).toEqual(['start'])
  })
})
