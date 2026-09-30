package httpapi

// runner_test.go 覆盖项目「构建机选择器」端点(FR-8-19):
// PUT 收 selector(兼容旧 runnerServerId → server:<id>)、GET 双字段返回、
// 非法选择器/钉死不存在机器 → 422、项目不存在 → 404。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/runner"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// fakeExister 可控「服务器是否存在」判定(钉死形式校验用)。
type fakeExister struct{ ok bool }

func (f fakeExister) Exists(context.Context, string) bool { return f.ok }

// setupRunnerAPI 构造带 auth + project + runner 配置的测试 server。
func setupRunnerAPI(t *testing.T, exister runner.ServerExister) (*httptest.Server, *http.Client, string) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	psvc := project.New(st.DB, v, stubProber{branch: "main"})
	rsvc := runner.New(st.DB, exister)
	srv := httptest.NewServer(New(testWebFSAuth(), svc, WithVault(v), WithProjects(psvc), WithRunnerConfig(rsvc)))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	return srv, client, csrf
}

// newRunnerProject 经 API 建一个项目,返回 id。
func newRunnerProject(t *testing.T, client *http.Client, srvURL, csrf string) string {
	t.Helper()
	credID := newGitCred(t, client, srvURL, csrf, "ghp_runner_test_secret")
	resp := doJSON(t, client, http.MethodPost, srvURL+"/api/projects", csrf,
		`{"name":"pool-proj","repoUrl":"https://gitee.com/acme/pool.git","credentialId":"`+credID+`"}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	id, _ := p["id"].(string)
	if id == "" {
		t.Fatalf("create project failed: %s", raw)
	}
	return id
}

func TestRunnerSelectorRoundTrip(t *testing.T) {
	srv, client, csrf := setupRunnerAPI(t, fakeExister{ok: true})
	pid := newRunnerProject(t, client, srv.URL, csrf)

	// PUT 标签选择器 → 200;GET 回 selector,派生 runnerServerId 为空。
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/projects/"+pid+"/runner", csrf,
		`{"selector":"linux,arch=arm64"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 合法选择器 status = %d, want 200: %s", resp.StatusCode, raw)
	}
	gresp := doJSON(t, client, http.MethodGet, srv.URL+"/api/projects/"+pid+"/runner", csrf, "")
	graw, _ := io.ReadAll(gresp.Body)
	gresp.Body.Close()
	var dto map[string]any
	_ = json.Unmarshal(graw, &dto)
	if dto["selector"] != "linux,arch=arm64" {
		t.Fatalf("GET selector = %v, want linux,arch=arm64: %s", dto["selector"], graw)
	}
	if dto["runnerServerId"] != "" {
		t.Fatalf("标签选择器派生 runnerServerId 应为空, got %v", dto["runnerServerId"])
	}

	// 旧客户端只传 runnerServerId → 转 server:<id>;GET 双字段一致。
	resp2 := doJSON(t, client, http.MethodPut, srv.URL+"/api/projects/"+pid+"/runner", csrf,
		`{"runnerServerId":"srv-9"}`)
	raw2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("PUT 旧 runnerServerId status = %d, want 200: %s", resp2.StatusCode, raw2)
	}
	gresp2 := doJSON(t, client, http.MethodGet, srv.URL+"/api/projects/"+pid+"/runner", csrf, "")
	graw2, _ := io.ReadAll(gresp2.Body)
	gresp2.Body.Close()
	var dto2 map[string]any
	_ = json.Unmarshal(graw2, &dto2)
	if dto2["selector"] != "server:srv-9" || dto2["runnerServerId"] != "srv-9" {
		t.Fatalf("旧入参应转 server:<id> 并双字段返回: %s", graw2)
	}
}

func TestRunnerInvalidSelector422(t *testing.T) {
	srv, client, csrf := setupRunnerAPI(t, fakeExister{ok: true})
	pid := newRunnerProject(t, client, srv.URL, csrf)
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/projects/"+pid+"/runner", csrf,
		`{"selector":"arch="}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("非法选择器 status = %d, want 422: %s", resp.StatusCode, raw)
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Error.Code != "invalid_runner_selector" {
		t.Fatalf("错误码 = %q, want invalid_runner_selector: %s", e.Error.Code, raw)
	}
}

func TestRunnerPinnedServerNotFound422(t *testing.T) {
	srv, client, csrf := setupRunnerAPI(t, fakeExister{ok: false}) // 无任何已登记服务器
	pid := newRunnerProject(t, client, srv.URL, csrf)
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/projects/"+pid+"/runner", csrf,
		`{"selector":"server:ghost"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("钉死不存在机器 status = %d, want 422: %s", resp.StatusCode, raw)
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Error.Code != "runner_server_not_found" {
		t.Fatalf("错误码 = %q, want runner_server_not_found: %s", e.Error.Code, raw)
	}
}

func TestRunnerProjectNotFound404(t *testing.T) {
	srv, client, csrf := setupRunnerAPI(t, fakeExister{ok: true})
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/projects/no-such/runner", csrf,
		`{"selector":"linux"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("项目不存在 status = %d, want 404: %s", resp.StatusCode, raw)
	}
}
