package httpapi

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func captureAccessLog(t *testing.T, handler http.Handler, path string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	// 恢复必须给显式 writer:SetOutput(nil) 会把输出置为 nil,殃及后续测试的全局 log.Printf。
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, path, nil))
	return buf.String(), rec
}

func TestAccessLogLogsAPIRequests(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	})
	out, rec := captureAccessLog(t, accessLog(inner), "/api/projects/p1/pipeline")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	for _, want := range []string{"PUT", "/api/projects/p1/pipeline", "201"} {
		if !strings.Contains(out, want) {
			t.Fatalf("access log %q missing %q", out, want)
		}
	}
}

func TestAccessLogSkipsNonAPI(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	out, _ := captureAccessLog(t, accessLog(inner), "/healthz")
	if strings.Contains(out, "[http]") {
		t.Fatalf("non-/api path should not be logged, got %q", out)
	}
}

func TestAccessLogDefaultsTo500OnUnwrittenPanic(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	// accessLog 自身不 recover(外层 Recoverer 负责);包一层 recover 模拟外层,
	// 只验证 defer 记下的状态码:handler 未写任何响应 → 500。
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = recover() }()
		accessLog(inner).ServeHTTP(w, r)
	})
	out, _ := captureAccessLog(t, wrapped, "/api/x")
	if !strings.Contains(out, "500") {
		t.Fatalf("panic(unwritten) should log 500, got %q", out)
	}
}

func TestStatusWriterForwardsInterfaces(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	var _ http.Flusher = sw                              // SSE 流依赖
	var _ http.Hijacker = sw                             // WS 终端依赖
	var _ interface{ Unwrap() http.ResponseWriter } = sw // http.ResponseController 依赖
	sw.WriteHeader(http.StatusBadRequest)
	if sw.finalStatus() != http.StatusBadRequest {
		t.Fatalf("finalStatus = %d, want 400", sw.finalStatus())
	}
}

func TestFormatDurationKeepsSubMillisecond(t *testing.T) {
	if !strings.HasSuffix(formatDuration(500), "µs") {
		t.Fatalf("sub-ms duration should keep µs resolution, got %q", formatDuration(500))
	}
	if !strings.HasSuffix(formatDuration(3*time.Millisecond), "ms") {
		t.Fatalf("ms-scale duration should round to ms, got %q", formatDuration(3*time.Millisecond))
	}
}
