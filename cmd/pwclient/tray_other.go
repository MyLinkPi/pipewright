//go:build !windows

// tray_other.go 是非 Windows 平台的托盘占位(客户端 GUI 首要支持 Win11)。
package main

import (
	"fmt"
	"os"
)

// runTray 启动托盘 GUI(非 Windows 未实现)。
func runTray() {
	fmt.Fprintln(os.Stderr, "托盘 GUI 当前仅支持 Windows(Win11)。可用子命令:console/status/doctor/service")
	os.Exit(2)
}
