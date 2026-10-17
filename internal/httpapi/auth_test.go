package httpapi

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

// ---- test helpers --------------------------------------------------------

// testStoreAuth 打开临时 SQLite 数据库(包含迁移)。
func testStoreAuth(t *testing.T) *store.Store {
	return storetest.Open(t)
}

// testWebFSAuth 返回测试用 FS。
func testWebFSAuth() fs.FS {
	return fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<!doctype html><title>pipewright</title>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log('hi')")},
	}
}

// setupAuthServer 构造带真实认证服务的测试 server;引导 admin/testpass。
func setupAuthServer(t *testing.T) (*httptest.Server, *auth.Service) {
	t.Helper()
	return setupAuthServerWithClock(t, nil)
}

func setupAuthServerWithClock(t *testing.T, clock auth.Clock) (*httptest.Server, *auth.Service) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, clock)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := httptest.NewServer(New(testWebFSAuth(), svc))
	t.Cleanup(srv.Close)
	return srv, svc
}

// doLogin 向 srv 发送登录请求。
func doLogin(t *testing.T, srv *httptest.Server, username, password string) *http.Response {
	t.Helper()
	body := strings.NewReader(`{"username":"` + username + `","password":"` + password + `"}`)
	resp, err := http.Post(srv.URL+"/api/auth/login", "application/json", body)
	if err != nil {
		t.Fatalf("POST /api/auth/login: %v", err)
	}
	return resp
}

// newTestClient 构造携带 cookies 的 http.Client(不跟随重定向)。
func newTestClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// loginWithClient 用 client 登录,返回 csrfToken(从 cookie 中提取)。
func loginWithClient(t *testing.T, client *http.Client, srvURL string) string {
	t.Helper()
	body := strings.NewReader(`{"username":"admin","password":"testpass"}`)
	resp, err := client.Post(srvURL+"/api/auth/login", "application/json", body)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	// 从 Set-Cookie 提取 csrf token。
	for _, c := range resp.Cookies() {
		if c.Name == cookieCsrf {
			return c.Value
		}
	}
	t.Fatal("pipewright_csrf cookie not found after login")
	return ""
}

// ---- mockClock --------------------------------------------------------

// mockClock 实现 auth.Clock 接口,允许在测试中推进时间。
type mockClock struct {
	unixSec atomic.Int64
}

func newMockClock() *mockClock {
	mc := &mockClock{}
	mc.unixSec.Store(time.Now().Unix())
	return mc
}

func (mc *mockClock) Now() time.Time {
	return time.Unix(mc.unixSec.Load(), 0).UTC()
}

func (mc *mockClock) Advance(d time.Duration) {
	mc.unixSec.Add(int64(d.Seconds()))
}

// ---- tests --------------------------------------------------------

// TestLoginSuccess 验证正确口令登录返回 200、设置 HttpOnly 会话 cookie 和可读 CSRF cookie。
func TestLoginSuccess(t *testing.T) {
	srv, _ := setupAuthServer(t)

	resp := doLogin(t, srv, "admin", "testpass")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["username"] != "admin" {
		t.Fatalf("body username = %q, want admin", body["username"])
	}

	var sessionCookie, csrfCookie *http.Cookie
	for _, c := range resp.Cookies() {
		switch c.Name {
		case cookieSession:
			sessionCookie = c
		case cookieCsrf:
			csrfCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("pipewright_session cookie not set")
	}
	if !sessionCookie.HttpOnly {
		t.Fatal("pipewright_session cookie should be HttpOnly")
	}
	if csrfCookie == nil {
		t.Fatal("pipewright_csrf cookie not set")
	}
	if csrfCookie.HttpOnly {
		t.Fatal("pipewright_csrf cookie should NOT be HttpOnly (frontend must read it)")
	}
}

// TestLoginWrongPassword 验证错误口令返回 401 + invalid_credentials。
func TestLoginWrongPassword(t *testing.T) {
	srv, _ := setupAuthServer(t)

	resp := doLogin(t, srv, "admin", "wrongpassword")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != "invalid_credentials" {
		t.Fatalf("error.code = %q, want invalid_credentials", body.Error.Code)
	}
}

// TestProtectedEndpoint401WithoutLogin 验证未登录访问受保护端点返回 401。
func TestProtectedEndpoint401WithoutLogin(t *testing.T) {
	srv, _ := setupAuthServer(t)

	resp, err := http.Post(srv.URL+"/api/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/auth/logout: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestCSRFMissingHeader 验证:已登录但缺 X-CSRF-Token header 的 POST → 403。
func TestCSRFMissingHeader(t *testing.T) {
	srv, _ := setupAuthServer(t)

	client := newTestClient(t)
	loginWithClient(t, client, srv.URL)

	// 携带 session cookie 但不带 X-CSRF-Token。
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/auth/logout", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST logout: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF)", resp.StatusCode)
	}
}

// TestCSRFWithCorrectHeader 验证:已登录且带正确 X-CSRF-Token → 登出成功 204。
func TestCSRFWithCorrectHeader(t *testing.T) {
	srv, _ := setupAuthServer(t)

	client := newTestClient(t)
	csrfToken := loginWithClient(t, client, srv.URL)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/auth/logout", nil)
	req.Header.Set("X-CSRF-Token", csrfToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST logout: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

// TestSessionEndpointUnauthenticated 验证未登录访问 GET /api/auth/session → 401。
func TestSessionEndpointUnauthenticated(t *testing.T) {
	srv, _ := setupAuthServer(t)

	resp, err := http.Get(srv.URL + "/api/auth/session")
	if err != nil {
		t.Fatalf("GET /api/auth/session: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestSessionEndpointAuthenticated 验证已登录访问 GET /api/auth/session → 200 + username。
func TestSessionEndpointAuthenticated(t *testing.T) {
	srv, _ := setupAuthServer(t)

	client := newTestClient(t)
	loginWithClient(t, client, srv.URL)

	resp, err := client.Get(srv.URL + "/api/auth/session")
	if err != nil {
		t.Fatalf("GET /api/auth/session: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["username"] != "admin" {
		t.Fatalf("username = %q, want admin", body["username"])
	}
}

// TestLockout 验证连续 5 次失败后第 6 次返回 429。
func TestLockout(t *testing.T) {
	srv, _ := setupAuthServer(t)

	for i := 0; i < 5; i++ {
		resp := doLogin(t, srv, "admin", "wrongpass")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, resp.StatusCode)
		}
	}

	// 第 6 次被锁定(429)。
	resp := doLogin(t, srv, "admin", "testpass")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 after lockout", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "locked_out" {
		t.Fatalf("error.code = %q, want locked_out", body.Error.Code)
	}
}

// TestLockoutResetOnSuccess 验证成功登录后计数重置。
func TestLockoutResetOnSuccess(t *testing.T) {
	srv, _ := setupAuthServer(t)

	// 4 次失败(未触发锁定)。
	for i := 0; i < 4; i++ {
		resp := doLogin(t, srv, "admin", "wrongpass")
		resp.Body.Close()
	}

	// 成功登录:重置计数。
	resp := doLogin(t, srv, "admin", "testpass")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login after 4 failures: status = %d, want 200", resp.StatusCode)
	}

	// 再失败 4 次仍不触发锁定(计数从 0 重新开始)。
	for i := 0; i < 4; i++ {
		resp := doLogin(t, srv, "admin", "wrongpass")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("post-reset attempt %d: status = %d, want 401", i+1, resp.StatusCode)
		}
	}
}

// TestLockoutExpiry 验证锁定到期后可再次尝试(使用可注入时钟)。
func TestLockoutExpiry(t *testing.T) {
	mc := newMockClock()
	srv, _ := setupAuthServerWithClock(t, mc)

	// 5 次失败触发锁定。
	for i := 0; i < 5; i++ {
		body := strings.NewReader(`{"username":"admin","password":"wrongpass"}`)
		resp, _ := http.Post(srv.URL+"/api/auth/login", "application/json", body)
		resp.Body.Close()
	}

	// 锁定中:正确口令也 429。
	body := strings.NewReader(`{"username":"admin","password":"testpass"}`)
	resp, err := http.Post(srv.URL+"/api/auth/login", "application/json", body)
	if err != nil {
		t.Fatalf("login during lockout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("during lockout: status = %d, want 429", resp.StatusCode)
	}

	// 时钟前进 16 分钟(超过 15 分钟锁定期)。
	mc.Advance(16 * time.Minute)

	// 锁定到期后可以正常登录。
	body = strings.NewReader(`{"username":"admin","password":"testpass"}`)
	resp, err = http.Post(srv.URL+"/api/auth/login", "application/json", body)
	if err != nil {
		t.Fatalf("login after lockout expiry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after lockout expiry: status = %d, want 200", resp.StatusCode)
	}
}

// TestCookieSecureBehindTrustedProxy 可信反代(直连对端为回环,即 platformHTTPS 的
// 本机 nginx 形态)且 X-Forwarded-Proto: https 时,会话/CSRF cookie 带 Secure;
// 直连明文则不带。
func TestCookieSecureBehindTrustedProxy(t *testing.T) {
	srv, _ := setupAuthServer(t)

	login := func(forwardedProto string) (session, csrf *http.Cookie) {
		body := strings.NewReader(`{"username":"admin","password":"testpass"}`)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/auth/login", body)
		req.Header.Set("Content-Type", "application/json")
		if forwardedProto != "" {
			req.Header.Set("X-Forwarded-Proto", forwardedProto)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("login status = %d", resp.StatusCode)
		}
		for _, c := range resp.Cookies() {
			switch c.Name {
			case cookieSession:
				session = c
			case cookieCsrf:
				csrf = c
			}
		}
		if session == nil || csrf == nil {
			t.Fatal("session/csrf cookies not set")
		}
		return
	}

	// httptest 服务端 RemoteAddr 恒为 127.0.0.1 → 落入可信反代(回环)分支。
	s1, c1 := login("https")
	if !s1.Secure || !c1.Secure {
		t.Fatal("cookies must carry Secure behind trusted proxy (X-Forwarded-Proto: https)")
	}
	// 无反代头(直连明文):不得置 Secure(否则纯 HTTP 环境下浏览器会拒收 cookie)。
	s2, c2 := login("")
	if s2.Secure || c2.Secure {
		t.Fatal("cookies must NOT be Secure on plain direct connection")
	}
}

// TestVersionEndpointRequiresAuth /api/version 需要登录;原公开 /version 已移除
//(版本元数据会帮助攻击者匹配已知漏洞版本)。
func TestVersionEndpointRequiresAuth(t *testing.T) {
	srv, _ := setupAuthServer(t)

	resp, err := http.Get(srv.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	client := newTestClient(t)
	loginWithClient(t, client, srv.URL)
	resp2, err := client.Get(srv.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version (authed): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", resp2.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp2.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["version"]; !ok {
		t.Fatal("version field missing in response")
	}
}

// TestSecurityHeaders 全局安全响应头:CSP(frame-ancestors 'none' 等)/X-Frame-Options/
// Referrer-Policy/nosniff。
func TestSecurityHeaders(t *testing.T) {
	srv, _ := setupAuthServer(t)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	h := resp.Header
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("Content-Security-Policy missing/too lax: %q", csp)
	}
	if h.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("X-Frame-Options = %q, want DENY", h.Get("X-Frame-Options"))
	}
	if h.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", h.Get("Referrer-Policy"))
	}
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", h.Get("X-Content-Type-Options"))
	}
}
