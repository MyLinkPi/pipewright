package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/opschat"
)

var opsTestProvider opschat.ProviderInfo

func opsRequest() opschat.AgentRequest {
	return opschat.AgentRequest{History: []opschat.Message{{Role: "user", Text: "list containers"}}, Targets: []string{"target_0"}, MaxResponseBytes: 32 << 10, Provider: opsTestProvider}
}

func TestOpsProvidersPrivacyAndStrictPlan(t *testing.T) {
	for _, provider := range []string{ProviderClaude, ProviderOpenAI} {
		t.Run(provider, func(t *testing.T) {
			key := "provider-secret-key"
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				if provider == ProviderOpenAI && r.Header.Get("Authorization") != "Bearer "+key {
					t.Error("missing auth")
				}
				if provider == ProviderClaude && (r.Header.Get("x-api-key") != key || r.URL.Path != "/v1/messages") {
					t.Error("claude protocol")
				}
				raw, _ := io.ReadAll(r.Body)
				if strings.Contains(string(raw), key) || strings.Contains(string(raw), "malicious-host") {
					t.Error("prompt leaked key or caller catalogue")
				}
				var body struct {
					Messages []struct{ Content string } `json:"messages"`
					Tools    []struct {
						Function struct {
							Parameters json.RawMessage `json:"parameters"`
						} `json:"function"`
						InputSchema json.RawMessage `json:"input_schema"`
					} `json:"tools"`
				}
				_ = json.Unmarshal(raw, &body)
				hasTarget := false
				for _, t := range body.Tools {
					if strings.Contains(string(t.Function.Parameters), "target_0") || strings.Contains(string(t.InputSchema), "target_0") {
						hasTarget = true
					}
				}
				if len(body.Messages) == 0 || !strings.Contains(string(raw), "target_0") || !hasTarget {
					t.Error("bad prompt")
				}
				var payload []byte
				switch provider {
				case ProviderClaude:
					payload, _ = json.Marshal(map[string]any{"content": []any{
						map[string]any{"type": "text", "text": "[MASKED]"},
						map[string]any{"type": "tool_use", "id": "call_1", "name": "docker_containers", "input": map[string]any{"target": "target_0"}},
					}})
				default:
					payload, _ = json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
						"content":    "[MASKED]",
						"tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "docker_containers", "arguments": `{"target":"target_0"}`}}},
					}}}})
				}
				_, _ = w.Write(payload)
			}))
			defer server.Close()
			svc, _, _ := newService(t, server.Client())
			base := server.URL + "/v1"
			if _, err := svc.Save(ctx(), SaveInput{Provider: provider, BaseURL: base, Model: "test-model", APIKey: &key, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			masker := mask.NewMasker()
			m := NewOpsModel(svc, masker)
			opsTestProvider = m.Info()
			info := m.Info()
			if count.Load() != 0 || info.Provider != provider || len(info.ConfigHash) != 64 {
				t.Fatal(info, count.Load())
			}
			in := opsRequest()
			in.Question = "list containers " + key
			in.Tools = opschat.Tools(opschat.ScopeServer)
			reply, err := m.AgentRound(ctx(), in)
			if err != nil || reply.Text != "[MASKED]" || len(reply.ToolCalls) != 1 || count.Load() != 1 {
				t.Fatal(reply, err, count.Load())
			}
		})
	}
}

type opsTransport func(*http.Request) (*http.Response, error)

func (f opsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func opsResponse(text string) *http.Response {
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": text}}}})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header)}
}

func TestOpsAnalysisFrozenConfigRotationAndDisabledRegistration(t *testing.T) {
	key := "old-secret-key"
	var calls atomic.Int32
	svc, db, _ := newService(t, &http.Client{})
	base := "http://127.0.0.1:12345"
	in := SaveInput{Provider: ProviderOpenAI, BaseURL: base, Model: "m", APIKey: &key, Enabled: true}
	if _, err := svc.Save(ctx(), in); err != nil {
		t.Fatal(err)
	}
	masker := mask.NewMasker()
	m := NewOpsModel(svc, masker)
	if _, err := db.Exec("UPDATE ai_config SET updated_at=? WHERE id=1", "2026-10-07T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	old := m.Info()
	newKey := "new-secret-key"
	in.APIKey = &newKey
	if _, err := svc.Save(ctx(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE ai_config SET updated_at=? WHERE id=1", "2026-10-07T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if m.Info().ConfigHash == old.ConfigHash {
		t.Fatal("rotation not detected")
	}
	svc.(*service).client.Transport = opsTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+newKey || r.URL.Host != "127.0.0.1:12345" {
			t.Error("not using approved snapshot")
		}
		// Change the database after snapshot capture: this request must not reload it.
		if _, err := db.Exec("UPDATE ai_config SET base_url=?,model=? WHERE id=1", "http://127.0.0.1:22222", "changed"); err != nil {
			t.Error(err)
		}
		return opsResponse(newKey), nil
	})
	request := opschat.AnalysisRequest{Provider: old, Items: []opschat.AnalysisItem{{CallID: "c", Output: "approved output"}}, MaxResponseBytes: 16 << 10}
	if _, err := m.Analyze(ctx(), request); err != opschat.ErrConsent || calls.Load() != 0 {
		t.Fatal(err, calls.Load())
	}
	request.Provider = m.Info()
	text, err := m.Analyze(ctx(), request)
	if err != nil || text != "[MASKED]" || calls.Load() != 1 {
		t.Fatal(text, err)
	}
	in.Enabled = false
	if _, err := svc.Save(ctx(), in); err != nil {
		t.Fatal(err)
	}
	if err := m.(opschat.SecretRegistrar).RegisterSecrets(ctx()); err != nil || masker.Scrub(newKey) != "[MASKED]" {
		t.Fatal(err)
	}
	if m.Info() != (opschat.ProviderInfo{}) {
		t.Fatal("disabled provider reported available")
	}
	freshMasker := mask.NewMasker()
	if err := NewOpsModel(svc, freshMasker).(opschat.SecretRegistrar).RegisterSecrets(ctx()); err != nil || freshMasker.Scrub(newKey) != "[MASKED]" {
		t.Fatal("disabled key not registered", err)
	}
}

func TestOpsRedirectDoesNotForwardDataOrCredentials(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1); w.WriteHeader(200) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer source.Close()
	client := source.Client()
	svc, _, _ := newService(t, client)
	key := "secret-redirect-key"
	if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: source.URL, Model: "m", APIKey: &key, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	m := NewOpsModel(svc, mask.NewMasker())
	opsTestProvider = m.Info()
	if _, err := m.AgentRound(ctx(), opsRequest()); !errors.Is(err, ErrGenerateFailed) || destinationCalls.Load() != 0 || client.CheckRedirect != nil {
		t.Fatal(err, destinationCalls.Load())
	}
}

func TestOpsOnlyModelDeadlinePreservesLegacyClient(t *testing.T) {
	short := false
	client := &http.Client{Timeout: 8 * time.Second, Transport: opsTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("missing model deadline")
		}
		remaining := time.Until(deadline)
		if !short && (remaining < 50*time.Second || remaining > 130*time.Second) {
			t.Fatal("model deadline", remaining)
		}
		return opsResponse(`{"text":"ok","actions":[]}`), nil
	})}
	svc, _, _ := newService(t, client)
	key := "model-key"
	if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: "http://127.0.0.1", Model: "m", APIKey: &key, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	m := NewOpsModel(svc, mask.NewMasker())
	opsTestProvider = m.Info()
	if _, err := m.AgentRound(ctx(), opsRequest()); err != nil {
		t.Fatal(err)
	}
	short = true
	shortCtx, cancel := context.WithTimeout(ctx(), time.Second)
	defer cancel()
	if _, err := m.AgentRound(shortCtx, opsRequest()); err != nil {
		t.Fatal(err)
	}
	if client.Timeout != 8*time.Second {
		t.Fatal("legacy timeout changed")
	}
}

func TestOpsAddressAndPostMaskBounds(t *testing.T) {
	for _, address := range []string{"file:///tmp/secret", "http://169.254.169.254", "http://0.0.0.0", "http://[fe80::1]", "http://user:pass@127.0.0.1", "http://127.0.0.1?key=secret", "http://127.0.0.1#fragment"} {
		if opsURLAllowed(ctx(), address) {
			t.Fatal(address)
		}
	}
	svc, _, _ := newService(t, &http.Client{Transport: opsTransport(func(*http.Request) (*http.Response, error) { return opsResponse(strings.Repeat("abcd", 100)), nil })})
	key := "abcd"
	if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: "http://127.0.0.1", Model: "m", APIKey: &key, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	m := NewOpsModel(svc, mask.NewMasker()).(*opsModel)
	cfg, secret, _, err := m.snapshot(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.chat(ctx(), cfg, secret, "prompt", 500); !errors.Is(err, ErrGenerateFailed) {
		t.Fatal("post-mask expanded output accepted", err)
	}
}

func TestOpsAnalysisEscapedInputAndInstructionQuota(t *testing.T) {
	svc, _, _ := newService(t, &http.Client{Transport: opsTransport(func(*http.Request) (*http.Response, error) { t.Fatal("oversize data sent"); return nil, nil })})
	if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI, APIKey: strp("sk-x"), Model: "m", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	m := NewOpsModel(svc, mask.NewMasker())
	opsTestProvider = m.Info()
	in := opschat.AnalysisRequest{Provider: m.Info(), Items: []opschat.AnalysisItem{{Output: strings.Repeat("\x00", 6000)}}, MaxResponseBytes: 16 << 10}
	if _, err := m.Analyze(ctx(), in); err != opschat.ErrQuota {
		t.Fatal(err)
	}
	in.Items[0].Output = ""
	raw, _ := json.Marshal(in.Items)
	in.Items[0].Output = strings.Repeat("a", (32<<10)-len(raw))
	if _, err := m.Analyze(ctx(), in); err != opschat.ErrQuota {
		t.Fatal("instruction overhead", err)
	}
}

type opsBrokenReader struct{}

func (opsBrokenReader) Read([]byte) (int, error) { return 0, errors.New("raw key and URL") }
func (opsBrokenReader) Close() error             { return nil }

func TestOpsEnvelopeOutputBoundsAndSanitizedFailures(t *testing.T) {
	for _, mode := range []string{"envelope", "read", "status", "output", "redirect", "transport", "limit"} {
		t.Run(mode, func(t *testing.T) {
			svc, _, _ := newService(t, &http.Client{Transport: opsTransport(func(*http.Request) (*http.Response, error) {
				switch mode {
				case "envelope":
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("a", opsEnvelopeBytes+1)))}, nil
				case "read":
					return &http.Response{StatusCode: 200, Body: opsBrokenReader{}}, nil
				case "status", "redirect":
					return &http.Response{StatusCode: 302, Body: io.NopCloser(strings.NewReader("secret"))}, nil
				case "transport":
					return nil, errors.New("raw secret http://host")
				case "limit":
					return opsResponse(`{"text":"longer than limit","actions":[]}`), nil
				default:
					return opsResponse(strings.Repeat("a", (32<<10)+1)), nil
				}
			})})
			key := "secret-key"
			if _, err := svc.Save(ctx(), SaveInput{Provider: ProviderOpenAI, BaseURL: "http://127.0.0.1:12345", Model: "m", APIKey: &key, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			m := NewOpsModel(svc, mask.NewMasker())
			opsTestProvider = m.Info()
			in := opsRequest()
			if mode == "limit" {
				in.MaxResponseBytes = 8
			}
			_, err := m.AgentRound(context.Background(), in)
			if !errors.Is(err, ErrGenerateFailed) {
				t.Fatal(err)
			}
		})
	}
}
