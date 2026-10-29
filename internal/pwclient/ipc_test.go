// ipc_test.go 覆盖 IPC 通道:token 鉴权、配置读写(密钥绝不回显)、导入、状态快照。
// 使用真实 loopback 监听(StartIPC 绑 127.0.0.1:0),下载器置 nil 防止触网。
package pwclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/version"
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

	// 阿里云安全组:AK + 多组保存 → 回显(AK 非机密明文、SK 只报存在性);SK 空串 =
	// 保持;不完整的组行被归一化拦截;清空组保留 AK。
	akID, akSK := "LTAI-test", "sk-test"
	groups := []AliyunGroupConfig{
		{RegionID: "cn-hangzhou", SecurityGroupID: "sg-1", Description: "pw"},
		{RegionID: "cn-shanghai", SecurityGroupID: "sg-2", Description: "pw"},
	}
	view, err = cli.PutConfig(ctx, ConfigSetRequest{AliyunAccessKeyID: &akID,
		AliyunAccessKeySecret: &akSK, AliyunGroups: &groups})
	if err != nil {
		t.Fatalf("aliyun put: %v", err)
	}
	if view.Aliyun.AccessKeyID != "LTAI-test" || !view.Aliyun.HasSecret || len(view.Aliyun.Groups) != 2 {
		t.Fatalf("aliyun view: %+v", view.Aliyun)
	}
	raw, _ = getRaw(cli, "/api/config")
	if strings.Contains(raw, "sk-test") {
		t.Fatalf("config view leaks aliyun secret: %s", raw)
	}
	newAK, emptySK := "LTAI-2", ""
	view, err = cli.PutConfig(ctx, ConfigSetRequest{AliyunAccessKeyID: &newAK, AliyunAccessKeySecret: &emptySK})
	if err != nil || !view.Aliyun.HasSecret || view.Aliyun.AccessKeyID != "LTAI-2" {
		t.Fatalf("空 SK 应保持已存: %+v err=%v", view.Aliyun, err)
	}
	if cfg, _ := rt.LoadConfig(); cfg.Aliyun.AccessKeySecret != "sk-test" {
		t.Fatal("SK 被空串意外清除")
	}
	badGroups := []AliyunGroupConfig{{RegionID: "cn-hangzhou"}}
	if _, err := cli.PutConfig(ctx, ConfigSetRequest{AliyunGroups: &badGroups}); err == nil {
		t.Fatal("不完整的安全组行应报错")
	}
	emptyGroups := []AliyunGroupConfig{}
	if _, err := cli.PutConfig(ctx, ConfigSetRequest{AliyunGroups: &emptyGroups}); err != nil {
		t.Fatalf("清空安全组: %v", err)
	}
	if cfg, _ := rt.LoadConfig(); len(cfg.Aliyun.Groups) != 0 || cfg.Aliyun.AccessKeyID != "LTAI-2" {
		t.Fatalf("组应被清空且 AK 保留: %+v", cfg.Aliyun)
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

// 升级检查端点:服务侧返回 UpdateInfo;快照带客户端版本/路径。
func TestIPCUpgradeCheck(t *testing.T) {
	rt, dir := newTestRuntime(t)
	rt.checkUpgrade = func(context.Context, bool) version.UpdateInfo {
		return version.UpdateInfo{Current: "v1.0.0", Latest: "v1.1.0", UpdateAvailable: true}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := StartIPC(ctx, dir, rt); err != nil {
		t.Fatalf("start ipc: %v", err)
	}
	cli, err := DialIPC(dir)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	info, err := cli.CheckUpgrade(ctx, false)
	if err != nil {
		t.Fatalf("check upgrade: %v", err)
	}
	if info.Latest != "v1.1.0" || !info.UpdateAvailable || info.Current != "v1.0.0" {
		t.Fatalf("升级检查回显不符: %+v", info)
	}

	snap, err := cli.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if snap.ClientVersion != version.Version {
		t.Errorf("快照客户端版本 = %q,期望 %q", snap.ClientVersion, version.Version)
	}
	if snap.ClientPath == "" {
		t.Errorf("快照应带服务 exe 路径")
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
	blob, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(blob), nil
}

func strPtr(s string) *string { return &s }

// TestIPCConfigViewJSONContract 锁定配置回显的 JSON 键名(与 StatusSnapshot 一致的小驼峰)
// 且密钥永不外泄 —— 新界面按这些键名渲染「配置总览」与逐项编辑表单。
func TestIPCConfigViewJSONContract(t *testing.T) {
	rt, dir := newTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := StartIPC(ctx, dir, rt); err != nil {
		t.Fatalf("start ipc: %v", err)
	}
	cli, err := DialIPC(dir)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	enabled := true
	port := 1080
	exits := []string{"10.126.14.2"}
	proxies := []ProxyConfig{{Port: 1081, ExitNodes: []string{"10.126.14.2"}}}
	secret, akSK := "top-secret", "sk-secret"
	groups := []AliyunGroupConfig{{RegionID: "cn-hangzhou", SecurityGroupID: "sg-1", Description: "pw"}}
	if _, err := cli.PutConfig(ctx, ConfigSetRequest{
		Enabled:               &enabled,
		NetworkName:           strPtr("net"),
		NetworkSecret:         &secret,
		Socks5Enabled:         &enabled,
		Socks5Port:            &port,
		Socks5ExitNodes:       &exits,
		Socks5Proxies:         &proxies,
		AliyunAccessKeyID:     strPtr("LTAI-x"),
		AliyunAccessKeySecret: &akSK,
		AliyunGroups:          &groups,
	}); err != nil {
		t.Fatalf("put config: %v", err)
	}

	raw, err := getRaw(cli, "/api/config")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	for _, want := range []string{
		`"socks5":{"enabled":true,"port":1080,"exitNodes":["10.126.14.2"]}`,
		`"port":1081`,
		`"regionId":"cn-hangzhou"`,
		`"binDir":`,
		`"rpcAddr":`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("配置回显缺键 %s: %s", want, raw)
		}
	}
	for _, leak := range []string{`"Enabled"`, `"Port"`, `"ExitNodes"`, `"BinDir"`, `"RPCAddr"`, `"RegionID"`, "top-secret", "sk-secret"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("配置回显出现不应有的键/密钥 %q: %s", leak, raw)
		}
	}
}

// TestIPCPutConfigPartialUpdate 验证单项修改只提交变更字段时其余配置保持不变。
func TestIPCPutConfigPartialUpdate(t *testing.T) {
	rt, dir := newTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := StartIPC(ctx, dir, rt); err != nil {
		t.Fatalf("start ipc: %v", err)
	}
	cli, err := DialIPC(dir)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	name, host := "net-a", "host-a"
	peers := []string{"tcp://1.2.3.4:11010"}
	routes := []string{"10.0.0.0/8"}
	if _, err := cli.PutConfig(ctx, ConfigSetRequest{
		NetworkName: &name, Hostname: &host, Peers: &peers, Routes: &routes,
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	newHost := "host-b"
	view, err := cli.PutConfig(ctx, ConfigSetRequest{Hostname: &newHost})
	if err != nil {
		t.Fatalf("partial put: %v", err)
	}
	if view.Join.Hostname != "host-b" {
		t.Fatalf("hostname 未更新: %+v", view.Join)
	}
	if view.Join.NetworkName != "net-a" || len(view.Join.Peers) != 1 || view.Join.Peers[0] != "tcp://1.2.3.4:11010" {
		t.Fatalf("未提交字段被意外改动: %+v", view.Join)
	}
	if len(view.Routes) != 1 || view.Routes[0] != "10.0.0.0/8" {
		t.Fatalf("routes 被意外改动: %v", view.Routes)
	}
}

// TestIPCLogsSince 验证日志增量端点:首拉 reset + 后续只返回新增行。
func TestIPCLogsSince(t *testing.T) {
	rt, dir := newTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := StartIPC(ctx, dir, rt); err != nil {
		t.Fatalf("start ipc: %v", err)
	}
	cli, err := DialIPC(dir)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, _ = rt.LogWriter().Write([]byte("a\nb\n"))
	chunk, err := cli.LogsSince(ctx, 0, 10)
	if err != nil {
		t.Fatalf("logs since: %v", err)
	}
	if !chunk.Reset || chunk.NextSeq < 2 || len(chunk.Lines) != 2 || chunk.Lines[0] != "a" || chunk.Lines[1] != "b" {
		t.Fatalf("首拉不符: %+v", chunk)
	}
	_, _ = rt.LogWriter().Write([]byte("c\n"))
	next, err := cli.LogsSince(ctx, chunk.NextSeq, 10)
	if err != nil {
		t.Fatalf("logs since 2: %v", err)
	}
	if next.Reset || len(next.Lines) != 1 || next.Lines[0] != "c" {
		t.Fatalf("增量不符: %+v", next)
	}
}

// TestIPCUpgradeStatus 验证升级进度端点:文件缺失返回空阶段,写入后回显。
func TestIPCUpgradeStatus(t *testing.T) {
	rt, dir := newTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := StartIPC(ctx, dir, rt); err != nil {
		t.Fatalf("start ipc: %v", err)
	}
	cli, err := DialIPC(dir)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	st, err := cli.UpgradeStatus(ctx)
	if err != nil || st.Phase != "" {
		t.Fatalf("缺失状态文件应返回空阶段: %+v err=%v", st, err)
	}
	if err := WriteUpgradeStatus(dir, UpgradeStatus{Phase: UpgradePhaseDownloading, Target: "v9.9.9"}); err != nil {
		t.Fatalf("write status: %v", err)
	}
	st, err = cli.UpgradeStatus(ctx)
	if err != nil || st.Phase != UpgradePhaseDownloading || st.Target != "v9.9.9" {
		t.Fatalf("升级状态回显不符: %+v err=%v", st, err)
	}
}
