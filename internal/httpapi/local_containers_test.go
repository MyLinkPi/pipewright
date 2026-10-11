package httpapi

// 控制节点本机容器域的 HTTP 契约测试。
//
// 覆盖:容器域端点(容器/镜像/…)对 serverID == "__local__" 走**本机执行**,返回正常 DTO
// (而非 404);登记域端点(/servers/{id})仍把本机视为不存在;真实 id 行为不变。
// 断言刻意不依赖测试机是否装了 docker(命令不存在 → runtime:"" + reachable:true 的降级路径)。

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/huangchengsir/pipewright/internal/target"
)

// localHostEnvelope 是容器域 DTO 的最小公共字段(容器/镜像/卷/网络响应同形)。
type localHostEnvelope struct {
	ServerID  string `json:"serverId"`
	Reachable bool   `json:"reachable"`
	Runtime   string `json:"runtime"`
	Error     string `json:"error"`
}

func getLocalHostEnvelope(t *testing.T, url string) (int, localHostEnvelope) {
	t.Helper()
	resp, err := http.DefaultClient.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env localHostEnvelope
	_ = json.Unmarshal(raw, &env)
	return resp.StatusCode, env
}

// TestLocalHostContainersEndpoint 容器列表端点认控制节点本机:200 + serverId=__local__ + reachable。
func TestLocalHostContainersEndpoint(t *testing.T) {
	srv, client, _ := setupServerAPI(t, linuxLikeDialer())

	resp, err := client.Get(srv.URL + "/api/servers/" + target.LocalHostID + "/containers")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var out localHostEnvelope
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	if out.ServerID != target.LocalHostID {
		t.Fatalf("serverId = %q, want %q", out.ServerID, target.LocalHostID)
	}
	// 本机执行永远「连得上」:无 docker 时是 runtime:"" + 人读说明,而不是不可达。
	if !out.Reachable {
		t.Fatalf("control node must stay reachable even without a runtime: %+v", out)
	}
}

// TestLocalHostImagesEndpoint 容器域的其它端点(此处取镜像列表)同样认本机。
func TestLocalHostImagesEndpoint(t *testing.T) {
	srv, client, _ := setupServerAPI(t, linuxLikeDialer())
	resp, err := client.Get(srv.URL + "/api/servers/" + target.LocalHostID + "/images")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}
	var out localHostEnvelope
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	if out.ServerID != target.LocalHostID || !out.Reachable {
		t.Fatalf("images response = %+v", out)
	}
}

// TestUnknownServerContainersEndpoint 未登记的普通 id 仍在定位阶段失败(容器域透传语义未变)。
func TestUnknownServerContainersEndpoint(t *testing.T) {
	srv, client, _ := setupServerAPI(t, linuxLikeDialer())
	resp, err := client.Get(srv.URL + "/api/servers/no-such-server/containers")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestLocalHostNotARegisteredServer 登记域(/servers/{id})不认控制节点 —— 它不是可登记实体。
func TestLocalHostNotARegisteredServer(t *testing.T) {
	srv, client, _ := setupServerAPI(t, linuxLikeDialer())
	resp, err := client.Get(srv.URL + "/api/servers/" + target.LocalHostID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (control node is not a registered server)", resp.StatusCode)
	}
}

// TestLocalHostContainersRequiresAuth 未登录 → 401(与其余 /api 一致)。
func TestLocalHostContainersRequiresAuth(t *testing.T) {
	srv, _, _ := setupServerAPI(t, linuxLikeDialer())
	status, _ := getLocalHostEnvelope(t, srv.URL+"/api/servers/"+target.LocalHostID+"/containers")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}
