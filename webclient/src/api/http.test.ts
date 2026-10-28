import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, OfflineError, getToken, http, initToken, setToken } from './http'

describe('initToken', () => {
  beforeEach(() => {
    sessionStorage.clear()
    window.history.replaceState(null, '', '/')
  })

  it('从 query 读取并清理地址栏', () => {
    window.history.replaceState(null, '', '/?t=query-token')
    initToken()
    expect(getToken()).toBe('query-token')
    expect(window.location.search).toBe('')
  })

  it('兼容从 fragment 读取并清理地址栏', () => {
    window.location.hash = '#t=abc123'
    initToken()
    expect(getToken()).toBe('abc123')
    expect(window.location.hash).toBe('')
  })

  it('无令牌时保留已存令牌', () => {
    setToken('kept')
    initToken()
    expect(getToken()).toBe('kept')
  })
})

describe('http', () => {
  beforeEach(() => {
    setToken('tk')
    vi.unstubAllGlobals()
  })

  it('请求携带 X-PW-Token 并解析 JSON', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    vi.stubGlobal('fetch', fetchMock)
    const result = await http.get<{ ok: boolean }>('/api/status')
    expect(result.ok).toBe(true)
    const init = fetchMock.mock.calls[0][1] as RequestInit
    expect((init.headers as Record<string, string>)['X-PW-Token']).toBe('tk')
  })

  it('403 错误信封转为 ApiError', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: '未授权:界面令牌缺失或错误' }), {
          status: 403,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    )
    await expect(http.get('/api/status')).rejects.toMatchObject({
      status: 403,
      message: '未授权:界面令牌缺失或错误',
    })
    await expect(http.get('/api/status')).rejects.toBeInstanceOf(ApiError)
  })

  it('503 归一为 OfflineError', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: '后台服务未运行' }), {
          status: 503,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    )
    await expect(http.get('/api/status')).rejects.toBeInstanceOf(OfflineError)
  })

  it('网络错误转为 status=0 的 ApiError', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('boom')))
    await expect(http.get('/api/status')).rejects.toMatchObject({ status: 0, message: 'boom' })
  })
})
