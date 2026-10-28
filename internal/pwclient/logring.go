// logring.go 是进程内日志环:并发安全的定长行缓冲,既是 log 输出目标(实现 io.Writer),
// 也供 IPC /api/logs 增量截取(新界面日志页按序号只拉新增行)。旧日志文件仍照常落盘,
// 环仅服务 UI 实时查看。
package pwclient

import (
	"strings"
	"sync"
)

// logEntry 是环内一行日志与它的单调序号(序号用于前端增量拉取)。
type logEntry struct {
	seq  uint64
	line string
}

// logRing 是定长日志行环。零值不可用,经 newLogRing 构造;线程安全。
type logRing struct {
	mu   sync.Mutex
	buf  []logEntry
	next int
	max  int
	seq  uint64
}

func newLogRing(maxLines int) *logRing {
	if maxLines <= 0 {
		maxLines = 100
	}
	return &logRing{buf: make([]logEntry, 0, maxLines), max: maxLines}
}

// Write 实现 io.Writer:按行入环(log 标准库的输出天然以 \n 结尾)。
func (r *logRing) Write(p []byte) (int, error) {
	n := len(p)
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		r.mu.Lock()
		r.seq++
		e := logEntry{seq: r.seq, line: line}
		if len(r.buf) < r.max {
			r.buf = append(r.buf, e)
		} else {
			r.buf[r.next] = e
			r.next = (r.next + 1) % r.max
		}
		r.mu.Unlock()
	}
	return n, nil
}

// orderedLocked 返回按时间序的副本(调用方已持锁)。
func (r *logRing) orderedLocked() []logEntry {
	if len(r.buf) < r.max {
		return append([]logEntry(nil), r.buf...)
	}
	out := make([]logEntry, 0, r.max)
	out = append(out, r.buf[r.next:]...)
	out = append(out, r.buf[:r.next]...)
	return out
}

// Tail 返回最近 n 行(时间序;不足 n 行返回全部)。n<=0 取全环。
func (r *logRing) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ordered := r.orderedLocked()
	if n > 0 && len(ordered) > n {
		ordered = ordered[len(ordered)-n:]
	}
	out := make([]string, len(ordered))
	for i, e := range ordered {
		out[i] = e.line
	}
	return out
}

// Since 返回序号大于 after 的日志行、当前最新序号,以及「历史已滚出环、前端应清空重填」
// 标志。after==0 视为首次拉取(取尾部 max 行并令 reset=true);max<=0 表示不限行数。
func (r *logRing) Since(after uint64, max int) (lines []string, next uint64, reset bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ordered := r.orderedLocked()
	next = r.seq
	lines = make([]string, 0, len(ordered))
	if len(ordered) == 0 {
		return lines, next, after == 0
	}
	if after == 0 || after+1 < ordered[0].seq {
		reset = true
	}
	for _, e := range ordered {
		if e.seq > after {
			lines = append(lines, e.line)
		}
	}
	if max > 0 && len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return lines, next, reset
}
