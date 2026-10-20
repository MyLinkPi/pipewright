//go:build linux

// stale_linux.go 清理孤儿 easytier-core:re-exec 自更新(syscall.Exec 不执行清理)、
// 进程崩溃或被 kill -9 都会把托管的 easytier 子进程留成孤儿,孤儿继续占用
// 监听/rpc-portal 端口,导致新进程的 easytier 起不来、孤儿以旧参数持续服务
// (v1.6.4 事故链:1.2.3 的孤儿让 1.6.5 的路由隔离形同虚设)。
//
// 匹配特征:进程名 easytier-core 且命令行含本节点 rpc-portal 地址 —— 只会命中
// 本平台托管的数据面子进程,不碰任何其他进程。
package easytier

import (
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// KillStaleByRPCPortal 终止命令行同时含 easytier-core 与指定 rpc-portal 的进程
// (不含自身)。返回清理数量;无匹配/读不了 /proc 一律安静返回。
func KillStaleByRPCPortal(rpcAddr string) int {
	if rpcAddr == "" {
		return 0
	}
	self := os.Getpid()
	killed := 0
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		cmd := strings.ReplaceAll(string(raw), "\x00", " ")
		if !strings.Contains(cmd, "easytier-core") || !strings.Contains(cmd, rpcAddr) {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
			killed++
			log.Printf("[easytier] 清理孤儿子进程 pid=%d(占位 %s)", pid, rpcAddr)
		}
	}
	return killed
}
