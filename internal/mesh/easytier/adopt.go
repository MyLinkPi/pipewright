// adopt.go 支撑「升级/服务重启零断网」的孤儿收养:宿主 re-exec 自更新或停机重启后,
// 托管的 easytier-core 成为孤儿继续服务(虚拟网不中断);新宿主按 rpc-portal 找到
// 占用者,命令行参数与期望完全一致则直接沿用(收养),参数有变化才杀旧拉新
// (重启生效本就是必须的)。
//
// 这是 v1.6.4 事故(孤儿占 rpc-portal 端口让新进程起不来)思路的反转:孤儿不再是
// 「必须先杀」,而是「先比对、一致即收养」;只有参数不一致的孤儿才是事故源(以旧
// 参数持续服务),仍走 KillStaleByRPCPortal 清理。比对必须严格:任何旗子/值的差异
// 都判不一致 —— 宁可多一次重启,不可误收养旧参数进程。
package easytier

import (
	"sort"
	"strings"
)

// StaleProcess 是按 rpc-portal 找到的一个 easytier-core 进程(Args 含 argv[0];
// 平台实现负责拆分原始命令行,Linux 从 /proc 精确拆,Windows 处理引号,macOS 受
// ps 输出所限为近似拆分 —— 拆不准会导致比对失败走杀旧拉新,保守方向正确)。
type StaleProcess struct {
	PID  int
	Args []string
}

// valFlags 是消耗下一 token 作为值的旗子(→ 归一键;proxy-networks 与历史单数
// proxy-network 语义同、peers 归一)。表外旗子一律按裸旗子记账并参与比对:
// 孤儿若带期望之外的旗子即判不一致,保守不收养。
var valFlags = map[string]string{
	"--network-name":      "--network-name",
	"--rpc-portal":        "--rpc-portal",
	"--hostname":          "--hostname",
	"--network-secret":    "--network-secret",
	"-i":                  "-i",
	"-p":                  "peers",
	"--proxy-networks":    "proxy",
	"--proxy-network":     "proxy",
	"--manual-routes":     "manual-routes",
	"--socks5":            "--socks5",
	"--exit-nodes":        "exit-nodes",
	"--enable-exit-node":  "--enable-exit-node",
	"--network-directory": "--network-directory",
}

// sortedFlags 是集合语义旗子(与传参顺序无关,排序后比对):peers/proxy 择优无序,
// manual-routes 列表无序(bare 旗子以空串值参与,天然区分)。exit-nodes 序即
// 出口优先级,必须保序,不在其列。
var sortedFlags = map[string]bool{"peers": true, "proxy": true, "manual-routes": true}

// SameArgs 报告孤儿 argv 与期望 argv(buildArgs 产物)语义等价:跳过 argv[0],
// 逐旗子比对值。只返回布尔不输出参数;日志侧需要打印命令行时走 maskArgs 打码。
func SameArgs(got, want []string) bool {
	return sameFlagSet(parseArgv(got), parseArgv(want))
}

// parseArgv 把 argv 解析为 旗子→值列表(bare 旗子值为空串)。非 "-" 开头的 token
// 跳过(仅 argv[0];本命令行无位置参数)。
func parseArgv(argv []string) map[string][]string {
	m := make(map[string][]string)
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if !strings.HasPrefix(tok, "-") {
			continue
		}
		if key, ok := valFlags[tok]; ok && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
			m[key] = append(m[key], argv[i+1])
			i++
			continue
		}
		// 裸旗子(--dhcp;--manual-routes 空值可选的 bare 形态):记存在性。
		m[tok] = append(m[tok], "")
	}
	return m
}

func sameFlagSet(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !sameValues(k, av, bv) {
			return false
		}
	}
	return true
}

func sameValues(key string, a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	if sortedFlags[key] {
		a, b = append([]string(nil), a...), append([]string(nil), b...)
		sort.Strings(a)
		sort.Strings(b)
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// splitCmdline 把 OS 命令行文本(CIM CommandLine / ps command)拆成 argv:处理
// 双引号包裹(引号内空格不分隔),不处理反斜杠转义 —— EasyTier 参数不含引号
// 转义场景,路径按生成原样回显。
func splitCmdline(s string) []string {
	var args []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return args
}
