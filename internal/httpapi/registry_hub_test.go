package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/registryhub"
)

// setupRegistryHubServer 构造带 auth + 内置 registry 的测试 server。
// opts 透传 registryhub.Options(注入 fake 本机执行器/registry API,不触网不碰真 docker)。
func setupRegistryHubServer(t *testing.T, opts registryhub.Options) (*httptest.Server, *http.Client, string) {
	t.Helper()
	st := testStoreAuth(t)
	svc := auth.NewService(st.DB, nil)
	if err := svc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	hub := registryhub.New(st.DB, nil, opts)
	srv := httptest.NewServer(New(testWebFSAuth(), svc, WithRegistryHub(hub)))
	t.Cleanup(srv.Close)
	c := newTestClient(t)
	csrf := loginWithClient(t, c, srv.URL)
	return srv, c, csrf
}

func TestRegistryHubGetLazyDefault(t *testing.T) {
	srv, client, csrf := setupRegistryHubServer(t, registryhub.Options{})
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/settings/registry", csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var dto map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &dto)
	if dto["enabled"] != false || dto["upstreamUrl"] != registryhub.DefaultUpstreamURL {
		t.Fatalf("惰性默认不符: %s", raw)
	}
	if _, ok := dto["suggestedAddr"]; !ok {
		t.Fatalf("suggestedAddr 字段应存在: %s", raw)
	}
	if dto["updatedAt"] != nil {
		t.Fatalf("默认 updatedAt 应 null: %s", raw)
	}
}

func TestRegistryHubPutSaveAndValidate(t *testing.T) {
	srv, client, csrf := setupRegistryHubServer(t, registryhub.Options{})

	// 启用但无地址 → 422。
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/registry", csrf,
		`{"enabled":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("缺地址应 422, got %d", resp.StatusCode)
	}

	// 合法保存 → 200,地址/端口兜底生效。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/registry", csrf,
		`{"enabled":true,"externalAddr":"192.168.1.10","keepPerProject":10,"maxAgeDays":30}`)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("保存应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var dto map[string]any
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	_ = json.Unmarshal(raw, &dto)
	if dto["enabled"] != true || dto["artifactAddr"] != "192.168.1.10:5000" || dto["cacheAddr"] != "192.168.1.10:5001" {
		t.Fatalf("保存结果不符: %s", raw)
	}
}

func TestRegistryHubPutRequiresCSRF(t *testing.T) {
	srv, client, _ := setupRegistryHubServer(t, registryhub.Options{})
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/registry", "", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("无 CSRF 应 403, got %d", resp.StatusCode)
	}
}

// applyLocalMachine 是 daemon-apply 成功路径的 fake 本机目标。
type applyLocalMachine struct{ uploads map[string]string }

func (m *applyLocalMachine) Exec(_ context.Context, cmd []string) (string, string, int, error) {
	joined := strings.Join(cmd, " ")
	switch {
	case strings.Contains(joined, "command -v dockerd"):
		return "/usr/bin/docker", "", 0, nil
	case strings.HasPrefix(joined, "chmod"), strings.Contains(joined, "dockerd --validate"),
		strings.Contains(joined, "test -f"), strings.Contains(joined, "mv -f"):
		return "", "", 0, nil
	case strings.Contains(joined, "systemctl restart docker"):
		return "", "", 0, nil
	case strings.Contains(joined, "docker info --format"):
		return `["http://192.168.1.10:5001"]`, "", 0, nil
	default:
		return "", "", 0, nil
	}
}

func (m *applyLocalMachine) Upload(_ context.Context, content io.Reader, remotePath string) error {
	b, _ := io.ReadAll(content)
	if m.uploads == nil {
		m.uploads = map[string]string{}
	}
	m.uploads[remotePath] = string(b)
	return nil
}

func TestRegistryHubDaemonApplyExplicitTargetsOnly(t *testing.T) {
	machine := &applyLocalMachine{}
	srv, client, csrf := setupRegistryHubServer(t, registryhub.Options{LocalMachine: machine})
	// 先保存启用配置(镜像源内容依赖 registry 地址)。
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/registry", csrf,
		`{"enabled":true,"externalAddr":"192.168.1.10"}`)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("保存配置失败")
	}
	resp.Body.Close()

	// 显式勾选:1 台不存在的服务器 + 控制机本机;结果与请求一一对应,不扩大范围。
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/registry/daemon-apply", csrf,
		`{"serverIds":["srv-404"],"includeLocal":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("apply 应 200, got %d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Results []registryhub.DaemonApplyResult `json:"results"`
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
	// 上传内容为生成的 daemon.json(含镜像源与 insecure-registries)。
	uploaded, ok := machine.uploads["/etc/docker/daemon.json.tmp.pipewright"]
	if !ok || !strings.Contains(uploaded, "http://192.168.1.10:5001") || !strings.Contains(uploaded, "192.168.1.10:5000") {
		t.Fatalf("上传内容不符: %q", uploaded)
	}
}

func TestRegistryHubDaemonApplyNoTargets(t *testing.T) {
	srv, client, csrf := setupRegistryHubServer(t, registryhub.Options{})
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/registry/daemon-apply", csrf,
		`{"serverIds":[],"includeLocal":false}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空目标应 400, got %d", resp.StatusCode)
	}
}

func TestRegistryHubDaemonApplyDisabled(t *testing.T) {
	srv, client, csrf := setupRegistryHubServer(t, registryhub.Options{LocalMachine: &applyLocalMachine{}})
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/registry/daemon-apply", csrf,
		`{"includeLocal":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("未启用应 422, got %d", resp.StatusCode)
	}
}

func TestRegistryHubInspectLocal(t *testing.T) {
	machine := &applyLocalMachine{}
	srv, client, csrf := setupRegistryHubServer(t, registryhub.Options{LocalMachine: machine})
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/registry/inspect?serverId=local", csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var ins map[string]any
	_ = json.Unmarshal(raw, &ins)
	if ins["isLocal"] != true {
		t.Fatalf("应为本机: %s", raw)
	}
	if !strings.Contains(string(raw), "192.168.1.10:5001") {
		t.Fatalf("镜像源应回显: %s", raw)
	}
}

func TestRegistryHubDeployStatusPrune(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeComposeRunner{baseDir: dir}
	api := &fakeRegistryAPIHub{repos: map[string][]registryhub.TagInfo{
		"acme": {
			{Tag: "a", Digest: "d1"},
			{Tag: "b", Digest: "d2"},
		},
	}}
	srv, client, csrf := setupRegistryHubServer(t, registryhub.Options{
		BaseDir: dir, Runner: runner, RegistryClient: api,
		LocalMachine: &applyLocalMachine{},
	})
	// 未启用先部署 → 422。
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/settings/registry/deploy", csrf, `{}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		resp.Body.Close()
		t.Fatalf("未启用部署应 422, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 启用 + 保留策略 → 部署 → 状态 → 清理。
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/api/settings/registry", csrf,
		`{"enabled":true,"externalAddr":"192.168.1.10","keepPerProject":1}`)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("保存失败")
	}
	resp.Body.Close()

	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/settings/registry/deploy", csrf, `{}`)
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

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/api/settings/registry/status", csrf, "")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"deployed":true`) {
		t.Fatalf("状态应已部署: %s", raw)
	}

	resp = doJSON(t, client, http.MethodPost, srv.URL+"/api/settings/registry/prune", csrf, `{}`)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("prune = %d", resp.StatusCode)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"deletedTags":1`) {
		t.Fatalf("应删 1 个 tag(keep=1): %s", raw)
	}
}

// fakeComposeRunner 覆盖 compose 探测/up/inspect/gc 的最小 LocalRunner。
type fakeComposeRunner struct {
	baseDir string
	upRan   bool
	gcRan   bool
}

func (r *fakeComposeRunner) Run(_ context.Context, name string, args []string, _ string) (string, string, int, error) {
	joined := name + " " + strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "compose version"):
		return "v2", "", 0, nil
	case strings.Contains(joined, "up -d"):
		r.upRan = true
		return "started", "", 0, nil
	case strings.Contains(joined, "inspect -f"):
		return "running", "", 0, nil
	case strings.Contains(joined, "garbage-collect"):
		r.gcRan = true
		return "", "", 0, nil
	default:
		return "", "", 0, nil
	}
}

// fakeRegistryAPIHub 是 httpapi 测试用的 registry API fake(排序无关,TagInfo 无时间 → 全零值)。
type fakeRegistryAPIHub struct {
	repos   map[string][]registryhub.TagInfo
	deleted int
}

func (f *fakeRegistryAPIHub) Catalog(context.Context) ([]string, error) {
	repos := make([]string, 0, len(f.repos))
	for r := range f.repos {
		repos = append(repos, r)
	}
	return repos, nil
}

func (f *fakeRegistryAPIHub) TagInfos(_ context.Context, repo string) ([]registryhub.TagInfo, error) {
	return f.repos[repo], nil
}

func (f *fakeRegistryAPIHub) DeleteManifest(_ context.Context, _, _ string) error {
	f.deleted++
	return nil
}
