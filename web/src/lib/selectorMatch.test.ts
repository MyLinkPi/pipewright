import { describe, expect, it } from 'vitest'
import { matchServers, parseSelectorTerms } from './selectorMatch'
import type { Server } from '../api/servers'

function srv(id: string, name: string, labels = ''): Server {
  return {
    id, name, host: '127.0.0.1', port: 22, user: 'deploy',
    credentialId: 'cred', credentialName: 'cred',
    sudoCredentialId: '', sudoCredentialName: '',
    labels, maxBuilds: 1, priority: 0, createdAt: '', updatedAt: '',
  }
}

const servers = [
  srv('a', 'web-1', 'web,env=prod'),
  srv('b', 'web-2', 'web,env=staging'),
  srv('c', 'db-1', 'db,env=prod'),
  srv('d', 'bare', ''),
]

describe('parseSelectorTerms', () => {
  it('逗号分隔、去空白、丢空项', () => {
    expect(parseSelectorTerms(' web , env=prod ,, ')).toEqual(['web', 'env=prod'])
  })
})

describe('matchServers', () => {
  it('AND 语义:全部项命中才匹配', () => {
    const hit = matchServers('web,env=prod', servers)
    expect(hit.map((s) => s.id)).toEqual(['a'])
  })

  it('纯 tag 不匹配 k=v(严格逐项相等)', () => {
    expect(matchServers('env', servers)).toEqual([])
    expect(matchServers('env=prod', servers).map((s) => s.id)).toEqual(['a', 'c'])
  })

  it('空表达式 → 空数组(无目标,部署侧跳过即成功)', () => {
    expect(matchServers('', servers)).toEqual([])
    expect(matchServers('  ', servers)).toEqual([])
  })

  it('零命中 → 空数组', () => {
    expect(matchServers('gpu', servers)).toEqual([])
  })

  it('未打标签的机器永不命中非空选择器', () => {
    expect(matchServers('web,db', servers)).toEqual([])
  })

  it('server:<id> 钉单机;id 不存在 → 空数组', () => {
    expect(matchServers('server:b', servers).map((s) => s.id)).toEqual(['b'])
    expect(matchServers('server:gone', servers)).toEqual([])
  })
})
