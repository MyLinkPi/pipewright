//go:build !windows

// svc_other.go 是非 Windows(Linux/macOS)的服务编排层:与 Windows 侧同名函数签名对齐,
// 供 upgrade_other.go / local_other.go / doctor 复用。真正的服务注册交由平台服务管理器
// (Linux systemd、macOS launchd)完成 —— 具体实现按 build tag 分文件(systemd/launchd)。
//
// 与 Windows 一致的落位约定:服务二进制安放于稳定目录(installDir),开机自启由服务
// 管理器托管;数据面核心与 IPC 由 runCore 装配,和 console 形态一致(进程即服务,收
// SIGTERM 优雅退出)。
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// serviceName 是服务标识(Linux unit 名 / macOS LaunchDaemon label 的基础名)。
const serviceName = "pipewright-client"

// defaultInstallDir 是服务副本的安放目录(Linux/macOS 通用;/opt 为宿主可变稳定位置,
// 不受 macOS SIP 对 /usr 的密封限制)。
const defaultInstallDir = "/opt/pipewright-client"

// serviceManager 是平台服务管理器的最小契约(按 OS 实现;见 svc_systemd.go / svc_launchd.go)。
type serviceManager interface {
	// available 报告本机是否具备可用的服务管理器(systemd/launchd 在位)。
	available() bool
	// install 依据 exePath 写 unit/plist 并设为开机自启(不含复制文件,复制由上层完成)。
	install(exePath string) error
	start() error
	stop() error
	uninstall() error
	// state 返回 RUNNING/STARTING/STOPPED/NOT_INSTALLED/UNKNOWN(容错,不返回错误)。
	state() string
	// binaryPath 返回服务配置的 exe 路径;未安装返回空串;查询失败上抛错误。
	binaryPath() (string, error)
	// autoStart 报告是否开机自启;systemd=is-enabled,launchd=RunAtLoad。
	autoStart() bool
	setAutoStart(on bool) error
}

// manager 惰性获取当前平台的服务管理器(单一实例足够:命令均为短生命周期进程)。
func manager() serviceManager { return newServiceManager() }

// installDir 是服务副本的安放目录(稳定位置;从下载/解压目录直接安装时,服务不能
// 指向用户随手清理的临时路径)。
func installDir() string { return defaultInstallDir }

// stageToInstallDir 把 exe 与随包 bin/(EasyTier)、licenses/、LICENSE 复制到安放目录,
// 返回服务应指向的路径。已在安放目录运行则原样返回(升级/重装幂等)。
func stageToInstallDir(exe string) (string, error) {
	dest := installDir()
	if filepath.Dir(exe) == dest {
		return exe, nil
	}
	if err := os.MkdirAll(filepath.Join(dest, "bin"), 0o755); err != nil {
		return "", fmt.Errorf("创建安放目录失败:%w", err)
	}
	target := filepath.Join(dest, filepath.Base(exe))
	if err := copyFileAny(exe, target); err != nil {
		return "", fmt.Errorf("复制程序到 %s 失败:%w", dest, err)
	}
	// 随包 EasyTier:服务进程靠 exe 同级 bin/ 定位(离线开箱即连的关键)。
	srcDir := filepath.Dir(exe)
	for _, sub := range []string{"bin", "licenses"} {
		entries, err := os.ReadDir(filepath.Join(srcDir, sub))
		if err != nil {
			continue // 无该子目录(如手工放置)则跳过
		}
		if err := os.MkdirAll(filepath.Join(dest, sub), 0o755); err != nil {
			return "", fmt.Errorf("创建 %s 目录失败:%w", sub, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if err := copyFileAny(filepath.Join(srcDir, sub, e.Name()), filepath.Join(dest, sub, e.Name())); err != nil {
				return "", fmt.Errorf("复制 %s 失败:%w", e.Name(), err)
			}
		}
	}
	if fileExistsAny(filepath.Join(srcDir, "LICENSE")) {
		_ = copyFileAny(filepath.Join(srcDir, "LICENSE"), filepath.Join(dest, "LICENSE"))
	}
	return target, nil
}

// copyFileAny 复制单个文件(覆盖已存在的旧副本,保留可执行位)。
func copyFileAny(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm()|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func fileExistsAny(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// serviceCmd 处理 `pwclient service <verb>`(非 Windows:run 前台常驻;其余经服务管理器)。
func serviceCmd(args []string) {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	var err error
	switch verb {
	case "run":
		err = runService()
	case "install":
		err = installService()
	case "start":
		err = startService()
	case "stop":
		err = stopService()
	case "uninstall":
		err = uninstallService()
	default:
		fmt.Fprintln(os.Stderr, "用法: pwclient service run|install|start|stop|uninstall")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "服务操作失败:", err)
		os.Exit(1)
	}
}

// runService 以前台进程形态常驻(systemd/launchd 拉起;收 SIGTERM/SIGINT 优雅退出)。
func runService() error {
	c, err := runCore("service")
	if err != nil {
		log.Printf("[pwclient] 服务核心装配失败:%v", err)
		return err
	}
	waitInterrupt()
	c.cancel()
	select {
	case <-c.stopWait:
	case <-time.After(15 * time.Second):
		log.Println("[pwclient] 等待核心退出超时,强制退出")
	}
	return nil
}

// requireRoot 校验 root 权限(安装/启停/卸载服务需要)。
func requireRoot(what string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s需要 root 权限:请以 sudo 运行 `pwclient service ...`", what)
	}
	return nil
}

// installService 复制程序到安放目录 + 注册服务(开机自启)+ 立即启动。幂等:先卸后装。
func installService() error {
	if err := requireRoot("安装服务"); err != nil {
		return err
	}
	m := manager()
	if !m.available() {
		return fmt.Errorf("当前平台未检测到可用的服务管理器(Linux 需 systemd,macOS 需 launchd)")
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位自身路径失败:%w", err)
	}
	// 已存在先卸载(重装换二进制),避免 unit/plist 残留。
	if p, _ := m.binaryPath(); p != "" {
		_ = m.stop()
		_ = m.uninstall()
	}
	target, err := stageToInstallDir(exe)
	if err != nil {
		return err
	}
	if err := m.install(target); err != nil {
		return fmt.Errorf("注册服务失败:%w", err)
	}
	if err := m.start(); err != nil {
		return fmt.Errorf("服务已注册但启动失败:%w", err)
	}
	fmt.Printf("服务已安装并启动(开机自启),位于 %s。\n", target)
	return nil
}

// startService 启动服务。
func startService() error {
	if err := requireRoot("启动服务"); err != nil {
		return err
	}
	return manager().start()
}

// stopService 停止服务(未安装视为成功,便于清理残留)。
func stopService() error {
	if err := requireRoot("停止服务"); err != nil {
		return err
	}
	m := manager()
	if p, _ := m.binaryPath(); p == "" {
		fmt.Println("服务未安装。")
		return nil
	}
	if err := m.stop(); err != nil {
		return err
	}
	fmt.Println("服务已停止。")
	return nil
}

// uninstallService 停止 + 删除 unit/plist。
func uninstallService() error {
	if err := requireRoot("卸载服务"); err != nil {
		return err
	}
	m := manager()
	_ = m.stop()
	if err := m.uninstall(); err != nil {
		return err
	}
	fmt.Println("服务已卸载(安放目录文件保留,如需清理请手动删除 " + installDir() + ")。")
	return nil
}

// serviceQueryState 返回服务状态(doctor 用)。
func serviceQueryState() string { return manager().state() }

// doctorPlatformExtra 是 doctor 的非 Windows 扩展检查项(服务安装状态)。
func doctorPlatformExtra(check checkFn) {
	state := serviceQueryState()
	check("服务安装", state == "RUNNING", state, "sudo pwclient service install")
}

// serviceBinaryPath 返回服务配置里的 exe 路径(未安装返回空串;查询失败上抛错误)。
func serviceBinaryPath() (string, error) { return manager().binaryPath() }

// serviceRunning 报告服务当前是否 RUNNING(未安装/查询失败视为 false)。
func serviceRunning() bool { return manager().state() == "RUNNING" }

// stopServiceAndWait 停止服务并等待退出(升级替换文件前必须确保进程已退出)。未安装视为成功。
func stopServiceAndWait(timeout time.Duration) error {
	m := manager()
	if p, _ := m.binaryPath(); p == "" {
		return nil // 未安装
	}
	if err := m.stop(); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st := m.state(); st != "RUNNING" && st != "STOPPING" && st != "STARTING" {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("等待服务停止超时(%s)", timeout)
}

// isElevated 报告当前进程是否具备 root 权限(非 Windows 服务操作需 root)。
func isElevated() bool { return os.Geteuid() == 0 }
