package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/opschat"
)

// provider 拒绝带 tools 的请求 → AgentRound 单次调用、错误直接抛(不降级重试)。
func TestAgentRoundRejectsWhenProviderRejectsTools(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, hasTools := body["tools"]; hasTools {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"tools not supported"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"plain answer"}}]}`)
	}))
	defer srv.Close()
	svc, _, _ := newService(t, srv.Client())
	key := "sk-x"
	if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: srv.URL + "/v1", Model: "m", APIKey: &key, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	m := NewOpsModel(svc, mask.NewMasker())
	req := opschat.AgentRequest{Question: "disk usage", Provider: m.Info(), System: "sys", Tools: opschat.Tools(), Targets: []string{"target_0"}, MaxResponseBytes: 32 << 10}
	reply, err := m.AgentRound(context.Background(), req)
	if err == nil {
		t.Fatal("rejection must propagate, got", reply)
	}
	if calls != 1 {
		t.Fatal("exactly one request, no fallback", calls)
	}
}

// 历史里的空文本条目(tool_call/summary)不进消息;Claude 首条 user、角色交替。
func TestAgentHistoryFiltersEmptyAndClaudeAlternation(t *testing.T) {
	req := opschat.AgentRequest{
		Question: "q",
		History: []opschat.Message{
			{Role: "tool_call", Text: ""},
			{Role: "user", Text: "first"},
			{Role: "user", Text: "second"},
			{Role: "assistant", Text: "mid"},
			{Role: "assistant", Text: "tail"},
		},
	}
	h := agentHistory(&opsModel{masker: mask.NewMasker()}, req)
	if len(h) != 4 || h[0].Role != "user" || strings.TrimSpace(h[1].Text) == "" {
		t.Fatal(h)
	}
	msgs := claudeAgentMessages(req, "q", h)
	if msgs[0]["role"] != "user" {
		t.Fatal("claude first message must be user", msgs[0])
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i]["role"] == msgs[i-1]["role"] {
			t.Fatal("claude messages must alternate", msgs)
		}
	}
	first := msgs[0]["content"].(string)
	if !strings.Contains(first, "first") || strings.Contains(first, "q") {
		t.Fatal("consecutive user history merges into one message", first)
	}
	last := msgs[len(msgs)-1]["content"].(string)
	if !strings.Contains(last, "q") {
		t.Fatal("question should land in the final user message", last)
	}
}
