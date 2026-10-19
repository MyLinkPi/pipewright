//go:build windows

// console_windows.go 把控制台输出代码页切到 UTF-8(中文输出在默认 GBK 代码页的终端
// 里会乱码;GUI 形态无控制台,设置无害失败)。输入侧一并切换,保证交互提示可读。
package main

import "golang.org/x/sys/windows"

func init() {
	_ = windows.SetConsoleOutputCP(65001)
	_ = windows.SetConsoleCP(65001)
}
