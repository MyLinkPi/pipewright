package uiserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLocal 是 LocalActions 的测试桩。
type fakeLocal struct {
	info  LocalInfo
	err   error
	theme string // SetTheme 记录最近一次主题(供主题端点断言)
	// upTag/upUnstable 记录最近一次 Upgrade 的参数(供升级端点透传断言)。
	upTag      string
	upUnstable bool
}

func (f *fakeLocal) Info(context.Context) (LocalInfo, error) { return f.info, f.err }
func (f *fakeLocal) InstallService(context.Context) error    { return f.err }
func (f *fakeLocal) ServiceAction(context.Context, string) error {
	return f.err
}
func (f *fakeLocal) SetAutoStart(context.Context, bool) error { return f.err }
func (f *fakeLocal) Upgrade(_ context.Context, tag string, includeUnstable bool) error {
	f.upTag, f.upUnstable = tag, includeUnstable
	return f.err
}
func (f *fakeLocal) Quit(context.Context) error          { return f.err }
func (f *fakeLocal) OpenLog(context.Context) error       { return f.err }
func (f *fakeLocal) RevealDataDir(context.Context) error { return f.err }
func (f *fakeLocal) SetTheme(_ context.Context, theme string) error {
	f.theme = theme
	return f.err
}

func startTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := Start(context.Background(), Options{
		DataDir: t.TempDir(),
		Local:   &fakeLocal{info: LocalInfo{Platform: "linux", Version: "test", DataDir: "/tmp/pw"}},
		Token:   "test-token",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// get 发起一次请求;host 非空时覆盖 Host 头(测 DNS rebinding 防护)。
func get(t *testing.T, s *Server, path, token, host string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.URLBase()+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if host != "" {
		req.Host = host
	}
	if token != "" {
		req.Header.Set("X-PW-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestRequireHost(t *testing.T) {
	s := startTestServer(t)
	resp, _ := get(t, s, "/", "", "evil.example")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Host 不匹配应 403,得到 %d", resp.StatusCode)
	}
	resp, _ = get(t, s, "/", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合法 Host 应 200,得到 %d", resp.StatusCode)
	}
}

func TestRequireToken(t *testing.T) {
	s := startTestServer(t)
	for _, path := range []string{"/api/status", "/local/info"} {
		resp, _ := get(t, s, path, "", "")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s 缺 token 应 403,得到 %d", path, resp.StatusCode)
		}
		resp, _ = get(t, s, path, "wrong", "")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s 错误 token 应 403,得到 %d", path, resp.StatusCode)
		}
	}
}

func TestStaticFallback(t *testing.T) {
	s := startTestServer(t)
	resp, body := get(t, s, "/", "", "")
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(body) == "" {
		t.Fatalf("静态首页应 200 且非空,得到 %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("首页 Content-Type 应 text/html,得到 %q", ct)
	}
	// 未命中的深层路径回落到 index.html。
	resp, _ = get(t, s, "/some/deep/path", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("深层路径应回落 200,得到 %d", resp.StatusCode)
	}
}

func TestAPIProxyServiceDown(t *testing.T) {
	s := startTestServer(t) // DataDir 无 ipc.json
	resp, body := get(t, s, "/api/status", "test-token", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("服务未运行应 503,得到 %d", resp.StatusCode)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error == "" {
		t.Fatalf("错误信封缺失: %q", body)
	}
}

func TestLocalInfo(t *testing.T) {
	s := startTestServer(t)
	resp, body := get(t, s, "/local/info", "test-token", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/local/info 应 200,得到 %d", resp.StatusCode)
	}
	var info LocalInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Platform != "linux" || info.Version != "test" {
		t.Fatalf("LocalInfo 内容不符: %+v", info)
	}
}

func TestUIInfoFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Start(context.Background(), Options{DataDir: dir, Local: &fakeLocal{}, Token: "fixed"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	blob, err := os.ReadFile(filepath.Join(dir, "ui.json"))
	if err != nil {
		t.Fatalf("ui.json 未落盘: %v", err)
	}
	var info uiInfo
	if err := json.Unmarshal(blob, &info); err != nil {
		t.Fatalf("decode ui.json: %v", err)
	}
	if info.Token != "fixed" || info.Port != s.port {
		t.Fatalf("ui.json 内容不符: %+v", info)
	}
	if st, err := os.Stat(filepath.Join(dir, "ui.json")); err == nil && st.Mode().Perm() != 0o600 {
		t.Fatalf("ui.json 权限应 0600,得到 %v", st.Mode().Perm())
	}
}
