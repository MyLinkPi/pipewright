// webhook_forward_test.go 覆盖 webhook 集群转发的关键正确性:
//  1. 端到端:入口节点公网收到本地不存在的 token → 经 mesh 签名转发给归属环境 →
//     归属环境验签/去重/创建运行,响应(含 401/404 语义)原样透传回调用方;
//  2. mesh 入站端点鉴权:未签名 401、ACL 默认拒绝 403(403 与 token 无关,转发侧忽略);
//  3. 降级路径:全对端无此 token → 本地 404;全对端不可达 → 502;归属缓存命中。
package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/trigger"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// setupWebhookForwardPair 构造「入口(entry) + 归属(owner)」两个独立节点的 server:
//   - owner:启用 mesh、登记 entry 公钥(TOFU)、按 grantTrigger 授予 trigger_allowed、
//     装配 webhookReceiver 与一个 push/main 触发配置的项目;
//   - entry:启用 mesh、把 owner 登记为对端(地址指向 ownerSrv;ownerAPIPort>0 时改指
//     该端口——测试失联用)、装配公网 webhook handler + 转发器。
//
// 返回 entry server、转发器、owner 的 webhook token/明文密钥、entry mesh 服务。
func setupWebhookForwardPair(t *testing.T, grantTrigger bool, ownerAPIPort int) (*httptest.Server, *WebhookForwarder, string, string, mesh.Service) {
	t.Helper()

	// ── entry 节点:先出身份(owner 侧 TOFU 登记需要它的公钥)。 ──
	entrySt := testStoreAuth(t)
	entryAuth := auth.NewService(entrySt.DB, nil)
	if err := entryAuth.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("entry bootstrap: %v", err)
	}
	ev := vault.New(entrySt.DB, testMasterKey())
	entryMesh := mesh.New(entrySt.DB, ev)
	entryStg := enableMesh(t, entryMesh)

	// ── owner 节点:完整装配(mesh 入站 + webhook 接收 + 项目/触发配置)。 ──
	ownerSrv, ownerMesh, token, secret := setupWebhookOwner(t, entryStg.NodeID, entryStg.PublicKey, grantTrigger)
	ownerStg, err := ownerMesh.Settings(context.Background())
	if err != nil {
		t.Fatalf("owner settings: %v", err)
	}

	// ── entry 侧:登记 owner 为对端 + 起 server(公网 handler 带转发器)。 ──
	ou, err := url.Parse(ownerSrv.URL)
	if err != nil {
		t.Fatalf("parse owner url: %v", err)
	}
	port := ownerAPIPort
	if port <= 0 {
		port, _ = strconv.Atoi(ou.Port())
	}
	if err := entryMesh.ApplyHello(context.Background(), ou.Hostname(), mesh.HelloInfo{
		NodeID: ownerStg.NodeID, Name: "owner-env", PublicKey: ownerStg.PublicKey, APIPort: port,
	}); err != nil {
		t.Fatalf("apply hello(owner): %v", err)
	}
	entryReceiver := trigger.NewReceiver(entrySt.DB, ev, NewRunCreator(run.New(entrySt.DB)))
	fwd := NewWebhookForwarder(entryMesh, mesh.NewClient(entryMesh))
	entryDeps := NewMeshDeps(entryMesh, nil, mesh.NewClient(entryMesh), nil, nil, nil,
		nil, nil, nil, nil, nil, entryReceiver, 0)
	entrySrv := httptest.NewServer(New(testWebFSAuth(), entryAuth,
		WithVault(ev), WithWebhooks(entryReceiver), WithWebhookForwarder(fwd), WithMesh(entryDeps)))
	t.Cleanup(entrySrv.Close)
	return entrySrv, fwd, token, secret, entryMesh
}

// setupWebhookOwner 构造归属节点 server:mesh 启用 + 登记 entry 公钥(按需授权触发)+
// webhookReceiver;并预置 push/main 触发配置的项目,返回 token 与明文密钥。
func setupWebhookOwner(t *testing.T, entryNodeID, entryPubKey string, grantTrigger bool) (*httptest.Server, mesh.Service, string, string) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("owner bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	psvc := project.New(st.DB, v, stubProber{branch: "main"})
	tsvc := trigger.New(st.DB, v)
	rsvc := run.New(st.DB)
	pool := run.NewWorkerPool(rsvc)
	pool.Start()
	t.Cleanup(func() { pool.Stop(context.Background()) })
	meshSvc := mesh.New(st.DB, v)
	receiver := trigger.NewReceiver(st.DB, v, NewRunCreator(rsvc))
	deps := NewMeshDeps(meshSvc, nil, mesh.NewClient(meshSvc), psvc, rsvc, pool,
		nil, nil, nil, nil, nil, receiver, 0)
	srv := httptest.NewServer(New(testWebFSAuth(), svc,
		WithVault(v), WithProjects(psvc), WithTriggers(tsvc), WithRuns(rsvc, pool),
		WithWebhooks(receiver), WithMesh(deps)))
	t.Cleanup(srv.Close)
	enableMesh(t, meshSvc)

	// 登记 entry 公钥(TOFU;owner 不回拨 entry,虚拟地址随意);按需授予 trigger_allowed。
	if err := meshSvc.ApplyHello(context.Background(), "10.9.9.9", mesh.HelloInfo{
		NodeID: entryNodeID, Name: "entry-env", PublicKey: entryPubKey, APIPort: 0,
	}); err != nil {
		t.Fatalf("apply hello(entry): %v", err)
	}
	if grantTrigger {
		if err := meshSvc.SaveACL(context.Background(), mesh.ACL{TriggerAllowed: []string{entryNodeID}}); err != nil {
			t.Fatalf("save acl: %v", err)
		}
	}

	// 预置项目 + 触发配置(push + main)。
	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	credID := newGitCred(t, client, srv.URL, csrf, "ghp_token_for_fwd")
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
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/projects/"+projID+"/trigger", csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var tg map[string]any
	_ = json.Unmarshal(raw, &tg)
	wu, _ := tg["webhookUrl"].(string)
	token := strings.TrimPrefix(wu, "/api/webhooks/")
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
	return srv, meshSvc, token, secret
}

// TestWebhookForwardToOwnerAccepts 端到端:入口收到本地不存在的 token → 转发 →
// 归属环境验签通过创建运行,响应透传(accepted:true + runId + 归属环境注记头)。
func TestWebhookForwardToOwnerAccepts(t *testing.T) {
	entrySrv, _, token, secret, _ := setupWebhookForwardPair(t, true, 0)

	resp := postWebhook(t, entrySrv.URL, token, "Push Hook", secret,
		[]byte(`{"ref":"refs/heads/main","after":"fwd-ok-1"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("X-PW-Webhook-Forwarded-For"); got != "owner-env" {
		t.Fatalf("forwarded-for header = %q, want owner-env", got)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["accepted"] != true || out["runId"] == "" || out["runId"] == nil {
		t.Fatalf("expected accepted:true with runId, got %v", out)
	}
}

// TestWebhookForwardRelaysUnauthorized 归属环境验签失败(错密钥)→ 401 原样透传。
func TestWebhookForwardRelaysUnauthorized(t *testing.T) {
	entrySrv, _, token, _, _ := setupWebhookForwardPair(t, true, 0)

	resp := postWebhook(t, entrySrv.URL, token, "Push Hook", "wrong-secret",
		[]byte(`{"ref":"refs/heads/main","after":"fwd-bad"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected relayed 401, got %d", resp.StatusCode)
	}
}

// TestWebhookForwardUnknownTokenStays404 全对端均无此 token → 维持本地 404。
func TestWebhookForwardUnknownTokenStays404(t *testing.T) {
	entrySrv, _, _, _, _ := setupWebhookForwardPair(t, true, 0)

	resp := postWebhook(t, entrySrv.URL, strings.Repeat("ab", 32), "Push Hook", "any",
		[]byte(`{"ref":"refs/heads/main"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestWebhookForwardACLDeniedFallsBack404 归属环境未授权(trigger_allowed 默认拒绝)
// → 403 与「非归属」不可区分,转发侧忽略 → 对外仍 404。
func TestWebhookForwardACLDeniedFallsBack404(t *testing.T) {
	entrySrv, _, token, secret, _ := setupWebhookForwardPair(t, false, 0)

	resp := postWebhook(t, entrySrv.URL, token, "Push Hook", secret,
		[]byte(`{"ref":"refs/heads/main","after":"fwd-deny"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 (ACL deny indistinguishable), got %d", resp.StatusCode)
	}
}

// TestWebhookForwardUnreachable502 有候选对端但全部不可达 → 502。
func TestWebhookForwardUnreachable502(t *testing.T) {
	// ownerAPIPort=1:本机几乎必无监听,拨号快速失败。
	entrySrv, _, token, secret, _ := setupWebhookForwardPair(t, true, 1)

	resp := postWebhook(t, entrySrv.URL, token, "Push Hook", secret,
		[]byte(`{"ref":"refs/heads/main","after":"fwd-dead"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 502, got %d: %s", resp.StatusCode, raw)
	}
}

// TestWebhookForwardCachesOwner 首次 accepted 后缓存 token→归属环境;再次投递仍可达。
func TestWebhookForwardCachesOwner(t *testing.T) {
	entrySrv, fwd, token, secret, _ := setupWebhookForwardPair(t, true, 0)

	resp := postWebhook(t, entrySrv.URL, token, "Push Hook", secret,
		[]byte(`{"ref":"refs/heads/main","after":"fwd-cache-1"}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first forward failed: %d", resp.StatusCode)
	}
	nodeID, ok := fwd.lookupOwner(token)
	if !ok || nodeID == "" {
		t.Fatalf("expected owner cache hit after accepted forward")
	}
	// 再次投递(不同 commit):缓存命中 → 直达 owner,仍返回 accepted。
	resp = postWebhook(t, entrySrv.URL, token, "Push Hook", secret,
		[]byte(`{"ref":"refs/heads/main","after":"fwd-cache-2"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("cached forward failed: %d %s", resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["accepted"] != true {
		t.Fatalf("expected accepted:true on cached path, got %v", out)
	}
}

// TestWebhookEndpointsListsPublicUrlPeers 验证可选环境地址列表:
// 对端未配置 publicUrl → 空;配置后 → 返回该项(base 去尾斜杠),且不含本节点。
func TestWebhookEndpointsListsPublicUrlPeers(t *testing.T) {
	entrySrv, _, _, _, entryMesh := setupWebhookForwardPair(t, true, 0)
	client := newTestClient(t)
	csrf := loginWithClient(t, client, entrySrv.URL)
	endpoint := entrySrv.URL + "/api/projects/any/trigger/webhook-endpoints"

	peers, err := entryMesh.ListPeers(context.Background())
	if err != nil || len(peers) != 1 {
		t.Fatalf("expect single registered peer, got %v err=%v", peers, err)
	}
	ownerNodeID := peers[0].NodeID

	// 未配置对外地址 → 空列表(虚拟 IP 对网外 Git 平台不可达,不列出)。
	resp := doJSON(t, client, http.MethodGet, endpoint, csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("endpoints status = %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Items []webhookEndpointDTO `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 0 {
		t.Fatalf("expected empty items before publicUrl set, got %v", out.Items)
	}

	// 补对外地址 → 列出该项。
	p, err := entryMesh.GetPeer(context.Background(), ownerNodeID)
	if err != nil {
		t.Fatalf("get peer: %v", err)
	}
	p.PublicURL = "https://owner.example.com/"
	if err := entryMesh.SaveManualPeer(context.Background(), *p); err != nil {
		t.Fatalf("save peer: %v", err)
	}
	resp = doJSON(t, client, http.MethodGet, endpoint, csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = json.Unmarshal(raw, &out)
	if len(out.Items) != 1 {
		t.Fatalf("expected 1 item after publicUrl set, got %s", raw)
	}
	got := out.Items[0]
	if got.NodeID != ownerNodeID || got.Name != "owner-env" || got.Base != "https://owner.example.com" {
		t.Fatalf("unexpected item: %+v", got)
	}
}

// signedWebhookPost 以对端身份签名发起一次 mesh webhook 转发请求(带原始投递头)。
func signedWebhookPost(t *testing.T, srvURL, token, event, tokenHdr string, body []byte, nodeID string, priv ed25519.PrivateKey) *http.Response {
	t.Helper()
	path := "/api/mesh/webhooks/" + token
	req, err := http.NewRequest(http.MethodPost, srvURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitee-Event", event)
	req.Header.Set("X-Gitee-Token", tokenHdr)
	req.Header.Set("X-Gitee-Delivery", "mesh-inbound-delivery-1")
	if err := mesh.SignHeaders(req.Header, http.MethodPost, path, body, nodeID, priv); err != nil {
		t.Fatalf("sign: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// TestMeshWebhookInboundAuthAndACL 直接打归属环境的 mesh 入站端点:
// 未签名 → 401;签名有效但 trigger_allowed 默认拒绝 → 403;授权后头透传 → 正常处理。
func TestMeshWebhookInboundAuthAndACL(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	nodeID := "peer-fwd"
	ownerSrv, ownerMesh, token, secret := setupWebhookOwner(t, nodeID, hex.EncodeToString(pub), false)

	body := []byte(`{"ref":"refs/heads/main","after":"mesh-in-1"}`)
	path := "/api/mesh/webhooks/" + token

	// 未签名 → 401。
	req, _ := http.NewRequest(http.MethodPost, ownerSrv.URL+path, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned: resp=%v err=%v", resp, err)
	}
	_ = resp.Body.Close()

	// 签名有效但 trigger_allowed 默认拒绝 → 403。
	resp = signedDo(t, http.MethodPost, ownerSrv.URL+path, path, body, nodeID, priv)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("acl deny status = %d, want 403", resp.StatusCode)
	}

	// 授权触发后:投递头随 mesh 请求透传 → owner 按同一套 Delivery 解析验签并创建运行。
	if err := ownerMesh.SaveACL(context.Background(), mesh.ACL{TriggerAllowed: []string{nodeID}}); err != nil {
		t.Fatalf("save acl: %v", err)
	}
	resp = signedWebhookPost(t, ownerSrv.URL, token, "Push Hook", secret, body, nodeID, priv)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("granted status = %d, want 200; body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["accepted"] != true {
		t.Fatalf("expected accepted:true, got %v", out)
	}
}
