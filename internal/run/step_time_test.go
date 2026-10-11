package run

// step_time_test.go 验证「日志时间线」两件事:
//   - 每条日志行在落库/出网前注入行首时间戳前缀(本地时区,与 ts 列同刻);
//   - 步骤开始/结束时向该步骤日志追加时间锚点行(状态 + 耗时),使日志文本自身
//     回答「这一步什么时候开始/结束、跑了多久」。
//
// 覆盖:正常步骤(开始/完成/耗时)、失败步骤(耗时)、终态兜底校正(未执行)、纯函数耗时格式化。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/mask"
)

// TestAppendLogTimePrefix AppendLog 在行首注入本地时区时间戳前缀(与 ts 列同刻);
// ts 列保持 UTC RFC3339(结构化时间语义不变)。
func TestAppendLogTimePrefix(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	projID := seedProject(t, db)
	r, err := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := svc.AppendLog(context.Background(), r.ID, streamStdout, 0, "$ echo hi", ""); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	logs, err := svc.GetLogs(context.Background(), r.ID, 0)
	if err != nil || len(logs) != 1 {
		t.Fatalf("GetLogs: %v / %d 行", err, len(logs))
	}
	l := logs[0]
	if !strings.HasSuffix(l.Text, " $ echo hi") {
		t.Fatalf("原文应完整保留在时间前缀之后, got %q", l.Text)
	}
	// 前缀时间与 ts 同刻(ts 为 UTC;本地时区格式化后应逐字符一致)。
	wantPrefix := "[" + l.Ts.Local().Format(LogTimeLayout) + "] "
	if !strings.HasPrefix(l.Text, wantPrefix) {
		t.Fatalf("行首时间前缀 = %q, want %q(与 ts 同刻)", l.Text, wantPrefix)
	}
	// 结构化 ts 仍为 UTC。
	if l.Ts.Location() != time.UTC {
		t.Fatalf("ts 应为 UTC, got %v", l.Ts.Location())
	}
}

// TestStubRunnerStepTimeAnchors 端到端:成功 run 的日志含步骤开始/完成锚点行(带耗时),
// 且每行都带时间戳前缀。
func TestStubRunnerStepTimeAnchors(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	pool := NewWorkerPool(svc,
		WithConcurrency(1),
		WithRunner(&StubRunner{Steps: []string{"拉取代码", "构建"}, FailAt: -1}),
	)
	pool.Start()
	t.Cleanup(func() { pool.Stop(context.Background()) })

	projID := seedProject(t, db)
	r, _ := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
	waitForStatus(t, svc, r.ID, StatusSuccess)

	logs, err := svc.GetLogs(context.Background(), r.ID, 0)
	if err != nil || len(logs) == 0 {
		t.Fatalf("GetLogs: %v / %d 行", err, len(logs))
	}
	var joined strings.Builder
	for _, l := range logs {
		if !strings.HasPrefix(l.Text, "[") {
			t.Fatalf("每行都应带时间戳前缀: %q", l.Text)
		}
		joined.WriteString(l.Text)
		joined.WriteByte('\n')
	}
	all := joined.String()
	if got := strings.Count(all, "→ 步骤开始"); got != 2 {
		t.Fatalf("两个步骤应各有 1 条开始锚点行, got %d:\n%s", got, all)
	}
	if got := strings.Count(all, "✓ 步骤完成(耗时 "); got != 2 {
		t.Fatalf("两个步骤应各有 1 条完成锚点行(带耗时), got %d:\n%s", got, all)
	}
}

// TestStubRunnerFailedStepTimeAnchor 失败 run:失败步骤输出 `✗ 步骤失败(耗时 Xs)`;
// 未执行步骤(终态兜底校正)输出 `✗ 步骤失败(未执行)`。
func TestStubRunnerFailedStepTimeAnchor(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	masker := mask.NewMasker()
	masker.RegisterSecret(StubFailureSecret)
	pool := NewWorkerPool(svc,
		WithConcurrency(1),
		WithRunner(&StubRunner{Steps: []string{"拉取代码", "构建"}, FailAt: 0}),
		WithLogMasker(masker),
	)
	pool.Start()
	t.Cleanup(func() { pool.Stop(context.Background()) })

	projID := seedProject(t, db)
	r, _ := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
	waitForStatus(t, svc, r.ID, StatusFailed)

	logs, err := svc.GetLogs(context.Background(), r.ID, 0)
	if err != nil || len(logs) == 0 {
		t.Fatalf("GetLogs: %v / %d 行", err, len(logs))
	}
	var joined strings.Builder
	for _, l := range logs {
		joined.WriteString(l.Text)
		joined.WriteByte('\n')
	}
	all := joined.String()
	if !strings.Contains(all, "✗ 步骤失败(耗时 ") {
		t.Fatalf("失败步骤应输出带耗时的失败锚点行:\n%s", all)
	}
	if !strings.Contains(all, "✗ 步骤失败(未执行)") {
		t.Fatalf("未执行步骤应输出「未执行」失败锚点行(终态兜底校正):\n%s", all)
	}
}

// TestStepDoneText 锚点行正文生成(跳过 / 未执行 / 有耗时)。
func TestStepDoneText(t *testing.T) {
	started := time.Now().Add(-90 * time.Second)
	cases := []struct {
		name    string
		status  string
		started *time.Time
		want    string
	}{
		{"成功带耗时", StepSuccess, &started, "✓ 步骤完成(耗时 1m"},
		{"失败带耗时", StepFailed, &started, "✗ 步骤失败(耗时 1m"},
		{"失败未执行", StepFailed, nil, "✗ 步骤失败(未执行)"},
		{"成功无开始时刻(退化)", StepSuccess, nil, "✓ 步骤完成"},
		{"跳过", StepSkipped, &started, "⏭ 步骤跳过"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stepDoneText(c.status, c.started)
			if c.want == "✓ 步骤完成(耗时 1m" || c.want == "✗ 步骤失败(耗时 1m" {
				if !strings.HasPrefix(got, c.want) {
					t.Fatalf("stepDoneText = %q, want 前缀 %q", got, c.want)
				}
				return
			}
			if got != c.want {
				t.Fatalf("stepDoneText = %q, want %q", got, c.want)
			}
		})
	}
}

// TestElapsedText 耗时人读格式化(秒级;与前端步骤耗时口径一致)。
func TestElapsedText(t *testing.T) {
	cases := map[time.Duration]string{
		0:                             "0s",
		12 * time.Second:              "12s",
		65 * time.Second:              "1m05s",
		2*time.Hour + 3*time.Minute:   "2h03m",
		-1 * time.Second:              "0s", // 负值(时钟回拨防御)归零
		59 * time.Second:              "59s",
		60 * time.Second:              "1m00s",
	}
	for d, want := range cases {
		if got := elapsedText(d); got != want {
			t.Errorf("elapsedText(%v) = %q, want %q", d, got, want)
		}
	}
}