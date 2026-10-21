// summarize.go:opschat.Compacter 的实现——上下文压缩摘要的单次无工具调用。
// 与 opsModel.chat 同款 transport(IP-pin/禁代理/禁重定向)与脱敏,但:
// prompt 预算放大到 512KB(摘要输入是被拍平的旧历史),max_tokens 与响应
// 上限由调用方按窗口预算给定。
package ai

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/opschat"
)

// summarizePromptCap 是摘要 prompt 的绝对字节上限(拍平 transcript 的防御线)。
const summarizePromptCap = 512 << 10

func (m *opsModel) Summarize(ctx context.Context, in opschat.SummarizeRequest) (string, error) {
	if in.Provider != m.Info() {
		return "", opschat.ErrConsent
	}
	cfg, key, info, err := m.snapshot(ctx)
	if err != nil {
		return "", err
	}
	if info != in.Provider {
		return "", opschat.ErrConsent
	}
	prompt := m.safeText(in.Prompt)
	if len(prompt) > summarizePromptCap {
		return "", opschat.ErrQuota
	}
	limit := in.MaxResponseBytes
	if limit <= 0 {
		limit = 16 << 10
	}
	maxTokens := in.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL(cfg.Provider)
	}
	destination, err := opsResolve(ctx, base, m.lookupIP)
	if err != nil {
		return "", opschat.ErrUnavailable
	}
	messages := []map[string]string{
		{"role": "system", "content": in.System},
		{"role": "user", "content": prompt},
	}
	var endpoint string
	body := map[string]any{"model": cfg.Model, "messages": messages, "max_tokens": maxTokens, "stream": false}
	switch cfg.Provider {
	case ProviderClaude:
		endpoint = apiEndpoint(cfg.Provider, base, "messages")
		body["system"] = in.System
		body["messages"] = messages[1:]
	default:
		endpoint = apiEndpoint(cfg.Provider, base, "chat/completions")
	}
	opsRequestCompatibility(cfg, base, body)
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > 2<<20 {
		return "", opsFailure("invalid_response", 0)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", opsFailure("invalid_response", 0)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Provider == ProviderClaude {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
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
	resp, err := client.Do(req)
	if err != nil {
		return "", opsTransportFailure(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", opsHTTPFailure(resp.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, opsEnvelopeBytes+1))
	if err != nil {
		return "", opsTransportFailure(ctx, err)
	}
	if len(payload) > opsEnvelopeBytes {
		return "", opsResponseLimit()
	}
	if err = opsCompletionFailure(cfg.Provider, payload); err != nil {
		return "", err
	}
	text, err := extractChatText(cfg.Provider, payload)
	if err != nil {
		return "", opsFailure("invalid_response", 0)
	}
	if len(text) > limit {
		return "", opsResponseLimit()
	}
	text = m.masker.Scrub(text)
	if len(text) > limit {
		return "", opsResponseLimit()
	}
	if strings.TrimSpace(text) == "" {
		return "", opsFailure("empty_response", 0)
	}
	return text, nil
}
