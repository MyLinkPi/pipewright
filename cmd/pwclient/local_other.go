//go:build !windows

// local_other.go 是非 Windows 的/LocalActions 实现:没有系统服务/自启/提权升级,
// 只支持查询本机信息、打开日志、打开数据目录与退出界面。
package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/huangchengsir/pipewright/internal/pwclient/uiserver"
	"github.com/huangchengsir/pipewright/internal/version"
)

type localActions struct{}

func newLocalActions(_ *core) uiserver.LocalActions { return &localActions{} }

func (l *localActions) Info(context.Context) (uiserver.LocalInfo, error) {
	return uiserver.LocalInfo{
		Platform:          runtime.GOOS,
		Version:           version.Version,
		DataDir:           dataDir(),
		ConfigFile:        filepath.Join(dataDir(), "config.yaml"),
		LogFile:           filepath.Join(dataDir(), "logs", "pwclient.log"),
		Service:           uiserver.ServiceState{Installed: false, State: "UNSUPPORTED"},
		AutoStart:         false,
		CanInstallService: false,
	}, nil
}

func (l *localActions) InstallService(context.Context) error { return uiserver.ErrUnsupported }

func (l *localActions) ServiceAction(context.Context, string) error { return uiserver.ErrUnsupported }

func (l *localActions) SetAutoStart(context.Context, bool) error { return uiserver.ErrUnsupported }

func (l *localActions) Upgrade(context.Context, string) error { return uiserver.ErrUnsupported }

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
