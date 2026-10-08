package run

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// gateRejectRunner 模拟审批门未放行的 runner:声明一个步骤后返回包装哨兵的错误。
type gateRejectRunner struct{ err error }

func (g *gateRejectRunner) Run(ctx context.Context, _ *Run, sink StepSink) error {
	if err := sink.Plan(ctx, []StepDecl{{Name: "部署", Stage: "部署"}}); err != nil {
		return err
	}
	return g.err
}

// TestPoolLandsRejectedOnGateSentinel 验证 worker 对包装 ErrGateRejected 的错误落
// StatusRejected(已拒绝)而非 failed。
func TestPoolLandsRejectedOnGateSentinel(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	pool := NewWorkerPool(svc, WithRunner(&gateRejectRunner{
		err: fmt.Errorf("dagrun: 审批门未放行(阶段: 部署): %w", ErrGateRejected),
	}))
	pool.Start()
	t.Cleanup(func() { pool.Stop(context.Background()) })

	projID := seedProject(t, db)
	r, err := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForStatus(t, svc, r.ID, StatusRejected)

	got, err := svc.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusRejected {
		t.Fatalf("run 应为 rejected,得到 %s", got.Status)
	}
}

// gateCancelRunner 模拟「门控等待中被取消」:等待 ctx 取消后返回包装哨兵的错误
// (取消语义优先,worker 应落 failed 而非 rejected)。
type gateCancelRunner struct{ started chan struct{} }

func (g *gateCancelRunner) Run(ctx context.Context, _ *Run, sink StepSink) error {
	if err := sink.Plan(ctx, []StepDecl{{Name: "部署", Stage: "部署"}}); err != nil {
		return err
	}
	close(g.started)
	<-ctx.Done()
	return fmt.Errorf("%v: %w", ctx.Err(), ErrGateRejected)
}

func TestPoolCancelWinsOverGateSentinel(t *testing.T) {
	db := testDB(t)
	svc := New(db)
	g := &gateCancelRunner{started: make(chan struct{})}
	pool := NewWorkerPool(svc, WithRunner(g))
	pool.Start()
	t.Cleanup(func() { pool.Stop(context.Background()) })

	projID := seedProject(t, db)
	r, err := svc.Create(context.Background(), projID, Trigger{Type: TriggerManual})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	<-g.started // 等 runner 进入门控等待
	if _, err := svc.Cancel(context.Background(), r.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitForStatus(t, svc, r.ID, StatusFailed)
}

// TestRejectedStateMachine 验证 rejected 的状态机语义:running→rejected 合法、终态、
// 枚举可筛选、不可再转移。
func TestRejectedStateMachine(t *testing.T) {
	if !canTransition(StatusRunning, StatusRejected) {
		t.Fatal("running→rejected 应为合法转移")
	}
	if canTransition(StatusRejected, StatusRunning) || canTransition(StatusRejected, StatusFailed) {
		t.Fatal("rejected 为终态,不可再转移")
	}
	if !IsTerminal(StatusRejected) {
		t.Fatal("rejected 应为终态")
	}
	if !isValidStatus(StatusRejected) {
		t.Fatal("rejected 应为合法枚举(列表筛选)")
	}
	if !errors.Is(ErrGateRejected, ErrGateRejected) {
		t.Fatal("哨兵自反")
	}
}
