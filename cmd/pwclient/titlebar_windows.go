//go:build windows

// titlebar_windows.go 同步原生窗口标题栏配色。
//
// WebView2 宿主是一个普通 Win32 窗口(WS_OVERLAPPEDWINDOW),标题栏由系统(DWM)绘制,
// 不随页面 CSS 的深/浅色变化 —— 这正是「深色模式下标题栏仍是浅色」的根因。解决办法是
// 设置 DWMWA_USE_IMMERSIVE_DARK_MODE 属性,让 DWM 用沉浸式深色绘制标题栏。
package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// dwmAUseImmersiveDarkMode:Windows 10 2004(19041)+ 与 Windows 11 的正式编号。
	dwmAUseImmersiveDarkMode = 20
	// dwmAUseImmersiveDarkModeOld:更早构建(1809/1903)沿用的编号。
	dwmAUseImmersiveDarkModeOld = 19
)

var (
	dwmapi                    = windows.NewLazySystemDLL("dwmapi.dll")
	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
)

// applyTitleBarDark 切换指定窗口的标题栏深/浅色。hwnd=0 时静默返回(窗口尚未创建)。
// 老系统不支持该特性时,DwmSetWindowAttribute 返回非 S_OK,静默降级(不影响功能)。
func applyTitleBarDark(hwnd uintptr, dark bool) {
	if hwnd == 0 {
		return
	}
	var value int32
	if dark {
		value = 1
	}
	size := unsafe.Sizeof(value)
	// 优先用新编号;返回失败(HRESULT≠0)再回退旧编号,覆盖新旧 Windows 构建。
	if hr, _, _ := procDwmSetWindowAttribute.Call(
		hwnd, uintptr(dwmAUseImmersiveDarkMode), uintptr(unsafe.Pointer(&value)), size,
	); hr != 0 {
		_, _, _ = procDwmSetWindowAttribute.Call(
			hwnd, uintptr(dwmAUseImmersiveDarkModeOld), uintptr(unsafe.Pointer(&value)), size,
		)
	}
}
