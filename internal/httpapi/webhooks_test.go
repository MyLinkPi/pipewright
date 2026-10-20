package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/trigger"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// setupWebhookServer 构造带 auth + project + trigger + run(worker pool)+ webhook 接收器的 server。
// 返回 server、已认证 client、csrf、项目 id、webhook token、webhook 明文密钥。
func setupWebhookServer(t *testing.T) (*httptest.Server, *http.Client, string, string, string, string) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	psvc := project.New(st.DB, v, stubProber{branch: "main"})
	tsvc := trigger.New(st.DB, v)
	rsvc := run.New(st.DB)
	pool := run.NewWorkerPool(rsvc)
	pool.Start()
	t.Cleanup(func() { pool.Stop(context.Background()) })

	receiver := trigger.NewReceiver(st.DB, v, NewRunCreator(rsvc))

	srv := httptest.NewServer(New(testWebFSAuth(), svc,
		WithVault(v), WithProjects(psvc), WithTriggers(tsvc),
		WithRuns(rsvc, pool), WithWebhooks(receiver)))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)

	credID := newGitCred(t, client, srv.URL, csrf, "ghp_token_for_webhook")
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects", csrf,
		`{"name":"acme","repoUrl":"https://gitee.com/acme/shop.git","credentialId":"`+credID+`"}`)
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	projID, _ := p["id"].(string)
	if projID == "" {
		t.Fatalf("create project failed: %s", raw)
	}

	// GET trigger → token;reset → 明文密钥;PUT → push 事件 + 分支映射 main→prod。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/projects/"+projID+"/trigger", csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var tg map[string]any
	_ = json.Unmarshal(raw, &tg)
	webhookURL, _ := tg["webhookUrl"].(string)
	token := webhookURL[len("/api/webhooks/"):]

	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/projects/"+projID+"/trigger/secret/reset", csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var rs map[string]any
	_ = json.Unmarshal(raw, &rs)
	secret, _ := rs["webhookSecret"].(string)
	if secret == "" {
		t.Fatalf("reset secret failed: %s", raw)
	}

	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/projects/"+projID+"/trigger", csrf,
		`{"events":{"push":true,"tag":false,"pullRequest":false},"branchFilters":["main"]}`)
	_ = resp.Body.Close()

	return srv, client, csrf, projID, token, secret
}

// TestManualTriggerCreatesAndAdvancesToSuccess 验证手动触发 → 201 → worker 推进到 success。
func TestManualTriggerCreatesAndAdvancesToSuccess(t *testing.T) {
	srv, client, csrf, projID, _, _ := setupWebhookServer(t)

	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects/"+projID+"/runs", csrf,
		`{"branch":"main"}`)
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var dto map[string]any
	_ = json.Unmarshal(raw, &dto)
	runID, _ := dto["id"].(string)
	if runID == "" {
		t.Fatalf("manual run id empty: %s", raw)
	}
	trig, _ := dto["trigger"].(map[string]any)
	if trig["type"] != "manual" || trig["actor"] != "admin" || trig["branch"] != "main" {
		t.Fatalf("trigger block unexpected: %v", trig)
	}
	// 冻结 DTO 不得含 resolved 内部字段。
	if _, leaked := dto["resolvedEnvironment"]; leaked {
		t.Fatalf("DTO leaked resolvedEnvironment")
	}

	final := waitRunStatus(t, client, srv, csrf, runID, run.StatusSuccess)
	if final["status"] != run.StatusSuccess {
		t.Fatalf("expected success, got %v", final["status"])
	}
}

// TestManualTriggerMissingBranch 验证缺分支 → 400。
// 分支可选:省略 branch 时由后端取项目默认分支并创建运行(201),不再 400。
func TestManualTriggerEmptyBranchUsesProjectDefault(t *testing.T) {
	srv, client, csrf, projID, _, _ := setupWebhookServer(t)
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/projects/"+projID+"/runs", csrf, `{}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("空 branch 应用项目默认分支创建成功(201),got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// postWebhook 投递 webhook(公开端点,无 cookie/CSRF)。
func postWebhook(t *testing.T, srvURL, token, event, tokenHdr string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/api/webhooks/"+token, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitee-Event", event)
	req.Header.Set("X-Gitee-Token", tokenHdr)
	// 故意用全新无 cookie 的 client,证明公开端点豁免会话/CSRF。
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("do webhook: %v", err)
	}
	return resp
}

// TestWebhookEndpointPublicNoAuthAccepts 验证公开 webhook 端点(无会话/CSRF)验签通过创建运行。
func TestWebhookEndpointPublicNoAuthAccepts(t *testing.T) {
	srv, client, csrf, _, token, secret := setupWebhookServer(t)

	body := []byte(`{"ref":"refs/heads/main","after":"abc123"}`)
	resp := postWebhook(t, srv.URL, token, "Push Hook", secret, body)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["accepted"] != true {
		t.Fatalf("expected accepted:true, got %s", raw)
	}
	runID, _ := out["runId"].(string)
	if runID == "" {
		t.Fatalf("webhook accepted but no runId: %s", raw)
	}
	// 响应体不得含密钥明文。
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("response leaked secret!")
	}
	// worker 推进到 success(用认证 client 查)。
	waitRunStatus(t, client, srv, csrf, runID, run.StatusSuccess)
}

// TestWebhookWrongSecret401 验证错误密钥 → 401,不创建。
func TestWebhookWrongSecret401(t *testing.T) {
	srv, _, _, _, token, _ := setupWebhookServer(t)
	resp := postWebhook(t, srv.URL, token, "Push Hook", "whsec_wrong", []byte(`{"ref":"refs/heads/main","after":"x"}`))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if bytes.Contains(raw, []byte("whsec_")) {
		// 错误体不应回显任何密钥前缀的明文(此处错误体本身不含)。
		t.Fatalf("401 body leaked secret-like content: %s", raw)
	}
}

// TestWebhookDuplicateDelivery 验证同 delivery id 重复 → ignored:duplicate,不重复创建。
func TestWebhookDuplicateDelivery(t *testing.T) {
	srv, _, _, _, token, secret := setupWebhookServer(t)
	body := []byte(`{"ref":"refs/heads/main","after":"dupcommit"}`)

	post := func() map[string]any {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/webhooks/"+token, bytes.NewReader(body))
		req.Header.Set("X-Gitee-Event", "Push Hook")
		req.Header.Set("X-Gitee-Token", secret)
		req.Header.Set("X-Gitee-Delivery", "delivery-dup-1")
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}

	first := post()
	if first["accepted"] != true {
		t.Fatalf("first should be accepted, got %v", first)
	}
	second := post()
	if second["accepted"] != false || second["ignored"] != "duplicate" {
		t.Fatalf("expected ignored:duplicate on replay, got %v", second)
	}
}

// TestWebhookBranchNoMatch 验证不匹配分支 → accepted:false。
func TestWebhookBranchNoMatch(t *testing.T) {
	srv, _, _, _, token, secret := setupWebhookServer(t)
	resp := postWebhook(t, srv.URL, token, "Push Hook", secret, []byte(`{"ref":"refs/heads/dev","after":"d1"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["accepted"] != false {
		t.Fatalf("expected accepted:false for unmatched branch, got %s", raw)
	}
	if out["ignored"] != "no_branch_match" {
		t.Fatalf("expected no_branch_match, got %v", out["ignored"])
	}
}

// ---- Gitea / Gogs 头兼容 ---------------------------------------------------

// postWebhookGiteaShape 按 Gitea/Gogs 形态投递:小写事件名 + 对 body 的
// hex(HMAC-SHA256) 签名头 + delivery 头(无 X-Gitee-* 头、无 timestamp)。
// headerPrefix 为 "Gitea" 或 "Gogs"(决定 X-<prefix>-* 三件套)。
func postWebhookGiteaShape(t *testing.T, srvURL, token, secret, headerPrefix, deliveryID string, body []byte) *http.Response {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, err := http.NewRequest(http.MethodPost, srvURL+"/api/webhooks/"+token, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-"+headerPrefix+"-Event", "push")
	req.Header.Set("X-"+headerPrefix+"-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-"+headerPrefix+"-Delivery", deliveryID)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("do webhook: %v", err)
	}
	return resp
}

// TestWebhookGiteaHeadersAccepted 验证 Gitea 形态(小写事件名 + body hex HMAC 签名)
// 经 HTTP 层头解析后创建运行,且运行触发 actor=gitea。
func TestWebhookGiteaHeadersAccepted(t *testing.T) {
	srv, client, csrf, _, token, secret := setupWebhookServer(t)

	body := []byte(`{"ref":"refs/heads/main","after":"gitea1"}`)
	resp := postWebhookGiteaShape(t, srv.URL, token, secret, "Gitea", "delivery-gitea-1", body)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["accepted"] != true {
		t.Fatalf("expected accepted:true, got %s", raw)
	}
	runID, _ := out["runId"].(string)
	if runID == "" {
		t.Fatalf("webhook accepted but no runId: %s", raw)
	}
	final := waitRunStatus(t, client, srv, csrf, runID, run.StatusSuccess)
	trig, _ := final["trigger"].(map[string]any)
	if trig["actor"] != "gitea" || trig["type"] != "webhook" {
		t.Fatalf("expected webhook run with actor gitea, got %v", trig)
	}
}

// TestWebhookGiteaBadSignature401 验证 Gitea 签名不匹配 → 401,不创建。
func TestWebhookGiteaBadSignature401(t *testing.T) {
	srv, _, _, _, token, _ := setupWebhookServer(t)
	body := []byte(`{"ref":"refs/heads/main","after":"x"}`)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/webhooks/"+token, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Event", "push")
	req.Header.Set("X-Gitea-Signature", strings.Repeat("0", 64))
	req.Header.Set("X-Gitea-Delivery", "delivery-gitea-bad")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("do webhook: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestWebhookGogsHeadersAccepted 验证仅 X-Gogs-* 头(Gogs 形态)也能创建运行。
func TestWebhookGogsHeadersAccepted(t *testing.T) {
	srv, _, _, _, token, secret := setupWebhookServer(t)

	body := []byte(`{"ref":"refs/heads/main","after":"gogs1"}`)
	resp := postWebhookGiteaShape(t, srv.URL, token, secret, "Gogs", "delivery-gogs-1", body)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["accepted"] != true {
		t.Fatalf("expected accepted:true, got %s", raw)
	}
}

// ---- GitHub 头兼容 ---------------------------------------------------------

// postWebhookGitHubShape 按 GitHub 形态投递:X-GitHub-Event + X-Hub-Signature-256
// (sha256=<hex HMAC-SHA256 over body>)+ X-GitHub-Delivery(无 X-Gitee-* 头、无 timestamp)。
func postWebhookGitHubShape(t *testing.T, srvURL, token, secret, deliveryID string, body []byte) *http.Response {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, err := http.NewRequest(http.MethodPost, srvURL+"/api/webhooks/"+token, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("do webhook: %v", err)
	}
	return resp
}

// TestWebhookGitHubHeadersAccepted 验证 GitHub 形态(sha256= 前缀 body 签名)经
// HTTP 层头解析后创建运行,且运行触发 actor=github。
func TestWebhookGitHubHeadersAccepted(t *testing.T) {
	srv, client, csrf, _, token, secret := setupWebhookServer(t)

	body := []byte(`{"ref":"refs/heads/main","after":"github1"}`)
	resp := postWebhookGitHubShape(t, srv.URL, token, secret, "delivery-github-1", body)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["accepted"] != true {
		t.Fatalf("expected accepted:true, got %s", raw)
	}
	runID, _ := out["runId"].(string)
	if runID == "" {
		t.Fatalf("webhook accepted but no runId: %s", raw)
	}
	final := waitRunStatus(t, client, srv, csrf, runID, run.StatusSuccess)
	trig, _ := final["trigger"].(map[string]any)
	if trig["actor"] != "github" || trig["type"] != "webhook" {
		t.Fatalf("expected webhook run with actor github, got %v", trig)
	}
}

// TestWebhookGitHubBadSignature401 验证 GitHub 签名不匹配 → 401,不创建。
func TestWebhookGitHubBadSignature401(t *testing.T) {
	srv, _, _, _, token, _ := setupWebhookServer(t)
	body := []byte(`{"ref":"refs/heads/main","after":"x"}`)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/webhooks/"+token, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	req.Header.Set("X-GitHub-Delivery", "delivery-github-bad")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestWebhookGitHubPingIgnored 验证 GitHub 建立 webhook 时的 ping 事件 → 200 accepted:false
// (event_not_subscribed),不创建运行。
func TestWebhookGitHubPingIgnored(t *testing.T) {
	srv, _, _, _, token, secret := setupWebhookServer(t)
	body := []byte(`{"zen":"Keep it logically awesome.","hook_id":1}`)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/webhooks/"+token, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "ping")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Delivery", "delivery-github-ping")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["accepted"] != false || out["ignored"] != "event_not_subscribed" {
		t.Fatalf("expected accepted:false / event_not_subscribed, got %s", raw)
	}
}
