import { describe, expect, it } from 'vitest'
import type { StatusSnapshot } from '../api/types'
import { connectionState, peerRows, primaryAction, routeRows } from './derive'

function snapshot(patch: Partial<StatusSnapshot> = {}): StatusSnapshot {
  return {
    configured: true,
    enabled: true,
    join: {
      networkName: 'prod',
      hasSecret: true,
      hasCredential: false,
      secureMode: true,
      peers: ['tcp://1.2.3.4:11010'],
      virtualIp: '',
      hostname: 'host-a',
    },
    routes: ['10.0.0.0/8'],
    socks5: { enabled: false, port: 1080, exitNodes: [] },
    proxies: [],
    supervisor: { running: false, lastErr: '', restarts: 0, startedAt: '', adopted: false },
    easyTier: null,
    binaries: { corePath: '/bin/core', cliPath: '', version: 'v2.6.4', downloading: false, lastErr: '' },
    service: { runningAs: 'service', pid: 100 },
    clientVersion: 'v1.6.7',
    clientPath: '/exe',
    securityGroup: {
      configured: false,
      accessKeyId: '',
      hasSecret: false,
      groups: [],
      lastIp: '',
      lastRun: '',
      lastErr: '',
      results: [],
    },
    ...patch,
  }
}

describe('connectionState', () => {
  it('服务离线优先于一切', () => {
    expect(connectionState(null, true)).toBe('service-down')
    expect(connectionState(snapshot(), true)).toBe('service-down')
  })

  it('未配置 / 连接中 / 已连接 / 已断开', () => {
    expect(connectionState(snapshot({ configured: false }), false)).toBe('not-configured')
    expect(connectionState(snapshot({ enabled: true }), false)).toBe('connecting')
    expect(
      connectionState(
        snapshot({
          supervisor: { running: true, lastErr: '', restarts: 0, startedAt: '', adopted: false },
          easyTier: {
            ok: true,
            err: '',
            selfIp: '10.126.14.9',
            hostname: 'host-a',
            version: 'v2.6.4',
            links: [],
            proxyCidrs: [],
            proxyRoutes: [],
          },
        }),
        false,
      ),
    ).toBe('connected')
    expect(connectionState(snapshot({ enabled: false }), false)).toBe('disconnected')
  })
})

describe('primaryAction', () => {
  it('按状态派生主按钮', () => {
    expect(primaryAction('service-down').kind).toBe('install-service')
    expect(primaryAction('not-configured').kind).toBe('import')
    expect(primaryAction('connected').kind).toBe('disconnect')
    expect(primaryAction('disconnected').kind).toBe('connect')
    expect(primaryAction('connecting').kind).toBe('connect')
  })
})

describe('peerRows', () => {
  it('直连/中继描述与延迟格式', () => {
    const rows = peerRows([
      {
        peerId: 'p1',
        virtualIp: '10.0.0.1',
        hostname: 'node-a',
        version: 'v2.6.4',
        latencyMs: 12.4,
        lossRate: '0',
        rxBytes: 0,
        txBytes: 0,
        tunnel: 'p2p',
        relay: false,
        relayVia: '',
      },
      {
        peerId: 'p2',
        virtualIp: '10.0.0.2',
        hostname: 'node-b',
        version: '',
        latencyMs: 0,
        lossRate: '',
        rxBytes: 0,
        txBytes: 0,
        tunnel: 'relay',
        relay: true,
        relayVia: '10.0.0.1',
      },
    ])
    expect(rows[0].kind).toBe('直连 p2p')
    expect(rows[0].latency).toBe('12ms')
    expect(rows[1].kind).toBe('中继 via 10.0.0.1')
    expect(rows[1].latency).toBe('—')
  })
})

describe('routeRows', () => {
  it('合并通告网段、网关与已启用状态', () => {
    const snap = snapshot({
      easyTier: {
        ok: true,
        err: '',
        selfIp: '10.126.14.9',
        hostname: 'host-a',
        version: '',
        links: [],
        proxyCidrs: ['10.0.0.0/8', '192.168.1.0/24'],
        proxyRoutes: [
          {
            cidr: '10.0.0.0/8',
            viaIp: '10.126.14.2',
            viaName: 'gw-a',
            vias: [
              { ip: '10.126.14.2', name: 'gw-a', relay: false, relayVia: '' },
              { ip: '10.126.14.3', name: 'gw-b', relay: false, relayVia: '' },
            ],
          },
        ],
      },
    })
    const rows = routeRows(snap)
    expect(rows).toHaveLength(2)
    expect(rows[0]).toMatchObject({ cidr: '10.0.0.0/8', gateway: 'gw-a(10.126.14.2)', backups: 1, enabled: true })
    expect(rows[1]).toMatchObject({ cidr: '192.168.1.0/24', gateway: '—', backups: 0, enabled: false })
  })
})
