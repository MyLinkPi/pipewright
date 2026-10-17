package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// stubClock 是可手动推进的测试时钟。
type stubClock struct{ now time.Time }

func newStubClock() *stubClock { return &stubClock{now: time.Now().UTC()} }
func (c *stubClock) Now() time.Time { return c.now }
func (c *stubClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// newLockoutTestService 建库 + 引导 admin/testpass,可选注入时钟。
func newLockoutTestService(t *testing.T, clock Clock) *Service {
	t.Helper()
	st := storetest.Open(t)
	svc := NewService(st.DB, clock)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return svc
}

// isLockedOut 判断 err 是否为 ErrLockedOut。
func isLockedOut(err error) bool {
	var locked *ErrLockedOut
	return errors.As(err, &locked)
}

// TestLockoutPerAccountIsolation 账户桶互相隔离:锁死 admin 不影响其它账户,
// 且 admin 的锁定跨 IP 生效(防分布式针对性爆破)。
func TestLockoutPerAccountIsolation(t *testing.T) {
	svc, db := newIdentityTestService(t)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	insertUserRow(t, db, "u1", "alice", "password123", "active")

	// 5 次失败(攻击者 IP 1.1.1.1)→ admin 账户桶锁定。
	for i := 0; i < LockoutThreshold; i++ {
		if _, err := svc.Login("admin", "wrongpass", "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: err = %v, want ErrInvalidCredentials", i+1, err)
		}
	}

	// 其它账户不受影响(旧版全局计数器会把她一并锁死——本用例即回归防线)。
	if _, err := svc.Login("alice", "password123", "2.2.2.2"); err != nil {
		t.Fatalf("alice login should not be affected: %v", err)
	}
	// admin 的账户锁定跨 IP:换来源 IP + 正确口令仍拒绝。
	if _, err := svc.Login("admin", "testpass", "3.3.3.3"); !isLockedOut(err) {
		t.Fatalf("admin should be locked across IPs, got %v", err)
	}
	// 垃圾用户名的失败只落在各自桶:别的用户名不受 admin 锁定影响。
	if _, err := svc.Login("nobody", "whatever", "4.4.4.4"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unrelated username should get invalid credentials, got %v", err)
	}
}

// TestLockoutPerIPLocksRotatingUsernames 同一 IP 轮换用户名也逃不过 IP 桶:
// 每个账户桶只记 1 次失败,但 IP 桶累计到阈值后整 IP 锁定。
func TestLockoutPerIPLocksRotatingUsernames(t *testing.T) {
	svc := newLockoutTestService(t, nil)

	// 10 个互不相同的用户名,各失败 1 次(每个账户桶 1 < 5,均不触发账户锁定)。
	for i := 0; i < ipLockoutThreshold; i++ {
		username := "ghost" + string(rune('a'+i))
		if _, err := svc.Login(username, "wrongpass", "9.9.9.9"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("rotating attempt %d: err = %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	// IP 桶已锁:即使换成正确的管理员凭据也拒绝。
	if _, err := svc.Login("admin", "testpass", "9.9.9.9"); !isLockedOut(err) {
		t.Fatalf("ip bucket should be locked, got %v", err)
	}
	// 其它 IP 不受影响。
	if _, err := svc.Login("admin", "testpass", "8.8.8.8"); err != nil {
		t.Fatalf("other ip should still work: %v", err)
	}
}

// TestLockoutSuccessResetsAccountNotIP 认证成功只重置账户桶;IP 桶计数保留
//(防攻击者用已知账号穿插成功登录来重置自己 IP 的失败计数)。
func TestLockoutSuccessResetsAccountNotIP(t *testing.T) {
	svc := newLockoutTestService(t, nil)

	// 6 个不同用户名各失败 1 次:IP 桶 = 6,任何账户桶均 = 1。
	for i := 0; i < 6; i++ {
		username := "ghost" + string(rune('a'+i))
		if _, err := svc.Login(username, "wrongpass", "7.7.7.7"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("pre-fill attempt %d: err = %v", i+1, err)
		}
	}
	// 成功登录(若 IP 桶被重置则归零,否则保持 6)。
	if _, err := svc.Login("admin", "testpass", "7.7.7.7"); err != nil {
		t.Fatalf("success login: %v", err)
	}
	// 再 4 次不同用户名失败:IP 桶未重置 → 6+4=10 达阈值锁定。
	for i := 0; i < 4; i++ {
		username := "noise" + string(rune('a'+i))
		if _, err := svc.Login(username, "wrongpass", "7.7.7.7"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("post-success attempt %d: err = %v", i+1, err)
		}
	}
	if _, err := svc.Login("admin", "testpass", "7.7.7.7"); !isLockedOut(err) {
		t.Fatalf("ip bucket must not be reset by successful login, got %v", err)
	}
}

// TestLockoutExpiryByClock 锁定到期后计数归零,可再次登录(注入时钟,免真实等待)。
func TestLockoutExpiryByClock(t *testing.T) {
	clock := newStubClock()
	svc := newLockoutTestService(t, clock)

	for i := 0; i < LockoutThreshold; i++ {
		_, _ = svc.Login("admin", "wrongpass", "1.1.1.1")
	}
	if _, err := svc.Login("admin", "testpass", "2.2.2.2"); !isLockedOut(err) {
		t.Fatalf("should be locked, got %v", err)
	}

	clock.Advance(LockoutDuration + time.Minute)
	if _, err := svc.Login("admin", "testpass", "2.2.2.2"); err != nil {
		t.Fatalf("login after expiry: %v", err)
	}
}

// TestChangePasswordThrottle 改密的当前口令校验受同款节流:连续错 5 次后,
// 即使口令正确也返回 ErrLockedOut(防已持会话者无限试当前口令)。
func TestChangePasswordThrottle(t *testing.T) {
	svc := newLockoutTestService(t, nil)
	sess, err := svc.Login("admin", "testpass", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	for i := 0; i < LockoutThreshold; i++ {
		err := svc.ChangePassword(RoleAdmin, "", "wrong-current", "newpass123", sess.Token)
		if !errors.Is(err, ErrInvalidCurrentPassword) {
			t.Fatalf("attempt %d: err = %v, want ErrInvalidCurrentPassword", i+1, err)
		}
	}
	// 第 6 次即使当前口令正确也被节流拒绝。
	if err := svc.ChangePassword(RoleAdmin, "", "testpass", "newpass123", sess.Token); !isLockedOut(err) {
		t.Fatalf("expected ErrLockedOut after repeated wrong current passwords, got %v", err)
	}
}
