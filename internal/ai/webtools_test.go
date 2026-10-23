package ai

import (
	"encoding/json"
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
		// openai 兼容协议任意地址(含 GLM/OpenRouter/自建网关)均可回退复用;
		// 供应商不支持工具接口时由首次真实调用给出明确错误。
		{"openai-glm", ProviderOpenAI, "https://open.bigmodel.cn/api/paas/v4", true, true, true, "https://open.bigmodel.cn/api/paas/v4"},
		{"openai-gateway", ProviderOpenAI, "https://gw.internal:8443/v1", true, true, true, "https://gw.internal:8443/v1"},
		// claude 协议(非 DeepSeek /anthropic 形态)不可复用 LLM 地址:回退官方默认,
		// 且无独立密钥时不可用(不把 anthropic 密钥发往 DeepSeek)。
		{"claude-other", ProviderClaude, "https://api.anthropic.com/v1", true, false, false, ""},
		// LLM 停用但已配置:网页工具有独立开关,地址/密钥仍可复用。
		{"openai-disabled", ProviderOpenAI, "https://api.deepseek.com/v1", false, true, true, "https://api.deepseek.com/v1"},
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
	// LLM 未配置:地址回退供应商官方默认,但无密钥 → 不可用并给出人读原因。
	cfg := &Config{Provider: "", BaseURL: "", Enabled: false, Configured: false, Web: WebConfig{Enabled: true}}
	if dest := webDestination(cfg, nil); dest.ok || dest.reason == "" {
		t.Fatalf("unconfigured LLM should fall back to provider default and need a key: %+v", dest)
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
		{"bad-base", SaveInput{WebProvider: ProviderWebDeepSeek, WebBaseURL: "ftp://x/"}},
		{"bad-base-no-provider", SaveInput{WebBaseURL: "notaurl"}},
	}
	for _, tc := range cases {
		if _, err := svc.Save(ctx(), tc.in); err == nil || !strings.Contains(err.Error(), "web tools config") {
			t.Fatalf("%s: want ErrInvalidWebConfig, got %v", tc.name, err)
		}
	}
	// 供应商类型与地址/密钥独立:仅提供地址或密钥(类型留空)同样合法。
	if _, err := svc.Save(ctx(), SaveInput{WebBaseURL: "https://api.deepseek.com/v1"}); err != nil {
		t.Fatalf("base without explicit provider must be accepted: %v", err)
	}
	if _, err := svc.Save(ctx(), SaveInput{WebAPIKey: &bad}); err != nil {
		t.Fatalf("key without explicit provider must be accepted: %v", err)
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
	// 主 LLM 非 DeepSeek(openai 兼容,如 GLM)→ 回退同样可用(沿用其地址与密钥;
	// 供应商不支持工具接口由首次真实调用报错)。
	if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: "https://open.bigmodel.cn/api/paas/v4", APIKey: &key, Enabled: true, WebEnabled: true}); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ = svc.load(ctx())
	if !cfg.Web.Available {
		t.Fatalf("openai-compatible LLM fallback should be available: %+v", cfg.Web)
	}
	// 主 LLM 为 claude 协议(非 DeepSeek /anthropic 形态)→ 不可用并给出人读原因。
	if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderClaude, BaseURL: "https://api.anthropic.com/v1", APIKey: &key, Enabled: true, WebEnabled: true,
		WebProvider: "", WebBaseURL: ""}); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ = svc.load(ctx())
	if cfg.Web.Available || cfg.Web.Reason == "" {
		t.Fatalf("claude-protocol LLM fallback must be unavailable with reason: %+v", cfg.Web)
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

func TestWebProviderEnum(t *testing.T) {
	for _, p := range []string{"", ProviderWebDeepSeek, ProviderWebTavily, ProviderWebBocha, ProviderWebOllama, ProviderWebGLM} {
		if _, err := normalizeWebProvider(p); err != nil {
			t.Fatalf("provider %q should be accepted: %v", p, err)
		}
	}
	if _, err := normalizeWebProvider("google"); err == nil {
		t.Fatal("unknown provider must be rejected")
	}
	if defaultWebBaseURL(ProviderWebTavily) != "https://api.tavily.com" || defaultWebBaseURL(ProviderWebBocha) != "https://api.bochaai.com/v1" || defaultWebBaseURL(ProviderWebOllama) != "https://ollama.com" {
		t.Fatal("provider default bases")
	}
	for p := range webProviders {
		if defaultWebBaseURL(p) == "" {
			t.Fatalf("provider %q missing default base", p)
		}
	}
}

func TestWebDestinationOtherProviders(t *testing.T) {
	// tavily/bocha:地址留空回退各自官方默认(绝不复用 LLM 的聊天地址);
	// 无独立密钥 → 不可用并提示填写。
	for _, provider := range []string{ProviderWebTavily, ProviderWebBocha, ProviderWebOllama} { // glm 可复用 LLM,单测见下
		cfg := &Config{Provider: ProviderOpenAI, BaseURL: "https://api.deepseek.com/v1", Enabled: true, Configured: true,
			Web: WebConfig{Enabled: true, Provider: provider}}
		dest := webDestination(cfg, []byte("main-sealed"))
		if dest.ok || dest.base != defaultWebBaseURL(provider) || !strings.Contains(dest.reason, "API 密钥") {
			t.Fatalf("%s: %+v", provider, dest)
		}
		// 有独立密钥 → 可用;主密钥不被跨供应商复用。
		cfg.webSealed = []byte("web-sealed")
		dest = webDestination(cfg, []byte("main-sealed"))
		if !dest.ok || dest.useMain {
			t.Fatalf("%s with own key: %+v", provider, dest)
		}
	}
}

func TestTavilyAdapterAgainstStub(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tvly-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/search":
			var body struct {
				Query      string `json:"query"`
				MaxResults int    `json:"max_results"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Query != "agent search" || body.MaxResults != 3 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"query":"agent search","results":[{"title":"Tavily docs","url":"https://docs.tavily.com","content":"chunk about api","score":0.81,"published_date":"2026-05-01"}],"response_time":0.4}`))
		case "/extract":
			var body struct {
				URLs []string `json:"urls"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.URLs) != 1 || body.URLs[0] != "https://docs.tavily.com" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"url":"https://docs.tavily.com","raw_content":"# Intro\napi usage"}],"failed_results":[],"response_time":0.5}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	svcI, _, _ := newService(t, ts.Client())
	svc := svcI.(*service)
	key := "tvly-test"
	if _, err := svc.Save(ctx(), SaveInput{WebEnabled: true, WebProvider: ProviderWebTavily, WebBaseURL: ts.URL, WebAPIKey: &key}); err != nil {
		t.Fatal(err)
	}
	w := NewWebTools(svc, mask.NewMasker()).(*webTools)
	out, err := w.Search(ctx(), "agent search", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"title":"Tavily docs"`) || !strings.Contains(out, `"date":"2026-05-01"`) || !strings.Contains(out, `"rerank_score":0.81`) {
		t.Fatalf("tavily search rows: %s", out)
	}
	page, err := w.Fetch(ctx(), "https://docs.tavily.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "api usage") {
		t.Fatalf("tavily extract output: %q", page)
	}
}

func TestBochaAdapterAgainstStub(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/web-search" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"_type":"SearchResponse","webPages":{"totalEstimatedMatches":10,"value":[{"name":"博查结果","url":"https://example.cn/a","snippet":"片段","summary":"摘要内容","siteName":"示例站","datePublished":"2026-06-01"}]}}`))
	}))
	defer ts.Close()
	svcI, _, _ := newService(t, ts.Client())
	svc := svcI.(*service)
	key := "sk-bocha"
	if _, err := svc.Save(ctx(), SaveInput{WebEnabled: true, WebProvider: ProviderWebBocha, WebBaseURL: ts.URL, WebAPIKey: &key}); err != nil {
		t.Fatal(err)
	}
	w := NewWebTools(svc, mask.NewMasker()).(*webTools)
	out, err := w.Search(ctx(), "测试查询", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"title":"博查结果"`, `"summary":"摘要内容"`, `"site_name":"示例站"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("bocha rows missing %s: %s", want, out)
		}
	}
	// 博查无抓取接口:web_fetch 返回明确错误。
	if _, err = w.Fetch(ctx(), "https://example.cn/a"); err == nil || !strings.Contains(err.Error(), "不提供网页抓取接口") {
		t.Fatalf("bocha fetch must fail with clear reason: %v", err)
	}
}

func TestOllamaAdapterAgainstStub(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ol-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/web_search":
			var body struct {
				Query      string `json:"query"`
				MaxResults int    `json:"max_results"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Query != "ollama web search" || body.MaxResults != 4 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"title":"Ollama docs","url":"https://docs.ollama.com","content":"web search capability snippet"}]}`))
		case "/api/web_fetch":
			_, _ = w.Write([]byte(`{"title":"Ollama page","content":"# Heading\npage body","links":["https://ollama.com"]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	svcI, _, _ := newService(t, ts.Client())
	svc := svcI.(*service)
	key := "ol-key"
	if _, err := svc.Save(ctx(), SaveInput{WebEnabled: true, WebProvider: ProviderWebOllama, WebBaseURL: ts.URL, WebAPIKey: &key}); err != nil {
		t.Fatal(err)
	}
	w := NewWebTools(svc, mask.NewMasker()).(*webTools)
	out, err := w.Search(ctx(), "ollama web search", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"title":"Ollama docs"`) || !strings.Contains(out, `"snippet":"web search capability snippet"`) {
		t.Fatalf("ollama search rows: %s", out)
	}
	page, err := w.Fetch(ctx(), "https://docs.ollama.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(page, "# Ollama page") || !strings.Contains(page, "page body") {
		t.Fatalf("ollama fetch output: %q", page)
	}
}

func TestWebDestinationGLMReusesLLM(t *testing.T) {
	// GLM 工具接口与 GLM LLM 同 base 同 Key:LLM 为 GLM 默认地址时,glm 供应商
	// 地址/密钥留空即复用 LLM 配置。
	cfg := &Config{Provider: ProviderOpenAI, BaseURL: defaultWebBaseURLGLM, Enabled: true, Configured: true,
		Web: WebConfig{Enabled: true, Provider: ProviderWebGLM}}
	dest := webDestination(cfg, []byte("main-sealed"))
	if !dest.ok || !dest.useMain || dest.base != defaultWebBaseURLGLM {
		t.Fatalf("glm should reuse the GLM LLM destination: %+v", dest)
	}
	// LLM 为其他 openai 兼容目的地:留空同样复用 LLM 地址与密钥(同目的地无
	// 新增暴露;供应商不支持该端点时首次真实调用返回明确错误)。
	cfg.BaseURL = "https://api.deepseek.com/v1"
	dest = webDestination(cfg, []byte("main-sealed"))
	if !dest.ok || !dest.useMain || dest.base != "https://api.deepseek.com/v1" {
		t.Fatalf("glm should reuse any openai-compatible LLM destination: %+v", dest)
	}
	// LLM 不可复用(claude 协议)且未填密钥 → 不可用并提示留空复用语义。
	cfg.Provider, cfg.BaseURL = ProviderClaude, "https://api.anthropic.com/v1"
	dest = webDestination(cfg, []byte("main-sealed"))
	if dest.ok || !strings.Contains(dest.reason, "留空") {
		t.Fatalf("glm without key and non-reusable LLM: %+v", dest)
	}
	cfg.Provider, cfg.BaseURL = ProviderOpenAI, "https://api.deepseek.com/v1"
	// 有独立密钥 → 可用且不复用主密钥。
	cfg.webSealed = []byte("web-sealed")
	dest = webDestination(cfg, []byte("main-sealed"))
	if !dest.ok || dest.useMain {
		t.Fatalf("glm with own key: %+v", dest)
	}
}

func TestGLMAdapterAgainstStub(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer glm-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/web_search":
			var body struct {
				SearchQuery  string `json:"search_query"`
				SearchEngine string `json:"search_engine"`
				SearchIntent bool   `json:"search_intent"`
				Count        int    `json:"count"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len([]rune(body.SearchQuery)) > 70 || body.SearchEngine != "search_std" || body.SearchIntent || (body.Count != 5 && body.Count != 3) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body.Count == 5 && body.SearchQuery != "glm web search" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"search_result":[{"title":"智谱开放平台","content":"搜索结果摘要","link":"https://open.bigmodel.cn","media":"bigmodel","refer":"1","publish_date":"2026-06-01"}]}`))
		case "/reader":
			_, _ = w.Write([]byte(`{"reader_result":{"title":"GLM docs","content":"# 标题\n正文内容","url":"https://open.bigmodel.cn"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	svcI, _, _ := newService(t, ts.Client())
	svc := svcI.(*service)
	key := "glm-key"
	if _, err := svc.Save(ctx(), SaveInput{WebEnabled: true, WebProvider: ProviderWebGLM, WebBaseURL: ts.URL, WebAPIKey: &key}); err != nil {
		t.Fatal(err)
	}
	w := NewWebTools(svc, mask.NewMasker()).(*webTools)
	out, err := w.Search(ctx(), "glm web search", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"title":"智谱开放平台"`, `"url":"https://open.bigmodel.cn"`, `"site_name":"bigmodel"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("glm rows missing %s: %s", want, out)
		}
	}
	page, err := w.Fetch(ctx(), "https://open.bigmodel.cn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(page, "# GLM docs") || !strings.Contains(page, "正文内容") {
		t.Fatalf("glm reader output: %q", page)
	}
	// query 超 70 字符按 rune 截断后仍成功(stub 校验长度)。
	long := strings.Repeat("查", 80)
	if _, err = w.Search(ctx(), long, 3); err != nil {
		t.Fatalf("long query should be clipped, got %v", err)
	}
}
