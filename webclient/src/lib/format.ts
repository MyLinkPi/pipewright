// 展示格式化:与旧托盘 GUI 的语义保持一致(错误只显示首行,空值用破折号)。

export function orDash(value: string | null | undefined): string {
  const v = (value ?? '').trim()
  return v === '' ? '—' : v
}

export function firstLine(text: string, max = 160): string {
  const line = (text ?? '').split('\n')[0].trim()
  return line.length > max ? `${line.slice(0, max)}…` : line
}

export function latency(ms: number): string {
  if (!ms || ms <= 0) return '—'
  return `${Math.round(ms)}ms`
}

// formatTime 把 RFC3339 转成本地「MM-DD HH:mm」;解析失败原样返回(空则破折号)。
export function formatTime(iso: string): string {
  const v = (iso ?? '').trim()
  if (!v) return '—'
  const date = new Date(v)
  if (Number.isNaN(date.getTime())) return v
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`
}

export function formatBytes(n: number): string {
  if (!n || n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = n
  let idx = 0
  while (value >= 1024 && idx < units.length - 1) {
    value /= 1024
    idx += 1
  }
  return `${value.toFixed(value >= 100 || idx === 0 ? 0 : 1)} ${units[idx]}`
}

// shortError 用于列表/卡片中的单行错误(过长截断)。
export function shortError(text: string): string {
  const line = firstLine(text)
  return line === '' ? '—' : line
}
