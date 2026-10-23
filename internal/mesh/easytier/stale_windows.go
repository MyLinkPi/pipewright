//go:build windows

// stale_windows.go 清理孤儿 easytier-core.exe(语义同 stale_linux;Windows 节点
// 用 CIM 查询命令行匹配)。孤儿来源:自更新 re-exec 不执行清理、进程崩溃、强杀。
package easytier

import (
	"log"
	"os/exec"
	"strconv"
	"strings"
)

// FindStaleByRPCPortal 查询(不杀)命令行同时含 easytier-core 与指定 rpc-portal
// 的进程,供收养比对;查询失败安静返回空。输出行 pid + tab + 完整命令行。
func FindStaleByRPCPortal(rpcAddr string) []StaleProcess {
	if rpcAddr == "" {
		return nil
	}
	script := `Get-CimInstance Win32_Process -Filter "Name='easytier-core.exe'" |` +
		` Where-Object { $_.CommandLine -like '*easytier-core*' -and $_.CommandLine -like '*` + rpcAddr + `*' } |` +
		` ForEach-Object { Write-Output ($_.ProcessId.ToString() + [char]9 + $_.CommandLine) }`
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	var found []StaleProcess
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		pid, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		found = append(found, StaleProcess{PID: pid, Args: splitCmdline(parts[1])})
	}
	return found
}

// KillStaleByRPCPortal 终止命令行同时含 easytier-core 与指定 rpc-portal 的进程。
// 返回清理数量;查询失败安静返回(不阻塞拉起流程)。
func KillStaleByRPCPortal(rpcAddr string) int {
	if rpcAddr == "" {
		return 0
	}
	// CommandLine LIKE 匹配;PowerShell 单引号串内不含用户可控内容(rpcAddr 为本机地址)。
	script := `Get-CimInstance Win32_Process -Filter "Name='easytier-core.exe'" |` +
		` Where-Object { $_.CommandLine -like '*easytier-core*' -and $_.CommandLine -like '*` + rpcAddr + `*' } |` +
		` ForEach-Object { Stop-Process -Id $_.ProcessId -Force; Write-Output $_.ProcessId }`
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).Output()
	if err != nil && len(out) == 0 {
		return 0
	}
	killed := 0
	for _, f := range strings.Fields(string(out)) {
		if strings.TrimSpace(f) != "" {
			killed++
		}
	}
	if killed > 0 {
		log.Printf("[easytier] 清理孤儿子进程 x%d(占位 %s)", killed, rpcAddr)
	}
	return killed
}
