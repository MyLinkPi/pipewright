// logring.go 是进程内日志环:并发安全的定长行缓冲,既是 log 输出目标(实现 io.Writer),
// 也供 IPC /api/logs 截取尾部(托盘 GUI「日志」视图)。旧日志文件仍照常落盘,环仅服务
// GUI 实时查看。
package pwclient

import (
	"strings"
	"sync"
)

// logRing 是定长字符串行环。零值不可用,经 newLogRing 构造;线程安全。
type logRing struct {
	mu   sync.Mutex
	buf  []string
	next int
	max  int
}

func newLogRing(maxLines int) *logRing {
	if maxLines <= 0 {
		maxLines = 100
	}
	return &logRing{buf: make([]string, 0, maxLines), max: maxLines}
}

// Write 实现 io.Writer:按行入环(log 标准库的输出天然以 \n 结尾)。
func (r *logRing) Write(p []byte) (int, error) {
	n := len(p)
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		r.mu.Lock()
		if len(r.buf) < r.max {
			r.buf = append(r.buf, line)
		} else {
			r.buf[r.next] = line
			r.next = (r.next + 1) % r.max
		}
		r.mu.Unlock()
	}
	return n, nil
}

// Tail 返回最近 n 行(时间序;不足 n 行返回全部)。n<=0 取全环。
func (r *logRing) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ordered []string
	if len(r.buf) < r.max {
		ordered = append(ordered, r.buf...)
	} else {
		ordered = append(ordered, r.buf[r.next:]...)
		ordered = append(ordered, r.buf[:r.next]...)
	}
	if n > 0 && len(ordered) > n {
		ordered = ordered[len(ordered)-n:]
	}
	out := make([]string, len(ordered))
	copy(out, ordered)
	return out
}
