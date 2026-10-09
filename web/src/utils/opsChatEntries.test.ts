import { describe, expect, it } from 'vitest'
import type { Entry } from '../api/opsChat'
import {
  cursorSeq,
  makeCursor,
  mergeEntries,
  visibleKinds,
} from './opsChatEntries'

function entry(seq: number, over: Partial<Entry> = {}): Entry {
  return { sessionId: 's1', seq, kind: 'user', createdAt: 't', ...over }
}

describe('mergeEntries', () => {
  it('filters entries of other sessions and invisible kinds', () => {
    const result = mergeEntries('s1', [], [
      entry(1),
      entry(2, { sessionId: 's2' }),
      entry(3, { kind: 'run' }),
      entry(4, { kind: 'assistant' }),
    ])
    expect(result.map((e) => e.seq)).toEqual([1, 4])
  })

  it('deduplicates by seq with incoming entries winning', () => {
    const result = mergeEntries(
      's1',
      [entry(1, { text: 'old' }), entry(2, { text: 'keep' })],
      [entry(1, { text: 'new' })],
    )
    expect(result).toHaveLength(2)
    expect(result[0]).toMatchObject({ seq: 1, text: 'new' })
    expect(result[1]).toMatchObject({ seq: 2, text: 'keep' })
  })

  it('sorts by seq ascending', () => {
    const result = mergeEntries(
      's1',
      [entry(5), entry(1)],
      [entry(3), entry(2)],
    )
    expect(result.map((e) => e.seq)).toEqual([1, 2, 3, 5])
  })

  it('keeps only the last 2000 entries', () => {
    const existing = Array.from({ length: 2000 }, (_, i) => entry(i + 1))
    const result = mergeEntries('s1', existing, [entry(2001)])
    expect(result).toHaveLength(2000)
    expect(result[0].seq).toBe(2)
    expect(result[1999].seq).toBe(2001)
  })

  it('exposes the visible kinds set', () => {
    expect([...visibleKinds].sort()).toEqual(
      ['analysis', 'assistant', 'tool_call', 'tool_request', 'user'].sort(),
    )
  })
})

describe('cursor helpers', () => {
  it('makeCursor and cursorSeq round-trip', () => {
    const cursor = makeCursor('session-1', 42)
    expect(cursor).toBe('session-1:42')
    expect(cursorSeq(cursor)).toBe(42)
  })

  it('makeCursor accepts string watermarks', () => {
    expect(makeCursor('s1', '7')).toBe('s1:7')
    expect(cursorSeq(makeCursor('s1', '7'))).toBe(7)
  })

  it('cursorSeq parses the segment after the last colon', () => {
    expect(cursorSeq('a:b:123')).toBe(123)
  })

  it('cursorSeq on empty string yields 0', () => {
    expect(cursorSeq('')).toBe(0)
  })

  it('cursorSeq without colon coerces the whole string', () => {
    expect(cursorSeq('9')).toBe(9)
    expect(Number.isNaN(cursorSeq('abc'))).toBe(true)
  })
})
