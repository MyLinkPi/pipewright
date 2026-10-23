package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// testMasterKey 返回确定性测试用 master key。
func testMasterKey() *[32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 11)
	}
	return &k
}

// testDB 打开临时 SQLite(含全部迁移),返回 *sql.DB 与库文件路径(供整库 dump 验明文)。
func testDB(t *testing.T) (*sql.DB, string) {
	return storetest.OpenDBWithPath(t)
}

// newService 构造带真实 vault + 注入 client 的 Service。
func newService(t *testing.T, client *http.Client) (Service, *sql.DB, string) {
	t.Helper()
	db, path := testDB(t)
	v := vault.New(db, testMasterKey())
	return New(db, v, client), db, path
}

func ctx() context.Context { return context.Background() }

func TestGetLazyEmptyDefault(t *testing.T) {
	svc, _, _ := newService(t, http.DefaultClient)
	cfg, err := svc.Get(ctx())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cfg.Configured || cfg.Enabled {
		t.Fatalf("首次无配置应 configured/enabled=false: %+v", cfg)
	}
	if cfg.Provider != "" || cfg.BaseURL != "" || cfg.APIKeyMasked != "" {
		t.Fatalf("空默认应全空: %+v", cfg)
	}
	if cfg.UpdatedAt != nil {
		t.Fatalf("空默认 updatedAt 应 nil")
	}
	if cfg.Budget.MonthlyTokenLimit != nil {
		t.Fatalf("空默认预算应 nil")
	}
}

func strp(s string) *string { return &s }

func TestSaveClaudeWithKeyMaskedAndConfigured(t *testing.T) {
	svc, _, _ := newService(t, http.DefaultClient)
	cfg, err := svc.Save(ctx(), SaveInput{
		Provider: ProviderClaude,
		Model:    "claude-sonnet-4",
		APIKey:   strp("sk-ant-secret-XYZ7"),
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !cfg.Configured || !cfg.Enabled {
		t.Fatalf("应 configured/enabled=true: %+v", cfg)
	}
	if cfg.BaseURL != defaultBaseURLClaude {
		t.Fatalf("baseUrl 应兜底默认: %q", cfg.BaseURL)
	}
	if !strings.HasPrefix(cfg.APIKeyMasked, "••••") || strings.Contains(cfg.APIKeyMasked, "secret") {
		t.Fatalf("apiKey 应掩码且不含明文: %q", cfg.APIKeyMasked)
	}
	if !strings.HasSuffix(cfg.APIKeyMasked, "XYZ7") {
		t.Fatalf("掩码应保留末 4 位: %q", cfg.APIKeyMasked)
	}
	if cfg.UpdatedAt == nil {
		t.Fatalf("保存后 updatedAt 应非 nil")
	}
}

func TestSaveWithoutKey422(t *testing.T) {
	svc, _, _ := newService(t, http.DefaultClient)
	_, err := svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI})
	if err != ErrAPIKeyRequired {
		t.Fatalf("无 key 的 openai 应 ErrAPIKeyRequired, got %v", err)
	}
}

func TestSaveInvalidProvider(t *testing.T) {
	svc, _, _ := newService(t, http.DefaultClient)
	_, err := svc.Save(ctx(), SaveInput{Provider: "gemini"})
	if err != ErrInvalidProvider {
		t.Fatalf("非法 provider 应 ErrInvalidProvider, got %v", err)
	}
}

func TestSaveKeyOmittedRetainsExisting(t *testing.T) {
	svc, _, _ := newService(t, http.DefaultClient)
	// 首存带 key。
	c1, err := svc.Save(ctx(), SaveInput{Provider: ProviderClaude, APIKey: strp("sk-ant-firstKEY9")})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	mask1 := c1.APIKeyMasked

	// 二次保存省略 key(nil)→ 应保留旧密文、仍 configured。
	c2, err := svc.Save(ctx(), SaveInput{Provider: ProviderClaude, Model: "claude-x", APIKey: nil})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if !c2.Configured {
		t.Fatalf("省略 key 二次保存应仍 configured")
	}
	if c2.APIKeyMasked != mask1 {
		t.Fatalf("省略 key 应保留旧掩码: %q != %q", c2.APIKeyMasked, mask1)
	}

	// 空串也保留。
	c3, err := svc.Save(ctx(), SaveInput{Provider: ProviderClaude, APIKey: strp("")})
	if err != nil {
		t.Fatalf("third save: %v", err)
	}
	if c3.APIKeyMasked != mask1 {
		t.Fatalf("空串 key 应保留旧掩码: %q", c3.APIKeyMasked)
	}

	// 非空轮换 → 掩码变化。
	c4, err := svc.Save(ctx(), SaveInput{Provider: ProviderClaude, APIKey: strp("sk-ant-rotateNEW2")})
	if err != nil {
		t.Fatalf("rotate save: %v", err)
	}
	if c4.APIKeyMasked == mask1 {
		t.Fatalf("轮换后掩码应变化")
	}
	if !strings.HasSuffix(c4.APIKeyMasked, "NEW2") {
		t.Fatalf("轮换掩码应保留新末 4 位: %q", c4.APIKeyMasked)
	}
}

func TestRawDBHasNoPlaintextKey(t *testing.T) {
	svc, _, path := newService(t, http.DefaultClient)
	const secret = "sk-ant-PLAINTEXTMARKER-zzz9"
	if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderClaude, APIKey: strp(secret)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 整库二进制 dump grep 明文 key:绝不出现。
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read db file: %v", err)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("裸 DB 不应含明文 apiKey")
		}
		if strings.Contains(string(raw), "PLAINTEXTMARKER") {
			t.Fatalf("裸 DB 不应含明文 key 片段")
		}
	}
}

// 存量迁移:旧默认裸根地址(曾依赖自动补 /v1、/api)读取时一次性升级为完整前缀;
// 自定义网关值原样保留,不被猜测改写。
func TestLegacyBaseURLMigration(t *testing.T) {
	svc, _, _ := newService(t, http.DefaultClient)
	if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: "https://api.openai.com", APIKey: strp("sk-mig")}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := svc.Get(ctx())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("旧默认根地址应迁移为带 /v1 形式: %q", got.BaseURL)
	}
	for _, tc := range []struct{ provider, raw, want string }{
		{ProviderClaude, "https://api.anthropic.com", "https://api.anthropic.com/v1"},
		{ProviderOpenAI, "https://api.deepseek.com", "https://api.deepseek.com/v1"},
		{ProviderClaude, "https://api.deepseek.com/anthropic", "https://api.deepseek.com/anthropic/v1"},
		{ProviderOpenAI, "https://api.openai.com/", "https://api.openai.com/v1"},
		{ProviderOpenAI, "https://gw.example/prefix", "https://gw.example/prefix"},
	} {
		if got := migrateLegacyBaseURL(tc.provider, tc.raw); got != tc.want {
			t.Fatalf("migrate(%s, %s) = %q, want %q", tc.provider, tc.raw, got, tc.want)
		}
	}
}

// TestMigrateOllamaProvider 验证存量 ollama 协议一次性迁移为 openai + /v1。
func TestMigrateOllamaProvider(t *testing.T) {
	for _, tc := range []struct{ raw, wantBase string }{
		{"", "http://localhost:11434/v1"},
		{"http://localhost:11434", "http://localhost:11434/v1"},
		{"http://127.0.0.1:11434", "http://127.0.0.1:11434/v1"},
		{"http://gpu.example:11434/api", "http://gpu.example:11434/v1"},
		{"http://gpu.example:11434/api/chat", "http://gpu.example:11434/v1"},
		{"http://gpu.example:11434", "http://gpu.example:11434/v1"},
		{"http://gpu.example:11434/v1", "http://gpu.example:11434/v1"},
	} {
		provider, base := migrateOllamaProvider("ollama", tc.raw)
		if provider != "openai" || base != tc.wantBase {
			t.Fatalf("migrateOllama(%q) = (%s, %q), want (openai, %q)", tc.raw, provider, base, tc.wantBase)
		}
	}
	if provider, base := migrateOllamaProvider("claude", "https://api.anthropic.com"); provider != "claude" || base != "https://api.anthropic.com" {
		t.Fatal("非 ollama provider 不应被改写")
	}
}

func TestBudgetPersisted(t *testing.T) {
	svc, _, _ := newService(t, http.DefaultClient)
	limit := int64(1000000)
	cfg, err := svc.Save(ctx(), SaveInput{
		Provider: ProviderOpenAI,
		APIKey:   strp("sk-budget"),
		Budget:   Budget{MonthlyTokenLimit: &limit},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if cfg.Budget.MonthlyTokenLimit == nil || *cfg.Budget.MonthlyTokenLimit != limit {
		t.Fatalf("预算应持久化: %+v", cfg.Budget)
	}
	// 回读。
	got, _ := svc.Get(ctx())
	if got.Budget.MonthlyTokenLimit == nil || *got.Budget.MonthlyTokenLimit != limit {
		t.Fatalf("回读预算丢失: %+v", got.Budget)
	}
}

// ---- Test 探测:三 provider stub ----

func TestProbeClaudeOK(t *testing.T) {
	var gotPath, gotMethod, gotKey, gotVer string
	var gotBody struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotKey, gotVer = r.URL.Path, r.Method, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[]}`))
	}))
	defer stub.Close()

	svc, _, _ := newService(t, stub.Client())
	res, err := svc.Test(ctx(), TestInput{
		Provider: strp(ProviderClaude),
		BaseURL:  strp(stub.URL + "/v1"),
		Model:    strp("claude-sonnet-4"),
		APIKey:   strp("sk-ant-draftKEY"),
	})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !res.OK {
		t.Fatalf("2xx 应 ok=true: %+v", res)
	}
	if res.LatencyMs < 0 {
		t.Fatalf("延迟应 >=0")
	}
	if res.Error != "" {
		t.Fatalf("成功不应有 error: %q", res.Error)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/messages" {
		t.Fatalf("claude 应 POST /v1/messages, got %s %q", gotMethod, gotPath)
	}
	if gotBody.Model != "claude-sonnet-4" || gotBody.MaxTokens != 1 || len(gotBody.Messages) != 1 {
		t.Fatalf("真实推理探测应带模型且 max_tokens=1: %+v", gotBody)
	}
	if gotKey != "sk-ant-draftKEY" {
		t.Fatalf("claude 应带 x-api-key header")
	}
	if gotVer != "2023-06-01" {
		t.Fatalf("claude 应带 anthropic-version header, got %q", gotVer)
	}
}

func TestProbeOpenAIOK(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	var gotBody struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	svc, _, _ := newService(t, stub.Client())
	res, err := svc.Test(ctx(), TestInput{
		Provider: strp(ProviderOpenAI),
		BaseURL:  strp(stub.URL + "/v1"),
		Model:    strp("gpt-4o"),
		APIKey:   strp("sk-openai-XYZ"),
	})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !res.OK {
		t.Fatalf("openai 2xx 应 ok=true: %+v", res)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/chat/completions" {
		t.Fatalf("openai 应 POST /v1/chat/completions, got %s %q", gotMethod, gotPath)
	}
	if gotBody.Model != "gpt-4o" || gotBody.MaxTokens != 1 {
		t.Fatalf("真实推理探测应带模型且 max_tokens=1: %+v", gotBody)
	}
	if gotAuth != "Bearer sk-openai-XYZ" {
		t.Fatalf("openai 应带 Authorization: Bearer, got %q", gotAuth)
	}
}

func TestProbeModelRequired(t *testing.T) {
	svc, _, _ := newService(t, http.DefaultClient)
	res, err := svc.Test(ctx(), TestInput{
		Provider: strp(ProviderOpenAI),
		BaseURL:  strp("https://gateway.invalid/v1"),
		APIKey:   strp("sk-key"),
	})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if res.OK || res.Error == "" {
		t.Fatalf("缺模型应 ok=false 且有人读 error: %+v", res)
	}
}

func TestProbeAuthFailure4xxNoKeyLeak(t *testing.T) {
	const secret = "sk-ant-SECRET-LEAKCHECK"
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer stub.Close()

	svc, _, _ := newService(t, stub.Client())
	res, err := svc.Test(ctx(), TestInput{
		Provider: strp(ProviderClaude),
		BaseURL:  strp(stub.URL),
		Model:    strp("claude-sonnet-4"),
		APIKey:   strp(secret),
	})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if res.OK {
		t.Fatalf("401 应 ok=false")
	}
	if res.Error == "" {
		t.Fatalf("失败应有人读 error")
	}
	if strings.Contains(res.Error, secret) || strings.Contains(res.Error, "SECRET") {
		t.Fatalf("error 绝不含密钥: %q", res.Error)
	}
	if !strings.Contains(res.Error, "认证失败") {
		t.Fatalf("401 应映射为认证失败: %q", res.Error)
	}
}

func TestProbeTimeout(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	client := stub.Client()
	client.Timeout = 20 * time.Millisecond
	svc, _, _ := newService(t, client)
	res, err := svc.Test(ctx(), TestInput{
		Provider: strp(ProviderOpenAI),
		BaseURL:  strp(stub.URL + "/v1"),
		Model:    strp("gpt-4o"),
		APIKey:   strp("sk-x"),
	})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if res.OK {
		t.Fatalf("超时应 ok=false")
	}
	if res.Error == "" {
		t.Fatalf("超时应有人读 error: %+v", res)
	}
}

func TestProbeConnectionFailure(t *testing.T) {
	svc, _, _ := newService(t, &http.Client{Timeout: time.Second})
	// 指向一个不监听的本地端口。
	res, err := svc.Test(ctx(), TestInput{
		Provider: strp(ProviderOpenAI),
		BaseURL:  strp("http://127.0.0.1:1/v1"),
		Model:    strp("gpt-4o"),
		APIKey:   strp("sk-x"),
	})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if res.OK {
		t.Fatalf("连接失败应 ok=false")
	}
	if !strings.Contains(res.Error, "连接") && !strings.Contains(res.Error, "超时") {
		t.Fatalf("应映射为连接/超时错误: %q", res.Error)
	}
}

func TestTestKeyOmittedUsesStoredKey(t *testing.T) {
	var gotKey string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	svc, _, _ := newService(t, stub.Client())
	// 先存 claude + key + baseUrl + model 指向 stub。
	if _, err := svc.Save(ctx(), SaveInput{
		Provider: ProviderClaude,
		BaseURL:  stub.URL,
		Model:    "claude-sonnet-4",
		APIKey:   strp("sk-ant-STORED-key1"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Test 省略所有字段 → 回退已存配置 + 已存密钥。
	res, err := svc.Test(ctx(), TestInput{})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !res.OK {
		t.Fatalf("应用已存配置探测成功: %+v", res)
	}
	if gotKey != "sk-ant-STORED-key1" {
		t.Fatalf("Test 省略 key 应用已存密钥, got %q", gotKey)
	}
}
