// Package ai — AgentRound:运维 Agent 的单轮原生工具调用请求。
// 每轮恰好一次 HTTP 请求(无重试、无降级);工具 schema 注入 target 枚举,
// 变更工具再注入 purpose/reason。输出限额 + 脱敏。
package ai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/huangchengsir/pipewright/internal/opschat"
)

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
	// 与 opsModel.chat 同款 transport:IP-pin + 禁代理 + 禁重定向;
	// 轮次超时由 opschat 的 ModelTimeout ctx 控制,这里再兜一层客户端上限。
	client := *m.service.client
	client.Timeout = 2 * time.Minute
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

	question := m.safeText(req.Question)
	history := agentHistory(m, req)
	endpoint, header, body := agentRequest(cfg.Provider, base, cfg.Model, key, req, question, history)
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
		log.Printf("[ai] agent round transport error provider=%s model=%s host=%s err=%v", cfg.Provider, cfg.Model, destination.host, err)
		return reply, opsTransportFailure(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// provider 拒绝:状态 + 响应体片段。服务端落完整日志;错误携带脱敏后的
		// provider 原文(仅清除我们自己登记的 secret,其余原样透出给配置该 provider 的管理员)。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		detail := sanitizeProviderDetail(m.safeText(string(raw)))
		log.Printf("[ai] agent round rejected provider=%s model=%s host=%s status=%d body=%.1000s",
			cfg.Provider, cfg.Model, destination.host, resp.StatusCode, detail)
		if detail == "" {
			return reply, opsHTTPFailure(resp.StatusCode)
		}
		return reply, fmt.Errorf("%w: %s", opsHTTPFailure(resp.StatusCode), detail)
	}
	if contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); contentType == "text/event-stream" {
		reply, err = consumeAgentStream(cfg.Provider, resp.Body)
		if err != nil {
			return reply, err
		}
	} else {
		// 网关忽略 stream 参数时回退整包:消费这一个响应,绝不重试计费请求。
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
	}
	reply.Text = m.safeText(reply.Text)
	if reply.Text == "" && len(reply.ToolCalls) == 0 {
		return reply, opsFailure("empty_response", 0)
	}
	if len(reply.Text) > 32<<10 || req.MaxResponseBytes > 0 && len(reply.Text) > req.MaxResponseBytes {
		return reply, opsResponseLimit()
	}
	if len(reply.ToolCalls) > 24 {
		return reply, opsFailure("invalid_response", 0)
	}
	return reply, nil
}

// ─── 请求构造 ─────────────────────────────────────────────────────────────────

func agentRequest(provider, base, model, key string, req opschat.AgentRequest, question string, history []opschat.Message) (string, map[string]string, map[string]any) {
	header := map[string]string{"Content-Type": "application/json"}
	switch provider {
	case ProviderClaude:
		header["x-api-key"] = key
		header["anthropic-version"] = "2023-06-01"
		body := map[string]any{
			"model":      model,
			"max_tokens": 4096,
			"stream":     true,
			"system":     req.System,
			"messages":   claudeAgentMessages(req, question, history),
		}
		if len(req.Tools) > 0 {
			body["tools"] = claudeAgentTools(req)
		}
		return apiEndpoint(provider, base, "messages"), header, body
	default:
		header["Authorization"] = "Bearer " + key
		body := map[string]any{
			"model":      model,
			"max_tokens": 4096,
			"stream":     true,
			"messages":   openaiAgentMessages(req, question, history),
		}
		if len(req.Tools) > 0 {
			body["tools"] = openaiAgentTools(req)
			body["tool_choice"] = "auto"
		}
		return apiEndpoint(provider, base, "chat/completions"), header, body
	}
}

// agentHistory 过滤会话历史:仅 user/assistant 角色且文本非空,并脱敏
// (tool_call/summary 等条目文本为空;Claude 拒绝空 content,部分网关同样拒绝)。
func agentHistory(m *opsModel, req opschat.AgentRequest) []opschat.Message {
	out := []opschat.Message{}
	for _, msg := range req.History {
		text := m.safeText(msg.Text)
		if strings.TrimSpace(text) == "" {
			continue
		}
		role := "user"
		if msg.Role == "assistant" {
			role = "assistant"
		}
		out = append(out, opschat.Message{Role: role, Text: text})
	}
	return out
}

// ─── 消息构造 ─────────────────────────────────────────────────────────────────

func openaiAgentMessages(req opschat.AgentRequest, question string, history []opschat.Message) []map[string]any {
	msgs := []map[string]any{{"role": "system", "content": req.System}}
	if req.WrapUp {
		return append(msgs, map[string]any{"role": "user", "content": wrapUpPrompt(question)})
	}
	for _, msg := range history {
		msgs = append(msgs, map[string]any{"role": msg.Role, "content": msg.Text})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": question})
	for _, round := range req.Rounds {
		calls := []map[string]any{}
		for _, c := range round.Calls {
			args := map[string]any{}
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
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": res.CallID, "content": agentResultText(res)})
		}
	}
	return msgs
}

// claudeAgentMessages 满足 Claude 的硬性要求:首条 user、角色交替、content 非空。
// 历史先合并相邻同角色、丢弃开头 assistant;问题并入末条 user;轮次消息
// (assistant tool_use / user tool_result)天然交替,直接追加。
func claudeAgentMessages(req opschat.AgentRequest, question string, history []opschat.Message) []map[string]any {
	if req.WrapUp {
		return []map[string]any{{"role": "user", "content": wrapUpPrompt(question)}}
	}
	merged := []opschat.Message{}
	for _, msg := range history {
		if len(merged) == 0 {
			if msg.Role != "user" {
				continue
			}
			merged = append(merged, msg)
			continue
		}
		if last := &merged[len(merged)-1]; last.Role == msg.Role {
			last.Text += "\n\n" + msg.Text
			continue
		}
		merged = append(merged, msg)
	}
	if n := len(merged); n > 0 && merged[n-1].Role == "user" {
		merged[n-1].Text += "\n\n" + question
	} else {
		merged = append(merged, opschat.Message{Role: "user", Text: question})
	}
	msgs := make([]map[string]any, 0, len(merged)+2*len(req.Rounds))
	for _, msg := range merged {
		msgs = append(msgs, map[string]any{"role": msg.Role, "content": msg.Text})
	}
	for _, round := range req.Rounds {
		blocks := []map[string]any{}
		for _, c := range round.Calls {
			args := map[string]any{}
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
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": res.CallID, "content": agentResultText(res)})
		}
		if len(results) > 0 {
			msgs = append(msgs, map[string]any{"role": "user", "content": results})
		}
	}
	return msgs
}

// agentResultText 工具结果转非空文本(空输出对 Claude 是非法 content)。
func agentResultText(res opschat.AgentToolResult) string {
	switch {
	case res.Denied:
		return fmt.Sprintf("denied by user; reason: %s", res.DenialReason)
	case res.Failed:
		if strings.TrimSpace(res.Output) == "" {
			return "failed"
		}
		return "failed: " + res.Output
	case strings.TrimSpace(res.Output) == "":
		return "ok"
	default:
		return res.Output
	}
}

// wrapUpPrompt 收尾轮指令:工具结果已随轮次消息在上下文中,不在此重复。
func wrapUpPrompt(question string) string {
	return "The tool round budget is exhausted. Regarding the question:\n" + question + "\n\nSummarize honestly: completed progress, what remains unfinished, and concrete suggestions. Never fabricate completion."
}

// ─── 工具 schema 注入 ─────────────────────────────────────────────────────────

var agentToolDescriptions = map[string]string{
	"host_resources":    "Disk, memory and uptime facts of the target host",
	"host_processes":    "Top processes by CPU on the target host",
	"host_ports":        "Listening TCP/UDP ports on the target host",
	"docker_containers": "List Docker containers on the target host",
	"docker_stats":      "Live CPU/memory metrics of Docker containers",
	"docker_images":     "List Docker images with sizes",
	"docker_inspect":    "Inspect one Docker container (state, ports, mounts)",
	"docker_logs":       "Tail logs of one Docker container",
	"systemd_status":    "Show a systemd unit's active state",
	"systemd_logs":      "Tail journal logs of a systemd unit",
	"docker_action":     "start/stop/restart/pause/unpause a container (requires confirmation)",
	"docker_exec":       "Run a literal-argv command inside a container (requires confirmation)",
	"systemd_action":    "start/stop/restart a systemd unit (requires confirmation)",
	"host_exec":         "Run a literal-argv command on the host itself (requires confirmation)",
}

// agentSchema 注入 target(必填枚举);变更工具再注入 purpose/reason(必填)。
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

func toolMutates(id string) bool {
	return id == "docker_action" || id == "systemd_action" || id == "docker_exec" || id == "host_exec"
}

func openaiAgentTools(req opschat.AgentRequest) []map[string]any {
	out := []map[string]any{}
	for _, t := range req.Tools {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.ID,
				"description": agentToolDescriptions[t.ID],
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
			"description":  agentToolDescriptions[t.ID],
			"input_schema": agentSchema(t, req.Targets, toolMutates(t.ID)),
		})
	}
	return out
}

// ─── 响应解析 ─────────────────────────────────────────────────────────────────

func agentCallFromArgs(id, toolID string, args json.RawMessage) (opschat.AgentToolCall, error) {
	if toolID == "" || (len(args) > 0 && !json.Valid(args)) {
		return opschat.AgentToolCall{}, opsFailure("invalid_response", 0)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	call := opschat.AgentToolCall{ID: id, ToolID: toolID, Args: args}
	var fields map[string]json.RawMessage
	if json.Unmarshal(args, &fields) == nil && fields != nil {
		_ = json.Unmarshal(fields["purpose"], &call.Purpose)
		_ = json.Unmarshal(fields["reason"], &call.Reason)
		_ = json.Unmarshal(fields["target"], &call.Target)
	}
	return call, nil
}

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
		if tc.Type != "function" {
			return opschat.AgentReply{}, opsFailure("invalid_response", 0)
		}
		call, err := agentCallFromArgs(tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
		if err != nil {
			return opschat.AgentReply{}, err
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
			call, err := agentCallFromArgs(block.ID, block.Name, block.Input)
			if err != nil {
				return opschat.AgentReply{}, err
			}
			reply.ToolCalls = append(reply.ToolCalls, call)
		}
	}
	if strings.TrimSpace(reply.Text) == "" && len(reply.ToolCalls) == 0 {
		return opschat.AgentReply{}, opsFailure("empty_response", 0)
	}
	return reply, nil
}

// sanitizeProviderDetail 把 provider 错误片段压成单行、去控制符、截断到 300 字节
// (secret 已由调用方 masker 脱敏;这是 provider 生成的诊断文本,面向管理员透出)。
func sanitizeProviderDetail(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

// ─── SSE 增量消费 ─────────────────────────────────────────────────────────────
//
// consumeAgentStream 消费 provider 的 SSE 流并累积为 AgentReply:
// OpenAI(delta.content + delta.tool_calls 按 index 合并)与 Claude
// (content_block_start/delta 的 text_delta 与 input_json_delta)。限额与
// chatStreamBudget 同款:wire 512KB、单帧 opsEnvelopeBytes、总帧 4096。

type agentStreamAccum struct {
	provider string
	text     strings.Builder
	calls    []opschat.AgentToolCall
	args     []strings.Builder // 与 calls 一一对应,累积 arguments/partial_json
	kind     map[int]int       // Claude index → 0=text / n+1=call n
	finishOK bool
	done     bool
}

func consumeAgentStream(provider string, body io.Reader) (opschat.AgentReply, error) {
	acc := &agentStreamAccum{provider: provider, kind: map[int]int{}}
	reader := &io.LimitedReader{R: body, N: 512<<10 + 1}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), opsEnvelopeBytes)
	frames := 0
	data := ""
	for scanner.Scan() {
		line := scanner.Text()
		if provider == ProviderClaude {
			if strings.HasPrefix(line, "data:") {
				if err := acc.frame(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")); err != nil {
					return opschat.AgentReply{}, err
				}
				frames++
				if frames > 4096 {
					return opschat.AgentReply{}, opsResponseLimit()
				}
			}
			if acc.done {
				break
			}
			continue
		}
		// OpenAI:空行结束一帧,多个 data: 行拼接。
		if line == "" && data != "" {
			if err := acc.frame(strings.TrimSuffix(data, "\n")); err != nil {
				return opschat.AgentReply{}, err
			}
			data = ""
			frames++
			if frames > 4096 {
				return opschat.AgentReply{}, opsResponseLimit()
			}
		} else if strings.HasPrefix(line, "data:") {
			data += strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ") + "\n"
			if len(data) > opsEnvelopeBytes {
				return opschat.AgentReply{}, opsResponseLimit()
			}
		}
		if acc.done {
			break
		}
	}
	if !acc.done && data != "" {
		if err := acc.frame(strings.TrimSuffix(data, "\n")); err != nil {
			return opschat.AgentReply{}, err
		}
	}
	if reader.N <= 0 || errors.Is(scanner.Err(), bufio.ErrTooLong) {
		return opschat.AgentReply{}, opsResponseLimit()
	}
	if err := scanner.Err(); err != nil {
		return opschat.AgentReply{}, opsTransportFailure(context.Background(), err)
	}
	if !acc.done || !acc.finishOK {
		return opschat.AgentReply{}, opsFailure("incomplete_stream", 0)
	}
	reply := opschat.AgentReply{Text: acc.text.String()}
	for i := range acc.calls {
		reply.ToolCalls = append(reply.ToolCalls, opschat.AgentToolCall{
			ID:      acc.calls[i].ID,
			ToolID:  acc.calls[i].ToolID,
			Target:  acc.calls[i].Target,
			Purpose: acc.calls[i].Purpose,
			Reason:  acc.calls[i].Reason,
			Args:    json.RawMessage(acc.args[i].String()),
		})
	}
	return reply, nil
}

func (a *agentStreamAccum) frame(data string) error {
	if data == "[DONE]" {
		a.done = true
		return nil
	}
	if !utf8.ValidString(data) {
		return opsFailure("invalid_response", 0)
	}
	if a.provider == ProviderClaude {
		return a.claudeFrame(data)
	}
	return a.openaiFrame(data)
}

func (a *agentStreamAccum) openaiFrame(data string) error {
	var event struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return opsFailure("invalid_response", 0)
	}
	for _, choice := range event.Choices {
		if choice.FinishReason != nil {
			if err := opsStopFailure(*choice.FinishReason, "stop", "tool_calls"); err != nil {
				return err
			}
			a.finishOK = true
		}
		a.text.WriteString(choice.Delta.Content)
		for _, tc := range choice.Delta.ToolCalls {
			for len(a.calls) <= tc.Index {
				a.calls = append(a.calls, opschat.AgentToolCall{})
				a.args = append(a.args, strings.Builder{})
			}
			call := &a.calls[tc.Index]
			if tc.ID != "" {
				call.ID = tc.ID
			}
			if tc.Function.Name != "" {
				call.ToolID = tc.Function.Name
			}
			a.args[tc.Index].WriteString(tc.Function.Arguments)
		}
	}
	return nil
}

func (a *agentStreamAccum) claudeFrame(data string) error {
	var event struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			StopReason  string `json:"stop_reason"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return opsFailure("invalid_response", 0)
	}
	switch event.Type {
	case "error":
		return opsFailure("rejected", 0)
	case "content_block_start":
		if event.ContentBlock.Type == "tool_use" {
			a.calls = append(a.calls, opschat.AgentToolCall{ID: event.ContentBlock.ID, ToolID: event.ContentBlock.Name})
			a.args = append(a.args, strings.Builder{})
			a.kind[event.Index] = len(a.calls) // n → call n-1
			return nil
		}
		a.kind[event.Index] = 0 // text
	case "content_block_delta":
		switch event.Delta.Type {
		case "text_delta":
			a.text.WriteString(event.Delta.Text)
		case "input_json_delta":
			if n := a.kind[event.Index]; n > 0 {
				a.args[n-1].WriteString(event.Delta.PartialJSON)
			}
		}
	case "message_delta":
		if event.Delta.StopReason != "" {
			if err := opsStopFailure(event.Delta.StopReason, "end_turn", "stop_sequence", "tool_use"); err != nil {
				return err
			}
			a.finishOK = true
		}
	case "message_stop":
		a.done = true
	}
	return nil
}
