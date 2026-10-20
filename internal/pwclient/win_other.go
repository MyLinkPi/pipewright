//go:build !windows

// win_other.go 是非 Windows 平台的占位实现(客户端首要支持 Win11,核心层保持可编译
// 可测试;Linux/macOS 形态属未来扩展)。数据目录改用 /etc 下路径,防火墙无操作。
package pwclient

import (
	"log"
	"path/filepath"
)

// ensureFirewall 非 Windows 无操作(Windows 防火墙规则仅存在于 win_windows.go)。
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
	log.Printf("[pwclient] 当前平台无需配置防火墙规则")
}

// reconcileSocks5Guard 非 Windows 无操作(防火墙规则仅存在于 win_windows.go;
// 非 Windows 形态部署上应自行以主机防火墙限制 SOCKS5 端口的非本机访问)。
func (rt *Runtime) reconcileSocks5Guard(ports []int) {
	rt.mu.Lock()
	rt.socks5GuardPorts = append([]int(nil), ports...)
	rt.mu.Unlock()
}

// DefaultDataDir 返回默认数据目录(非 Windows 形态;/etc/pipewright-client)。
func DefaultDataDir() string {
	return filepath.Join("/etc", "pipewright-client")
}
