package run

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedRunAt 插一个指定状态与 created_at 的 run,返回 run id(供「最近一次」排序验证)。
func seedRunAt(t *testing.T, svc Service, projID, status string, createdAt time.Time) string {
	t.Helper()
	s := svc.(*service)
	runID := uuid.NewString()
	if _, err := s.db.Exec(
		`INSERT INTO pipeline_runs
		   (id, project_id, status, trigger_type, trigger_branch, trigger_commit, trigger_actor, created_at)
		 VALUES (?, ?, ?, 'manual', 'main', 'abc', 'admin', ?)`,
		runID, projID, status, createdAt.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}

// TestLatestByProject 验证每项目取 created_at 最新一条(含创建时间);无运行的项目不出现在
// 结果中;空入参返回空 map 不报错。
func TestLatestByProject(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projA := seedProject(t, db)
	projB := seedProject(t, db)

	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	seedRunAt(t, svc, projA, StatusSuccess, base)
	seedRunAt(t, svc, projA, StatusFailed, base.Add(time.Hour))    // A 最新 → failed
	seedRunAt(t, svc, projB, StatusRunning, base.Add(2*time.Hour)) // B 唯一 → running

	m, err := svc.LatestByProject(context.Background(), []string{projA, projB})
	if err != nil {
		t.Fatalf("LatestByProject: %v", err)
	}
	if len(m) != 2 ||
		m[projA].Status != StatusFailed || !m[projA].CreatedAt.Equal(base.Add(time.Hour)) ||
		m[projB].Status != StatusRunning || !m[projB].CreatedAt.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("期望 A=failed / B=running(含时间), got %+v", m)
	}

	// 混入无运行的项目:该缺席;已有项目的取值不变。
	projC := seedProject(t, db)
	m, err = svc.LatestByProject(context.Background(), []string{projA, projC})
	if err != nil {
		t.Fatalf("LatestByProject(含无运行项目): %v", err)
	}
	if len(m) != 1 || m[projA].Status != StatusFailed {
		t.Fatalf("无运行项目应缺席,期望仅 A=failed, got %+v", m)
	}

	if m, err = svc.LatestByProject(context.Background(), nil); err != nil || len(m) != 0 {
		t.Fatalf("空入参应返回空 map, got %+v err=%v", m, err)
	}
}
