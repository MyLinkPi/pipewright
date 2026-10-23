package ai

// agenttest_test.go 覆盖 AI 测试领域纯逻辑:需求解析(frontmatter 可省/缺省值/重复 id/
// 空正文)、needs 拓扑排序(环/缺引用)、裁决契约解析(工具参数/自由文本兜底)与
// HTTP 动作集(httptest 观察/方法校验/URL 防线)。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/opschat"
)

func TestParseAITestRequirementsDefaults(t *testing.T) {
	files := map[string][]byte{
		".pipewright/tests/requirements/web/checkout.md": []byte("登录测试账号,加购任一商品并结算。"),
	}
	reqs, err := ParseAITestRequirements(files)
	if err != nil {
		t.Fatalf("裸正文应通过:%v", err)
	}
	r := reqs[0]
	if r.ID != "web-checkout" {
		t.Fatalf("缺省 id 应为路径 slug,得到 %q", r.ID)
	}
	if r.Severity != AITestSeverityBlocker {
		t.Fatalf("缺省 severity 应为 blocker,得到 %q", r.Severity)
	}
	if r.Target != "" || len(r.Needs) != 0 {
		t.Fatalf("缺省 target/needs 应为空,得到 %+v", r)
	}
}

func TestParseAITestRequirementsFrontmatter(t *testing.T) {
	files := map[string][]byte{
		"a.md": []byte("---\nid: coupon\nseverity: suggest\ntarget: http://${BASE_URL}\nneeds: [\"login\"]\n---\n正文"),
	}
	reqs, err := ParseAITestRequirements(files)
	if err != nil {
		t.Fatalf("frontmatter 解析失败:%v", err)
	}
	r := reqs[0]
	if r.ID != "coupon" || r.Severity != AITestSeveritySuggest || r.Target != "http://${BASE_URL}" || len(r.Needs) != 1 || r.Needs[0] != "login" {
		t.Fatalf("frontmatter 字段解析不符:%+v", r)
	}
}

func TestParseAITestRequirementsFailures(t *testing.T) {
	cases := map[string]map[string][]byte{
		"显式 id 重复": {
			"a.md": []byte("---\nid: dup\n---\n正文A"),
			"b.md": []byte("---\nid: dup\n---\n正文B"),
		},
		"正文为空":           {"a.md": []byte("---\nid: x\n---\n   \n")},
		"frontmatter 非法": {"a.md": []byte("---\n{bad json\n---\n正文")},
		"severity 非法":    {"a.md": []byte("---\nseverity: fatal\n---\n正文")},
	}
	for name, files := range cases {
		if _, err := ParseAITestRequirements(files); err == nil {
			t.Fatalf("%s 应报错", name)
		}
	}
}

func TestOrderAITestRequirements(t *testing.T) {
	reqs := []AITestRequirement{
		{ID: "delete", Needs: []string{"create"}},
		{ID: "create"},
		{ID: "login"},
	}
	ordered, err := OrderAITestRequirements(reqs)
	if err != nil {
		t.Fatalf("拓扑排序失败:%v", err)
	}
	pos := map[string]int{}
	for i, r := range ordered {
		pos[r.ID] = i
	}
	if !(pos["create"] < pos["delete"]) {
		t.Fatalf("create 应在 delete 前,得到 %v", ordered)
	}
	// 环 → 报错。
	if _, err := OrderAITestRequirements([]AITestRequirement{{ID: "a", Needs: []string{"b"}}, {ID: "b", Needs: []string{"a"}}}); err == nil {
		t.Fatal("成环应报错")
	}
	// 缺引用 → 报错。
	if _, err := OrderAITestRequirements([]AITestRequirement{{ID: "a", Needs: []string{"ghost"}}}); err == nil {
		t.Fatal("缺引用应报错")
	}
}

func TestParseAITestVerdict(t *testing.T) {
	ok, err := parseAITestVerdictArgs(json.RawMessage(`{"verdict":"pass","reason":"金额立减","evidence":"95.00→89.00"}`))
	if err != nil || !ok.Pass || ok.Reason != "金额立减" {
		t.Fatalf("合法裁决解析不符:%+v err=%v", ok, err)
	}
	if _, err := parseAITestVerdictArgs(json.RawMessage(`{"verdict":"maybe","reason":"x","evidence":"y"}`)); err == nil {
		t.Fatal("verdict 非枚举应报错")
	}
	if _, err := parseAITestVerdictArgs(json.RawMessage(`{"verdict":"pass","reason":"","evidence":"y"}`)); err == nil {
		t.Fatal("reason 缺失应报错")
	}
	// 自由文本兜底:能抠出 JSON 则用。
	v := parseAITestVerdictText(`结论如下 {"verdict":"fail","reason":"无提示","evidence":"未见文案"} 以上。`)
	if v == nil || v.Pass {
		t.Fatalf("自由文本兜底应解析出 fail,得到 %+v", v)
	}
	if parseAITestVerdictText("完全无关的文本") != nil {
		t.Fatal("无 JSON 的自由文本应返回 nil")
	}
}

func TestAITestMessagesKeepRequirementWithNudge(t *testing.T) {
	// 回归:question(需求正文)必须在每一轮的消息里恒定在场,推进指令(nudge)
	// 追加在最新位置——修复前 question 被推进语覆盖,第 2 轮起模型看不到需求。
	history := []opschat.Message{{Role: "assistant", Text: "我先看看"}}
	exchanges := []opschat.AgentExchange{
		{
			Calls:   []opschat.AgentToolCall{{ID: "c1", ToolID: "http_request", Args: json.RawMessage(`{"url":"http://x"}`)}},
			Results: []opschat.AgentToolResult{{CallID: "c1", Output: "200 OK"}},
		},
	}
	openai := aiTestOpenAIMessages(aiTestSystemPrompt, "REQUIREMENT-正文", history, exchanges, "NUDGE-继续")
	if len(openai) != 6 { // system + history + question + assistant calls + tool result + nudge
		t.Fatalf("OpenAI 消息数不符:%d", len(openai))
	}
	if openai[2]["content"] != "REQUIREMENT-正文" {
		t.Fatalf("需求正文应在 history 之后恒定在场:%v", openai[2])
	}
	if openai[5]["role"] != "user" || openai[5]["content"] != "NUDGE-继续" {
		t.Fatalf("nudge 应在最新消息位置:%v", openai[5])
	}

	claude := aiTestClaudeMessages("REQUIREMENT-正文", history, exchanges, "NUDGE-继续")
	// Claude:开头 assistant 历史被丢弃;question 成首条 user;nudge 并入最后一条
	// user(tool_result)的 content 块,角色交替不被破坏。
	if claude[0]["role"] != "user" || claude[0]["content"] != "REQUIREMENT-正文" {
		t.Fatalf("Claude 首条应为需求正文 user:%v", claude[0])
	}
	last := claude[len(claude)-1]
	if last["role"] != "user" {
		t.Fatalf("Claude 末条应为 user:%v", last)
	}
	blocks, ok := last["content"].([]map[string]any)
	if !ok || blocks[len(blocks)-1]["type"] != "text" || blocks[len(blocks)-1]["text"] != "NUDGE-继续" {
		t.Fatalf("nudge 应并入最后 user 的 content 块:%v", last["content"])
	}
	// exchanges 为空时 nudge 并入 question 文本(string content 分支)。
	claude0 := aiTestClaudeMessages("REQUIREMENT-正文", nil, nil, "NUDGE-纠正")
	if len(claude0) != 1 || claude0[0]["content"] != "REQUIREMENT-正文\n\nNUDGE-纠正" {
		t.Fatalf("无轮次时 nudge 应并入 question:%v", claude0)
	}
}

func TestAITestContextBudget(t *testing.T) {
	// ceiling:窗口 − 预留(max(10%, 16K@≥32K));下限半窗;0 窗口不裁剪。
	if c := aiTestContextCeiling(200_000); c != 180_000 {
		t.Fatalf("200k 窗口阈值应为 180k(预留 max(20k,16k)),得到 %d", c)
	}
	if c := aiTestContextCeiling(4_000); c != 3_600 {
		t.Fatalf("4k 窗口阈值应为 3600,得到 %d", c)
	}
	if c := aiTestContextCeiling(0); c != 0 {
		t.Fatalf("0 窗口应不裁剪,得到 %d", c)
	}
	// 裁剪:超阈值从最旧轮整轮丢弃,至少保留最后一轮。
	big := strings.Repeat("x", 4096) // ≈1k tokens/轮
	exchanges := []opschat.AgentExchange{}
	for range 6 {
		exchanges = append(exchanges, opschat.AgentExchange{
			Calls:   []opschat.AgentToolCall{{ID: "c", ToolID: "http_request", Args: json.RawMessage(`{}`)}},
			Results: []opschat.AgentToolResult{{CallID: "c", Output: big}},
		})
	}
	kept, dropped := aiTestTrimExchanges("q", nil, exchanges, nil, 3_000)
	if dropped == 0 || len(kept) == len(exchanges) {
		t.Fatalf("超阈值应丢弃最旧轮:dropped=%d kept=%d", dropped, len(kept))
	}
	if len(kept) < 1 {
		t.Fatal("至少保留最后一轮")
	}
	// 只剩一轮时即使超阈值也不再丢(当前轮是裁决依据)。
	one := exchanges[:1]
	kept1, dropped1 := aiTestTrimExchanges("q", nil, one, nil, 1)
	if dropped1 != 0 || len(kept1) != 1 {
		t.Fatalf("单轮不应被丢:dropped=%d", dropped1)
	}
	// target ≤ 0 → 不裁剪。
	if _, d := aiTestTrimExchanges("q", nil, exchanges, nil, 0); d != 0 {
		t.Fatal("target≤0 不应裁剪")
	}
	if !strings.Contains(aiTestContextNote(3), "3 earliest tool-call round(s)") {
		t.Fatal("裁剪说明应含累计轮数")
	}
}

func TestHTTPActionsExecute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/order" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"amount": 89.0}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	h := &HTTPActions{}

	out, err := h.AITestExecute(context.Background(), "http_request",
		json.RawMessage(fmt.Sprintf(`{"url":%q}`, srv.URL+"/api/order")), func(string) {})
	if err != nil {
		t.Fatalf("GET 执行失败:%v", err)
	}
	if !strings.Contains(out, "200") || !strings.Contains(out, "89.0") {
		t.Fatalf("观察应含状态与响应体,得到:%s", out)
	}
	// 非 http/https → 拒。
	if _, err := h.AITestExecute(context.Background(), "http_request", json.RawMessage(`{"url":"file:///etc/passwd"}`), func(string) {}); err == nil {
		t.Fatal("非 http(s) URL 应被拒")
	}
	// 写方法缺 method → 拒。
	if _, err := h.AITestExecute(context.Background(), "http_write", json.RawMessage(`{"url":"http://x"}`), func(string) {}); err == nil {
		t.Fatal("http_write 无 method 应被拒")
	}
	// 工具面:读工具非变更、写工具变更(purpose 注入依据)。
	tools := h.AITestTools()
	if len(tools) != 2 || tools[0].ID != "http_request" || tools[0].Mutation || !tools[1].Mutation {
		t.Fatalf("工具面不符:%+v", tools)
	}
}
