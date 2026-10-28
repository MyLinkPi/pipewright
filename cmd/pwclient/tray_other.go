//go:build !windows

// tray_other.go 是 Linux/macOS 的桌面形态:前台运行核心 + uiserver,用系统浏览器打开界面。
// 不做系统服务/托盘/开机自启(仅 Windows 提供)。
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

// localCoreMode 非 Windows 在 UI 进程内前台运行核心(console 装配)。
func localCoreMode() string { return "console" }

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

// quitUI 响应界面退出请求(仅关 UI 与前台核心,不影响任何后台服务——非 Windows 无服务)。
func quitUI() {
	select {
	case <-quitCh:
	default:
		close(quitCh)
	}
}
