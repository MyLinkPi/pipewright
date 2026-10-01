package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/deploy"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// setupDeployServer 构造带 auth + project + run + servers(target)+ deploy 的测试 server。
// dialer 注入可控 SSH 结果(不触网)。返回项目 id、run service、target service。
func setupDeployServer(t *testing.T, dialer target.SSHDialer) (
	*httptest.Server, *http.Client, string, string, run.Service, target.Service,
) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	psvc := project.New(st.DB, v, stubProber{branch: "main"})
	rsvc := run.New(st.DB)
	tsvc := target.New(st.DB, v, dialer)
	dsvc := deploy.New(tsvc, rsvc)
	pool := run.NewWorkerPool(rsvc)
	pool.Start()
	t.Cleanup(func() { pool.Stop(context.Background()) })

	srv := httptest.NewServer(New(testWebFSAuth(), svc,
		WithVault(v), WithProjects(psvc), WithRuns(rsvc, pool),
		WithServers(tsvc), WithDeploy(dsvc)))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)

	credID := newGitCred(t, client, srv.URL, csrf, "ghp_token_for_run")
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects", csrf,
		`{"name":"acme","repoUrl":"https://gitee.com/acme/shop.git","credentialId":"`+credID+`"}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	projID, _ := p["id"].(string)
	if projID == "" {
		t.Fatalf("create project failed: %s", raw)
	}
	return srv, client, csrf, projID, rsvc, tsvc
}

// createServer 经 API 建一台目标服务器(绑定一条 ssh_key 凭据),返回 server id。
func createServer(t *testing.T, client *http.Client, srvURL, csrf, name string) string {
	t.Helper()
	credID := newSSHCredAPI(t, client, srvURL, csrf, "PRIVKEY_LEAKMARKER")
	body := `{"name":"` + name + `","host":"127.0.0.1","port":22,"user":"deploy","credentialId":"` + credID + `"}`
	resp := doJSON(t, client, http.MethodPost, srvURL+"/api/servers", csrf, body)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var s map[string]any
	_ = json.Unmarshal(raw, &s)
	id, _ := s["id"].(string)
	if id == "" {
		t.Fatalf("create server failed: %s", raw)
	}
	return id
}

// firstArtifactID 经 GET /api/runs/{id}/artifacts 取首个产物 id(成功 run 由桩 runner emit)。
func firstArtifactID(t *testing.T, client *http.Client, srvURL, csrf, runID string) string {
	t.Helper()
	resp := doJSON(t, client, http.MethodGet, srvURL+"/api/runs/"+runID+"/artifacts", csrf, "")
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Artifacts []struct {
			ID string `json:"id"`
		} `json:"artifacts"`
	}
	_ = json.Unmarshal(raw, &body)
	if len(body.Artifacts) == 0 {
		t.Fatalf("成功 run 无产物: %s", raw)
	}
	return body.Artifacts[0].ID
}

// TestDeployEndpointFillsTargetsSlot 验证 POST deploy → 200 + targets 数组,且 run-detail
// targets slot 被填(冻结子 DTO 形状),输出无凭据明文。
func TestDeployEndpointFillsTargetsSlot(t *testing.T) {
	srv, client, csrf, projID, rsvc, _ := setupDeployServer(t,
		stubDialer{res: &target.ExecResult{ExitCode: 0, Stdout: "ok"}})

	r, err := rsvc.Create(context.Background(), projID,
		run.Trigger{Type: run.TriggerManual, Branch: "main", Commit: "abc", Actor: "admin"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitRunStatus(t, client, srv, csrf, r.ID, run.StatusSuccess)

	artID := firstArtifactID(t, client, srv.URL, csrf, r.ID)
	serverID := createServer(t, client, srv.URL, csrf, "web-prod-1")

	body := `{"artifactId":"` + artID + `","serverIds":["` + serverID + `"]}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+r.ID+"/deploy", csrf, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if string(raw) != "" && strings.Contains(string(raw), "LEAKMARKER") {
		t.Fatalf("deploy 响应泄漏凭据明文: %s", raw)
	}
	var dr struct {
		Targets []map[string]any `json:"targets"`
	}
	_ = json.Unmarshal(raw, &dr)
	if len(dr.Targets) != 1 {
		t.Fatalf("want 1 target, got %d: %s", len(dr.Targets), raw)
	}
	tg := dr.Targets[0]
	for _, k := range []string{"serverId", "serverName", "status", "message", "startedAt"} {
		if _, ok := tg[k]; !ok {
			t.Fatalf("target DTO 缺字段 %q: %v", k, tg)
		}
	}
	if tg["status"] != "success" {
		t.Fatalf("target status = %v, want success: %v", tg["status"], tg)
	}
	if tg["serverName"] != "web-prod-1" {
		t.Fatalf("serverName = %v, want web-prod-1", tg["serverName"])
	}

	// run-detail targets slot 被填(非 null,数组)。
	dto := waitRunStatus(t, client, srv, csrf, r.ID, run.StatusSuccess)
	rawTargets, ok := dto["targets"]
	if !ok || rawTargets == nil {
		t.Fatalf("run-detail targets slot 应被填: %v", dto["targets"])
	}
	arr, ok := rawTargets.([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("targets 应为含 1 元素的数组: %v", rawTargets)
	}
}

// TestDeployFailureNot500 验证 SSH 执行失败 → 整体仍 200,该机 status=failed 人读,绝不 500/泄漏。
func TestDeployFailureNot500(t *testing.T) {
	srv, client, csrf, projID, rsvc, _ := setupDeployServer(t,
		stubDialer{err: target.ErrUnreachable})

	r, _ := rsvc.Create(context.Background(), projID,
		run.Trigger{Type: run.TriggerManual, Branch: "main", Commit: "abc", Actor: "admin"})
	waitRunStatus(t, client, srv, csrf, r.ID, run.StatusSuccess)
	artID := firstArtifactID(t, client, srv.URL, csrf, r.ID)
	serverID := createServer(t, client, srv.URL, csrf, "dead-1")

	body := `{"artifactId":"` + artID + `","serverIds":["` + serverID + `"]}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+r.ID+"/deploy", csrf, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy 执行失败应仍 200, got %d: %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "LEAKMARKER") {
		t.Fatalf("deploy 失败响应泄漏凭据明文: %s", raw)
	}
	var dr struct {
		Targets []map[string]any `json:"targets"`
	}
	_ = json.Unmarshal(raw, &dr)
	if len(dr.Targets) != 1 || dr.Targets[0]["status"] != "failed" {
		t.Fatalf("want 1 failed target: %s", raw)
	}
	if msg, _ := dr.Targets[0]["message"].(string); msg == "" {
		t.Fatalf("failed 机应有人读 message: %s", raw)
	}
}

// TestDeployNonSuccessRun422 验证非成功 run 部署 → 422(人读),不 500。
func TestDeployNonSuccessRun422(t *testing.T) {
	srv, client, csrf, projID, rsvc, _ := setupDeployServer(t,
		stubDialer{res: &target.ExecResult{ExitCode: 0}})

	r, _ := rsvc.Create(context.Background(), projID,
		run.Trigger{Type: run.TriggerManual, Branch: "main", Commit: "abc", Actor: "admin"})
	// 立刻取消 → failed 终态(非成功)。
	cresp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+r.ID+"/cancel", csrf, "")
	cresp.Body.Close()
	waitRunStatus(t, client, srv, csrf, r.ID, run.StatusFailed)
	serverID := createServer(t, client, srv.URL, csrf, "s1")

	body := `{"artifactId":"x","serverIds":["` + serverID + `"]}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+r.ID+"/deploy", csrf, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("非成功 run 部署 status = %d, want 422", resp.StatusCode)
	}
}

// TestDeployRunNotFound404 验证 run 不存在 → 404。
func TestDeployRunNotFound404(t *testing.T) {
	srv, client, csrf, _, _, _ := setupDeployServer(t,
		stubDialer{res: &target.ExecResult{ExitCode: 0}})
	serverID := createServer(t, client, srv.URL, csrf, "s1")
	body := `{"artifactId":"x","serverIds":["` + serverID + `"]}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/nonexistent/deploy", csrf, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("run 不存在 status = %d, want 404", resp.StatusCode)
	}
}

// TestDeployServerNotFound422 验证服务器不存在 → 422。
func TestDeployServerNotFound422(t *testing.T) {
	srv, client, csrf, projID, rsvc, _ := setupDeployServer(t,
		stubDialer{res: &target.ExecResult{ExitCode: 0}})
	r, _ := rsvc.Create(context.Background(), projID,
		run.Trigger{Type: run.TriggerManual, Branch: "main", Commit: "abc", Actor: "admin"})
	waitRunStatus(t, client, srv, csrf, r.ID, run.StatusSuccess)
	artID := firstArtifactID(t, client, srv.URL, csrf, r.ID)

	body := `{"artifactId":"` + artID + `","serverIds":["nonexistent-server"]}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+r.ID+"/deploy", csrf, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("服务器不存在 status = %d, want 422", resp.StatusCode)
	}
}

// TestRetryDeployRunNotFound404 验证 retry:run 不存在 → 404。
func TestRetryDeployRunNotFound404(t *testing.T) {
	srv, client, csrf, _, _, _ := setupDeployServer(t,
		stubDialer{res: &target.ExecResult{ExitCode: 0}})
	body := `{"artifactId":"x"}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/nonexistent/deploy/retry", csrf, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("retry run 不存在 status = %d, want 404", resp.StatusCode)
	}
}

// TestRetryDeployNoFailedTargets422 验证 retry:全成功 run(无失败目标)→ 422 人读,不 500。
func TestRetryDeployNoFailedTargets422(t *testing.T) {
	srv, client, csrf, projID, rsvc, _ := setupDeployServer(t,
		stubDialer{res: &target.ExecResult{ExitCode: 0}})
	r, _ := rsvc.Create(context.Background(), projID,
		run.Trigger{Type: run.TriggerManual, Branch: "main", Commit: "abc", Actor: "admin"})
	waitRunStatus(t, client, srv, csrf, r.ID, run.StatusSuccess)
	artID := firstArtifactID(t, client, srv.URL, csrf, r.ID)
	serverID := createServer(t, client, srv.URL, csrf, "ok-1")

	// 先成功部署一台。
	body := `{"artifactId":"` + artID + `","serverIds":["` + serverID + `"]}`
	dresp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+r.ID+"/deploy", csrf, body)
	dresp.Body.Close()

	// 无失败目标 → retry 应 422。
	rresp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+r.ID+"/deploy/retry", csrf,
		`{"artifactId":"`+artID+`"}`)
	rraw, _ := io.ReadAll(rresp.Body)
	rresp.Body.Close()
	if rresp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("retry 无失败目标 status = %d, want 422: %s", rresp.StatusCode, rraw)
	}
	if strings.Contains(string(rraw), "LEAKMARKER") {
		t.Fatalf("retry 响应泄漏凭据明文: %s", rraw)
	}
}

// TestRetryDeployRequiresCSRF 验证 retry 写方法缺 CSRF → 403。
func TestRetryDeployRequiresCSRF(t *testing.T) {
	srv, client, _, _, _, _ := setupDeployServer(t,
		stubDialer{res: &target.ExecResult{ExitCode: 0}})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/runs/whatever/deploy/retry", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("retry 缺 CSRF status = %d, want 403", resp.StatusCode)
	}
}

// TestDeployRequiresCSRF 验证写方法缺 CSRF → 403。
func TestDeployRequiresCSRF(t *testing.T) {
	srv, client, csrf, projID, rsvc, _ := setupDeployServer(t,
		stubDialer{res: &target.ExecResult{ExitCode: 0}})
	r, _ := rsvc.Create(context.Background(), projID,
		run.Trigger{Type: run.TriggerManual, Branch: "main", Commit: "abc", Actor: "admin"})
	waitRunStatus(t, client, srv, csrf, r.ID, run.StatusSuccess)

	// 不带 CSRF header。
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/runs/"+r.ID+"/deploy", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF status = %d, want 403", resp.StatusCode)
	}
}

// createLabeledServer 经 API 建一台带构建机池标签的服务器(部署目标圈选共用 servers.labels)。
func createLabeledServer(t *testing.T, client *http.Client, srvURL, csrf, name, labels string) string {
	t.Helper()
	credID := newSSHCredAPI(t, client, srvURL, csrf, "PRIVKEY_LEAKMARKER")
	body := `{"name":"` + name + `","host":"127.0.0.1","port":22,"user":"deploy","credentialId":"` + credID + `","labels":"` + labels + `"}`
	resp := doJSON(t, client, http.MethodPost, srvURL+"/api/servers", csrf, body)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var s map[string]any
	_ = json.Unmarshal(raw, &s)
	id, _ := s["id"].(string)
	if id == "" {
		t.Fatalf("create labeled server failed: %s", raw)
	}
	return id
}

// TestDeploySelectorResolvesTargets 验证目标选择器圈选:只部署打了匹配标签的机器(AND 语义),
// 未打标签 / 标签不匹配的机器不进 targets。
func TestDeploySelectorResolvesTargets(t *testing.T) {
	srv, client, csrf, projID, rsvc, _ := setupDeployServer(t,
		stubDialer{res: &target.ExecResult{ExitCode: 0, Stdout: "ok"}})
	r, err := rsvc.Create(context.Background(), projID,
		run.Trigger{Type: run.TriggerManual, Branch: "main", Commit: "abc", Actor: "admin"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitRunStatus(t, client, srv, csrf, r.ID, run.StatusSuccess)
	artID := firstArtifactID(t, client, srv.URL, csrf, r.ID)

	createLabeledServer(t, client, srv.URL, csrf, "web-prod-1", "web,env=prod")
	createLabeledServer(t, client, srv.URL, csrf, "web-prod-2", "web,env=prod")
	createLabeledServer(t, client, srv.URL, csrf, "db-prod-1", "db,env=prod")

	body := `{"artifactId":"` + artID + `","selector":"web,env=prod"}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+r.ID+"/deploy", csrf, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var dr struct {
		Targets []struct {
			ServerName string `json:"serverName"`
			Status     string `json:"status"`
		} `json:"targets"`
	}
	_ = json.Unmarshal(raw, &dr)
	if len(dr.Targets) != 2 {
		t.Fatalf("selector web,env=prod 应圈选 2 台, got %d: %s", len(dr.Targets), raw)
	}
	for _, tg := range dr.Targets {
		if tg.ServerName == "db-prod-1" || tg.Status != "success" {
			t.Fatalf("db 机器不应被圈选 / 目标应 success: %+v", dr.Targets)
		}
	}
}

// TestDeployNoTargetsSkipsSuccess 验证空目标(选择器空 / 零命中)→ 200 空 targets(跳过即成功,不再 422)。
func TestDeployNoTargetsSkipsSuccess(t *testing.T) {
	srv, client, csrf, projID, rsvc, _ := setupDeployServer(t,
		stubDialer{res: &target.ExecResult{ExitCode: 0}})
	r, err := rsvc.Create(context.Background(), projID,
		run.Trigger{Type: run.TriggerManual, Branch: "main", Commit: "abc", Actor: "admin"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitRunStatus(t, client, srv, csrf, r.ID, run.StatusSuccess)
	artID := firstArtifactID(t, client, srv.URL, csrf, r.ID)
	createLabeledServer(t, client, srv.URL, csrf, "db-1", "db")

	for name, reqBody := range map[string]string{
		"空选择器":  `{"artifactId":"` + artID + `"}`,
		"零命中":   `{"artifactId":"` + artID + `","selector":"gpu"}`,
		"钉单机已删": `{"artifactId":"` + artID + `","selector":"server:gone"}`,
	} {
		resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+r.ID+"/deploy", csrf, reqBody)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200: %s", name, resp.StatusCode, raw)
		}
		var dr struct {
			Targets []map[string]any `json:"targets"`
		}
		_ = json.Unmarshal(raw, &dr)
		if dr.Targets == nil || len(dr.Targets) != 0 {
			t.Fatalf("%s: want empty targets array, got %s", name, raw)
		}
	}
	// 跳过的部署不落 deploy_targets:run-detail targets slot 仍为 null。
	dto := waitRunStatus(t, client, srv, csrf, r.ID, run.StatusSuccess)
	if v, ok := dto["targets"]; ok && v != nil {
		t.Fatalf("跳过部署不应写 targets: %v", v)
	}
}

// TestDeployInvalidSelector422 验证选择器语法非法 → 422 invalid_deploy_selector。
func TestDeployInvalidSelector422(t *testing.T) {
	srv, client, csrf, projID, rsvc, _ := setupDeployServer(t,
		stubDialer{res: &target.ExecResult{ExitCode: 0}})
	r, err := rsvc.Create(context.Background(), projID,
		run.Trigger{Type: run.TriggerManual, Branch: "main", Commit: "abc", Actor: "admin"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitRunStatus(t, client, srv, csrf, r.ID, run.StatusSuccess)
	artID := firstArtifactID(t, client, srv.URL, csrf, r.ID)

	body := `{"artifactId":"` + artID + `","selector":"a b!!"}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/runs/"+r.ID+"/deploy", csrf, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "invalid_deploy_selector") {
		t.Fatalf("want invalid_deploy_selector code: %s", raw)
	}
}
