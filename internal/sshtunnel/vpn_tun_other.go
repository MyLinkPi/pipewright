//go:build !linux

package sshtunnel

import (
	"fmt"
	"os"
)

// createTunDevice 在非 Linux 平台直接报错(Windows 需 wintun 驱动、macOS 需内核
// 扩展,均不支持)。vpn 模式仅 Linux 控制节点可用,一般场景建议集群自组网。
func createTunDevice(idx int) (*os.File, string, error) {
	return nil, "", fmt.Errorf("VPN 隧道(-w)仅支持 Linux 控制节点(建议改用集群自组网)")
}
