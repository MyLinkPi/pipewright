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
//(免首连下载的关键回退路径;dataDir 未落盘时也能直接用随包二进制)。
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
