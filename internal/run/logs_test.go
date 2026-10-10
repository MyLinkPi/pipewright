package run

import (
	"context"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/mask"
)

// TestAppendLogSeqMonotonic 验证 AppendLog 在同一 run 内分配单调递增 seq(从 1 起)。
func TestAppendLogSeqMonotonic(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)
	r, err := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := 1; i <= 5; i++ {
		seq, err := svc.AppendLog(context.Background(), r.ID, streamStdout, 0, "line", "")
		if err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
		if seq != i {
			t.Fatalf("第 %d 次 AppendLog seq 应为 %d, got %d", i, i, seq)
		}
	}
}

// TestGetLogsSincePaging 验证 GetLogs 按 sinceSeq 升序分页返回。
func TestGetLogsSincePaging(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)
	r, _ := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})

	for i := 0; i < 4; i++ {
		if _, err := svc.AppendLog(context.Background(), r.ID, streamStdout, i, "L", ""); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
	}

	all, err := svc.GetLogs(context.Background(), r.ID, 0)
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("sinceSeq=0 应返回 4 行, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Fatalf("seq 应升序: %d <= %d", all[i].Seq, all[i-1].Seq)
		}
	}

	rest, _ := svc.GetLogs(context.Background(), r.ID, 2)
	if len(rest) != 2 {
		t.Fatalf("sinceSeq=2 应返回 2 行, got %d", len(rest))
	}
	if rest[0].Seq != 3 {
		t.Fatalf("sinceSeq=2 首行 seq 应为 3, got %d", rest[0].Seq)
	}
}

// TestStubRunnerEmitsMaskedLogs 端到端验证:桩 runner 逐行 emit 日志(含假 secret)→
// 经 dbStepSink.Log 脱敏落库 → run_logs 表/GetLogs 中假 secret 一律 [MASKED]、绝无明文(AC-SEC-04)。
func TestStubRunnerEmitsMaskedLogs(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	masker := mask.NewMasker()
	masker.RegisterSecret(StubFailureSecret)
	// FailAt=0 让首步失败,既 emit stdout(含假 secret 行)又 emit stderr。
	pool := NewWorkerPool(svc,
		WithConcurrency(1),
		WithRunner(&StubRunner{Steps: []string{"构建镜像"}, FailAt: 0}),
		WithLogMasker(masker),
	)
	pool.Start()
	t.Cleanup(func() { pool.Stop(context.Background()) })

	projID := seedProject(t, db)
	r, _ := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
	waitForStatus(t, svc, r.ID, StatusFailed)

	logs, err := svc.GetLogs(context.Background(), r.ID, 0)
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("失败 run 应有日志行")
	}

	sawMasked := false
	hasStderr := false
	for _, l := range logs {
		if strings.Contains(l.Text, StubFailureSecret) {
			t.Fatalf("日志行含明文假 secret(脱敏失败): %q", l.Text)
		}
		if strings.Contains(l.Text, mask.Placeholder) {
			sawMasked = true
		}
		if l.Stream == streamStderr {
			hasStderr = true
		}
	}
	if !sawMasked {
		t.Fatalf("应有一行被替换为 %s(验脱敏命中)", mask.Placeholder)
	}
	if !hasStderr {
		t.Fatalf("失败步骤应追加 stderr 日志行")
	}

	// 裸 DB 断言:run_logs 表内绝无明文假 secret。
	var leak int
	if err := db.QueryRow(
		"SELECT COUNT(1) FROM run_logs WHERE run_id = ? AND `text` LIKE ?",
		r.ID, "%"+StubFailureSecret+"%",
	).Scan(&leak); err != nil {
		t.Fatalf("查 run_logs 明文泄漏: %v", err)
	}
	if leak != 0 {
		t.Fatalf("run_logs 表残留 %d 行明文假 secret", leak)
	}
}

// TestLogMachineRoundTrip 验证日志行的机器归属(ctx 通道 → 落库 → 读回):
// WithLogMachine 挂载的行 machine 落 run_logs.machine,GetLogs 原样读回;未挂载行为空串。
func TestLogMachineRoundTrip(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)
	r, err := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := svc.AppendLog(context.Background(), r.ID, streamStdout, 0, "控制机行", ""); err != nil {
		t.Fatalf("AppendLog 控制机行: %v", err)
	}
	if _, err := svc.AppendLog(context.Background(), r.ID, streamStdout, 0, "构建机行", "build-01(10.0.0.5)"); err != nil {
		t.Fatalf("AppendLog 构建机行: %v", err)
	}

	lines, err := svc.GetLogs(context.Background(), r.ID, 0)
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("应返回 2 行, got %d", len(lines))
	}
	if lines[0].Machine != "" {
		t.Fatalf("未标注行 machine 应为空串, got %q", lines[0].Machine)
	}
	if lines[1].Machine != "build-01(10.0.0.5)" {
		t.Fatalf("标注行 machine 应原样读回, got %q", lines[1].Machine)
	}
}

// TestLogMachineFromCtx 验证 ctx 通道语义:WithLogMachine 挂载/LogMachineFrom 读出,空串不挂载。
func TestLogMachineFromCtx(t *testing.T) {
	ctx := context.Background()
	if got := LogMachineFrom(ctx); got != "" {
		t.Fatalf("裸 ctx 应为空串, got %q", got)
	}
	if got := LogMachineFrom(WithLogMachine(ctx, "")); got != "" {
		t.Fatalf("空机器名不应挂载, got %q", got)
	}
	if got := LogMachineFrom(WithLogMachine(ctx, "web-01")); got != "web-01" {
		t.Fatalf("应读回挂载值, got %q", got)
	}
}
