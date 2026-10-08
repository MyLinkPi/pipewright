package deploy

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// seedResumeRun 插入项目种子 + 一个恢复运行行(resume 两列非空),返回 run id。
func seedResumeRun(t *testing.T, db *sql.DB) string {
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
	runID := uuid.NewString()
	if _, err := db.Exec(
		`INSERT INTO pipeline_runs
		   (id, project_id, status, trigger_type, trigger_branch, trigger_commit, trigger_actor,
		    resolved_environment, resolved_target_server_ids, created_at, started_at, finished_at,
		    resume_of_run_id, resume_plan_json)
		 VALUES (?, ?, 'failed', 'manual', 'main', 'abc', 'admin', '', '[]', ?, ?, ?, 'parent', '["run","retry"]')`,
		runID, projID, now, now, now); err != nil {
		t.Fatalf("seed resume run: %v", err)
	}
	return runID
}

// seedTargetRow 直插一条部署目标行。
func seedTargetRow(t *testing.T, db *sql.DB, runID, serverID, name, status, msg string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO deploy_targets (id, run_id, server_id, server_name, status, message, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), runID, serverID, name, status, msg, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed deploy target: %v", err)
	}
}

// TestDeployForStageResumeIncremental 验证恢复运行的部署节点增量语义:已成功机器跳过不重部署,
// 只重试失败机器;持久化逐目标 upsert(成功机器的继承行原状保留)。
func TestDeployForStageResumeIncremental(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	execCount := 0
	tgt := &stubTarget{execFn: func(serverID string, _ []string) (*target.ExecResult, error) {
		execCount++
		return &target.ExecResult{ExitCode: 0, Stdout: "ok"}, nil
	}}
	srvA := seedLabeledServer(t, tgt, "web-a", "pool=web")
	srvB := seedLabeledServer(t, tgt, "web-b", "pool=web")

	runID := seedResumeRun(t, db)
	seedTargetRow(t, db, runID, srvA.ID, "web-a", run.TargetSuccess, "继承的成功行")
	seedTargetRow(t, db, runID, srvB.ID, "web-b", run.TargetFailed, "上次失败")

	res, err := New(tgt, rsvc).DeployForStage(context.Background(), runID, "pool=web",
		map[string]string{"deployMode": "command", "restartCommand": "echo hi"}, "")
	if err != nil {
		t.Fatalf("DeployForStage: %v", err)
	}
	if execCount != 1 {
		t.Fatalf("只应部署失败的那台机器,实际执行 %d 次", execCount)
	}
	if len(res) != 1 || res[0].ServerID != srvB.ID || res[0].Status != run.TargetSuccess {
		t.Fatalf("结果应只含重试成功的 srvB:%+v", res)
	}
	// srvA 继承行原状(消息未改写),srvB 行被重试结果覆盖为成功。
	targets, _ := rsvc.ListDeployTargets(context.Background(), runID)
	byServer := map[string]run.DeployTarget{}
	for _, tt := range targets {
		byServer[tt.ServerID] = tt
	}
	if byServer[srvA.ID].Message != "继承的成功行" {
		t.Fatalf("已成功机器的继承行应原状保留:%+v", byServer[srvA.ID])
	}
	if byServer[srvB.ID].Status != run.TargetSuccess {
		t.Fatalf("失败机器行应被重试结果覆盖为成功:%+v", byServer[srvB.ID])
	}
}

// TestDeployForStageResumeAllSucceededSkips 验证恢复运行全部目标机已成功 → 不执行、节点放行、
// 继承行不落库改写。
func TestDeployForStageResumeAllSucceededSkips(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	execCount := 0
	tgt := &stubTarget{execFn: func(string, []string) (*target.ExecResult, error) {
		execCount++
		return &target.ExecResult{ExitCode: 0}, nil
	}}
	srvA := seedLabeledServer(t, tgt, "web-a", "pool=web")
	srvB := seedLabeledServer(t, tgt, "web-b", "pool=web")

	runID := seedResumeRun(t, db)
	seedTargetRow(t, db, runID, srvA.ID, "web-a", run.TargetSuccess, "A 继承")
	seedTargetRow(t, db, runID, srvB.ID, "web-b", run.TargetSuccess, "B 继承")

	res, err := New(tgt, rsvc).DeployForStage(context.Background(), runID, "pool=web",
		map[string]string{"deployMode": "command", "restartCommand": "echo hi"}, "")
	if err != nil {
		t.Fatalf("DeployForStage: %v", err)
	}
	if execCount != 0 {
		t.Fatalf("全部已成功不应执行任何部署命令,实际 %d 次", execCount)
	}
	if len(res) != 2 {
		t.Fatalf("应返回逐机跳过结果供日志展示:%+v", res)
	}
	for _, r := range res {
		if r.Status != run.TargetSuccess || !strings.Contains(r.Message, "跳过") {
			t.Fatalf("逐机结果应为跳过语义:%+v", r)
		}
	}
}

// TestDeployForStageNormalRunNotIncremental 验证普通运行(非恢复)行为不变:即使 targets 里
// 已有成功行,也照常全量部署(整批 save)。
func TestDeployForStageNormalRunNotIncremental(t *testing.T) {
	db := testDB(t)
	rsvc := run.New(db)
	execCount := 0
	tgt := &stubTarget{execFn: func(string, []string) (*target.ExecResult, error) {
		execCount++
		return &target.ExecResult{ExitCode: 0}, nil
	}}
	srvA := seedLabeledServer(t, tgt, "web-a", "pool=web")
	seedLabeledServer(t, tgt, "web-b", "pool=web")

	runID := seedResumeRun(t, db)
	// 清空 resume 列 → 普通运行。
	if _, err := db.Exec(`UPDATE pipeline_runs SET resume_of_run_id = '', resume_plan_json = '' WHERE id = ?`, runID); err != nil {
		t.Fatalf("clear resume cols: %v", err)
	}
	seedTargetRow(t, db, runID, srvA.ID, "web-a", run.TargetSuccess, "旧行")

	if _, err := New(tgt, rsvc).DeployForStage(context.Background(), runID, "pool=web",
		map[string]string{"deployMode": "command", "restartCommand": "echo hi"}, ""); err != nil {
		t.Fatalf("DeployForStage: %v", err)
	}
	if execCount != 2 {
		t.Fatalf("普通运行应对全部目标机部署,实际执行 %d 次", execCount)
	}
}
