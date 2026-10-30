//go:build !windows

// tray_other.go 是 Linux/macOS 的桌面形态:uiserver + 系统浏览器打开界面。核心优先复用
// 后台服务(systemd/launchd,经 IPC);未装/未运行时在 UI 进程内前台跑核心(console)。
// 托盘仍仅 Windows 提供。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
)

// quitCh 接收界面「退出客户端」请求(/local/quit)。
var quitCh = make(chan struct{})

// localCoreMode 服务已持有(或正在接管)数据面核心时不启核心(与 Windows 一致,避免双
// 实例争用 IPC/网卡,界面经 IPC 复用服务的数据面);否则在 UI 进程内前台运行核心
// (console 装配)。STARTING 也算服务持有:此刻界面抢先起核心,服务随即启动完成就会
// 撞出双实例。
func localCoreMode() string {
	switch manager().state() {
	case "RUNNING", "STARTING":
		return ""
	}
	return "console"
}

// preflightDesktop 非 Windows 无单实例限制。
func preflightDesktop() bool { return true }

// runDesktop 打开浏览器并阻塞到退出信号(noOpen 时仅等待,URL 已由 uiMain 打印)。
func runDesktop(url string, noOpen bool) {
	if !noOpen {
		openBrowser(url)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	select {
	case <-sig:
	case <-quitCh:
	}
}

// openBrowser 用系统默认浏览器打开地址(失败时打印手动访问提示;后台收尸避免僵尸)。
func openBrowser(url string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	c := exec.Command(cmd, url)
	if err := c.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "无法自动打开浏览器,请手动访问:", url)
		return
	}
	go func() { _ = c.Wait() }()
}

// quitUI 响应界面退出请求(只关本 UI 与其前台核心;后台服务不受影响)。
func quitUI() {
	select {
	case <-quitCh:
	default:
		close(quitCh)
	}
}
