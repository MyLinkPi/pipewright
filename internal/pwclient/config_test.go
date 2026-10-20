// config_test.go 覆盖配置归一化/持久化/导出导入的关键行为(纯文件操作,不触网)。
package pwclient

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeValidatesAndFillsDefaults(t *testing.T) {
	// 非法 CIDR / 非法虚拟 IP 必须报错并带回原值(管理员/GUI 手输须即时反馈)。
	if _, err := Normalize(Config{Routes: []string{"192.168.30.0/24", "10.0.0.1/8", "not-a-cidr"}}); err == nil || !strings.Contains(err.Error(), "not-a-cidr") {
		t.Fatalf("invalid cidr should error with value: %v", err)
	}
	if _, err := Normalize(Config{Join: JoinConfig{VirtualIP: "999.1.2.3"}}); err == nil {
		t.Fatal("invalid virtual ip should error")
	}

	cfg, err := Normalize(Config{
		Join:   JoinConfig{NetworkName: " net ", NetworkSecret: " sec ", Peers: []string{" tcp://1.2.3.4:11010 ", "", "tcp://1.2.3.4:11010"}},
		Routes: []string{" 10.5.0.0/16 ", "10.5.0.0/16", ""},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.Join.NetworkName != "net" || cfg.Join.NetworkSecret != "sec" {
		t.Fatalf("trim failed: %+v", cfg.Join)
	}
	if len(cfg.Join.Peers) != 1 || len(cfg.Routes) != 1 || cfg.Routes[0] != "10.5.0.0/16" {
		t.Fatalf("dedupe failed: peers=%v routes=%v", cfg.Join.Peers, cfg.Routes)
	}
	if cfg.EasyTier.RPCAddr != "127.0.0.1:15889" {
		t.Fatalf("rpc default not filled: %q", cfg.EasyTier.RPCAddr)
	}

	// SOCKS5:启用且未填端口 → 默认 1080;非法端口报错;关闭时端口保留。
	s5, serr := Normalize(Config{Socks5: Socks5Config{Enabled: true}})
	if serr != nil || s5.Socks5.Port != socks5DefaultPort {
		t.Fatalf("socks5 default port: %d err=%v", s5.Socks5.Port, serr)
	}
	if _, err := Normalize(Config{Socks5: Socks5Config{Enabled: true, Port: 70000}}); err == nil || !strings.Contains(err.Error(), "70000") {
		t.Fatalf("invalid socks5 port should error: %v", err)
	}
	kept := Config{Socks5: Socks5Config{Enabled: false, Port: 1080}}
	if out, _ := Normalize(kept); out.Socks5.Port != 1080 {
		t.Fatalf("disabled socks5 should keep port: %+v", out.Socks5)
	}
}

func TestStoreRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	store := &Store{Path: path}

	// 文件不存在 = 未配置态,非错误。
	if cfg, err := store.Load(); err != nil || cfg.Configured() {
		t.Fatalf("missing file should be zero config: %+v err=%v", cfg, err)
	}

	in := Config{
		Enabled: true,
		Join:    JoinConfig{NetworkName: "prod", NetworkSecret: "s3cr3t", Peers: []string{"tcp://10.0.0.1:11010"}, Hostname: "my-pc"},
		Routes:  []string{"192.168.30.0/24"},
	}
	if err := store.Save(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !out.Enabled || out.Join.NetworkName != "prod" || out.Join.NetworkSecret != "s3cr3t" ||
		len(out.Join.Peers) != 1 || len(out.Routes) != 1 || !out.Configured() {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
	// 0600 权限(含明文密钥)。Windows/NTFS 不映射 Unix 权限位,仅非 Windows 校验。
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("config perm should be 0600: %v %v", fi.Mode().Perm(), err)
		}
	}
}

const exportSample = `join:
  networkName: prod-net
  networkSecret: topsecret
  peers:
    - tcp://1.2.3.4:11010
    - udp://5.6.7.8:11010
routes:
  - 192.168.30.0/24
  - 10.5.0.0/16
export:
  from: node-a
  nodeId: abc
  platformVersion: v1.2.3
`

func TestImportExport(t *testing.T) {
	join, routes, meta, err := ImportExport([]byte(exportSample))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if join.NetworkName != "prod-net" || join.NetworkSecret != "topsecret" || len(join.Peers) != 2 {
		t.Fatalf("join mismatch: %+v", join)
	}
	if len(routes) != 2 || meta.From != "node-a" || meta.PlatformVersion != "v1.2.3" {
		t.Fatalf("routes/meta mismatch: %v %+v", routes, meta)
	}

	// 缺密钥 → 明确报错。
	bad := strings.Replace(exportSample, "topsecret", "", 1)
	if _, _, _, err := ImportExport([]byte(bad)); err == nil || !strings.Contains(err.Error(), "密钥") {
		t.Fatalf("missing secret should error: %v", err)
	}
	// 非法网段 → 带原值报错。
	badRoutes := strings.Replace(exportSample, "10.5.0.0/16", "10.5.0.0/99", 1)
	if _, _, _, err := ImportExport([]byte(badRoutes)); err == nil || !strings.Contains(err.Error(), "10.5.0.0/99") {
		t.Fatalf("invalid route should error: %v", err)
	}
}

func TestRuntimeImportJoin(t *testing.T) {
	dir := t.TempDir()
	rt := NewRuntime(&Store{Path: filepath.Join(dir, "config.yaml")}, dir)
	rt.newDownloader = nil // 测试不触网

	// 导入(并集模式):既有 routes 与导出 routes 合并去重,enabled 置 true。
	if err := rt.SaveConfig(Config{
		Enabled: false,
		Join:    JoinConfig{NetworkName: "old", NetworkSecret: "old-secret"},
		Routes:  []string{"10.5.0.0/16"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	cfg, err := rt.ImportJoin([]byte(exportSample), false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !cfg.Enabled || cfg.Join.NetworkName != "prod-net" || cfg.Join.NetworkSecret != "topsecret" {
		t.Fatalf("join not replaced: %+v", cfg)
	}
	if len(cfg.Routes) != 2 { // 10.5.0.0/16(既有)+ 192.168.30.0/24(导出)
		t.Fatalf("routes union: %v", cfg.Routes)
	}

	// 覆盖模式:routes 以导出文件为准。
	cfg2, err := rt.ImportJoin([]byte(exportSample), true)
	if err != nil || len(cfg2.Routes) != 2 {
		t.Fatalf("replace mode: %v err=%v", cfg2.Routes, err)
	}
	// 密钥只写不读:LoadConfig 返回明文(服务内使用),IPC 层的 toConfigView 才脱敏。
	view := toConfigView(cfg2)
	if view.Join.HasSecret != true {
		t.Fatalf("view should report hasSecret")
	}
}

// TestNormalizeSocks5ExitNodes 验证出口节点归一化:非法 IP 报错、去重保序、空白剔除。
func TestNormalizeSocks5ExitNodes(t *testing.T) {
	cfg, err := Normalize(Config{Socks5: Socks5Config{Enabled: true,
		ExitNodes: []string{" 10.126.126.2 ", "", "10.126.126.2", "10.126.126.3"}}})
	if err != nil {
		t.Fatalf("归一化失败:%v", err)
	}
	want := []string{"10.126.126.2", "10.126.126.3"}
	if len(cfg.Socks5.ExitNodes) != len(want) {
		t.Fatalf("exitNodes = %v, want %v", cfg.Socks5.ExitNodes, want)
	}
	for i := range want {
		if cfg.Socks5.ExitNodes[i] != want[i] {
			t.Fatalf("exitNodes[%d] = %q, want %q", i, cfg.Socks5.ExitNodes[i], want[i])
		}
	}
	if _, err := Normalize(Config{Socks5: Socks5Config{Enabled: true, ExitNodes: []string{"10.0.0.0/8"}}}); err == nil {
		t.Fatal("CIDR 形式的出口节点应报错(须为虚拟 IP)")
	}
}

// TestNormalizeProxies 验证多出口代理归一化:出口去重保序、端口冲突/重复/越界报错。
func TestNormalizeProxies(t *testing.T) {
	cfg, err := Normalize(Config{Socks5: Socks5Config{Enabled: true, Port: 1080},
		Proxies: []ProxyConfig{
			{Port: 1081, ExitNodes: []string{" 10.126.126.2 ", "10.126.126.2", "10.126.126.3"}},
			{Port: 1082},
		}})
	if err != nil {
		t.Fatalf("归一化失败:%v", err)
	}
	if len(cfg.Proxies) != 2 {
		t.Fatalf("proxies = %+v", cfg.Proxies)
	}
	want := []string{"10.126.126.2", "10.126.126.3"}
	if len(cfg.Proxies[0].ExitNodes) != len(want) || cfg.Proxies[0].ExitNodes[0] != want[0] || cfg.Proxies[0].ExitNodes[1] != want[1] {
		t.Fatalf("proxy0 exits = %v, want %v", cfg.Proxies[0].ExitNodes, want)
	}
	if len(cfg.Proxies[1].ExitNodes) != 0 {
		t.Fatalf("proxy1 exits should be empty: %v", cfg.Proxies[1].ExitNodes)
	}

	cases := []struct {
		name    string
		proxies []ProxyConfig
	}{
		{"端口重复", []ProxyConfig{{Port: 1081}, {Port: 1081}}},
		{"端口越界", []ProxyConfig{{Port: 0}}},
		{"端口超上限", []ProxyConfig{{Port: 70000}}},
		{"与主 SOCKS5 冲突", []ProxyConfig{{Port: 1080}}},
		{"出口非法 IP", []ProxyConfig{{Port: 1081, ExitNodes: []string{"not-an-ip"}}}},
	}
	for _, tc := range cases {
		base := Config{Socks5: Socks5Config{Enabled: true, Port: 1080}, Proxies: tc.proxies}
		if _, err := Normalize(base); err == nil {
			t.Fatalf("%s 应报错", tc.name)
		}
	}

	// 主 SOCKS5 未启用时不视为冲突(端口当前未占用)。
	if _, err := Normalize(Config{Socks5: Socks5Config{Enabled: false, Port: 1080},
		Proxies: []ProxyConfig{{Port: 1080}}}); err != nil {
		t.Fatalf("主代理停用时同端口不应报错:%v", err)
	}
}
