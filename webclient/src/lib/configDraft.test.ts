import { describe, expect, it } from 'vitest'
import type { ConfigView, StatusSnapshot } from '../api/types'
import {
  aliyunDraft,
  buildAliyunPatch,
  buildEasytierPatch,
  buildJoinPatch,
  buildRoutesPatch,
  buildSocksPatch,
  describeConfig,
  joinDraft,
  socksDraft,
  type JoinDraft,
  type SocksDraft,
} from './configDraft'

function view(): ConfigView {
  return {
    enabled: true,
    join: {
      networkName: 'net-a',
      hasSecret: true,
      hasCredential: false,
      secureMode: true,
      peers: ['tcp://1.2.3.4:11010'],
      virtualIp: '',
      hostname: 'host-a',
    },
    routes: ['10.0.0.0/8'],
    socks5: { enabled: true, port: 1080, exitNodes: ['10.126.14.2'] },
    proxies: [{ port: 1081, exitNodes: ['10.126.14.2'] }],
    easytier: { binDir: '', rpcAddr: '127.0.0.1:15889' },
    aliyun: {
      accessKeyId: 'LTAI-xxxx',
      hasSecret: true,
      groups: [{ regionId: 'cn-hangzhou', securityGroupId: 'sg-1', description: 'pw' }],
    },
  }
}

describe('buildJoinPatch', () => {
  it('未修改时为空补丁', () => {
    const base = joinDraft(view().join)
    expect(buildJoinPatch({ ...base, peers: [...base.peers] }, base)).toEqual({})
  })

  it('只提交变更字段', () => {
    const base = joinDraft(view().join)
    const draft: JoinDraft = { ...base, hostname: 'host-b', peers: [...base.peers] }
    expect(buildJoinPatch(draft, base)).toEqual({ hostname: 'host-b' })
  })

  it('对端地址增删按数组提交且过滤空行', () => {
    const base = joinDraft(view().join)
    const draft: JoinDraft = { ...base, peers: ['tcp://1.2.3.4:11010', 'tcp://5.6.7.8:11010', '  '] }
    expect(buildJoinPatch(draft, base)).toEqual({
      peers: ['tcp://1.2.3.4:11010', 'tcp://5.6.7.8:11010'],
    })
  })

  it('密钥/凭据只写:留空不提交,填写才提交', () => {
    const base = joinDraft(view().join)
    expect(buildJoinPatch({ ...base, networkSecret: '' }, base)).toEqual({})
    expect(buildJoinPatch({ ...base, networkSecret: 's3cret' }, base)).toEqual({ networkSecret: 's3cret' })
    expect(buildJoinPatch({ ...base, credential: 'cred' }, base)).toEqual({ credential: 'cred' })
  })
})

describe('buildEasytierPatch', () => {
  it('只提交变化的目录/RPC', () => {
    const base = { binDir: '', rpcAddr: '127.0.0.1:15889' }
    expect(buildEasytierPatch(base, base)).toEqual({})
    expect(buildEasytierPatch({ binDir: '/opt/bin', rpcAddr: base.rpcAddr }, base)).toEqual({
      easytierBinDir: '/opt/bin',
    })
  })
})

describe('buildRoutesPatch', () => {
  it('未变化不提交;变化时提交清理后的列表', () => {
    expect(buildRoutesPatch(['10.0.0.0/8'], ['10.0.0.0/8'])).toEqual({})
    expect(buildRoutesPatch(['10.0.0.0/8', ' 192.168.1.0/24 '], ['10.0.0.0/8'])).toEqual({
      routes: ['10.0.0.0/8', '192.168.1.0/24'],
    })
    expect(buildRoutesPatch([''], ['10.0.0.0/8'])).toEqual({ routes: [] })
  })
})

describe('buildSocksPatch', () => {
  it('只提交变化字段', () => {
    const base: SocksDraft = { enabled: true, port: 1080, exitNodes: ['10.126.14.2'] }
    const baseProxies = [{ port: 1081, exitNodes: ['10.126.14.2'] }]
    expect(
      buildSocksPatch({ ...base, exitNodes: [...base.exitNodes] }, [...baseProxies], base, baseProxies),
    ).toEqual({})
    expect(
      buildSocksPatch({ ...base, port: 1082 }, [...baseProxies], base, baseProxies),
    ).toEqual({ socks5Port: 1082 })
    expect(
      buildSocksPatch({ ...base, exitNodes: [...base.exitNodes] }, [{ port: 1082, exitNodes: [] }], base, baseProxies),
    ).toEqual({ socks5Proxies: [{ port: 1082, exitNodes: [] }] })
  })
})

describe('buildAliyunPatch', () => {
  it('SK 留空不提交;AK/组变化才提交', () => {
    const base = aliyunDraft(view().aliyun)
    expect(buildAliyunPatch({ ...base, groups: base.groups.map((g) => ({ ...g })) }, base)).toEqual({})
    const draft = {
      accessKeyId: 'LTAI-new',
      accessKeySecret: 'sk',
      groups: [{ regionId: 'cn-shanghai', securityGroupId: 'sg-2', description: 'pw2' }],
    }
    expect(buildAliyunPatch(draft, base)).toEqual({
      aliyunAccessKeyId: 'LTAI-new',
      aliyunAccessKeySecret: 'sk',
      aliyunGroups: [{ regionId: 'cn-shanghai', securityGroupId: 'sg-2', description: 'pw2' }],
    })
    expect(buildAliyunPatch({ ...base, accessKeySecret: '' }, base)).toEqual({})
  })
})

describe('describeConfig', () => {
  it('区分配置虚拟 IP 与实际虚拟 IP', () => {
    const snap: StatusSnapshot = {
      configured: true,
      enabled: true,
      join: {
        networkName: 'net-a',
        hasSecret: true,
        hasCredential: false,
        secureMode: true,
        peers: [],
        virtualIp: '',
        hostname: 'actual-host',
      },
      routes: ['10.0.0.0/8'],
      socks5: { enabled: true, port: 1080, exitNodes: [] },
      proxies: [],
      supervisor: { running: true, lastErr: '', restarts: 0, startedAt: '', adopted: false },
      easytier: {
        ok: true,
        err: '',
        selfIp: '10.126.14.9',
        hostname: 'actual-host',
        version: '',
        links: [],
        proxyCidrs: [],
        proxyRoutes: [],
      },
      binaries: { corePath: '/bin/core', cliPath: '', version: 'v2.6.4', downloading: false, lastErr: '' },
      service: { runningAs: 'service', pid: 1 },
      clientVersion: 'v1.6.7',
      clientPath: '',
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
    }
    const rows = describeConfig(view(), snap)
    const vip = rows.find((r) => r.key === 'virtualIp')
    expect(vip?.configured).toContain('自动分配')
    expect(vip?.actual).toBe('10.126.14.9')
    const host = rows.find((r) => r.key === 'hostname')
    expect(host?.configured).toBe('host-a')
    expect(host?.actual).toBe('actual-host')
  })
})

describe('socksDraft', () => {
  it('复制出口数组避免共享引用', () => {
    const source = { enabled: true, port: 1080, exitNodes: ['10.0.0.1'] }
    const draft = socksDraft(source)
    draft.exitNodes.push('10.0.0.2')
    expect(source.exitNodes).toHaveLength(1)
  })
})
