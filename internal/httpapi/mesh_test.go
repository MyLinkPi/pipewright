// mesh_test.go 覆盖集群入站链路的关键正确性:
//  1. 路由共存:根级 /api/mesh/*(公开,签名鉴权)与受保护组内 /api/mesh/{settings,acl,...}
//     (admin session)同树挂载不冲突(webhook 先例的等价性验证);
//  2. meshAuth:未签名 401;签名有效但 ACL 默认全拒绝 → 403;授权后放行;
//  3. 推送接收:multipart 推送 → 首推自动创建 pushed 项目(origin 幂等)→ 修订落库,
//     blob 哈希不符拒绝;pushsource.Resolve 供物化按修订/条目取用。
package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/pushsource"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// setupMeshServer 构造带集群装配的 server:mesh 服务 + 接收器(真制品库,无镜像导入器)
// + 项目/运行/参数服务。返回 server、mesh 服务、修订存储、制品库。
func setupMeshServer(t *testing.T) (*httptest.Server, mesh.Service, pushsource.Store, *artifactstore.Store, project.Service) {
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
	deps := NewMeshDeps(meshSvc, receiver, mesh.NewClient(meshSvc), psvc, rsvc, pool,
		nil, nil, nil, nil, revs, 8080)

	srv := httptest.NewServer(New(testWebFSAuth(), svc,
		WithVault(v), WithProjects(psvc), WithRuns(rsvc, pool), WithMesh(deps)))
	t.Cleanup(srv.Close)
	return srv, meshSvc, revs, blobs, psvc
}

// enableMesh 打开集群开关并返回本节点设置。
func enableMesh(t *testing.T, svc mesh.Service) *mesh.Settings {
	t.Helper()
	enabled := true
	st, err := svc.SaveSettings(context.Background(), mesh.SaveSettingsInput{Enabled: &enabled})
	if err != nil {
		t.Fatalf("enable mesh: %v", err)
	}
	if !st.Enabled || st.NodeID == "" || st.PublicKey == "" {
		t.Fatalf("mesh settings not ready: %+v", st)
	}
	return st
}

// registerPeer 生成一个对端密钥对并 TOFU 登记,返回对端 node_id 与私钥(签名用)。
func registerPeer(t *testing.T, svc mesh.Service, name string) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	nodeID := "peer-" + name
	if err := svc.ApplyHello(context.Background(), "10.1.1.2", mesh.HelloInfo{
		NodeID: nodeID, Name: name, PublicKey: hex.EncodeToString(pub), APIPort: 8080,
	}); err != nil {
		t.Fatalf("apply hello: %v", err)
	}
	return nodeID, priv
}

// signedDo 以对端身份签名发起请求(path 为 URL 路径部分,签名串与之对齐)。
func signedDo(t *testing.T, method, url, path string, body []byte, nodeID string, priv ed25519.PrivateKey) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if err := mesh.SignHeaders(req.Header, method, path, body, nodeID, priv); err != nil {
		t.Fatalf("sign: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestMeshRoutingCoexistence(t *testing.T) {
	srv, meshSvc, _, _, _ := setupMeshServer(t)
	enableMesh(t, meshSvc)

	// 公开 hello(webhook 同级):无 session 可达。
	resp, err := http.Get(srv.URL + "/api/mesh/hello")
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hello status = %d, want 200", resp.StatusCode)
	}
	var hello mesh.HelloInfo
	if err := json.NewDecoder(resp.Body).Decode(&hello); err != nil || hello.NodeID == "" {
		t.Fatalf("hello body invalid: %v %+v", err, hello)
	}

	// 管理面在受保护组内:未登录 401(证明未被根级挂载吞掉/放行)。
	resp2, err := http.Get(srv.URL + "/api/mesh/settings")
	if err != nil {
		t.Fatalf("admin settings: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin settings unauthenticated status = %d, want 401", resp2.StatusCode)
	}
}

func TestMeshInboundAuthAndACL(t *testing.T) {
	srv, meshSvc, _, _, _ := setupMeshServer(t)
	enableMesh(t, meshSvc)
	nodeID, priv := registerPeer(t, meshSvc, "alpha")

	// 未签名 → 401。
	resp, err := http.Get(srv.URL + "/api/mesh/projects")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned: resp=%v err=%v", resp, err)
	}
	_ = resp.Body.Close()

	// 签名有效但默认全拒绝 → 403。
	resp = signedDo(t, http.MethodGet, srv.URL+"/api/mesh/projects", "/api/mesh/projects", nil, nodeID, priv)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("default deny status = %d, want 403", resp.StatusCode)
	}

	// 授权可见后 → 200。
	if err := meshSvc.SaveACL(context.Background(), mesh.ACL{VisibleTo: []string{nodeID}}); err != nil {
		t.Fatalf("save acl: %v", err)
	}
	resp = signedDo(t, http.MethodGet, srv.URL+"/api/mesh/projects", "/api/mesh/projects", nil, nodeID, priv)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("granted status = %d, want 200; body=%s", resp.StatusCode, body)
	}
}

func TestMeshPushCreatesPushedProject(t *testing.T) {
	srv, meshSvc, revs, blobs, psvc := setupMeshServer(t)
	enableMesh(t, meshSvc)
	nodeID, priv := registerPeer(t, meshSvc, "beta")
	if err := meshSvc.SaveACL(context.Background(), mesh.ACL{PushAllowed: []string{nodeID}}); err != nil {
		t.Fatalf("save acl: %v", err)
	}

	payload := []byte("hello-cluster-artifact")
	sum := sha256.Sum256(payload)
	blobSHA := hex.EncodeToString(sum[:])

	manifest := mesh.PushManifest{
		OriginNodeID:      nodeID,
		OriginProjectRef:  "proj-42",
		OriginProjectName: "web-app",
		OriginRunID:       "run-7",
		RevisionLabel:     "main-abc1234",
		Items: []mesh.PushItem{{
			Name: "web-app-app.jar", Kind: "file", SHA256: blobSHA,
			Size: int64(len(payload)), DestName: "target/app.jar",
		}},
	}
	push := func(m mesh.PushManifest, parts map[string][]byte) (*http.Response, []byte) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mj, _ := mw.CreateFormField("manifest")
		mb, _ := json.Marshal(m)
		_, _ = mj.Write(mb)
		for sha, data := range parts {
			fw, _ := mw.CreateFormFile("blob", sha)
			_, _ = fw.Write(data)
		}
		_ = mw.Close()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/mesh/push", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		if err := mesh.SignHeaders(req.Header, http.MethodPost, "/api/mesh/push", nil, nodeID, priv); err != nil {
			t.Fatalf("sign: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, body
	}

	// 首推:自动创建项目 + 修订。
	resp, body := push(manifest, map[string][]byte{blobSHA: payload})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push status = %d body=%s", resp.StatusCode, body)
	}
	var result mesh.PushResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("push result: %v", err)
	}
	if !result.Created || result.ProjectID == "" || result.RevisionID == "" {
		t.Fatalf("push result incomplete: %+v", result)
	}

	// 二推(同 origin):幂等复用项目。
	manifest.RevisionLabel = "main-def5678"
	resp, body = push(manifest, map[string][]byte{blobSHA: payload}) // 同 blob,制品库去重
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second push status = %d body=%s", resp.StatusCode, body)
	}
	var result2 mesh.PushResult
	_ = json.Unmarshal(body, &result2)
	if result2.Created || result2.ProjectID != result.ProjectID {
		t.Fatalf("second push should reuse project: %+v vs %+v", result2, result)
	}

	// 哈希不符拒绝(整次失败)。
	bad := manifest
	bad.Items[0].SHA256 = strings.Repeat("0", 64)
	resp, body = push(bad, map[string][]byte{strings.Repeat("0", 64): []byte("tampered")})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("tampered push status = %d body=%s", resp.StatusCode, body)
	}

	// 物化侧:按标签 + 条目选择解析(部署时选择)。
	ctx := context.Background()
	rev, items, err := pushsource.Resolve(ctx, revs, result.ProjectID, "main-abc1234", []string{"web-app-app.jar"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(items) != 1 || items[0].SHA256 != blobSHA || items[0].DestName != "target/app.jar" {
		t.Fatalf("resolved items mismatch: %+v", items)
	}
	if rev.Label != "main-abc1234" {
		t.Fatalf("resolved label = %q", rev.Label)
	}
	// 真字节经制品库可取回(部署链路消费)。
	rc, err := blobs.Open(items[0].SHA256)
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("blob bytes mismatch: %q", got)
	}

	// 推送源项目幂等键:GetByOrigin 命中;源类型为 pushed。
	origin, err := psvc.GetByOrigin(ctx, nodeID, "proj-42")
	if err != nil {
		t.Fatalf("get by origin: %v", err)
	}
	if origin.SourceType != project.SourceTypePushed || origin.Name != "web-app" {
		t.Fatalf("pushed project mismatch: %+v", origin)
	}
}
