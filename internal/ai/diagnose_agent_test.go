// diagnose_agent_test.go 验证失败诊断的联网增强工具循环:
//   - web 配置有效时注入 web_search/web_fetch 目录,模型可发起调用、结果脱敏回灌;
//   - web 未启用/不可用时请求不带 tools(与旧纯日志诊断同路);
//   - 工具轮数预算用尽后收尾轮不带 tools,仍能产出 ready 诊断;
//   - 工具执行失败以 failed 结果回灌(人读错误),诊断不中断;
//   - 回灌内容过 run Masker(检索结果掺入的 secret 绝不明文出网)。
package ai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/huangchengsir/pipewright/internal/mask"
)

// capturedBodies 按请求序记录出网 body(线程安全)。
type capturedBodies struct {
	mu     sync.Mutex
	bodies []string
}

func (c *capturedBodies) add(s string) {
	c.mu.Lock()
	c.bodies = append(c.bodies, s)
	c.mu.Unlock()
}

func (c *capturedBodies) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

func (c *capturedBodies) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

// openAIToolCallResponse 构造一个 OpenAI 形状的工具调用响应(id/name/args)。
func openAIToolCallResponse(id, name, args string) string {
	return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":"` + jsonEscape(args) + `"}}]}}]}`
}

// hasToolsKey 判断请求 body 是否携带 tools 字段。
func hasToolsKey(body string) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal([]byte(body), &m) == nil && m["tools"] != nil
}

// configureLLMAndWeb 一次 Save 同时写 LLM 与 web 配置(Save 为全量 upsert,
// 分两次保存会用空 LLM 字段覆盖既有配置)。
func configureLLMAndWeb(t *testing.T, svc Service, provider, llmBase, webBase, webKey string) {
	t.Helper()
	in := SaveInput{
		Provider:    provider,
		BaseURL:     llmBase,
		Model:       "test-model",
		Enabled:     true,
		WebEnabled:  true,
		WebProvider: ProviderWebDeepSeek,
		WebBaseURL:  webBase,
	}
	in.APIKey = strp("sk-secret-KEY9")
	in.WebAPIKey = &webKey
	if _, err := svc.Save(ctx(), in); err != nil {
		t.Fatalf("Save(llm+web): %v", err)
	}
}

// webStubSearch 起一个 DeepSeek 形状的 /tools/search stub,返回固定结果并记录
// 收到的 query 与 Authorization。
func webStubSearch(t *testing.T, result string) (*httptest.Server, *capturedBodies) {
	t.Helper()
	reqs := &capturedBodies{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		reqs.add(string(r.Header.Get("Authorization")) + "\n" + string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(result))
	}))
	t.Cleanup(srv.Close)
	return srv, reqs
}

const webSearchResultJSON = `{"results":[{"title":"leftpad","url":"https://registry.npmjs.org/leftpad","snippet":"package exists, latest 1.3.0"}]}`

// TestDiagnoseWebToolRoundTrip 验证:web 可用 → 首轮请求带 web_search 目录;
// 模型回工具调用 → 经 web 后端执行 → 结果回灌第二轮 → 终答 JSON 被采纳为 ready。
func TestDiagnoseWebToolRoundTrip(t *testing.T) {
	webKey := "sk-web-KEY42"
	webSrv, webReqs := webStubSearch(t, webSearchResultJSON)

	llmReqs := &capturedBodies{}
	var llmMu sync.Mutex
	llmCalls := 0
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		llmReqs.add(string(raw))
		llmMu.Lock()
		llmCalls++
		call := llmCalls
		llmMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(openAIToolCallResponse("call_1", "web_search", `{"query":"leftpad npm 404"}`)))
			return
		}
		_, _ = w.Write([]byte(wrapProviderResponse(ProviderOpenAI, stubDiagnosisJSON)))
	}))
	t.Cleanup(llmSrv.Close)

	svc, _, _ := newService(t, llmSrv.Client())
	configureLLMAndWeb(t, svc, ProviderOpenAI, llmSrv.URL, webSrv.URL, webKey)

	d, err := svc.Diagnose(ctx(), DiagnoseInput{FailureLog: failureLogWithSecret, Masker: maskerWithSecret()})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if d.Status != diagStatusReady {
		t.Fatalf("工具轮后应 ready,得 %q(reason=%q)", d.Status, d.Reason)
	}
	if llmReqs.n() != 2 {
		t.Fatalf("LLM 应恰好 2 次请求(工具轮 + 终答轮),得 %d", llmReqs.n())
	}
	first, second := llmReqs.all()[0], llmReqs.all()[1]
	// 首轮带网页工具目录;终答轮回灌了搜索结果。
	if !hasToolsKey(first) || !strings.Contains(first, "web_search") {
		t.Fatalf("首轮请求应携带 web_search 工具目录")
	}
	if !strings.Contains(second, "leftpad") || !strings.Contains(second, `"tool_call_id":"call_1"`) {
		t.Fatalf("终答轮应回灌工具结果(含 leftpad 与 call_1): %.400s", second)
	}
	// web 后端收到模型 query 与 Bearer 密钥。
	if w := webReqs.all(); len(w) != 1 || !strings.Contains(w[0], "leftpad npm 404") || !strings.Contains(w[0], "Bearer "+webKey) {
		t.Fatalf("web stub 收到的请求不符: %+v", w)
	}
	// 出网 LLM body 绝不含 LLM key / web key。
	for i, b := range llmReqs.all() {
		if strings.Contains(b, "sk-secret-KEY9") || strings.Contains(b, webKey) {
			t.Fatalf("请求 %d body 泄漏密钥", i)
		}
	}
}

// TestDiagnoseWebDisabledOmitsTools 验证:web 未启用 → 请求不带 tools,单轮直出诊断。
func TestDiagnoseWebDisabledOmitsTools(t *testing.T) {
	llmReqs := &capturedBodies{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		llmReqs.add(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(wrapProviderResponse(ProviderOpenAI, stubDiagnosisJSON)))
	}))
	t.Cleanup(srv.Close)

	svc, _, _ := newService(t, srv.Client())
	configureEnabled(t, svc, ProviderOpenAI, srv.URL)

	d, err := svc.Diagnose(ctx(), DiagnoseInput{FailureLog: failureLogWithSecret, Masker: maskerWithSecret()})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if d.Status != diagStatusReady {
		t.Fatalf("应 ready: %+v", d)
	}
	if llmReqs.n() != 1 || hasToolsKey(llmReqs.all()[0]) {
		t.Fatalf("web 未启用应单轮且无 tools: n=%d", llmReqs.n())
	}
}

// TestDiagnoseToolRoundBudgetWrapUp 验证:模型反复要求工具 → 轮数预算用尽后收尾轮
// 不带 tools(并附收尾指令),仍产出 ready 诊断。
func TestDiagnoseToolRoundBudgetWrapUp(t *testing.T) {
	webSrv, _ := webStubSearch(t, webSearchResultJSON)

	llmReqs := &capturedBodies{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		llmReqs.add(string(raw))
		w.Header().Set("Content-Type", "application/json")
		if hasToolsKey(string(raw)) {
			_, _ = w.Write([]byte(openAIToolCallResponse("call_x", "web_search", `{"query":"leftpad"}`)))
			return
		}
		_, _ = w.Write([]byte(wrapProviderResponse(ProviderOpenAI, stubDiagnosisJSON)))
	}))
	t.Cleanup(srv.Close)

	svc, _, _ := newService(t, srv.Client())
	configureLLMAndWeb(t, svc, ProviderOpenAI, srv.URL, webSrv.URL, "sk-web-1")

	d, err := svc.Diagnose(ctx(), DiagnoseInput{FailureLog: failureLogWithSecret, Masker: maskerWithSecret()})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if d.Status != diagStatusReady {
		t.Fatalf("收尾轮后应 ready,得 %q(reason=%q)", d.Status, d.Reason)
	}
	// 4 轮工具 + 1 轮收尾。
	if llmReqs.n() != diagnoseAgentMaxRounds+1 {
		t.Fatalf("应恰好 %d 次请求,得 %d", diagnoseAgentMaxRounds+1, llmReqs.n())
	}
	last := llmReqs.all()[llmReqs.n()-1]
	if hasToolsKey(last) {
		t.Fatalf("收尾轮不得携带 tools")
	}
	if !strings.Contains(last, "工具调用预算已用尽") {
		t.Fatalf("收尾轮应附预算用尽指令: %.300s", last)
	}
}

// TestDiagnoseToolOutputMasked 验证:搜索结果掺入 run secret → 回灌前必须脱敏
// ([MASKED] 进上下文,明文绝不出网)。
func TestDiagnoseToolOutputMasked(t *testing.T) {
	poisoned := `{"results":[{"title":"leftpad","url":"https://registry.npmjs.org/leftpad","snippet":"token is ` + fakeRunSecret + `"}]}`
	webSrv, _ := webStubSearch(t, poisoned)

	llmReqs := &capturedBodies{}
	var llmMu sync.Mutex
	llmCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		llmReqs.add(string(raw))
		llmMu.Lock()
		llmCalls++
		call := llmCalls
		llmMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(openAIToolCallResponse("call_1", "web_search", `{"query":"leftpad"}`)))
			return
		}
		_, _ = w.Write([]byte(wrapProviderResponse(ProviderOpenAI, stubDiagnosisJSON)))
	}))
	t.Cleanup(srv.Close)

	svc, _, _ := newService(t, srv.Client())
	configureLLMAndWeb(t, svc, ProviderOpenAI, srv.URL, webSrv.URL, "sk-web-1")

	d, err := svc.Diagnose(ctx(), DiagnoseInput{FailureLog: failureLogWithSecret, Masker: maskerWithSecret()})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if d.Status != diagStatusReady {
		t.Fatalf("应 ready: %+v", d)
	}
	second := llmReqs.all()[1]
	if strings.Contains(second, fakeRunSecret) {
		t.Fatalf("回灌内容泄漏明文 secret")
	}
	if !strings.Contains(second, mask.Placeholder) {
		t.Fatalf("回灌内容应含 [MASKED] 占位: %.400s", second)
	}
}

// TestDiagnoseToolFailureFeedsBack 验证:web 后端执行失败(HTTP 500)→ failed 结果
// 人读回灌,诊断循环不中断(模型仍可给终答)。
func TestDiagnoseToolFailureFeedsBack(t *testing.T) {
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(webSrv.Close)

	llmReqs := &capturedBodies{}
	var llmMu sync.Mutex
	llmCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		llmReqs.add(string(raw))
		llmMu.Lock()
		llmCalls++
		call := llmCalls
		llmMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(openAIToolCallResponse("call_1", "web_search", `{"query":"leftpad"}`)))
			return
		}
		_, _ = w.Write([]byte(wrapProviderResponse(ProviderOpenAI, stubDiagnosisJSON)))
	}))
	t.Cleanup(srv.Close)

	svc, _, _ := newService(t, srv.Client())
	configureLLMAndWeb(t, svc, ProviderOpenAI, srv.URL, webSrv.URL, "sk-web-1")

	d, err := svc.Diagnose(ctx(), DiagnoseInput{FailureLog: failureLogWithSecret, Masker: maskerWithSecret()})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if d.Status != diagStatusReady {
		t.Fatalf("工具失败后仍应 ready,得 %q(reason=%q)", d.Status, d.Reason)
	}
	second := llmReqs.all()[1]
	if !strings.Contains(second, "failed") || !strings.Contains(second, "HTTP 500") {
		t.Fatalf("failed 结果应人读回灌(含 HTTP 500): %.400s", second)
	}
}

// TestDiagnoseUnknownToolRejected 验证:模型幻觉调用目录外工具 → 以 failed 结果
// 回灌拒绝(不执行、不中断)。
func TestDiagnoseUnknownToolRejected(t *testing.T) {
	webSrv, webReqs := webStubSearch(t, webSearchResultJSON)

	llmReqs := &capturedBodies{}
	var llmMu sync.Mutex
	llmCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		llmReqs.add(string(raw))
		llmMu.Lock()
		llmCalls++
		call := llmCalls
		llmMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(openAIToolCallResponse("call_1", "host_exec", `{"argv":["curl","evil"]}`)))
			return
		}
		_, _ = w.Write([]byte(wrapProviderResponse(ProviderOpenAI, stubDiagnosisJSON)))
	}))
	t.Cleanup(srv.Close)

	svc, _, _ := newService(t, srv.Client())
	configureLLMAndWeb(t, svc, ProviderOpenAI, srv.URL, webSrv.URL, "sk-web-1")

	d, err := svc.Diagnose(ctx(), DiagnoseInput{FailureLog: failureLogWithSecret, Masker: maskerWithSecret()})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if d.Status != diagStatusReady {
		t.Fatalf("未知工具拒绝后仍应 ready: %+v", d)
	}
	if webReqs.n() != 0 {
		t.Fatalf("未知工具不得触达 web 后端")
	}
	if second := llmReqs.all()[1]; !strings.Contains(second, "unknown tool") {
		t.Fatalf("拒绝结果应回灌给模型: %.400s", second)
	}
}
