//go:build windows

// console_windows.go 把控制台输出代码页切到 UTF-8(中文输出在默认 GBK 代码页的终端
// 里会乱码;GUI 形态无控制台,设置无害失败)。输入侧一并切换,保证交互提示可读。
package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func init() {
	_ = windows.SetConsoleOutputCP(65001)
	_ = windows.SetConsoleCP(65001)
}

// attachConsoleProc 是 kernel32!AttachConsole(x/sys 未封装,ATTACH_PARENT_PROCESS = -1)。
var attachConsoleProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")

// attachParentConsole 把 GUI 子系统(-H=windowsgui,双击不弹控制台窗口)程序的文本
// 输出接回调用方控制台,使 status/doctor/version 等子命令在终端仍可正常打印。
// 已有有效标准输出(重定向到管道/文件)时不动,避免抢走管道输出;双击启动无父控制台
// 时静默跳过。
func attachParentConsole() {
	if h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE); err == nil &&
		h != 0 && h != windows.InvalidHandle {
		return
	}
	if ret, _, _ := attachConsoleProc.Call(^uintptr(0)); ret == 0 { // ATTACH_PARENT_PROCESS
		return
	}
	out, err := windows.CreateFile(windows.StringToUTF16Ptr("CONOUT$"),
		windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(out), "CONOUT$")
	os.Stdout = f
	os.Stderr = f
}
