package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedFailedRunWithDeps 直接插一个 failed 父运行 + 步骤/产物/部署目标行,返回 run id。
func seedFailedRunWithDeps(t *testing.T, svc Service, projID string) string {
	t.Helper()
	s := svc.(*service)
	now := time.Now().UTC().Format(time.RFC3339)
	runID := uuid.NewString()
	if _, err := s.db.Exec(
		`INSERT INTO pipeline_runs
		   (id, project_id, status, trigger_type, trigger_branch, trigger_commit, trigger_actor,
		    resolved_environment, resolved_target_server_ids, params_json, created_at, started_at, finished_at)
		 VALUES (?, ?, 'failed', 'webhook', 'main', 'abc1234', 'ci', 'prod', '["srv-a"]', '{"k":"v"}', ?, ?, ?)`,
		runID, projID, now, now, now); err != nil {
		t.Fatalf("seed parent run: %v", err)
	}
	// 三步骤:0 success / 1 failed / 2 skipped。
	for i, st := range []string{StepSuccess, StepFailed, StepSkipped} {
		if _, err := s.db.Exec(
			`INSERT INTO run_steps (id, run_id, name, stage, status, ordinal, started_at, finished_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), runID, "step"+string(rune('A'+i)), "阶段", st, i, now, now); err != nil {
			t.Fatalf("seed step: %v", err)
		}
	}
	if _, err := svc.AddArtifact(context.Background(), Artifact{
		RunID: runID, Type: ArtifactDist, Name: "shop", Reference: "dist/shop",
	}); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if err := svc.SaveDeployTargets(context.Background(), runID, []DeployTarget{
		{ServerID: "srv-a", ServerName: "web-a", Status: TargetSuccess, Message: "ok", StartedAt: time.Now().UTC()},
		{ServerID: "srv-b", ServerName: "web-b", Status: TargetFailed, Message: "boom", StartedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("seed deploy targets: %v", err)
	}
	return runID
}

// TestCreateResumeCopiesContextAndRows 验证派生运行:触发上下文复制(type=manual/分支/参数/
// 解析环境与目标机)、resume 两列、产物与部署目标行复制。
func TestCreateResumeCopiesContextAndRows(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)
	parentID := seedFailedRunWithDeps(t, svc, projID)

	child, err := svc.CreateResume(context.Background(), parentID, []string{NodeInherit, NodeRetry, NodeRun}, "operator")
	if err != nil {
		t.Fatalf("CreateResume: %v", err)
	}
	if child.Status != StatusQueued {
		t.Fatalf("派生运行应 queued,得到 %s", child.Status)
	}
	if child.Trigger.Type != TriggerManual {
		t.Fatalf("恢复运行触发类型应落 manual(when 条件同手动语义),得到 %s", child.Trigger.Type)
	}
	if child.Trigger.Branch != "main" || child.Trigger.Commit != "abc1234" {
		t.Fatalf("分支/commit 应复制自父运行:%+v", child.Trigger)
	}
	if child.Trigger.Params["k"] != "v" || child.Trigger.ResolvedEnvironment != "prod" {
		t.Fatalf("参数/解析环境应复制:%+v", child.Trigger)
	}
	if len(child.Trigger.ResolvedTargetServerIDs) != 1 || child.Trigger.ResolvedTargetServerIDs[0] != "srv-a" {
		t.Fatalf("目标机应复制:%v", child.Trigger.ResolvedTargetServerIDs)
	}
	if child.Trigger.Actor != "operator" {
		t.Fatalf("actor 应为恢复操作人,得到 %q", child.Trigger.Actor)
	}
	if child.ResumeOfRunID != parentID {
		t.Fatalf("resume_of_run_id 应为父运行,得到 %q", child.ResumeOfRunID)
	}
	if child.Resume == nil || len(child.Resume.Actions) != 3 ||
		child.Resume.Actions[0] != NodeInherit || child.Resume.Actions[1] != NodeRetry {
		t.Fatalf("恢复计划应回读: %+v", child.Resume)
	}

	// 产物/部署目标行复制到子运行(引用原样,便于下游恢复/增量部署)。
	arts, _ := svc.ListArtifacts(context.Background(), child.ID)
	if len(arts) != 1 || arts[0].Reference != "dist/shop" {
		t.Fatalf("父产物应复制到子运行:%+v", arts)
	}
	targets, _ := svc.ListDeployTargets(context.Background(), child.ID)
	if len(targets) != 2 {
		t.Fatalf("父部署目标应复制到子运行:%+v", targets)
	}
}

// TestCreateResumeActorFallback 未传操作人时沿用父运行 actor。
func TestCreateResumeActorFallback(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)
	parentID := seedFailedRunWithDeps(t, svc, projID)
	child, err := svc.CreateResume(context.Background(), parentID, []string{NodeInherit, NodeRetry, NodeRun}, "  ")
	if err != nil {
		t.Fatalf("CreateResume: %v", err)
	}
	if child.Trigger.Actor != "ci" {
		t.Fatalf("actor 应回退父运行 actor,得到 %q", child.Trigger.Actor)
	}
}

// TestCreateResumeGuards 校验拒绝路径:父运行缺失 / 非失败终态 / 计划非法。
func TestCreateResumeGuards(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)
	parentID := seedFailedRunWithDeps(t, svc, projID)

	if _, err := svc.CreateResume(context.Background(), "nope", []string{NodeInherit, NodeRetry, NodeRun}, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("父运行不存在应 ErrNotFound,得到 %v", err)
	}
	okRun := seedRunRow(t, svc, projID, StatusSuccess)
	if _, err := svc.CreateResume(context.Background(), okRun, []string{NodeRun}, ""); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("成功运行不可恢复,得到 %v", err)
	}
	for _, bad := range [][]string{
		nil,
		{NodeInherit, NodeInherit, NodeInherit}, // 全 inherit = 空转
		{NodeInherit, "bogus", NodeRun},
	} {
		if _, err := svc.CreateResume(context.Background(), parentID, bad, ""); !errors.Is(err, ErrInvalidResumePlan) {
			t.Fatalf("非法计划 %v 应 ErrInvalidResumePlan,得到 %v", bad, err)
		}
	}
}

// TestGetResumeCorruptPlanYieldsEmptyPlan 计划 JSON 损坏(脏数据防御):Resume 置**空计划**
// 而非 nil —— nil 会被执行器当普通运行静默全量重跑;空计划让 dagrun 因长度不符明确失败。
func TestGetResumeCorruptPlanYieldsEmptyPlan(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)
	s := svc.(*service)
	runID := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(
		`INSERT INTO pipeline_runs
		   (id, project_id, status, trigger_type, trigger_branch, trigger_actor, created_at,
		    resume_of_run_id, resume_plan_json)
		 VALUES (?, ?, 'failed', 'manual', 'main', 'admin', ?, 'parent-1', '{corrupt')`,
		runID, projID, now); err != nil {
		t.Fatalf("seed corrupt-plan run: %v", err)
	}
	got, err := svc.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ResumeOfRunID != "parent-1" {
		t.Fatalf("溯源应保留,得到 %q", got.ResumeOfRunID)
	}
	if got.Resume == nil {
		t.Fatal("损坏计划应置空计划(非 nil),否则执行器会当普通运行静默全量重跑")
	}
	if len(got.Resume.Actions) != 0 {
		t.Fatalf("损坏计划应解析为空动作数组,得到 %v", got.Resume.Actions)
	}
}

// TestPlanInitialStatuses 验证 dbStepSink.Plan 的初始终态(继承/跳过节点 Plan 即落终态)。
func TestPlanInitialStatuses(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)
	s := svc.(*service)
	runID := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(
		`INSERT INTO pipeline_runs (id, project_id, status, trigger_type, trigger_branch, trigger_actor, created_at)
		 VALUES (?, ?, 'running', 'manual', 'main', 'admin', ?)`,
		runID, projID, now); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	sink := &dbStepSink{svc: s, runID: runID}
	err := sink.Plan(context.Background(), []StepDecl{
		{Name: "a", Stage: "S", Initial: StepSuccess},
		{Name: "b", Stage: "S", Initial: StepSkipped},
		{Name: "c", Stage: "S"},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got, err := s.loadSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("loadSteps: %v", err)
	}
	want := []string{StepSuccess, StepSkipped, StepPending}
	for i := range want {
		if got[i].Status != want[i] {
			t.Fatalf("step %d 状态 = %s,期望 %s", i, got[i].Status, want[i])
		}
		if want[i] != StepPending && got[i].FinishedAt == nil {
			t.Fatalf("初始终态步骤 %d 应带结束时刻", i)
		}
	}
	if err := sink.Plan(context.Background(), []StepDecl{{Name: "x", Initial: "bogus"}}); err == nil {
		t.Fatal("非法初始状态应报错")
	}
}
