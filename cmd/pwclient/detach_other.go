//go:build !windows && !linux && !darwin

// detach_other.go 为其余平台(FreeBSD 等,仅保持可编译)提供占位:无 Setsid 语义,
// 子进程原样启动(这些平台无发行产物)。
package main

import "os/exec"

func detachChild(*exec.Cmd) {}
