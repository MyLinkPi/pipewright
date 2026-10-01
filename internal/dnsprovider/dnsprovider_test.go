package dnsprovider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// --- 假 vault / RouteCreator / http transport -------------------------------

// stubVault 是注入用的假 vaultReader:按 id 返回 Secret 明文 + 存在性。
type stubVault struct {
	tokens map[string]string
	uncfg  bool // true → 模拟 vault 未配置
}

func (s stubVault) Reveal(id string) (string, error) {
	if s.uncfg {
		return "", vault.ErrVaultUnconfigured
	}
	t, ok := s.tokens[id]
	if !ok {
		return "", vault.ErrNotFound
	}
	return t, nil
}

func (s stubVault) Exists(id string) (bool, error) {
	if s.uncfg {
		return false, vault.ErrVaultUnconfigured
	}
	_, ok := s.tokens[id]
	return ok, nil
}

// stubClient 是注入用的假 DNSClient:记录 Verify/Ensure/Delete 调用,可配置逐 zone 校验错误。
type stubClient struct {
	verifyCalls  []string          // 每次 VerifyZone 的 zone 参数
	ensureZones  []string          // 每次 EnsureARecord 的 zone 参数
	deleteZones  []string          // 每次 DeleteARecord 的 zone 参数
	verifyErrBy  map[string]error  // zone → 校验错误(缺省 nil)
}

func (c *stubClient) EnsureARecord(_ context.Context, zone, _, _ string) error {
	c.ensureZones = append(c.ensureZones, zone)
	return nil
}

func (c *stubClient) DeleteARecord(_ context.Context, zone, _ string) error {
	c.deleteZones = append(c.deleteZones, zone)
	return nil
}

func (c *stubClient) VerifyZone(_ context.Context, zone string) error {
	c.verifyCalls = append(c.verifyCalls, zone)
	if c.verifyErrBy != nil {
		return c.verifyErrBy[zone]
	}
	return nil
}

// roundTripFunc 是 mock http.RoundTripper。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// newTestService 造一个注入了 stub 的 service(不触真网络)。
func newTestService(t *testing.T, vault vaultReader, dial dialFactory) *service {
	t.Helper()
	st := storetest.OpenDB(t)
	svc := &service{
		store: NewStore(st),
		vault: vault,
		dial:  dial,
	}
	return svc
}

// --- store CRUD + zone CRUD -------------------------------------------------

func TestStoreCRUD(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, stubVault{tokens: map[string]string{"cred-1": "tok"}}, prodDialFactory)

	p, err := svc.Create(ctx, CreateInput{
		Type: "cloudflare", Name: "CF", CredentialID: "cred-1",
		BaseDomains: []string{"Example.COM", "Example.ORG", "example.com"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 根区应归一化小写 + 去重,一提供商多根区。
	if len(p.Zones) != 2 || p.Zones[0].BaseDomain != "example.com" || p.Zones[1].BaseDomain != "example.org" {
		t.Fatalf("根区应归一化去重, got %+v", p.Zones)
	}
	for _, z := range p.Zones {
		if z.ProviderID != p.ID {
			t.Fatalf("根区应挂在该提供商下: %+v", z)
		}
	}

	got, err := svc.Get(ctx, p.ID)
	if err != nil || got.Name != "CF" || len(got.Zones) != 2 {
		t.Fatalf("Get(含根区装配): %v / %+v", err, got)
	}
	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 || len(list[0].Zones) != 2 {
		t.Fatalf("List(含根区装配): %v / %+v", err, list)
	}
	if err := svc.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删后 Get 应 ErrNotFound, got %v", err)
	}
	// 删提供商应连带删根区(重复建同根区不撞唯一约束)。
	p2, err := svc.Create(ctx, CreateInput{Type: "cloudflare", Name: "CF2", CredentialID: "cred-1", BaseDomains: []string{"example.com"}})
	if err != nil {
		t.Fatalf("删除后重建同根区应成功: %v", err)
	}
	_ = p2
}

func TestZoneAddRemove(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, stubVault{tokens: map[string]string{"cred-1": "tok"}}, prodDialFactory)
	p, _ := svc.Create(ctx, CreateInput{Type: "cloudflare", Name: "CF", CredentialID: "cred-1", BaseDomains: []string{"example.com"}})

	z, err := svc.AddZone(ctx, p.ID, " Example.NET ")
	if err != nil || z.BaseDomain != "example.net" {
		t.Fatalf("AddZone(归一化): %v / %+v", err, z)
	}
	if _, err := svc.AddZone(ctx, p.ID, "example.com"); !errors.Is(err, ErrInvalidBaseDomain) {
		t.Fatalf("重复根区应 ErrInvalidBaseDomain, got %v", err)
	}
	if _, err := svc.AddZone(ctx, p.ID, "not a domain"); !errors.Is(err, ErrInvalidBaseDomain) {
		t.Fatalf("非法根域应 ErrInvalidBaseDomain, got %v", err)
	}
	if _, err := svc.AddZone(ctx, "no-such", "a.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的提供商应 ErrNotFound, got %v", err)
	}
	if err := svc.RemoveZone(ctx, p.ID, z.ID); err != nil {
		t.Fatalf("RemoveZone: %v", err)
	}
	if err := svc.RemoveZone(ctx, p.ID, z.ID); !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("重复删根区应 ErrZoneNotFound, got %v", err)
	}
	// 删别家提供商的根区 → ErrZoneNotFound(越权拦截)。
	p2, _ := svc.Create(ctx, CreateInput{Type: "cloudflare", Name: "CF2", CredentialID: "cred-1", BaseDomains: []string{"other.com"}})
	if err := svc.RemoveZone(ctx, p2.ID, p.Zones[0].ID); !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("删别家根区应 ErrZoneNotFound, got %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, stubVault{tokens: map[string]string{"cred-1": "tok"}}, prodDialFactory)

	cases := []struct {
		in   CreateInput
		want error
	}{
		{CreateInput{Type: "route53", Name: "x", CredentialID: "cred-1", BaseDomains: []string{"example.com"}}, ErrInvalidType},
		{CreateInput{Type: "cloudflare", Name: "", CredentialID: "cred-1", BaseDomains: []string{"example.com"}}, ErrEmptyName},
		{CreateInput{Type: "cloudflare", Name: "x", CredentialID: "", BaseDomains: []string{"example.com"}}, ErrEmptyCredentialID},
		{CreateInput{Type: "cloudflare", Name: "x", CredentialID: "cred-1"}, ErrNoBaseDomains},
		{CreateInput{Type: "cloudflare", Name: "x", CredentialID: "cred-1", BaseDomains: []string{}}, ErrNoBaseDomains},
		{CreateInput{Type: "cloudflare", Name: "x", CredentialID: "cred-1", BaseDomains: []string{"*.example.com"}}, ErrInvalidBaseDomain},
		{CreateInput{Type: "cloudflare", Name: "x", CredentialID: "cred-1", BaseDomains: []string{"ok.com", "not a domain"}}, ErrInvalidBaseDomain},
		{CreateInput{Type: "cloudflare", Name: "x", CredentialID: "no-such-cred", BaseDomains: []string{"example.com"}}, ErrCredentialNotFound},
		// API ID 与类型须匹配:dnspod/alidns 必填、cloudflare 须空。
		{CreateInput{Type: "dnspod", Name: "x", CredentialID: "cred-1", BaseDomains: []string{"example.com"}}, ErrInvalidAPIID},
		{CreateInput{Type: "alidns", Name: "x", CredentialID: "cred-1", BaseDomains: []string{"example.com"}}, ErrInvalidAPIID},
		{CreateInput{Type: "cloudflare", Name: "x", APIID: "should-be-empty", CredentialID: "cred-1", BaseDomains: []string{"example.com"}}, ErrInvalidAPIID},
	}
	for i, c := range cases {
		_, err := svc.Create(ctx, c.in)
		if !errors.Is(err, c.want) {
			t.Fatalf("case %d: want %v, got %v", i, c.want, err)
		}
	}
}

func TestUpdate(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, stubVault{tokens: map[string]string{"cred-1": "tok"}}, prodDialFactory)
	p, _ := svc.Create(ctx, CreateInput{Type: "dnspod", Name: "DP", APIID: "12345", CredentialID: "cred-1", BaseDomains: []string{"example.com"}})

	// 改名 + 改 API ID。
	name := "DP2"
	apiID := "67890"
	got, err := svc.Update(ctx, p.ID, UpdateInput{Name: &name, APIID: &apiID})
	if err != nil || got.Name != "DP2" || got.APIID != "67890" {
		t.Fatalf("Update: %v / %+v", err, got)
	}
	if len(got.Zones) != 1 {
		t.Fatalf("Update 应装配根区: %+v", got.Zones)
	}
	// dnspod 清空 API ID → ErrInvalidAPIID。
	empty := ""
	if _, err := svc.Update(ctx, p.ID, UpdateInput{APIID: &empty}); !errors.Is(err, ErrInvalidAPIID) {
		t.Fatalf("清空 dnspod API ID 应 ErrInvalidAPIID, got %v", err)
	}
	// cloudflare 填 API ID → ErrInvalidAPIID。
	cf, _ := svc.Create(ctx, CreateInput{Type: "cloudflare", Name: "CF", CredentialID: "cred-1", BaseDomains: []string{"a.com"}})
	if _, err := svc.Update(ctx, cf.ID, UpdateInput{APIID: &apiID}); !errors.Is(err, ErrInvalidAPIID) {
		t.Fatalf("cloudflare 填 API ID 应 ErrInvalidAPIID, got %v", err)
	}
	// 空名 → ErrEmptyName。
	if _, err := svc.Update(ctx, p.ID, UpdateInput{Name: &empty}); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("空名应 ErrEmptyName, got %v", err)
	}
	if _, err := svc.Update(ctx, "no-such", UpdateInput{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在应 ErrNotFound, got %v", err)
	}
}

// --- Verify(多根区逐个校验)--------------------------------------------------

func TestVerifyMultipleZones(t *testing.T) {
	ctx := context.Background()
	fc := &stubClient{}
	dial := func(providerType, apiID, secret string) DNSClient { return fc }
	svc := newTestService(t, stubVault{tokens: map[string]string{"cred-1": "tok"}}, dial)
	p, _ := svc.Create(ctx, CreateInput{Type: "cloudflare", Name: "CF", CredentialID: "cred-1", BaseDomains: []string{"a.com", "b.com"}})

	results, err := svc.Verify(ctx, p.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(results) != 2 || len(fc.verifyCalls) != 2 {
		t.Fatalf("应逐根区校验, got %+v / %v", results, fc.verifyCalls)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("全部应通过: %+v", r)
		}
	}

	// 一个 zone 失败 → 该 zone 结果带错,另一个仍通过。
	fc.verifyErrBy = map[string]error{"a.com": errors.New("boom")}
	results, err = svc.Verify(ctx, p.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("应仍返回全部结果: %+v", results)
	}
	byDomain := map[string]ZoneVerifyResult{}
	for _, r := range results {
		byDomain[r.BaseDomain] = r
	}
	if byDomain["a.com"].Err == nil || byDomain["b.com"].Err != nil {
		t.Fatalf("逐 zone 结果不符: %+v", results)
	}

	// 删光根区 → ErrZoneNotFound。
	for _, z := range p.Zones {
		_ = svc.RemoveZone(ctx, p.ID, z.ID)
	}
	if _, err := svc.Verify(ctx, p.ID); !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("无根区 Verify 应 ErrZoneNotFound, got %v", err)
	}
}

// --- 根区覆盖 / DeleteSubdomainRecord(最长后缀匹配)---------------------------

func TestZoneCoveringAndDeleteRecord(t *testing.T) {
	ctx := context.Background()
	fc := &stubClient{}
	dial := func(providerType, apiID, secret string) DNSClient { return fc }
	svc := newTestService(t, stubVault{tokens: map[string]string{"cred-1": "tok"}}, dial)
	p, _ := svc.Create(ctx, CreateInput{Type: "cloudflare", Name: "CF", CredentialID: "cred-1", BaseDomains: []string{"example.com", "preview.example.com"}})

	// ZoneCovers:等值 / 后缀 / 最长后缀 / 不覆盖。
	cases := []struct {
		fqdn         string
		covered, ok  bool
	}{
		{"example.com", true, true},
		{"a.example.com", true, true},
		{"x.preview.example.com", true, true},
		{"example.org", false, true},
		{"notexample.com", false, true},
	}
	for _, c := range cases {
		covered, ok, err := svc.ZoneCovers(ctx, p.ID, c.fqdn)
		if err != nil || covered != c.covered || ok != c.ok {
			t.Fatalf("ZoneCovers(%q): want (%v,%v) got (%v,%v) err=%v", c.fqdn, c.covered, c.ok, covered, ok, err)
		}
	}
	// 提供商不存在 → ok=false。
	if _, ok, err := svc.ZoneCovers(ctx, "no-such-provider", "example.com"); err != nil || ok {
		t.Fatalf("ZoneCovers(不存在提供商): got ok=%v err=%v", ok, err)
	}

	// DeleteSubdomainRecord 按最长后缀选根区。
	if err := svc.DeleteSubdomainRecord(ctx, p.ID, "pr-1-x.preview.example.com"); err != nil {
		t.Fatalf("DeleteSubdomainRecord: %v", err)
	}
	if len(fc.deleteZones) != 1 || fc.deleteZones[0] != "preview.example.com" {
		t.Fatalf("应选最长后缀根区, got %v", fc.deleteZones)
	}
	if err := svc.DeleteSubdomainRecord(ctx, p.ID, "app-abc.example.com"); err != nil {
		t.Fatalf("DeleteSubdomainRecord: %v", err)
	}
	if len(fc.deleteZones) != 2 || fc.deleteZones[1] != "example.com" {
		t.Fatalf("应选覆盖根区, got %v", fc.deleteZones)
	}
	// 不在任何根区下 → ErrZoneNotFound。
	if err := svc.DeleteSubdomainRecord(ctx, p.ID, "x.example.org"); !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("不覆盖应 ErrZoneNotFound, got %v", err)
	}
}

// --- Cloudflare 客户端(mock transport)-------------------------------------

func TestCloudflareVerifyZoneSuccess(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer cf-token" {
			t.Fatalf("应带 Bearer token")
		}
		if !strings.Contains(r.URL.String(), "/zones") {
			t.Fatalf("VerifyZone 应查 /zones, got %s", r.URL)
		}
		return jsonResp(200, `{"success":true,"result":[{"id":"zone123","name":"example.com"}]}`), nil
	})
	c := newCloudflareClient("cf-token", rt, "")
	if err := c.VerifyZone(context.Background(), "example.com"); err != nil {
		t.Fatalf("VerifyZone: %v", err)
	}
}

func TestCloudflareVerifyZoneNoPermission(t *testing.T) {
	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		// 成功但返回空 zone 列表(token 无该 zone 权限)。
		return jsonResp(200, `{"success":true,"result":[]}`), nil
	})
	c := newCloudflareClient("cf-token", rt, "")
	err := c.VerifyZone(context.Background(), "example.com")
	if !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("无权限应 ErrVerifyFailed, got %v", err)
	}
}

func TestCloudflareAPIError(t *testing.T) {
	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return jsonResp(403, `{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`), nil
	})
	c := newCloudflareClient("bad", rt, "")
	err := c.VerifyZone(context.Background(), "example.com")
	if !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("API 拒绝应 ErrVerifyFailed, got %v", err)
	}
	// token 绝不能出现在错误文本。
	if strings.Contains(err.Error(), "bad") {
		t.Fatalf("错误文本不应含 token: %v", err)
	}
}

func TestCloudflareEnsureARecordCreate(t *testing.T) {
	var posted bool
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/zones") && !strings.Contains(r.URL.Path, "dns_records") && r.Method == http.MethodGet:
			return jsonResp(200, `{"success":true,"result":[{"id":"z1","name":"example.com"}]}`), nil
		case strings.Contains(r.URL.Path, "dns_records") && r.Method == http.MethodGet:
			// 无既有记录 → 应走 POST 新建。
			return jsonResp(200, `{"success":true,"result":[]}`), nil
		case strings.Contains(r.URL.Path, "dns_records") && r.Method == http.MethodPost:
			posted = true
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"content":"203.0.113.5"`) {
				t.Fatalf("POST 应含目标 IP, got %s", body)
			}
			return jsonResp(200, `{"success":true,"result":{"id":"rec1"}}`), nil
		}
		t.Fatalf("未预期请求 %s %s", r.Method, r.URL)
		return nil, nil
	})
	c := newCloudflareClient("cf-token", rt, "")
	if err := c.EnsureARecord(context.Background(), "example.com", "app-abc.example.com", "203.0.113.5"); err != nil {
		t.Fatalf("EnsureARecord: %v", err)
	}
	if !posted {
		t.Fatalf("无既有记录时应 POST 新建")
	}
}

func TestCloudflareEnsureARecordUpdate(t *testing.T) {
	var putID string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/zones") && !strings.Contains(r.URL.Path, "dns_records") && r.Method == http.MethodGet:
			return jsonResp(200, `{"success":true,"result":[{"id":"z1","name":"example.com"}]}`), nil
		case strings.Contains(r.URL.Path, "dns_records") && r.Method == http.MethodGet:
			return jsonResp(200, `{"success":true,"result":[{"id":"recOLD","name":"app.example.com","type":"A","content":"1.1.1.1"}]}`), nil
		case strings.Contains(r.URL.Path, "dns_records/recOLD") && r.Method == http.MethodPut:
			putID = "recOLD"
			return jsonResp(200, `{"success":true,"result":{"id":"recOLD"}}`), nil
		}
		t.Fatalf("未预期请求 %s %s", r.Method, r.URL)
		return nil, nil
	})
	c := newCloudflareClient("cf-token", rt, "")
	if err := c.EnsureARecord(context.Background(), "example.com", "app.example.com", "203.0.113.9"); err != nil {
		t.Fatalf("EnsureARecord: %v", err)
	}
	if putID != "recOLD" {
		t.Fatalf("有既有记录应 PUT 更新该记录, got %q", putID)
	}
}

// --- AllocateSubdomain(按根区分配)------------------------------------------

// dialDNS 造一个用 mock transport 的 Cloudflare client(供 AllocateSubdomain 测试)。
func dialDNS(rt http.RoundTripper) dialFactory {
	return func(providerType, apiID, secret string) DNSClient {
		return newDNSClient(providerType, apiID, secret, rt, "")
	}
}

// createCF 建一个 cloudflare 提供商(便捷封装)。
func createCF(t *testing.T, svc *service, domains ...string) *Provider {
	t.Helper()
	p, err := svc.Create(context.Background(), CreateInput{
		Type: "cloudflare", Name: "CF", CredentialID: "cred-1", BaseDomains: domains,
	})
	if err != nil {
		t.Fatalf("Create provider: %v", err)
	}
	return p
}

// --- AllocateFQDN(最长后缀选根区,预览环境)----------------------------------

func TestAllocateFQDNStrictZoneMatch(t *testing.T) {
	ctx := context.Background()
	fc := &stubClient{}
	dial := func(providerType, apiID, secret string) DNSClient { return fc }
	svc := newTestService(t, stubVault{tokens: map[string]string{"cred-1": "tok"}}, dial)
	p := createCF(t, svc, "example.com", "preview.example.com")

	in := AllocateInput{
		ProviderID: p.ID,
		HostIP:     "203.0.113.5", Subdomain: "pr-12-x.PREVIEW.example.com",
	}
	// 非法 hostIP → ErrAllocate(且不建任何记录)。
	for _, badIP := range []string{"", "not-an-ip", "2001:db8::1", "example.com"} {
		inBad := in
		inBad.HostIP = badIP
		if _, err := svc.AllocateFQDN(ctx, inBad); !errors.Is(err, ErrAllocate) {
			t.Fatalf("非法 hostIP %q 应 ErrAllocate, got %v", badIP, err)
		}
	}
	if len(fc.ensureZones) != 0 {
		t.Fatalf("非法 IP 不应建任何记录")
	}
	ref, err := svc.AllocateFQDN(ctx, in)
	if err != nil {
		t.Fatalf("AllocateFQDN: %v", err)
	}
	if ref.Domain != "pr-12-x.preview.example.com" {
		t.Fatalf("子域名应归一化小写: %q", ref.Domain)
	}
	// A 记录应建在最长后缀根区 preview.example.com(而非 example.com)。
	if len(fc.ensureZones) != 1 || fc.ensureZones[0] != "preview.example.com" {
		t.Fatalf("应选最长后缀根区, got %v", fc.ensureZones)
	}

	// 与全部根区 apex 同名 / 不在任何根区下 → ErrAllocate。注意 "preview.example.com"
	// 虽是 preview 根区 apex,但仍是 example.com 的严格子域 → 合法(建在 example.com 下)。
	in2 := in
	in2.Subdomain = "preview.example.com"
	if ref, err := svc.AllocateFQDN(ctx, in2); err != nil || ref.Domain != "preview.example.com" {
		t.Fatalf("apex 但为另一根区严格子域应成功: %v / %+v", err, ref)
	} else if len(fc.ensureZones) != 2 || fc.ensureZones[1] != "example.com" {
		t.Fatalf("应选 example.com 根区, got %v", fc.ensureZones)
	}
	for _, bad := range []string{"", "example.com", "pr-1.example.org"} {
		in.Subdomain = bad
		if _, err := svc.AllocateFQDN(ctx, in); !errors.Is(err, ErrAllocate) {
			t.Fatalf("子域名 %q 应 ErrAllocate, got %v", bad, err)
		}
	}
}

// --- 安全:Secret 绝不进 DTO/Provider 视图 ----------------------------------

func TestProviderViewHasNoSecret(t *testing.T) {
	ctx := context.Background()
	const secret = "super-secret-cf-token"
	svc := newTestService(t, stubVault{tokens: map[string]string{"cred-1": secret}}, prodDialFactory)
	p := createCF(t, svc, "example.com")
	// 领域视图只持 CredentialID 引用,绝无 Secret 明文。
	if p.CredentialID != "cred-1" {
		t.Fatalf("应持凭据引用")
	}
	// 把整个 Provider(含 API ID/根区)渲染成字符串也不应出现 Secret。
	if strings.Contains(provString(*p), secret) {
		t.Fatalf("Provider 视图泄漏了 Secret")
	}
	list, _ := svc.List(ctx)
	for _, pp := range list {
		if strings.Contains(provString(pp), secret) {
			t.Fatalf("List 视图泄漏了 Secret")
		}
	}
	// API ID 非机密,应明文可见(可回显)。
	dp, _ := svc.Create(ctx, CreateInput{Type: "dnspod", Name: "DP", APIID: "12345", CredentialID: "cred-1", BaseDomains: []string{"a.com"}})
	if dp.APIID != "12345" {
		t.Fatalf("API ID 应可回显, got %q", dp.APIID)
	}
}

func provString(p Provider) string {
	parts := []string{p.ID, p.Type, p.Name, p.APIID, p.CredentialID}
	for _, z := range p.Zones {
		parts = append(parts, z.ID, z.BaseDomain)
	}
	return strings.Join(parts, "|")
}
