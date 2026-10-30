//go:build !windows

// local_other.go 是非 Windows(Linux/macOS)的 LocalActions 实现:服务安装/启停/卸载与
// 自启经平台服务管理器(systemd/launchd)完成,升级以独立子进程异步执行(界面轮询状态)。
//
// 权限模型:非 Windows 无 UAC 提权弹窗,界面进程需以 root 运行(sudo pwclient ui)方能
// 操作服务/升级;否则这些动作返回"需要 root"错误,由界面提示。打开日志/数据目录、退出
// 界面无需特权。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/huangchengsir/pipewright/internal/pwclient/uiserver"
	"github.com/huangchengsir/pipewright/internal/version"
)

type localActions struct{}

func newLocalActions(_ *core) uiserver.LocalActions { return &localActions{} }

func (l *localActions) Info(context.Context) (uiserver.LocalInfo, error) {
	raw := serviceQueryState()
	state := raw
	switch raw {
	case "RUNNING", "STARTING", "STOPPING", "STOPPED", "NOT_INSTALLED", "UNKNOWN":
		// 原样透出。
	default:
		state = "UNKNOWN"
	}
	installed := raw == "RUNNING" || raw == "STARTING" || raw == "STOPPING" || raw == "STOPPED"
	return uiserver.LocalInfo{
		Platform:          runtime.GOOS,
		Version:           version.Version,
		DataDir:           dataDir(),
		ConfigFile:        filepath.Join(dataDir(), "config.yaml"),
		LogFile:           filepath.Join(dataDir(), "logs", "pwclient.log"),
		Service:           uiserver.ServiceState{Installed: installed, State: state},
		AutoStart:         manager().autoStart(),
		CanInstallService: manager().available(),
	}, nil
}

func (l *localActions) InstallService(context.Context) error { return installService() }

func (l *localActions) ServiceAction(_ context.Context, action string) error {
	switch action {
	case "start":
		return startService()
	case "stop":
		return stopService()
	case "uninstall":
		return uninstallService()
	default:
		return fmt.Errorf("未知服务动作:%s", action)
	}
}

func (l *localActions) SetAutoStart(_ context.Context, enabled bool) error {
	if !manager().available() {
		return uiserver.ErrUnsupported
	}
	return manager().setAutoStart(enabled)
}

// Upgrade 以独立子进程异步执行升级(界面轮询 upgrade-status.json;与 Windows 同款模型)。
func (l *localActions) Upgrade(_ context.Context, tag string, includeUnstable bool) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位自身路径失败:%w", err)
	}
	// 已装服务时升级须 root:提前同步拦截。否则子进程在写任何状态前就退出,而其输出
	// 已被丢弃,界面只会看到"已启动升级"却永远等不到进度(假成功)。
	if p, _ := manager().binaryPath(); p != "" && !isElevated() {
		return errors.New("升级已安装的服务需要 root 权限:请以 sudo 运行界面进程(或 sudo pwclient upgrade)")
	}
	args := []string{"upgrade"}
	if trimmed := strings.TrimSpace(tag); trimmed != "" && trimmed != "latest" {
		args = append(args, "--tag", trimmed)
	}
	if includeUnstable {
		args = append(args, "--unstable")
	}
	cmd := exec.Command(self, args...)
	// 自建会话脱离界面进程组:升级替换自身后子进程仍能继续,且终端关闭的组信号
	// (SIGHUP)不会打断停服务→替换→重启的流程;输出丢弃(进度经状态文件呈现)。
	detachChild(cmd)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动升级进程失败:%w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
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

// SetTheme 非 Windows 无原生标题栏可同步(界面跑在系统浏览器里),空操作;主题已由 uiserver 落盘。
func (l *localActions) SetTheme(context.Context, string) error { return nil }

// openPath 用系统默认程序打开文件或目录(后台收尸,避免长驻进程累积僵尸)。
func openPath(path string) error {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	c := exec.Command(cmd, path)
	if err := c.Start(); err != nil {
		return fmt.Errorf("打开 %s 失败:%w", path, err)
	}
	go func() { _ = c.Wait() }()
	return nil
}
