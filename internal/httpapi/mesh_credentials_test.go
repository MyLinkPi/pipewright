// mesh_credentials_test.go 覆盖临时凭据管理面与 Secure Mode 统一开关:
//   - 双通道认证:Bearer 令牌(无效/有效/撤销后)与浏览器管理员会话;
//   - 签发/列表/撤销端点(fake 签发器注入,不触真机 easytier-cli);
//   - API 令牌 CRUD;
//   - 统一开关:单节点 armed 流程、状态聚合、入站端点的验签 + switch_allowed ACL。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/mesh/easytier"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/pushsource"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// fakeCredIssuer 注入可控签发器(记录调用、返回固定 secret;List 返回预置活跃表)。
type fakeCredIssuer struct {
	generated   []int64
	revoked     []string
	secret      string
	listEntries []easytier.CredentialEntry
	listErr     error
}

func (f *fakeCredIssuer) Generate(_ context.Context, ttl int64, _ string) (string, error) {
	f.generated = append(f.generated, ttl)
	return f.secret, nil
}

func (f *fakeCredIssuer) List(context.Context) ([]easytier.CredentialEntry, error) {
	return f.listEntries, f.listErr
}

func (f *fakeCredIssuer) Revoke(_ context.Context, id string) error {
	f.revoked = append(f.revoked, id)
	return nil
}

// credRuntime 包装 fakeMeshRuntime,带上签发器。
type credRuntime struct {
	fakeMeshRuntime
	issuer *fakeCredIssuer
}

func (r *credRuntime) CredentialProvider() easytier.CredentialIssuer { return r.issuer }

// setupCredServer 构造带 fake 签发器与统一开关协调器的 server。
func setupCredServer(t *testing.T) (*httptest.Server, mesh.Service, *fakeCredIssuer) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	psvc := project.New(st.DB, v, stubProber{branch: "main"})
	rsvc := run.New(st.DB)
	pool := run.NewWorkerPool(rsvc)
	pool.Start()
	t.Cleanup(func() { pool.Stop(context.Background()) })

	meshSvc := mesh.New(st.DB, v)
	revs := pushsource.New(st.DB)
	blobs, err := artifactstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("artifactstore: %v", err)
	}
	receiver := mesh.NewReceiver(psvc, blobs, nil, revs)
	issuer := &fakeCredIssuer{secret: "SECRET-xyz"}
	rt := &credRuntime{fakeMeshRuntime: fakeMeshRuntime{status: &easytier.Status{OK: true}}, issuer: issuer}
	deps := NewMeshDeps(meshSvc, receiver, mesh.NewClient(meshSvc), psvc, rsvc, pool,
		nil, nil, nil, nil, revs, nil, 8080).
		WithRuntime(rt).WithModeSwitcher(mesh.NewModeSwitcher(meshSvc, mesh.NewClient(meshSvc)))
	srv := httptest.NewServer(New(testWebFSAuth(), svc,
		WithVault(v), WithProjects(psvc), WithRuns(rsvc, pool), WithMesh(deps)))
	t.Cleanup(srv.Close)
	return srv, meshSvc, issuer
}

// enableSecureMesh 启用集群并打开 Secure Mode。
func enableSecureMesh(t *testing.T, svc mesh.Service) *mesh.Settings {
	t.Helper()
	enabled, secure := true, true
	st, err := svc.SaveSettings(context.Background(), mesh.SaveSettingsInput{
		Enabled: &enabled, SecureMode: &secure,
	})
	if err != nil {
		t.Fatalf("enable secure mesh: %v", err)
	}
	return st
}

func bearerDo(t *testing.T, method, url string, body any, token string) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// TestMeshCredentialAuthChannels 双通道认证:无凭证 401;伪令牌 401;有效令牌 200;
// 撤销后令牌立即失效;浏览器管理员会话(CSRF 缺失 403 → 补齐 200)。
func TestMeshCredentialAuthChannels(t *testing.T) {
	srv, meshSvc, issuer := setupCredServer(t)
	enableSecureMesh(t, meshSvc)
	_, plaintext, err := meshSvc.CreateApiToken(context.Background(), "ci")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	// 无认证。
	resp := bearerDo(t, http.MethodPost, srv.URL+"/api/mesh/credentials", map[string]any{"ttlSeconds": 3600}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无认证 status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	// 伪令牌。
	resp = bearerDo(t, http.MethodPost, srv.URL+"/api/mesh/credentials", map[string]any{"ttlSeconds": 3600}, "pwmc_notreal")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("伪令牌 status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// 有效令牌签发。
	resp = bearerDo(t, http.MethodPost, srv.URL+"/api/mesh/credentials", map[string]any{"ttlSeconds": 3600, "remark": "ci 测试"}, plaintext)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("令牌签发 status = %d, want 200", resp.StatusCode)
	}
	var issued struct {
		CredentialID     string `json:"credentialId"`
		CredentialSecret string `json:"credentialSecret"`
		ExpiresAt        string `json:"expiresAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issued); err != nil || issued.CredentialSecret != "SECRET-xyz" || issued.CredentialID == "" {
		t.Fatalf("签发响应异常:%v %+v", err, issued)
	}
	if len(issuer.generated) != 1 || issuer.generated[0] != 3600 {
		t.Fatalf("签发器应被调用一次(ttl=3600):%v", issuer.generated)
	}

	// 撤销令牌 → 立即失效。
	if err := meshSvc.RevokeApiToken(context.Background(), "ci"); err != nil {
		t.Fatalf("revoke token: %v", err)
	}
	resp = bearerDo(t, http.MethodPost, srv.URL+"/api/mesh/credentials", map[string]any{}, plaintext)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("撤销后 status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// 浏览器路径:登录 → 无 CSRF 的 POST 403 → 带 CSRF 200。
	client := newTestClient(t)
	csrf := loginAs(t, client, srv.URL, "admin", "testpass")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/mesh/credentials", strings.NewReader(`{"ttlSeconds":7200}`))
	req.Header.Set("Content-Type", "application/json")
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF status = %d, want 403", resp2.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/mesh/credentials", strings.NewReader(`{"ttlSeconds":7200}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp3, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("管理员会话签发 status = %d, want 200", resp3.StatusCode)
	}
}

// TestMeshCredentialListAndRevoke 列表合并(revoked/active/missing + external)与撤销。
func TestMeshCredentialListAndRevoke(t *testing.T) {
	srv, meshSvc, issuer := setupCredServer(t)
	enableSecureMesh(t, meshSvc)
	ctx := context.Background()

	revoked, _ := meshSvc.IssueCredentialRecord(ctx, "c-revoked", "已撤销", 3600, "admin", "admin")
	_ = meshSvc.MarkCredentialRevoked(ctx, "c-revoked")
	_, _ = meshSvc.IssueCredentialRecord(ctx, "c-active", "在线", 3600, "admin", "admin")
	_, _ = meshSvc.IssueCredentialRecord(ctx, "c-missing", "丢失", 90*24*3600, "admin", "admin")
	_ = revoked
	issuer.listEntries = []easytier.CredentialEntry{
		{CredentialID: "c-active"},
		{CredentialID: "c-external"}, // 外部签发:仅存在于 easytier。
	}

	client := newTestClient(t)
	csrf := loginAs(t, client, srv.URL, "admin", "testpass")
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/mesh/credentials", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var out struct {
		Items []struct {
			CredentialID string `json:"credentialId"`
			Status       string `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	statusOf := map[string]string{}
	for _, it := range out.Items {
		statusOf[it.CredentialID] = it.Status
	}
	if statusOf["c-revoked"] != "revoked" || statusOf["c-active"] != "active" ||
		statusOf["c-missing"] != "missing" || statusOf["c-external"] != "active" {
		t.Fatalf("状态合并异常:%v", statusOf)
	}

	// 撤销:issuer.Revoke 被调 + 登记簿标记。
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/mesh/credentials/c-active/revoke", nil)
	req.Header.Set("X-CSRF-Token", csrf)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d", resp2.StatusCode)
	}
	if len(issuer.revoked) != 1 || issuer.revoked[0] != "c-active" {
		t.Fatalf("签发器撤销未触达:%v", issuer.revoked)
	}
}

// TestMeshApiTokenEndpoints API 令牌 CRUD(管理员会话):创建返回明文一次、重名 422、
// 列表无哈希、撤销 404 幂等语义。
func TestMeshApiTokenEndpoints(t *testing.T) {
	srv, meshSvc, _ := setupCredServer(t)
	enableMesh(t, meshSvc)
	client := newTestClient(t)
	csrf := loginAs(t, client, srv.URL, "admin", "testpass")

	do := func(method, path string, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	resp := do(http.MethodPost, "/api/mesh/api-tokens", `{"name":"bot"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var created struct {
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || !strings.HasPrefix(created.Token, "pwmc_") {
		t.Fatalf("创建响应异常:%v %+v", err, created)
	}
	resp = do(http.MethodPost, "/api/mesh/api-tokens", `{"name":"bot"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("重名 status = %d, want 422", resp.StatusCode)
	}
	resp = do(http.MethodGet, "/api/mesh/api-tokens", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	resp = do(http.MethodDelete, "/api/mesh/api-tokens/bot", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp = do(http.MethodDelete, "/api/mesh/api-tokens/bot", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除 status = %d, want 404", resp.StatusCode)
	}
	_ = meshSvc
}

// TestMeshSecureModeSwitchSingleNode 统一开关 HTTP 面:发起 → armed + 本端指令;
// 状态聚合含本端行;撤销后指令清除。
func TestMeshSecureModeSwitchSingleNode(t *testing.T) {
	srv, meshSvc, _ := setupCredServer(t)
	enableMesh(t, meshSvc)
	client := newTestClient(t)
	csrf := loginAs(t, client, srv.URL, "admin", "testpass")
	do := func(method, path, body string) (int, *http.Response) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp.StatusCode, resp
	}

	status, resp := do(http.MethodPost, "/api/mesh/secure-mode/switch", `{"action":"enable","effectiveInMinutes":2}`)
	body, _ := readAll(t, resp)
	if status != http.StatusOK {
		t.Fatalf("switch status = %d body %s", status, body)
	}
	var res struct {
		ChangeID    string `json:"changeId"`
		EffectiveAt string `json:"effectiveAt"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.ChangeID == "" {
		t.Fatalf("switch 响应异常:%s", body)
	}
	if pending, _ := meshSvc.GetModePending(context.Background()); pending == nil || pending.ChangeID != res.ChangeID {
		t.Fatalf("本端指令未落库:%+v", pending)
	}

	status, resp = do(http.MethodGet, "/api/mesh/secure-mode/status", "")
	body, _ = readAll(t, resp)
	resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	var st struct {
		SecureMode bool `json:"secureMode"`
		Pending    *struct {
			ChangeID  string `json:"changeId"`
			AppliedAt string `json:"appliedAt"`
		} `json:"pending"`
		Nodes []struct {
			IsSelf bool `json:"isSelf"`
			Acked  bool `json:"acked"`
		} `json:"nodes"`
		Skipped []string `json:"skipped"`
	}
	if err := json.Unmarshal(body, &st); err != nil || len(st.Nodes) != 1 || !st.Nodes[0].IsSelf || !st.Nodes[0].Acked {
		t.Fatalf("状态聚合异常:%s", body)
	}
	// 未应用必须是空串(不能是 time.Time 零值 "0001-01-01T00:00:00Z" —— 前端按空串
	// 判「未应用」,零值串会恒判为已应用,撤销/轮询全失效)。
	if st.Pending == nil || st.Pending.ChangeID != res.ChangeID || st.Pending.AppliedAt != "" {
		t.Fatalf("pending 形态异常(未应用应输出空 appliedAt):%s", body)
	}
	// 无虚拟 IP 的节点清单:无则空数组(非 null),前端可直接判 length。
	if st.Skipped == nil || len(st.Skipped) != 0 {
		t.Fatalf("skipped 应为空数组:%s", body)
	}

	status, resp = do(http.MethodPost, "/api/mesh/secure-mode/abort", `{"changeId":"`+res.ChangeID+`"}`)
	resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("abort status = %d", status)
	}
	if pending, _ := meshSvc.GetModePending(context.Background()); pending != nil {
		t.Fatal("abort 后本端指令应清除")
	}
}

func readAll(t *testing.T, resp *http.Response) ([]byte, *http.Response) {
	t.Helper()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	resp.Body.Close()
	return buf.Bytes(), resp
}

// TestMeshModeChangeInbound 入站端点:验签失败 401;switch_allowed 默认拒绝 403;
// 授权后下发 200 + 指令落库;GET 上报本端态;DELETE 按 ID 清除。
func TestMeshModeChangeInbound(t *testing.T) {
	srv, meshSvc, _ := setupCredServer(t)
	st := enableMesh(t, meshSvc)
	nodeID, priv := registerPeer(t, meshSvc, "switcher")
	base := srv.URL

	effAt := time.Now().UTC().Add(3 * time.Minute).Format(time.RFC3339)
	payload, _ := json.Marshal(mesh.ModeChangeMsg{ChangeID: "ch-x", Action: mesh.ModeActionEnable, EffectiveAt: effAt, InitiatedBy: nodeID})

	// 无签名头 → 401。
	resp := bearerDo(t, http.MethodPost, base+"/api/mesh/mode-change", json.RawMessage(payload), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无签名 status = %d, want 401", resp.StatusCode)
	}
	// 签名有效但 ACL 默认拒绝 → 403。
	resp = signedDo(t, http.MethodPost, base+"/api/mesh/mode-change", "/api/mesh/mode-change", payload, nodeID, priv)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("默认拒绝 status = %d, want 403", resp.StatusCode)
	}
	// 授权 switch_allowed → 200 + 指令落库。
	acl, _ := meshSvc.ACL(context.Background())
	acl.SwitchAllowed = []string{nodeID}
	if err := meshSvc.SaveACL(context.Background(), *acl); err != nil {
		t.Fatalf("save acl: %v", err)
	}
	resp = signedDo(t, http.MethodPost, base+"/api/mesh/mode-change", "/api/mesh/mode-change", payload, nodeID, priv)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("授权后 status = %d, want 200", resp.StatusCode)
	}
	if pending, _ := meshSvc.GetModePending(context.Background()); pending == nil || pending.ChangeID != "ch-x" {
		t.Fatalf("指令未落库:%+v", pending)
	}

	// GET 上报本端态。
	resp = signedDo(t, http.MethodGet, base+"/api/mesh/mode-change", "/api/mesh/mode-change", nil, nodeID, priv)
	var state mesh.ModeRemoteState
	_ = json.NewDecoder(resp.Body).Decode(&state)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || state.NodeID != st.NodeID || state.Pending == nil || state.Pending.ChangeID != "ch-x" {
		t.Fatalf("本端态上报异常:status %d %+v", resp.StatusCode, state)
	}

	// DELETE 按 ID 清除(错 ID 不清,对 ID 清)。
	resp = signedDo(t, http.MethodDelete, base+"/api/mesh/mode-change/other", "/api/mesh/mode-change/other", nil, nodeID, priv)
	resp.Body.Close()
	if pending, _ := meshSvc.GetModePending(context.Background()); pending == nil {
		t.Fatal("错 ID 不应清除")
	}
	resp = signedDo(t, http.MethodDelete, base+"/api/mesh/mode-change/ch-x", "/api/mesh/mode-change/ch-x", nil, nodeID, priv)
	resp.Body.Close()
	if pending, _ := meshSvc.GetModePending(context.Background()); pending != nil {
		t.Fatal("对 ID 应清除")
	}
}
