package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/dnsprovider"
	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// setupDNSProvidersAPI 构造挂了 DNS 提供商端点的测试 server(admin/testpass),
// 并预建一个 cloudflare 提供商(凭据 cf-secret 已入 vault),返回其 id 与审计记录器。
func setupDNSProvidersAPI(t *testing.T) (srv *httptest.Server, client *http.Client, csrf, providerID string, v vault.Vault, rec audit.Recorder) {
	t.Helper()
	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v = vault.New(st.DB, testMasterKey())
	rec = audit.New(st.DB, mask.NewMasker(), nil)
	dnsSvc := dnsprovider.New(st.DB, v)

	cred, err := v.Create(vault.CreateInput{Name: "DNS · CF", Type: vault.TypeDNSToken, Secret: "cf-secret"})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	p, err := dnsSvc.Create(context.Background(), dnsprovider.CreateInput{
		Type: "cloudflare", Name: "CF", CredentialID: cred.ID, BaseDomains: []string{"example.com"},
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}

	srv = httptest.NewServer(New(testWebFSAuth(), authSvc,
		WithVault(v), WithDNSProviders(dnsSvc), WithAudit(rec)))
	t.Cleanup(srv.Close)

	client = newTestClient(t)
	csrf = loginWithClient(t, client, srv.URL)
	return srv, client, csrf, p.ID, v, rec
}

// getProviderName 经 GET /api/dns/providers 回读指定提供商的展示名(验证是否被改)。
func getProviderName(t *testing.T, client *http.Client, srvURL, csrf, id string) string {
	t.Helper()
	resp := doJSON(t, client, http.MethodGet, srvURL+"/api/dns/providers", csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list providers 应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode list: %v body=%s", err, raw)
	}
	for _, it := range list.Items {
		if it.ID == id {
			return it.Name
		}
	}
	t.Fatalf("提供商 %s 不在列表中: %s", id, raw)
	return ""
}

// --- PUT:非法轮换入参(空 secret)须在落库前拒绝,不留半生效状态、不产生审计 ---

func TestUpdateDNSProviderEmptySecretRejectedBeforeMutation(t *testing.T) {
	srv, client, csrf, id, _, rec := setupDNSProvidersAPI(t)

	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/dns/providers/"+id, csrf,
		`{"name":"CF2","secret":"  "}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 secret 应 400, got %d body=%s", resp.StatusCode, raw)
	}
	// name 不得被改(校验先于落库)。
	if got := getProviderName(t, client, srv.URL, csrf, id); got != "CF" {
		t.Fatalf("拒绝路径不应改 name, got %q", got)
	}
	// 未发生任何变更 → 不应有 update 审计。
	res, err := rec.List(context.Background(), audit.ListFilter{Action: auditActionDNSProviderUpdate})
	if err != nil || len(res.Entries) != 0 {
		t.Fatalf("拒绝路径不应写 update 审计, got %d err=%v", len(res.Entries), err)
	}
}

// --- PUT:改名成功 → 200 + 审计留痕;轮换 Secret → vault 中为新值、审计标 secretRotated ---

func TestUpdateDNSProviderRenameAndRotate(t *testing.T) {
	srv, client, csrf, id, v, rec := setupDNSProvidersAPI(t)

	// 改名。
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/dns/providers/"+id, csrf, `{"name":"CF2"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改名应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var dto struct {
		Name                 string `json:"name"`
		CredentialConfigured bool   `json:"credentialConfigured"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatalf("decode: %v body=%s", err, raw)
	}
	if dto.Name != "CF2" || !dto.CredentialConfigured {
		t.Fatalf("DTO 不符: %+v", dto)
	}

	// 轮换 Secret:vault 中应为新值,credentialId 不变(credentialConfigured 仍 true)。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/dns/providers/"+id, csrf, `{"secret":"rotated-secret"}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("轮换应 200, got %d body=%s", resp.StatusCode, raw)
	}
	// 经列表确认仍已配凭据;直接 Reveal 验证新 Secret 生效(测试内进程内取值,不外泄)。
	list, err := v.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("应恰有 1 条凭据: %v / %d", err, len(list))
	}
	secret, err := v.Reveal(list[0].ID)
	if err != nil || secret != "rotated-secret" {
		t.Fatalf("轮换后 vault 应为新 Secret: %v", err)
	}

	// 两次成功各写一条审计;轮换那条 secretRotated=true。
	res, err := rec.List(context.Background(), audit.ListFilter{Action: auditActionDNSProviderUpdate})
	if err != nil || len(res.Entries) != 2 {
		t.Fatalf("应有 2 条 update 审计, got %d err=%v", len(res.Entries), err)
	}
	var sawRotated bool
	for _, e := range res.Entries {
		if e.Detail["secretRotated"] == true {
			sawRotated = true
		}
		if _, leak := e.Detail["secret"]; leak {
			t.Fatalf("审计 detail 绝不应含 secret 字段: %+v", e.Detail)
		}
	}
	if !sawRotated {
		t.Fatalf("轮换审计应标 secretRotated: %+v", res.Entries)
	}
}

// --- DELETE:删除提供商须级联删除其 vault 凭据,不留「DNS · ××」孤儿条目 ---

func TestDeleteDNSProviderCascadesCredential(t *testing.T) {
	srv, client, csrf, id, v, rec := setupDNSProvidersAPI(t)

	resp := doJSON(t, client, http.MethodDelete, srv.URL+"/api/dns/providers/"+id, csrf, "")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除应 200, got %d body=%s", resp.StatusCode, raw)
	}

	// 提供商与凭据都应消失(vault 只剩 0 条,无孤儿)。
	list, err := v.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("删除后 vault 应为空(无孤儿凭据): %v / %d", err, len(list))
	}
	res, err := rec.List(context.Background(), audit.ListFilter{Action: auditActionDNSProviderDelete})
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("应有 1 条 delete 审计, got %d err=%v", len(res.Entries), err)
	}
	if res.Entries[0].Detail["credentialDeleted"] != true {
		t.Fatalf("delete 审计应标 credentialDeleted: %+v", res.Entries[0].Detail)
	}

	// 再删同一 id → 404,不产生新审计。
	resp = doJSON(t, client, http.MethodDelete, srv.URL+"/api/dns/providers/"+id, csrf, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除应 404, got %d", resp.StatusCode)
	}
	res, _ = rec.List(context.Background(), audit.ListFilter{Action: auditActionDNSProviderDelete})
	if len(res.Entries) != 1 {
		t.Fatalf("404 路径不应写 delete 审计, got %d", len(res.Entries))
	}
}
