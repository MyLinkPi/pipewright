package ai

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/opschat"
)

var compatibleVendors = []struct {
	name string
	base string
	api  string
}{
	{"gemini", "https://generativelanguage.googleapis.com/v1beta/openai", "https://generativelanguage.googleapis.com/v1beta/openai"},
	{"glm", "https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/paas/v4"},
	{"glm-coding", "https://open.bigmodel.cn/api/coding/paas/v4", "https://open.bigmodel.cn/api/coding/paas/v4"},
	{"kimi-cn", "https://api.moonshot.cn/v1", "https://api.moonshot.cn/v1"},
	{"kimi-ai", "https://api.moonshot.ai/v1", "https://api.moonshot.ai/v1"},
	{"mimo", "https://api.xiaomimimo.com/v1", "https://api.xiaomimimo.com/v1"},
	{"custom-root", "https://gateway.invalid/prefix", "https://gateway.invalid/prefix"},
	{"custom-versioned", "https://gateway.invalid/prefix/v1", "https://gateway.invalid/prefix/v1"},
	{"custom-gemini-prefix", "https://gateway.invalid/v1beta/openai", "https://gateway.invalid/v1beta/openai"},
	{"custom-glm-prefix", "https://gateway.invalid/api/paas/v4", "https://gateway.invalid/api/paas/v4"},
}

func compatibleMockModel(t *testing.T, base string, transport opsTransport) *opsModel {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	svc, _, _ := newService(t, &http.Client{Transport: transport})
	return &opsModel{service: svc.(*service), masker: mask.NewMasker(), lookupIP: func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != u.Hostname() {
			t.Fatalf("unexpected DNS destination %q", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}}
}

func TestCompatibleVendorsCatalogProbeGenerateChatAndStream(t *testing.T) {
	for _, vendor := range compatibleVendors {
		for _, slash := range []string{"", "/"} {
			t.Run(vendor.name+slash, func(t *testing.T) {
				catalogs, chats, streams := 0, 0, 0
				base := vendor.base + slash
				m := compatibleMockModel(t, base, opsTransport(func(r *http.Request) (*http.Response, error) {
					deadline, ok := r.Context().Deadline()
					if !ok || time.Until(deadline) > 60*time.Second || r.Header.Get("Authorization") != "Bearer mock-key" || r.Header.Get("x-api-key") != "" {
						t.Error("unbounded request or incorrect compatible authentication")
					}
					body := `{"data":[{"id":"account/model:custom","name":"Custom"},{"id":"a"},{"id":"a"}]}`
					header := http.Header{"Content-Type": {"application/json"}}
					if r.Method == http.MethodGet {
						catalogs++
						if r.URL.String() != vendor.api+"/models" || time.Until(deadline) > 8*time.Second {
							t.Errorf("catalog endpoint %q", r.URL.String())
						}
					} else {
						if r.URL.String() != vendor.api+"/chat/completions" || r.Method != http.MethodPost {
							t.Errorf("inference endpoint %q", r.URL.String())
						}
						var request struct {
							Model  string `json:"model"`
							Stream bool   `json:"stream"`
						}
						if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "account/model:custom" {
							t.Error("manual/account model ID changed")
						}
						body = `{"choices":[{"message":{"content":"reply"}}]}`
						if request.Stream {
							streams++
							body = "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"hidden\",\"content\":\"First \"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"reply\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
							header.Set("Content-Type", "text/event-stream")
						} else {
							chats++
						}
					}
					return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				}))
				if apiEndpoint(ProviderOpenAI, base, "chat/completions") != vendor.api+"/chat/completions" || apiEndpoint(ProviderOpenAI, base, "models") != vendor.api+"/models" {
					t.Fatal("shared endpoint mismatch")
				}
				res, err := m.models(ctx(), TestInput{Provider: strp(ProviderOpenAI), BaseURL: &base, APIKey: strp("mock-key")})
				if err != nil || !res.OK || len(res.Items) != 2 || res.Items[1].ID != "account/model:custom" {
					t.Fatal(res, err)
				}
				if res := m.probe(ctx(), ProviderOpenAI, base, "account/model:custom", "mock-key"); !res.OK {
					t.Fatal(res)
				}
				cfg := &Config{Provider: ProviderOpenAI, BaseURL: base, Model: "account/model:custom"}
				text, err := m.chat(ctx(), cfg, "mock-key", "question", 4096)
				if err != nil || text != "reply" {
					t.Fatal(text, err)
				}
				text, err = m.chatWithTokens(ctx(), ProviderOpenAI, base, cfg.Model, "mock-key", "question", 4096)
				if err != nil || text != "First reply" { // 流式累积(含首帧前缀)
					t.Fatal("generate/shared chat", text, err)
				}
				var visible strings.Builder
				chunks := 0
				text, err = m.chatStream(ctx(), cfg, "mock-key", "question", 4096, false, func(chunk opschat.StreamChunk) error {
					if chunk.Fallback {
						t.Error("SSE falsely treated as fallback")
					}
					chunks++
					visible.WriteString(chunk.Text)
					return nil
				})
				if err != nil || text != "First reply" || visible.String() != text || chunks < 2 || catalogs != 1 || chats != 1 || streams != 3 {
					t.Fatal(text, err, visible.String(), chunks, catalogs, chats, streams)
				}
			})
		}
	}
}

// 原样拼接契约:base 仅去空白与尾部斜杠后直接拼接资源路径,任何形态(含裸根、
// 携带 query/userinfo/fragment 的畸形值)都不被改写;唯一的特例是 DeepSeek 的
// Claude 协议模型目录映射。
func TestCompatibleEndpointVerbatimConcatenation(t *testing.T) {
	for _, base := range []string{
		"https://gateway.invalid/prefix",
		"https://gateway.invalid/prefix/v1",
		"https://gateway.invalid/prefix/v2",
		"https://generativelanguage.googleapis.com.evil.invalid/v1beta/openai",
		"https://user@gateway.invalid/prefix",
		"https://gateway.invalid/prefix?key=x",
		"https://gateway.invalid/prefix#fragment",
		"https://gateway.invalid/api/paas/v4",
	} {
		if apiEndpoint(ProviderOpenAI, base, "chat/completions") != strings.TrimRight(base, "/")+"/chat/completions" {
			t.Fatal("base rewritten", base)
		}
	}
	// 裸根形式不再自动补版本段;尾部斜杠等价。
	if apiEndpoint(ProviderOpenAI, "https://api.openai.com", "chat/completions") != "https://api.openai.com/chat/completions" {
		t.Fatal("bare root gained a version segment")
	}
	if apiEndpoint(ProviderOpenAI, "https://api.openai.com/v1/", "models") != "https://api.openai.com/v1/models" {
		t.Fatal("trailing slash not normalized away")
	}
	for _, base := range []string{"https://generativelanguage.googleapis.com/v1beta/openai", "https://open.bigmodel.cn/api/paas/v4"} {
		if apiEndpoint(ProviderClaude, base, "messages") != base+"/messages" {
			t.Fatal("non-OpenAI protocol changed", base)
		}
	}
	// 唯一特例:DeepSeek Claude 目录走官方 OpenAI 目录前缀。
	if apiEndpoint(ProviderClaude, "https://api.deepseek.com/anthropic", "models") != "https://api.deepseek.com/v1/models" {
		t.Fatal("DeepSeek catalog mapping lost")
	}
	if apiEndpoint(ProviderClaude, "https://api.deepseek.com/anthropic", "messages") != "https://api.deepseek.com/anthropic/messages" {
		t.Fatal("inference endpoint specialized")
	}
}

func TestCompatibleVendorsCatalog404IsFailureAndPreservesManualModel(t *testing.T) {
	for _, vendor := range compatibleVendors[:6] {
		t.Run(vendor.name, func(t *testing.T) {
			calls := 0
			m := compatibleMockModel(t, vendor.base, opsTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				// 目录走 GET models;真实推理探测走 POST chat/completions —— 均返回 404。
				if r.Method == http.MethodGet {
					if r.URL.String() != vendor.api+"/models" {
						t.Fatal("catalog failure retried inference or changed destination")
					}
				} else if r.URL.String() != vendor.api+"/chat/completions" || r.Method != http.MethodPost {
					t.Fatal("inference probe changed destination")
				}
				return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("mock-key")), Request: r}, nil
			}))
			if _, err := m.service.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: vendor.base, APIKey: strp("mock-key"), Model: "manual-model"}); err != nil {
				t.Fatal(err)
			}
			res, err := m.models(ctx(), TestInput{})
			if err != nil || res.OK || len(res.Items) != 0 || !strings.Contains(res.Error, "404") || strings.Contains(res.Error, "mock-key") {
				t.Fatal(res, err)
			}
			if res := m.probe(ctx(), ProviderOpenAI, vendor.base, "manual-model", "mock-key"); res.OK || !strings.Contains(res.Error, "404") || strings.Contains(res.Error, "mock-key") {
				t.Fatal(res)
			}
			cfg, err := m.service.Get(ctx())
			if err != nil || cfg.Model != "manual-model" || cfg.BaseURL != vendor.base || calls != 2 {
				t.Fatal("catalog failure mutated manual configuration", cfg, err, calls)
			}
		})
	}
}

func TestCompatibleVendorsSavedKeyDestinationBoundaries(t *testing.T) {
	for _, vendor := range compatibleVendors[:6] {
		t.Run(vendor.name, func(t *testing.T) {
			calls := 0
			m := compatibleMockModel(t, vendor.base, opsTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"a"}]}`)), Request: r}, nil
			}))
			if _, err := m.service.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: vendor.base, APIKey: strp("saved-key")}); err != nil {
				t.Fatal(err)
			}
			if res, err := m.models(ctx(), TestInput{BaseURL: strp(vendor.base + "/")}); err != nil || !res.OK || calls != 1 {
				t.Fatal("same base no longer works", res, err, calls)
			}
			changed := vendor.base + "/custom"
			gateway, _ := url.Parse(vendor.base)
			gateway.Host = "gateway.invalid"
			for _, in := range []TestInput{
				{BaseURL: &changed},
				{BaseURL: strp(gateway.String())},
				{Provider: strp(ProviderClaude), BaseURL: &vendor.base},
			} {
				if _, err := m.models(ctx(), in); err != ErrAPIKeyRequired {
					t.Fatal("models reused saved key", err)
				}
				if _, err := m.service.Test(ctx(), in); err != ErrAPIKeyRequired {
					t.Fatal("probe reused saved key", err)
				}
			}
			for _, other := range compatibleVendors[:6] {
				if other.base == vendor.base {
					continue
				}
				in := TestInput{BaseURL: &other.base}
				if _, err := m.models(ctx(), in); err != ErrAPIKeyRequired {
					t.Fatal("catalog reused key across vendor/region/plan", other.name, err)
				}
				if _, err := m.service.Test(ctx(), in); err != ErrAPIKeyRequired {
					t.Fatal("probe reused key across vendor/region/plan", other.name, err)
				}
			}
			if _, err := m.service.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: changed}); err != ErrAPIKeyRequired || calls != 1 {
				t.Fatal("save reused saved key", err, calls)
			}
		})
	}
}
