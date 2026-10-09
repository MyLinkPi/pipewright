import type { Entry } from '../api/opsChat'

export const visibleKinds = new Set([
  'user',
  'assistant',
  'analysis',
  'tool_request',
  'tool_call',
])

export function mergeEntries(
  currentId: string,
  existing: Entry[],
  incoming: Entry[],
): Entry[] {
  const bySeq = new Map(existing.map((e) => [e.seq, e]))
  for (const entry of incoming)
    if (entry.sessionId === currentId && visibleKinds.has(entry.kind))
      bySeq.set(entry.seq, entry)
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq).slice(-2000)
}

export function makeCursor(id: string, watermark: number | string): string {
  return id + ':' + watermark
}

export function cursorSeq(cursor: string): number {
  return Number(cursor.slice(cursor.lastIndexOf(':') + 1))
}
