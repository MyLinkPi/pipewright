package platformhttps

import (
	"strings"
	"testing"
)

func TestRenderPlatformConfRedirectOn(t *testing.T) {
	conf := renderPlatformConf("pip.efg.com", "127.0.0.1", 8080, true)
	for _, want := range []string{
		"# " + confMarker,
		"map $http_upgrade $connection_upgrade",
		"server_name pip.efg.com",
		"listen 80;",
		"return 301 https://$host$request_uri;",
		"listen 443 ssl;",
		"ssl_certificate     /etc/nginx/pipewright/certs/pip.efg.com/fullchain.pem;",
		"ssl_certificate_key /etc/nginx/pipewright/certs/pip.efg.com/privkey.pem;",
		"ssl_protocols TLSv1.2 TLSv1.3;",
		"client_max_body_size 512m;",
		"proxy_pass http://127.0.0.1:8080;",
		"proxy_set_header X-Forwarded-Proto https;",
		"proxy_set_header Upgrade $http_upgrade;",
		"proxy_read_timeout 3600s;",
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("配置应包含 %q:\n%s", want, conf)
		}
	}
	// 跳转开时 80 块不应反代(X-Forwarded-Proto https 只出现一次)。
	if n := strings.Count(conf, "proxy_pass"); n != 1 {
		t.Fatalf("跳转开启时应只有 443 一处反代,得 %d:\n%s", n, conf)
	}
}

func TestRenderPlatformConfRedirectOff(t *testing.T) {
	conf := renderPlatformConf("pip.efg.com", "10.0.0.8", 3000, false)
	if strings.Contains(conf, "return 301") {
		t.Fatalf("跳转关闭时不应有 301:\n%s", conf)
	}
	if n := strings.Count(conf, "proxy_pass http://10.0.0.8:3000;"); n != 2 {
		t.Fatalf("80/443 应各有一处反代,得 %d:\n%s", n, conf)
	}
	// 80 块声明 http、443 块声明 https。
	if !strings.Contains(conf, "proxy_set_header X-Forwarded-Proto http;") ||
		!strings.Contains(conf, "proxy_set_header X-Forwarded-Proto https;") {
		t.Fatalf("两块应分别声明 X-Forwarded-Proto http/https:\n%s", conf)
	}
}

func TestRenderPlatformConfDeterministic(t *testing.T) {
	a := renderPlatformConf("a.efg.com", "127.0.0.1", 8080, true)
	b := renderPlatformConf("a.efg.com", "127.0.0.1", 8080, true)
	if a != b {
		t.Fatalf("渲染应确定性输出")
	}
}
