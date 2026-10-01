package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/certmgmt"
	"github.com/huangchengsir/pipewright/internal/platformhttps"
)

// fakePlatformCertSource 是注入 platformhttps 的假证书源(c1=*.efg.com 已签发)。
type fakePlatformCertSource struct{}

func (fakePlatformCertSource) GetCert(_ context.Context, id string) (*platformhttps.CertInfo, error) {
	if id == "c1" {
		return &platformhttps.CertInfo{
			ID: "c1", PrimaryDomain: "*.efg.com", Domains: []string{"*.efg.com", "efg.com"}, Status: "issued",
		}, nil
	}
	return nil, certmgmt.ErrNotFound
}
func (fakePlatformCertSource) OpenCertPEM(context.Context, string) (string, string, error) {
	return "-----CERT-----", "-----KEY-----", nil
}

// setupPlatformHTTPSServer 构造挂了平台 HTTPS 的测试 server(admin/testpass;fake target/证书源)。
func setupPlatformHTTPSServer(t *testing.T) (*httptest.Server, *fakeSRTarget) {
	t.Helper()
	st := testStoreAuth(t)
	ft := &fakeSRTarget{}
	phSvc := platformhttps.New(st.DB, ft, fakePlatformCertSource{}, 8080)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := httptest.NewServer(New(testWebFSAuth(), authSvc, WithPlatformHTTPS(phSvc)))
	t.Cleanup(srv.Close)
	return srv, ft
}

// phErrCode 解码契约错误体 {"error":{"code":...}} 并返回错误码。
func phErrCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解码错误体:%v", err)
	}
	return body.Error.Code
}

func TestPlatformHTTPSAuthAndCSRF(t *testing.T) {
	srv, _ := setupPlatformHTTPSServer(t)
	client, _ := loginSR(t, srv.URL)

	// 匿名 GET → 401。
	resp, err := newTestClient(t).Get(srv.URL + "/api/platform-https/settings")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名读取应 401:%d", resp.StatusCode)
	}

	// 会话但缺 CSRF 的写 → 403。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/platform-https/settings", "",
		`{"domain":"pip.efg.com"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应 403:%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPlatformHTTPSSettingsFlow(t *testing.T) {
	srv, ft := setupPlatformHTTPSServer(t)
	client, csrf := loginSR(t, srv.URL)

	// 默认设置行:未启用。
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/platform-https/settings", csrf, "")
	var st struct {
		Enabled           bool   `json:"enabled"`
		EffectiveUpstream string `json:"effectiveUpstream"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Enabled || st.EffectiveUpstream != "127.0.0.1:8080" {
		t.Fatalf("默认行不符:%+v", st)
	}

	// 未启用时显式 apply → https_settings_incomplete。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/platform-https/apply", csrf, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("禁用态 apply 应 400:%d", resp.StatusCode)
	}
	resp.Body.Close()

	// 非法域名 → 契约错误码。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/platform-https/settings", csrf,
		`{"domain":"bad_domain"}`)
	if resp.StatusCode != http.StatusBadRequest || phErrCode(t, resp) != "invalid_https_domain" {
		t.Fatalf("非法域名应 400 invalid_https_domain:%d", resp.StatusCode)
	}

	// 证书不覆盖 → https_cert_not_cover。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/platform-https/settings", csrf,
		`{"enabled":true,"serverId":"srv-1","domain":"x.other.com","certId":"c1"}`)
	if resp.StatusCode != http.StatusBadRequest || phErrCode(t, resp) != "https_cert_not_cover" {
		t.Fatalf("不覆盖应 400 https_cert_not_cover:%d", resp.StatusCode)
	}

	// 完整配置 + apply=true:fake target 的 nginx -T 无平台标记 → 应用失败,
	// 但保存成功,返回 200 + settings.status=failed + applyError 人话。
	ft.execCalls = nil
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/platform-https/settings", csrf,
		`{"enabled":true,"serverId":"srv-1","domain":"pip.efg.com","certId":"c1","httpRedirect":true,"apply":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存应 200:%d", resp.StatusCode)
	}
	var saved struct {
		Settings struct {
			Enabled bool   `json:"enabled"`
			Domain  string `json:"domain"`
			Status  string `json:"status"`
		} `json:"settings"`
		ApplyError string `json:"applyError"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Settings.Enabled || saved.Settings.Domain != "pip.efg.com" {
		t.Fatalf("设置应已保存:%+v", saved.Settings)
	}
	if saved.Settings.Status != "failed" || saved.ApplyError == "" {
		t.Fatalf("应用失败应回填状态与人话:%+v/%q", saved.Settings, saved.ApplyError)
	}

	// 已启用但 fake 机器 nginx -T 无平台标记 → 显式 apply 返回 502 https_apply_failed。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/platform-https/apply", csrf, "")
	if resp.StatusCode != http.StatusBadGateway || phErrCode(t, resp) != "https_apply_failed" {
		t.Fatalf("应用失败应 502 https_apply_failed:%d", resp.StatusCode)
	}
	resp.Body.Close()

	// 已应用过(status=failed 非空)时变更域名 → 409 https_applied_change(防旧配置孤儿)。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/platform-https/settings", csrf,
		`{"domain":"new.efg.com"}`)
	if resp.StatusCode != http.StatusConflict || phErrCode(t, resp) != "https_applied_change" {
		t.Fatalf("已应用时换域名应 409 https_applied_change:%d", resp.StatusCode)
	}
	resp.Body.Close()

	// 禁用:远端无平台配置(fake:首次应用失败已回滚)→ no-op 成功。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/platform-https/disable", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("禁用应 200:%d", resp.StatusCode)
	}
	var after struct {
		Enabled bool   `json:"enabled"`
		Status  string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if after.Enabled || after.Status != "" {
		t.Fatalf("禁用后应复位:%+v", after)
	}
}

func TestPlatformHTTPSDetect(t *testing.T) {
	srv, _ := setupPlatformHTTPSServer(t)
	client, csrf := loginSR(t, srv.URL)

	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/platform-https/detect?serverId=srv-1", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("探测应 200:%d", resp.StatusCode)
	}
	var d struct {
		Installed bool `json:"installed"`
		SudoOk    bool `json:"sudoOk"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if !d.Installed || !d.SudoOk {
		t.Fatalf("fake 机器应 installed+sudoOk:%+v", d)
	}

	// 空参 → server_not_found。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/platform-https/detect", csrf, "")
	if resp.StatusCode != http.StatusUnprocessableEntity || phErrCode(t, resp) != "server_not_found" {
		t.Fatalf("空参应 422 server_not_found:%d", resp.StatusCode)
	}
}
