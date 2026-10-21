package ai

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/opschat"
)

// AgentRound 实现原生工具调用的一轮 Agent 请求(OpenAI / Claude 协议;非流式):
// 每个工具 schema 注入 target 枚举(必填),变更工具再注入 purpose/reason(必填);
// 响应解析为 终答文本 或 工具批。输出统一限额 + 脱敏。
func (m *opsModel) AgentRound(ctx context.Context, req opschat.AgentRequest) (opschat.AgentReply, error) {
	var reply opschat.AgentReply
	if req.Provider != m.Info() {
		return reply, opschat.ErrConsent
	}
	cfg, key, info, err := m.snapshot(ctx)
	if err != nil {
		return reply, err
	}
	if info != req.Provider {
		return reply, opschat.ErrConsent
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL(cfg.Provider)
	}
	destination, err := opsResolve(ctx, base, m.lookupIP)
	if err != nil {
		return reply, opschat.ErrUnavailable
	}
	// 与 opsModel.chat 同款 transport 构建:IP-pin + 禁代理 + 禁重定向;超时由调用方 ctx 控制。
	client := *m.service.client
	client.Timeout = 2 * time.Minute // 模型轮次超时(opschat 侧另有 ModelTimeout ctx)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if original, ok := transport.(*http.Transport); ok {
		pinned := original.Clone()
		pinned.Proxy = nil
		pinned.DialContext = destination.dial(m.dialContext)
		pinned.DialTLS = nil
		pinned.DialTLSContext = nil
		if pinned.TLSClientConfig == nil {
			pinned.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		pinned.TLSClientConfig.ServerName = destination.host
		pinned.DisableKeepAlives = true
		client.Transport = pinned
		defer pinned.CloseIdleConnections()
	}
	_ = transport

	var (
		endpoint string
		header   = map[string]string{"Content-Type": "application/json"}
		body     map[string]any
	)
	switch cfg.Provider {
	case ProviderClaude:
		endpoint = apiEndpoint(cfg.Provider, base, "messages")
		header["x-api-key"] = key
		header["anthropic-version"] = "2023-06-01"
		msgs := claudeAgentMessages(scrubAgentRequest(m, req))
		body = map[string]any{
			"model":      cfg.Model,
			"max_tokens": 4096,
			"system":     req.System,
			"messages":   msgs,
			"tools":      claudeAgentTools(req),
		}
	case ProviderOpenAI:
		endpoint = apiEndpoint(cfg.Provider, base, "chat/completions")
		header["Authorization"] = "Bearer " + key
		scrubbed := scrubAgentRequest(m, req)
		body = map[string]any{
			"model":       cfg.Model,
			"messages":    openaiAgentMessages(scrubbed),
			"tools":       openaiAgentTools(scrubbed),
			"tool_choice": "auto",
		}
	default:
		return reply, opschat.ErrUnavailable
	}
	if len(req.Tools) == 0 {
		delete(body, "tools")
		delete(body, "tool_choice")
	}
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > 256<<10 {
		return reply, opsFailure("invalid_response", 0)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return reply, opsFailure("invalid_response", 0)
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	resp, err := client.Do(r)
	if err != nil {
		return reply, opsTransportFailure(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return reply, opsHTTPFailure(resp.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, opsEnvelopeBytes+1))
	if err != nil {
		return reply, opsTransportFailure(ctx, err)
	}
	if len(payload) > opsEnvelopeBytes {
		return reply, opsResponseLimit()
	}
	if err := opsCompletionFailure(cfg.Provider, payload); err != nil {
		return reply, err
	}
	if cfg.Provider == ProviderClaude {
		reply, err = parseClaudeAgentReply(payload)
	} else {
		reply, err = parseOpenAIAgentReply(payload)
	}
	if err != nil {
		return reply, err
	}
	reply.Text = m.safeText(reply.Text)
	if reply.Text == "" && len(reply.ToolCalls) == 0 {
		return reply, opsFailure("empty_response", 0)
	}
	if len(reply.Text) > req.MaxResponseBytes && req.MaxResponseBytes > 0 {
		return reply, opsResponseLimit()
	}
	if len(reply.Text) > 32<<10 {
		return reply, opsResponseLimit()
	}
	if len(reply.ToolCalls) > 24 {
		return reply, opsFailure("invalid_response", 0)
	}
	return reply, nil
}

// ─── schema 注入 ─────────────────────────────────────────────────────────────

// agentSchema 把工具 schema 注入 target(必填枚举)与变更工具的 purpose/reason(必填)。
func agentSchema(t opschat.Tool, targets []string, mut bool) map[string]any {
	var schema struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	_ = json.Unmarshal(t.Schema, &schema)
	if schema.Properties == nil {
		schema.Properties = map[string]any{}
	}
	schema.Properties["target"] = map[string]any{"type": "string", "enum": targets}
	schema.Required = append(schema.Required, "target")
	if mut {
		schema.Properties["purpose"] = map[string]any{"type": "string", "maxLength": 512}
		schema.Properties["reason"] = map[string]any{"type": "string", "maxLength": 512}
		schema.Required = append(schema.Required, "purpose", "reason")
	}
	return map[string]any{"type": schema.Type, "properties": schema.Properties, "required": schema.Required, "additionalProperties": false}
}

func openaiAgentTools(req opschat.AgentRequest) []map[string]any {
	out := []map[string]any{}
	for _, t := range req.Tools {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.ID,
				"description": agentToolDescription(t.ID, req),
				"parameters":  agentSchema(t, req.Targets, toolMutates(t.ID)),
			},
		})
	}
	return out
}

func claudeAgentTools(req opschat.AgentRequest) []map[string]any {
	out := []map[string]any{}
	for _, t := range req.Tools {
		out = append(out, map[string]any{
			"name":         t.ID,
			"description":  agentToolDescription(t.ID, req),
			"input_schema": agentSchema(t, req.Targets, toolMutates(t.ID)),
		})
	}
	return out
}

// toolMutates 与 opschat.mutation 保持一致(本地镜像,避免导出)。
func toolMutates(id string) bool {
	return id == "docker_action" || id == "systemd_action" || id == "docker_exec" || id == "host_exec"
}

func agentToolDescription(id string, req opschat.AgentRequest) string {
	if toolMutates(id) {
		return "Mutating operation on the selected target; requires purpose and reason; runs only after human confirmation."
	}
	if req.WrapUp {
		return ""
	}
	return "Read-only collection on the selected target."
}

// ─── messages 构造 ───────────────────────────────────────────────────────────

func openaiAgentMessages(req opschat.AgentRequest) []map[string]any {
	msgs := []map[string]any{{"role": "system", "content": req.System}}
	if req.WrapUp {
		msgs = append(msgs, map[string]any{"role": "user", "content": wrapUpPrompt(req)})
		return msgs
	}
	for _, m := range req.History {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": m.Text})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": req.Question})
	for _, round := range req.Rounds {
		calls := []map[string]any{}
		for _, c := range round.Calls {
			args := map[string]any{"target": c.Target}
			if len(c.Args) > 0 {
				_ = json.Unmarshal(c.Args, &args)
			}
			args["target"] = c.Target
			raw, _ := json.Marshal(args)
			calls = append(calls, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.ToolID,
					"arguments": string(raw),
				},
			})
		}
		if len(calls) > 0 {
			msgs = append(msgs, map[string]any{"role": "assistant", "tool_calls": calls})
		}
		for _, res := range round.Results {
			content := res.Output
			if res.Denied {
				content = fmt.Sprintf("denied by user; reason: %s", res.DenialReason)
			} else if res.Failed {
				content = "failed: " + content
			}
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": res.CallID, "content": content})
		}
	}
	return msgs
}

func claudeAgentMessages(req opschat.AgentRequest) []map[string]any {
	msgs := []map[string]any{}
	if req.WrapUp {
		return []map[string]any{{"role": "user", "content": wrapUpPrompt(req)}}
	}
	for _, m := range req.History {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": m.Text})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": req.Question})
	for _, round := range req.Rounds {
		blocks := []map[string]any{}
		for _, c := range round.Calls {
			args := map[string]any{"target": c.Target}
			if len(c.Args) > 0 {
				_ = json.Unmarshal(c.Args, &args)
			}
			args["target"] = c.Target
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": c.ID, "name": c.ToolID, "input": args})
		}
		if len(blocks) > 0 {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
		}
		results := []map[string]any{}
		for _, res := range round.Results {
			content := res.Output
			if res.Denied {
				content = fmt.Sprintf("denied by user; reason: %s", res.DenialReason)
			} else if res.Failed {
				content = "failed: " + content
			}
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": res.CallID, "content": content})
		}
		if len(results) > 0 {
			msgs = append(msgs, map[string]any{"role": "user", "content": results})
		}
	}
	return msgs
}

func wrapUpPrompt(req opschat.AgentRequest) string {
	summary := ""
	for _, round := range req.Rounds {
		for _, c := range round.Calls {
			summary += c.ToolID + " -> "
			for _, res := range round.Results {
				if res.CallID == c.ID {
					if res.Denied {
						summary += "denied"
					} else if res.Failed {
						summary += "failed"
					} else {
						summary += "ok"
					}
				}
			}
			summary += "\n"
		}
	}
	return "The tool round budget is exhausted. Summarize honestly: completed progress, what remains unfinished, and concrete suggestions for the user. Tool log so far:\n" + summary
}

// ─── 响应解析 ────────────────────────────────────────────────────────────────

func parseOpenAIAgentReply(payload []byte) (opschat.AgentReply, error) {
	var r struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &r); err != nil || len(r.Choices) == 0 {
		return opschat.AgentReply{}, opsFailure("invalid_response", 0)
	}
	msg := r.Choices[0].Message
	reply := opschat.AgentReply{Text: msg.Content}
	for _, tc := range msg.ToolCalls {
		if tc.Type != "function" || tc.Function.Name == "" {
			return opschat.AgentReply{}, opsFailure("invalid_response", 0)
		}
		args := json.RawMessage(tc.Function.Arguments)
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		if !json.Valid(args) {
			return opschat.AgentReply{}, opsFailure("invalid_response", 0)
		}
		call := opschat.AgentToolCall{ID: tc.ID, ToolID: tc.Function.Name, Args: args}
		var fields map[string]json.RawMessage
		if json.Unmarshal(args, &fields) == nil {
			_ = json.Unmarshal(fields["purpose"], &call.Purpose)
			_ = json.Unmarshal(fields["reason"], &call.Reason)
			_ = json.Unmarshal(fields["target"], &call.Target)
		}
		reply.ToolCalls = append(reply.ToolCalls, call)
	}
	return reply, nil
}

func parseClaudeAgentReply(payload []byte) (opschat.AgentReply, error) {
	var r struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(payload, &r); err != nil {
		return opschat.AgentReply{}, opsFailure("invalid_response", 0)
	}
	reply := opschat.AgentReply{}
	for _, block := range r.Content {
		switch block.Type {
		case "text":
			reply.Text += block.Text
		case "tool_use":
			if block.Name == "" {
				return opschat.AgentReply{}, opsFailure("invalid_response", 0)
			}
			call := opschat.AgentToolCall{ID: block.ID, ToolID: block.Name, Args: block.Input}
			if len(call.Args) == 0 {
				call.Args = json.RawMessage("{}")
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(call.Args, &fields) == nil {
				_ = json.Unmarshal(fields["purpose"], &call.Purpose)
				_ = json.Unmarshal(fields["reason"], &call.Reason)
				_ = json.Unmarshal(fields["target"], &call.Target)
			}
			reply.ToolCalls = append(reply.ToolCalls, call)
		}
	}
	if strings.TrimSpace(reply.Text) == "" && len(reply.ToolCalls) == 0 {
		return opschat.AgentReply{}, opsFailure("empty_response", 0)
	}
	return reply, nil
}

// scrubAgentRequest 对请求内全部文本(问题/历史/系统提示)脱敏;工具结果在 opschat 侧已脱敏。
func scrubAgentRequest(m *opsModel, req opschat.AgentRequest) opschat.AgentRequest {
	out := req
	out.Question = m.safeText(req.Question)
	out.System = m.safeText(req.System)
	if len(req.History) > 0 {
		out.History = append([]opschat.Message(nil), req.History...)
		for i := range out.History {
			out.History[i].Text = m.safeText(out.History[i].Text)
		}
	}
	return out
}
