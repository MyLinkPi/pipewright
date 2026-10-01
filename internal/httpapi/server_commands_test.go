package httpapi

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
	"github.com/huangchengsir/pipewright/internal/servercmd"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// batchDialer 按主机路由 Exec 结果/错误的多机假 SSH 层(批量命令端点测试用)。
type batchDialer struct {
	outByHost  map[string]*target.ExecResult
	errByHost  map[string]error
	calledHost []string
	lastCmd    []string
}

func (d *batchDialer) Run(_ context.Context, addr string, _ target.SSHConfig, cmd []string) (*target.ExecResult, error) {
	host := addr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		host = addr[:i]
	}
	d.calledHost = append(d.calledHost, host)
	d.lastCmd = cmd
	if err, ok := d.errByHost[host]; ok {
		return nil, err
	}
	if res, ok := d.outByHost[host]; ok {
		return res, nil
	}
	return &target.ExecResult{ExitCode: 0}, nil
}
func (d *batchDialer) RunWithStdin(_ context.Context, addr string, _ target.SSHConfig, cmd []string, _ io.Reader) (*target.ExecResult, error) {
	return d.Run(context.Background(), addr, target.SSHConfig{}, cmd)
}
func (d *batchDialer) RunStream(_ context.Context, _ string, _ target.SSHConfig, _ []string) (io.ReadCloser, error) {
	return nil, nil
}
func (d *batchDialer) RunInteractive(_ context.Context, _ string, _ target.SSHConfig, _ []string) (target.Session, error) {
	return nil, nil
}

// setupServerCommandsAPI 构造挂了批量命令端点的测试 server(admin/testpass)。
func setupServerCommandsAPI(t *testing.T, dialer *batchDialer) (*httptest.Server, *http.Client, string, audit.Recorder, *store.Store) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	v := vault.New(st.DB, testMasterKey())
	rec := audit.New(st.DB, mask.NewMasker(), nil)
	tsvc := target.New(st.DB, v, dialer)
	scmd := servercmd.New(st.DB, tsvc)
	srv := httptest.NewServer(New(testWebFSAuth(), svc,
		WithVault(v), WithServers(tsvc), WithServerCommands(scmd), WithAudit(rec)))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	return srv, client, csrf, rec, st
}

// newServerAtHost 经 API 建一条指定 host 的 server,返回 id。
func newServerAtHost(t *testing.T, client *http.Client, srvURL, csrf, name, host string) string {
	t.Helper()
	credID := newSSHCredAPI(t, client, srvURL, csrf, "LEAKMARKER_priv_key")
	body := `{"name":"` + name + `","host":"` + host + `","port":22,"user":"deploy","credentialId":"` + credID + `"}`
	resp := doJSON(t, client, http.MethodPost, srvURL+"/api/servers", csrf, body)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var s map[string]any
	_ = json.Unmarshal(raw, &s)
	id, _ := s["id"].(string)
	if id == "" {
		t.Fatalf("create server %s(%s) failed: %s", name, host, raw)
	}
	return id
}

func postBatchCommand(t *testing.T, client *http.Client, srvURL, csrf, body string) *http.Response {
	t.Helper()
	return doJSON(t, client, http.MethodPost, srvURL+"/api/servers/commands/batch", csrf, body)
}

// --- 校验:非法入参 → 400 invalid_command_request(命令不构造、不执行) ---

func TestBatchCommandValidation(t *testing.T) {
	dialer := &batchDialer{}
	srv, client, csrf, _, _ := setupServerCommandsAPI(t, dialer)
	id := newServerAtHost(t, client, srv.URL, csrf, "web-1", "10.0.0.1")

	overIDs := make([]string, 0, servercmd.MaxServers+1)
	for i := 0; i <= servercmd.MaxServers; i++ {
		overIDs = append(overIDs, fmt.Sprintf("s%d", i))
	}
	overJSON, _ := json.Marshal(overIDs)

	bad := []string{
		`{"command":"","serverIds":["` + id + `"]}`,
		`{"command":"   ","serverIds":["` + id + `"]}`,
		`{"command":"` + strings.Repeat("a", servercmd.MaxCommandLen+1) + `","serverIds":["` + id + `"]}`,
		`{"command":"ls","serverIds":[]}`,
		`{"command":"ls"}`,
		`{"command":"ls","serverIds":` + string(overJSON) + `}`,
		`not-json`,
	}
	for _, b := range bad {
		resp := postBatchCommand(t, client, srv.URL, csrf, b)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("非法入参应 400, got %d body=%s req=%.80s", resp.StatusCode, raw, b)
		}
		var e errBody
		_ = json.Unmarshal(raw, &e)
		if e.Error.Code != "invalid_command_request" && e.Error.Code != "bad_request" {
			t.Fatalf("错误码应 invalid_command_request/bad_request, got %q", e.Error.Code)
		}
	}
	if len(dialer.calledHost) != 0 {
		t.Fatalf("拒绝路径不应触达 SSH, got %v", dialer.calledHost)
	}
}

// --- 成功:200 + runId/items/summary + 命令经 sh -c array 化 + 审计留痕 ---

func TestBatchCommandSuccess(t *testing.T) {
	dialer := &batchDialer{outByHost: map[string]*target.ExecResult{
		"10.0.0.1": {Stdout: "Linux srv1", ExitCode: 0},
		"10.0.0.2": {Stdout: "Linux srv2", ExitCode: 0},
	}}
	srv, client, csrf, rec, _ := setupServerCommandsAPI(t, dialer)
	id1 := newServerAtHost(t, client, srv.URL, csrf, "web-1", "10.0.0.1")
	id2 := newServerAtHost(t, client, srv.URL, csrf, "web-2", "10.0.0.2")

	body := `{"command":"uname -a","serverIds":["` + id1 + `","` + id2 + `"],"timeoutSeconds":30}`
	resp := postBatchCommand(t, client, srv.URL, csrf, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("成功应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var dto batchCommandResponse
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatalf("decode: %v body=%s", err, raw)
	}
	if dto.RunID == "" || len(dto.Items) != 2 {
		t.Fatalf("runId/items 不符: %+v", dto)
	}
	if dto.Summary.Total != 2 || dto.Summary.OK != 2 || dto.Summary.Failed != 0 {
		t.Fatalf("summary 不符: %+v", dto.Summary)
	}
	byID := map[string]batchCommandItemDTO{}
	for _, it := range dto.Items {
		byID[it.ServerID] = it
	}
	if !byID[id1].OK || byID[id1].Name != "web-1" || byID[id1].Stdout != "Linux srv1" {
		t.Fatalf("web-1 item 不符: %+v", byID[id1])
	}
	// 命令 array 化:sh -c <命令>,三段独立。
	if len(dialer.lastCmd) != 3 || dialer.lastCmd[0] != "sh" || dialer.lastCmd[1] != "-c" || dialer.lastCmd[2] != "uname -a" {
		t.Fatalf("命令应经 sh -c array 形式, got %v", dialer.lastCmd)
	}

	// 审计留痕:action=server_command,detail 含机器数/成败计数/runId。
	res, err := rec.List(context.Background(), audit.ListFilter{Action: audit.ActionServerCommand})
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("应有 1 条 server_command 审计, got %d err=%v", len(res.Entries), err)
	}
	e := res.Entries[0]
	if e.TargetType != audit.TargetServer {
		t.Fatalf("审计 target_type 应 server, got %s", e.TargetType)
	}
	if e.Detail["command"] != "uname -a" || e.Detail["servers"] != float64(2) || e.Detail["ok"] != float64(2) {
		t.Fatalf("审计 detail 不符: %+v", e.Detail)
	}
}

// --- 逐机容错:一台失败 / 一台未知 id → 200,只标记该机 ok:false(不 500) ---

func TestBatchCommandPerItemFaultTolerance(t *testing.T) {
	dialer := &batchDialer{
		outByHost: map[string]*target.ExecResult{"10.0.0.1": {Stdout: "ok", ExitCode: 0}},
		errByHost: map[string]error{"10.0.0.2": target.ErrUnreachable},
	}
	srv, client, csrf, _, _ := setupServerCommandsAPI(t, dialer)
	id1 := newServerAtHost(t, client, srv.URL, csrf, "web-1", "10.0.0.1")
	id2 := newServerAtHost(t, client, srv.URL, csrf, "web-2", "10.0.0.2")

	body := `{"command":"uptime","serverIds":["` + id1 + `","` + id2 + `","ghost-id"]}`
	resp := postBatchCommand(t, client, srv.URL, csrf, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("逐机失败应 200(不 500), got %d body=%s", resp.StatusCode, raw)
	}
	var dto batchCommandResponse
	_ = json.Unmarshal(raw, &dto)
	if dto.Summary.Total != 3 || dto.Summary.OK != 1 || dto.Summary.Failed != 2 {
		t.Fatalf("summary 不符: %+v body=%s", dto.Summary, raw)
	}
	byID := map[string]batchCommandItemDTO{}
	for _, it := range dto.Items {
		byID[it.ServerID] = it
	}
	if !byID[id1].OK {
		t.Fatalf("web-1 应成功: %+v", byID[id1])
	}
	if byID[id2].OK || byID[id2].Error == "" {
		t.Fatalf("web-2 应 ok:false + 人读 error: %+v", byID[id2])
	}
	if byID["ghost-id"].OK || byID["ghost-id"].Error == "" {
		t.Fatalf("未知 id 应 per-item 错误: %+v", byID["ghost-id"])
	}
}

// --- 历史回看:runs 列表 + 详情;不存在 404 ---

func TestBatchCommandHistory(t *testing.T) {
	dialer := &batchDialer{outByHost: map[string]*target.ExecResult{
		"10.0.0.1": {Stdout: "df output", ExitCode: 0},
	}}
	srv, client, csrf, _, _ := setupServerCommandsAPI(t, dialer)
	id := newServerAtHost(t, client, srv.URL, csrf, "web-1", "10.0.0.1")

	resp := postBatchCommand(t, client, srv.URL, csrf,
		`{"command":"df -h","serverIds":["`+id+`"]}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("执行应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var run batchCommandResponse
	_ = json.Unmarshal(raw, &run)

	// 列表。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/servers/commands/runs?limit=10", csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("runs 列表应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var list struct {
		Items []commandRunSummaryDTO `json:"items"`
	}
	_ = json.Unmarshal(raw, &list)
	if len(list.Items) != 1 || list.Items[0].Command != "df -h" || list.Items[0].Total != 1 {
		t.Fatalf("runs 列表不符: %s", raw)
	}

	// 详情:含逐机输出。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/servers/commands/runs/"+run.RunID, csrf, "")
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run 详情应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var det commandRunDetailDTO
	_ = json.Unmarshal(raw, &det)
	if det.ID != run.RunID || len(det.Items) != 1 || det.Items[0].Stdout != "df output" {
		t.Fatalf("run 详情不符: %s", raw)
	}

	// 不存在 → 404。
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/servers/commands/runs/bogus", csrf, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在 run 应 404, got %d", resp.StatusCode)
	}
}

// --- 未认证 → 401;缺 CSRF → 403 ---

func TestBatchCommandAuthAndCSRF(t *testing.T) {
	dialer := &batchDialer{}
	srv, client, csrf, _, _ := setupServerCommandsAPI(t, dialer)
	id := newServerAtHost(t, client, srv.URL, csrf, "web-1", "10.0.0.1")

	// 未认证(无 cookie)。
	resp, err := http.Post(srv.URL+"/api/servers/commands/batch", "application/json",
		strings.NewReader(`{"command":"ls","serverIds":["`+id+`"]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证应 401, got %d", resp.StatusCode)
	}

	// 已认证但缺 CSRF header → 403。
	resp2 := postBatchCommand(t, client, srv.URL, "", `{"command":"ls","serverIds":["`+id+`"]}`)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应 403, got %d", resp2.StatusCode)
	}
}

// --- 服务未注入 → 503 ---

func TestBatchCommandServiceUnavailable(t *testing.T) {
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := httptest.NewServer(New(testWebFSAuth(), svc))
	t.Cleanup(srv.Close)

	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	resp := postBatchCommand(t, client, srv.URL, csrf, `{"command":"ls","serverIds":["a"]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未注入服务应 503, got %d", resp.StatusCode)
	}
}
