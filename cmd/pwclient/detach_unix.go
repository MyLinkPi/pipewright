//go:build linux || darwin

// detach_unix.go 为发行目标平台(Linux/macOS)提供子进程会话脱离:升级子进程自建
// 会话后,终端/界面进程的组信号(SIGHUP)不再波及它,停服务→替换→重启的流程不会
// 因关闭终端被中途打断(留下停机的服务)。
package main

import (
	"os/exec"
	"syscall"
)

func detachChild(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
