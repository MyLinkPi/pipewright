package uiserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// postJSON 向测试服务发一次带令牌的 POST,返回状态码与响应体。
func postJSON(t *testing.T, s *Server, path, token string, body any) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = strings.NewReader(string(blob))
	}
	req, err := http.NewRequest(http.MethodPost, s.URLBase()+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-PW-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestThemePersistenceAndInjection 覆盖主题三态:默认深色 → POST 浅色落盘 → 首页注入浅色。
// 这正是「切换后重启不保存」回归的防线:主题落后端文件而非随机端口下的 localStorage。
func TestThemePersistenceAndInjection(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeLocal{}
	s, err := Start(context.Background(), Options{DataDir: dir, Local: fake, Token: "tk"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// 1. 默认深色,且首页注入深色。
	if got := s.LoadTheme(); got != "dark" {
		t.Fatalf("默认主题应为 dark,得到 %q", got)
	}
	_, body := get(t, s, "/", "", "")
	if !strings.Contains(body, `data-theme="dark"`) {
		t.Fatalf("首页应注入 data-theme=\"dark\",得到:\n%s", body)
	}

	// 2. POST 浅色:落盘 + 交给宿主 SetTheme + 后续首页注入浅色。
	code, _ := postJSON(t, s, "/local/theme", "tk", map[string]string{"theme": "light"})
	if code != http.StatusOK {
		t.Fatalf("POST /local/theme 应 200,得到 %d", code)
	}
	if got := s.LoadTheme(); got != "light" {
		t.Fatalf("主题应持久化为 light,得到 %q", got)
	}
	if fake.theme != "light" {
		t.Fatalf("应调用宿主 SetTheme(light),得到 %q", fake.theme)
	}
	_, body = get(t, s, "/", "", "")
	if !strings.Contains(body, `data-theme="light"`) {
		t.Fatalf("首页应注入 data-theme=\"light\",得到:\n%s", body)
	}

	// 3. 非法主题拒绝。
	code, _ = postJSON(t, s, "/local/theme", "tk", map[string]string{"theme": "neon"})
	if code != http.StatusBadRequest {
		t.Fatalf("非法主题应 400,得到 %d", code)
	}

	// 4. 缺 token 拒绝。
	code, _ = postJSON(t, s, "/local/theme", "", map[string]string{"theme": "dark"})
	if code != http.StatusForbidden {
		t.Fatalf("缺 token 应 403,得到 %d", code)
	}
}

// TestInjectThemeIsSafeNoop 验证主题缺失/异常时注入回退深色且不破坏 HTML。
func TestInjectThemeIsSafeNoop(t *testing.T) {
	html := []byte(`<html lang="zh-CN" data-theme="dark"><body>x</body></html>`)
	if got := string(injectTheme(html, "light")); !strings.Contains(got, `data-theme="light"`) {
		t.Fatalf("应替换为 light,得到 %s", got)
	}
	if got := string(injectTheme(html, "garbage")); !strings.Contains(got, `data-theme="dark"`) {
		t.Fatalf("非法值应回退 dark,得到 %s", got)
	}
}

// TestPrefsRoundTrip 覆盖升级口径开关的持久化:默认不含预发布 → POST 开启后落盘,
// 且与主题互不覆盖(读-改-写整份偏好)。
func TestPrefsRoundTrip(t *testing.T) {
	s, err := Start(context.Background(), Options{DataDir: t.TempDir(), Local: &fakeLocal{}, Token: "tk"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// 默认:深色 + 不含预发布。
	_, body := get(t, s, "/local/prefs", "tk", "")
	if !strings.Contains(body, `"theme":"dark"`) || !strings.Contains(body, `"includeUnstable":false`) {
		t.Fatalf("默认偏好应为 dark + 不含预发布,得到 %s", body)
	}

	// 先切浅色,再经 /local/prefs 开启预发布:两个字段须同时存活。
	if code, _ := postJSON(t, s, "/local/theme", "tk", map[string]string{"theme": "light"}); code != http.StatusOK {
		t.Fatalf("POST /local/theme 应 200,得到 %d", code)
	}
	if code, _ := postJSON(t, s, "/local/prefs", "tk", UIPrefs{Theme: "light", IncludeUnstable: true}); code != http.StatusOK {
		t.Fatalf("POST /local/prefs 应 200,得到 %d", code)
	}
	if p := s.Prefs(); !p.IncludeUnstable || p.Theme != "light" {
		t.Fatalf("偏好应同时保留主题与升级口径,得到 %+v", p)
	}

	// 非法主题拒绝。
	if code, _ := postJSON(t, s, "/local/prefs", "tk", UIPrefs{Theme: "neon"}); code != http.StatusBadRequest {
		t.Fatalf("非法主题应 400,得到 %d", code)
	}
}

// TestLocalUpgradePassthroughUnstable 验证 /local/upgrade 把 includeUnstable 透传给宿主,
// 非法 tag 仍被白名单拦截。
func TestLocalUpgradePassthroughUnstable(t *testing.T) {
	fake := &fakeLocal{}
	s, err := Start(context.Background(), Options{DataDir: t.TempDir(), Local: fake, Token: "tk"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	code, _ := postJSON(t, s, "/local/upgrade", "tk", map[string]any{"tag": "v1.2.3-beta.1", "includeUnstable": true})
	if code != http.StatusAccepted {
		t.Fatalf("预发布 tag 应 202,得到 %d", code)
	}
	if fake.upTag != "v1.2.3-beta.1" || !fake.upUnstable {
		t.Fatalf("应透传 tag 与 includeUnstable,得到 %q %v", fake.upTag, fake.upUnstable)
	}

	if code, _ := postJSON(t, s, "/local/upgrade", "tk", map[string]any{"tag": "v1.2.3 --force"}); code != http.StatusBadRequest {
		t.Fatalf("非法 tag 应 400,得到 %d", code)
	}
}
