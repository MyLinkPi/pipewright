package httpapi

// pveadmin_test.go — PVE 管理 HTTP 面契约测试:路由/错误映射/审计计数(接口桩),
// 服务器登记探测钩子(桩返回 DiscoveryResult → 响应携带 pveDiscovery),
// 应用商店部署 PVE 物理机 422 拦截,server-pools 只读端点。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/pveadmin"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// stubPVEAdmin 是 pveadmin.Service 接口桩(仅本文件用;触网方法按用例注入)。
type stubPVEAdmin struct {
	list     []pveadmin.ClusterView
	get      *pveadmin.ClusterView
	disco    *pveadmin.DiscoveryResult
	vmRef    *pveadmin.TaskRef
	vmErr    error
	tokenErr error
	testErr  error
	calls    []string
}

func (s *stubPVEAdmin) OnServerRegistered(_ context.Context, _ string) (*pveadmin.DiscoveryResult, error) {
	s.calls = append(s.calls, "discover")
	return s.disco, nil
}
func (s *stubPVEAdmin) ListClusters(context.Context) ([]pveadmin.ClusterView, error) {
	return s.list, nil
}
func (s *stubPVEAdmin) GetCluster(_ context.Context, _ string) (*pveadmin.ClusterView, error) {
	return s.get, nil
}
func (s *stubPVEAdmin) SetToken(context.Context, string, string, string, string, string, string) error {
	s.calls = append(s.calls, "token")
	return s.tokenErr
}
func (s *stubPVEAdmin) TestCluster(context.Context, string) error { return s.testErr }
func (s *stubPVEAdmin) SetReadonly(context.Context, string, bool) error {
	s.calls = append(s.calls, "readonly")
	return nil
}
func (s *stubPVEAdmin) DeleteCluster(context.Context, string) error {
	s.calls = append(s.calls, "delete")
	return nil
}
func (s *stubPVEAdmin) Overview(context.Context, string) (*pveadmin.Overview, error) {
	return &pveadmin.Overview{Version: "8.2.4", Quorate: true}, nil
}
func (s *stubPVEAdmin) ListVMs(context.Context, string) ([]pveadmin.VM, error) {
	return []pveadmin.VM{{VMID: 101, Name: "web-1", Node: "pve1", Type: "qemu", Status: "running"}}, nil
}
func (s *stubPVEAdmin) VMAction(_ context.Context, _, _, _ string, _ int64, _ string) (*pveadmin.TaskRef, error) {
	s.calls = append(s.calls, "vmaction")
	return s.vmRef, s.vmErr
}
func (s *stubPVEAdmin) TaskStatus(context.Context, string, string, string) (*pveadmin.TaskStatusView, error) {
	return &pveadmin.TaskStatusView{Status: "stopped", ExitStatus: "OK", Done: true, OK: true}, nil
}
func (s *stubPVEAdmin) ListTasks(context.Context, string, int) ([]pveadmin.ClusterTask, error) {
	return nil, nil
}

func setupPVEAdminAPI(t *testing.T, stub *stubPVEAdmin) (srvURL string, client *http.Client, csrf string, rec audit.Recorder) {
	t.Helper()
	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	rec = audit.New(st.DB, mask.NewMasker(), nil)
	srv := httptest.NewServer(New(testWebFSAuth(), authSvc,
		WithVault(v), WithPVEAdmin(stub), WithServers(target.New(st.DB, v, nil)), WithAudit(rec)))
	t.Cleanup(func() { srv.Close() })
	client = newTestClient(t)
	csrf = loginWithClient(t, client, srv.URL)
	return srv.URL, client, csrf, rec
}

// TestPVEClusterEndpoints 集群列表/详情/Token/只读开关/删除:200 形状 + 审计计数。
func TestPVEClusterEndpoints(t *testing.T) {
	stub := &stubPVEAdmin{
		list: []pveadmin.ClusterView{{ID: "cl-1", Name: "pve-prod", State: "ready", PoolName: "pve-pve-prod", NodeCount: 3}},
		get:  &pveadmin.ClusterView{ID: "cl-1", Name: "pve-prod", HasToken: true, State: "ready"},
	}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)

	resp := doJSON(t, client, http.MethodGet, base+"/api/pve/clusters", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodGet, base+"/api/pve/clusters/cl-1", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPut, base+"/api/pve/clusters/cl-1/token", csrf,
		`{"apiUrl":"https://10.0.0.1:8006","tokenName":"root@pam!pw","tokenSecret":"sec","tlsMode":"insecure"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPut, base+"/api/pve/clusters/cl-1/readonly", csrf, `{"readonly":false}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readonly = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodDelete, base+"/api/pve/clusters/cl-1", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	for _, action := range []string{auditActionPVEClusterToken, auditActionPVEClusterReadonly, auditActionPVEClusterDelete} {
		entries, err := rec.List(context.Background(), audit.ListFilter{Action: action})
		if err != nil || len(entries.Entries) != 1 {
			t.Fatalf("audit %s = %d entries / %v, want 1", action, len(entries.Entries), err)
		}
	}
}

// TestPVEVMActionReadonlyMaps409 电源操作被只读拒绝 → 409 且留审计(拒绝也留痕)。
func TestPVEVMActionReadonlyMaps409(t *testing.T) {
	stub := &stubPVEAdmin{vmErr: fmt.Errorf("%w: 集群 pve-prod 处于只读模式", pveadmin.ErrReadonly)}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)

	resp := doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/vms/pve1/101/status", csrf, `{"action":"start","kind":"qemu"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("readonly vm action = %d, want 409", resp.StatusCode)
	}
	entries, err := rec.List(context.Background(), audit.ListFilter{Action: auditActionPVEVMAction})
	if err != nil || len(entries.Entries) != 1 {
		t.Fatalf("readonly 拒绝应留审计: %d / %v", len(entries.Entries), err)
	}
}

// TestPVEVMActionAccepted 电源操作受理 → 202 {node,upid} + 审计。
func TestPVEVMActionAccepted(t *testing.T) {
	stub := &stubPVEAdmin{vmRef: &pveadmin.TaskRef{Node: "pve1", UPID: "UPID:pve1:ABC:1:qmstart:101:"}}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)

	resp := doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/vms/pve1/101/status", csrf, `{"action":"start","kind":"qemu"}`)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("vm action = %d body=%s, want 202", resp.StatusCode, raw)
	}
	entries, _ := rec.List(context.Background(), audit.ListFilter{Action: auditActionPVEVMAction})
	if len(entries.Entries) != 1 || !strings.Contains(entries.Entries[0].Detail["upid"].(string), "UPID:") {
		t.Fatalf("audit: %+v", entries)
	}
}

// TestCreateServerCarriesPVEDiscovery 登记成功后探测钩子触发,响应携带 pveDiscovery
// 与归池后的 kind/pool 字段;Token 值/密文不出现在任何响应。
func TestCreateServerCarriesPVEDiscovery(t *testing.T) {
	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	cred, err := v.Create(vault.CreateInput{Name: "k", Type: vault.TypeSSHPassword, Secret: "pw"})
	if err != nil {
		t.Fatalf("cred: %v", err)
	}
	stub := &stubPVEAdmin{disco: &pveadmin.DiscoveryResult{
		ClusterName: "pve-prod", PoolID: "p1", PoolName: "pve-pve-prod", CreatedSiblings: []string{"pve2"},
	}}
	rec := audit.New(st.DB, mask.NewMasker(), nil)
	srv := httptest.NewServer(New(testWebFSAuth(), authSvc,
		WithVault(v), WithServers(target.New(st.DB, v, nil)), WithPVEAdmin(stub), WithAudit(rec)))
	defer srv.Close()
	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)

	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/servers", csrf,
		`{"name":"pve1","host":"10.1.0.11","user":"root","credentialId":"`+cred.ID+`"}`)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		PVEDiscovery *pveadmin.DiscoveryResult `json:"pveDiscovery"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.PVEDiscovery == nil || out.PVEDiscovery.ClusterName != "pve-prod" ||
		len(out.PVEDiscovery.CreatedSiblings) != 1 {
		t.Fatalf("pveDiscovery = %+v", out.PVEDiscovery)
	}
	if len(stub.calls) != 1 || stub.calls[0] != "discover" {
		t.Fatalf("probe hook calls = %v", stub.calls)
	}
}

// TestCreateServerProbeFailureStillRegisters 探测钩子报错 → 登记仍成功,无 pveDiscovery 字段值。
func TestCreateServerProbeFailureStillRegisters(t *testing.T) {
	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	cred, err := v.Create(vault.CreateInput{Name: "k", Type: vault.TypeSSHPassword, Secret: "pw"})
	if err != nil {
		t.Fatalf("cred: %v", err)
	}
	stub := &stubPVEAdmin{} // OnServerRegistered 返回 (nil, nil) 语义由领域层保证;这里桩返回 nil disco
	rec := audit.New(st.DB, mask.NewMasker(), nil)
	srv := httptest.NewServer(New(testWebFSAuth(), authSvc,
		WithVault(v), WithServers(target.New(st.DB, v, nil)), WithPVEAdmin(stub), WithAudit(rec)))
	defer srv.Close()
	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)

	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/servers", csrf,
		`{"name":"web-1","host":"10.2.0.5","user":"deploy","credentialId":"`+cred.ID+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("probe 失败不应阻断登记: %d", resp.StatusCode)
	}
}

// TestAppDeployRejectsPVENode 应用商店部署到 PVE 物理机 → 422 pve_node_not_target。
func TestAppDeployRejectsPVENode(t *testing.T) {
	apps, servers := rustfsDeployFakes()
	servers.srv.Kind = target.KindPVENode

	code, raw := runDeployRaw(t, apps, servers, nil, `{"templateId":"tpl-1","params":{}}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(raw, "pve_node_not_target") {
		t.Fatalf("deploy to pve node = %d %s, want 422 pve_node_not_target", code, raw)
	}
}

// TestServerPoolsEndpoint GET /api/server-pools 只读列表(default 池 + 计数)。
func TestServerPoolsEndpoint(t *testing.T) {
	stub := &stubPVEAdmin{}
	base, client, csrf, _ := setupPVEAdminAPI(t, stub)

	resp := doJSON(t, client, http.MethodGet, base+"/api/server-pools", csrf, "")
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("server-pools = %d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Items []serverPoolDTO `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].ID != target.DefaultPoolID || out.Items[0].Kind != target.KindStandard {
		t.Fatalf("pools = %+v, want default standard pool", out.Items)
	}
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return raw
}
