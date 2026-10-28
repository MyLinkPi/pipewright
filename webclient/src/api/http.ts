// fetch 封装:
//   - 界面令牌来自 URL fragment(#t=...),读入后存 sessionStorage;
//   - 统一 X-PW-Token 头、超时、错误信封 {error:"..."};
//   - 503 归一为 OfflineError(后台服务未运行),界面据此显示离线横幅。
const TOKEN_KEY = 'pw_ui_token'

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

export class OfflineError extends ApiError {
  constructor(message: string) {
    super(503, message)
    this.name = 'OfflineError'
  }
}

export function setToken(token: string): void {
  sessionStorage.setItem(TOKEN_KEY, token)
}

export function getToken(): string {
  return sessionStorage.getItem(TOKEN_KEY) ?? ''
}

// initToken 从 URL 读取界面令牌并清理地址栏(query 优先,兼容 fragment)。
// 正常情况下 index.html 的内联脚本已先行处理;此处是模块化环境的兜底。
export function initToken(): void {
  let token = new URLSearchParams(window.location.search).get('t') ?? ''
  if (!token) {
    const matched = window.location.hash.match(/(?:^#|&)t=([^&]+)/)
    if (matched) token = decodeURIComponent(matched[1])
  }
  if (!token) return
  setToken(token)
  window.history.replaceState(null, '', window.location.pathname)
}

export function hasToken(): boolean {
  return getToken() !== ''
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  timeoutMs = 15000,
): Promise<T> {
  const controller = new AbortController()
  const timer = window.setTimeout(() => controller.abort(), timeoutMs)
  try {
    const headers: Record<string, string> = { 'X-PW-Token': getToken() }
    let payload: string | undefined
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json'
      payload = JSON.stringify(body)
    }
    const resp = await fetch(path, {
      method,
      headers,
      body: payload,
      signal: controller.signal,
    })
    const text = await resp.text()
    let data: unknown
    if (text) {
      try {
        data = JSON.parse(text)
      } catch {
        data = text
      }
    }
    if (!resp.ok) {
      const message =
        data && typeof data === 'object' && 'error' in data
          ? String((data as { error: unknown }).error)
          : `HTTP ${resp.status}`
      if (resp.status === 503) throw new OfflineError(message)
      throw new ApiError(resp.status, message)
    }
    return data as T
  } catch (err) {
    if (err instanceof ApiError) throw err
    if (err instanceof DOMException && err.name === 'AbortError') {
      throw new ApiError(0, '请求超时')
    }
    throw new ApiError(0, err instanceof Error ? err.message : '网络错误')
  } finally {
    window.clearTimeout(timer)
  }
}

export const http = {
  get<T>(path: string): Promise<T> {
    return request<T>('GET', path)
  },
  post<T>(path: string, body?: unknown): Promise<T> {
    return request<T>('POST', path, body)
  },
  put<T>(path: string, body?: unknown): Promise<T> {
    return request<T>('PUT', path, body)
  },
}

export function isOffline(err: unknown): boolean {
  return err instanceof OfflineError
}

export function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}
