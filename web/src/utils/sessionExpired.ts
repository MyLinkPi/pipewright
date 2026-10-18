/**
 * 会话过期的统一出口。
 *
 * 背景:被动登出后曾出现「页面卡死」——fetch 封装会跳登录,但 SSE(EventSource)
 * 与 WebSocket 不走封装,EventSource 对 401 看不到状态码且会无限自动重连,界面
 * 停在原地。本模块把「确认未登录 → 跳转登录页」收敛为单一实现,供 fetch 封装与
 * SSE/WS 错误路径共用。
 *
 * 跳转用 location.replace 整页加载(不留在历史栈);并发的多个 401 只触发一次。
 */

let redirecting = false

/** 是否已处于「正在跳转登录」状态(调用方可据此停止重试/关闭连接)。 */
export function isRedirectingToLogin(): boolean {
  return redirecting
}

/** 跳转到登录页,携带当前路径作为回跳目标。重复调用只生效一次。 */
export function sessionExpired(): void {
  if (redirecting) return
  redirecting = true
  const current =
    location.pathname + (location.search ?? '') + (location.hash ?? '')
  // 与登录后跳转校验同口径:必须以 / 开头且不以 // 开头(防协议相对开放重定向)。
  const redirect = current.startsWith('/') && !current.startsWith('//') ? current : '/'
  location.replace(`/login?redirect=${encodeURIComponent(redirect)}`)
}

/**
 * 探测会话是否已失效(SSE/WS 断开时调用):直接 fetch 会话端点,
 * 401 → 触发统一跳转并返回 true;仍有效/网络故障 → false(维持原有重试逻辑)。
 * 用原生 fetch 而非 http 封装,避免与封装自身的 401 分支互相递归。
 */
export async function probeSessionExpired(): Promise<boolean> {
  if (redirecting) return true
  try {
    const resp = await fetch('/api/auth/session', { credentials: 'same-origin' })
    if (resp.status === 401) {
      sessionExpired()
      return true
    }
    return false
  } catch {
    // 网络故障不代表登出,交由调用方原有降级逻辑处理。
    return false
  }
}
