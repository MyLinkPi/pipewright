// artifact_transfer_test.go 覆盖产物跨环境传输 HTTP 面:发起(参数校验/错误码映射)、
// 状态轮询(透传/404)、取消(进行中 ok/已终态 409)。
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/build"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// fakeArtifactTransferService 是传输服务 fake:Start 可注入错误并记录入参;Get 回固定
// 快照;Cancel 记录调用。
type fakeArtifactTransferService struct {
	startErr error
	gotInput build.TransferInput
	status   *build.TransferStatus
	getOK    bool
	cancelID string
}

func (f *fakeArtifactTransferService) Start(_ context.Context, in build.TransferInput) (string, error) {
	f.gotInput = in
	if f.startErr != nil {
		return "", f.startErr
	}
	return "at-fixed", nil
}

func (f *fakeArtifactTransferService) Get(id string) (*build.TransferStatus, bool) {
	if !f.getOK || f.status == nil || f.status.ID != id {
		return nil, false
	}
	return f.status, true
}

func (f *fakeArtifactTransferService) Cancel(id string) bool {
	if !f.getOK || f.status == nil || f.status.ID != id || f.status.Status != build.TransferRunning {
		return false
	}
	f.cancelID = id
	return true
}

// setupArtifactTransferServer 起一个挂了传输服务 fake 的测试服务并登录 admin。
func setupArtifactTransferServer(t *testing.T, svc *fakeArtifactTransferService) (*httptest.Server, *http.Client, string) {
	t.Helper()
	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	srv := httptest.NewServer(New(testWebFSAuth(), authSvc, WithVault(v), WithArtifactTransfer(svc)))
	t.Cleanup(srv.Close)
	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	return srv, client, csrf
}

// postTransfer 发起传输并解析响应码与错误 code。
func postTransfer(t *testing.T, client *http.Client, srvURL, csrf, body string) (int, string, map[string]any) {
	t.Helper()
	resp := doJSON(t, client, http.MethodPost, srvURL+"/api/runs/run-1/artifacts/transfer", csrf, body)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	code := ""
	if e, ok := out["error"].(map[string]any); ok {
		code, _ = e["code"].(string)
	}
	return resp.StatusCode, code, out
}

func TestArtifactTransferStartHandler(t *testing.T) {
	svc := &fakeArtifactTransferService{}
	srv, client, csrf := setupArtifactTransferServer(t, svc)

	code, errCode, body := postTransfer(t, client, srv.URL, csrf,
		`{"nodeId":"peer-1","artifactIds":["a1","a2"],"revisionLabel":"promote"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d(%s), want 200", code, errCode)
	}
	if id, _ := body["transferId"].(string); id != "at-fixed" {
		t.Fatalf("transferId = %v, want at-fixed", body["transferId"])
	}
	if svc.gotInput.RunID != "run-1" || svc.gotInput.PeerNodeID != "peer-1" ||
		len(svc.gotInput.ArtifactIDs) != 2 || svc.gotInput.RevisionLabel != "promote" {
		t.Fatalf("入参转发不符:%+v", svc.gotInput)
	}

	// 缺目标节点 / 缺产物清单 / 非法 JSON → 400。
	if code, errCode, _ := postTransfer(t, client, srv.URL, csrf, `{"artifactIds":["a1"]}`); code != http.StatusBadRequest || errCode != "invalid_body" {
		t.Fatalf("缺 nodeId: status=%d code=%s", code, errCode)
	}
	if code, errCode, _ := postTransfer(t, client, srv.URL, csrf, `{"nodeId":"peer-1"}`); code != http.StatusBadRequest || errCode != "invalid_body" {
		t.Fatalf("缺 artifactIds: status=%d code=%s", code, errCode)
	}
	if code, _, _ := postTransfer(t, client, srv.URL, csrf, `{bad json`); code != http.StatusBadRequest {
		t.Fatalf("非法 JSON: status=%d", code)
	}
}

func TestArtifactTransferStartErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantKey  string
	}{
		{"run 不存在", fmt.Errorf("get: %w", run.ErrNotFound), http.StatusNotFound, "run_not_found"},
		{"产物不存在", fmt.Errorf("artifact: %w", run.ErrArtifactNotFound), http.StatusNotFound, "artifact_not_found"},
		{"对端不存在", fmt.Errorf("mesh: %w", mesh.ErrNotFound), http.StatusNotFound, "peer_not_found"},
		{"不可传输", fmt.Errorf("build: %w", build.ErrArtifactNotTransferable), http.StatusBadRequest, "artifact_not_transferable"},
		{"对端未组网", fmt.Errorf("build: %w", build.ErrTransferPeerNotNetworked), http.StatusConflict, "peer_not_networked"},
		{"并发上限", fmt.Errorf("build: %w", build.ErrTransferBusy), http.StatusTooManyRequests, "transfer_busy"},
		{"依赖未装配", fmt.Errorf("build: %w", build.ErrTransferUnavailable), http.StatusServiceUnavailable, "transfer_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeArtifactTransferService{startErr: tc.err}
			srv, client, csrf := setupArtifactTransferServer(t, svc)
			code, gotKey, _ := postTransfer(t, client, srv.URL, csrf, `{"nodeId":"peer-1","artifactIds":["a1"]}`)
			if code != tc.wantCode || gotKey != tc.wantKey {
				t.Fatalf("status=%d code=%s, want %d/%s", code, gotKey, tc.wantCode, tc.wantKey)
			}
		})
	}
}

func TestArtifactTransferStatusHandler(t *testing.T) {
	running := &build.TransferStatus{
		ID: "at-x", RunID: "run-1", ProjectName: "acme", PeerNodeID: "peer-1", PeerName: "prod",
		Items: []string{"app.jar"}, Status: build.TransferRunning, Phase: "传输中",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	svc := &fakeArtifactTransferService{status: running, getOK: true}
	srv, client, _ := setupArtifactTransferServer(t, svc)

	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/artifacts/transfers/at-x", "", "")
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200(%s)", resp.StatusCode, raw)
	}
	var out build.TransferStatus
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Status != build.TransferRunning || out.Phase != "传输中" || out.PeerName != "prod" {
		t.Fatalf("快照透传不符:%+v", out)
	}

	// 未知 id → 404。
	resp404 := doJSON(t, client, http.MethodGet, srv.URL+"/api/artifacts/transfers/at-nope", "", "")
	defer resp404.Body.Close()
	if resp404.StatusCode != http.StatusNotFound {
		t.Fatalf("未知 id status = %d, want 404", resp404.StatusCode)
	}
}

func TestArtifactTransferCancelHandler(t *testing.T) {
	running := &build.TransferStatus{ID: "at-x", Status: build.TransferRunning}
	svc := &fakeArtifactTransferService{status: running, getOK: true}
	srv, client, csrf := setupArtifactTransferServer(t, svc)

	resp := doJSON(t, client, http.MethodDelete, srv.URL+"/api/artifacts/transfers/at-x", csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取消进行中: status = %d, want 200", resp.StatusCode)
	}
	if svc.cancelID != "at-x" {
		t.Fatalf("Cancel 未被调用")
	}

	// 已终态 → 409。
	done := &build.TransferStatus{ID: "at-x", Status: build.TransferSuccess}
	svcDone := &fakeArtifactTransferService{status: done, getOK: true}
	srv2, client2, csrf2 := setupArtifactTransferServer(t, svcDone)
	resp409 := doJSON(t, client2, http.MethodDelete, srv2.URL+"/api/artifacts/transfers/at-x", csrf2, "")
	defer resp409.Body.Close()
	if resp409.StatusCode != http.StatusConflict {
		t.Fatalf("取消已终态: status = %d, want 409", resp409.StatusCode)
	}

	// 未知 id → 404(Get 不可见,Cancel 返回 false)。
	svcNone := &fakeArtifactTransferService{}
	srv3, client3, csrf3 := setupArtifactTransferServer(t, svcNone)
	resp404 := doJSON(t, client3, http.MethodDelete, srv3.URL+"/api/artifacts/transfers/at-nope", csrf3, "")
	defer resp404.Body.Close()
	if resp404.StatusCode != http.StatusNotFound {
		t.Fatalf("取消未知: status = %d, want 404", resp404.StatusCode)
	}
}
