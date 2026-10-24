package httpapi

// 安全中心 HTTP 契约测试:真 secscan.Service(sqlite)+ fake Runner/artifacts,
// 挂 mountSecurity 直打端点 —— 不触真 SSH。覆盖:发起扫描(202 + running)、
// 定时规则非法 cron → 400、报告 HTML 含服务器名与标题、扫描器登记 CRUD、明细过滤。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/secscan"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// secRunnerFake:同领域测试的替身(此处复刻最小版,避免跨包导出)。
type secRunnerFake struct {
	servers []secscan.ServerInfo
	execs   int
	fail    bool
}

func (r *secRunnerFake) Servers(context.Context) ([]secscan.ServerInfo, error) { return r.servers, nil }
func (r *secRunnerFake) Server(ctx context.Context, id string) (secscan.ServerInfo, error) {
	for _, s := range r.servers {
		if s.ID == id {
			return s, nil
		}
	}
	return secscan.ServerInfo{}, secscan.ErrServerNotFound
}
func (r *secRunnerFake) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	r.execs++
	if r.fail {
		return &target.ExecResult{ExitCode: 127, Stderr: "docker: not found"}, nil
	}
	joined := strings.Join(cmd, " ")
	switch {
	case strings.Contains(joined, " rootfs "):
		return &target.ExecResult{ExitCode: 0, Stdout: `{"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-1","PkgName":"openssl","Severity":"HIGH"}]}]}`}, nil
	case strings.Contains(joined, "nuclei"):
		return &target.ExecResult{ExitCode: 0, Stdout: `{"template-id":"t1","info":{"name":"N","severity":"low"},"host":"http://10.0.0.1","matched-at":"http://10.0.0.1"}`}, nil
	default:
		return &target.ExecResult{ExitCode: 0, Stdout: ""}, nil
	}
}

// secVaultFake:可逆假密封。
type secVaultFake struct{}

func (secVaultFake) SealSecret(p []byte) ([]byte, error) {
	out := make([]byte, 0, len(p)+2)
	out = append(out, 's', ':')
	return append(out, p...), nil
}
func (secVaultFake) OpenSecret(s []byte) ([]byte, error) {
	if len(s) > 2 && s[0] == 's' && s[1] == ':' {
		return s[2:], nil
	}
	return nil, io.ErrUnexpectedEOF
}

// secArtFake:内存 artifact store。
type secArtFake struct{ m map[string][]byte }

func (a *secArtFake) Put(r io.Reader) (string, int64, error) {
	b, _ := io.ReadAll(r)
	k := "k" + time.Now().Format("150405.000000000")
	a.m[k] = b
	return k, int64(len(b)), nil
}
func (a *secArtFake) Open(k string) (io.ReadCloser, error) {
	if b, ok := a.m[k]; ok {
		return io.NopCloser(strings.NewReader(string(b))), nil
	}
	return nil, io.ErrUnexpectedEOF
}

func newSecurityTestRouter(t *testing.T, st *store.Store, runner secscan.Runner) (chi.Router, secscan.Service) {
	t.Helper()
	svc := secscan.New(st.DB,
		secscan.WithVault(secVaultFake{}),
		secscan.WithRunner(runner),
		secscan.WithArtifacts(&secArtFake{m: map[string][]byte{}}),
	)
	r := chi.NewRouter()
	mountSecurity(r, svc, nil)
	return r, svc
}

// waitStatus 轮询 GET /api/security/scans?serverId= 直到出现非 running(或超时)。
func waitScanDone(t *testing.T, r chi.Router) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/security/scans?limit=10", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code == 200 && !strings.Contains(rec.Body.String(), `"running"`) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("扫描未在期限内完成")
}

func TestSecurityScanEndpoints(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		runner := &secRunnerFake{servers: []secscan.ServerInfo{{ID: "s1", Name: "机器A", Host: "10.0.0.1"}}}
		r, _ := newSecurityTestRouter(t, st, runner)

		// 发起 trivy 扫描 → 202 + running run。
		req := httptest.NewRequest(http.MethodPost, "/security/scans",
			strings.NewReader(`{"serverId":"s1","scanType":"trivy"}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"status":"running"`) {
			t.Fatalf("发起扫描不符:%d %s", rec.Code, rec.Body.String())
		}
		waitScanDone(t, r)

		// 概览:该机 trivy done,合计高危 1。
		req = httptest.NewRequest(http.MethodGet, "/security/overview", nil)
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"high":1`) || !strings.Contains(rec.Body.String(), "机器A") {
			t.Fatalf("概览不符:%d %s", rec.Code, rec.Body.String())
		}

		// 明细:severity 过滤。
		req = httptest.NewRequest(http.MethodGet, "/security/scans?serverId=s1", nil)
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), `"findingsUrl"`) {
			t.Fatalf("历史行应带 findingsUrl:%s", rec.Body.String())
		}
		runID := extractJSONStringField(t, rec.Body.String(), "id")
		req = httptest.NewRequest(http.MethodGet, "/security/scans/"+runID+"/findings?severity=high", nil)
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "CVE-1") {
			t.Fatalf("明细不符:%d %s", rec.Code, rec.Body.String())
		}

		// 非法入参:scanType 枚举 / PVE 目标 / nuclei 非法 URL。
		for _, body := range []string{
			`{"serverId":"s1","scanType":"bogus"}`,
			`{"serverId":"s1","scanType":"nuclei","targets":["ftp://x"]}`,
		} {
			req = httptest.NewRequest(http.MethodPost, "/security/scans", strings.NewReader(body))
			rec = httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("非法入参应 400:%d %s", rec.Code, rec.Body.String())
			}
		}
	})
}

func TestSecurityScheduleAndScanners(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		r, _ := newSecurityTestRouter(t, st, &secRunnerFake{})

		// 默认规则。
		req := httptest.NewRequest(http.MethodGet, "/security/schedule", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":false`) {
			t.Fatalf("默认规则不符:%d %s", rec.Code, rec.Body.String())
		}
		// 非法 cron → 400 invalid_security_input。
		req = httptest.NewRequest(http.MethodPut, "/security/schedule",
			strings.NewReader(`{"enabled":true,"expression":"bad","scanTypes":["trivy"]}`))
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_security_input") {
			t.Fatalf("非法 cron 应 400:%d %s", rec.Code, rec.Body.String())
		}
		// 合法保存。
		req = httptest.NewRequest(http.MethodPut, "/security/schedule",
			strings.NewReader(`{"enabled":true,"expression":"0 5 * * *","scanTypes":["trivy","nuclei"]}`))
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"expression":"0 5 * * *"`) {
			t.Fatalf("保存规则不符:%d %s", rec.Code, rec.Body.String())
		}

		// 扫描器登记 CRUD(响应无 token)。
		req = httptest.NewRequest(http.MethodPost, "/security/scanners",
			strings.NewReader(`{"name":"外部 Trivy","host":"10.0.0.8","port":4954,"token":"t0k"}`))
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"tokenConfigured":true`) ||
			strings.Contains(rec.Body.String(), "t0k") {
			t.Fatalf("登记响应不符:%d %s", rec.Code, rec.Body.String())
		}
		req = httptest.NewRequest(http.MethodGet, "/security/scanners", nil)
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), "10.0.0.8") {
			t.Fatalf("列表不符:%s", rec.Body.String())
		}
		id := extractJSONStringField(t, rec.Body.String(), "id")
		req = httptest.NewRequest(http.MethodDelete, "/security/scanners/"+id, nil)
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("删除不符:%d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestSecurityReportHTML(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		runner := &secRunnerFake{servers: []secscan.ServerInfo{{ID: "s1", Name: "报表机", Host: "10.0.0.1"}}}
		r, _ := newSecurityTestRouter(t, st, runner)

		req := httptest.NewRequest(http.MethodPost, "/security/scans",
			strings.NewReader(`{"serverId":"s1","scanType":"trivy"}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("发起失败:%d %s", rec.Code, rec.Body.String())
		}
		waitScanDone(t, r)

		req = httptest.NewRequest(http.MethodGet, "/security/report", nil)
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") ||
			!strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
			t.Fatalf("报告头不符:%d %v", rec.Code, rec.Header())
		}
		for _, want := range []string{"Pipewright 安全审计报告", "报表机", "CVE-1", "openssl", "Trivy"} {
			if !strings.Contains(body, want) {
				t.Fatalf("报告缺内容 %q", want)
			}
		}
	})
}

// extractJSONStringField 从 JSON 数组响应里取第一个 "id" 字段值(够用即止,不引解析器)。
func extractJSONStringField(t *testing.T, body, field string) string {
	t.Helper()
	needle := `"` + field + `":"`
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatalf("响应缺字段 %s:%s", field, body)
	}
	rest := body[i+len(needle):]
	if j := strings.Index(rest, `"`); j >= 0 {
		return rest[:j]
	}
	t.Fatal("字段值未闭合")
	return ""
}
