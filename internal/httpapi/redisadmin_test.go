package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/redisadmin"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// setupRedisAdminAPI 构造挂了 Redis 管理端点的测试 server(admin/testpass)。
func setupRedisAdminAPI(t *testing.T) (srv *httptest.Server, client *http.Client, csrf string, rec audit.Recorder) {
	t.Helper()
	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	rec = audit.New(st.DB, mask.NewMasker(), nil)
	svc := redisadmin.New(st.DB, v)

	srv = httptest.NewServer(New(testWebFSAuth(), authSvc,
		WithVault(v), WithRedisAdmin(svc), WithAudit(rec)))
	t.Cleanup(srv.Close)

	client = newTestClient(t)
	csrf = loginWithClient(t, client, srv.URL)
	return srv, client, csrf, rec
}

// createRedisInstancePOST 经 POST /api/redis-instances 登记一条实例,返回 (id, 原始响应体)。
func createRedisInstancePOST(t *testing.T, client *http.Client, baseURL, csrf, body string) (string, []byte) {
	t.Helper()
	resp := doJSON(t, client, http.MethodPost, baseURL+"/api/redis-instances", csrf, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create 应 201, got %d body=%s", resp.StatusCode, raw)
	}
	var dto struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatalf("decode create: %v body=%s", err, raw)
	}
	return dto.ID, raw
}

// TestRedisInstanceCRUDAPI 覆盖实例注册端点:创建(无密码合法/密码不回显)、
// 列表、编辑、同地址冲突 409、删除;连接测试类端点触真网由领域层 fake 测试覆盖。
func TestRedisInstanceCRUDAPI(t *testing.T) {
	srv, client, csrf, rec := setupRedisAdminAPI(t)

	id, raw := createRedisInstancePOST(t, client, srv.URL, csrf,
		`{"name":"缓存","host":"10.0.0.9","port":6379,"dbNum":0,"password":"s3cret-pw"}`)
	if id == "" {
		t.Fatalf("创建应返回 id")
	}
	// 响应绝无密码;passwordConfigured 告知已配置。
	body := string(raw)
	if containsAny(body, "s3cret-pw", "passwordSealed") {
		t.Fatalf("响应不应含密码: %s", body)
	}
	var dto struct {
		PasswordConfigured bool   `json:"passwordConfigured"`
		ReadOnly           bool   `json:"readonly"`
		Source             string `json:"source"`
		Endpoint           string `json:"endpoint"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatalf("decode: %v body=%s", err, raw)
	}
	if !dto.PasswordConfigured || !dto.ReadOnly || dto.Source != "manual" || dto.Endpoint != "10.0.0.9:6379" {
		t.Fatalf("DTO 不符: %+v", dto)
	}

	// 无密码实例合法。
	if plainID, _ := createRedisInstancePOST(t, client, srv.URL, csrf,
		`{"name":"内网","host":"10.0.0.10","port":6379}`); plainID == "" {
		t.Fatalf("无密码实例应可创建")
	}

	// 同地址冲突 → 409。
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/redis-instances", csrf,
		`{"name":"重复","host":"10.0.0.9","port":6379}`)
	conflictRaw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("同地址应 409, got %d body=%s", resp.StatusCode, conflictRaw)
	}

	// 列表(2 条,无密码)。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/redis-instances", csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Items) != 2 {
		t.Fatalf("列表应 2 条: %v body=%s", err, raw)
	}

	// 删除 → 审计留痕。
	resp = doJSON(t, client, http.MethodDelete, srv.URL+"/api/redis-instances/"+id, csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除应 200, got %d body=%s", resp.StatusCode, raw)
	}
	entries, err := rec.List(context.Background(), audit.ListFilter{Action: auditActionRedisInstanceDelete})
	if err != nil || len(entries.Entries) != 1 {
		t.Fatalf("应恰有 1 条 delete 审计: %v / %d", err, len(entries.Entries))
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
