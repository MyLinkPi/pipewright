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
	"time"
)

// fwRuleName 是本客户端在 Windows 防火墙里的规则名(幂等:先删后加,best-effort)。
const fwRuleName = "pipewright-client (EasyTier)"

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

// DefaultDataDir 返回默认数据目录:%ProgramData%\pipewright-client(配置/二进制/日志)。
func DefaultDataDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "pipewright-client")
}
