package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/mfa"
)

// setupMFAServer 构造已装配 mfa 服务的测试 server(管理员已开启 TOTP),
// 返回 (server, 当前有效动态码生成函数)。
func setupMFAServer(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	mfaSvc := mfa.NewService(st.DB, nil, nil, nil)
	svc.SetMFAChecker(mfaSvc)

	setup, err := mfaSvc.SetupTOTP(auth.RoleAdmin, "", "admin")
	if err != nil {
		t.Fatalf("SetupTOTP: %v", err)
	}
	code, err := totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	if err := mfaSvc.EnableTOTP(auth.RoleAdmin, "", code); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}

	srv := httptest.NewServer(New(testWebFSAuth(), svc, WithMFA(mfaSvc)))
	t.Cleanup(srv.Close)
	return srv, func() string {
		c, err := totp.GenerateCode(setup.Secret, time.Now())
		if err != nil {
			t.Fatalf("generate code: %v", err)
		}
		return c
	}
}

// postJSONCode 向 url POST {code},带 CSRF header。
func postJSONCode(client *http.Client, url, code, csrf string) (*http.Response, error) {
	body := strings.NewReader(`{"code":"` + code + `"}`)
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerCsrf, csrf)
	return client.Do(req)
}

// decodeCodeFromCookie 从 client jar 提取 csrf cookie。
func csrfFromJar(t *testing.T, client *http.Client, srvURL string) string {
	t.Helper()
	u := mustParseURL(t, srvURL)
	cookies := client.Jar.Cookies(u)
	for _, c := range cookies {
		if c.Name == cookieCsrf {
			return c.Value
		}
	}
	t.Fatal("csrf cookie missing")
	return ""
}

// TestLoginWithTOTP 全链路:口令过 → mfa_required(pending 会话,受保护 API 拒绝)→
// 错码 401 → 正码升级 → 会话探测通过。Passkey options 探测为 false(未注册密钥)。
func TestLoginWithTOTP(t *testing.T) {
	srv, codeNow := setupMFAServer(t)
	client := newTestClient(t)

	// 第一步:口令登录 → mfa_required,且会话 cookie 已下发。
	resp, err := client.Post(srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"testpass"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode login body: %v", err)
	}
	if payload["mfa_required"] != true {
		t.Fatalf("login body = %v, want mfa_required", payload)
	}

	// pending 会话:session 探测与受保护 API 都应 401 mfa_required。
	for _, path := range []string{"/api/auth/session", "/api/account/sessions"} {
		r, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		decodeErr := json.NewDecoder(r.Body).Decode(&e)
		_ = r.Body.Close()
		if r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s status = %d, want 401", path, r.StatusCode)
		}
		if decodeErr != nil {
			t.Fatalf("GET %s decode: %v", path, decodeErr)
		}
		if e.Error.Code != "mfa_required" {
			t.Fatalf("GET %s code = %q, want mfa_required", path, e.Error.Code)
		}
	}

	csrf := csrfFromJar(t, client, srv.URL)

	// 错码:401 invalid_code。
	r, err := postJSONCode(client, srv.URL+"/api/auth/login/mfa", "000000", csrf)
	if err != nil {
		t.Fatalf("login/mfa(bad): %v", err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login/mfa(bad) status = %d, want 401", r.StatusCode)
	}

	// 缺 CSRF:403。
	r, err = postJSONCode(client, srv.URL+"/api/auth/login/mfa", codeNow(), "")
	if err != nil {
		t.Fatalf("login/mfa(no csrf): %v", err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("login/mfa(no csrf) status = %d, want 403", r.StatusCode)
	}

	// 正码:升级为正式会话,返回身份。
	r, err = postJSONCode(client, srv.URL+"/api/auth/login/mfa", codeNow(), csrf)
	if err != nil {
		t.Fatalf("login/mfa: %v", err)
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("login/mfa status = %d", r.StatusCode)
	}
	var ident map[string]string
	if err := json.NewDecoder(r.Body).Decode(&ident); err != nil {
		t.Fatalf("decode ident: %v", err)
	}
	if ident["username"] != "admin" || ident["role"] != "admin" {
		t.Fatalf("ident = %v", ident)
	}

	// 升级后:session 探测 200。
	r2, err := client.Get(srv.URL + "/api/auth/session")
	if err != nil {
		t.Fatalf("session probe: %v", err)
	}
	defer func() { _ = r2.Body.Close() }()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("session probe status = %d", r2.StatusCode)
	}

	// options 探测:未注册 passkey → false。
	r3, err := client.Get(srv.URL + "/api/auth/options")
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	defer func() { _ = r3.Body.Close() }()
	var opts map[string]bool
	if err := json.NewDecoder(r3.Body).Decode(&opts); err != nil {
		t.Fatalf("decode options: %v", err)
	}
	if opts["passkey_login"] {
		t.Fatal("passkey_login must be false when no passkey registered")
	}
}
