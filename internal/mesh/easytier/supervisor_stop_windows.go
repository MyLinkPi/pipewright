//go:build windows

// supervisor_stop_windows.go:Windows 无进程间信号(os.Process.Signal 仅支持 os.Kill,
// 发 os.Interrupt 只会报错)——旧实现在这里温和信号永远失败,兜底强杀又挂在脱离线程上,
// 宿主服务进程退出即丢失,easytier-core 成为孤儿继续占用二进制文件(升级替换失败根因)。
// Kill 在 Windows 上终止进程并回收其创建的 TUN 设备句柄,即为本地的"停止"语义。
package easytier

import "os"

func stopSignal(p *os.Process) error { return p.Kill() }
