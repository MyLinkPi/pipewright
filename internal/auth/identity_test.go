package auth

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// newIdentityTestService 建库并返回 Service(未引导管理员)。
func newIdentityTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	st := storetest.Open(t)
	return NewService(st.DB, nil), st.DB
}

// insertUserRow 直插一条普通用户(status 可控),口令经 HashPassword 生成。
func insertUserRow(t *testing.T, db *sql.DB, id, username, password, status string) {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO users (id, username, password_hash, role, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, username, hash, RoleMember, status, now, now,
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

// TestAdminLoginIdentity 管理员登录携带 role=admin、空 userID、用户名 admin。
func TestAdminLoginIdentity(t *testing.T) {
	svc, _ := newIdentityTestService(t)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	sess, err := svc.Login("admin", "testpass", "")
	if err != nil {
		t.Fatalf("Login(admin): %v", err)
	}
	if sess.Role != RoleAdmin {
		t.Fatalf("role = %q, want admin", sess.Role)
	}
	if sess.UserID != "" {
		t.Fatalf("管理员 userID 应为空, got %q", sess.UserID)
	}
	if sess.Username != "admin" {
		t.Fatalf("username = %q, want admin", sess.Username)
	}

	if _, err := svc.Login("admin", "wrongpass", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("错误口令应 ErrInvalidCredentials, got %v", err)
	}
}

// TestMemberLoginIdentity 普通用户(active)可登录,会话携带 member 身份。
func TestMemberLoginIdentity(t *testing.T) {
	svc, db := newIdentityTestService(t)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	insertUserRow(t, db, "u1", "alice", "password123", "active")

	sess, err := svc.Login("alice", "password123", "")
	if err != nil {
		t.Fatalf("Login(alice): %v", err)
	}
	if sess.Role != RoleMember {
		t.Fatalf("role = %q, want member", sess.Role)
	}
	if sess.UserID != "u1" {
		t.Fatalf("userID = %q, want u1", sess.UserID)
	}
	if sess.Username != "alice" {
		t.Fatalf("username = %q, want alice", sess.Username)
	}
}

// TestDisabledMemberCannotLogin 非 active 状态的用户不可登录。
func TestDisabledMemberCannotLogin(t *testing.T) {
	svc, db := newIdentityTestService(t)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	insertUserRow(t, db, "u1", "disabled", "password123", "disabled")

	if _, err := svc.Login("disabled", "password123", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled 用户应 ErrInvalidCredentials, got %v", err)
	}
}

// TestSessionStoreIdentity 会话 Create/Get 往返携带 role/userID/username。
func TestSessionStoreIdentity(t *testing.T) {
	_, db := newIdentityTestService(t)
	ss := NewSessionStore(db)

	// 普通用户会话。
	member, err := ss.Create(RoleMember, "u1", "alice")
	if err != nil {
		t.Fatalf("Create(member): %v", err)
	}
	got, err := ss.Get(member.Token)
	if err != nil {
		t.Fatalf("Get(member): %v", err)
	}
	if got.Role != RoleMember || got.UserID != "u1" || got.Username != "alice" {
		t.Fatalf("member 会话身份 = role=%q userID=%q username=%q", got.Role, got.UserID, got.Username)
	}

	// 管理员会话(UserID 空)。
	admin, err := ss.Create(RoleAdmin, "", "admin")
	if err != nil {
		t.Fatalf("Create(admin): %v", err)
	}
	got, err = ss.Get(admin.Token)
	if err != nil {
		t.Fatalf("Get(admin): %v", err)
	}
	if got.Role != RoleAdmin || got.UserID != "" || got.Username != "admin" {
		t.Fatalf("admin 会话身份 = role=%q userID=%q username=%q", got.Role, got.UserID, got.Username)
	}
}

// TestChangePasswordAdmin 管理员改口令:旧口令失效、新口令可登录、其它会话失效、当前会话保留。
func TestChangePasswordAdmin(t *testing.T) {
	svc, _ := newIdentityTestService(t)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	s1, err := svc.Login("admin", "testpass", "")
	if err != nil {
		t.Fatalf("Login(s1): %v", err)
	}
	s2, err := svc.Login("admin", "testpass", "")
	if err != nil {
		t.Fatalf("Login(s2): %v", err)
	}

	if err := svc.ChangePassword(RoleAdmin, "", "testpass", "newpass123", s2.Token); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	// 其它会话被删,当前会话保留。
	if _, err := svc.sessions.Get(s1.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("其它会话应失效, got %v", err)
	}
	if _, err := svc.sessions.Get(s2.Token); err != nil {
		t.Fatalf("当前会话应保留, got %v", err)
	}

	// 旧口令失效、新口令可登录。
	if _, err := svc.Login("admin", "testpass", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("旧口令应失效, got %v", err)
	}
	if _, err := svc.Login("admin", "newpass123", ""); err != nil {
		t.Fatalf("新口令应可登录, got %v", err)
	}
}

// TestChangePasswordMember 普通用户改口令仅影响 users 行,管理员口令不受影响;
// 其它会话失效、当前会话保留。
func TestChangePasswordMember(t *testing.T) {
	svc, db := newIdentityTestService(t)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	insertUserRow(t, db, "u1", "alice", "password123", "active")

	s1, err := svc.Login("alice", "password123", "")
	if err != nil {
		t.Fatalf("Login(s1): %v", err)
	}
	s2, err := svc.Login("alice", "password123", "")
	if err != nil {
		t.Fatalf("Login(s2): %v", err)
	}

	if err := svc.ChangePassword(RoleMember, "u1", "password123", "memberpass1", s2.Token); err != nil {
		t.Fatalf("ChangePassword(member): %v", err)
	}

	// 其它会话被删,当前会话保留。
	if _, err := svc.sessions.Get(s1.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("其它会话应失效, got %v", err)
	}
	if _, err := svc.sessions.Get(s2.Token); err != nil {
		t.Fatalf("当前会话应保留, got %v", err)
	}

	// 管理员口令不受影响。
	if _, err := svc.Login("admin", "testpass", ""); err != nil {
		t.Fatalf("管理员口令不应受影响, got %v", err)
	}
	// 成员旧口令失效、新口令可登录。
	if _, err := svc.Login("alice", "password123", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("成员旧口令应失效, got %v", err)
	}
	if _, err := svc.Login("alice", "memberpass1", ""); err != nil {
		t.Fatalf("成员新口令应可登录, got %v", err)
	}
}

// TestChangePasswordErrors 覆盖改口令的错误分支。
func TestChangePasswordErrors(t *testing.T) {
	svc, _ := newIdentityTestService(t)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	sess, err := svc.Login("admin", "testpass", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// currentToken 为空 → 拒绝(防误删全部会话)。
	if err := svc.ChangePassword(RoleAdmin, "", "testpass", "newpass123", ""); !errors.Is(err, ErrInvalidCurrentPassword) {
		t.Fatalf("空 currentToken 应 ErrInvalidCurrentPassword, got %v", err)
	}
	// 新口令过弱(<8)。
	if err := svc.ChangePassword(RoleAdmin, "", "testpass", "short", sess.Token); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("弱新口令应 ErrWeakPassword, got %v", err)
	}
	// 当前口令错误。
	if err := svc.ChangePassword(RoleAdmin, "", "wrongpass", "newpass123", sess.Token); !errors.Is(err, ErrInvalidCurrentPassword) {
		t.Fatalf("错误当前口令应 ErrInvalidCurrentPassword, got %v", err)
	}
	// member 用户不存在。
	if err := svc.ChangePassword(RoleMember, "no-such-user", "whatever", "newpass123", sess.Token); !errors.Is(err, ErrInvalidCurrentPassword) {
		t.Fatalf("member 用户不存在应 ErrInvalidCurrentPassword, got %v", err)
	}
}
