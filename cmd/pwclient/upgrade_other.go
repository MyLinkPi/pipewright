//go:build !windows

// upgrade_other.go 是非 Windows 平台的升级占位(客户端自升级基于 Windows 服务与 exe 替换)。
package main

import (
	"fmt"
	"os"
)

// runUpgrade 处理 `pwclient upgrade`(非 Windows 未实现)。
func runUpgrade(_ []string) {
	fmt.Fprintln(os.Stderr, "升级当前仅支持 Windows(Win11)。")
	os.Exit(2)
}
