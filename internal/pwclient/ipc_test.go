// ipc_test.go 覆盖 IPC 通道:token 鉴权、配置读写(密钥绝不回显)、导入、状态快照。
// 使用真实 loopback 监听(StartIPC 绑 127.0.0.1:0),下载器置 nil 防止触网。
package pwclient

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestRuntime(t *testing.T) (*Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	rt := NewRuntime(&Store{Path: filepath.Join(dir, "config.yaml")}, dir)
	rt.newDownloader = nil
	return rt, dir
}

func TestIPCTokenAndEndpoints(t *testing.T) {
	rt, dir := newTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := StartIPC(ctx, dir, rt); err != nil {
		t.Fatalf("start ipc: %v", err)
	}

	// 错误 token → 403;无 token → 403。
	cli, err := DialIPC(dir)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	bad := &IPCClient{Base: cli.Base, Token: "wrong-token", HTTP: cli.HTTP}
	if _, err := bad.Status(ctx); err == nil || !strings.Contains(err.Error(), "鉴权失败") {
		t.Fatalf("wrong token should fail: %v", err)
	}
	noTok := &IPCClient{Base: cli.Base, Token: "", HTTP: cli.HTTP}
	if _, err := noTok.Status(ctx); err == nil {
		t.Fatal("missing token should fail")
	}

	// 初始状态:未配置。
	snap, err := cli.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if snap.Configured || snap.Enabled {
		t.Fatalf("fresh state should be unconfigured: %+v", snap)
	}

	// 写配置(enabled + routes);GET 回显密钥只报存在性。
	enabled := true
	routes := []string{"192.168.30.0/24"}
	secret := "topsecret"
	view, err := cli.PutConfig(ctx, ConfigSetRequest{
		Enabled:       &enabled,
		NetworkName:   strPtr("prod-net"),
		NetworkSecret: &secret,
		Peers:         &[]string{"tcp://1.2.3.4:11010"},
		Routes:        &routes,
	})
	if err != nil {
		t.Fatalf("put config: %v", err)
	}
	if !view.Enabled || view.Join.NetworkName != "prod-net" || !view.Join.HasSecret {
		t.Fatalf("view mismatch: %+v", view)
	}
	if len(view.Routes) != 1 || view.Routes[0] != "192.168.30.0/24" {
		t.Fatalf("routes view: %v", view.Routes)
	}
	// 回显体里绝无密钥明文。
	raw, _ := getRaw(cli, "/api/config")
	if strings.Contains(raw, "topsecret") {
		t.Fatalf("config view leaks secret: %s", raw)
	}

	// 导入:替换 join 与 routes,enabled 自动置 true。
	view, err = cli.Import(ctx, exportSample, true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if view.Join.NetworkName != "prod-net" || len(view.Routes) != 2 || !view.Enabled {
		t.Fatalf("import view: %+v", view)
	}
	// 导入后密钥更新为导出文件里的值(只写不读)。
	cfg, _ := rt.LoadConfig()
	if cfg.Join.NetworkSecret != "topsecret" || cfg.Join.Peers[0] != "tcp://1.2.3.4:11010" {
		t.Fatalf("import persisted wrong: %+v", cfg.Join)
	}

	// 断开:enabled=false。
	disabled := false
	if _, err := cli.PutConfig(ctx, ConfigSetRequest{Enabled: &disabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if cfg, _ := rt.LoadConfig(); cfg.Enabled {
		t.Fatal("disable not persisted")
	}

	// SOCKS5:部分更新(启用+端口)→ 回显生效;关闭后端口保留;非法端口报错。
	on, port := true, 1080
	view, err = cli.PutConfig(ctx, ConfigSetRequest{Socks5Enabled: &on, Socks5Port: &port})
	if err != nil {
		t.Fatalf("socks5 put: %v", err)
	}
	if !view.Socks5.Enabled || view.Socks5.Port != 1080 {
		t.Fatalf("socks5 view: %+v", view.Socks5)
	}
	off := false
	view, err = cli.PutConfig(ctx, ConfigSetRequest{Socks5Enabled: &off})
	if err != nil || view.Socks5.Enabled || view.Socks5.Port != 1080 {
		t.Fatalf("socks5 off should keep port: %+v err=%v", view.Socks5, err)
	}
	badPort := 99999
	if _, err := cli.PutConfig(ctx, ConfigSetRequest{Socks5Enabled: &on, Socks5Port: &badPort}); err == nil {
		t.Fatal("invalid socks5 port should error")
	}

	// 多出口代理:保存列表 → 回显与状态;端口重复被归一化拦截。
	proxies := []ProxyConfig{
		{Port: 1081, ExitNodes: []string{"10.126.126.2"}},
		{Port: 1082, ExitNodes: []string{"10.126.126.3", "10.126.126.4"}},
	}
	view, err = cli.PutConfig(ctx, ConfigSetRequest{Socks5Proxies: &proxies})
	if err != nil {
		t.Fatalf("proxies put: %v", err)
	}
	if len(view.Proxies) != 2 || view.Proxies[0].Port != 1081 || len(view.Proxies[1].ExitNodes) != 2 {
		t.Fatalf("proxies view: %+v", view.Proxies)
	}
	snap, err = cli.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(snap.Proxies) != 2 || snap.Proxies[0].Port != 1081 || len(snap.Proxies[0].ExitNodes) != 1 {
		t.Fatalf("proxies snapshot: %+v", snap.Proxies)
	}
	dup := []ProxyConfig{{Port: 1081}, {Port: 1081}}
	if _, err := cli.PutConfig(ctx, ConfigSetRequest{Socks5Proxies: &dup}); err == nil {
		t.Fatal("duplicate proxy port should error")
	}
	empty := []ProxyConfig{}
	if _, err := cli.PutConfig(ctx, ConfigSetRequest{Socks5Proxies: &empty}); err != nil {
		t.Fatalf("clear proxies: %v", err)
	}
	if cfg, _ := rt.LoadConfig(); len(cfg.Proxies) != 0 {
		t.Fatalf("proxies should be cleared: %+v", cfg.Proxies)
	}

	// 日志环:写两条,尾部可见。
	_, _ = rt.LogWriter().Write([]byte("line-1\nline-2\n"))
	var logs map[string]any
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cli.Base+"/api/logs?n=10", nil)
	req.Header.Set("Authorization", "Bearer "+cli.Token)
	resp, err := cli.HTTP.Do(req)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(&logs); err != nil {
		t.Fatalf("logs decode: %v", err)
	}
	lines, _ := logs["lines"].([]any)
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1].(string), "line-2") {
		t.Fatalf("log tail wrong: %v", lines)
	}
}

func TestIPCInfoRemovedOnShutdown(t *testing.T) {
	rt, dir := newTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := StartIPC(ctx, dir, rt); err != nil {
		t.Fatalf("start ipc: %v", err)
	}
	infoPath := filepath.Join(dir, "ipc.json")
	if _, err := os.Stat(infoPath); err != nil {
		t.Fatalf("ipc.json should exist: %v", err)
	}
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(infoPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("ipc.json should be removed after shutdown")
}

func getRaw(cli *IPCClient, path string) (string, error) {
	req, _ := http.NewRequest(http.MethodGet, cli.Base+path, nil)
	req.Header.Set("Authorization", "Bearer "+cli.Token)
	resp, err := cli.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n]), nil
}

func strPtr(s string) *string { return &s }
