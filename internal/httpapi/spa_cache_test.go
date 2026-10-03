package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// spaCacheFS 模拟 web/dist:入口页 + 内容哈希资源 + 非 assets 根文件(favicon)。
var spaCacheFS = fstest.MapFS{
	"index.html":         &fstest.MapFile{Data: []byte("<html>index</html>")},
	"favicon.svg":        &fstest.MapFile{Data: []byte("<svg/>")},
	"assets/app-a1b2.js": &fstest.MapFile{Data: []byte("console.log(1)")},
}

// TestSPACacheHeaders 验证静态资源缓存策略:非哈希文件(index.html、favicon)no-cache
// 全量重拉,保证升级后立即生效;assets/ 哈希文件 immutable 一年,永不重验。
// 回退/404 语义见 router_test.go。
func TestSPACacheHeaders(t *testing.T) {
	srv := httptest.NewServer(New(spaCacheFS, nil))
	t.Cleanup(srv.Close)

	tests := []struct {
		name string
		path string
		want string
	}{
		{"根路径即入口页", "/", "no-cache"},
		{"显式入口页", "/index.html", "no-cache"},
		{"非 assets 根文件", "/favicon.svg", "no-cache"},
		{"SPA 路由回退入口页", "/runs/42", "no-cache"},
		{"assets 哈希文件", "/assets/app-a1b2.js", "public, max-age=31536000, immutable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("Cache-Control"); got != tt.want {
				t.Errorf("%s Cache-Control = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
