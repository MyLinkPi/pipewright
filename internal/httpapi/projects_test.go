package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/servicereg"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// stubProber 让 HTTP 层测试可控仓库探测结果,不触网。
type stubProber struct {
	branch string
	err    error
}

func (s stubProber) Probe(_ context.Context, _ string, _ vault.GitAuth) (string, error) {
	return s.branch, s.err
}

// setupProjectServer 构造带 auth + vault + project 的测试 server。
func setupProjectServer(t *testing.T, pr project.RemoteProber) (*httptest.Server, *http.Client, string, vault.Vault) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	psvc := project.New(st.DB, v, pr)
	srv := httptest.NewServer(New(testWebFSAuth(), svc, WithVault(v), WithProjects(psvc)))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	return srv, client, csrf, v
}

// newGitCred 经 API 建一个 git_token 凭据,返回 id。
func newGitCred(t *testing.T, client *http.Client, srvURL, csrf, secret string) string {
	t.Helper()
	resp := doJSON(t, client, http.MethodPost, srvURL+"/api/credentials", csrf,
		`{"name":"ci","type":"git_token","secret":"`+secret+`"}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	id, _ := c["id"].(string)
	if id == "" {
		t.Fatalf("create credential failed: %s", raw)
	}
	return id
}

func TestProjectCreateAndList(t *testing.T) {
	srv, client, csrf, _ := setupProjectServer(t, stubProber{branch: "main"})
	credID := newGitCred(t, client, srv.URL, csrf, "ghp_LEAKMARKER_secret")

	body := `{"name":"shop","repoUrl":"https://gitee.com/acme/shop.git","credentialId":"` + credID + `"}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects", csrf, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "LEAKMARKER") {
		t.Fatalf("create 响应泄漏凭据明文: %s", raw)
	}
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	if p["defaultBranch"] != "main" {
		t.Fatalf("defaultBranch = %v, want main(探测)", p["defaultBranch"])
	}
	if p["credentialName"] != "ci" {
		t.Fatalf("credentialName = %v, want ci", p["credentialName"])
	}
	if p["lastRunStatus"] != nil {
		t.Fatalf("lastRunStatus 本期应为 null, got %v", p["lastRunStatus"])
	}
	ts, ok := p["targetServers"].([]any)
	if !ok || len(ts) != 0 {
		t.Fatalf("targetServers 本期应为 [], got %v", p["targetServers"])
	}

	// 列表。
	listResp := doJSON(t, client, http.MethodGet, srv.URL+"/api/projects", "", "")
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listResp.StatusCode)
	}
	listRaw, _ := io.ReadAll(listResp.Body)
	if strings.Contains(string(listRaw), "LEAKMARKER") {
		t.Fatalf("list 泄漏凭据明文: %s", listRaw)
	}
	var list []map[string]any
	_ = json.Unmarshal(listRaw, &list)
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
}

func TestProjectCreateMissingCredential422(t *testing.T) {
	srv, client, csrf, _ := setupProjectServer(t, stubProber{branch: "main"})
	body := `{"name":"x","repoUrl":"https://gitee.com/a/b.git","credentialId":"no-such"}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects", csrf, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	assertErrCode(t, resp, "credential_error")
}

func TestTestCloneCredentialError422(t *testing.T) {
	srv, client, csrf, _ := setupProjectServer(t, stubProber{err: project.ErrCredentialError})
	credID := newGitCred(t, client, srv.URL, csrf, "badtoken_LEAK")
	body := `{"repoUrl":"https://gitee.com/a/b.git","credentialId":"` + credID + `"}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects/test-clone", csrf, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "LEAK") {
		t.Fatalf("错误体泄漏凭据明文: %s", raw)
	}
	var body2 struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body2)
	if body2.Error.Code != "credential_error" {
		t.Fatalf("code = %q, want credential_error", body2.Error.Code)
	}
}

func TestTestCloneRepoUnreachable422(t *testing.T) {
	srv, client, csrf, _ := setupProjectServer(t, stubProber{err: project.ErrRepoUnreachable})
	credID := newGitCred(t, client, srv.URL, csrf, "tok")
	body := `{"repoUrl":"https://bad.invalid/a/b.git","credentialId":"` + credID + `"}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects/test-clone", csrf, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	assertErrCode(t, resp, "repo_unreachable")
}

func TestTestCloneSuccess(t *testing.T) {
	srv, client, csrf, _ := setupProjectServer(t, stubProber{branch: "develop"})
	credID := newGitCred(t, client, srv.URL, csrf, "tok")
	body := `{"repoUrl":"https://gitee.com/a/b.git","credentialId":"` + credID + `"}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects/test-clone", csrf, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["ok"] != true || out["defaultBranch"] != "develop" {
		t.Fatalf("test-clone success body = %v", out)
	}
}

func TestProjectVaultUnconfigured422(t *testing.T) {
	// 项目服务持有未配置的 vault → 创建/test-clone 应 422 vault_unconfigured。
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, nil) // 未配置
	psvc := project.New(st.DB, v, stubProber{branch: "main"})
	srv := httptest.NewServer(New(testWebFSAuth(), svc, WithVault(v), WithProjects(psvc)))
	t.Cleanup(srv.Close)
	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)

	body := `{"repoUrl":"https://gitee.com/a/b.git","credentialId":"any"}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects/test-clone", csrf, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	assertErrCode(t, resp, "vault_unconfigured")
}

func TestProjectPatchAndDelete(t *testing.T) {
	srv, client, csrf, _ := setupProjectServer(t, stubProber{branch: "main"})
	credID := newGitCred(t, client, srv.URL, csrf, "tok")
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects", csrf,
		`{"name":"old","repoUrl":"https://g/a.git","credentialId":"`+credID+`"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var created map[string]any
	_ = json.Unmarshal(raw, &created)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("no id: %s", raw)
	}

	patchResp := doJSON(t, client, http.MethodPatch, srv.URL+"/api/projects/"+id, csrf, `{"name":"renamed"}`)
	defer patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", patchResp.StatusCode)
	}
	pr, _ := io.ReadAll(patchResp.Body)
	var patched map[string]any
	_ = json.Unmarshal(pr, &patched)
	if patched["name"] != "renamed" {
		t.Fatalf("name not updated: %s", pr)
	}

	delResp := doJSON(t, client, http.MethodDelete, srv.URL+"/api/projects/"+id, csrf, "")
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", delResp.StatusCode)
	}
	delAgain := doJSON(t, client, http.MethodDelete, srv.URL+"/api/projects/"+id, csrf, "")
	defer delAgain.Body.Close()
	if delAgain.StatusCode != http.StatusNotFound {
		t.Fatalf("delete-again status = %d, want 404", delAgain.StatusCode)
	}
	assertErrCode(t, delAgain, "project_not_found")
}

func TestProjectRequiresCSRF(t *testing.T) {
	srv, client, _, _ := setupProjectServer(t, stubProber{branch: "main"})
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects", "",
		`{"name":"x","repoUrl":"https://g/a.git","credentialId":"c"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF)", resp.StatusCode)
	}
}

func TestProjectRequiresAuth(t *testing.T) {
	srv, _, _, _ := setupProjectServer(t, stubProber{branch: "main"})
	resp, err := http.Get(srv.URL + "/api/projects")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// ─── 卡片「服务注册地址」(serviceRegs)────────────────────────────────────────

// stubServiceReg 是 servicereg.Service 的测试替身:只覆盖 ListServices(其余方法不参与
// 本组用例;嵌入 nil 接口,误调会 panic 以暴露越界依赖)。
type stubServiceReg struct {
	servicereg.Service
	items []servicereg.ServiceWithDomain
	err   error
}

func (s stubServiceReg) ListServices(context.Context) ([]servicereg.ServiceWithDomain, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.items, nil
}

// setupProjectRegServer 构造带 pipeline 真实现 + 服务注册替身的测试 server。
func setupProjectRegServer(t *testing.T, sr stubServiceReg) (*httptest.Server, *http.Client, string, pipeline.Service) {
	t.Helper()
	st := testStoreAuth(t)
	asvc := auth.NewService(st.DB, nil)
	if err := asvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	psvc := project.New(st.DB, v, stubProber{branch: "main"})
	pipe := pipeline.New(st.DB)
	srv := httptest.NewServer(New(testWebFSAuth(), asvc,
		WithVault(v), WithProjects(psvc), WithPipelines(pipe), WithServiceReg(sr)))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	return srv, client, csrf, pipe
}

// regBoundSpec 构造一份合法流水线 spec:部署阶段两个节点分别以 regServiceId
// 与 legacy gatewayService(服务名)绑定服务注册。
func regBoundSpec() pipeline.Spec {
	return pipeline.Spec{Stages: []pipeline.Stage{
		{Name: "源", Kind: pipeline.KindSource, Jobs: []pipeline.Job{
			{Name: "src", Type: "git_source", Config: map[string]any{}},
		}},
		{Name: "部署", Kind: pipeline.KindDeploy, Jobs: []pipeline.Job{
			{Name: "d1", Type: "deploy", Config: map[string]any{"regServiceId": "svc-1"}},
			{Name: "d2", Type: "deploy", Config: map[string]any{"gatewayService": "admin"}},
		}},
	}}
}

// createProjectViaAPI 经 API 建一个项目,返回 id。
func createProjectViaAPI(t *testing.T, client *http.Client, srvURL, csrf, credID, name string) string {
	t.Helper()
	resp := doJSON(t, client, http.MethodPost, srvURL+"/api/projects", csrf,
		`{"name":"`+name+`","repoUrl":"https://gitee.com/acme/`+name+`.git","credentialId":"`+credID+`"}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project %s: status %d, body %s", name, resp.StatusCode, raw)
	}
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	id, _ := p["id"].(string)
	if id == "" {
		t.Fatalf("create project %s: 响应无 id: %s", name, raw)
	}
	return id
}

// fetchProjectServiceRegs 拉取项目列表,返回 projectID → serviceRegs 原始条目
// (并断言 serviceRegs 恒为数组,而非 null/缺失)。
func fetchProjectServiceRegs(t *testing.T, client *http.Client, srvURL string) map[string][]map[string]any {
	t.Helper()
	resp := doJSON(t, client, http.MethodGet, srvURL+"/api/projects", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("list 解析失败: %v(body %s)", err, raw)
	}
	out := make(map[string][]map[string]any, len(list))
	for _, p := range list {
		id, _ := p["id"].(string)
		items, ok := p["serviceRegs"].([]any)
		if !ok {
			t.Fatalf("项目 %s 的 serviceRegs 应为数组(而非 null/缺失): %v", id, p["serviceRegs"])
		}
		regs := make([]map[string]any, 0, len(items))
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				t.Fatalf("serviceRegs 条目形状异常: %v", it)
			}
			regs = append(regs, m)
		}
		out[id] = regs
	}
	return out
}

// TestProjectListIncludesServiceRegs 验证项目列表按流水线部署节点绑定解析服务注册地址:
// regServiceId 精确匹配(FQDN / TCP 端口),legacy gatewayService 按服务名兜底;无绑定 → []。
func TestProjectListIncludesServiceRegs(t *testing.T) {
	sr := stubServiceReg{items: []servicereg.ServiceWithDomain{
		{RegisteredService: servicereg.RegisteredService{
			ID: "svc-1", Name: "shop", Protocol: servicereg.ProtocolHTTP,
		}, BaseDomain: "efg.com"},
		{RegisteredService: servicereg.RegisteredService{
			ID: "svc-2", Name: "admin", Protocol: servicereg.ProtocolTCP, TCPListenPort: 19000,
		}, BaseDomain: "efg.com"},
	}}
	srv, client, csrf, pipe := setupProjectRegServer(t, sr)
	credID := newGitCred(t, client, srv.URL, csrf, "tok")
	bound := createProjectViaAPI(t, client, srv.URL, csrf, credID, "shop-app")
	plain := createProjectViaAPI(t, client, srv.URL, csrf, credID, "plain-app")

	if _, err := pipe.Save(context.Background(), bound, regBoundSpec()); err != nil {
		t.Fatalf("save pipeline: %v", err)
	}

	regsByID := fetchProjectServiceRegs(t, client, srv.URL)
	regs := regsByID[bound]
	if len(regs) != 2 {
		t.Fatalf("绑定项目 serviceRegs = %v, want 2 条", regs)
	}
	if regs[0]["id"] != "svc-1" || regs[0]["fqdn"] != "shop.efg.com" || regs[0]["protocol"] != "http" {
		t.Fatalf("serviceRegs[0] = %v, want svc-1/shop.efg.com/http", regs[0])
	}
	if regs[1]["id"] != "svc-2" || regs[1]["protocol"] != "tcp" {
		t.Fatalf("serviceRegs[1] = %v, want svc-2/tcp(legacy 服务名兜底)", regs[1])
	}
	if got, _ := regs[1]["tcpListenPort"].(float64); got != 19000 {
		t.Fatalf("tcpListenPort = %v, want 19000", regs[1]["tcpListenPort"])
	}
	if len(regsByID[plain]) != 0 {
		t.Fatalf("无绑定项目 serviceRegs = %v, want []", regsByID[plain])
	}

	// 更新响应同样携带 serviceRegs(前端以更新响应替换卡片,缺省会丢展示)。
	patchResp := doJSON(t, client, http.MethodPatch, srv.URL+"/api/projects/"+bound, csrf, `{"name":"shop-app-2"}`)
	defer patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", patchResp.StatusCode)
	}
	var patched map[string]any
	_ = json.NewDecoder(patchResp.Body).Decode(&patched)
	if pregs, _ := patched["serviceRegs"].([]any); len(pregs) != 2 {
		t.Fatalf("更新响应 serviceRegs = %v, want 2 条", patched["serviceRegs"])
	}
}

// TestProjectListServiceRegsDegradesWhenRegistryUnavailable 验证服务注册读取失败时
// 列表仍 200 且 serviceRegs 输出 [](卡片附加展示不阻断项目列表)。
func TestProjectListServiceRegsDegradesWhenRegistryUnavailable(t *testing.T) {
	srv, client, csrf, pipe := setupProjectRegServer(t, stubServiceReg{err: errors.New("boom")})
	credID := newGitCred(t, client, srv.URL, csrf, "tok")
	id := createProjectViaAPI(t, client, srv.URL, csrf, credID, "shop-app")
	if _, err := pipe.Save(context.Background(), id, regBoundSpec()); err != nil {
		t.Fatalf("save pipeline: %v", err)
	}
	regsByID := fetchProjectServiceRegs(t, client, srv.URL)
	if len(regsByID[id]) != 0 {
		t.Fatalf("注册服务不可用时 serviceRegs 应输出 [], got %v", regsByID[id])
	}
}
