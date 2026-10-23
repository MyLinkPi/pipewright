package easytier

import (
	"reflect"
	"testing"
)

// baseArgs 是期望参数样例(buildArgs 产物形态:固定头 + 密钥 + 静态 IP + peers +
// 子网代理 + 手工路由 + network-directory)。
func baseArgs() []string {
	return []string{
		"--network-name", "net",
		"--rpc-portal", "127.0.0.1:15888",
		"--hostname", "h-px",
		"--network-secret", "sec",
		"-i", "10.126.126.5",
		"-p", "tcp://1.2.3.4:11010",
		"-p", "tcp://5.6.7.8:11010",
		"--proxy-networks", "192.168.30.0/24",
		"--manual-routes", "10.5.0.0/16",
		"--network-directory", `/data/easytier`,
	}
}

// TestSameArgs 验证收养比对:argv[0] 差异不影响;无序集合旗子(peers/proxy/
// manual-routes)排序后比对;其余任何差异(密钥/IP/hostname/目录/多余旗子/
// exit-nodes 顺序)都判不一致 —— 宁可多一次重启,不可误收养旧参数。
func TestSameArgs(t *testing.T) {
	base := baseArgs()
	withArgv0 := append([]string{"/opt/bin/easytier-core"}, base...)

	cases := []struct {
		name string
		got  []string
		want []string
		same bool
	}{
		{"完全一致(含 argv[0])", withArgv0, base, true},
		{"peers 顺序互换", swapPeers(base), base, true},
		{"密钥不同", mutate(base, "--network-secret", "sec", "other"), base, false},
		{"虚拟 IP 不同", mutate(base, "-i", "10.126.126.5", "10.126.126.9"), base, false},
		{"hostname 不同", mutate(base, "--hostname", "h-px", "h-px2"), base, false},
		{"network-directory 不同", mutate(base, "--network-directory", "/data/easytier", "/tmp/x"), base, false},
		{"子网代理不同", mutate(base, "--proxy-networks", "192.168.30.0/24", "192.168.31.0/24"), base, false},
		{"手工路由多一项", append(append([]string(nil), base...), "--manual-routes", "10.6.0.0/16"), base, false},
		{"多余旗子(裸 --dhcp)", append(append([]string(nil), base...), "--dhcp"), base, false},
		{"缺少 network-directory", dropPair(base, "--network-directory"), base, false},
		{"单复数旗子同值互认", mutate(base, "--proxy-networks", "--proxy-networks", "--proxy-network"), base, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SameArgs(c.got, c.want); got != c.same {
				t.Fatalf("SameArgs = %v, want %v\ngot:  %v\nwant: %v", got, c.same, c.got, c.want)
			}
		})
	}
}

// TestSameArgsExitNodeOrder 验证 exit-nodes 保序:序即优先级,顺序不同 = 语义不同,
// 不收养(与 peers 的集合语义相反)。
func TestSameArgsExitNodeOrder(t *testing.T) {
	a := []string{"--exit-nodes", "10.1.1.2", "--exit-nodes", "10.1.1.3"}
	b := []string{"--exit-nodes", "10.1.1.3", "--exit-nodes", "10.1.1.2"}
	if SameArgs(a, b) {
		t.Fatal("exit-nodes 顺序不同应判不一致(序即优先级)")
	}
	if !SameArgs(a, a) {
		t.Fatal("exit-nodes 相同顺序应判一致")
	}
}

// TestSameArgsBareManualRoutes 验证裸 --manual-routes(Strict 空列表,fail-closed)
// 与不传参、与带值形态互相区分:裸旗子 = 拒绝全部通告,语义必须精确。
func TestSameArgsBareManualRoutes(t *testing.T) {
	head := []string{"--network-name", "net", "--rpc-portal", "127.0.0.1:15888", "--hostname", "h"}
	bare := append(append([]string(nil), head...), "--manual-routes")
	WithValue := append(append([]string(nil), head...), "--manual-routes", "10.0.0.0/8")
	none := head
	if SameArgs(bare, none) || SameArgs(bare, WithValue) || SameArgs(WithValue, none) {
		t.Fatal("裸旗子/带值/缺省三种 manual-routes 形态必须互相区分")
	}
	if !SameArgs(bare, append([]string(nil), bare...)) {
		t.Fatal("同为裸旗子应判一致")
	}
}

// TestSameArgsDhcpAndEnableExitNode 验证 --dhcp 裸旗子参与比对;--enable-exit-node
// 带值形态值不同判不一致。
func TestSameArgsDhcpAndEnableExitNode(t *testing.T) {
	head := []string{"--network-name", "net", "--rpc-portal", "127.0.0.1:15888", "--hostname", "h"}
	dhcp := append(append([]string(nil), head...), "--dhcp")
	if SameArgs(dhcp, head) {
		t.Fatal("--dhcp 存在性应参与比对")
	}
	a := append(append([]string(nil), head...), "--enable-exit-node", "true")
	b := append(append([]string(nil), head...), "--enable-exit-node", "false")
	if SameArgs(a, b) {
		t.Fatal("--enable-exit-node 值不同应判不一致")
	}
}

// TestSplitCmdline 验证 CIM/ps 命令行文本 → argv:引号内空格不分隔(含空格路径),
// 引号剥离,多空白分隔。
func TestSplitCmdline(t *testing.T) {
	got := splitCmdline(`"C:\Program Files\et\easytier-core.exe" --network-name net -p "tcp://a b:11010"`)
	want := []string{`C:\Program Files\et\easytier-core.exe`, "--network-name", "net", "-p", `tcp://a b:11010`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitCmdline = %q, want %q", got, want)
	}
	if got := splitCmdline(""); len(got) != 0 {
		t.Fatalf("空行应拆出空 argv:%q", got)
	}
}

// TestAdoptedSupervisor 验证收养态监督器:Snapshot 报 Running+Adopted;Start 拒绝
// (收养即不拉子进程);Stop 幂等(目标进程不存在也不报错)。
func TestAdoptedSupervisor(t *testing.T) {
	sup := NewAdopted(Config{Mode: "managed", NetworkName: "net", RPCAddr: "127.0.0.1:15888"}, 999999)
	if snap := sup.Snapshot(); !snap.Running || !snap.Adopted {
		t.Fatalf("收养态快照应 Running+Adopted:%+v", snap)
	}
	if err := sup.Start(nil); err == nil {
		t.Fatal("收养态 Start 应报错(不拉子进程)")
	}
	sup.Stop() // pid 999999 不存在:Kill 失败静默,不 panic
	sup.Stop() // 幂等
}

// swapPeers 返回两个 -p 值互换后的副本(peers 是集合语义,顺序无关)。
func swapPeers(args []string) []string {
	out := append([]string(nil), args...)
	idx := []int{}
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-p" {
			idx = append(idx, i+1)
		}
	}
	if len(idx) != 2 {
		panic("swapPeers: 需要恰好两个 -p")
	}
	out[idx[0]], out[idx[1]] = out[idx[1]], out[idx[0]]
	return out
}

// mutate 返回替换 pair(flag, oldVal) 为 (flag, newVal) 后的副本(用于构造
// 「单个值不同」的对照 argv)。
func mutate(args []string, flag, oldVal, newVal string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == flag && out[i+1] == oldVal {
			head := append([]string(nil), out[:i+1]...)
			head = append(head, newVal)
			return append(head, out[i+2:]...)
		}
	}
	if flag == oldVal { // 旗子本身改名(单复数互认用)
		for i, a := range out {
			if a == flag {
				out[i] = newVal
				return out
			}
		}
	}
	panic("mutate: pair not found: " + flag + " " + oldVal)
}

// dropPair 返回删除 (flag, value) 对后的副本(值取 pair 的下一个 token)。
func dropPair(args []string, flag string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == flag {
			return append(out[:i], out[i+2:]...)
		}
	}
	panic("dropPair: flag not found: " + flag)
}
