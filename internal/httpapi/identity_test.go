package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/identity"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// identityTestEnv 是用户体系 HTTP 测试环境(真库 + 真 httptest server)。
type identityTestEnv struct {
	srv    *httptest.Server
	db     *sql.DB
	idSvc  identity.Service
	runSvc run.Service
}

// setupIdentityServer 构造带 auth + identity + project + run + parameters 的测试 server,
// 并引导 admin/testpass。
func setupIdentityServer(t *testing.T) *identityTestEnv {
	t.Helper()
	st := testStoreAuth(t)
	db := st.DB
	authSvc := auth.NewService(db, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(db, testMasterKey())
	psvc := project.New(db, v, stubProber{branch: "main"})
	rsvc := run.New(db)
	idSvc := identity.New(db)
	paramSvc := run.NewParameterService(db)

	srv := httptest.NewServer(New(testWebFSAuth(), authSvc,
		WithIdentity(idSvc), WithVault(v), WithProjects(psvc),
		WithRuns(rsvc, nil), WithParameters(paramSvc)))
	t.Cleanup(srv.Close)
	return &identityTestEnv{srv: srv, db: db, idSvc: idSvc, runSvc: rsvc}
}

// seedIdentityProject 直插一条凭据 + 项目(满足外键),返回 project id。
func seedIdentityProject(t *testing.T, db *sql.DB, id, name string) string {
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
		id, name, credID, now, now,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

// registerMember 经 identity 服务建邀请并注册一个普通用户。
func registerMember(t *testing.T, idSvc identity.Service, username, password string) *identity.User {
	t.Helper()
	inv, err := idSvc.CreateInvitation(context.Background(), "admin", 0)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	u, err := idSvc.Register(context.Background(), inv.Token, username, password)
	if err != nil {
		t.Fatalf("Register(%s): %v", username, err)
	}
	return u
}

// loginAs 以指定用户名/口令登录,返回 csrf token。
func loginAs(t *testing.T, client *http.Client, srvURL, username, password string) string {
	t.Helper()
	body := strings.NewReader(`{"username":"` + username + `","password":"` + password + `"}`)
	resp, err := client.Post(srvURL+"/api/auth/login", "application/json", body)
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login %s: status = %d, want 200", username, resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieCsrf {
			return c.Value
		}
	}
	t.Fatalf("csrf cookie not found for %s", username)
	return ""
}

func hasCookie(resp *http.Response, name string) bool {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return true
		}
	}
	return false
}

// TestRegisterEndpointSuccess 注册成功:201 + 下发会话 cookie + 自动登录 + 会话回显 member。
func TestRegisterEndpointSuccess(t *testing.T) {
	env := setupIdentityServer(t)
	client := newTestClient(t)

	inv, err := env.idSvc.CreateInvitation(context.Background(), "admin", 0)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	const password = "passw0rd123"
	resp := doJSON(t, client, http.MethodPost, env.srv.URL+"/api/auth/register", "",
		`{"token":"`+inv.Token+`","username":"alice","password":"`+password+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("register status = %d, want 201; body=%s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), password) {
		t.Fatalf("注册响应泄漏明文口令: %s", raw)
	}
	var body map[string]string
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["username"] != "alice" || body["role"] != "member" {
		t.Fatalf("注册响应身份异常: %+v", body)
	}
	if !hasCookie(resp, cookieSession) {
		t.Fatal("注册成功后应下发会话 cookie(自动登录)")
	}

	// 会话端点回显身份(自动登录生效)。
	sessResp := doJSON(t, client, http.MethodGet, env.srv.URL+"/api/auth/session", "", "")
	defer sessResp.Body.Close()
	if sessResp.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d, want 200", sessResp.StatusCode)
	}
	var sess map[string]string
	_ = json.NewDecoder(sessResp.Body).Decode(&sess)
	if sess["username"] != "alice" || sess["role"] != "member" {
		t.Fatalf("会话身份异常: %+v", sess)
	}
}

// TestRegisterEndpointErrors 注册错误分支 → 稳定状态码 + 错误码。
func TestRegisterEndpointErrors(t *testing.T) {
	env := setupIdentityServer(t)
	client := newTestClient(t)
	register := func(token, username, password string) *http.Response {
		return doJSON(t, client, http.MethodPost, env.srv.URL+"/api/auth/register", "",
			`{"token":"`+token+`","username":"`+username+`","password":"`+password+`"}`)
	}

	t.Run("unknown_token_404", func(t *testing.T) {
		// 用户名合法性先于 token 校验,故此处用合法用户名以确保命中 token 分支。
		resp := register("no-such-token", "unknown1", "password123")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != "invitation_invalid" {
			t.Fatalf("error code = %q, want invitation_invalid", code)
		}
	})

	t.Run("used_token_409", func(t *testing.T) {
		inv, _ := env.idSvc.CreateInvitation(context.Background(), "admin", 0)
		first := register(inv.Token, "used1", "password123")
		first.Body.Close()
		if first.StatusCode != http.StatusCreated {
			t.Fatalf("首次注册 status = %d, want 201", first.StatusCode)
		}
		resp := register(inv.Token, "used2", "password123")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != "invitation_used" {
			t.Fatalf("error code = %q, want invitation_used", code)
		}
	})

	t.Run("expired_token_410", func(t *testing.T) {
		inv, _ := env.idSvc.CreateInvitation(context.Background(), "admin", 0)
		past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		if _, err := env.db.Exec(`UPDATE invitations SET expires_at = ? WHERE id = ?`, past, inv.ID); err != nil {
			t.Fatalf("update expires_at: %v", err)
		}
		resp := register(inv.Token, "exp1", "password123")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusGone {
			t.Fatalf("status = %d, want 410", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != "invitation_expired" {
			t.Fatalf("error code = %q, want invitation_expired", code)
		}
	})

	t.Run("username_taken_409", func(t *testing.T) {
		inv1, _ := env.idSvc.CreateInvitation(context.Background(), "admin", 0)
		first := register(inv1.Token, "dup", "password123")
		first.Body.Close()
		if first.StatusCode != http.StatusCreated {
			t.Fatalf("首次注册 status = %d, want 201", first.StatusCode)
		}
		inv2, _ := env.idSvc.CreateInvitation(context.Background(), "admin", 0)
		resp := register(inv2.Token, "dup", "password123")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != "username_taken" {
			t.Fatalf("error code = %q, want username_taken", code)
		}
	})

	t.Run("invalid_username_422", func(t *testing.T) {
		inv, _ := env.idSvc.CreateInvitation(context.Background(), "admin", 0)
		resp := register(inv.Token, "ab", "password123")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != "invalid_username" {
			t.Fatalf("error code = %q, want invalid_username", code)
		}
	})

	t.Run("weak_password_422", func(t *testing.T) {
		inv, _ := env.idSvc.CreateInvitation(context.Background(), "admin", 0)
		resp := register(inv.Token, "weakuser", "short")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != "weak_password" {
			t.Fatalf("error code = %q, want weak_password", code)
		}
	})
}

// TestInvitationInfoEndpoint 公开的邀请状态查询:pending 与未知 token(404)。
func TestInvitationInfoEndpoint(t *testing.T) {
	env := setupIdentityServer(t)
	client := newTestClient(t)

	inv, _ := env.idSvc.CreateInvitation(context.Background(), "admin", 0)
	resp := doJSON(t, client, http.MethodGet, env.srv.URL+"/api/auth/invitations/"+inv.Token, "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["state"] != "pending" {
		t.Fatalf("state = %q, want pending", body["state"])
	}

	unknown := doJSON(t, client, http.MethodGet, env.srv.URL+"/api/auth/invitations/no-such-token", "", "")
	defer unknown.Body.Close()
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("未知 token status = %d, want 404", unknown.StatusCode)
	}
	if code := errorCode(t, unknown); code != "invitation_invalid" {
		t.Fatalf("error code = %q, want invitation_invalid", code)
	}
}

// TestAdminInvitationEndpoints 管理员:创建/列表/撤销邀请,用户列表。
func TestAdminInvitationEndpoints(t *testing.T) {
	env := setupIdentityServer(t)
	client := newTestClient(t)
	csrf := loginWithClient(t, client, env.srv.URL) // admin/testpass

	// 创建邀请 → 201 含 token/id。
	resp := doJSON(t, client, http.MethodPost, env.srv.URL+"/api/invitations", csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create invitation status = %d, want 201; body=%s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	var inv map[string]any
	_ = json.Unmarshal(raw, &inv)
	token, _ := inv["token"].(string)
	id, _ := inv["id"].(string)
	if token == "" || id == "" {
		t.Fatalf("邀请响应缺 token/id: %s", raw)
	}
	if inv["state"] != "pending" {
		t.Fatalf("invitation state = %v, want pending", inv["state"])
	}

	// 列表 → {invitations:[...]}。
	listResp := doJSON(t, client, http.MethodGet, env.srv.URL+"/api/invitations", "", "")
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list invitations status = %d, want 200", listResp.StatusCode)
	}
	var listBody struct {
		Invitations []map[string]any `json:"invitations"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&listBody)
	if len(listBody.Invitations) != 1 {
		t.Fatalf("invitations len = %d, want 1", len(listBody.Invitations))
	}

	// 用户列表 → {users:[...]},绝不含口令字段。
	usersResp := doJSON(t, client, http.MethodGet, env.srv.URL+"/api/users", "", "")
	defer usersResp.Body.Close()
	if usersResp.StatusCode != http.StatusOK {
		t.Fatalf("list users status = %d, want 200", usersResp.StatusCode)
	}
	uraw, _ := io.ReadAll(usersResp.Body)
	if strings.Contains(string(uraw), "password") {
		t.Fatalf("用户列表泄漏口令字段: %s", uraw)
	}
	var usersBody struct {
		Users []map[string]any `json:"users"`
	}
	_ = json.Unmarshal(uraw, &usersBody)
	if len(usersBody.Users) != 0 {
		t.Fatalf("应 0 个普通用户, got %d", len(usersBody.Users))
	}

	// 撤销 → 204;再撤销同 id → 404 invitation_not_found。
	del := doJSON(t, client, http.MethodDelete, env.srv.URL+"/api/invitations/"+id, csrf, "")
	defer del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", del.StatusCode)
	}
	delAgain := doJSON(t, client, http.MethodDelete, env.srv.URL+"/api/invitations/"+id, csrf, "")
	defer delAgain.Body.Close()
	if delAgain.StatusCode != http.StatusNotFound {
		t.Fatalf("re-revoke status = %d, want 404", delAgain.StatusCode)
	}
	if code := errorCode(t, delAgain); code != "invitation_not_found" {
		t.Fatalf("error code = %q, want invitation_not_found", code)
	}
}

// TestAdminProjectMemberEndpoints 管理员:项目成员的增/查/删(含响应不含明文口令)。
func TestAdminProjectMemberEndpoints(t *testing.T) {
	env := setupIdentityServer(t)
	client := newTestClient(t)
	csrf := loginWithClient(t, client, env.srv.URL)

	projID := seedIdentityProject(t, env.db, "proj-a", "A")
	u := registerMember(t, env.idSvc, "member1", "password123")

	// 添加成员(userId)→ 204。
	add := doJSON(t, client, http.MethodPost, env.srv.URL+"/api/projects/"+projID+"/members", csrf,
		`{"userId":"`+u.ID+`"}`)
	defer add.Body.Close()
	if add.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(add.Body)
		t.Fatalf("add member status = %d, want 204; body=%s", add.StatusCode, raw)
	}

	// 查询成员 → 1 条,且响应无明文口令。
	list := doJSON(t, client, http.MethodGet, env.srv.URL+"/api/projects/"+projID+"/members", "", "")
	defer list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list members status = %d, want 200", list.StatusCode)
	}
	lraw, _ := io.ReadAll(list.Body)
	if strings.Contains(string(lraw), "password") || strings.Contains(string(lraw), "password123") {
		t.Fatalf("成员列表泄漏口令: %s", lraw)
	}
	var mb struct {
		Members []map[string]any `json:"members"`
	}
	_ = json.Unmarshal(lraw, &mb)
	if len(mb.Members) != 1 || mb.Members[0]["username"] != "member1" {
		t.Fatalf("成员列表异常: %s", lraw)
	}

	// 用 username 再加一次(已存在 → 幂等 204)。
	addByName := doJSON(t, client, http.MethodPost, env.srv.URL+"/api/projects/"+projID+"/members", csrf,
		`{"username":"member1"}`)
	defer addByName.Body.Close()
	if addByName.StatusCode != http.StatusNoContent {
		t.Fatalf("add by username status = %d, want 204", addByName.StatusCode)
	}

	// 删除成员 → 204;再删 → 404 member_not_found。
	del := doJSON(t, client, http.MethodDelete, env.srv.URL+"/api/projects/"+projID+"/members/"+u.ID, csrf, "")
	defer del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("remove member status = %d, want 204", del.StatusCode)
	}
	delAgain := doJSON(t, client, http.MethodDelete, env.srv.URL+"/api/projects/"+projID+"/members/"+u.ID, csrf, "")
	defer delAgain.Body.Close()
	if delAgain.StatusCode != http.StatusNotFound {
		t.Fatalf("re-remove status = %d, want 404", delAgain.StatusCode)
	}
	if code := errorCode(t, delAgain); code != "member_not_found" {
		t.Fatalf("error code = %q, want member_not_found", code)
	}
}

// TestMemberForbiddenAdminEndpoints 普通用户访问管理员端点 → 403。
func TestMemberForbiddenAdminEndpoints(t *testing.T) {
	env := setupIdentityServer(t)
	registerMember(t, env.idSvc, "member2", "password123")

	mc := newTestClient(t)
	mcs := loginAs(t, mc, env.srv.URL, "member2", "password123")

	cases := []struct{ name, method, path string }{
		{"users", http.MethodGet, "/api/users"},
		{"invitations_list", http.MethodGet, "/api/invitations"},
		{"invitations_create", http.MethodPost, "/api/invitations"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := doJSON(t, mc, c.method, env.srv.URL+c.path, mcs, "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s %s: status = %d, want 403", c.method, c.path, resp.StatusCode)
			}
		})
	}
}

// TestMemberProjectAndRunScoping 普通用户仅可见其加入的项目与其运行;越权 403;diff 被拒。
func TestMemberProjectAndRunScoping(t *testing.T) {
	env := setupIdentityServer(t)
	ctx := context.Background()

	projA := seedIdentityProject(t, env.db, "proj-a", "A")
	projB := seedIdentityProject(t, env.db, "proj-b", "B")
	runA, err := env.runSvc.Create(ctx, projA, run.Trigger{Type: run.TriggerManual, Branch: "main"})
	if err != nil {
		t.Fatalf("create runA: %v", err)
	}
	if _, err := env.runSvc.Create(ctx, projB, run.Trigger{Type: run.TriggerManual, Branch: "main"}); err != nil {
		t.Fatalf("create runB: %v", err)
	}

	member := registerMember(t, env.idSvc, "member_a", "password123")
	if err := env.idSvc.AddMember(ctx, projA, member.ID); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	mc := newTestClient(t)
	_ = loginAs(t, mc, env.srv.URL, "member_a", "password123")

	// GET /api/projects 只含 A。
	resp := doJSON(t, mc, http.MethodGet, env.srv.URL+"/api/projects", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("projects status = %d, want 200", resp.StatusCode)
	}
	var projects []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&projects)
	if len(projects) != 1 || projects[0]["id"] != projA {
		t.Fatalf("成员项目列表应只含 A, got %+v", projects)
	}

	// 参数端点:成员项目放行(200),非成员 403。
	pa := doJSON(t, mc, http.MethodGet, env.srv.URL+"/api/projects/"+projA+"/parameters", "", "")
	pa.Body.Close()
	if pa.StatusCode != http.StatusOK {
		t.Fatalf("A parameters status = %d, want 200", pa.StatusCode)
	}
	pb := doJSON(t, mc, http.MethodGet, env.srv.URL+"/api/projects/"+projB+"/parameters", "", "")
	pb.Body.Close()
	if pb.StatusCode != http.StatusForbidden {
		t.Fatalf("B parameters status = %d, want 403", pb.StatusCode)
	}

	// GET /api/runs 只含 A 的运行。
	rresp := doJSON(t, mc, http.MethodGet, env.srv.URL+"/api/runs", "", "")
	defer rresp.Body.Close()
	if rresp.StatusCode != http.StatusOK {
		t.Fatalf("runs status = %d, want 200", rresp.StatusCode)
	}
	var runsBody struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	_ = json.NewDecoder(rresp.Body).Decode(&runsBody)
	if runsBody.Total != 1 || len(runsBody.Items) != 1 || runsBody.Items[0]["id"] != runA.ID {
		t.Fatalf("成员运行列表应只含 runA, got %+v", runsBody)
	}

	// 运行详情:所属项目放行;非所属 403;不存在 403(不泄露存在性)。
	getRun := func(id string) int {
		r := doJSON(t, mc, http.MethodGet, env.srv.URL+"/api/runs/"+id, "", "")
		r.Body.Close()
		return r.StatusCode
	}
	if code := getRun(runA.ID); code != http.StatusOK {
		t.Fatalf("runA detail status = %d, want 200", code)
	}
	if code := getRun("no-such-run"); code != http.StatusForbidden {
		t.Fatalf("不存在运行 status = %d, want 403", code)
	}

	// diff 端点对普通用户默认拒绝。
	diff := doJSON(t, mc, http.MethodGet, env.srv.URL+"/api/runs/"+runA.ID+"/diff", "", "")
	diff.Body.Close()
	if diff.StatusCode != http.StatusForbidden {
		t.Fatalf("diff status = %d, want 403", diff.StatusCode)
	}
}

// TestMemberWithoutProjectsSeesEmpty 无任何项目的普通用户:项目/运行列表均为空。
func TestMemberWithoutProjectsSeesEmpty(t *testing.T) {
	env := setupIdentityServer(t)
	registerMember(t, env.idSvc, "lonely", "password123")

	mc := newTestClient(t)
	_ = loginAs(t, mc, env.srv.URL, "lonely", "password123")

	rresp := doJSON(t, mc, http.MethodGet, env.srv.URL+"/api/runs", "", "")
	defer rresp.Body.Close()
	if rresp.StatusCode != http.StatusOK {
		t.Fatalf("runs status = %d, want 200", rresp.StatusCode)
	}
	var runsBody struct {
		Items []any `json:"items"`
		Total int   `json:"total"`
		Page  int   `json:"page"`
	}
	_ = json.NewDecoder(rresp.Body).Decode(&runsBody)
	if runsBody.Total != 0 || len(runsBody.Items) != 0 {
		t.Fatalf("无项目成员的运行列表应为空, got %+v", runsBody)
	}

	presp := doJSON(t, mc, http.MethodGet, env.srv.URL+"/api/projects", "", "")
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusOK {
		t.Fatalf("projects status = %d, want 200", presp.StatusCode)
	}
	var projects []any
	_ = json.NewDecoder(presp.Body).Decode(&projects)
	if len(projects) != 0 {
		t.Fatalf("无项目成员的项目列表应为空, got %+v", projects)
	}
}
