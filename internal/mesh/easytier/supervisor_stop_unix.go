//go:build !windows

// supervisor_stop_unix.go:温和退出通道 —— SIGINT 交子进程自行收尾(不直接强杀)。
// 调用方(Supervisor.Stop)限时等待子进程退出,超时后兜底强杀 —— 不依赖
// exec.CommandContext 的异步强杀(宿主主进程退出后该 goroutine 已不存在)。
package easytier

import "os"

func stopSignal(p *os.Process) error { return p.Signal(os.Interrupt) }
