package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/identity"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

// TestPathSegments 覆盖路径切段边界。
func TestPathSegments(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"/", nil},
		{"", nil},
		{"///", nil},
		{"/api/runs/x/", []string{"api", "runs", "x"}},
		{"api/runs", []string{"api", "runs"}},
		{"/api/projects/p1/members", []string{"api", "projects", "p1", "members"}},
	}
	for _, c := range cases {
		got := pathSegments(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("pathSegments(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("pathSegments(%q) = %v, want %v", c.in, got, c.want)
			}
		}
	}
}

// TestMemberAllowed 表驱动校验白名单:白名单项放行、其余默认拒绝(段数须完全一致)。
func TestMemberAllowed(t *testing.T) {
	allowed := []struct{ method, path string }{
		{http.MethodGet, "/api/account/sessions"},
		{http.MethodPost, "/api/account/password"},
		{http.MethodDelete, "/api/account/sessions/abc"},
		{http.MethodGet, "/api/projects"},
		{http.MethodGet, "/api/projects/p1/parameters"},
		{http.MethodPost, "/api/projects/p1/runs"},
		{http.MethodGet, "/api/runs"},
		{http.MethodGet, "/api/runs/r1"},
		{http.MethodGet, "/api/runs/r1/events"},
		{http.MethodGet, "/api/runs/r1/logs"},
		{http.MethodGet, "/api/runs/r1/artifacts"},
		{http.MethodGet, "/api/runs/r1/artifacts/a1/download"},
	}
	for _, c := range allowed {
		if !memberAllowed(c.method, pathSegments(c.path)) {
			t.Errorf("应放行: %s %s", c.method, c.path)
		}
	}

	denied := []struct{ method, path string }{
		// 关键负例:diff 段数(4)不匹配任何白名单模式 → 拒绝。
		{http.MethodGet, "/api/runs/r1/diff"},
		{http.MethodGet, "/api/runs/r1/steps"},
		// /api/runs/{id}(3 段)放行,但多一段即拒。
		{http.MethodPost, "/api/projects/p1"},
		{http.MethodGet, "/api/credentials"},
		{http.MethodGet, "/api/users"},
		// 参数端点只放行 GET。
		{http.MethodPut, "/api/projects/p1/parameters"},
		{http.MethodGet, "/api/projects/p1/members"},
		{http.MethodGet, "/api/invitations"},
		{http.MethodGet, "/api/projects/p1/diff"},
	}
	for _, c := range denied {
		if memberAllowed(c.method, pathSegments(c.path)) {
			t.Errorf("应拒绝: %s %s", c.method, c.path)
		}
	}
}

// runMemberGate 直接调用 memberGate(注入 session 到 context),返回状态码与 next 是否被调用。
func runMemberGate(id identity.Service, runs run.Service, r *http.Request) (int, bool) {
	rec := httptest.NewRecorder()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	memberGate(id, runs, next).ServeHTTP(rec, r)
	return rec.Code, called
}

// gateRequest 构造请求并把 session 注入 context(未导出 contextKeySession 同包可见)。
func gateRequest(method, target string, sess *auth.Session) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	if sess != nil {
		r = r.WithContext(context.WithValue(r.Context(), contextKeySession, sess))
	}
	return r
}

// TestMemberGate 覆盖网关:管理员/无会话放行,普通用户白名单+归属校验,默认拒绝。
func TestMemberGate(t *testing.T) {
	// 数据:项目 A、B + A 内运行 + 仅为 A 成员的普通用户。
	db := storetest.OpenDB(t)
	idSvc := identity.New(db)
	runSvc := run.New(db)
	ctx := context.Background()

	projA := seedIdentityProject(t, db, "proj-a", "A")
	projB := seedIdentityProject(t, db, "proj-b", "B")
	runA, err := runSvc.Create(ctx, projA, run.Trigger{Type: run.TriggerManual, Branch: "main"})
	if err != nil {
		t.Fatalf("create runA: %v", err)
	}
	runB, err := runSvc.Create(ctx, projB, run.Trigger{Type: run.TriggerManual, Branch: "main"})
	if err != nil {
		t.Fatalf("create runB: %v", err)
	}
	member := registerMember(t, idSvc, "gate_member", "password123")
	if err := idSvc.AddMember(ctx, projA, member.ID); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	memberSess := &auth.Session{Role: roleMember, UserID: member.ID, Username: "gate_member"}
	adminSess := &auth.Session{Role: auth.RoleAdmin, Username: "admin"}

	t.Run("admin_passes_through", func(t *testing.T) {
		code, called := runMemberGate(idSvc, runSvc, gateRequest(http.MethodGet, "/api/users", adminSess))
		if !called || code != http.StatusOK {
			t.Fatalf("管理员应放行: code=%d called=%v", code, called)
		}
	})

	t.Run("no_session_passes_through", func(t *testing.T) {
		code, called := runMemberGate(idSvc, runSvc, gateRequest(http.MethodGet, "/api/users", nil))
		if !called || code != http.StatusOK {
			t.Fatalf("无会话应交由后续处理: code=%d called=%v", code, called)
		}
	})

	t.Run("member_whitelist_non_project_allowed", func(t *testing.T) {
		// 账户端点不涉项目归属;省略 identity/runs 也应放行。
		code, called := runMemberGate(nil, nil, gateRequest(http.MethodGet, "/api/account/sessions", memberSess))
		if !called || code != http.StatusOK {
			t.Fatalf("白名单账户端点应放行: code=%d called=%v", code, called)
		}
	})

	t.Run("member_not_in_allowlist_403", func(t *testing.T) {
		// 省略 identity/runs 也不能 panic。
		code, called := runMemberGate(nil, nil, gateRequest(http.MethodGet, "/api/users", memberSess))
		if called || code != http.StatusForbidden {
			t.Fatalf("白名单外应 403: code=%d called=%v", code, called)
		}
	})

	t.Run("member_project_member_allowed", func(t *testing.T) {
		code, called := runMemberGate(idSvc, runSvc, gateRequest(http.MethodGet, "/api/projects/"+projA+"/parameters", memberSess))
		if !called || code != http.StatusOK {
			t.Fatalf("项目成员应放行: code=%d called=%v", code, called)
		}
	})

	t.Run("member_project_non_member_403", func(t *testing.T) {
		code, called := runMemberGate(idSvc, runSvc, gateRequest(http.MethodGet, "/api/projects/"+projB+"/parameters", memberSess))
		if called || code != http.StatusForbidden {
			t.Fatalf("非项目成员应 403: code=%d called=%v", code, called)
		}
	})

	t.Run("member_run_of_member_project_allowed", func(t *testing.T) {
		code, called := runMemberGate(idSvc, runSvc, gateRequest(http.MethodGet, "/api/runs/"+runA.ID, memberSess))
		if !called || code != http.StatusOK {
			t.Fatalf("所属项目运行应放行: code=%d called=%v", code, called)
		}
	})

	t.Run("member_run_of_other_project_403", func(t *testing.T) {
		code, called := runMemberGate(idSvc, runSvc, gateRequest(http.MethodGet, "/api/runs/"+runB.ID, memberSess))
		if called || code != http.StatusForbidden {
			t.Fatalf("非所属项目运行应 403: code=%d called=%v", code, called)
		}
	})

	t.Run("member_run_not_found_403", func(t *testing.T) {
		code, called := runMemberGate(idSvc, runSvc, gateRequest(http.MethodGet, "/api/runs/no-such-run", memberSess))
		if called || code != http.StatusForbidden {
			t.Fatalf("不存在的运行应 403(不泄露存在性): code=%d called=%v", code, called)
		}
	})

	t.Run("member_run_diff_403", func(t *testing.T) {
		code, called := runMemberGate(idSvc, runSvc, gateRequest(http.MethodGet, "/api/runs/"+runA.ID+"/diff", memberSess))
		if called || code != http.StatusForbidden {
			t.Fatalf("diff 应被拒绝: code=%d called=%v", code, called)
		}
	})
}
