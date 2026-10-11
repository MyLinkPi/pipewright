package identity

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

// eachDB 对 SQLite / MySQL 各跑一遍 body(MySQL 未配置时其子测试自动 Skip)。
func eachDB(t *testing.T, body func(t *testing.T, db *sql.DB)) {
	t.Helper()
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) { body(t, st.DB) })
}

// seedProject 直插一条项目(含其凭据行,满足外键),返回 project id。
// 参考 run 包 seedProject:先插 credentials 再插 projects。
func seedProject(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	credID := "cred-" + id
	if _, err := db.Exec(
		`INSERT INTO credentials (id, name, type, scope, ciphertext, masked_value, created_at, updated_at)
		 VALUES (?, 'c', 'git_token', '', X'00', 'm', ?, ?)`,
		credID, now, now,
	); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO projects (id, name, repo_url, default_branch, credential_id, created_at, updated_at)
		 VALUES (?, ?, 'https://example.com/p.git', 'main', ?, ?, ?)`,
		id, id, credID, now, now,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

// registerUser 建邀请并注册一个普通用户,返回用户。
func registerUser(t *testing.T, svc Service, username, password string) *User {
	t.Helper()
	inv, err := svc.CreateInvitation(context.Background(), "admin", 0)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	u, err := svc.Register(context.Background(), inv.Token, username, password)
	if err != nil {
		t.Fatalf("Register(%s): %v", username, err)
	}
	return u
}

// TestInvitationLifecycle 覆盖邀请的 创建(默认/显式 TTL) → 列表 → 撤销。
func TestInvitationLifecycle(t *testing.T) {
	eachDB(t, func(t *testing.T, db *sql.DB) {
		svc := New(db)
		ctx := context.Background()

		// ttl<=0 → 用默认 InvitationTTL。
		def, err := svc.CreateInvitation(ctx, "admin", 0)
		if err != nil {
			t.Fatalf("CreateInvitation(默认 ttl): %v", err)
		}
		if def.Token == "" || def.ID == "" {
			t.Fatalf("邀请应有 id 与非空 token: %+v", def)
		}
		if def.State != InvitationPending {
			t.Fatalf("新建邀请状态应 pending, got %q", def.State)
		}
		if d := def.ExpiresAt.Sub(def.CreatedAt); d < InvitationTTL-time.Minute || d > InvitationTTL+time.Minute {
			t.Fatalf("默认 TTL 异常: %v(期望约 %v)", d, InvitationTTL)
		}

		// 显式 ttl 生效。
		short, err := svc.CreateInvitation(ctx, "admin", time.Hour)
		if err != nil {
			t.Fatalf("CreateInvitation(1h): %v", err)
		}
		if d := short.ExpiresAt.Sub(short.CreatedAt); d < time.Hour-time.Minute || d > time.Hour+time.Minute {
			t.Fatalf("显式 TTL 异常: %v(期望约 1h)", d)
		}

		list, err := svc.ListInvitations(ctx)
		if err != nil {
			t.Fatalf("ListInvitations: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("ListInvitations 应 2 条, got %d", len(list))
		}

		// 撤销存在的邀请 → 之后按 token 查不到。
		if err := svc.RevokeInvitation(ctx, short.ID); err != nil {
			t.Fatalf("RevokeInvitation: %v", err)
		}
		if _, err := svc.InvitationInfo(ctx, short.Token); !errors.Is(err, ErrInvitationNotFound) {
			t.Fatalf("已撤销邀请应 ErrInvitationNotFound, got %v", err)
		}

		// 撤销不存在的 id → ErrInvitationNotFound(而非静默成功)。
		if err := svc.RevokeInvitation(ctx, "no-such-invitation"); !errors.Is(err, ErrInvitationNotFound) {
			t.Fatalf("撤销不存在 → 应 ErrInvitationNotFound, got %v", err)
		}

		list, err = svc.ListInvitations(ctx)
		if err != nil {
			t.Fatalf("ListInvitations(2): %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("撤销后应剩 1 条, got %d", len(list))
		}
	})
}

// TestInvitationState 覆盖状态计算:pending / expired / used。
func TestInvitationState(t *testing.T) {
	eachDB(t, func(t *testing.T, db *sql.DB) {
		svc := New(db)
		ctx := context.Background()

		inv, err := svc.CreateInvitation(ctx, "admin", 0)
		if err != nil {
			t.Fatalf("CreateInvitation: %v", err)
		}
		info, err := svc.InvitationInfo(ctx, inv.Token)
		if err != nil {
			t.Fatalf("InvitationInfo: %v", err)
		}
		if info.State != InvitationPending {
			t.Fatalf("新邀请状态应 pending, got %q", info.State)
		}

		// 直接把 expires_at 改为过去 → expired。
		past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		if _, err := db.Exec(`UPDATE invitations SET expires_at = ? WHERE id = ?`, past, inv.ID); err != nil {
			t.Fatalf("update expires_at: %v", err)
		}
		info, err = svc.InvitationInfo(ctx, inv.Token)
		if err != nil {
			t.Fatalf("InvitationInfo(expired): %v", err)
		}
		if info.State != InvitationExpired {
			t.Fatalf("过期邀请状态应 expired, got %q", info.State)
		}

		// 注册后同 token → used;记录 UsedAt/UsedBy。
		inv2, err := svc.CreateInvitation(ctx, "admin", 0)
		if err != nil {
			t.Fatalf("CreateInvitation(2): %v", err)
		}
		u, err := svc.Register(ctx, inv2.Token, "alice", "password123")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		info2, err := svc.InvitationInfo(ctx, inv2.Token)
		if err != nil {
			t.Fatalf("InvitationInfo(used): %v", err)
		}
		if info2.State != InvitationUsed {
			t.Fatalf("已用邀请状态应 used, got %q", info2.State)
		}
		if info2.UsedAt == nil || info2.UsedBy != u.ID {
			t.Fatalf("used 邀请应记录 UsedAt 与 UsedBy=注册用户, got %+v", info2)
		}

		// 未知 token → ErrInvitationNotFound。
		if _, err := svc.InvitationInfo(ctx, "no-such-token"); !errors.Is(err, ErrInvitationNotFound) {
			t.Fatalf("未知 token 应 ErrInvitationNotFound, got %v", err)
		}
	})
}

// TestRegisterSuccess 覆盖注册成功路径:返回用户属性、邀请转 used、
// GetUserByUsername 可查、且新账号可经 auth.Login 登录(同一套 argon2 PHC)。
func TestRegisterSuccess(t *testing.T) {
	eachDB(t, func(t *testing.T, db *sql.DB) {
		svc := New(db)
		ctx := context.Background()

		inv, err := svc.CreateInvitation(ctx, "admin", 0)
		if err != nil {
			t.Fatalf("CreateInvitation: %v", err)
		}
		u, err := svc.Register(ctx, inv.Token, "alice", "password123")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if u.Role != RoleMember || u.Status != StatusActive {
			t.Fatalf("用户应 role=member/status=active, got role=%q status=%q", u.Role, u.Status)
		}
		if u.Username != "alice" {
			t.Fatalf("username = %q, want alice", u.Username)
		}

		info, err := svc.InvitationInfo(ctx, inv.Token)
		if err != nil {
			t.Fatalf("InvitationInfo: %v", err)
		}
		if info.State != InvitationUsed {
			t.Fatalf("注册后邀请应 used, got %q", info.State)
		}

		got, err := svc.GetUserByUsername(ctx, "alice")
		if err != nil {
			t.Fatalf("GetUserByUsername: %v", err)
		}
		if got.ID != u.ID {
			t.Fatalf("GetUserByUsername id = %q, want %q", got.ID, u.ID)
		}

		users, err := svc.ListUsers(ctx)
		if err != nil {
			t.Fatalf("ListUsers: %v", err)
		}
		if len(users) != 1 || users[0].ID != u.ID {
			t.Fatalf("ListUsers 应恰含新用户, got %+v", users)
		}

		// 用该账号登录(auth 与 identity 共用 argon2id 规则)。
		sess, err := auth.NewService(db, nil).Login("alice", "password123")
		if err != nil {
			t.Fatalf("Login(alice): %v", err)
		}
		if sess.Role != auth.RoleMember || sess.UserID != u.ID {
			t.Fatalf("登录会话身份异常: role=%q userID=%q", sess.Role, sess.UserID)
		}
	})
}

// TestRegisterErrors 覆盖注册全部错误分支(返回稳定哨兵错误)。
func TestRegisterErrors(t *testing.T) {
	eachDB(t, func(t *testing.T, db *sql.DB) {
		svc := New(db)
		ctx := context.Background()

		// 未知 token。
		if _, err := svc.Register(ctx, "no-such-token", "alice", "password123"); !errors.Is(err, ErrInvitationNotFound) {
			t.Fatalf("未知 token: 应 ErrInvitationNotFound, got %v", err)
		}

		// 已用 token。
		used, _ := svc.CreateInvitation(ctx, "admin", 0)
		if _, err := svc.Register(ctx, used.Token, "alice", "password123"); err != nil {
			t.Fatalf("Register(首次): %v", err)
		}
		if _, err := svc.Register(ctx, used.Token, "alice2", "password123"); !errors.Is(err, ErrInvitationUsed) {
			t.Fatalf("已用 token: 应 ErrInvitationUsed, got %v", err)
		}

		// 过期 token。
		expired, _ := svc.CreateInvitation(ctx, "admin", 0)
		past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		if _, err := db.Exec(`UPDATE invitations SET expires_at = ? WHERE id = ?`, past, expired.ID); err != nil {
			t.Fatalf("update expires_at: %v", err)
		}
		if _, err := svc.Register(ctx, expired.Token, "bob", "password123"); !errors.Is(err, ErrInvitationExpired) {
			t.Fatalf("过期 token: 应 ErrInvitationExpired, got %v", err)
		}

		// 非法用户名(长度不足 / 含空格 / 含 @)。
		for _, bad := range []string{"ab", "bad name", "has@sign"} {
			inv, _ := svc.CreateInvitation(ctx, "admin", 0)
			if _, err := svc.Register(ctx, inv.Token, bad, "password123"); !errors.Is(err, ErrInvalidUsername) {
				t.Fatalf("用户名 %q: 应 ErrInvalidUsername, got %v", bad, err)
			}
		}

		// 弱口令(<8)。
		weak, _ := svc.CreateInvitation(ctx, "admin", 0)
		if _, err := svc.Register(ctx, weak.Token, "carol", "short"); !errors.Is(err, ErrWeakPassword) {
			t.Fatalf("弱口令: 应 ErrWeakPassword, got %v", err)
		}

		// 用户名占用(普通用户之间)。
		dupInv, _ := svc.CreateInvitation(ctx, "admin", 0)
		if _, err := svc.Register(ctx, dupInv.Token, "alice", "password123"); !errors.Is(err, ErrUsernameTaken) {
			t.Fatalf("用户名占用: 应 ErrUsernameTaken, got %v", err)
		}

		// 用户名与管理员冲突。
		if err := auth.NewService(db, nil).Bootstrap("admin", "testpass", ""); err != nil {
			t.Fatalf("Bootstrap: %v", err)
		}
		adminInv, _ := svc.CreateInvitation(ctx, "admin", 0)
		if _, err := svc.Register(ctx, adminInv.Token, "admin", "password123"); !errors.Is(err, ErrUsernameTaken) {
			t.Fatalf("与管理员名冲突: 应 ErrUsernameTaken, got %v", err)
		}
	})
}

// TestMemberRelations 覆盖项目成员关系:幂等加入、查询、按用户列项目、移除。
func TestMemberRelations(t *testing.T) {
	eachDB(t, func(t *testing.T, db *sql.DB) {
		svc := New(db)
		ctx := context.Background()
		projID := seedProject(t, db, "proj-1")
		u := registerUser(t, svc, "bob", "password123")

		// 幂等:重复加入不报错。
		if err := svc.AddMember(ctx, projID, u.ID); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
		if err := svc.AddMember(ctx, projID, u.ID); err != nil {
			t.Fatalf("AddMember(重复): %v", err)
		}
		members, err := svc.ListProjectMembers(ctx, projID)
		if err != nil {
			t.Fatalf("ListProjectMembers: %v", err)
		}
		if len(members) != 1 || members[0].ID != u.ID {
			t.Fatalf("幂等加入后应恰 1 条成员, got %+v", members)
		}

		ok, err := svc.IsMember(ctx, u.ID, projID)
		if err != nil || !ok {
			t.Fatalf("IsMember 应为 true: ok=%v err=%v", ok, err)
		}
		ok, err = svc.IsMember(ctx, u.ID, "other-project")
		if err != nil || ok {
			t.Fatalf("非成员 IsMember 应为 false: ok=%v err=%v", ok, err)
		}

		ids, err := svc.ListUserProjectIDs(ctx, u.ID)
		if err != nil {
			t.Fatalf("ListUserProjectIDs: %v", err)
		}
		if len(ids) != 1 || ids[0] != projID {
			t.Fatalf("用户项目列表应含 %s, got %v", projID, ids)
		}

		// 移除 → IsMember false;重复移除 → ErrMemberNotFound。
		if err := svc.RemoveMember(ctx, projID, u.ID); err != nil {
			t.Fatalf("RemoveMember: %v", err)
		}
		if ok, _ := svc.IsMember(ctx, u.ID, projID); ok {
			t.Fatal("移除后 IsMember 应为 false")
		}
		if err := svc.RemoveMember(ctx, projID, u.ID); !errors.Is(err, ErrMemberNotFound) {
			t.Fatalf("重复移除应 ErrMemberNotFound, got %v", err)
		}
	})
}

// TestAddMemberErrors 覆盖 AddMember 的前置校验错误。
func TestAddMemberErrors(t *testing.T) {
	eachDB(t, func(t *testing.T, db *sql.DB) {
		svc := New(db)
		ctx := context.Background()
		projID := seedProject(t, db, "proj-1")
		u := registerUser(t, svc, "carol", "password123")

		// 用户不存在。
		if err := svc.AddMember(ctx, projID, "no-such-user"); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("不存在 userID: 应 ErrUserNotFound, got %v", err)
		}
		// 项目不存在。
		if err := svc.AddMember(ctx, "no-such-project", u.ID); !errors.Is(err, ErrProjectNotFound) {
			t.Fatalf("不存在 projectID: 应 ErrProjectNotFound, got %v", err)
		}
		// 空 projectID。
		if err := svc.AddMember(ctx, "", u.ID); !errors.Is(err, ErrProjectNotFound) {
			t.Fatalf("空 projectID: 应 ErrProjectNotFound, got %v", err)
		}
	})
}

// TestProjectDeleteCascadesMembers 覆盖项目删除时成员关系被级联清除。
func TestProjectDeleteCascadesMembers(t *testing.T) {
	eachDB(t, func(t *testing.T, db *sql.DB) {
		svc := New(db)
		ctx := context.Background()
		projID := seedProject(t, db, "proj-1")
		u := registerUser(t, svc, "dave", "password123")
		if err := svc.AddMember(ctx, projID, u.ID); err != nil {
			t.Fatalf("AddMember: %v", err)
		}

		if _, err := db.Exec(`DELETE FROM projects WHERE id = ?`, projID); err != nil {
			t.Fatalf("delete project: %v", err)
		}

		ids, err := svc.ListUserProjectIDs(ctx, u.ID)
		if err != nil {
			t.Fatalf("ListUserProjectIDs: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("项目删除后用户项目列表应为空, got %v", ids)
		}
	})
}
