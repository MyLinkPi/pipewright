package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/trigger"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// validationTestEnv 持有校验测试用的 server + 客户端 + csrf + 项目 id。
type validationTestEnv struct {
	srv    *httptest.Server
	client *http.Client
	csrf   string
	projID string
}

// setupValidationServer 构造带全部五个协作服务的测试 server,返回一个绑定有效凭据的项目。
func setupValidationServer(t *testing.T) validationTestEnv {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	psvc := project.New(st.DB, v, stubProber{branch: "main"})
	plsvc := pipeline.New(st.DB)
	setsvc := pipeline.NewSettingsService(st.DB, v)
	trsvc := trigger.New(st.DB, v)

	srv := httptest.NewServer(New(testWebFSAuth(), svc,
		WithVault(v), WithProjects(psvc), WithPipelines(plsvc),
		WithPipelineSettings(setsvc), WithTriggers(trsvc)))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)

	credID := newGitCred(t, client, srv.URL, csrf, "ghp_token_for_validation_proj")
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects", csrf,
		`{"name":"shop","repoUrl":"https://gitee.com/acme/shop.git","credentialId":"`+credID+`"}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	projID, _ := p["id"].(string)
	if projID == "" {
		t.Fatalf("create project failed: %s", raw)
	}
	return validationTestEnv{srv: srv, client: client, csrf: csrf, projID: projID}
}

// getValidation 拉取校验结果并解析为 {ready, issues}。
func getValidation(t *testing.T, env validationTestEnv) (bool, []map[string]any) {
	t.Helper()
	resp := doJSON(t, env.client, http.MethodGet,
		env.srv.URL+"/api/projects/"+env.projID+"/pipeline/validation", env.csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("validation status = %d, want 200, body=%s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	var dto struct {
		Ready  bool             `json:"ready"`
		Issues []map[string]any `json:"issues"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatalf("decode validation: %v, body=%s", err, raw)
	}
	return dto.Ready, dto.Issues
}

func hasIssueCode(issues []map[string]any, code string) (map[string]any, bool) {
	for _, is := range issues {
		if is["code"] == code {
			return is, true
		}
	}
	return nil, false
}

func putSettings(t *testing.T, env validationTestEnv, body string) {
	t.Helper()
	resp := doJSON(t, env.client, http.MethodPut,
		env.srv.URL+"/api/projects/"+env.projID+"/pipeline/settings", env.csrf, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT settings status = %d, body=%s", resp.StatusCode, raw)
	}
}

// TestValidationRequiresAuth 未认证 → 401。
func TestValidationRequiresAuth(t *testing.T) {
	env := setupValidationServer(t)
	resp, err := http.Get(env.srv.URL + "/api/projects/" + env.projID + "/pipeline/validation")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无会话 GET status = %d, want 401", resp.StatusCode)
	}
}

// TestValidationProjectNotFound 不存在项目 → 404 project_not_found。
func TestValidationProjectNotFound(t *testing.T) {
	env := setupValidationServer(t)
	resp := doJSON(t, env.client, http.MethodGet,
		env.srv.URL+"/api/projects/00000000-0000-0000-0000-000000000000/pipeline/validation", env.csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404, body=%s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	var e map[string]map[string]string
	_ = json.Unmarshal(raw, &e)
	if e["error"]["code"] != "project_not_found" {
		t.Fatalf("code = %q, want project_not_found: %s", e["error"]["code"], raw)
	}
}

// TestValidationDefaultManualOnly 默认种子(空构建/部署、events 全 false、无映射):
// ready=true 且含 manual_only(info) + no_build_or_deploy(warning),无 error。
func TestValidationDefaultReadyManualOnly(t *testing.T) {
	env := setupValidationServer(t)
	ready, issues := getValidation(t, env)
	if !ready {
		t.Fatalf("默认配置应 ready, issues=%+v", issues)
	}
	if _, ok := hasIssueCode(issues, "manual_only"); !ok {
		t.Fatalf("应含 manual_only: %+v", issues)
	}
	// 默认无 build/deploy 任务 → 应有 no_build_or_deploy warning。
	if _, ok := hasIssueCode(issues, "no_build_or_deploy"); !ok {
		t.Fatalf("应含 no_build_or_deploy: %+v", issues)
	}
}

// TestValidationToolchainIncomplete model=toolchain 缺版本 → error toolchain_incomplete。
func TestValidationToolchainIncomplete(t *testing.T) {
	env := setupValidationServer(t)
	putSettings(t, env, `{"build":{"model":"toolchain","toolchain":{"language":"node","version":""},"artifactType":"jar"}}`)
	ready, issues := getValidation(t, env)
	is, ok := hasIssueCode(issues, "toolchain_incomplete")
	if !ok {
		t.Fatalf("应报 toolchain_incomplete: %+v", issues)
	}
	if is["severity"] != "error" {
		t.Fatalf("应为 error: %+v", is)
	}
	if ready {
		t.Fatalf("toolchain 不全不应 ready")
	}
}
