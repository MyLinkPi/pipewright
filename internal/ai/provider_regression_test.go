// provider_regression_test.go 固化 issue #189 的两处回归:
//  1. 有界结构化回复的 token 预算必须如实下发(不被兼容层改写),且请求体不注入
//     提供方私有开关(thinking 等)——由调用方按预算自行适配,而非改写模型语义;
//  2. 截断的不完整回复(finish_reason=length 等)必须被拒绝,不得当作有效命令采纳。
//
// 取自上游 26f3aa8;该提交所附的另外四个用例依赖 94c4295(未引入)的探测
// 超时行为,故不随迁。helper(compatibleMockModel/wrapProviderResponse/opsTransport)
// 复用包内既有定义。
package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIssue189StructuredRequestTokenBudget(t *testing.T) {
	for _, tc := range []struct {
		name, provider, base, model string
	}{
		{"openai", ProviderOpenAI, "https://api.deepseek.com", "deepseek-v4-pro"},
		{"openai-v1", ProviderOpenAI, "https://api.deepseek.com/v1", "deepseek-flash"},
		{"anthropic", ProviderClaude, "https://api.deepseek.com/anthropic", "deepseek-v4-pro"},
		{"explicit-reasoner", ProviderOpenAI, "https://api.deepseek.com", "deepseek-reasoner"},
		{"gateway", ProviderOpenAI, "https://gateway.example/v1", "deepseek-v4-pro"},
		{"custom-path", ProviderOpenAI, "https://api.deepseek.com/custom/v1", "deepseek-v4-pro"},
		{"lookalike", ProviderOpenAI, "https://api.deepseek.com.example/v1", "deepseek-v4-pro"},
	} {
		for _, limit := range []int{DefaultMaxOutputTokens, diagnoseMinTokens, 64} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, limit), func(t *testing.T) {
				calls := 0
				m := compatibleMockModel(t, tc.base, opsTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if _, injected := body["thinking"]; injected {
						t.Fatal("request must not inject provider-specific reasoning switches", string(body["thinking"]))
					}
					if got := string(body["max_tokens"]); got != fmt.Sprint(limit) {
						t.Fatal("unexpected token budget", got)
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wrapProviderResponse(tc.provider, "reply"))), Request: r}, nil
				}))
				text, err := m.chatWithTokens(ctx(), tc.provider, tc.base, tc.model, "mock-key", "public fixture", limit)
				if err != nil || text != "reply" || calls != 1 {
					t.Fatalf("text=%q err=%v calls=%d", text, err, calls)
				}
			})
		}
	}
}

func TestIssue189StructuredReplyRejectsTruncatedCommand(t *testing.T) {
	for _, tc := range []struct {
		provider, base, response string
	}{
		{ProviderOpenAI, "https://api.deepseek.com", `{"choices":[{"message":{"content":"docker run --name demo"},"finish_reason":"length"}]}`},
		{ProviderClaude, "https://api.deepseek.com/anthropic", `{"content":[{"type":"text","text":"docker run --name demo"}],"stop_reason":"max_tokens"}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			calls := 0
			m := compatibleMockModel(t, tc.base, opsTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.response)), Request: r}, nil
			}))
			text, err := m.chatWithTokens(ctx(), tc.provider, tc.base, "deepseek-v4-pro", "mock-key", "complete docker run", 64)
			if !errors.Is(err, ErrGenerateFailed) || !strings.Contains(err.Error(), "长度限制") || text != "" || calls != 1 {
				t.Fatalf("partial command accepted: text=%q err=%v calls=%d", text, err, calls)
			}
		})
	}
}
