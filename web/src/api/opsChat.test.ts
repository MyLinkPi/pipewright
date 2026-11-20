import { describe, expect, it, vi } from 'vitest'
import { requestId, sseParser, utf8Bytes, opsApi, formatExecArgv } from './opsChat'
import { http } from './http'
describe('ops wire helpers', () => {
  it('creates a cryptographic UUID on insecure HTTP without randomUUID', () => {
    const spy = vi
      .spyOn(crypto, 'randomUUID')
      .mockImplementation(undefined as unknown as typeof crypto.randomUUID)
    const ids = Array.from({ length: 100 }, requestId)
    expect(new Set(ids).size).toBe(100)
    for (const id of ids)
      expect(id).toMatch(
        /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
      )
    spy.mockRestore()
  })
  it('counts UTF-8, not characters', () => {
    expect(utf8Bytes('中文')).toBe(6)
  })
  it('parses split CRLF frames, multiline JSON and ignores comments', () => {
    const out = vi.fn(),
      parse = sseParser(out)
    parse(': keepalive\r\nevent: ready\r\ndata: {"watermark":9}\r\n')
    expect(out).not.toHaveBeenCalled()
    parse('\r\nid: s:2\nevent: entry\ndata: {\ndata: "seq":2}\n\n')
    expect(out.mock.calls.map((c) => c[0])).toEqual([
      { type: 'ready', id: '', data: { watermark: 9 } },
      { type: 'entry', id: 's:2', data: { seq: 2 } },
    ])
  })
  it('bounds incomplete and complete frames', () => {
    expect(() => sseParser(vi.fn())('x'.repeat(256 * 1024 + 1))).toThrow()
    expect(() =>
      sseParser(vi.fn())('data: "' + 'x'.repeat(256 * 1024 + 1) + '"\n\n'),
    ).toThrow()
  })
  it('sends the full exact confirmation without view-only properties', async () => {
    const post = vi.spyOn(http, 'post').mockResolvedValue({})
    const calls = [
      {
        callId: 'c',
        serverId: 's',
        toolId: 'docker_action',
        object: 'a'.repeat(64),
        argsHash: 'a',
        targetHash: 'b',
      },
    ]
    await opsApi.confirm('chat', {
      runId: 'r',
      nonce: 'n',
      expiresAt: 'time',
      valid: true,
      calls,
    })
    expect(post).toHaveBeenCalledWith(
      '/api/ai/ops/sessions/chat/calls/confirm',
      { runId: 'r', nonce: 'n', calls },
    )
    post.mockRestore()
  })
  it('accepts legal escaped analysis frames and continues to the next event', () => {
    const deliver = vi.fn(),
      parse = sseParser(deliver)
    const text = '<'.repeat(16 * 1024)
    const body = JSON.stringify({ text }).replace(/</g, '\\u003c')
    const frame = `event: entry\ndata: ${body}\n\n`
    parse(frame.slice(0, 70000))
    parse(frame.slice(70000) + 'event: heartbeat\ndata: {"watermark":2}\n\n')
    expect(deliver.mock.calls[0]?.[0].data.text).toBe(text)
    expect(deliver.mock.calls[1]?.[0].type).toBe('heartbeat')
  })
})

describe('formatExecArgv', () => {
  it('renders simple argv as a readable command line', () => {
    expect(formatExecArgv(['docker', 'system', 'df'])).toBe('docker system df')
    expect(formatExecArgv(['du', '-sh', '/var/log'])).toBe('du -sh /var/log')
    expect(formatExecArgv(['ls'])).toBe('ls')
  })
  it('single-quotes tokens with spaces, metacharacters or empty string', () => {
    expect(formatExecArgv(['echo', 'hello world'])).toBe(`echo 'hello world'`)
    expect(formatExecArgv(['sh', '-c', 'a; b | c'])).toBe(`sh -c 'a; b | c'`)
    expect(formatExecArgv(['printf', ''])).toBe(`printf ''`)
    expect(formatExecArgv(['grep', 'a$b'])).toBe(`grep 'a$b'`)
  })
  it('escapes embedded single quotes POSIX-style', () => {
    expect(formatExecArgv(['echo', "it's"])).toBe(`echo 'it'\\''s'`)
  })
  it('makes bidi, zero-width, tag and non-ASCII whitespace characters visible', () => {
    expect(formatExecArgv(['echo', '\u202efoo\u200b\u00a0'])).toBe(`echo '\\u{202e}foo\\u{200b}\\u{a0}'`)
    expect(formatExecArgv(['echo', 'a\u2028b\u{e0061}'])).toBe(`echo 'a\\u{2028}b\\u{e0061}'`)
    expect(formatExecArgv(['echo', '磁盘'])).toBe(`echo '磁盘'`)
  })
  it('distinguishes invisible characters from identical-looking literal backslash text', () => {
    const actual = formatExecArgv(['echo', '\u202e'])
    const literal = formatExecArgv(['echo', String.raw`\u{202e}`])
    expect(actual).toBe(String.raw`echo '\u{202e}'`)
    expect(literal).toBe(String.raw`echo '\\u{202e}'`)
    expect(actual).not.toBe(literal)
  })
})
