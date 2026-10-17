// ceph_test.go — Ceph 管理面 HTTP 端到端测试(真实 target.Service + 假 SSH dialer;
// 覆盖:集群登记/部署管线(202+轮询)/状态/池管理/错误映射)。
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/ceph"
	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// cephDialer 按「命令全文」路由输出的假 SSH 层(root 登录,免 sudo 探测)。
type cephDialer struct{ lastCmds []string }

func (d *cephDialer) Run(_ context.Context, addr string, _ target.SSHConfig, cmd []string) (*target.ExecResult, error) {
	joined := strings.Join(cmd, " ")
	d.lastCmds = append(d.lastCmds, joined)
	out, code := cephDialerHandle(joined)
	if code != 0 {
		return &target.ExecResult{ExitCode: code, Stderr: out}, nil
	}
	return &target.ExecResult{ExitCode: 0, Stdout: out}, nil
}
func (d *cephDialer) RunWithStdin(_ context.Context, addr string, _ target.SSHConfig, cmd []string, _ io.Reader) (*target.ExecResult, error) {
	return d.Run(context.Background(), addr, target.SSHConfig{}, cmd)
}
func (d *cephDialer) RunStream(_ context.Context, _ string, _ target.SSHConfig, _ []string) (io.ReadCloser, error) {
	return nil, nil
}
func (d *cephDialer) RunInteractive(_ context.Context, _ string, _ target.SSHConfig, _ []string) (target.Session, error) {
	return nil, nil
}

// cephDialerHandle 覆盖部署管线 + 管理面全部命令形态(与域层测试同源,独立精简)。
func cephDialerHandle(joined string) (string, int) {
	switch {
	case joined == "id -u":
		return "0", 0
	case joined == "hostname":
		return "mon-host", 0
	case joined == "docker --version", joined == "systemctl --version", joined == "python3 --version", joined == "lvs --version":
		return "ok", 0
	case strings.HasPrefix(joined, "timedatectl show"):
		return "1", 0
	case strings.HasPrefix(joined, "lsblk "):
		return `{"blockdevices":[{"path":"/dev/sdb","type":"disk","fstype":"","mountpoint":null}]}`, 0
	case strings.HasPrefix(joined, "apt-get "):
		return "ok", 0
	case joined == "cephadm --version":
		return "cephadm version 19.2.1", 0
	case strings.HasPrefix(joined, "cephadm "):
		return "Cluster fsid: 3b84f4a1-1111-2222-3333-abcdefabcdef", 0
	case joined == "ceph -v":
		return "ceph version 19.2.1", 0
	case strings.HasPrefix(joined, "ceph orch host "):
		if strings.Contains(joined, "-f json") {
			return `[{"host":"mon-host","addr":"10.0.0.11","labels":[],"status":""}]`, 0
		}
		return "ok", 0
	case strings.HasPrefix(joined, "ceph orch daemon add osd"):
		return "Created osd.osd.0 on host 'mon-host'", 0
	case joined == "ceph -s -f json":
		return `{"health":{"status":"HEALTH_OK"},"monmap":{"num_mons":1},"osdmap":{"num_osds":1,"num_up_osds":1,"num_in_osds":1},"pgmap":{"bytes_total":1000,"bytes_used":400,"bytes_avail":600,"pgs_by_state":[{"state_name":"active+clean","count":64}]}}`, 0
	case strings.HasPrefix(joined, "ceph health detail"):
		return `{"checks":{}}`, 0
	case strings.HasPrefix(joined, "ceph mgr services"):
		return `{"dashboard":"https://10.0.0.11:8443/"}`, 0
	case strings.HasPrefix(joined, "ceph osd pool ls detail"):
		return `[{"pool_name":"rbd","size":3,"pg_num":32,"bytes_used":1024,"application_metadata":{"rbd":{}}}]`, 0
	case strings.HasPrefix(joined, "ceph osd pool "):
		return "ok", 0
	case strings.HasPrefix(joined, "ceph osd df"):
		return `{"nodes":[{"id":0,"status":"up,in","kb":1024,"used_kb":512}]}`, 0
	case strings.HasPrefix(joined, "ceph osd metadata"):
		return `[{"id":0,"hostname":"mon-host","devices":["sdb"]}]`, 0
	}
	return "", 0
}

// setupCephAPI 构造挂了 Ceph 端点的测试 server,并登记一台 root 登录主机。
func setupCephAPI(t *testing.T) (*httptest.Server, *http.Client, string) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	rec := audit.New(st.DB, mask.NewMasker(), nil)
	dialer := &cephDialer{}
	tsvc := target.New(st.DB, v, dialer)

	// 登记一台 root 主机(凭据内容无关紧要:dialer 不校验)。
	cred, err := v.Create(vault.CreateInput{Name: "k", Type: vault.TypeSSHKey, Secret: testPEM})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tsvc.Create(context.Background(), target.CreateInput{
		Name: "node1", Host: "10.0.0.11", Port: 22, User: "root", CredentialID: cred.ID,
	}); err != nil {
		t.Fatal(err)
	}

	cephSvc := ceph.New(st.DB, tsvc, v)
	srv := httptest.NewServer(New(testWebFSAuth(), svc,
		WithVault(v), WithServers(tsvc), WithCeph(cephSvc), WithAudit(rec)))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	return srv, client, csrf
}

func TestCephClusterLifecycleHTTP(t *testing.T) {
	srv, client, csrf := setupCephAPI(t)

	// 创建:非法名 400;合法 201。
	resp := doJSON(t, client, "POST", srv.URL+"/api/ceph/clusters", csrf, `{"name":"bad name!"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法名应 400,得 %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doJSON(t, client, "POST", srv.URL+"/api/ceph/clusters", csrf,
		`{"name":"prod","bootstrapServerId":"srv-unknown","hosts":[{"serverId":"srv-unknown"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知主机应 400,得 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 先取登记主机 id(唯一一台)。
	var serversResp struct {
		Items []struct {
			ID string `json:"id"`
		}
	}
	if r, err := client.Get(srv.URL + "/api/servers"); err != nil {
		t.Fatal(err)
	} else {
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		_ = json.Unmarshal(raw, &serversResp)
	}
	if len(serversResp.Items) != 1 {
		t.Fatal("应有一台登记主机")
	}
	sid := serversResp.Items[0].ID

	resp = doJSON(t, client, "POST", srv.URL+"/api/ceph/clusters", csrf,
		`{"name":"prod","bootstrapServerId":"`+sid+`","hosts":[{"serverId":"`+sid+`","roles":["mon","mgr"],"devices":["/dev/sdb"]}]}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建应 201,得 %d: %s", resp.StatusCode, string(raw))
	}
	var cl cephClusterDTO
	if err := json.Unmarshal(raw, &cl); err != nil || cl.State != "created" || cl.DashboardUser != "admin" {
		t.Fatalf("DTO 不符: %s", string(raw))
	}

	// 部署:202;轮询至完成。
	resp = doJSON(t, client, "POST", srv.URL+"/api/ceph/clusters/"+cl.ID+"/deploy", csrf, `{}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy 应 202,得 %d", resp.StatusCode)
	}
	resp.Body.Close()
	deadline := time.Now().Add(15 * time.Second)
	var st ceph.DeployStatus
	for {
		r, err := client.Get(srv.URL + "/api/ceph/clusters/" + cl.ID + "/deploy/status")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ = io.ReadAll(r.Body)
		r.Body.Close()
		_ = json.Unmarshal(raw, &st)
		if st.Done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("部署超时: %+v", st)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if st.Error != "" {
		t.Fatalf("部署失败: %s", st.Error)
	}

	// 详情:ready + fsid/dashboard 回填。
	r, err := client.Get(srv.URL + "/api/ceph/clusters/" + cl.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(r.Body)
	r.Body.Close()
	var detail struct {
		Cluster cephClusterDTO `json:"cluster"`
	}
	_ = json.Unmarshal(raw, &detail)
	if detail.Cluster.State != "ready" || detail.Cluster.FSID == "" || detail.Cluster.DashboardURL == "" {
		t.Fatalf("回填不符: %s", string(raw))
	}
	// dashboard 密码只入 vault,任何响应不含明文口令。StartDeploy 生成的口令不回传;
	// 凭据 reveal 走既有 /api/credentials/{id}/reveal,此处只断言引用存在。
	if detail.Cluster.DashboardCredentialID == "" {
		t.Fatal("dashboard 凭据引用应已生成(vault 已配置)")
	}

	// 状态 + 池。
	r, _ = client.Get(srv.URL + "/api/ceph/clusters/" + cl.ID + "/status")
	raw, _ = io.ReadAll(r.Body)
	r.Body.Close()
	var status ceph.ClusterStatus
	_ = json.Unmarshal(raw, &status)
	if status.Health != "HEALTH_OK" || status.MonCount != 1 {
		t.Fatalf("状态不符: %s", string(raw))
	}
	r, _ = client.Get(srv.URL + "/api/ceph/clusters/" + cl.ID + "/pools")
	raw, _ = io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(raw), `"rbd"`) {
		t.Fatalf("池列表不符: %s", string(raw))
	}

	// 建池(写方法过 CSRF;缺失 CSRF 的写请求应被拒)。
	if r2, err := http.NewRequest("POST", srv.URL+"/api/ceph/clusters/"+cl.ID+"/pools",
		strings.NewReader(`{"name":"data","pgNum":64}`)); err == nil {
		resp2, _ := client.Do(r2)
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusForbidden {
			t.Fatalf("无 CSRF 写请求应 403,得 %d", resp2.StatusCode)
		}
	}
	resp = doJSON(t, client, "POST", srv.URL+"/api/ceph/clusters/"+cl.ID+"/pools", csrf, `{"name":"data","pgNum":64,"size":3,"apps":["rbd"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建池应 201,得 %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 未知集群 404。
	r, _ = client.Get(srv.URL + "/api/ceph/clusters/nope/status")
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("未知集群应 404,得 %d", r.StatusCode)
	}
	r.Body.Close()
}
