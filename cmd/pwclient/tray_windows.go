//go:build windows

// tray_windows.go 是 Windows 桌面形态:系统托盘(打开主界面/连接/断开/退出)。
// 主界面是 WebView2 窗口(gui_windows.go);核心由后台服务提供,本进程只跑 uiserver。
package main

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/lxn/walk"

	"github.com/huangchengsir/pipewright/internal/pwclient"
)

// trayApp 是托盘宿主(隐藏的 MainWindow 承载 NotifyIcon 与消息循环)。
type trayApp struct {
	mw      *walk.MainWindow
	ni      *walk.NotifyIcon
	url     string
	exiting atomic.Bool
}

// trayHost 供 /local/quit 关闭托盘与进程。
var trayHost *trayApp

// localCoreMode Windows 不启核心:核心由后台服务独占(避免双实例争用 IPC/网卡)。
func localCoreMode() string { return "" }

// preflightDesktop 在启动 uiserver 之前做单实例守卫(已有实例时唤起并返回 false)。
func preflightDesktop() bool { return ensureSingleInstance() }

// runDesktop 启动托盘并阻塞到退出;noOpen 时不弹主窗口(开机自启/后台形态)。
func runDesktop(url string, noOpen bool) {
	app := &trayApp{url: url}
	if err := app.build(); err != nil {
		fmt.Fprintln(os.Stderr, "托盘初始化失败:", err)
		os.Exit(1)
	}
	trayHost = app
	if !noOpen {
		openMainWindow(url)
	}
	go app.pollLoop()
	app.mw.Run()
}

// build 创建隐藏宿主窗口、托盘图标与菜单。
func (a *trayApp) build() error {
	mw, err := walk.NewMainWindow()
	if err != nil {
		return err
	}
	a.mw = mw

	ni, err := walk.NewNotifyIcon(mw)
	if err != nil {
		return err
	}
	a.ni = ni
	icon, err := walk.NewIconFromResourceId(1)
	if err != nil {
		icon = walk.IconApplication()
	}
	_ = ni.SetIcon(icon)
	_ = ni.SetToolTip("pipewright 集群客户端")

	add := func(text string, fn func()) {
		action := walk.NewAction()
		_ = action.SetText(text)
		action.Triggered().Attach(fn)
		_ = ni.ContextMenu().Actions().Add(action)
	}
	add("打开主界面", func() { openMainWindow(a.url) })
	add("连接", func() { traySetEnabled(true) })
	add("断开", func() { traySetEnabled(false) })
	_ = ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())
	add("退出(组网不受影响)", a.exit)

	// 左键单击打开主界面(与双击 exe 的行为一致)。
	ni.MouseDown().Attach(func(_, _ int, button walk.MouseButton) {
		if button == walk.LeftButton {
			openMainWindow(a.url)
		}
	})
	return ni.SetVisible(true)
}

// exit 关闭托盘与消息循环(不影响后台服务)。/local/quit 会从 HTTP handler 协程
// 调入:所有 walk UI 操作经 Synchronize 投递回消息循环线程(与 pollLoop 同款,
// walk 有线程亲和约束);不用 walk.App().Exit —— 那等价 ExitProcess,会跳过
// uiMain 收尾(uiserver 清理协程不执行,含令牌的 ui.json 残留数据目录)。
func (a *trayApp) exit() {
	a.exiting.Store(true)
	if a.mw != nil {
		a.mw.Synchronize(func() {
			_ = a.ni.Dispose() // 已 Dispose 时返回错误无副作用
			a.mw.Close()       // mw.Run() 随之返回,栈正常展开
		})
	}
}

// quitUI 响应界面「退出客户端」(/local/quit)。
func quitUI() {
	if trayHost != nil {
		trayHost.exit()
	}
}

// pollLoop 每 3 秒刷新托盘提示(服务未运行/连接状态)。
func (a *trayApp) pollLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		text := "pipewright 客户端 — 服务未运行"
		if cli, err := pwclient.DialIPC(dataDir()); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			if snap, serr := cli.Status(ctx); serr == nil {
				text = trayTooltip(snap)
			}
			cancel()
		}
		a.mw.Synchronize(func() { _ = a.ni.SetToolTip(text) })
		if a.exiting.Load() {
			return
		}
	}
}

// trayTooltip 生成托盘提示文案(与界面状态语义一致)。
func trayTooltip(snap *pwclient.StatusSnapshot) string {
	switch {
	case !snap.Configured:
		return "pipewright 客户端 — 未配置"
	case snap.Supervisor.Running && snap.EasyTier != nil && snap.EasyTier.SelfIP != "":
		return "pipewright 客户端 — 已连接 " + snap.EasyTier.SelfIP
	case snap.Enabled:
		return "pipewright 客户端 — 连接中…"
	default:
		return "pipewright 客户端 — 已断开"
	}
}

// traySetEnabled 经 IPC 切换连接开关(失败静默:托盘菜单不承载错误展示)。
func traySetEnabled(on bool) {
	cli, err := pwclient.DialIPC(dataDir())
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	value := on
	_, _ = cli.PutConfig(ctx, pwclient.ConfigSetRequest{Enabled: &value})
}
