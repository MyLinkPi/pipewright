//go:build linux

// svc_systemd.go 是 Linux 的服务管理器实现:systemd unit(/etc/systemd/system/
// pipewright-client.service)。进程即服务(Type=simple),停止经 systemctl stop 发
// SIGTERM,由 runService 的 waitInterrupt 优雅收尾(等托管 EasyTier 子进程回收)。
//
// 非 systemd 发行版(纯 OpenWrt/部分容器)检测失败时 available()=false,`service install`
// 会明确拒绝并提示前台形态(console/ui)可用。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	unitName = serviceName + ".service"
	unitPath = "/etc/systemd/system/" + unitName
)

// systemdManager 经 systemctl 管理 unit。
type systemdManager struct{}

func newServiceManager() serviceManager { return systemdManager{} }

// available systemd 在位判定:ctl 可执行 + /run/systemd/system 存在(容器/非 systemd 启动
// 时该目录不存在,此时经 systemctl 操作必败,须提前拒绝而非误报权限问题)。
func (systemdManager) available() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	st, err := os.Stat("/run/systemd/system")
	return err == nil && st.IsDir()
}

func (systemdManager) install(exePath string) error {
	unit := fmt.Sprintf(`[Unit]
Description=Pipewright Cluster Client (EasyTier mesh)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s service run
Restart=on-failure
RestartSec=3
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
`, exePath)
	// 0644:普通用户 pwclient status/doctor 可读 unit 里的 ExecStart(定位安装目标)。
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败:%w", unitPath, err)
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	return systemctl("enable", unitName)
}

func (systemdManager) start() error { return systemctl("start", unitName) }

func (systemdManager) stop() error { return systemctl("stop", unitName) }

func (systemdManager) uninstall() error {
	// disable 失败(unit 已删)不阻断,以 rm 结果为准。
	_ = systemctl("disable", "--now", unitName)
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 %s 失败:%w", unitPath, err)
	}
	return systemctl("daemon-reload")
}

// unitExists 经 systemctl cat 判定 unit 是否已安装(不存在时 cat 返回非零)。
func (systemdManager) unitExists() bool {
	_, err := run("systemctl", "cat", unitName)
	return err == nil
}

// state 归一服务运行状态(RUNNING/STARTING/STOPPING/STOPPED/NOT_INSTALLED/UNKNOWN)。
func (m systemdManager) state() string {
	out, _ := run("systemctl", "is-active", unitName)
	switch word := strings.TrimSpace(string(out)); word {
	case "active":
		return "RUNNING"
	case "activating":
		return "STARTING"
	case "deactivating":
		return "STOPPING"
	case "inactive", "failed", "reloading":
		if m.unitExists() {
			return "STOPPED"
		}
		return "NOT_INSTALLED"
	default:
		// 空输出:ctl 不可用/异常。unit 不存在时按未安装,否则未知。
		if word == "" && !m.unitExists() {
			return "NOT_INSTALLED"
		}
		return "UNKNOWN"
	}
}

// binaryPath 从 unit 的 ExecStart 取 exe 路径(systemctl cat 覆盖 override/drop-in)。
// 未安装返回错误(上层据此判未安装)。
func (systemdManager) binaryPath() (string, error) {
	out, err := run("systemctl", "cat", unitName)
	if err != nil {
		return "", fmt.Errorf("服务未安装或无法读取配置:%v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); ok {
			v = strings.TrimLeft(v, "@+-: ")
			if fields := strings.Fields(v); len(fields) > 0 {
				return strings.Trim(fields[0], `"`), nil
			}
		}
	}
	return "", fmt.Errorf("unit %s 缺少 ExecStart", unitName)
}

func (systemdManager) autoStart() bool {
	out, err := run("systemctl", "is-enabled", unitName)
	return err == nil && strings.TrimSpace(string(out)) == "enabled"
}

func (systemdManager) setAutoStart(on bool) error {
	if on {
		return systemctl("enable", unitName)
	}
	return systemctl("disable", unitName)
}

// systemctl 执行并聚合输出为人读错误。
func systemctl(args ...string) error {
	out, err := run("systemctl", args...)
	if err != nil {
		return fmt.Errorf("systemctl %s 失败:%v(%s)", strings.Join(args, " "), err, firstLine(string(out)))
	}
	return nil
}

// run 执行命令返回合并输出(stdout+stderr,便于错误聚合;systemctl 状态词走 stdout、
// 错误走 stderr,统一捕获后按内容判定)。
func run(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "SYSTEMCTL_SKIP_REDIRECT=1")
	return cmd.CombinedOutput()
}
