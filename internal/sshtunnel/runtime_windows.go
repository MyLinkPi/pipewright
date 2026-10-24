//go:build windows

package sshtunnel

import "syscall"

// detachedSysProcAttr 让 helper 脱离主进程的进程组与控制台:DETACHED_PROCESS 使其
// 不继承控制台(关控制台/Ctrl+C 不波及),CREATE_NEW_PROCESS_GROUP 隔离组信号。
// stdio 已显式重定向到日志文件,脱离控制台不影响输出。
func detachedSysProcAttr() *syscall.SysProcAttr {
	const detachedProcess = 0x00000008
	return &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
}
