//go:build windows

// local_windows.go 是 LocalActions 的 Windows 实现:服务安装/启停/卸载、开机自启、
// 提权升级、打开日志/数据目录、退出界面。所有需要管理员的操作经 UAC 提权启动本程序子命令。
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/huangchengsir/pipewright/internal/pwclient"
	"github.com/huangchengsir/pipewright/internal/pwclient/uiserver"
	"github.com/huangchengsir/pipewright/internal/version"
)

type localActions struct{}

func newLocalActions(_ *core) uiserver.LocalActions { return &localActions{} }

func (l *localActions) Info(context.Context) (uiserver.LocalInfo, error) {
	raw := serviceQueryState()
	state := raw
	switch raw {
	case "RUNNING", "STARTING", "STOPPING", "STOPPED", "PAUSED":
		// 已安装(状态原样透出)。
	case "NOT_INSTALLED":
		state = "NOT_INSTALLED"
	default:
		state = "UNKNOWN"
	}
	installed := raw == "RUNNING" || raw == "STARTING" || raw == "STOPPING" || raw == "STOPPED" || raw == "PAUSED"
	return uiserver.LocalInfo{
		Platform:          "windows",
		Version:           version.Version,
		DataDir:           dataDir(),
		ConfigFile:        filepath.Join(dataDir(), "config.yaml"),
		LogFile:           filepath.Join(dataDir(), "logs", "pwclient.log"),
		Service:           uiserver.ServiceState{Installed: installed, State: state},
		AutoStart:         autoStartEnabled(),
		CanInstallService: true,
	}, nil
}

func (l *localActions) InstallService(context.Context) error {
	return runElevated("service install")
}

func (l *localActions) ServiceAction(_ context.Context, action string) error {
	switch action {
	case "start", "stop", "uninstall":
		return runElevated("service " + action)
	default:
		return fmt.Errorf("未知服务动作:%s", action)
	}
}

func (l *localActions) SetAutoStart(_ context.Context, enabled bool) error {
	if err := setAutoStart(enabled); err != nil {
		return err
	}
	return nil
}

func (l *localActions) Upgrade(_ context.Context, tag string) error {
	args := "upgrade"
	if trimmed := strings.TrimSpace(tag); trimmed != "" {
		// 双重防线(uiserver 层亦校验):tag 拼入提权命令行,白名单校验拒一切非法字符。
		if err := pwclient.ValidateTag(trimmed); err != nil {
			return err
		}
		args += ` --tag "` + trimmed + `"`
	}
	return runElevated(args)
}

func (l *localActions) Quit(context.Context) error {
	quitUI()
	return nil
}

func (l *localActions) OpenLog(context.Context) error {
	return openPath(filepath.Join(dataDir(), "logs", "pwclient.log"))
}

func (l *localActions) RevealDataDir(context.Context) error {
	return openPath(dataDir())
}

// SetTheme 同步主窗口原生标题栏深/浅色。窗口未打开时静默返回(下次打开按持久化主题初始化)。
func (l *localActions) SetTheme(_ context.Context, theme string) error {
	applyTitleBarDark(uintptr(findMainWindow()), theme == "dark")
	return nil
}

// runElevated 经 UAC 提权启动本程序子命令。
func runElevated(args string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位自身路径失败:%w", err)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	arg, _ := windows.UTF16PtrFromString(args)
	cwd, _ := windows.UTF16PtrFromString("")
	if err := windows.ShellExecute(0, verb, file, arg, cwd, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("提权启动失败(用户取消或被策略拦截):%w", err)
	}
	return nil
}

// openPath 用系统默认程序打开文件或目录(后台收尸,避免长驻进程累积僵尸)。
func openPath(path string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("打开 %s 失败:%w", path, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
