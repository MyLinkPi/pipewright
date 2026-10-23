package ai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/mask"
)

func TestWebDestinationFollowLLM(t *testing.T) {
	cases := []struct {
		name        string
		provider    string
		base        string
		enabled     bool
		wantOK      bool
		wantUseMain bool
		wantBase    string
	}{
		{"openai-deepseek", ProviderOpenAI, "https://api.deepseek.com/v1", true, true, true, "https://api.deepseek.com/v1"},
		{"openai-deepseek-trailing-slash", ProviderOpenAI, "https://api.deepseek.com/v1/", true, true, true, "https://api.deepseek.com/v1/"},
		{"claude-deepseek-anthropic", ProviderClaude, "https://api.deepseek.com/anthropic", true, true, true, defaultWebBaseURLDeepSeek},
		{"claude-deepseek-anthropic-v1", ProviderClaude, "https://api.deepseek.com/anthropic/v1", true, true, true, defaultWebBaseURLDeepSeek},
		{"openai-other", ProviderOpenAI, "https://open.bigmodel.cn/api/paas/v4", true, false, false, ""},
		{"openai-disabled", ProviderOpenAI, "https://api.deepseek.com/v1", false, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Provider: tc.provider, BaseURL: tc.base, Enabled: tc.enabled, Configured: true,
				Web: WebConfig{Enabled: true}}
			dest := webDestination(cfg, []byte("sealed"))
			if dest.ok != tc.wantOK || dest.useMain != tc.wantUseMain {
				t.Fatalf("ok=%v useMain=%v reason=%q", dest.ok, dest.useMain, dest.reason)
			}
			if dest.ok && strings.TrimRight(dest.base, "/") != strings.TrimRight(tc.wantBase, "/") {
				t.Fatalf("base=%q want %q", dest.base, tc.wantBase)
			}
		})
	}
	// LLM 未配置 → 回退不可用。
	cfg := &Config{Provider: "", BaseURL: "", Enabled: false, Configured: false, Web: WebConfig{Enabled: true}}
	if dest := webDestination(cfg, nil); dest.ok {
		t.Fatalf("unconfigured LLM fallback should be unavailable")
	}
}

func TestWebDestinationExplicitProvider(t *testing.T) {
	// 显式供应商 + 独立密钥:可用,不复用主密钥。
	cfg := &Config{Provider: ProviderOpenAI, BaseURL: "https://api.openai.com/v1", Enabled: true, Configured: true,
		Web: WebConfig{Enabled: true, Provider: ProviderWebDeepSeek, BaseURL: "https://api.deepseek.com/v1"}}
	cfg.webSealed = []byte("web-sealed")
	dest := webDestination(cfg, []byte("main-sealed"))
	if !dest.ok || dest.useMain {
		t.Fatalf("explicit provider with own key: ok=%v useMain=%v", dest.ok, dest.useMain)
	}
	// 无独立密钥但与 LLM 同目的地 → 安全复用主密钥。
	cfg.BaseURL = "https://api.deepseek.com/v1"
	cfg.webSealed = nil
	dest = webDestination(cfg, []byte("main-sealed"))
	if !dest.ok || !dest.useMain {
		t.Fatalf("same destination should reuse main key: ok=%v useMain=%v reason=%q", dest.ok, dest.useMain, dest.reason)
	}
	// 无独立密钥且目的地不同 → 不可用(绝不把主密钥发往其他地址)。
	cfg.Web.BaseURL = "https://proxy.example.com/v1"
	if dest := webDestination(cfg, []byte("main-sealed")); dest.ok || dest.reason == "" {
		t.Fatalf("different destination without own key must be unavailable with reason: %+v", dest)
	}
}

func TestWebConfigSaveValidation(t *testing.T) {
	svc, _, _ := newService(t, http.DefaultClient)
	bad := "bad"
	cases := []struct {
		name string
		in   SaveInput
	}{
		{"bad-provider", SaveInput{WebProvider: "google", WebAPIKey: &bad}},
		{"base-without-provider", SaveInput{WebProvider: "", WebBaseURL: "https://api.deepseek.com/v1"}},
		{"key-without-provider", SaveInput{WebProvider: "", WebAPIKey: &bad}},
		{"bad-base", SaveInput{WebProvider: ProviderWebDeepSeek, WebBaseURL: "ftp://x/"}},
	}
	for _, tc := range cases {
		if _, err := svc.Save(ctx(), tc.in); err == nil || !strings.Contains(err.Error(), "web tools config") {
			t.Fatalf("%s: want ErrInvalidWebConfig, got %v", tc.name, err)
		}
	}
}

func TestWebConfigSaveRoundtrip(t *testing.T) {
	svcI, _, _ := newService(t, http.DefaultClient)
	svc := svcI.(*service)
	key := "sk-web-1"
	if _, err := svc.Save(ctx(), SaveInput{WebEnabled: true, WebProvider: ProviderWebDeepSeek, WebBaseURL: "https://api.deepseek.com/v1", WebAPIKey: &key}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := svc.load(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Web.Enabled || cfg.Web.Provider != ProviderWebDeepSeek || cfg.Web.APIKeyMasked == "" {
		t.Fatalf("web config roundtrip: %+v", cfg.Web)
	}
	if !cfg.Web.Available {
		t.Fatalf("explicit provider with key should be available: %+v", cfg.Web)
	}
	// 目的地不变 + 未提供新密钥 → 保留既有密文。
	if _, err = svc.Save(ctx(), SaveInput{WebEnabled: true, WebProvider: ProviderWebDeepSeek, WebBaseURL: "https://api.deepseek.com/v1"}); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ = svc.load(ctx())
	if len(cfg.webSealed) == 0 || cfg.Web.APIKeyMasked == "" {
		t.Fatalf("web key should be retained on unchanged destination")
	}
	// 目的地变更 → 清空密文(尾部斜杠是同一目的地,密钥保留属预期)。
	if _, err = svc.Save(ctx(), SaveInput{WebEnabled: true, WebProvider: ProviderWebDeepSeek, WebBaseURL: "https://proxy.example.com/v1"}); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ = svc.load(ctx())
	if len(cfg.webSealed) != 0 {
		t.Fatalf("web key must be cleared when destination changes")
	}
	// 主配置改变不影响 web 密钥(PUT 整体替换,web 字段原样携带时独立保留)。
	if _, err = svc.Save(ctx(), SaveInput{WebEnabled: true, WebProvider: ProviderWebDeepSeek, WebBaseURL: "https://api.deepseek.com/v1", WebAPIKey: &key}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: "https://api.openai.com/v1", APIKey: &key,
		WebEnabled: true, WebProvider: ProviderWebDeepSeek, WebBaseURL: "https://api.deepseek.com/v1"}); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ = svc.load(ctx())
	if len(cfg.webSealed) == 0 {
		t.Fatalf("main config change must not clear web key")
	}
}

func TestWebToolsSearchFetchAgainstStub(t *testing.T) {
	var gotAuth, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/v1/tools/search":
			_, _ = w.Write([]byte(`{"results":[{"rerank_score":2.1,"title":"DeepSeek V4","url":"https://example.com/v4","snippet":"s","summary":"sum","content":"dropped","site_name":"Example","date":"2026-06-01T00:00:00+00:00","authority":3},{"rerank_score":0.5,"title":"t2","url":"https://example.com/2","snippet":"s2","summary":"sum2"}]}`))
		case "/v1/tools/fetch":
			_, _ = w.Write([]byte(`{"title":"Docs page","content":"# heading\nbody"}`))
		default:
			w.WriteHeader(http.StatusPaymentRequired)
		}
	}))
	defer ts.Close()
	svcI, _, _ := newService(t, ts.Client())
	svc := svcI.(*service)
	key := "sk-web"
	if _, err := svc.Save(ctx(), SaveInput{WebEnabled: true, WebProvider: ProviderWebDeepSeek, WebBaseURL: ts.URL + "/v1", WebAPIKey: &key}); err != nil {
		t.Fatal(err)
	}
	w := NewWebTools(svc, mask.NewMasker())
	if w == nil {
		t.Fatal("NewWebTools returned nil")
	}
	if !w.Available(ctx()) {
		t.Fatal("web tools should be available")
	}
	out, err := w.Search(ctx(), "deepseek v4 release", 5)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-web" || gotPath != "/v1/tools/search" {
		t.Fatalf("request shape: auth=%q path=%q", gotAuth, gotPath)
	}
	if !strings.Contains(out, `"results"`) || !strings.Contains(out, `"summary":"sum"`) || strings.Contains(out, "dropped") {
		t.Fatalf("search output should drop content: %s", out)
	}
	page, err := w.Fetch(ctx(), "https://example.com/v4")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(page, "# Docs page") || !strings.Contains(page, "body") {
		t.Fatalf("fetch output: %q", page)
	}
}

func TestWebToolsUnavailableWhenDisabled(t *testing.T) {
	svcI, _, _ := newService(t, http.DefaultClient)
	svc := svcI.(*service)
	key := "sk-web"
	if _, err := svc.Save(ctx(), SaveInput{WebEnabled: false, WebProvider: ProviderWebDeepSeek, WebBaseURL: "https://api.deepseek.com/v1", WebAPIKey: &key}); err != nil {
		t.Fatal(err)
	}
	w := NewWebTools(svc, mask.NewMasker())
	if w.Available(ctx()) {
		t.Fatal("disabled toggle must hide web tools")
	}
	if _, err := w.Search(ctx(), "q", 1); err == nil {
		t.Fatal("search must fail when disabled")
	}
	// GET 面:关闭态 Available=false 且 Reason 为空(常态非故障)。
	cfg, _, _ := svc.load(ctx())
	if cfg.Web.Available || cfg.Web.Reason != "" {
		t.Fatalf("disabled web should be quietly unavailable: %+v", cfg.Web)
	}
}

func TestWebFollowLLMAvailability(t *testing.T) {
	svcI, _, _ := newService(t, http.DefaultClient)
	svc := svcI.(*service)
	key := "sk-main"
	// 主 LLM = DeepSeek 官方,web 未单独配置但开关打开 → 回退可用。
	if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: "https://api.deepseek.com/v1", APIKey: &key, Enabled: true, WebEnabled: true}); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := svc.load(ctx())
	if !cfg.Web.Available {
		t.Fatalf("deepseek LLM fallback should be available: %+v", cfg.Web)
	}
	// 主 LLM 非 DeepSeek → 不可用并给出人读原因。
	if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: "https://open.bigmodel.cn/api/paas/v4", APIKey: &key, Enabled: true, WebEnabled: true}); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ = svc.load(ctx())
	if cfg.Web.Available || cfg.Web.Reason == "" {
		t.Fatalf("non-deepseek LLM without explicit config must be unavailable with reason: %+v", cfg.Web)
	}
}

func TestWebTestProbe(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tools/search" {
			_, _ = w.Write([]byte(`{"results":[{"title":"t","url":"https://x","snippet":"s","summary":"m"}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	svcI, _, _ := newService(t, ts.Client())
	svc := svcI.(*service)
	// draft 探测(未保存):provider+baseUrl+apiKey → ok。
	res, err := svc.WebTest(ctx(), WebTestInput{
		Provider: strPtr(ProviderWebDeepSeek), BaseURL: strPtr(ts.URL + "/v1"), APIKey: strPtr("sk-draft"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("draft probe should succeed: %+v", res)
	}
	// draft 未填密钥且无已存 → 解析失败给原因,不发请求。
	res, err = svc.WebTest(ctx(), WebTestInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Error == "" {
		t.Fatalf("empty probe should fail with reason: %+v", res)
	}
}

func strPtr(s string) *string { return &s }

func TestMapWebStatusError(t *testing.T) {
	cases := []struct {
		resource string
		status   int
		contains string
	}{
		{"search", 401, "认证失败"},
		{"search", 402, "余额不足"},
		{"fetch", 403, "安全策略拒绝"},
		{"fetch", 404, "目标网页无法获取"},
		{"search", 404, "接口不存在"},
		{"search", 429, "频率限制"},
		{"fetch", 504, "服务端错误"},
	}
	for _, tc := range cases {
		msg := mapWebStatusError(tc.resource, tc.status).Error()
		if !strings.Contains(msg, tc.contains) {
			t.Fatalf("%s/%d: %q missing %q", tc.resource, tc.status, msg, tc.contains)
		}
	}
}
