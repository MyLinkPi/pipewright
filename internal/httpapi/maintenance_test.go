package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/maintenance"
)

// POST /api/maintenance/cleanup:白名单外 target → 400;合法 target → 200 + 结果 JSON;
// 未注入维护服务 → 503。
func TestMaintenanceCleanupWhitelistAndRun(t *testing.T) {
	dir := t.TempDir()
	// 造一个超龄孤儿,让 local-temp 有可清项。
	orphan := filepath.Join(dir, "pipewright-build-x")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := time.Now().Add(-72 * time.Hour)
	_ = os.Chtimes(orphan, old, old)

	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	svc := maintenance.New(st.DB).WithTempDir(dir)

	withSvc := httptest.NewServer(New(testWebFSAuth(), authSvc, WithMaintenance(svc)))
	t.Cleanup(withSvc.Close)
	c1 := newTestClient(t)
	csrf1 := loginWithClient(t, c1, withSvc.URL)

	// 白名单外(哪怕看起来像命令)→ 400。
	resp := doJSON(t, c1, http.MethodPost, withSvc.URL+"/api/maintenance/cleanup", csrf1, `{"target":"rm -rf /"}`)
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("非法 target 应 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// local-temp → 200 + ok + items=1 + 执行明细(删了哪个文件一目了然)。
	resp = doJSON(t, c1, http.MethodPost, withSvc.URL+"/api/maintenance/cleanup", csrf1, `{"target":"local-temp"}`)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("local-temp = %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"target":"local-temp"`) || !strings.Contains(string(raw), `"ok":true`) || !strings.Contains(string(raw), `"items":1`) {
		t.Fatalf("结果 JSON 不符:%s", raw)
	}
	if !strings.Contains(string(raw), "pipewright-build-x") || !strings.Contains(string(raw), "扫描目录") {
		t.Fatalf("结果应带执行明细(扫描目录 + 删除路径):%s", raw)
	}

	// 未注入维护服务 → 503。
	without := httptest.NewServer(New(testWebFSAuth(), authSvc))
	t.Cleanup(without.Close)
	c2 := newTestClient(t)
	csrf2 := loginWithClient(t, c2, without.URL)
	resp = doJSON(t, c2, http.MethodPost, without.URL+"/api/maintenance/cleanup", csrf2, `{"target":"buildcache"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		resp.Body.Close()
		t.Fatalf("未装配服务应 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
