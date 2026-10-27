package easytier

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestParseHelpFlags 验证 --help 输出 → 旗子集合解析:官方复数形态优先,历史单数兼容,
// 缺失/探测失败为不支持(保守)。
func TestParseHelpFlags(t *testing.T) {
	cases := []struct {
		name string
		out  string
		ok   bool
		want helpFlags
	}{
		{"官方复数", "--proxy-networks <CIDR>...\n--manual-routes <CIDR>... \n--socks5 <PORT>", true,
			helpFlags{proxyNetwork: "--proxy-networks", manualRoutes: true, socks5: true}},
		{"历史单数无socks5", "  --proxy-network <CIDR>", true,
			helpFlags{proxyNetwork: "--proxy-network"}},
		{"两者都没有", "--network-name <NAME>", true, helpFlags{}},
		{"探测失败", "--proxy-networks whatever", false, helpFlags{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseHelpFlags(c.out, c.ok); got != c.want {
				t.Fatalf("parseHelpFlags = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestBuildArgs 验证参数组装:基础参数序、DHCP/静态 IP 分支、peers、子网代理按探测到的
// 旗子名逐值重复传、manual-routes 逐值重复传且仅在支持时传递、socks5 仅在支持时传递。
func TestBuildArgs(t *testing.T) {
	base := Config{
		NetworkName:   "net",
		NetworkSecret: "sec",
		RPCAddr:       "127.0.0.1:15888",
		Hostname:      "h1",
		VirtualIP:     "10.1.1.5",
		Peers:         []string{"tcp://1.2.3.4:11010", ""},
		ProxyNetworks: []string{"192.168.30.0/24", ""},
		ManualRoutes:  []string{"10.5.0.0/16"},
		Socks5Port:    1080,
	}
	got := buildArgs(base, helpFlags{proxyNetwork: "--proxy-networks", manualRoutes: true, socks5: true})
	want := []string{
		"--network-name", "net",
		"--rpc-portal", "127.0.0.1:15888",
		"--hostname", "h1",
		"--private-mode", "true",
		"--network-secret", "sec",
		"-i", "10.1.1.5",
		"-p", "tcp://1.2.3.4:11010",
		"--proxy-networks", "192.168.30.0/24",
		"--manual-routes", "10.5.0.0/16",
		"--socks5", "1080",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildArgs = %v, want %v", got, want)
	}

	// DHCP 分支 + 单数旗子兼容 + 不支持 manual-routes/socks5 时忽略。
	dhcp := base
	dhcp.VirtualIP = ""
	got = buildArgs(dhcp, helpFlags{proxyNetwork: "--proxy-network"})
	if !containsPair(got, "--dhcp", "") {
		t.Fatalf("dhcp 分支缺少 --dhcp: %v", got)
	}
	if containsPair(got, "-i", "10.1.1.5") {
		t.Fatalf("dhcp 分支不应传静态 IP: %v", got)
	}
	if !containsPair(got, "--proxy-network", "192.168.30.0/24") {
		t.Fatalf("应按探测旗子名单数传参: %v", got)
	}
	if containsPair(got, "--proxy-networks", "192.168.30.0/24") {
		t.Fatalf("单数旗子形态下不应出现复数参数: %v", got)
	}
	assertFlagAbsent(t, got, "--manual-routes", "manual-routes 不支持时不应传参")
	assertFlagAbsent(t, got, "--socks5", "socks5 不支持时不应传参")

	// 未知子网代理旗子(二进制不支持)→ 不传代理参数;socks5 关闭(0)→ 不传。
	noSocks := base
	noSocks.Socks5Port = 0
	got = buildArgs(noSocks, helpFlags{})
	assertFlagAbsent(t, got, "--proxy-network", "不支持子网代理时不应传参")
	assertFlagAbsent(t, got, "--socks5", "socks5 关闭时不应传参")
}

func assertFlagAbsent(t *testing.T, args []string, flag, msg string) {
	t.Helper()
	for _, a := range args {
		if strings.HasPrefix(a, flag) {
			t.Fatalf("%s: %v", msg, args)
		}
	}
}

func containsPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && (val == "" || args[i+1] == val) {
			return true
		}
	}
	return false
}

// TestLocateExeAdjacent 验证发行包布局:exe 同级 bin/ 下的二进制可被定位
// (免首连下载的关键回退路径;dataDir 未落盘时也能直接用随包二进制)。
func TestLocateExeAdjacent(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("取自身路径失败:%v", err)
	}
	adj := filepath.Join(filepath.Dir(exe), "bin", CoreBinary())
	if err := os.MkdirAll(filepath.Dir(adj), 0o755); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(adj) }()
	if err := os.WriteFile(adj, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Locate("", t.TempDir(), CoreBinary()); got != adj {
		t.Fatalf("Locate 未命中 exe 同级 bin:got %q, want %q", got, adj)
	}
}

// TestBuildArgsPrivateMode 验证私有模式无条件传参:不随 --help 探测结果变化、与
// 其它配置无关(安全默认);且必须显式带值 "true" —— v2.6.4 的布尔旗子是
// <PRIVATE_MODE> 取值形(possible values true/false),裸旗子会被 clap 拒绝、
// easytier-core 秒退(2026-10-27 线上事故根因)。
func TestBuildArgsPrivateMode(t *testing.T) {
	cfg := Config{NetworkName: "net", RPCAddr: "127.0.0.1:15888", Hostname: "h1"}
	for _, flags := range []helpFlags{{}, {socks5: true, manualRoutes: true}} {
		if !containsPair(buildArgs(cfg, flags), "--private-mode", "true") {
			t.Fatalf("私有模式应无条件以显式值启用(flags=%+v):%v", flags, buildArgs(cfg, flags))
		}
	}
}

// TestBuildArgsStrictEmptyRoutes 验证空列表的两种语义:Strict 时传裸 --manual-routes
// (值可选 = 拒绝全部子网代理通告,fail-closed);非 Strict 不传参(平台中枢接受全部)。
func TestBuildArgsStrictEmptyRoutes(t *testing.T) {
	base := Config{NetworkName: "net", RPCAddr: "127.0.0.1:15888", Hostname: "h1", Socks5Port: 1080, StrictManualRoutes: true}
	flags := helpFlags{manualRoutes: true, socks5: true}

	strict := buildArgs(base, flags)
	hasFlag := false
	for _, a := range strict {
		if a == "--manual-routes" {
			hasFlag = true
		}
	}
	if !hasFlag {
		t.Fatalf("Strict 空列表应传裸旗子(不带值):%v", strict)
	}
	// 裸旗子后紧跟 --socks5:clap 可选值遇 - 前缀旗子即终止,顺序合法。
	for i, a := range strict {
		if a == "--manual-routes" && i+1 < len(strict) && !strings.HasPrefix(strict[i+1], "-") {
			t.Fatalf("裸 --manual-routes 后不应跟值:%v", strict)
		}
	}

	base.StrictManualRoutes = false
	loose := buildArgs(base, flags)
	for _, a := range loose {
		if a == "--manual-routes" {
			t.Fatalf("非 Strict 空列表不应传 --manual-routes:%v", loose)
		}
	}
}

// TestBuildArgsExitNodes 验证出口节点传参:逐值重复传(序即优先级)、EnableExitNode
// 单独成参、不支持的二进制不传参。
func TestBuildArgsExitNodes(t *testing.T) {
	cfg := Config{
		NetworkName: "net", RPCAddr: "127.0.0.1:15888", Hostname: "h1",
		Socks5Port: 1080, ExitNodes: []string{"10.126.126.2", "10.126.126.3"}, EnableExitNode: true,
	}
	got := buildArgs(cfg, helpFlags{socks5: true, exitNodes: true, enableExitNode: true})
	if !containsPair(got, "--exit-nodes", "10.126.126.2") || !containsPair(got, "--exit-nodes", "10.126.126.3") {
		t.Fatalf("出口节点应逐值传:%v", got)
	}
	if !containsPair(got, "--enable-exit-node", "true") {
		t.Fatalf("EnableExitNode 应传参:%v", got)
	}
	// 顺序即优先级:第一个出口在第二个之前。
	first, second := -1, -1
	for i, a := range got {
		if a == "10.126.126.2" {
			first = i
		}
		if a == "10.126.126.3" {
			second = i
		}
	}
	if first < 0 || second < 0 || first > second {
		t.Fatalf("出口节点顺序应保留:%v", got)
	}

	old := buildArgs(cfg, helpFlags{socks5: true}) // 旧二进制:两个旗子都不支持
	for _, a := range old {
		if a == "--exit-nodes" || a == "--enable-exit-node" {
			t.Fatalf("不支持的旗子不应传参:%v", old)
		}
	}
}

// TestBuildArgsNetworkDirRemoved 验证不再传 --network-directory:v2.6.4 已移除该旗子,
// 传入即 clap 报错秒退。NetworkDir 仍作子进程 cwd(见 Config 注释),但绝不出现在
// 命令行参数里。
func TestBuildArgsNetworkDirRemoved(t *testing.T) {
	cfg := Config{NetworkName: "net", RPCAddr: "127.0.0.1:15890", Hostname: "h-px",
		NetworkDir: `C:\et\proxy-1081`}
	for _, flags := range []helpFlags{{}, {socks5: true}} {
		got := buildArgs(cfg, flags)
		for _, a := range got {
			if strings.HasPrefix(a, "--network-directory") {
				t.Fatalf("--network-directory 已在 v2.6.4 移除,不应传参:%v", got)
			}
		}
	}
}
