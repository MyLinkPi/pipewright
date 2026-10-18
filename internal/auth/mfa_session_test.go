package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// TestSessionMFATiers 三档会话 TTL 与语义:
//   - 普通:7 天、非 pending/verified;
//   - pending:10 分钟、List 不列出、CompleteMFA 可升级;
//   - verified:30 天、CompleteMFA 拒绝(非 pending)。
func TestSessionMFATiers(t *testing.T) {
	st := storetest.Open(t)
	ss := NewSessionStore(st.DB)

	normal, err := ss.Create(RoleAdmin, "", "admin")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if normal.MFAPending || normal.MFAVerified || normal.MFAState != MFAStateNone {
		t.Fatalf("normal session state = %+v", normal)
	}
	if d := normal.ExpiresAt.Sub(normal.CreatedAt); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("normal ttl = %v, want ~7d", d)
	}

	pending, err := ss.CreateWithMFAState(RoleAdmin, "", "admin", MFAStatePending)
	if err != nil {
		t.Fatalf("CreateWithMFAState(pending): %v", err)
	}
	if !pending.MFAPending || pending.MFAVerified {
		t.Fatalf("pending flags = pending=%v verified=%v", pending.MFAPending, pending.MFAVerified)
	}
	if d := pending.ExpiresAt.Sub(pending.CreatedAt); d < 10*time.Minute-time.Minute || d > 10*time.Minute+time.Minute {
		t.Fatalf("pending ttl = %v, want ~10m", d)
	}

	verified, err := ss.CreateWithMFAState(RoleMember, "u1", "alice", MFAStateVerified)
	if err != nil {
		t.Fatalf("CreateWithMFAState(verified): %v", err)
	}
	if verified.MFAPending || !verified.MFAVerified {
		t.Fatalf("verified flags = %+v", verified)
	}
	if d := verified.ExpiresAt.Sub(verified.CreatedAt); d < 30*24*time.Hour-time.Minute || d > 30*24*time.Hour+time.Minute {
		t.Fatalf("verified ttl = %v, want ~30d", d)
	}

	// List 只列正式会话(pending 不出现)。
	metas, err := ss.List(normal.Token)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("len(metas) = %d, want 2 (pending excluded)", len(metas))
	}

	// CompleteMFA:pending → verified(30 天),原 token 语义不变。
	upgraded, err := ss.CompleteMFA(pending.Token)
	if err != nil {
		t.Fatalf("CompleteMFA: %v", err)
	}
	if upgraded.MFAPending || !upgraded.MFAVerified || upgraded.Token != pending.Token {
		t.Fatalf("upgraded = %+v", upgraded)
	}
	if d := upgraded.ExpiresAt.Sub(time.Now().UTC()); d < 30*24*time.Hour-time.Minute {
		t.Fatalf("upgraded expiry too soon: %v", d)
	}

	// 非 pending 会话不可再升级。
	if _, err := ss.CompleteMFA(normal.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("CompleteMFA(normal) = %v, want ErrSessionNotFound", err)
	}
	if _, err := ss.CompleteMFA(upgraded.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("CompleteMFA(upgraded) = %v, want ErrSessionNotFound", err)
	}
}

// stubMFAChecker 是可控的 TOTP 启用检查器替身。
type stubMFAChecker struct{ enabled bool }

func (c stubMFAChecker) TOTPEnabled(role, userID string) bool { return c.enabled }

// TestLoginMFAPending 登录分支:检查器报已启用 → Login 返回 pending 会话;
// 未启用 → 正常 7 天会话;CompleteMFA 走通升级路径。
func TestLoginMFAPending(t *testing.T) {
	st := storetest.Open(t)
	checker := &stubMFAChecker{enabled: true}
	svc := NewService(st.DB, nil, WithMFAChecker(checker))
	if err := svc.Bootstrap("admin", "password123", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	sess, err := svc.Login("admin", "password123", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !sess.MFAPending {
		t.Fatal("Login with TOTP-enabled account must return pending session")
	}
	if d := sess.ExpiresAt.Sub(sess.CreatedAt); d > 15*time.Minute {
		t.Fatalf("pending ttl = %v, want ~10m", d)
	}

	// 关闭启用状态 → 单步登录。
	checker.enabled = false
	plain, err := svc.Login("admin", "password123", "")
	if err != nil {
		t.Fatalf("Login(single-step): %v", err)
	}
	if plain.MFAPending || plain.MFAVerified {
		t.Fatalf("single-step session = %+v", plain)
	}

	// pending 会话经 CompleteMFA 升级为 verified。
	upgraded, err := svc.CompleteMFA(sess.Token)
	if err != nil {
		t.Fatalf("CompleteMFA: %v", err)
	}
	if !upgraded.MFAVerified {
		t.Fatal("upgraded must be MFAVerified")
	}

	// Passkey 会话工厂:直接产 verified 会话。
	pk, err := svc.CreatePasskeySession(RoleMember, "u1", "alice")
	if err != nil {
		t.Fatalf("CreatePasskeySession: %v", err)
	}
	if !pk.MFAVerified || pk.Role != RoleMember || pk.UserID != "u1" {
		t.Fatalf("passkey session = %+v", pk)
	}
}
