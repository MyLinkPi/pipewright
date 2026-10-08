package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// resumeSpec 返回与种子步骤对齐的流水线配置:构建(lint,build)→ 部署(deploy)。
// 布局 ordinal:0=lint/构建、1=build/构建、2=deploy/部署。
func resumeSpec() *pipeline.Config {
	return &pipeline.Config{Spec: pipeline.Spec{Stages: []pipeline.Stage{
		{ID: "s1", Name: "构建", Jobs: []pipeline.Job{
			{ID: "j1", Name: "lint", Type: "script"},
			{ID: "j2", Name: "build", Type: "script"},
		}},
		{ID: "s2", Name: "部署", Needs: []string{"s1"}, Jobs: []pipeline.Job{
			{ID: "j3", Name: "deploy", Type: "deploy_container"},
		}},
	}}}
}

// setupResumeServer 构造带固定 spec loader 的测试 server(SQL 直种,不经 worker pool,
// 派生运行稳定停在 queued 便于断言),返回 (srv, client, csrf, runSvc, db)。
func setupResumeServer(t *testing.T) (*httptest.Server, *http.Client, string, run.Service, *sql.DB) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	psvc := project.New(st.DB, v, stubProber{branch: "main"})
	rsvc := run.New(st.DB)

	srv := httptest.NewServer(New(testWebFSAuth(), svc,
		WithVault(v), WithProjects(psvc), WithRuns(rsvc, nil),
		WithSpecLoader(dagrun.SpecLoaderFunc(func(context.Context, string) (*pipeline.Config, error) { return resumeSpec(), nil }))))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	return srv, client, csrf, rsvc, st.DB
}

// seedResumeProject 直种一个项目,返回项目 id。
func seedResumeProject(t *testing.T, db *sql.DB) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	credID := uuid.NewString()
	if _, err := db.Exec(
		`INSERT INTO credentials (id, name, type, scope, ciphertext, masked_value, created_at, updated_at)
		 VALUES (?, 'c', 'git_token', '', X'00', 'm', ?, ?)`, credID, now, now); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	projID := uuid.NewString()
	if _, err := db.Exec(
		`INSERT INTO projects (id, name, repo_url, default_branch, credential_id, created_at, updated_at)
		 VALUES (?, 'acme', 'https://example.com/p.git', 'main', ?, ?, ?)`, projID, credID, now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return projID
}

// seedResumeParentRun 直种父运行(可指定状态)+ 与 resumeSpec() 布局对齐的步骤,返回 run id。
// steps 为空时按 [lint=success, build=failed, deploy=failed] 种(布局 ordinal 0..2)。
func seedResumeParentRun(t *testing.T, db *sql.DB, projID, status string) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	runID := uuid.NewString()
	if _, err := db.Exec(
		`INSERT INTO pipeline_runs
		   (id, project_id, status, trigger_type, trigger_branch, trigger_commit, trigger_actor, created_at, started_at, finished_at)
		 VALUES (?, ?, ?, 'manual', 'main', 'abc1234', 'ci', ?, ?, ?)`,
		runID, projID, status, now, now, now); err != nil {
		t.Fatalf("seed parent run: %v", err)
	}
	steps := []struct{ name, stage, status string }{
		{"lint", "构建", run.StepSuccess},
		{"build", "构建", run.StepFailed},
		{"deploy", "部署", run.StepFailed},
	}
	for i, st := range steps {
		if _, err := db.Exec(
			`INSERT INTO run_steps (id, run_id, name, stage, status, ordinal, started_at, finished_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), runID, st.name, st.stage, st.status, i, now, now); err != nil {
			t.Fatalf("seed step: %v", err)
		}
	}
	return runID
}

// TestResumeRunEndpoint 走通 201:失败父运行 + 合法处置 → 派生运行(queued、resumedFrom、ordinal)。
func TestResumeRunEndpoint(t *testing.T) {
	srv, client, csrf, rsvc, db := setupResumeServer(t)
	projID := seedResumeProject(t, db)
	parentID := seedResumeParentRun(t, db, projID, run.StatusFailed)

	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+parentID+"/resume", csrf,
		`{"actions":{"1":"retry","2":"skip"}}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("应 201,得到 %d:%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["status"] != run.StatusQueued {
		t.Fatalf("派生运行应 queued:%s", raw)
	}
	if out["resumedFrom"] != parentID {
		t.Fatalf("resumedFrom 应指向父运行:%s", raw)
	}
	childID, _ := out["id"].(string)
	if childID == "" {
		t.Fatalf("缺子运行 id:%s", raw)
	}
	// 子运行的步骤要等 worker 执行时才由 runner 计划,201 响应里 steps 为空属预期;
	// ordinal 字段(恢复计划的前端依据)在**父运行**详情上验证。
	presp := doJSON(t, client, http.MethodGet, srv.URL+"/api/runs/"+parentID, csrf, "")
	defer presp.Body.Close()
	praw, _ := io.ReadAll(presp.Body)
	var parent map[string]any
	if err := json.Unmarshal(praw, &parent); err != nil {
		t.Fatalf("decode parent: %v", err)
	}
	psteps, _ := parent["steps"].([]any)
	if len(psteps) != 3 {
		t.Fatalf("父运行应声明 3 个步骤:%s", praw)
	}
	first, _ := psteps[0].(map[string]any)
	if first["ordinal"] != float64(0) || first["name"] != "lint" {
		t.Fatalf("步骤应带 ordinal/名称:%+v", first)
	}
	// 子运行可经 GET 读回,恢复计划已持久化。
	got, err := rsvc.Get(context.Background(), childID)
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if got.Resume == nil || len(got.Resume.Actions) != 3 || got.Resume.Actions[1] != run.NodeRetry || got.Resume.Actions[2] != run.NodeSkip {
		t.Fatalf("子运行恢复计划应持久化:%+v", got.Resume)
	}
	if got.Trigger.Type != run.TriggerManual || got.Trigger.Commit != "abc1234" || got.Trigger.Actor != "admin" {
		t.Fatalf("触发上下文应复制自父运行(type=manual/commit/actor=admin):%+v", got.Trigger)
	}
}

// TestResumeRunEndpointErrors 验证错误映射:非失败运行 409 / 配置不一致 409 / 缺 actions 422 /
// 缺失败节点处置 422 / 父运行不存在 404。
func TestResumeRunEndpointErrors(t *testing.T) {
	srv, client, csrf, _, db := setupResumeServer(t)
	projID := seedResumeProject(t, db)

	post := func(runID, body string) (int, string) {
		resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+runID+"/resume", csrf, body)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	// 成功态父运行 → 409 run_not_resumable(先种成功运行,步骤全 success)。
	okID := seedResumeParentRun(t, db, projID, run.StatusSuccess)
	if _, err := db.Exec(`UPDATE run_steps SET status = ? WHERE run_id = ?`, run.StepSuccess, okID); err != nil {
		t.Fatalf("flip steps: %v", err)
	}
	if code, raw := post(okID, `{"actions":{"1":"retry","2":"skip"}}`); code != http.StatusConflict || !strings.Contains(raw, "run_not_resumable") {
		t.Fatalf("成功运行应 409 run_not_resumable,得到 %d %s", code, raw)
	}

	// 失败父运行但步骤名与当前 spec 不一致 → 409 spec_changed。
	changedID := seedResumeParentRun(t, db, projID, run.StatusFailed)
	if _, err := db.Exec(`UPDATE run_steps SET name = 'renamed' WHERE run_id = ? AND ordinal = 1`, changedID); err != nil {
		t.Fatalf("rename step: %v", err)
	}
	if code, raw := post(changedID, `{"actions":{"1":"retry","2":"skip"}}`); code != http.StatusConflict || !strings.Contains(raw, "spec_changed") {
		t.Fatalf("配置不一致应 409 spec_changed,得到 %d %s", code, raw)
	}

	// 失败节点缺处置 → 422。
	badID := seedResumeParentRun(t, db, projID, run.StatusFailed)
	if code, raw := post(badID, `{"actions":{"1":"retry"}}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("缺失败节点处置应 422,得到 %d %s", code, raw)
	}

	// 空 actions → 422;不存在的运行 → 404。
	if code, _ := post(badID, `{}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("缺 actions 应 422,得到 %d", code)
	}
	if code, raw := post("nope", `{"actions":{"0":"retry"}}`); code != http.StatusNotFound || !strings.Contains(raw, "run_not_found") {
		t.Fatalf("父运行不存在应 404,得到 %d %s", code, raw)
	}
}
