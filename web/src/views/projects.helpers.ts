/**
 * projects.helpers.ts — 项目创建页的纯逻辑:仓库地址解析 + 「凭据被远端拒绝」提示分类。
 *
 * 抽离动机:这两件事都必须与后端 internal/gitauth.ParseRepoURL 同语义(先看 "://",
 * 再回退 SCP 风格 [user@]host:path),且直接决定用户看到的排障方向(换令牌 / 补公钥 /
 * 换凭据类型)。写在 .vue 里无法被 vitest 直接覆盖,故独立成模块。
 */
import type { CredentialType } from '../api/credentials'

/** SSH 系凭据:走 SSH 传输层认证,只能配 ssh:// 或 git@host:path 地址。 */
const SSH_CRED_TYPES: readonly CredentialType[] = ['ssh_key', 'ssh_password']

/** 从仓库地址解析 host(http(s)/ssh scheme 或 git@host:path SCP 风格);解析不出返回空串。 */
export function repoHost(repoUrl: string): string {
  const s = repoUrl.trim()
  const schemeIdx = s.indexOf('://')
  if (schemeIdx > 0) {
    try {
      return new URL(s).hostname
    } catch {
      return ''
    }
  }
  // SCP 风格:冒号必须在首个斜杠之前(path 可含 /),与后端 scpLikeRe 同语义。
  const scp = s.match(/^(?:[^@/]+@)?([^/:]+):/)
  return scp ? scp[1] : ''
}

/** 仓库地址是否 SSH 协议:显式 ssh:// scheme,或无 scheme 的 [user@]host:path SCP 风格。 */
export function isSSHRepoUrl(repoUrl: string): boolean {
  const s = repoUrl.trim()
  const i = s.indexOf('://')
  if (i >= 0) return s.slice(0, i).toLowerCase() === 'ssh'
  return repoHost(s) !== ''
}

/**
 * credential_error 的提示分类。后端把「协议与凭据类型错配」(gitauth.ErrSchemeMismatch)
 * 与「服务端拒绝认证」都映射为 project.ErrCredentialError → HTTP code credential_error,
 * 前端只能靠 (地址协议 × 凭据类型) 自行区分:
 *
 *   unknown          地址解析不出 host → 无法定向,回退后端消息
 *   mismatchSshUrl   SSH 地址 + 令牌/密码凭据 → 该换凭据类型(或换 HTTPS 地址)
 *   mismatchHttpsUrl http(s) 地址 + SSH 凭据 → 该换凭据类型(或换 SSH 地址)
 *   ssh              SSH 地址 + SSH 凭据 → 公钥未登记 / 账号无仓库权限
 *   token            http(s) 地址 + 令牌凭据 → 令牌无效 / 账号无仓库权限
 */
export type CredentialRejectKind =
  | 'unknown'
  | 'mismatchSshUrl'
  | 'mismatchHttpsUrl'
  | 'ssh'
  | 'token'

/**
 * 按仓库地址协议与凭据类型分类凭据被拒原因。credType 缺省(凭据不在可选列表中)时
 * 只按地址协议给出 ssh/token 两类提示。
 */
export function classifyCredentialReject(
  repoUrl: string,
  credType?: CredentialType,
): CredentialRejectKind {
  if (!repoHost(repoUrl)) return 'unknown'
  const sshUrl = isSSHRepoUrl(repoUrl)
  if (!credType) return sshUrl ? 'ssh' : 'token'
  const sshCred = SSH_CRED_TYPES.includes(credType)
  if (sshUrl !== sshCred) return sshUrl ? 'mismatchSshUrl' : 'mismatchHttpsUrl'
  return sshUrl ? 'ssh' : 'token'
}
