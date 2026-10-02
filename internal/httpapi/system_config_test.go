package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/systemcfg"
)

func setupSystemConfigServer(t *testing.T) *httptest.Server {
	t.Helper()
	st := testStoreAuth(t)
	svc := systemcfg.New(st.DB)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := httptest.NewServer(New(testWebFSAuth(), authSvc, WithSystemConfig(svc)))
	t.Cleanup(srv.Close)
	return srv
}

func TestSystemConfigFlow(t *testing.T) {
	srv := setupSystemConfigServer(t)

	// 匿名 → 401。
	resp, err := newTestClient(t).Get(srv.URL + "/api/system/config")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名读取应 401:%d", resp.StatusCode)
	}

	client, csrf := loginSR(t, srv.URL)

	// 缺 CSRF 的写 → 403。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/system/config", "",
		`{"publicUrl":"https://ci.example.com"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应 403:%d", resp.StatusCode)
	}
	resp.Body.Close()

	// 默认:空(未配置)。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/system/config", csrf, "")
	var cfg struct {
		PublicURL string `json:"publicUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "" {
		t.Fatalf("默认应为空,得 %q", cfg.PublicURL)
	}

	// 非法 URL → 400 invalid_public_url。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/system/config", csrf,
		`{"publicUrl":"ci.example.com/prefix"}`)
	if resp.StatusCode != http.StatusBadRequest || phErrCode(t, resp) != "invalid_public_url" {
		t.Fatalf("非法 URL 应 400 invalid_public_url:%d", resp.StatusCode)
	}

	// 合法(带尾斜杠,服务端归一)→ 200;GET 读回归一值。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/system/config", csrf,
		`{"publicUrl":"https://ci.example.com/"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存应 200:%d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://ci.example.com" {
		t.Fatalf("应归一化去尾斜杠,得 %q", cfg.PublicURL)
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/system/config", csrf, "")
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://ci.example.com" {
		t.Fatalf("读回不符:%q", cfg.PublicURL)
	}

	// 清除(空串)。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/system/config", csrf, `{"publicUrl":""}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("清除应 200:%d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "" {
		t.Fatalf("清除后应为空,得 %q", cfg.PublicURL)
	}
}

// releaseMirror:单独保存不影响 publicUrl;非法值 400;缺省字段 = 不动该项。
func TestSystemConfigReleaseMirror(t *testing.T) {
	srv := setupSystemConfigServer(t)
	client, csrf := loginSR(t, srv.URL)

	var cfg struct {
		PublicURL     string `json:"publicUrl"`
		ReleaseMirror string `json:"releaseMirror"`
	}

	// 非法镜像 → 400 invalid_release_mirror。
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/system/config", csrf,
		`{"releaseMirror":"mirror.example.com"}`)
	if resp.StatusCode != http.StatusBadRequest || phErrCode(t, resp) != "invalid_release_mirror" {
		t.Fatalf("非法镜像应 400 invalid_release_mirror:%d", resp.StatusCode)
	}
	resp.Body.Close()

	// 合法(带子路径 + 尾斜杠,服务端归一)→ 200。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/system/config", csrf,
		`{"releaseMirror":"https://mirror.example.com/gh/"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存应 200:%d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cfg.ReleaseMirror != "https://mirror.example.com/gh" {
		t.Fatalf("应归一化去尾斜杠,得 %q", cfg.ReleaseMirror)
	}

	// 只发 publicUrl:releaseMirror 不动(缺省 = 不更新,向后兼容旧客户端)。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/system/config", csrf,
		`{"publicUrl":"https://ci.example.com"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存应 200:%d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cfg.PublicURL != "https://ci.example.com" || cfg.ReleaseMirror != "https://mirror.example.com/gh" {
		t.Fatalf("缺省字段不应被清空,得 %+v", cfg)
	}

	// 清除镜像(空串 = 清除),publicUrl 仍在。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/system/config", csrf,
		`{"releaseMirror":""}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("清除应 200:%d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cfg.ReleaseMirror != "" || cfg.PublicURL != "https://ci.example.com" {
		t.Fatalf("应只清除镜像,得 %+v", cfg)
	}
}
