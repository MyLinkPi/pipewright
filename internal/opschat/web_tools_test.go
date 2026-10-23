package opschat

// 网页搜索/抓取工具测试:参数校验、目录可见性(按 WebBackend 可用性过滤)、
// planner 校验(target 必须省略、调用视图字段)与执行路径(fake 后端)。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/target"
)

func TestParseWebArgsValidation(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		raw   string
		valid bool
	}{
		{"search-ok", "web_search", `{"query":"nginx upstream error","count":5}`, true},
		{"search-default-count", "web_search", `{"query":"docker"}`, true},
		{"search-explicit-zero-count", "web_search", `{"query":"docker","count":0}`, false},
		{"search-count-over", "web_search", `{"query":"docker","count":11}`, false},
		{"search-empty-query", "web_search", `{"query":""}`, false},
		{"search-blank-query", "web_search", `{"query":"   "}`, false},
		{"search-control-query", "web_search", "{\"query\":\"a\nb\"}", false},
		{"search-extra-key", "web_search", `{"query":"a","url":"https://x"}`, false},
		{"fetch-ok", "web_fetch", `{"url":"https://example.com/docs?a=1"}`, true},
		{"fetch-http", "web_fetch", `{"url":"http://example.com/docs"}`, true},
		{"fetch-no-scheme", "web_fetch", `{"url":"example.com/docs"}`, false},
		{"fetch-userinfo", "web_fetch", `{"url":"https://user:pw@example.com/"}`, false},
		{"fetch-fragment", "web_fetch", `{"url":"https://example.com/#sec"}`, false},
		{"fetch-space", "web_fetch", `{"url":"https://example.com/a b"}`, false},
		{"fetch-empty", "web_fetch", `{"url":""}`, false},
		{"unknown-id", "web_grab", `{}`, false},
	}
	for _, tc := range cases {
		args, err := parseWebArgs(tc.id, json.RawMessage(tc.raw))
		if tc.valid != (err == nil) {
			t.Fatalf("%s: valid=%v err=%v", tc.name, tc.valid, err)
		}
		if tc.valid && tc.id == "web_search" && args.Count == 0 {
			t.Fatalf("%s: default count must resolve to 5", tc.name)
		}
	}
}

// fakeWeb 是测试用 WebBackend:可切换可用性,回放固定输出。
type fakeWeb struct {
	available bool
	searchOut string
	fetchOut  string
	calls     []string
}

func (f *fakeWeb) Available(context.Context) bool { return f.available }
func (f *fakeWeb) Search(_ context.Context, query string, count int) (string, error) {
	f.calls = append(f.calls, "search:"+query)
	return f.searchOut, nil
}
func (f *fakeWeb) Fetch(_ context.Context, rawURL string) (string, error) {
	f.calls = append(f.calls, "fetch:"+rawURL)
	return f.fetchOut, nil
}

func TestWebCatalogAvailability(t *testing.T) {
	on := &Service{web: &fakeWeb{available: true}}
	off := &Service{web: &fakeWeb{available: false}}
	bare := &Service{}
	server := runRecord{Body: runBody{Targets: []target.ConnectionSnapshot{{Server: target.Server{ID: uuid.NewString(), Name: "n"}}}}}
	project := runRecord{Body: runBody{Project: &ProjectSnapshot{ID: uuid.NewString()}, Targets: []target.ConnectionSnapshot{{Server: target.Server{ID: uuid.NewString(), Name: "n"}}}}}
	for name, r := range map[string]runRecord{"server": server, "project": project} {
		hasWeb := func(list []Tool) bool {
			for _, tool := range list {
				if isWebTool(tool.ID) {
					return true
				}
			}
			return false
		}
		if !hasWeb(on.toolsForRun(r)) {
			t.Fatalf("%s scope: web tools missing when available", name)
		}
		if hasWeb(off.toolsForRun(r)) || hasWeb(bare.toolsForRun(r)) {
			t.Fatalf("%s scope: web tools must be hidden when unavailable", name)
		}
	}
	// 目录层(对外 GET /tools)始终含网页工具,与项目工具口径一致。
	if !isWebTool("web_search") || !isWebTool("web_fetch") {
		t.Fatal("web tool ids")
	}
	for _, tool := range webTools() {
		if tool.Targeted || tool.Mutation {
			t.Fatalf("web tools must be non-targeted read-only: %+v", tool)
		}
	}
}

func webRunRecord(m *fakeModel) runRecord {
	return runRecord{View: Run{ID: uuid.NewString(), SessionID: uuid.NewString()},
		Body: runBody{Provider: m.Info(), Targets: []target.ConnectionSnapshot{{Server: target.Server{ID: uuid.NewString(), Name: "node"}}}}}
}

func TestAgentCallsWebToolValidation(t *testing.T) {
	s, _, _ := fixture(t, executor(uuid.NewString()), model())
	m := model()
	r := webRunRecord(m)
	call := func(target string) AgentReply {
		c := AgentToolCall{ID: "w1", ToolID: "web_search", Args: json.RawMessage(`{"query":"pipewright docs","count":3}`)}
		c.Target = target
		return AgentReply{ToolCalls: []AgentToolCall{c}}
	}
	// target 省略:合法;ServerID 空、TargetHash=轮次提供方指纹。
	candidates, err := s.agentCalls(r, call(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates: %d", len(candidates))
	}
	v := candidates[0].Body.View
	if v.ServerID != "" || v.ServerName != "" || v.Object != "pipewright docs" {
		t.Fatalf("web call view: %+v", v)
	}
	if v.TargetHash != m.Info().ConfigHash {
		t.Fatalf("web call must fingerprint the provider config")
	}
	if v.Args.Count != 3 {
		t.Fatalf("count: %+v", v.Args)
	}
	// 任何 target(含 target_0)都拒绝:网页工具不绑定目标。
	if _, err = s.agentCalls(r, call("target_0")); err == nil || !strings.Contains(err.Error(), "omit target") {
		t.Fatalf("target_0 must be rejected: %v", err)
	}
	if _, err = s.agentCalls(r, call("target_1")); err == nil || !strings.Contains(err.Error(), "omit target") {
		t.Fatalf("target_1 must be rejected: %v", err)
	}
}

func TestWebToolRoundTrip(t *testing.T) {
	srv := uuid.NewString()
	f := executor(srv)
	m := model()
	s, _, _ := fixture(t, f, m)
	web := &fakeWeb{available: true, searchOut: `{"results":[{"title":"docs","url":"https://example.com"}]}`, fetchOut: "# Example\n\ncontent"}
	s.web = web
	c := chat(t, s, srv)

	// 1) web_search:模型计划(无 target)→ 执行成功 → 输出过 scrub 落库。
	m.mu.Lock()
	m.queue = append(m.queue, AgentReply{ToolCalls: []AgentToolCall{{ID: "w1", ToolID: "web_search", Args: json.RawMessage(`{"query":"pipewright docs","count":3}`)}}})
	m.mu.Unlock()
	r := submitTool(t, s, c, "web_search")
	snap := waitRun(t, s, c.ID, r.ID, Succeeded)
	if len(snap.Calls) != 1 || snap.Calls[0].ToolID != "web_search" {
		t.Fatalf("calls: %+v", snap.Calls)
	}
	call := snap.Calls[0]
	if call.Status != Succeeded || !strings.Contains(call.Output, "pipewright") && !strings.Contains(call.Output, "docs") {
		t.Fatalf("search call: %+v", call)
	}
	if call.ServerID != "" || call.ServerName != "" {
		t.Fatalf("web call must not bind a server: %+v", call)
	}

	// 2) web_fetch:同轮成功路径。
	m.mu.Lock()
	m.queue = append(m.queue, AgentReply{ToolCalls: []AgentToolCall{{ID: "w2", ToolID: "web_fetch", Args: json.RawMessage(`{"url":"https://example.com/intro"}`)}}})
	m.mu.Unlock()
	c2, e := s.Get(context.Background(), c.ID)
	if e != nil {
		t.Fatal(e)
	}
	r2 := submitTool(t, s, c2, "web_fetch")
	snap2 := waitRun(t, s, c.ID, r2.ID, Succeeded)
	var fetch *Call
	for i := range snap2.Calls {
		if snap2.Calls[i].ID == "w2" {
			fetch = &snap2.Calls[i]
		}
	}
	if fetch == nil || fetch.ToolID != "web_fetch" || !strings.Contains(fetch.Output, "# Example") {
		t.Fatalf("fetch call: %+v", snap2.Calls)
	}
	if len(web.calls) != 2 || web.calls[1] != "fetch:https://example.com/intro" {
		t.Fatalf("web backend calls: %v", web.calls)
	}
}

// driftModel 在返回工具计划的同时轮换 Info 指纹,模拟「轮次开始后 AI 配置变更」。
type driftModel struct {
	*fakeModel
}

func (d *driftModel) AgentRound(ctx context.Context, in AgentRequest) (AgentReply, error) {
	reply, err := d.fakeModel.AgentRound(ctx, in)
	if err == nil && len(reply.ToolCalls) > 0 {
		d.mu.Lock()
		d.info = ProviderInfo{Provider: "fake", Model: "test", ConfigHash: strings.Repeat("1", 64)}
		d.mu.Unlock()
	}
	return reply, err
}

func waitCall(t *testing.T, s *Service, session, callID string) Call {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := s.Snapshot(context.Background(), session)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range snap.Calls {
			if c.ID == callID && terminal(c.Status) {
				return c
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("call did not settle")
	return Call{}
}

func TestWebToolExecutionGuards(t *testing.T) {
	srv := uuid.NewString()

	// 1) 后端未装配(nil)→ 调用明确失败,不悬挂。
	m1 := model()
	s1, _, _ := fixture(t, executor(srv), m1)
	s1.web = nil
	c1 := chat(t, s1, srv)
	m1.mu.Lock()
	m1.queue = append(m1.queue, AgentReply{ToolCalls: []AgentToolCall{{ID: "g1", ToolID: "web_fetch", Args: json.RawMessage(`{"url":"https://example.com/x"}`)}}})
	m1.mu.Unlock()
	r1 := submitTool(t, s1, c1, "web_fetch")
	waitRun(t, s1, c1.ID, r1.ID, Succeeded, Failed)
	call := waitCall(t, s1, c1.ID, "g1")
	if call.Status != Failed || !strings.Contains(call.Error, "web tools backend unavailable") {
		t.Fatalf("nil backend call: %+v", call)
	}

	// 2) 提供方指纹漂移(轮次开始后配置变更)→ fail-closed。
	s2, _, _ := fixture(t, executor(srv), model())
	s2.web = &fakeWeb{available: true}
	c2 := chat(t, s2, srv)
	m2 := &driftModel{fakeModel: model()}
	// fixture 绑定的是 model();替换服务内模型引用。
	s2.model = m2
	m2.mu.Lock()
	m2.queue = append(m2.queue, AgentReply{ToolCalls: []AgentToolCall{{ID: "g2", ToolID: "web_search", Args: json.RawMessage(`{"query":"q"}`)}}})
	m2.mu.Unlock()
	r2 := submitTool(t, s2, c2, "web_search")
	waitRun(t, s2, c2.ID, r2.ID, Succeeded, Failed)
	call2 := waitCall(t, s2, c2.ID, "g2")
	if call2.Status != Failed || !strings.Contains(call2.Error, "target configuration changed") {
		t.Fatalf("drifted config call: %+v", call2)
	}
}
