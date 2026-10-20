//go:build windows

// svc_windows.go 是 Windows 服务形态:经 golang.org/x/sys/windows/svc 与 SCM 对接
// (LocalSystem 自启,开机即连,无需 UAC);安装/启停经服务管理器 API(需管理员,
// 由托盘经 runas 提权调起)。核心运行时与 IPC 由 runCore 装配,与 console 形态一致。
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// serviceName 是 Windows 服务的注册名。
const serviceName = "pipewright-client"

// serviceCmd 处理 `pwclient service <verb>`。
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
	case "stop":
		err = stopService()
	case "uninstall":
		err = uninstallService()
	default:
		fmt.Fprintln(os.Stderr, "用法: pwclient service run|install|stop|uninstall")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "服务操作失败:", err)
		os.Exit(1)
	}
}

// pwService 实现 svc.Handler:装配失败退非零;Stop/Shutdown 时收尾退出。
type pwService struct{}

func (m *pwService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending, WaitHint: 20000}
	c, err := runCore("service")
	if err != nil {
		log.Printf("[pwclient] 服务核心装配失败:%v", err)
		return false, 1
	}
	changes <- svc.Status{State: svc.Running, Accepts: accepts}
	for req := range r {
		switch req.Cmd {
		case svc.Interrogate:
			changes <- req.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending, WaitHint: 20000}
			c.cancel()
			time.Sleep(1 * time.Second) // 给监督器停子进程/IPC 移除 ipc.json 留窗口
			return false, 0
		}
	}
	return false, 0
}

// runService 是 SCM 入口。
func runService() error { return svc.Run(serviceName, &pwService{}) }

// connectSCM 连接服务管理器(失败通常是权限不足,给出人读提示)。
func connectSCM() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("连接服务管理器失败(需以管理员运行):%w", err)
	}
	return m, nil
}

// installDir 是服务副本的安放目录(稳定位置;从压缩包/下载目录直接安装时,服务
// 二进制不能指向用户随手清理的临时路径)。
func installDir() string {
	return filepath.Join(os.Getenv("ProgramFiles"), "Pipewright Client")
}

// stageToProgramFiles 把 exe 与随包 bin/(EasyTier)复制到安放目录,返回服务应指向
// 的路径。已在安放目录运行则原样返回(升级/重装幂等)。
func stageToProgramFiles(exe string) (string, error) {
	dest := installDir()
	if strings.EqualFold(filepath.Dir(exe), dest) {
		return exe, nil
	}
	if err := os.MkdirAll(filepath.Join(dest, "bin"), 0o755); err != nil {
		return "", fmt.Errorf("创建安放目录失败:%w", err)
	}
	target := filepath.Join(dest, filepath.Base(exe))
	if err := copyFile(exe, target); err != nil {
		return "", fmt.Errorf("复制程序到 %s 失败:%w", dest, err)
	}
	// 随包 EasyTier:服务进程靠 exe 同级 bin/ 定位(离线开箱即连的关键)。
	binSrc := filepath.Join(filepath.Dir(exe), "bin")
	if entries, err := os.ReadDir(binSrc); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if err := copyFile(filepath.Join(binSrc, e.Name()), filepath.Join(dest, "bin", e.Name())); err != nil {
				return "", fmt.Errorf("复制 EasyTier 二进制失败:%w", err)
			}
		}
	}
	return target, nil
}

// copyFile 复制单个文件(覆盖已存在的旧副本)。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// installService 创建(自启,LocalSystem)+ 描述 + 立即启动。幂等:已存在先删除重建。
// 服务二进制安放于 %ProgramFiles%\Pipewright Client(与运行位置无关):先停旧服务
// 再复制(旧副本运行中会被锁),非管理员在复制前即被 SCM 权限拦下。
func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位自身路径失败:%w", err)
	}
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer func() { _ = m.Disconnect() }()

	if s, oerr := m.OpenService(serviceName); oerr == nil {
		_, _ = s.Control(svc.Stop)
		time.Sleep(1 * time.Second)
		_ = s.Delete()
		_ = s.Close()
		time.Sleep(1 * time.Second)
	}

	exe, err = stageToProgramFiles(exe)
	if err != nil {
		return err
	}
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		StartType:    mgr.StartAutomatic,
		DisplayName:  "Pipewright Cluster Client",
		Description:  "pipewright 集群客户端(EasyTier 组网与网段路由)",
		Dependencies: []string{"Tcpip", "Dnscache"},
	}, "service", "run")
	if err != nil {
		return fmt.Errorf("创建服务失败:%w", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Start(); err != nil {
		return fmt.Errorf("服务已创建但启动失败:%w", err)
	}
	fmt.Printf("服务已安装并启动(开机自启),位于 %s。\n", exe)
	return nil
}

// stopService 停止服务(未安装视为成功,便于清理残留)。
func stopService() error {
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(serviceName)
	if err != nil {
		fmt.Println("服务未安装。")
		return nil
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("停止服务失败:%w", err)
	}
	fmt.Println("服务已停止。")
	return nil
}

// uninstallService 停止 + 删除。
func uninstallService() error {
	if err := stopService(); err != nil {
		return err
	}
	out, err := exec.Command("sc.exe", "delete", serviceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("删除服务失败:%v(%s)", err, firstLine(string(out)))
	}
	fmt.Println("服务已卸载。")
	return nil
}

// serviceQueryState 返回服务状态(RUNNING/STOPPED/NOT_INSTALLED;doctor 用,容错)。
func serviceQueryState() string {
	m, err := mgr.Connect()
	if err != nil {
		return "UNKNOWN"
	}
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return "NOT_INSTALLED"
	}
	defer func() { _ = s.Close() }()
	q, err := s.Query()
	if err != nil {
		return "UNKNOWN"
	}
	switch q.State {
	case svc.Running:
		return "RUNNING"
	case svc.StartPending, svc.ContinuePending:
		return "STARTING"
	case svc.StopPending:
		return "STOPPING"
	case svc.Paused:
		return "PAUSED"
	default:
		return "STOPPED"
	}
}

// doctorPlatformExtra 是 doctor 的 Windows 扩展检查项(服务安装状态)。
func doctorPlatformExtra(check checkFn) {
	state := serviceQueryState()
	check("服务安装", state == "RUNNING", state, "pwclient service install")
}
