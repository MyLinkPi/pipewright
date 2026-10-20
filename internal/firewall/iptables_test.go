// iptables_test.go 覆盖规则构造纯函数(golden)与网段/端口小件。
package firewall

import (
	"reflect"
	"testing"
)

func TestInputChainRules(t *testing.T) {
	got := inputChainRules("iptables", []int{80, 443}, []string{"10.0.0.0/24"})
	want := [][]string{
		{"iptables", "-N", chainInput},
		{"iptables", "-F", chainInput},
		{"iptables", "-A", chainInput, "-p", "tcp", "-m", "multiport", "--dports", "80,443", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"iptables", "-A", chainInput, "-i", "lo", "-j", "ACCEPT"},
		{"iptables", "-A", chainInput, "-p", "tcp", "-m", "multiport", "--dports", "80,443", "-s", "10.0.0.0/24", "-j", "ACCEPT"},
		{"iptables", "-A", chainInput, "-p", "tcp", "-m", "multiport", "--dports", "80,443", "-j", "DROP"},
		{"iptables", "-A", chainInput, "-j", "RETURN"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inputChainRules:\n got  %v\n want %v", got, want)
	}
}

func TestDockerChainRules(t *testing.T) {
	got := dockerChainRules("iptables", []int{5000}, nil)
	want := [][]string{
		{"iptables", "-N", chainDocker},
		{"iptables", "-F", chainDocker},
		{"iptables", "-A", chainDocker, "-p", "tcp", "-m", "multiport", "--dports", "5000", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"},
		{"iptables", "-A", chainDocker, "-p", "tcp", "-m", "multiport", "--dports", "5000", "-j", "DROP"},
		{"iptables", "-A", chainDocker, "-j", "RETURN"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dockerChainRules:\n got  %v\n want %v", got, want)
	}
}

func TestHookRulesPortsFree(t *testing.T) {
	// 挂钩规则体不含端口/网段:改过配置后,旧挂钩同样删得掉、查得到。
	if got := inputHookRule("iptables"); !reflect.DeepEqual(got, []string{"iptables", "-m", "comment", "--comment", ruleComment, "-j", chainInput}) {
		t.Fatalf("inputHookRule = %v", got)
	}
	if got := dockerHookRule("/usr/sbin/iptables"); !reflect.DeepEqual(got, []string{"/usr/sbin/iptables", "-m", "comment", "--comment", ruleComment, "-j", chainDocker}) {
		t.Fatalf("dockerHookRule = %v", got)
	}
}

func TestCleanupCommands(t *testing.T) {
	got := cleanupCommands("iptables", true)
	want := [][]string{
		{"iptables", "-D", "INPUT", "-m", "comment", "--comment", ruleComment, "-j", chainInput},
		{"iptables", "-F", chainInput},
		{"iptables", "-X", chainInput},
		{"iptables", "-D", "DOCKER-USER", "-m", "comment", "--comment", ruleComment, "-j", chainDocker},
		{"iptables", "-F", chainDocker},
		{"iptables", "-X", chainDocker},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanupCommands:\n got  %v\n want %v", got, want)
	}
	if got := cleanupCommands("ip6tables", false); len(got) != 3 {
		t.Fatalf("v6 cleanup 应只有 INPUT 侧三条, got %v", got)
	}
}

func TestMeshCIDRFromSelfIP(t *testing.T) {
	cases := map[string]string{
		"10.126.126.7": "10.126.126.0/24",
		" 172.16.3.9":  "172.16.3.0/24",
		"fd00::1":      "", // mesh 虚拟网是 IPv4
		"not-an-ip":    "",
		"":             "",
	}
	for in, want := range cases {
		if got := meshCIDRFromSelfIP(in); got != want {
			t.Fatalf("meshCIDRFromSelfIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitCIDRs(t *testing.T) {
	v4, v6 := splitCIDRs([]string{"10.126.126.0/24", "192.168.30.0/24", "fd00::/8"})
	if !reflect.DeepEqual(v4, []string{"10.126.126.0/24", "192.168.30.0/24"}) || !reflect.DeepEqual(v6, []string{"fd00::/8"}) {
		t.Fatalf("splitCIDRs v4=%v v6=%v", v4, v6)
	}
}

func TestNormalizePorts(t *testing.T) {
	got := normalizePorts([]int{8080, 80, 80, 0, 70000, 443})
	want := []int{80, 443, 8080}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizePorts = %v, want %v", got, want)
	}
}

func TestNormalizeCIDRs(t *testing.T) {
	got, err := normalizeCIDRs([]string{"10.1.2.3/24", " 10.1.2.0/24 ", "fd00::/8", ""})
	if err != nil {
		t.Fatalf("normalizeCIDRs err: %v", err)
	}
	want := []string{"10.1.2.0/24", "fd00::/8"} // 归一化 + 去重 + 排序
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeCIDRs = %v, want %v", got, want)
	}
	if _, err := normalizeCIDRs([]string{"10.0.0.0/33"}); err == nil {
		t.Fatal("非法 CIDR 应报错")
	}
}

func TestIPAllowed(t *testing.T) {
	cidrs := []string{"10.126.126.0/24", "fd00::/8"}
	cases := map[string]bool{
		"127.0.0.1":    true,
		"::1":          true,
		"10.126.126.9": true,
		"fd00::42":     true,
		"8.8.8.8":      false,
		"10.126.127.1": false,
		"garbage":      false,
	}
	for ip, want := range cases {
		if got := ipAllowed(ip, cidrs); got != want {
			t.Fatalf("ipAllowed(%q) = %v, want %v", ip, got, want)
		}
	}
}
