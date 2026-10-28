// 纯函数校验:前端即时提示,后端 config.Normalize 仍会兜底并返回更精确的错误。
import type { AliyunGroupConfig, ProxyConfig } from '../api/types'

export function isIP(value: string): boolean {
  const v = value.trim()
  if (v === '') return false
  if (v.includes(':')) {
    // 粗略 IPv6:十六进制/冒号/点/百分号,且至少两个冒号分组。
    return /^[0-9a-fA-F:.%]+$/.test(v) && v.split(':').length >= 3
  }
  const parts = v.split('.')
  if (parts.length !== 4) return false
  return parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) <= 255)
}

export function isCIDR(value: string): boolean {
  const v = value.trim()
  if (v === '') return false
  const pieces = v.split('/')
  if (pieces.length !== 2) return false
  const [addr, bits] = pieces
  if (!isIP(addr) || bits === '') return false
  const n = Number(bits)
  const max = addr.includes(':') ? 128 : 32
  return Number.isInteger(n) && n >= 0 && n <= max
}

// isPeerAddress 接受 EasyTier 的 peer 形式(tcp://host:port、udp://、ws://、wss://、quic://)
// 或裸 host:port。
export function isPeerAddress(value: string): boolean {
  const v = value.trim()
  if (v === '') return false
  const matched = v.match(/^([a-zA-Z]+):\/\/(.+)$/)
  if (matched) {
    return /^(tcp|udp|ws|wss|quic)$/i.test(matched[1]) && matched[2].length > 0
  }
  return v.includes(':')
}

export function validateVirtualIP(value: string): string | null {
  const v = value.trim()
  if (v === '') return null // 空 = DHCP 自动分配
  return isIP(v) ? null : '虚拟 IP 不是合法地址(留空 = 自动分配)'
}

export function validatePeers(peers: string[]): string | null {
  for (const raw of peers) {
    const peer = raw.trim()
    if (peer === '') continue
    if (!isPeerAddress(peer)) {
      return `对端地址「${peer}」格式无效(形如 tcp://1.2.3.4:11010)`
    }
  }
  return null
}

export function validateRoutes(routes: string[]): string | null {
  for (const raw of routes) {
    const cidr = raw.trim()
    if (cidr === '') continue
    if (!isCIDR(cidr)) {
      return `网段「${cidr}」不是合法 CIDR(形如 192.168.30.0/24)`
    }
  }
  return null
}

export function validateProxies(
  socks5Enabled: boolean,
  socks5Port: number,
  proxies: ProxyConfig[],
): string | null {
  const seen = new Set<number>()
  for (const p of proxies) {
    if (!Number.isInteger(p.port) || p.port < 1 || p.port > 65535) {
      return `多出口代理端口 ${p.port} 超出范围(1-65535)`
    }
    if (seen.has(p.port)) return `多出口代理端口 ${p.port} 重复`
    if (socks5Enabled && p.port === socks5Port) {
      return `多出口代理端口 ${p.port} 与主 SOCKS5 端口冲突`
    }
    seen.add(p.port)
    for (const exit of p.exitNodes) {
      if (exit.trim() !== '' && !isIP(exit)) {
        return `代理 ${p.port} 的出口节点「${exit}」不是合法 IP`
      }
    }
  }
  if (socks5Enabled && (!Number.isInteger(socks5Port) || socks5Port < 1 || socks5Port > 65535)) {
    return `SOCKS5 端口 ${socks5Port} 超出范围(1-65535)`
  }
  return null
}

export function validateSecGroups(groups: AliyunGroupConfig[]): string | null {
  for (const g of groups) {
    const region = g.regionId.trim()
    const id = g.securityGroupId.trim()
    const desc = g.description.trim()
    if (region === '' || id === '' || desc === '') {
      return '安全组每行须填「地域 安全组ID 规则描述」三段'
    }
  }
  return null
}
