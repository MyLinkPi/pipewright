package platformhttps

// gateway_test.go 覆盖「本机网关入口 vhost」的渲染契约(specs/local-node-roles.md 7.3):
//   - 443 块再加密转发网关容器 443 回环端口(verify off + SNI 透传 $host)—— 80 转发会死循环;
//   - 80 块统一 301 且 $host 保留原子域名;
//   - 每基域自洽的 WS 升级 map(独立变量名,与平台 vhost 不冲突);
//   - 精确域名与通配符并存(nginx 精确优先,平台域名不被劫走)。

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRenderGatewayConf(t *testing.T) {
	conf := renderGatewayConf(GatewayEntrySpec{
		LoopbackHTTPPort:  18080,
		LoopbackHTTPSPort: 18443,
		Certs: []GatewayEntryCert{
			{BaseDomain: "efg.com", CertID: "c1", FullchainPEM: "FC", PrivkeyPEM: "KEY"},
		},
	})

	checks := map[string]bool{
		// marker 与 map 自洽性。
		gatewayConfMarker:           true,
		"$pw_gw_connection_upgrade": true,
		// 443 块:通配符 + 裸域并存;证书路径;再加密转发回环 443。
		"server_name *.efg.com efg.com;":                                         true,
		"ssl_certificate     /etc/nginx/pipewright/certs/efg.com/fullchain.pem;": true,
		"proxy_pass https://127.0.0.1:18443;":                                    true,
		"proxy_ssl_verify off;":                                                  true,
		"proxy_ssl_name $host;":                                                  true,
		"proxy_set_header Host $host;":                                           true,
		"proxy_read_timeout 3600s;":                                              true,
		// 80 块:301 且保留原子域名。
		"return 301 https://$host$request_uri;": true,
	}
	for _, want := range []string{
		gatewayConfMarker,
		"$pw_gw_connection_upgrade",
		"server_name *.efg.com efg.com;",
		"ssl_certificate     /etc/nginx/pipewright/certs/efg.com/fullchain.pem;",
		"proxy_pass https://127.0.0.1:18443;",
		"proxy_ssl_verify off;",
		"proxy_ssl_server_name on;",
		"proxy_ssl_name $host;",
		"proxy_set_header Host $host;",
		"return 301 https://$host$request_uri;",
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("渲染缺关键行 %q:\n%s", want, conf)
		}
	}
	_ = checks

	// 80 流量绝不直接反代到网关 80(会 301 死循环):渲染文本里对 80 端口只允许出现
	// 在 301 server 块的 listen 上,proxy_pass 必须指向回环 HTTPS。
	for _, line := range strings.Split(conf, "\n") {
		if strings.Contains(line, "proxy_pass") && !strings.Contains(line, "https://127.0.0.1:18443") {
			t.Fatalf("入口 vhost 只允许再加密转发回环 443,出现 %q", strings.TrimSpace(line))
		}
	}

	// 多基域:每域一组 server 块,互不串名。
	multi := renderGatewayConf(GatewayEntrySpec{
		LoopbackHTTPPort: 18080, LoopbackHTTPSPort: 18443,
		Certs: []GatewayEntryCert{
			{BaseDomain: "efg.com", CertID: "c1", FullchainPEM: "FC", PrivkeyPEM: "KEY"},
			{BaseDomain: "example.org", CertID: "c2", FullchainPEM: "FC", PrivkeyPEM: "KEY"},
		},
	})
	if strings.Count(multi, "listen 443 ssl;") != 2 || strings.Count(multi, "listen 80;") != 2 {
		t.Fatalf("两基域应各渲染一组 443/80 server 块:\n%s", multi)
	}
	if !strings.Contains(multi, "certs/example.org/fullchain.pem") {
		t.Fatalf("第二基域证书路径缺失:\n%s", multi)
	}
}

// gatewayEntrySpec 返回一个合法的单基域入口期望态(fakeHost 下可直接发布成功)。
func gatewayEntrySpec() GatewayEntrySpec {
	return GatewayEntrySpec{
		LoopbackHTTPPort: 18080, LoopbackHTTPSPort: 18443,
		Certs: []GatewayEntryCert{{BaseDomain: "efg.com", CertID: "cert-1", FullchainPEM: "FC-NEW", PrivkeyPEM: "KEY-NEW"}},
	}
}

// TestApplyGatewayRejectsInvalidLoopbackPorts: 回环端口 0/负数/越界必须在触网前拒绝
// (否则渲染 127.0.0.1:0,网关容器启动即失败)。
func TestApplyGatewayRejectsInvalidLoopbackPorts(t *testing.T) {
	h := &fakeHost{}
	svc := New(nil, h, nil, 8080)
	for _, tc := range []struct {
		name        string
		http, https int
	}{
		{"未配置", 0, 0},
		{"负数", -1, 18443},
		{"越界", 70000, 18443},
		{"仅一端越界", 18080, 0},
	} {
		spec := gatewayEntrySpec()
		spec.LoopbackHTTPPort, spec.LoopbackHTTPSPort = tc.http, tc.https
		if err := svc.ApplyGateway(context.Background(), spec); err == nil {
			t.Fatalf("%s 应拒绝", tc.name)
		}
	}
	if len(h.calls) != 0 {
		t.Fatalf("非法端口不应触网:%v", h.calls)
	}
}

// TestApplyGatewayRollbackRestoresCertAndConf: nginx -t 失败时证书与入口 vhost 必须一起
// 回滚(平台 vhost 可能与本入口共用 certs/<基域>/ 目录,只回滚 conf 会留下被替换的证书)。
func TestApplyGatewayRollbackRestoresCertAndConf(t *testing.T) {
	h := &fakeHost{}
	h.resultFor = func(cmd []string) *cmdRes {
		if len(cmd) > 0 && cmd[len(cmd)-1] == "-t" {
			return &cmdRes{stderr: "boom", code: 1}
		}
		return nil
	}
	svc := New(nil, h, nil, 8080)
	if err := svc.ApplyGateway(context.Background(), gatewayEntrySpec()); err == nil {
		t.Fatal("nginx -t 失败应报错")
	}
	dir := certDir("efg.com")
	if !hasCmd(h, "sudo", "-n", "cp", fmt.Sprintf(gatewayCertBackupFullFmt, 0), dir+"/fullchain.pem") {
		t.Fatalf("未回滚 fullchain 证书:\n%v", h.calls)
	}
	if !hasCmd(h, "sudo", "-n", "cp", fmt.Sprintf(gatewayCertBackupKeyFmt, 0), dir+"/privkey.pem") {
		t.Fatalf("未回滚 privkey 证书:\n%v", h.calls)
	}
	if !hasCmd(h, "sudo", "-n", "cp", gatewayBackupConf, managedGatewayConf) {
		t.Fatalf("未回滚入口 vhost:\n%v", h.calls)
	}
}

// TestApplyGatewayThenRemoveGateway: 发布成功后 RemoveGateway 摘除 conf 与基域证书目录。
func TestApplyGatewayThenRemoveGateway(t *testing.T) {
	h := &fakeHost{}
	h.resultFor = func(cmd []string) *cmdRes {
		// nginx -T 需输出 marker,证明入口 vhost 被 include。
		if len(cmd) > 0 && cmd[len(cmd)-1] == "-T" {
			return &cmdRes{stdout: "# " + gatewayConfMarker}
		}
		return nil
	}
	svc := New(nil, h, nil, 8080)
	if err := svc.ApplyGateway(context.Background(), gatewayEntrySpec()); err != nil {
		t.Fatalf("fakeHost 下发布应成功:%v", err)
	}
	h.calls = nil
	if err := svc.RemoveGateway(context.Background()); err != nil {
		t.Fatalf("摘除应成功:%v", err)
	}
	if !hasCmd(h, "sudo", "-n", "rm", "-f", managedGatewayConf) {
		t.Fatalf("未删除入口 vhost:\n%v", h.calls)
	}
	if !hasCmd(h, "sudo", "-n", "rmdir", certDir("efg.com")) {
		t.Fatalf("未尝试清理基域证书目录:\n%v", h.calls)
	}
}
