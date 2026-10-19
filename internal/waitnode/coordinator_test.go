package waitnode

import (
	"testing"
	"time"
)

func TestWaitResolveDeliversSkip(t *testing.T) {
	c := New()
	key := Key("run1", "job1")
	ch := c.Wait(key, WaitInfo{Ordinal: 2, JobID: "job1", JobName: "等待", TotalSeconds: 60})

	if !c.Resolve(key, Skip{Actor: "admin"}) {
		t.Fatal("Resolve 应命中等待者")
	}
	select {
	case s := <-ch:
		if s.Actor != "admin" {
			t.Fatalf("收到的 Actor = %q,想要 admin", s.Actor)
		}
	default:
		t.Fatal("Resolve 后应能立即收到决定(缓冲 1)")
	}
	// 同一等待只决一次:二次 Resolve 不命中。
	if c.Resolve(key, Skip{Actor: "admin"}) {
		t.Fatal("已决的等待不应再被 Resolve 命中")
	}
}

func TestResolveWithoutWaiterReturnsFalse(t *testing.T) {
	c := New()
	if c.Resolve(Key("run1", "nope"), Skip{Actor: "admin"}) {
		t.Fatal("无等待者时 Resolve 应返回 false")
	}
}

func TestCancelRemovesWaiter(t *testing.T) {
	c := New()
	key := Key("run1", "job1")
	c.Wait(key, WaitInfo{})
	c.Cancel(key)
	c.Cancel(key) // 幂等
	if c.Resolve(key, Skip{Actor: "admin"}) {
		t.Fatal("Cancel 后 Resolve 不应命中")
	}
}

func TestPendingByRunFiltersAndSorts(t *testing.T) {
	c := New()
	base := time.Now()
	c.Wait(Key("runA", "j2"), WaitInfo{Ordinal: 5, JobID: "j2", JobName: "b", Deadline: base.Add(time.Minute)})
	c.Wait(Key("runA", "j1"), WaitInfo{Ordinal: 1, JobID: "j1", JobName: "a", Deadline: base.Add(time.Minute)})
	c.Wait(Key("runB", "j1"), WaitInfo{Ordinal: 0, JobID: "j1"})

	got := c.PendingByRun("runA")
	if len(got) != 2 {
		t.Fatalf("runA 等待数 = %d,想要 2", len(got))
	}
	if got[0].JobID != "j1" || got[1].JobID != "j2" {
		t.Fatalf("应按 ordinal 升序,得到 %s,%s", got[0].JobID, got[1].JobID)
	}
	// runID 前缀不得误吞(如 runA 与 runA2)。
	c.Wait(Key("runA2", "jx"), WaitInfo{Ordinal: 9})
	if n := len(c.PendingByRun("runA")); n != 2 {
		t.Fatalf("前缀过滤泄漏:runA 等待数 = %d,想要 2", n)
	}
	if n := len(c.PendingByRun("none")); n != 0 {
		t.Fatalf("无关 run 应为空,得到 %d", n)
	}
}

func TestRemainingSecondsClampsAtZero(t *testing.T) {
	w := WaitInfo{Deadline: time.Now().Add(-time.Second)}
	if got := w.RemainingSeconds(); got != 0 {
		t.Fatalf("过期等待剩余 = %d,想要 0", got)
	}
}
