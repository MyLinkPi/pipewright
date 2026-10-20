// pwclient 是 pipewright 集群客户端(首要支持 Windows 11):以 Windows 服务常驻组网
// (EasyTier 托管子进程 + 可限定网段路由),以系统托盘 GUI 管理。
//
// 子命令:
//
//	pwclient                  启动托盘 GUI(Windows)
//	pwclient service run      作为 Windows 服务运行(SCM 调起;数据面 + IPC)
//	pwclient service install  安装并启动服务(需管理员;经托盘按钮以 runas 提权调起)
//	pwclient service stop     停止服务
//	pwclient service uninstall 卸载服务
//	pwclient console          前台直跑核心(调试;不经服务,需管理员创建虚拟网卡)
//	pwclient status           打印运行状态(经服务 IPC)
//	pwclient doctor           环境自检(目录/配置/二进制/服务/链路逐项排查)
//	pwclient version          版本信息
//
// 数据目录:env PIPEWRIGHT_CLIENT_DATA 覆盖,默认 %ProgramData%\pipewright-client
// (config.yaml / bin/easytier-*.exe / logs/ / ipc.json)。
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/huangchengsir/pipewright/internal/pwclient"
	"github.com/huangchengsir/pipewright/internal/version"
)

func main() {
	args := os.Args[1:]
	cmd := "tray"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "tray":
		runTray()
	case "service":
		serviceCmd(args[1:])
	case "console":
		runConsole()
	case "status":
		runStatus()
	case "doctor":
		runDoctor()
	case "version", "-v", "--version":
		info := version.Get()
		fmt.Printf("pwclient %s (commit %s, %s, %s/%s)\n", info.Version, info.Commit, info.Date, info.Platform, info.GoVersion)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令:%s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`pwclient — pipewright 集群客户端(Win11)

用法:
  pwclient                     启动托盘 GUI
  pwclient service install     安装并启动后台服务(需管理员)
  pwclient service stop        停止服务
  pwclient service uninstall   卸载服务
  pwclient console             前台直跑核心(调试,需管理员)
  pwclient status              查看运行状态
  pwclient doctor             环境自检
  pwclient version             版本信息

数据目录(PIPEWRIGHT_CLIENT_DATA 可覆盖):
  ` + dataDir() + "\n")
}

// dataDir 解析数据目录(env 覆盖 > 平台默认)。
func dataDir() string {
	if d := strings.TrimSpace(os.Getenv("PIPEWRIGHT_CLIENT_DATA")); d != "" {
		return d
	}
	return pwclient.DefaultDataDir()
}

// core 是一次核心装配的句柄(console 与服务形态共用;GUI 一律经 IPC 间接使用)。
type core struct {
	rt     *pwclient.Runtime
	cancel context.CancelFunc
}

// runCore 装配并启动核心运行时 + IPC:日志写文件与进程内日志环(console 形态另回显终端)。
func runCore(runningAs string) (*core, error) {
	dir := dataDir()
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败:%w", err)
	}
	store := &pwclient.Store{Path: filepath.Join(dir, "config.yaml")}
	rt := pwclient.NewRuntime(store, dir)
	rt.SetServiceInfo(pwclient.ServiceInfo{RunningAs: runningAs, PID: os.Getpid()})

	writers := []io.Writer{rt.LogWriter()}
	if f, err := os.OpenFile(filepath.Join(dir, "logs", "pwclient.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		writers = append(writers, f)
	} else {
		log.Printf("[pwclient] 打开日志文件失败(仅用内存日志):%v", err)
	}
	if runningAs == "console" {
		writers = append(writers, os.Stderr)
	}
	log.SetOutput(io.MultiWriter(writers...))
	log.SetPrefix("")
	log.Printf("[pwclient] 核心启动(形态 %s,数据目录 %s)", runningAs, dir)

	ctx, cancel := context.WithCancel(context.Background())
	rt.Start(ctx)
	if _, err := pwclient.StartIPC(ctx, dir, rt); err != nil {
		cancel()
		return nil, fmt.Errorf("启动服务通道失败:%w", err)
	}
	return &core{rt: rt, cancel: cancel}, nil
}

// runConsole 前台直跑(调试形态):Ctrl+C 退出。
func runConsole() {
	c, err := runCore("console")
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
	fmt.Println("pwclient 核心已在前台运行(Ctrl+C 退出)。数据目录:", dataDir())
	waitInterrupt()
	c.cancel()
}

// waitInterrupt 阻塞到 Ctrl+C / SIGTERM。
func waitInterrupt() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}

// runStatus 打印状态快照(经服务 IPC;服务未运行时给出指引)。
func runStatus() {
	cli, err := pwclient.DialIPC(dataDir())
	if err != nil {
		fmt.Println("服务通道不可用(服务未运行?)。可运行:pwclient service install")
		os.Exit(1)
	}
	snap, err := cli.Status(context.Background())
	if err != nil {
		fmt.Println("读取状态失败:", err)
		os.Exit(1)
	}
	printSnapshot(snap)
}

func printSnapshot(snap *pwclient.StatusSnapshot) {
	state := "未配置"
	switch {
	case !snap.Configured:
		state = "未配置(请导入 pipewright-client.yaml)"
	case snap.Supervisor.Running && snap.EasyTier.SelfIP != "":
		state = "已连接 " + snap.EasyTier.SelfIP
	case snap.Enabled:
		state = "连接中…" + errSuffix(snap)
	default:
		state = "已断开"
	}
	fmt.Println("状态:      ", state)
	fmt.Println("网络:      ", orDash(snap.Join.NetworkName))
	fmt.Println("对端地址:  ", strings.Join(snap.Join.Peers, ", "))
	if n := len(snap.EasyTier.Links); n > 0 {
		fmt.Printf("集群链路:   %d 个对端在线\n", n)
		for _, l := range snap.EasyTier.Links {
			tunnel := l.Tunnel
			if tunnel == "" {
				tunnel = "?"
			}
			fmt.Printf("  %-16s %-20s 延迟 %.0fms(%s)\n", l.VirtualIP, l.Hostname, l.LatencyMS, tunnel)
		}
	}
	if len(snap.EasyTier.ProxyCIDRs) > 0 {
		fmt.Println("通告网段:  ", strings.Join(snap.EasyTier.ProxyCIDRs, ", "))
	}
	if len(snap.Routes) > 0 {
		fmt.Println("本机路由:  ", strings.Join(snap.Routes, ", "), "(仅这些网段)")
	} else if snap.Configured {
		fmt.Println("本机路由:   全部通告网段(未限定)")
	}
	if snap.Socks5.Enabled {
		fmt.Println("本地代理:   socks5://127.0.0.1:" + strconv.Itoa(snap.Socks5.Port) + "(应用代理指向此地址即经集群访问)")
	}
	fmt.Println("EasyTier:  ", binState(snap))
}

func errSuffix(snap *pwclient.StatusSnapshot) string {
	if msg := snap.EasyTier.Err; msg != "" {
		return " [" + firstLine(msg) + "]"
	}
	if msg := snap.Supervisor.LastErr; msg != "" {
		return " [" + firstLine(msg) + "]"
	}
	return ""
}

func binState(snap *pwclient.StatusSnapshot) string {
	b := snap.Binaries
	switch {
	case b.Downloading:
		return "正在下载…"
	case b.CorePath == "":
		return "未就位" + func() string {
			if b.LastErr != "" {
				return "(" + firstLine(b.LastErr) + ")"
			}
			return "(启用后自动下载)"
		}()
	default:
		if b.Version != "" {
			return b.Version
		}
		return "已就位 " + b.CorePath
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// runDoctor 环境自检:逐项检查并给出修复指引,全部通过时退出码 0。
func runDoctor() {
	dir := dataDir()
	ok := true
	check := func(name string, pass bool, detail, fix string) {
		mark := "✓"
		if !pass {
			mark = "✗"
			ok = false
		}
		fmt.Printf("%s %s:%s\n", mark, name, func() string {
			if detail != "" {
				return " " + detail
			}
			return ""
		}())
		if !pass && fix != "" {
			fmt.Println("    修复建议:", fix)
		}
	}

	// 1. 数据目录可写。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		check("数据目录", false, err.Error(), "检查 "+dir+" 的权限")
		return
	}
	probe := filepath.Join(dir, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		check("数据目录可写", false, dir, "以管理员运行或调整目录权限")
	} else {
		_ = os.Remove(probe)
		check("数据目录可写", true, dir, "")
	}

	// 2. 配置加载。
	store := &pwclient.Store{Path: filepath.Join(dir, "config.yaml")}
	cfg, err := store.Load()
	if err != nil {
		check("配置文件", false, firstLine(err.Error()), "修正 config.yaml")
	} else if !cfg.Configured() {
		check("配置文件", false, "尚未导入集群配置", "托盘「导入配置文件」或从平台集群页导出 pipewright-client.yaml")
	} else {
		check("配置文件", true, "网络 "+cfg.Join.NetworkName+",peers "+strings.Join(cfg.Join.Peers, ","), "")
	}

	// 3. 服务与 IPC。
	cli, ipcErr := pwclient.DialIPC(dir)
	if ipcErr != nil {
		check("后台服务", false, "服务通道不可用", "pwclient service install")
	} else {
		check("后台服务", true, "IPC 就绪", "")
		snap, err := cli.Status(context.Background())
		if err != nil {
			check("运行状态", false, firstLine(err.Error()), "")
		} else {
			detail := "核心运行中"
			if snap.EasyTier.SelfIP != "" {
				detail += ",虚拟 IP " + snap.EasyTier.SelfIP
			}
			if snap.EasyTier.Err != "" {
				check("运行状态", false, firstLine(snap.EasyTier.Err), "检查 easytier 二进制版本(需 v2.1+,支持 --proxy-networks/--manual-routes)")
			} else {
				check("运行状态", true, detail, "")
			}
			check("EasyTier 二进制", snap.Binaries.CorePath != "" || snap.Binaries.Downloading,
				binState(snap), "托盘「下载」或检查网络/镜像(PIPEWRIGHT_RELEASE_MIRROR)")
			if snap.Socks5.Enabled {
				addr := fmt.Sprintf("127.0.0.1:%d", snap.Socks5.Port)
				conn, derr := net.DialTimeout("tcp", addr, 2*time.Second)
				listening := derr == nil
				if listening {
					_ = conn.Close()
				}
				check("SOCKS5 监听", listening, addr,
					"组网未就绪或二进制不支持 --socks5(pwclient doctor 的运行状态项会给出原因)")
			}
		}
	}
	doctorPlatformExtra(check)

	if !ok {
		os.Exit(1)
	}
	fmt.Println("自检通过。")
}

// checkFn 是 doctor 的检查项签名(平台扩展点复用)。
type checkFn func(name string, pass bool, detail, fix string)
