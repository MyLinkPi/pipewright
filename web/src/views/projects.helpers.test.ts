/**
 * projects.helpers.test.ts — 仓库地址解析与「凭据被拒」提示分类的回归。
 *
 * 关键回归点:SSH 地址配了令牌凭据(或反之)时,后端返回的 code 也是 credential_error,
 * 前端必须提示「换凭据类型」而不是「轮换令牌 / 补登记公钥」——否则用户会在错误方向
 * 上反复排查(实测 Codeup SSH 场景)。
 */
import { describe, it, expect } from 'vitest'
import { repoHost, isSSHRepoUrl, classifyCredentialReject } from './projects.helpers'

describe('repoHost', () => {
  it('解析 http(s)/ssh scheme 地址的 host(不含端口与用户段)', () => {
    expect(repoHost('https://gitee.com/a/b.git')).toBe('gitee.com')
    expect(repoHost('https://user:token@gitee.com/a/b.git')).toBe('gitee.com')
    expect(repoHost('https://gitee.com:8443/a/b.git')).toBe('gitee.com')
    expect(repoHost('ssh://git@gitee.com/a/b.git')).toBe('gitee.com')
  })

  it('解析 SCP 风格地址的 host(冒号在首个斜杠前)', () => {
    expect(repoHost('git@gitee.com:a/b.git')).toBe('gitee.com')
    expect(repoHost('gitee.com:a/b.git')).toBe('gitee.com')
    expect(repoHost('git@localhost:8080/x')).toBe('localhost')
  })

  it('解析不出 host 时返回空串(前后空白先裁剪)', () => {
    expect(repoHost('  git@gitee.com:a/b.git  ')).toBe('gitee.com')
    expect(repoHost('')).toBe('')
    expect(repoHost('gitee.com/a/b.git')).toBe('')
    expect(repoHost('https://')).toBe('')
  })
})

describe('isSSHRepoUrl', () => {
  it('ssh:// 与 SCP 风格均为 SSH', () => {
    expect(isSSHRepoUrl('ssh://git@gitee.com/a/b.git')).toBe(true)
    expect(isSSHRepoUrl('SSH://gitee.com/a/b.git')).toBe(true)
    expect(isSSHRepoUrl('git@gitee.com:a/b.git')).toBe(true)
  })

  it('http(s) 与不可解析地址不是 SSH', () => {
    expect(isSSHRepoUrl('https://gitee.com/a/b.git')).toBe(false)
    expect(isSSHRepoUrl('http://gitee.com/a/b.git')).toBe(false)
    expect(isSSHRepoUrl('git://gitee.com/a/b.git')).toBe(false)
    expect(isSSHRepoUrl('gitee.com/a/b.git')).toBe(false)
    expect(isSSHRepoUrl('')).toBe(false)
  })
})

describe('classifyCredentialReject', () => {
  it('host 解析不出 → unknown(回退后端消息)', () => {
    expect(classifyCredentialReject('', 'git_token')).toBe('unknown')
    expect(classifyCredentialReject('gitee.com/a/b.git', 'ssh_key')).toBe('unknown')
  })

  it('SSH 地址 + SSH 凭据 → ssh(提示补登记公钥/权限)', () => {
    expect(classifyCredentialReject('git@gitee.com:a/b.git', 'ssh_key')).toBe('ssh')
    expect(classifyCredentialReject('ssh://gitee.com/a/b.git', 'ssh_password')).toBe('ssh')
  })

  it('http(s) 地址 + 令牌凭据 → token(提示令牌/权限)', () => {
    expect(classifyCredentialReject('https://gitee.com/a/b.git', 'git_token')).toBe('token')
    expect(classifyCredentialReject('https://gitee.com/a/b.git', 'git_http')).toBe('token')
  })

  it('SSH 地址 + 令牌凭据 → mismatchSshUrl(协议错配,不是令牌失效)', () => {
    expect(classifyCredentialReject('git@gitee.com:a/b.git', 'git_token')).toBe('mismatchSshUrl')
    expect(classifyCredentialReject('git@codeup.aliyun.com:a/b.git', 'git_http')).toBe('mismatchSshUrl')
    expect(classifyCredentialReject('ssh://gitee.com/a/b.git', 'git_token')).toBe('mismatchSshUrl')
  })

  it('http(s) 地址 + SSH 凭据 → mismatchHttpsUrl', () => {
    expect(classifyCredentialReject('https://gitee.com/a/b.git', 'ssh_key')).toBe('mismatchHttpsUrl')
    expect(classifyCredentialReject('https://gitee.com/a/b.git', 'ssh_password')).toBe('mismatchHttpsUrl')
  })

  it('凭据类型未知时只按地址协议分类', () => {
    expect(classifyCredentialReject('git@gitee.com:a/b.git')).toBe('ssh')
    expect(classifyCredentialReject('https://gitee.com/a/b.git')).toBe('token')
    expect(classifyCredentialReject('git@gitee.com:a/b.git', undefined)).toBe('ssh')
  })
})
