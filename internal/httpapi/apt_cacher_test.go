package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/aptcacherng"
	"github.com/huangchengsir/pipewright/internal/auth"
)

// setupAptCacheServer 构造带 auth + 内置 apt 缓存的测试 server。
// opts 透传 aptcacherng.Options(注入 fake 本机执行器/机器,不触网不碰真 docker)。
func setupAptCacheServer(t *testing.T, opts aptcacherng.Options) (*httptest.Server, *http.Client, string) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	hub := aptcacherng.New(st.DB, nil, opts)
	srv := httptest.NewServer(New(testWebFSAuth(), svc, WithAptCache(hub)))
	t.Cleanup(srv.Close)
	c := newTestClient(t)
	csrf := loginWithClient(t, c, srv.URL)
	return srv, c, csrf
}

func TestAptCacheGetLazyDefault(t *testing.T) {
	srv, client, _ := setupAptCacheServer(t, aptcacherng.Options{})
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/settings/apt-cache", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var dto map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &dto)
	if dto["enabled"] != false || dto["port"] != float64(3142) {
		t.Fatalf("惰性默认不符: %s", raw)
	}
	if _, ok := dto["suggestedAddr"]; !ok {
		t.Fatalf("suggestedAddr 字段应存在: %s", raw)
	}
	if dto["updatedAt"] != nil {
		t.Fatalf("默认 updatedAt 应 null: %s", raw)
	}
}

func TestAptCachePutSaveAndValidate(t *testing.T) {
	srv, client, csrf := setupAptCacheServer(t, aptcacherng.Options{})

	// 启用但无地址 → 422。
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/apt-cache", csrf, `{"enabled":true}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("缺地址应 422, got %d", resp.StatusCode)
	}

	// 地址带 scheme → 422。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/apt-cache", csrf,
		`{"enabled":true,"externalAddr":"http://bad"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("非法地址应 422, got %d", resp.StatusCode)
	}

	// 合法保存 → 200,端口兜底。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/apt-cache", csrf,
		`{"enabled":true,"externalAddr":"192.168.1.10"}`)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("保存应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var dto map[string]any
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	_ = json.Unmarshal(raw, &dto)
	if dto["enabled"] != true || dto["addr"] != "192.168.1.10:3142" || dto["tlsEnabled"] != false {
		t.Fatalf("保存结果不符: %s", raw)
	}
}

func TestAptCachePutRequiresCSRF(t *testing.T) {
	srv, client, _ := setupAptCacheServer(t, aptcacherng.Options{})
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/apt-cache", "", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("无 CSRF 应 403, got %d", resp.StatusCode)
	}
}

// aptFakeMachine 是 apt 代理下发成功路径的 fake 本机目标:apt-get 可用 + apt-config 指向本机。
type aptFakeMachine struct {
	addr    string // apt-config dump 指向的地址(空 = 用上传内容里的地址)
	uploads map[string]string
}

func (m *aptFakeMachine) Exec(_ context.Context, cmd []string) (string, string, int, error) {
	j := strings.Join(cmd, " ")
	switch {
	case strings.Contains(j, "command -v apt-get"):
		return "/usr/bin/apt-get", "", 0, nil
	case strings.Contains(j, "apt-config dump"):
		return "Acquire\n{\n http\n {\n  Proxy \"http://" + m.addr + "/\";\n };\n};", "", 0, nil
	case strings.Contains(j, "Acquire::http::Proxy/f"):
		return "http://" + m.addr + "/", "", 0, nil
	case strings.Contains(j, "Acquire::https::Proxy/f"):
		return "http://" + m.addr + "/", "", 0, nil
	default:
		return "", "", 0, nil
	}
}

func (m *aptFakeMachine) Upload(_ context.Context, content io.Reader, remotePath string) error {
	b, _ := io.ReadAll(content)
	if m.uploads == nil {
		m.uploads = map[string]string{}
	}
	m.uploads[remotePath] = string(b)
	return nil
}

func TestAptCacheProxyApplyExplicitTargetsOnly(t *testing.T) {
	machine := &aptFakeMachine{addr: "192.168.1.10:3142"}
	srv, client, csrf := setupAptCacheServer(t, aptcacherng.Options{BaseDir: t.TempDir(), LocalMachine: machine})
	// 先启用并保存(片段内容依赖缓存地址)。
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/apt-cache", csrf,
		`{"enabled":true,"externalAddr":"192.168.1.10"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存配置失败")
	}

	// 显式勾选:1 台不存在的服务器 + 控制机本机;结果一一对应,不扩大范围。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/apt-cache/proxy-apply", csrf,
		`{"serverIds":["srv-404"],"includeLocal":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("apply 应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Results []aptcacherng.ProxyApplyResult `json:"results"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	if len(out.Results) != 2 {
		t.Fatalf("结果应与请求目标一一对应(2), got %d: %s", len(out.Results), raw)
	}
	okLocal, failRemote := false, false
	for _, r := range out.Results {
		if r.IsLocal && r.OK {
			okLocal = true
		}
		if r.TargetID == "srv-404" && !r.OK && r.Error != "" {
			failRemote = true
		}
	}
	if !okLocal || !failRemote {
		t.Fatalf("本机应成功、不存在服务器应失败: %s", raw)
	}
	// 上传内容为生成的代理片段(含指向本缓存的 http/https 代理)。
	up, ok := machine.uploads["/etc/apt/apt.conf.d/95pipewright-proxy.tmp.pipewright"]
	if !ok || !strings.Contains(up, `Acquire::http::Proxy "http://192.168.1.10:3142";`) ||
		!strings.Contains(up, `Acquire::https::Proxy "http://192.168.1.10:3142";`) {
		t.Fatalf("上传内容不符: %q", up)
	}
}

func TestAptCacheProxyApplyNoTargets(t *testing.T) {
	srv, client, csrf := setupAptCacheServer(t, aptcacherng.Options{})
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/apt-cache/proxy-apply", csrf,
		`{"serverIds":[],"includeLocal":false}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空目标应 400, got %d", resp.StatusCode)
	}
}

func TestAptCacheProxyApplyDisabled(t *testing.T) {
	srv, client, csrf := setupAptCacheServer(t, aptcacherng.Options{LocalMachine: &aptFakeMachine{}})
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/apt-cache/proxy-apply", csrf,
		`{"includeLocal":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("未启用应 422, got %d", resp.StatusCode)
	}
}

func TestAptCacheInspectLocal(t *testing.T) {
	machine := &aptFakeMachine{addr: "192.168.1.10:3142"}
	srv, client, csrf := setupAptCacheServer(t, aptcacherng.Options{LocalMachine: machine})
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/apt-cache/inspect?serverId=local", csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var ins map[string]any
	_ = json.Unmarshal(raw, &ins)
	if ins["isLocal"] != true {
		t.Fatalf("应为本机巡检: %s", raw)
	}
	if !strings.Contains(string(raw), "192.168.1.10:3142") {
		t.Fatalf("代理值应回显: %s", raw)
	}
}

func TestAptCacheInspectMissingParam(t *testing.T) {
	srv, client, csrf := setupAptCacheServer(t, aptcacherng.Options{})
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/apt-cache/inspect", csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 serverId 应 400, got %d", resp.StatusCode)
	}
}

// aptStubRunner 恒放行 compose 探测/up/image inspect/pull(适配 deploy/status/clear/upgrade)。
type aptStubRunner struct {
	upRan    bool
	inspects int
	before   string
	after    string
}

func (r *aptStubRunner) Run(_ context.Context, name string, args []string, _ string) (string, string, int, error) {
	j := name + " " + strings.Join(args, " ")
	switch {
	case strings.Contains(j, "compose version"):
		return "v2", "", 0, nil
	case strings.Contains(j, " up -d"):
		r.upRan = true
		return "started", "", 0, nil
	case strings.Contains(j, "image inspect"):
		r.inspects++
		if r.inspects >= 2 {
			return r.after, "", 0, nil
		}
		return r.before, "", 0, nil
	case strings.Contains(j, "pull"):
		return "", "pulled", 0, nil
	default:
		return "", "", 0, nil
	}
}

func TestAptCacheDeployStatusClear(t *testing.T) {
	dir := t.TempDir()
	srv, client, csrf := setupAptCacheServer(t, aptcacherng.Options{BaseDir: dir, Runner: &aptStubRunner{before: "sha256:same", after: "same"}})

	// 未启用先部署 → 422。
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/settings/apt-cache/deploy", csrf, `{}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("未启用部署应 422, got %d", resp.StatusCode)
	}

	// 启用 → 部署 → 状态 → 清空缓存。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/apt-cache", csrf,
		`{"enabled":true,"externalAddr":"192.168.1.10"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存失败")
	}

	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/settings/apt-cache/deploy", csrf, `{}`)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("部署应 200, got %d body=%s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("部署应 ok: %s", raw)
	}

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/settings/apt-cache/status", csrf, "")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"deployed":true`) {
		t.Fatalf("状态应已部署: %s", raw)
	}

	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/settings/apt-cache/clear-cache", csrf, `{}`)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("clear-cache = %d body=%s", resp.StatusCode, raw)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("清空应 ok: %s", raw)
	}
}

func TestAptCacheUpgradeImage(t *testing.T) {
	runner := &aptStubRunner{before: "sha256:oldoldold", after: "sha256:newnewnew"}
	srv, client, csrf := setupAptCacheServer(t, aptcacherng.Options{BaseDir: t.TempDir(), Runner: runner})

	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/settings/apt-cache/upgrade-image", csrf, `{}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("未启用升级应 422, got %d", resp.StatusCode)
	}

	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/apt-cache", csrf,
		`{"enabled":true,"externalAddr":"192.168.1.10"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存失败")
	}

	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/settings/apt-cache/upgrade-image", csrf, `{}`)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("升级应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var out aptcacherng.ImageUpgradeResult
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if !out.OK || !out.Changed || out.Image != "sameersbn/apt-cacher-ng" {
		t.Fatalf("镜像变了应 ok+changed: %s", raw)
	}
	if out.Before != "oldoldold" || out.After != "newnewnew" {
		t.Fatalf("前后镜像 ID 应截短回显: %s", raw)
	}
	if !runner.upRan {
		t.Fatalf("镜像变了应 compose up 重建")
	}
}

// aptFakeCerts 是 aptcacherng.CertSource 的 httpapi 测试 fake(cert-1 覆盖 *.mylinkpi.cn)。
type aptFakeCerts struct{}

func (aptFakeCerts) GetCert(_ context.Context, id string) (*aptcacherng.CertInfo, error) {
	if id == "cert-1" {
		return &aptcacherng.CertInfo{ID: id, PrimaryDomain: "mylinkpi.cn",
			Domains: []string{"mylinkpi.cn", "*.mylinkpi.cn"}}, nil
	}
	return nil, errors.New("not found")
}

func (aptFakeCerts) OpenCertPEM(_ context.Context, _ string) (string, string, error) {
	return "---CERT---", "---KEY---", nil
}

func TestAptCachePutTLSCert(t *testing.T) {
	srv, client, csrf := setupAptCacheServer(t, aptcacherng.Options{Certs: aptFakeCerts{}})

	// 证书不存在 → 422 invalid_apt_tls。
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/apt-cache", csrf,
		`{"enabled":true,"externalAddr":"apt.mylinkpi.cn","tlsCertId":"cert-404"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("证书不存在应 422, got %d body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "invalid_apt_tls") {
		t.Fatalf("错误码应为 invalid_apt_tls: %s", raw)
	}

	// 域名覆盖 → 200,DTO 回显 tlsCertId + tlsEnabled。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/apt-cache", csrf,
		`{"enabled":true,"externalAddr":"apt.mylinkpi.cn","tlsCertId":"cert-1"}`)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合法 TLS 证书应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var dto map[string]any
	_ = json.Unmarshal(raw, &dto)
	if dto["tlsCertId"] != "cert-1" || dto["tlsEnabled"] != true {
		t.Fatalf("选证书后应回显 tlsCertId 且 tlsEnabled=true: %s", raw)
	}
}

func TestAptCacheNilService503(t *testing.T) {
	// 未注入 apt 服务(WithAptCache 不调用)→ 各端点 503。
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := httptest.NewServer(New(testWebFSAuth(), svc))
	t.Cleanup(srv.Close)
	c := newTestClient(t)
	csrf := loginWithClient(t, c, srv.URL)
	resp := doJSON(t, c, http.MethodGet, srv.URL+"/api/settings/apt-cache", csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503, got %d", resp.StatusCode)
	}
}
