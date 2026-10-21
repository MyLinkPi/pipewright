// provider_regression_test.go 固化 issue #189 的两处回归:
//  1. DeepSeek 系端点的结构化推理兼容(thinking 禁用与 token 预算注入矩阵);
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

func TestIssue189StructuredDeepSeekRequestCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, provider, base, model string
		disabled                    bool
	}{
		{"openai", ProviderOpenAI, "https://api.deepseek.com", "deepseek-v4-pro", true},
		{"openai-v1", ProviderOpenAI, "https://api.deepseek.com/v1", "deepseek-flash", true},
		{"anthropic", ProviderClaude, "https://api.deepseek.com/anthropic", "deepseek-v4-pro", true},
		{"explicit-reasoner", ProviderOpenAI, "https://api.deepseek.com", "deepseek-reasoner", false},
		{"gateway", ProviderOpenAI, "https://gateway.example/v1", "deepseek-v4-pro", false},
		{"custom-path", ProviderOpenAI, "https://api.deepseek.com/custom/v1", "deepseek-v4-pro", false},
		{"lookalike", ProviderOpenAI, "https://api.deepseek.com.example/v1", "deepseek-v4-pro", false},
	} {
		for _, limit := range []int{generateMaxTokens, diagnoseMaxTokens, completeMaxTokens} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, limit), func(t *testing.T) {
				calls := 0
				m := compatibleMockModel(t, tc.base, opsTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					_, disabled := body["thinking"]
					if disabled != tc.disabled || disabled && string(body["thinking"]) != `{"type":"disabled"}` {
						t.Fatal("unexpected reasoning mode", string(body["thinking"]))
					}
					_, bounded := body["max_tokens"]
					if bounded != (tc.provider == ProviderClaude || tc.disabled) || bounded && string(body["max_tokens"]) != fmt.Sprint(limit) {
						t.Fatal("unexpected token budget", string(body["max_tokens"]))
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
			text, err := m.chatWithTokens(ctx(), tc.provider, tc.base, "deepseek-v4-pro", "mock-key", "complete docker run", completeMaxTokens)
			if !errors.Is(err, ErrGenerateFailed) || !strings.Contains(err.Error(), "长度限制") || text != "" || calls != 1 {
				t.Fatalf("partial command accepted: text=%q err=%v calls=%d", text, err, calls)
			}
		})
	}
}
