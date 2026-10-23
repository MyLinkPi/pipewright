package opschat

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// haltHeartbeat 模拟进程崩溃:停掉工作上下文与租约心跳但不释放租约行,
// 供测试观察下一个实例的启动等待与恢复行为。仅测试使用。
func (s *Service) haltHeartbeat() {
	s.mu.Lock()
	if s.stop != nil {
		s.stop()
		s.stop = nil
		s.root = nil
	}
	if s.loopDone != nil {
		close(s.loopDone)
		s.loopDone = nil
	}
	done := s.leaseDone
	s.mu.Unlock()
	<-done
}

func leaseRow(t *testing.T, db *sql.DB) (string, int64) {
	t.Helper()
	var owner string
	var until int64
	if err := db.QueryRow("SELECT lease_owner,lease_until FROM ops_chat_state WHERE id=1").Scan(&owner, &until); err != nil {
		t.Fatal(err)
	}
	return owner, until
}

func waitAvailable(t *testing.T, s *Service, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.Capabilities().Available == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("available did not become %v", want)
}

// 健康服务每秒续期:租约截止单调前进且始终归本实例。
func TestLeaseRenewalAdvancesDeadline(t *testing.T) {
	s, _, db := fixture(t, executor(), nil)
	owner0, until0 := leaseRow(t, db)
	if owner0 != s.instance {
		t.Fatal("lease not owned by started instance", owner0)
	}
	time.Sleep(2500 * time.Millisecond)
	owner1, until1 := leaseRow(t, db)
	if owner1 != s.instance || until1 <= until0 {
		t.Fatal("renewal did not advance deadline", until0, until1)
	}
	if !s.Capabilities().Available {
		t.Fatal("healthy service reported unavailable")
	}
}

// 线上事故场景:续期被业务流量饿死导致自己的租约过期。旧实现把它误判为
// "被其他实例接管"而永久自杀,只能重启进程;现在 fence 降级后 leaseLoop
// 在租约空窗期 CAS 重取,服务自愈,活动态 run 收敛 Interrupted(不重放)。
func TestLeaseSelfExpiryFencesThenSelfHeals(t *testing.T) {
	id := uuid.NewString()
	f := executor(id)
	m := model()
	s, _, db := fixture(t, f, m)
	c := chat(t, s, id)
	run := seedReviewRun(t, s, db, c, Running, runBody{Turn: TurnInput{Text: "question"}})
	// 自持租约过期(续期饿死):owner 仍是本实例,until 已在过去。
	if _, err := db.Exec("UPDATE ops_chat_state SET lease_until=? WHERE id=1", time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	waitAvailable(t, s, false)
	if _, err := s.Submit(context.Background(), c.ID, TurnInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, Text: "x"}); err != ErrLease {
		t.Fatal("fenced service accepted submission", err)
	}
	// 租约空窗 → 自动重取 → 自愈;活动态 run 收敛为 Interrupted。
	waitAvailable(t, s, true)
	snap := waitRun(t, s, c.ID, run, Interrupted)
	if snap.Session.ActiveRunID != "" {
		t.Fatal("reacquire did not release active run", snap.Session)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(m.rounds) != 0 || len(f.invocations) != 0 {
		t.Fatal("reacquire replayed remote work")
	}
}

// 租约被其他实例接管:fence 降级,但对方租约仍有效时绝不抢租;对方过期后
// 自动夺回并恢复服务。
func TestLeaseTakeoverFencesThenRecoversAfterExpiry(t *testing.T) {
	s, _, db := fixture(t, executor(), nil)
	thief := uuid.NewString()
	if _, err := db.Exec("UPDATE ops_chat_state SET lease_owner=?,lease_until=? WHERE id=1", thief, time.Now().Add(2*time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	waitAvailable(t, s, false)
	// fence 最迟发生在对方过期前 1s(每秒一个 tick),此处对方租约必仍有效:
	// 既不得自行恢复,也不得覆盖行内租约。
	time.Sleep(500 * time.Millisecond)
	if s.Capabilities().Available {
		t.Fatal("stole a live lease from another instance")
	}
	if owner, _ := leaseRow(t, db); owner != thief {
		t.Fatal("live lease was overwritten while still valid")
	}
	waitAvailable(t, s, true)
	if owner, _ := leaseRow(t, db); owner != s.instance {
		t.Fatal("did not reacquire after expiry", owner)
	}
}

// Close 停掉心跳并释放租约行,下一个实例可立即接管(无需等 15s 过期)。
func TestCloseReleasesLeaseAndStopsHeartbeat(t *testing.T) {
	db := storetest.OpenDB(t)
	key := &[32]byte{1}
	s, err := New(Options{DB: db, Vault: vault.New(db, key), Executor: executor()})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	owner, until := leaseRow(t, db)
	if owner != "" || until != 0 {
		t.Fatal("close did not release lease", owner, until)
	}
	select {
	case <-s.leaseDone:
	default:
		t.Fatal("heartbeat still running after close")
	}
}
