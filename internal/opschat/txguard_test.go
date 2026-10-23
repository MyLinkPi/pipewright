package opschat

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// 守卫自测:事务窗口内同 goroutine 的池 IO 必须立即 panic 并带定位指引,
// 事务结束后标记必须清除。这也是对线上自死锁(policy→aiContextPolicy→
// ai.load)这一事故类别的结构性防线。
func TestTxGuardPanicsOnNestedPoolIO(t *testing.T) {
	s, _, _ := fixture(t, executor(), nil)
	defer func() { _ = s.Close(context.Background()) }()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("nested pool IO inside transaction did not panic")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "nested pool IO") || !strings.Contains(msg, "self-deadlock") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	_ = s.transaction(context.Background(), "", false, func(tx *sql.Tx) error {
		var one int
		_ = s.db.QueryRowContext(context.Background(), "SELECT 1").Scan(&one)
		return nil
	})
}

func TestTxGuardPanicsOnPolicyInsideTx(t *testing.T) {
	s, _, _ := fixture(t, executor(), nil)
	defer func() { _ = s.Close(context.Background()) }()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("policy() inside transaction did not panic")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "policy") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	_ = s.transaction(context.Background(), "", false, func(tx *sql.Tx) error {
		_ = s.policy() // 必须在事务外获取
		return nil
	})
}

func TestTxGuardClearsMarkAfterTransaction(t *testing.T) {
	s, _, _ := fixture(t, executor(), nil)
	defer func() { _ = s.Close(context.Background()) }()
	if err := s.transaction(context.Background(), "", false, func(tx *sql.Tx) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 事务结束后同一 goroutine 的池查询必须恢复正常(标记已清除)。
	var one int
	if err := s.db.QueryRowContext(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("pool query after transaction failed: err=%v one=%d", err, one)
	}
}
