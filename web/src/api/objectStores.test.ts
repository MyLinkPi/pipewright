/**
 * objectStores API 客户端回归：
 *  - GET 列表走只读、写方法带 CSRF，URL 正确；
 *  - 只写 Secret 的语义：创建必带 secret，编辑未提供时不带 secret（保留原值）。
 * 照 oauth.test.ts 的 fetch stub 模式。
 */
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import {
  listObjectStores,
  createObjectStore,
  updateObjectStore,
  deleteObjectStore,
  testObjectStore,
} from './objectStores'

function jsonResponse(status: number, body: unknown, ok = status < 400): Response {
  return {
    status,
    ok,
    headers: new Headers({ 'content-type': 'application/json' }),
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response
}

describe('objectStores api client', () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    document.cookie.split(';').forEach((c) => {
      const name = c.split('=')[0].trim()
      if (name) document.cookie = `${name}=; expires=Thu, 01 Jan 1970 00:00:00 GMT`
    })
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('GET /api/object-stores returns the item list, defaulting to [] when absent', async () => {
    const items = [
      {
        id: 'os-1',
        type: 'aliyun_oss',
        name: '生产 OSS',
        endpoint: 'oss-cn-hangzhou.aliyuncs.com',
        bucket: 'prod-bucket',
        accessKeyId: 'LTAI5t',
        credentialConfigured: true,
        createdAt: '2026-01-01T00:00:00Z',
      },
    ]
    fetchMock.mockResolvedValue(jsonResponse(200, { items }))
    expect(await listObjectStores()).toEqual(items)
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/object-stores')
    expect(init.method).toBe('GET')
    // 读方法不带 CSRF
    expect((init.headers as Headers).get('X-CSRF-Token')).toBeNull()

    fetchMock.mockResolvedValue(jsonResponse(200, {}))
    expect(await listObjectStores()).toEqual([])
  })

  it('createObjectStore POSTs the payload with the write-only secret + CSRF', async () => {
    document.cookie = 'pipewright_csrf=tok123'
    fetchMock.mockResolvedValue(jsonResponse(201, { id: 'os-2' }))
    await createObjectStore({
      type: 'aliyun_oss',
      name: '生产 OSS',
      endpoint: 'oss-cn-hangzhou.aliyuncs.com',
      bucket: 'prod-bucket',
      pathPrefix: '/ci/prod/',
      accessKeyId: 'LTAI5t',
      secret: 'super-secret',
    })
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/object-stores')
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body)).toEqual({
      type: 'aliyun_oss',
      name: '生产 OSS',
      endpoint: 'oss-cn-hangzhou.aliyuncs.com',
      bucket: 'prod-bucket',
      pathPrefix: '/ci/prod/',
      accessKeyId: 'LTAI5t',
      secret: 'super-secret',
    })
    expect((init.headers as Headers).get('X-CSRF-Token')).toBe('tok123')
  })

  it('updateObjectStore PUTs to the per-id URL and omits secret when not rotating', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { id: 'os/1' }))
    await updateObjectStore('os/1', { name: 'renamed', bucket: 'new-bucket' })
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/object-stores/os%2F1')
    expect(init.method).toBe('PUT')
    const body = JSON.parse(init.body)
    expect(body).toEqual({ name: 'renamed', bucket: 'new-bucket' })
    expect(body).not.toHaveProperty('secret')
  })

  it('updateObjectStore can change or clear the path prefix (empty string clears)', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, {}))
    await updateObjectStore('os-1', { pathPrefix: '/backups/2026/' })
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ pathPrefix: '/backups/2026/' })
    await updateObjectStore('os-1', { pathPrefix: '' })
    expect(JSON.parse(fetchMock.mock.calls[1][1].body)).toEqual({ pathPrefix: '' })
  })

  it('updateObjectStore forwards the secret when rotating', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, {}))
    await updateObjectStore('os-1', { secret: 'rotated' })
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ secret: 'rotated' })
  })

  it('deleteObjectStore issues DELETE to the per-id URL', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { ok: true }))
    expect(await deleteObjectStore('os-1')).toEqual({ ok: true })
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/object-stores/os-1')
    expect(init.method).toBe('DELETE')
  })

  it('testObjectStore POSTs to {id}/test', async () => {
    const result = { ok: true, latencyMs: 12, detail: 'prod-bucket', error: null }
    fetchMock.mockResolvedValue(jsonResponse(200, result))
    expect(await testObjectStore('os-1')).toEqual(result)
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/object-stores/os-1/test')
    expect(init.method).toBe('POST')
  })
})
