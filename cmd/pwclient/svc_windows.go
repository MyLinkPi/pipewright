//go:build windows

// svc_windows.go 是 Windows 服务形态:经 golang.org/x/sys/windows/svc 与 SCM 对接
// (LocalSystem 自启,开机即连,无需 UAC);安装/启停经服务管理器 API(需管理员,
// 由托盘经 runas 提权调起)。核心运行时与 IPC 由 runCore 装配,与 console 形态一致。
package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/huangchengsir/pipewright/internal/pwclient"
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
			// 等核心真正退出(对账循环收尾会终止托管的 easytier-core 并确保回收)
			// 再交回 SCM:否则 SCM 报 Stopped 时子进程仍占用 bin/ 下二进制,升级替换失败。
			select {
			case <-c.stopWait:
			case <-time.After(15 * time.Second):
				log.Printf("[pwclient] 等待核心退出超时,服务仍按停止处理")
			}
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

// serviceBinaryPath 返回服务配置里的 exe 路径。未安装返回空串;查询失败(如普通
// 权限连不上 SCM)返回错误 —— 调用方不得把错误当作「未安装」。
func serviceBinaryPath() (string, error) {
	m, s, err := openServiceForQuery()
	if err != nil {
		return "", err
	}
	if s == nil {
		return "", nil // 未安装(scm 句柄已在 openServiceForQuery 内关闭)
	}
	defer func() { _ = s.Close() }()
	// 手工 Create 的 mgr.Mgr 不会自动断开 SCM:与 serviceRunning 同款自行 Disconnect,
	// 避免句柄挂到进程退出才回收。
	defer func() { _ = m.Disconnect() }()
	cfg, err := s.Config()
	if err != nil {
		return "", fmt.Errorf("读取服务配置失败:%w", err)
	}
	return pwclient.ParseServiceBinaryPath(cfg.BinaryPathName), nil
}

// openServiceForQuery 以查询所需的最小权限连接 SCM 并打开本服务:
// mgr.Connect/mgr.OpenService 申请 ALL_ACCESS,普通权限下必败 —— 旧实现把权限不足
// 误判为「服务未安装」,升级会绕过管理员校验去更新错误目标。这里用
// SC_MANAGER_CONNECT + SERVICE_QUERY_CONFIG,并区分「服务不存在」(返回 nil 服务)
// 与真错误(上抛,由调用方决定是否要求提权)。
func openServiceForQuery() (*mgr.Mgr, *mgr.Service, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, nil, fmt.Errorf("连接服务管理器失败(查询服务配置需要管理员权限):%w", err)
	}
	h, oerr := windows.OpenService(scm, windows.StringToUTF16Ptr(serviceName), windows.SERVICE_QUERY_CONFIG)
	if oerr != nil {
		_ = windows.CloseServiceHandle(scm)
		if errors.Is(oerr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, nil, nil // 未安装
		}
		return nil, nil, fmt.Errorf("打开服务失败:%w", oerr)
	}
	return &mgr.Mgr{Handle: scm}, &mgr.Service{Name: serviceName, Handle: h}, nil
}

// serviceRunning 报告服务当前是否 RUNNING(未安装/查询失败视为 false)。
func serviceRunning() bool {
	m, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return false
	}
	defer func() { _ = s.Close() }()
	q, err := s.Query()
	return err == nil && q.State == svc.Running
}

// stopServiceAndWait 停止服务并等待进入 STOPPED(升级替换程序/EasyTier 二进制前必须
// 确保进程已退出,否则文件被占用)。未安装视为成功。
func stopServiceAndWait(timeout time.Duration) error {
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return nil // 未安装
	}
	defer func() { _ = s.Close() }()

	q, err := s.Query()
	if err != nil {
		return fmt.Errorf("查询服务状态失败:%w", err)
	}
	if q.State == svc.Stopped {
		return nil
	}
	if q.State != svc.StopPending {
		if _, cerr := s.Control(svc.Stop); cerr != nil && !errors.Is(cerr, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return fmt.Errorf("停止服务失败:%w", cerr)
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		q, err := s.Query()
		if err != nil {
			return fmt.Errorf("查询服务状态失败:%w", err)
		}
		if q.State == svc.Stopped {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("等待服务停止超时(%s)", timeout)
}

// startService 启动服务(升级完成后恢复为升级前的运行状态)。
func startService() error {
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("服务未安装:%w", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Start(); err != nil {
		return fmt.Errorf("启动服务失败:%w", err)
	}
	return nil
}
