//go:build darwin

// stale_darwin.go 清理孤儿 easytier-core(语义同 stale_linux;macOS 无 /proc,
// 用 ps 取全量命令行匹配)。孤儿来源:自更新 re-exec 不执行清理、进程崩溃、强杀。
package easytier

import (
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// KillStaleByRPCPortal 终止命令行同时含 easytier-core 与指定 rpc-portal 的进程
// (不含自身)。返回清理数量;ps 查询失败安静返回(不阻塞拉起流程)。
func KillStaleByRPCPortal(rpcAddr string) int {
	if rpcAddr == "" {
		return 0
	}
	self := os.Getpid()
	out, err := exec.Command("ps", "ax", "-o", "pid=,command=").Output()
	if err != nil {
		return 0
	}
	killed := 0
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == self {
			continue
		}
		cmd := strings.Join(fields[1:], " ")
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
