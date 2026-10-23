package opschat

// 项目维度会话/工具回路测试:fake ProjectBackend + fakeModel 走完整 Agent 循环
// (含 run_trigger 确认→放行→效果校验),并覆盖目录组装、参数校验与上下文键。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

type fakeProjects struct {
	mu        sync.Mutex
	snapshots map[string]ProjectSnapshot
	detail    map[string]string
	triggered int
	cancelled []string
	failNext  string // 非空:下一个同名方法返回错误
}

func projectsBackend(ids ...string) *fakeProjects {
	f := &fakeProjects{snapshots: map[string]ProjectSnapshot{}, detail: map[string]string{}}
	for _, id := range ids {
		f.snapshots[id] = ProjectSnapshot{ID: id, Name: "proj-" + id[:8], RepoURL: "https://git.example/acme/app.git", DefaultBranch: "main", PacEnabled: true}
		f.detail[id] = `{"runId":"` + id + `","status":"queued"}`
	}
	return f
}
func (f *fakeProjects) Snapshot(ctx context.Context, projectID string) (ProjectSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.snapshots[projectID]
	if !ok {
		return s, ErrNotFound
	}
	return s, nil
}
func (f *fakeProjects) Info(ctx context.Context, projectID string) (string, error) {
	return `{"name":"demo","defaultBranch":"main","pacEnabled":true}`, nil
}
func (f *fakeProjects) CodeTree(ctx context.Context, projectID, ref, path string) (string, error) {
	return `{"entries":[{"name":"main.go","type":"file"}]}`, nil
}
func (f *fakeProjects) CodeRead(ctx context.Context, projectID, ref, path string, lines int) (string, error) {
	return `{"path":"` + path + `","content":"package main"}`, nil
}
func (f *fakeProjects) PipelineConfig(ctx context.Context, projectID string) (string, error) {
	return `{"yaml":"stages: []","status":"draft"}`, nil
}
func (f *fakeProjects) RepoConfig(ctx context.Context, projectID, branch string) (string, error) {
	return `{"found":true,"valid":true,"pacEnabled":true}`, nil
}
func (f *fakeProjects) Runs(ctx context.Context, projectID, status string, limit int) (string, error) {
	return `{"total":1,"runs":[{"runId":"` + uuid.NewString() + `","status":"failed"}]}`, nil
}
func (f *fakeProjects) RunDetail(ctx context.Context, runID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.detail[runID]; ok {
		return d, nil
	}
	return "", ErrNotFound
}
func (f *fakeProjects) RunLogs(ctx context.Context, runID string, stepOrdinal, lines int) (string, error) {
	return "1 2026-01-01T00:00:00Z stdout build ok", nil
}
func (f *fakeProjects) TriggerRun(ctx context.Context, projectID, branch string, params map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext == "trigger" {
		return "", ErrUnavailable
	}
	f.triggered++
	id := uuid.NewString()
	f.detail[id] = `{"runId":"` + id + `","status":"queued"}`
	return `{"runId":"` + id + `","status":"queued","branch":"main"}`, nil
}
func (f *fakeProjects) CancelRun(ctx context.Context, runID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext == "cancel" {
		return "", ErrUnavailable
	}
	f.cancelled = append(f.cancelled, runID)
	f.detail[runID] = `{"runId":"` + runID + `","status":"failed"}`
	return `{"runId":"` + runID + `","status":"failed"}`, nil
}

func projectFixture(t *testing.T, serverIDs []string, fp ProjectBackend, m Model) *Service {
	t.Helper()
	db := storetest.OpenDB(t)
	key := &[32]byte{1}
	v := vault.New(db, key)
	masker := mask.NewMasker()
	s, err := New(Options{DB: db, Vault: v, Executor: executor(serverIDs...), Model: m, Projects: fp, LocalRecorder: audit.New(db, masker, nil), Masker: masker})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}

func projectChat(t *testing.T, s *Service, projectID string, serverIDs ...string) *Session {
	t.Helper()
	c, e := s.Create(context.Background(), CreateInput{Scope: ScopeProject, ProjectID: projectID, ServerIDs: serverIDs, Context: "projects"})
	if e != nil {
		t.Fatal(e)
	}
	return c
}

// projectPlan 给 fakeModel 入队一个项目工具调用计划(项目工具不带 target)。
func projectPlan(m *fakeModel, toolID, args string) {
	call := AgentToolCall{ID: uuid.NewString(), ToolID: toolID, Args: json.RawMessage(args)}
	if projectMutation(toolID) || mutation(toolID) {
		call.Purpose = "test purpose"
		call.Reason = "test reason"
	}
	m.mu.Lock()
	m.queue = append(m.queue, AgentReply{ToolCalls: []AgentToolCall{call}})
	m.mu.Unlock()
}

func TestProjectCatalogScoping(t *testing.T) {
	server := Tools(ScopeServer)
	if len(server) != 16 {
		t.Fatalf("server catalog changed: %d", len(server))
	}
	for _, tool := range server {
		if isWebTool(tool.ID) {
			continue
		}
		if !tool.Targeted || tool.Description == "" {
			t.Fatalf("machine tool missing targeting metadata: %+v", tool)
		}
		if isProjectTool(tool.ID) {
			t.Fatalf("project tool leaked into server catalog: %s", tool.ID)
		}
	}
	project := Tools(ScopeProject)
	if len(project) != 26 {
		t.Fatalf("project catalog size: %d", len(project))
	}
	// toolsForRun 是 Service 方法(web 可用性注入);无 web 后端 = 目录不含网页工具。
	svc := &Service{}
	// 无关联机器:只给项目工具(机器工具全部剔除)。
	bare := svc.toolsForRun(runRecord{Body: runBody{Project: &ProjectSnapshot{ID: uuid.NewString()}}})
	if len(bare) != 10 {
		t.Fatalf("bare project run catalog size: %d", len(bare))
	}
	for _, tool := range bare {
		if tool.Targeted {
			t.Fatalf("project tool should not take target: %s", tool.ID)
		}
	}
	// 关联机器:项目工具 + 机器工具。
	attached := svc.toolsForRun(runRecord{Body: runBody{Project: &ProjectSnapshot{ID: uuid.NewString()}, Targets: []target.ConnectionSnapshot{{Server: target.Server{ID: uuid.NewString(), Name: "node"}}}}})
	if len(attached) != 24 {
		t.Fatalf("attached project run catalog size: %d", len(attached))
	}
}

func TestParseProjectArgsValidation(t *testing.T) {
	runID := uuid.NewString()
	cases := []struct {
		name  string
		id    string
		raw   string
		valid bool
	}{
		{"tree-root", "code_tree", `{}`, true},
		{"tree-path", "code_tree", `{"path":"cmd/server","ref":"release/1.2"}`, true},
		{"tree-parent", "code_tree", `{"path":"../etc"}`, false},
		{"tree-absolute", "code_tree", `{"path":"/etc/passwd"}`, false},
		{"tree-backslash", "code_tree", `{"path":"a\\b"}`, false},
		{"tree-dot-segment", "code_tree", `{"path":"a/./b"}`, false},
		{"tree-bad-ref", "code_tree", `{"ref":"main;rm -rf"}`, false},
		{"read-missing-path", "code_read", `{"ref":"main"}`, false},
		{"read-explicit-zero-lines", "code_read", `{"path":"a.go","lines":0}`, false},
		{"read-default-lines", "code_read", `{"path":"a.go"}`, true},
		{"read-lines-over", "code_read", `{"path":"a.go","lines":1001}`, false},
		{"runs-bad-status", "project_runs", `{"status":"exploded"}`, false},
		{"runs-explicit-zero-limit", "project_runs", `{"limit":0}`, false},
		{"runs-ok", "project_runs", `{"status":"failed","limit":5}`, true},
		{"detail-bad-id", "run_detail", `{"runId":"not-a-uuid"}`, false},
		{"detail-ok", "run_detail", `{"runId":"` + runID + `"}`, true},
		{"logs-ordinal-negative2", "run_logs", `{"runId":"` + runID + `","stepOrdinal":-2}`, false},
		{"logs-ordinal-ok", "run_logs", `{"runId":"` + runID + `","stepOrdinal":3,"lines":50}`, true},
		{"trigger-ok", "run_trigger", `{"branch":"feat/x","params":{"env":"staging"}}`, true},
		{"trigger-bad-branch", "run_trigger", `{"branch":"-Doutput"}`, false},
		{"trigger-too-many-params", "run_trigger", `{"params":{"k1":"v","k2":"v","k3":"v","k4":"v","k5":"v","k6":"v","k7":"v","k8":"v","k9":"v","k10":"v","k11":"v","k12":"v","k13":"v","k14":"v","k15":"v","k16":"v","k17":"v"}}`, false},
		{"cancel-ok", "run_cancel", `{"runId":"` + runID + `"}`, true},
		{"cancel-extra-field", "run_cancel", `{"runId":"` + runID + `","force":true}`, false},
		{"machine-tool-id", "run_cancel", `{"container":"x"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProjectArgs(tc.id, json.RawMessage(tc.raw))
			if tc.valid && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !tc.valid && err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestProjectSessionRequiresBackendAndProject(t *testing.T) {
	pid := uuid.NewString()
	// 无后端:项目会话创建被拒。
	s, _, _ := fixture(t, executor(), model())
	if _, err := s.Create(context.Background(), CreateInput{Scope: ScopeProject, ProjectID: pid}); err != ErrInvalid {
		t.Fatalf("expected ErrInvalid without backend, got %v", err)
	}
	// 有后端但项目不存在:同样被拒。
	svc := projectFixture(t, nil, projectsBackend(uuid.NewString()), model())
	if _, err := svc.Create(context.Background(), CreateInput{Scope: ScopeProject, ProjectID: pid}); err != ErrInvalid {
		t.Fatalf("expected ErrInvalid for unknown project, got %v", err)
	}
}

func TestProjectToolRoundTrip(t *testing.T) {
	pid := uuid.NewString()
	fp := projectsBackend(pid)
	m := model()
	s := projectFixture(t, nil, fp, m)
	c := projectChat(t, s, pid)
	projectPlan(m, "project_runs", `{"status":"failed","limit":5}`)
	r, err := s.Submit(context.Background(), c.ID, TurnInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, Text: "最近失败的运行有哪些?"})
	if err != nil {
		t.Fatal(err)
	}
	snap := waitRun(t, s, c.ID, r.ID, Succeeded, Failed)
	if snap.Runs[0].Status != Succeeded {
		t.Fatalf("run status: %+v", snap.Runs)
	}
	if len(snap.Calls) != 1 || snap.Calls[0].Status != Succeeded {
		t.Fatalf("project call not executed: %+v", snap.Calls)
	}
	call := snap.Calls[0]
	if call.ServerID != pid || call.ServerName != "proj-"+pid[:8] || !strings.Contains(call.Output, "failed") {
		t.Fatalf("project call projection: %+v", call)
	}
	// 模型侧:项目系统提示 + 无 target 枚举 + 项目目录(无机器工具)。
	m.mu.Lock()
	round := m.rounds[0]
	m.mu.Unlock()
	if round.System != projectSystemPrompt || len(round.Targets) != 0 {
		t.Fatalf("project round shaping failed: targets=%v", round.Targets)
	}
	seen := map[string]bool{}
	for _, tool := range round.Tools {
		seen[tool.ID] = true
	}
	if !seen["project_runs"] || seen["docker_containers"] {
		t.Fatalf("bare project catalog leaked machine tools: %v", round.Tools)
	}
}

func TestProjectTriggerRunsThroughConfirmation(t *testing.T) {
	pid := uuid.NewString()
	fp := projectsBackend(pid)
	m := model()
	s := projectFixture(t, nil, fp, m)
	c := projectChat(t, s, pid)
	projectPlan(m, "run_trigger", `{"branch":"main","params":{"env":"staging"}}`)
	r, err := s.Submit(context.Background(), c.ID, TurnInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, Text: "触发一次构建"})
	if err != nil {
		t.Fatal(err)
	}
	snap := waitRun(t, s, c.ID, r.ID, AwaitingConfirmation)
	if len(snap.Confirmations) != 1 || len(snap.Confirmations[0].Calls) != 1 {
		t.Fatalf("confirmation missing: %+v", snap.Confirmations)
	}
	fp.mu.Lock()
	triggered := fp.triggered
	fp.mu.Unlock()
	if triggered != 0 {
		t.Fatal("mutation executed before confirmation")
	}
	a := snap.Confirmations[0]
	if _, err := s.Confirm(context.Background(), c.ID, ConfirmInput{RunID: r.ID, Nonce: a.Nonce, Calls: a.Calls}); err != nil {
		t.Fatal(err)
	}
	final := waitRun(t, s, c.ID, r.ID, Succeeded, Failed)
	if final.Runs[0].Status != Succeeded {
		t.Fatalf("confirmed trigger run status: %+v", final.Runs)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.triggered != 1 {
		t.Fatalf("trigger executed %d times", fp.triggered)
	}
	for _, call := range final.Calls {
		if call.ToolID == "run_trigger" && (call.Status != Succeeded || !strings.Contains(call.Output, "runId")) {
			t.Fatalf("trigger call result: %+v", call)
		}
	}
}

func TestProjectMachineToolsUseShiftedAliases(t *testing.T) {
	pid := uuid.NewString()
	srv := uuid.NewString()
	fp := projectsBackend(pid)
	m := model()
	s := projectFixture(t, []string{srv}, fp, m)
	c := projectChat(t, s, pid, srv)
	// 机器工具在项目会话中必须用 target_1(target_0 是项目,非法)。
	m.mu.Lock()
	m.queue = append(m.queue, AgentReply{ToolCalls: []AgentToolCall{{ID: uuid.NewString(), ToolID: "docker_inspect", Target: "target_1", Args: json.RawMessage(`{"container":"postgres"}`)}}})
	m.mu.Unlock()
	r, err := s.Submit(context.Background(), c.ID, TurnInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, Text: "看下容器状态"})
	if err != nil {
		t.Fatal(err)
	}
	snap := waitRun(t, s, c.ID, r.ID, Succeeded, Failed, AwaitingContinuation)
	if snap.Runs[0].Status != Succeeded {
		t.Fatalf("mixed project run: %+v", snap.Runs)
	}
	for _, call := range snap.Calls {
		if call.ToolID == "docker_inspect" && call.Status != Succeeded {
			t.Fatalf("machine call in project session: %+v", call)
		}
	}
	// 模型侧 target 枚举只含机器别名(target_1),不含 target_0。
	m.mu.Lock()
	round := m.rounds[0]
	m.mu.Unlock()
	if len(round.Targets) != 1 || round.Targets[0] != "target_1" {
		t.Fatalf("project machine aliases: %v", round.Targets)
	}
}

func TestProjectContextKeys(t *testing.T) {
	pid := uuid.NewString()
	s := projectFixture(t, nil, projectsBackend(pid), model())
	ctx := context.Background()
	if err := s.validateContext(ctx, "projects"); err != nil {
		t.Fatal(err)
	}
	if err := s.validateContext(ctx, "project:"+pid); err != nil {
		t.Fatal(err)
	}
	if err := s.validateContext(ctx, "project:"+uuid.NewString()); err != ErrInvalid {
		t.Fatalf("unknown project context: %v", err)
	}
	if err := s.validateContext(ctx, "project:not-a-uuid"); err != ErrInvalid {
		t.Fatalf("non-uuid project context: %v", err)
	}
}

func TestProjectToolRejectsMachineSessionAndTarget(t *testing.T) {
	id := uuid.NewString()
	f := executor(id)
	m := model()
	s, _, _ := fixture(t, f, m)
	c := chat(t, s, id)
	// 机器会话里模型提出项目工具 → 整轮 invalid_plan;自纠后允许文本终答,
	// 但绝不执行任何调用。
	m.mu.Lock()
	m.queue = append(m.queue, AgentReply{ToolCalls: []AgentToolCall{{ID: uuid.NewString(), ToolID: "project_runs", Args: json.RawMessage(`{}`)}}})
	m.mu.Unlock()
	r, err := s.Submit(context.Background(), c.ID, TurnInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, Text: "看下运行"})
	if err != nil {
		t.Fatal(err)
	}
	snap := waitRun(t, s, c.ID, r.ID, Succeeded, Failed, Unknown, PartialFailed)
	if len(snap.Calls) != 0 {
		t.Fatalf("project tool executed in a server session: %+v", snap.Calls)
	}
	f.mu.Lock()
	invoked := len(f.invocations)
	f.mu.Unlock()
	if invoked != 0 {
		t.Fatalf("server was touched: %d invocations", invoked)
	}
	// 项目会话里项目工具显式带 target_1 → invalid_plan,同样不执行。
	pid := uuid.NewString()
	mm := model()
	svc := projectFixture(t, nil, projectsBackend(pid), mm)
	pc := projectChat(t, svc, pid)
	mm.mu.Lock()
	mm.queue = append(mm.queue, AgentReply{ToolCalls: []AgentToolCall{{ID: uuid.NewString(), ToolID: "project_runs", Target: "target_1", Args: json.RawMessage(`{}`)}}})
	mm.mu.Unlock()
	pr, err := svc.Submit(context.Background(), pc.ID, TurnInput{ClientRequestID: uuid.NewString(), Revision: pc.Revision, Text: "看下运行"})
	if err != nil {
		t.Fatal(err)
	}
	psnap := waitRun(t, svc, pc.ID, pr.ID, Succeeded, Failed, Unknown, PartialFailed)
	if len(psnap.Calls) != 0 {
		t.Fatalf("explicit target on project tool was executed: %+v", psnap.Calls)
	}
}
