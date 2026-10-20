//go:build windows

// win_windows.go 是 Windows 专属支撑:防火墙放行(EasyTier 子网代理要求虚拟网卡流量
// 可入站;按 easytier-core 程序路径放行,免猜网卡名)与默认数据目录(%ProgramData%)。
package pwclient

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// fwRuleName 是本客户端在 Windows 防火墙里的规则名(幂等:先删后加,best-effort)。
const fwRuleName = "pipewright-client (EasyTier)"

// socks5GuardRuleName 是 SOCKS5 端口的入站阻断规则名:EasyTier 的 SOCKS5 服务无认证,
// 阻断非本机访问防局域网白拿集群入口(loopback 流量不过 Windows 防火墙,本机使用不受影响)。
const socks5GuardRuleName = "pipewright-client socks5 guard"

// ensureFirewall 为 easytier-core 添加入站放行规则(每进程每路径只做一次;失败仅记日志,
// 不阻断组网 —— 多数场景出站访问通告网段并不依赖该规则,放行是为入站方向完整可达)。
func (rt *Runtime) ensureFirewall(corePath string) {
	rt.mu.Lock()
	done := rt.fwEnsured
	rt.mu.Unlock()
	if done {
		return
	}
	rt.mu.Lock()
	rt.fwEnsured = true
	rt.mu.Unlock()

	run := func(args ...string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "netsh", args...).Run()
	}
	run("advfirewall", "firewall", "delete", "rule", "name="+fwRuleName)
	run("advfirewall", "firewall", "add", "rule", "name="+fwRuleName, "dir=in", "action=allow",
		"program="+filepath.Clean(corePath), "enable=yes")
	log.Printf("[pwclient] 已尝试配置防火墙入站放行规则(%s)", fwRuleName)
}

// reconcileSocks5Guard 按「生效端口集」对账 SOCKS5 阻断规则:非空确保规则覆盖全部
// 监听端口(主 SOCKS5 + 多出口代理,逗号并列);空集移除规则。端口集变化自动换绑
// (先删后加)。失败仅记日志,不阻断组网。
func (rt *Runtime) reconcileSocks5Guard(ports []int) {
	wanted := append([]int(nil), ports...)
	sort.Ints(wanted)
	rt.mu.Lock()
	applied := append([]int(nil), rt.socks5GuardPorts...)
	rt.socks5GuardPorts = append([]int(nil), wanted...)
	rt.mu.Unlock()
	if slices.Equal(applied, wanted) {
		return
	}
	run := func(args ...string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "netsh", args...).Run()
	}
	run("advfirewall", "firewall", "delete", "rule", "name="+socks5GuardRuleName)
	if len(wanted) > 0 {
		strs := make([]string, len(wanted))
		for i, p := range wanted {
			strs[i] = strconv.Itoa(p)
		}
		run("advfirewall", "firewall", "add", "rule", "name="+socks5GuardRuleName,
			"dir=in", "action=block", "protocol=TCP", "localport="+strings.Join(strs, ","), "enable=yes")
		log.Printf("[pwclient] 已启用 SOCKS5 端口 %v 的入站阻断规则(仅限本机使用)", wanted)
	}
}

// DefaultDataDir 返回默认数据目录:%ProgramData%\pipewright-client(配置/二进制/日志)。
func DefaultDataDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "pipewright-client")
}
