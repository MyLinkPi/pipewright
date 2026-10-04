package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/labels"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// setupLabelsAPI 构造带 auth + 标签登记处的测试 server(无服务器 → 删除不做引用检查)。
func setupLabelsAPI(t *testing.T) (*httptest.Server, *http.Client, string) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	lsvc := labels.New(st.DB, nil)
	srv := httptest.NewServer(New(testWebFSAuth(), svc, WithVault(v), WithLabels(lsvc)))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	return srv, client, csrf
}

func TestLabelsCRUDContract(t *testing.T) {
	srv, client, csrf := setupLabelsAPI(t)

	// 未登录不可读。
	unauth, err := http.Get(srv.URL + "/api/labels")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: want 401, got %d", unauth.StatusCode)
	}

	// 建两个标签(含悬置的 gpu);非法名 400;重复 409。
	for _, name := range []string{"linux", "gpu"} {
		resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/labels", csrf,
			fmt.Sprintf(`{"name":%q}`, name))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("create %q: want 201, got %d (%s)", name, resp.StatusCode, raw)
		}
	}
	if resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/labels", csrf, `{"name":"bad name!"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid name: want 400, got %d", resp.StatusCode)
	}
	if resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/labels", csrf, `{"name":"linux"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate: want 409, got %d", resp.StatusCode)
	}

	// 列表往返(按名排序)。
	listResp := doJSON(t, client, http.MethodGet, srv.URL+"/api/labels", csrf, "")
	defer listResp.Body.Close()
	raw, _ := io.ReadAll(listResp.Body)
	var got struct {
		Items []struct {
			Name      string `json:"name"`
			CreatedAt string `json:"createdAt"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode list: %v (%s)", err, raw)
	}
	if len(got.Items) != 2 || got.Items[0].Name != "gpu" || got.Items[1].Name != "linux" {
		t.Fatalf("unexpected items: %s", raw)
	}

	// 删除:204;再删 404。
	if resp := doJSON(t, client, http.MethodDelete, srv.URL+"/api/labels/gpu", csrf, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d", resp.StatusCode)
	}
	if resp := doJSON(t, client, http.MethodDelete, srv.URL+"/api/labels/gpu", csrf, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing: want 404, got %d", resp.StatusCode)
	}
}

func TestLabelsUnavailableWithoutService(t *testing.T) {
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := httptest.NewServer(New(testWebFSAuth(), svc))
	t.Cleanup(srv.Close)
	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)

	if resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/labels", csrf, ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("list without svc: want 503, got %d", resp.StatusCode)
	}
}
