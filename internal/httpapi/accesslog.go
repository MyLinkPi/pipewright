package httpapi

import (
	"bufio"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// accessLog 给 /api/* 请求打一行访问日志(method path status duration remote)。
//
// 背景:除启动与零散错误外进程此前没有请求级输出,故障时(请求挂起 / 卡在反代层 /
// handler 静默失败)"没有 log" 只能靠 SIGQUIT goroutine dump 事后定位。这行日志是
// 最短证据链:
//   - 请求出现在这里 = 已到达 Go 进程(问题在 handler 或其后);
//   - 没出现 = 根本没到(反代 / 隧道 / 浏览器层)。
//
// 只记 /api/*(静态资源与 /healthz 探活降噪);SSE 流(如 /api/runs/{id}/events)
// 在流结束时记一行,duration = 整段流时长。超过 1s 追加 SLOW 标记,渐进劣化在
// 日志里一眼可见。panic 时 handler 未写状态( Recoverer 在外层稍后写 500),记 500。
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			mark := ""
			if time.Since(start) > time.Second {
				mark = " SLOW"
			}
			log.Printf("[http] %s %s %d %s%s %s",
				r.Method, r.URL.Path, rw.finalStatus(), formatDuration(time.Since(start)), mark, remoteHost(r))
		}()
		next.ServeHTTP(rw, r)
	})
}

// formatDuration:亚毫秒记 µs,其余取整到 ms,避免一排 "0s" 失去分辨力。
func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return d.Round(time.Microsecond).String()
	}
	return d.Round(time.Millisecond).String()
}

func remoteHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// statusWriter 记录 handler 写下的状态码;转发 Flush(SSE)/Hijack(WebSocket 终端)/
// Unwrap(http.ResponseController),与 localeResponseWriter 同款透传。
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// finalStatus:handler 未写任何响应(panic / 直接 return)按 500 记,与 Recoverer 兜底一致。
func (w *statusWriter) finalStatus() int {
	if !w.wrote {
		return http.StatusInternalServerError
	}
	return w.status
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support Hijack")
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
