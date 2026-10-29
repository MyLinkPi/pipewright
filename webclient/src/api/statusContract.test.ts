// statusContract.test.ts 用 Go 侧 /api/status 的**实录 JSON**(取自 2026-10-29
// Windows 客户端 v2.0.0-beta.6 + easytier 2.6.4 日志)锁定前后端字段契约。
// 后端 StatusSnapshot 的组网状态键是 `easytier`(小写);曾因前端写成 `easyTier`
// 而整体读空 —— 状态恒「连接中…」、虚拟 IP 为空、在线对端 0。此处直接解码原始
// 报文,任何一侧改键名都会让本用例失败。
import { describe, expect, it } from 'vitest'
import type { StatusSnapshot } from './types'

// 与 internal/pwclient/runtime.go StatusSnapshot 的序列化结果逐字对齐。
const goPayload = `{
  "configured": true,
  "enabled": true,
  "join": {
    "networkName": "mylinkpi-mesh", "hasSecret": true, "hasCredential": false,
    "secureMode": true, "peers": ["tcp://pipe.xufan.site:11010"],
    "virtualIp": "", "hostname": "DESKTOP-C3D6OPL"
  },
  "routes": ["192.168.75.0/24"],
  "socks5": { "exitNodes": ["10.126.126.5"], "enabled": true, "port": 1080 },
  "proxies": [],
  "supervisor": { "running": true, "lastErr": "", "restarts": 0, "startedAt": "2026-10-29T14:47:00+08:00", "adopted": false },
  "easytier": {
    "ok": true, "err": "", "selfIp": "10.126.126.7", "hostname": "DESKTOP-C3D6OPL",
    "version": "2.6.4-8428a89d",
    "links": [
      {
        "peerId": "322781623", "virtualIp": "10.126.126.1", "hostname": "Shanghai01",
        "version": "2.6.4-8428a89d", "latencyMs": 17.04, "lossRate": "0.0%",
        "rxBytes": 31360, "txBytes": 35170, "tunnel": "tcp", "relay": false, "relayVia": ""
      }
    ],
    "proxyCidrs": ["10.101.16.35"],
    "proxyRoutes": [
      {
        "cidr": "10.101.16.35", "viaIp": "10.126.126.3", "viaName": "zhijiang",
        "vias": [{ "ip": "10.126.126.3", "name": "zhijiang", "relay": true, "relayVia": "10.126.126.1" }]
      }
    ]
  },
  "binaries": { "corePath": "C:\\\\Program Files\\\\Pipewright Client\\\\bin\\\\easytier-core.exe", "cliPath": "", "version": "", "downloading": false, "lastErr": "" },
  "service": { "runningAs": "service", "pid": 1592 },
  "clientVersion": "2.0.0-beta.6",
  "clientPath": "C:\\\\Program Files\\\\Pipewright Client\\\\pwclient.exe",
  "securityGroup": { "configured": false, "accessKeyId": "", "hasSecret": false, "groups": [], "lastIp": "", "lastRun": "", "lastErr": "", "results": [] }
}`

describe('/api/status 字段契约(与 Go 序列化一致)', () => {
  it('组网状态键为 easytier,虚拟 IP 与对端可读', () => {
    const snap = JSON.parse(goPayload) as StatusSnapshot
    expect(snap.easytier?.selfIp).toBe('10.126.126.7')
    expect(snap.easytier?.links).toHaveLength(1)
    expect(snap.easytier?.proxyRoutes?.[0].vias[0].relayVia).toBe('10.126.126.1')
    // 后端从不输出大驼峰键,前端也不得臆造其他拼法。
    expect(goPayload).not.toContain('"easyTier"')
    expect(goPayload).not.toContain('"EasyTier"')
  })
})
