// mesh_admin_test.go 覆盖集群管理面聚合(overview)的关键正确性:节点表只含对端,
// 本节点自身行(历史发现循环误登记)与虚拟 IP 冲突行不外显。
package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/mesh/easytier"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/pushsource"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// fakeMeshRuntime 注入可控 EasyTier 状态(overview 聚合的数据源;其余视图零值)。
type fakeMeshRuntime struct{ status *easytier.Status }

func (f *fakeMeshRuntime) EasyTierStatus(context.Context) *easytier.Status { return f.status }
func (f *fakeMeshRuntime) SupervisorSnapshot() easytier.Runtime            { return easytier.Runtime{} }
func (f *fakeMeshRuntime) ExternalMode() bool                              { return false }
func (f *fakeMeshRuntime) Binaries() easytier.BinStatus                    { return easytier.BinStatus{} }
func (f *fakeMeshRuntime) TriggerDownload() error                          { return nil }
func (f *fakeMeshRuntime) CredentialProvider() easytier.CredentialIssuer   { return nil }

// TestMeshOverviewExcludesSelf 验证集群页节点表不含本节点自身:EasyTier 链路表恒不含
// 本机,自身行(node_id 即本机的误登记行,或虚拟 IP 与本机相同的冲突行)永不匹配
// 链路 → 恒显离线;overview 按 node_id 与虚拟 IP 双重识别并排除。
func TestMeshOverviewExcludesSelf(t *testing.T) {
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
	runtime := &fakeMeshRuntime{status: &easytier.Status{
		OK:     true,
		SelfIP: "10.1.1.1",
		Links:  []easytier.Link{{VirtualIP: "10.1.1.2", Hostname: "beta", LatencyMS: 5}},
	}}
	deps := NewMeshDeps(meshSvc, receiver, mesh.NewClient(meshSvc), psvc, rsvc, pool,
		nil, nil, nil, nil, revs, nil, 8080).WithRuntime(runtime)
	srv := httptest.NewServer(New(testWebFSAuth(), svc,
		WithVault(v), WithProjects(psvc), WithRuns(rsvc, pool), WithMesh(deps)))
	t.Cleanup(srv.Close)

	settings := enableMesh(t, meshSvc)

	// 三类登记行:本机自身(node_id 相同)、虚拟 IP 冲突(IP 与本机相同)、正常对端。
	if err := meshSvc.ApplyHello(context.Background(), "10.1.1.1", mesh.HelloInfo{
		NodeID: settings.NodeID, Name: "self-ghost", PublicKey: settings.PublicKey, APIPort: 8080,
	}); err != nil {
		t.Fatalf("apply self hello: %v", err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	if err := meshSvc.ApplyHello(context.Background(), "10.1.1.1", mesh.HelloInfo{
		NodeID: "peer-conflict", Name: "conflict", PublicKey: hex.EncodeToString(pub), APIPort: 8080,
	}); err != nil {
		t.Fatalf("apply conflict hello: %v", err)
	}
	registerPeer(t, meshSvc, "beta") // 10.1.1.2 / peer-beta,有链路

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/mesh/overview", csrf, "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("overview status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Peers []struct {
			NodeID   string  `json:"nodeId"`
			Online   bool    `json:"online"`
			LinkType string  `json:"linkType"`
			Latency  float64 `json:"latencyMs"`
		} `json:"peers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if len(out.Peers) != 1 || out.Peers[0].NodeID != "peer-beta" {
		t.Fatalf("peers = %+v, want 仅 beta(自身行与 IP 冲突行都应排除)", out.Peers)
	}
	if p := out.Peers[0]; !p.Online || p.LinkType != "p2p" || p.Latency != 5 {
		t.Fatalf("beta 行状态错误(应在线 p2p 5ms):%+v", p)
	}
}
