//go:build !windows

// svc_other.go 是非 Windows 平台的服务占位(客户端首要支持 Win11;核心层保持可编译)。
package main

import (
	"fmt"
	"os"
)

// serviceCmd 处理 `pwclient service <verb>`(非 Windows 未实现)。
func serviceCmd(args []string) {
	fmt.Fprintln(os.Stderr, "服务形态当前仅支持 Windows(Win11);非 Windows 可用子命令:console/status/doctor")
	os.Exit(2)
}

// doctorPlatformExtra 是 doctor 的平台扩展占位。
func doctorPlatformExtra(check checkFn) {
	check("服务安装", true, "非 Windows 平台(跳过)", "")
}
