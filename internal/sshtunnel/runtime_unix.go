//go:build !windows

package sshtunnel

import "syscall"

// detachedSysProcAttr 让 helper 自成会话(Setsid):脱离主进程的控制终端,收不到
// SIGHUP/组信号;主进程退出后自然孤儿化由 init 接管。stdio 已显式重定向到日志文件。
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
