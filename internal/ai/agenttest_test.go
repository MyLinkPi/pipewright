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
		"正文为空":     {"a.md": []byte("---\nid: x\n---\n   \n")},
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
