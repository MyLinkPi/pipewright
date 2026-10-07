package httpapi

// servers_jumps_test.go 覆盖服务器跳板链(多跳)的 HTTP 契约:
//   - 创建时携带 jumps → 响应/回读一致(仅 ID 引用,无明文);
//   - PUT 语义:未带 jumps = 不修改;空数组 = 清空;非法跳 = 400;凭据类型错配 = 422;
//   - 被跳板链引用的凭据删除 → 409 in_use。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/target"
)

func TestServerJumpsAPI(t *testing.T) {
	srv, client, csrf := setupServerAPI(t, stubDialer{res: &target.ExecResult{Stdout: "ok"}})
	credID := newSSHCredAPI(t, client, srv.URL, csrf, "LEAKMARKER_jump_priv_key")

	// 创建时携带 1 跳。
	body := `{"name":"jump-srv","host":"10.0.0.5","port":22,"user":"deploy","credentialId":"` + credID + `",` +
		`"jumps":[{"host":"bastion.corp","port":2222,"user":"ops","credentialId":"` + credID + `"}]}`
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/servers", csrf, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "LEAKMARKER") {
		t.Fatalf("create 响应泄漏凭据明文: %s", raw)
	}
	var created struct {
		ID    string `json:"id"`
		Jumps []struct {
			Host         string `json:"host"`
			Port         int    `json:"port"`
			User         string `json:"user"`
			CredentialID string `json:"credentialId"`
		} `json:"jumps"`
	}
	_ = json.Unmarshal(raw, &created)
	if created.ID == "" || len(created.Jumps) != 1 {
		t.Fatalf("create 响应缺 id/jumps: %s", raw)
	}
	if created.Jumps[0].Host != "bastion.corp" || created.Jumps[0].Port != 2222 ||
		created.Jumps[0].User != "ops" || created.Jumps[0].CredentialID != credID {
		t.Fatalf("jumps roundtrip 不一致: %+v", created.Jumps)
	}

	// GET 回读。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/servers/"+created.ID, csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"host":"bastion.corp"`) {
		t.Fatalf("GET 未回读 jumps: %s", raw)
	}

	// PUT 未带 jumps = 保留。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/servers/"+created.ID, csrf, `{"user":"deploy2"}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "bastion.corp") {
		t.Fatalf("PUT 无 jumps 应保留链: %d %s", resp.StatusCode, raw)
	}

	// PUT 空数组 = 清空。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/servers/"+created.ID, csrf, `{"jumps":[]}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(raw), "bastion.corp") {
		t.Fatalf("PUT 空 jumps 应清空: %d %s", resp.StatusCode, raw)
	}

	// PUT 非法跳 = 400 invalid_server。
	bad := `{"jumps":[{"host":"","port":2222,"user":"ops","credentialId":"` + credID + `"}]}`
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/servers/"+created.ID, csrf, bad)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "invalid_server") {
		t.Fatalf("非法跳应 400 invalid_server: %d %s", resp.StatusCode, raw)
	}

	// 凭据类型错配(git token 作跳板凭据)= 422。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/credentials", csrf,
		`{"name":"gt","type":"git_token","secret":"tok"}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var gt struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &gt)
	if gt.ID == "" {
		t.Fatalf("create git_token failed: %s", raw)
	}
	badType := `{"jumps":[{"host":"h","port":22,"user":"u","credentialId":"` + gt.ID + `"}]}`
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/servers/"+created.ID, csrf, badType)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("类型错配应 422: %d %s", resp.StatusCode, raw)
	}

	// 绑定独立凭据为跳板后,删除该凭据 → 409 in_use。
	hopCred := newSSHCredAPI(t, client, srv.URL, csrf, "LEAKMARKER_hop2_key")
	bind := `{"jumps":[{"host":"h2","port":22,"user":"u2","credentialId":"` + hopCred + `"}]}`
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/servers/"+created.ID, csrf, bind)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bind jump failed: %d %s", resp.StatusCode, raw)
	}
	resp = doJSON(t, client, http.MethodDelete, srv.URL+"/api/credentials/"+hopCred, csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("删除被跳板引用的凭据应 409: %d %s", resp.StatusCode, raw)
	}
}
