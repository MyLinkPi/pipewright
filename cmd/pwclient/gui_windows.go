//go:build windows

// gui_windows.go 是主窗口:WebView2 内嵌窗口(纯 Go,无需 CGO);WebView2 Runtime 缺失时
// 回退 Edge --app 窗口,再退回系统默认浏览器。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	webview2 "github.com/jchv/go-webview2"
)

var (
	winMu   sync.Mutex
	winBusy bool
)

// openMainWindow 打开或唤起主窗口。窗口已存在时聚焦;创建中则忽略重复请求。
func openMainWindow(url string) {
	if findMainWindow() != 0 {
		activateExistingInstance()
		return
	}
	winMu.Lock()
	if winBusy {
		winMu.Unlock()
		return
	}
	winBusy = true
	winMu.Unlock()

	go func() {
		runtime.LockOSThread()
		defer func() {
			winMu.Lock()
			winBusy = false
			winMu.Unlock()
		}()
		w := webview2.NewWithOptions(webview2.WebViewOptions{
			AutoFocus: true,
			WindowOptions: webview2.WindowOptions{
				Title:  mainWindowTitle,
				Width:  1120,
				Height: 760,
				IconId: 1,
				Center: true,
			},
		})
		if w == nil {
			openInBrowser(url)
			return
		}
		defer w.Destroy()
		w.Navigate(url)
		w.Run()
	}()
}

// openInBrowser 依次尝试 Edge --app 窗口与系统默认浏览器(后台收尸,避免僵尸)。
func openInBrowser(url string) {
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Microsoft", "Edge", "Application", "msedge.exe"),
	}
	for _, exe := range candidates {
		if _, err := os.Stat(exe); err != nil {
			continue
		}
		if cmd := exec.Command(exe, "--app="+url, "--window-size=1120,760"); cmd.Start() == nil {
			go func() { _ = cmd.Wait() }()
			return
		}
	}
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "无法打开界面,请手动访问:", url)
		return
	}
	go func() { _ = cmd.Wait() }()
}
