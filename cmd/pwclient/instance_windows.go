// instance_windows.go 是托盘 GUI 的单实例守卫:登录自启 + 手动双击并存时,
// 后起的进程唤起已有实例的主窗口后退出,避免出现两个托盘图标。
//
//go:build windows

package main

import (
	"errors"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

const (
	// Local\ 前缀 = 每个登录会话一份(托盘是会话级 UI;服务/console 形态不经此守卫)。
	instanceMutexName = `Local\pipewright-client-tray`
	mainWindowTitle   = "pipewright 集群客户端"
)

// instanceMutex 持有互斥句柄到进程退出(无需显式关闭)。
var instanceMutex windows.Handle

// ensureSingleInstance 保证托盘 GUI 单实例:已有实例时唤起其主窗口并返回 false。
// 无法判定(罕见)时放行 —— 宁可重复也不阻断。
func ensureSingleInstance() bool {
	h, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(instanceMutexName))
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return true
	}
	instanceMutex = h
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		activateExistingInstance()
		return false
	}
	return true
}

// activateExistingInstance 唤起已有实例的主窗口(可能正隐藏在托盘)。
func activateExistingInstance() {
	title, err := windows.UTF16PtrFromString(mainWindowTitle)
	if err != nil {
		return
	}
	if h := win.FindWindow(nil, title); h != 0 {
		win.ShowWindow(h, win.SW_RESTORE)
		win.SetForegroundWindow(h)
	}
}
