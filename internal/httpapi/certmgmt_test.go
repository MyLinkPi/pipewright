package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/certmgmt"
)

// fakeCertResolver 是注入 certmgmt 的假 DNS 凭据解析器(不触 vault/网络)。
type fakeCertResolver struct{}

func (fakeCertResolver) Resolve(context.Context, string) (string, string, string, bool, error) {
	return "cloudflare", "", "cf-secret", true, nil
}
func (fakeCertResolver) ProviderType(_ context.Context, id string) (string, bool, error) {
	if id == "prov-1" {
		return "cloudflare", true, nil
	}
	return "", false, nil
}
func (fakeCertResolver) ProviderZones(_ context.Context, id string) ([]string, bool, error) {
	if id == "prov-1" {
		return []string{"example.com"}, true, nil
	}
	return nil, false, nil
}

// fakeCertGateway 是注入 certmgmt 的假网关信息(主机 srv-1)。
type fakeCertGateway struct{}

func (fakeCertGateway) Gateway(context.Context) (string, bool, error) {
	return "srv-1", true, nil
}

// fakeCertSink 记录证书同步调用(不触网关);基域清单固定含 efg.com。
type fakeCertSink struct {
	uploads []string // domainID 列表
	cleared []string
}

func (f *fakeCertSink) ListBaseDomains(context.Context) ([]certmgmt.BaseDomain, error) {
	return []certmgmt.BaseDomain{{ID: "dom-1", BaseDomain: "efg.com"}}, nil
}
func (f *fakeCertSink) UploadCert(_ context.Context, domainID, _, _ string) error {
	f.uploads = append(f.uploads, domainID)
	return nil
}
func (f *fakeCertSink) CurrentCertPEM(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeCertSink) ClearCert(_ context.Context, domainID string) error {
	f.cleared = append(f.cleared, domainID)
	return nil
}

// setupCertMgmtServer 构造挂了证书管理的测试 server(admin/testpass;fake target/vault/resolver/sink)。
// 返回 certmgmt.Service 供单测晚绑 GatewayInfo,以及 fake sink 供断言下发。
func setupCertMgmtServer(t *testing.T) (*httptest.Server, certmgmt.Service, *fakeCertSink) {
	t.Helper()
	st := testStoreAuth(t)
	cm := certmgmt.New(st.DB, &fakeSRTarget{}, fakeSRSealer{})
	if cfg, ok := cm.(interface{ SetCredentialsResolver(certmgmt.CredentialsResolver) }); ok {
		cfg.SetCredentialsResolver(fakeCertResolver{})
	}
	sink := &fakeCertSink{}
	if cfg, ok := cm.(interface{ SetCertSink(certmgmt.CertSink) }); ok {
		cfg.SetCertSink(sink)
	}
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := httptest.NewServer(New(testWebFSAuth(), authSvc, WithCertMgmt(cm)))
	t.Cleanup(srv.Close)
	return srv, cm, sink
}

// certErrCode 解码契约错误体 {"error":{"code":...}} 并返回错误码。
func certErrCode(t *testing.T, resp *http.Response) string {
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

// TestCertMgmtAuthAndCSRF:证书管理端点一律要认证;写方法还要 CSRF。
func TestCertMgmtAuthAndCSRF(t *testing.T) {
	srv, _, _ := setupCertMgmtServer(t)
	client, _ := loginSR(t, srv.URL)

	// 匿名 GET → 401。
	anon := &http.Client{}
	resp, err := anon.Get(srv.URL + "/api/certmgmt/certs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名列表应 401:%d", resp.StatusCode)
	}

	// 会话但缺 CSRF 的写 → 403。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/certmgmt/certs", "",
		`{"primaryDomain":"a.example.com","dnsProviderId":"prov-1"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应 403:%d", resp.StatusCode)
	}
	resp.Body.Close()

	// 认证 + CSRF 的合法读 → 200。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/certmgmt/certs", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("列表应 200:%d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestCertMgmtCreateValidation:创建入参校验与错误码映射(域名/CA/提供商/根区覆盖)。
func TestCertMgmtCreateValidation(t *testing.T) {
	srv, _, _ := setupCertMgmtServer(t)
	client, csrf := loginSR(t, srv.URL)

	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"非法主域", `{"primaryDomain":"not a domain","dnsProviderId":"prov-1"}`, "invalid_cert_domain"},
		{"缺 DNS 提供商", `{"primaryDomain":"a.example.com"}`, "dns_provider_required"},
		{"提供商不存在", `{"primaryDomain":"a.example.com","dnsProviderId":"no-such"}`, "invalid_dns_provider"},
		{"CA 非法", `{"primaryDomain":"a.example.com","dnsProviderId":"prov-1","ca":"bogus"}`, "invalid_cert_input"},
		{"域名不在根区下", `{"primaryDomain":"a.other.com","dnsProviderId":"prov-1"}`, "zone_uncovered"},
	}
	for _, tc := range cases {
		resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/certmgmt/certs", csrf, tc.body)
		code := certErrCode(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || code != tc.wantCode {
			t.Fatalf("%s:应 400/%s, got %d/%s", tc.name, tc.wantCode, resp.StatusCode, code)
		}
	}

	// 合法创建 → 201 + pending(异步签发;fake target 不具备真实 acme.sh,后台失败不影响本断言)。
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/certmgmt/certs", csrf,
		`{"primaryDomain":"a.example.com","dnsProviderId":"prov-1"}`)
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.Status != "pending" || created.ID == "" {
		t.Fatalf("创建应 201/pending:%d %+v", resp.StatusCode, created)
	}

	// 同主域重复创建 → 409 cert_domain_taken。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/certmgmt/certs", csrf,
		`{"primaryDomain":"a.example.com","dnsProviderId":"prov-1"}`)
	dupCode := certErrCode(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || dupCode != "cert_domain_taken" {
		t.Fatalf("重复主域应 409/cert_domain_taken:%d/%s", resp.StatusCode, dupCode)
	}
}

// TestCertMgmtImportFlow:导入自签证书 → 列表可见 → 重新下发 → 删除;全程响应不含 PEM。
func TestCertMgmtImportFlow(t *testing.T) {
	srv, cm, sink := setupCertMgmtServer(t)
	// 晚绑网关信息:manual 重新下发(renew)需要网关已接入且已配主机。
	cm.(interface{ SetGateway(certmgmt.GatewayInfo) }).SetGateway(fakeCertGateway{})
	client, csrf := loginSR(t, srv.URL)

	certPEM, keyPEM := srSelfSignedCert(t, "*.efg.com")
	payload, _ := json.Marshal(map[string]string{"certPem": certPEM, "keyPem": keyPEM})

	// 导入 → 201 issued,主域 = 首个 SAN。
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/certmgmt/certs/import", csrf, string(payload))
	var imported struct {
		ID            string `json:"id"`
		PrimaryDomain string `json:"primaryDomain"`
		Status        string `json:"status"`
		Source        string `json:"source"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&imported)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || imported.Status != "issued" || imported.Source != "manual" {
		t.Fatalf("导入应 201/issued/manual:%d %+v", resp.StatusCode, imported)
	}
	if imported.PrimaryDomain != "*.efg.com" {
		t.Fatalf("主域应取自 SAN:%q", imported.PrimaryDomain)
	}

	// 列表/详情绝不含 PEM 内容。
	for _, url := range []string{"/api/certmgmt/certs", "/api/certmgmt/certs/" + imported.ID} {
		resp = doJSON(t, client, http.MethodGet, srv.URL+url, "", "")
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s:%d", url, resp.StatusCode)
		}
		if strings.Contains(buf.String(), "BEGIN") {
			t.Fatalf("响应泄漏 PEM:%s", buf.String())
		}
	}

	// 重复导入同一主域 → 409。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/certmgmt/certs/import", csrf, string(payload))
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复导入应 409:%d", resp.StatusCode)
	}

	// 导入即同步一次网关(*.efg.com 覆盖基域 efg.com → dom-1)。
	if len(sink.uploads) != 1 || sink.uploads[0] != "dom-1" {
		t.Fatalf("导入后应已下发 dom-1 一次:%v", sink.uploads)
	}

	// manual 证书续期 = 重新下发(同步)→ 200,且再下发一次。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/certmgmt/certs/"+imported.ID+"/renew", csrf, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manual 重新下发应 200:%d", resp.StatusCode)
	}
	if len(sink.uploads) != 2 {
		t.Fatalf("重新下发后应共下发两次:%v", sink.uploads)
	}

	// 自动续期开关 → 200。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/certmgmt/certs/"+imported.ID+"/auto-renew", csrf, `{"enabled":true}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("自动续期开关应 200:%d", resp.StatusCode)
	}

	// 删除 → 200;再查 → 404 cert_not_found。
	resp = doJSON(t, client, http.MethodDelete, srv.URL+"/api/certmgmt/certs/"+imported.ID, csrf, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除应 200:%d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/certmgmt/certs/"+imported.ID, "", "")
	goneCode := certErrCode(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || goneCode != "cert_not_found" {
		t.Fatalf("删除后应 404/cert_not_found:%d/%s", resp.StatusCode, goneCode)
	}

	// 非法 PEM → 400 invalid_cert。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/certmgmt/certs/import", csrf,
		`{"certPem":"junk","keyPem":"junk"}`)
	badCode := certErrCode(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || badCode != "invalid_cert" {
		t.Fatalf("非法 PEM 应 400/invalid_cert:%d/%s", resp.StatusCode, badCode)
	}
}

// TestCertMgmtEngine:引擎探测/显式部署(未配网关主机 → configured:false / 400)。
func TestCertMgmtEngine(t *testing.T) {
	srv, _, _ := setupCertMgmtServer(t)
	client, csrf := loginSR(t, srv.URL)

	// 未注入 GatewayInfo → configured:false,非错误。
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/certmgmt/engine", "", "")
	var eng struct {
		Configured bool `json:"configured"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&eng)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || eng.Configured {
		t.Fatalf("引擎探测应 200/configured=false:%d %+v", resp.StatusCode, eng)
	}

	// 显式部署:网关未配置 → 400 gateway_not_configured。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/certmgmt/engine/deploy", csrf, "")
	depCode := certErrCode(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || depCode != "gateway_not_configured" {
		t.Fatalf("未配网关部署应 400/gateway_not_configured:%d/%s", resp.StatusCode, depCode)
	}
}

// TestCertMgmtNilService:未注入证书管理服务 → 端点 503。
func TestCertMgmtNilService(t *testing.T) {
	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := httptest.NewServer(New(testWebFSAuth(), authSvc))
	t.Cleanup(srv.Close)
	client, _ := loginSR(t, srv.URL)

	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/certmgmt/certs", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未初始化应 503:%d", resp.StatusCode)
	}
}
