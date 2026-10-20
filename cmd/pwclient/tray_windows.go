//go:build windows

// tray_windows.go 是托盘 GUI(lxn/walk 原生 Win32):主窗口(状态/网段路由/集群配置/
// 服务管理/日志)+ 常驻托盘(状态提示/快捷菜单)。GUI 不直接碰运行时 —— 一律经服务
// IPC(服务未运行时引导安装;关闭主窗口仅隐藏到托盘,退出经托盘菜单,组网不受影响)。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"golang.org/x/sys/windows"

	"github.com/huangchengsir/pipewright/internal/pwclient"
)

// trayApp 聚 GUI 全部可变态(控件句柄 + IPC 客户端 + 最近快照)。
type trayApp struct {
	mw *walk.MainWindow
	ni *walk.NotifyIcon

	stateLabel  *walk.Label
	detailLabel *walk.Label
	binLabel    *walk.Label
	svcLabel    *walk.Label

	connectBtn    *walk.PushButton
	installBtn    *walk.PushButton
	downloadBtn   *walk.PushButton
	importBtn     *walk.PushButton
	applyRoutes   *walk.PushButton
	availList     *walk.ListBox
	routesEdit    *walk.TextEdit
	logEdit       *walk.TextEdit
	actOpen       *walk.Action
	actConnect    *walk.Action
	actDisconnect *walk.Action

	socks5Check    *walk.CheckBox
	socks5Port     *walk.NumberEdit
	socks5Addr     *walk.Label
	copyProxyBtn   *walk.PushButton
	socks5ApplyBtn *walk.PushButton

	autoStartCheck *walk.CheckBox

	importEdit *walk.TextEdit // 粘贴 pipewright-client.yaml 全文导入
	peersLabel *walk.Label    // 对端地址(入网入口)只读展示 —— 随导入的配置,不单独配置
	routeItems []string       // 通告网段列表当前行(双击取 CIDR)

	ipc     *pwclient.IPCClient
	snap    *pwclient.StatusSnapshot
	ipcDown bool
	exiting bool
}

// runTray 启动托盘 GUI(主入口)。已有实例时唤起它并直接退出。
func runTray() {
	if !ensureSingleInstance() {
		return
	}
	a := &trayApp{}
	if err := a.build(); err != nil {
		fmt.Fprintln(os.Stderr, "GUI 初始化失败:", err)
		os.Exit(1)
	}
	go a.pollLoop()
	a.mw.Show()
	a.mw.Run() // 消息循环;主窗口隐藏到托盘,经托盘「退出」结束
}

// build 创建主窗口与托盘。
func (a *trayApp) build() error {
	var err error
	if err = (MainWindow{
		AssignTo: &a.mw,
		Title:    "pipewright 集群客户端",
		Icon:     1, // RT_GROUP_ICON 资源 ID(exe 内嵌图标)
		MinSize:  Size{Width: 620, Height: 700},
		Layout:   VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}, Spacing: 8},
		Children: []Widget{
			GroupBox{
				Title:  "状态",
				Layout: VBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{AssignTo: &a.stateLabel, Text: "正在连接服务…", TextColor: walk.Color(0x666666)},
					Label{AssignTo: &a.detailLabel, Text: ""},
					Label{AssignTo: &a.binLabel, Text: ""},
					Composite{
						Layout: HBox{MarginsZero: true, Spacing: 8},
						Children: []Widget{
							PushButton{AssignTo: &a.connectBtn, Text: "连接", OnClicked: a.onToggleConnect, MinSize: Size{Width: 96}},
							PushButton{AssignTo: &a.downloadBtn, Text: "下载 EasyTier", OnClicked: a.onDownload},
							PushButton{AssignTo: &a.installBtn, Text: "安装并启动服务", OnClicked: a.onInstallService},
						},
					},
					Label{AssignTo: &a.svcLabel, TextColor: walk.Color(0x888888)},
				},
			},
			GroupBox{
				Title:  "网段路由(经集群隧道访问的远端网段)",
				Layout: VBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{Text: "集群通告的网段(← 网关节点,双击追加到下方列表):", TextColor: walk.Color(0x666666)},
					ListBox{AssignTo: &a.availList, MinSize: Size{Height: 76}, OnItemActivated: a.onAddRoute},
					Label{Text: "本机路由的网段,每行一个 CIDR;留空 = 不路由任何通告网段(仅虚拟网互通):", TextColor: walk.Color(0x666666)},
					TextEdit{AssignTo: &a.routesEdit, MinSize: Size{Height: 96}, VScroll: true},
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							HSpacer{},
							PushButton{AssignTo: &a.applyRoutes, Text: "保存并应用", OnClicked: a.onApplyRoutes, MinSize: Size{Width: 116}},
						},
					},
				},
			},
			GroupBox{
				Title:  "本地代理(SOCKS5,可选)",
				Layout: VBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{Text: "应用(浏览器/终端等)把代理指向 127.0.0.1:端口 即可经集群访问,不影响其他流量;可达范围 = 上方路由列表内的网段。无认证,仅限本机使用;域名解析走本机 DNS。", TextColor: walk.Color(0x666666)},
					Composite{
						Layout: HBox{MarginsZero: true, Spacing: 8},
						Children: []Widget{
							CheckBox{AssignTo: &a.socks5Check, Text: "启用"},
							Label{Text: "端口:"},
							NumberEdit{AssignTo: &a.socks5Port, MinValue: 1, MaxValue: 65535, Value: 1080.0, Decimals: 0, MinSize: Size{Width: 72}},
							PushButton{AssignTo: &a.copyProxyBtn, Text: "复制代理地址", OnClicked: a.onCopyProxy},
							HSpacer{},
							PushButton{AssignTo: &a.socks5ApplyBtn, Text: "保存并应用", OnClicked: a.onApplySocks5, MinSize: Size{Width: 96}},
						},
					},
					Label{AssignTo: &a.socks5Addr, TextColor: walk.Color(0x666666)},
				},
			},
			GroupBox{
				Title:  "集群配置",
				Layout: VBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{Text: "从平台「设置 → 集群 → 导出客户端配置」复制 YAML 粘贴到下面导入(网络/密钥/对端地址一次就位);网段路由与本地代理的流量均经集群侧节点转发。", TextColor: walk.Color(0x666666)},
					TextEdit{AssignTo: &a.importEdit, MinSize: Size{Height: 72}, VScroll: true},
					Composite{
						Layout: HBox{MarginsZero: true, Spacing: 8},
						Children: []Widget{
							PushButton{AssignTo: &a.importBtn, Text: "导入粘贴的配置", OnClicked: a.onImportText, MinSize: Size{Width: 136}},
							PushButton{Text: "从文件导入…", OnClicked: a.onImport},
							HSpacer{},
						},
					},
					Label{AssignTo: &a.peersLabel, TextColor: walk.Color(0x888888)},
				},
			},
			GroupBox{
				Title:  "常规",
				Layout: VBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					CheckBox{AssignTo: &a.autoStartCheck, Text: "开机自启(登录后自动运行托盘;后台组网服务本就随系统启动,与此开关无关)", OnClicked: a.onToggleAutoStart},
				},
			},
			GroupBox{
				Title:  "日志(尾部)",
				Layout: VBox{MarginsZero: true},
				Children: []Widget{
					TextEdit{AssignTo: &a.logEdit, ReadOnly: true, MinSize: Size{Height: 128}, VScroll: true},
				},
			},
		},
	}).Create(); err != nil {
		return err
	}

	// 关闭主窗口 = 隐藏到托盘(组网/服务不受影响);退出经托盘菜单。
	a.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if !a.exiting {
			*canceled = true
			a.mw.Hide()
		}
	})

	if err = a.buildTray(); err != nil {
		return err
	}
	a.syncAutoStart()
	a.refreshOnce()
	return nil
}

// buildTray 创建托盘图标与菜单。
func (a *trayApp) buildTray() error {
	ni, err := walk.NewNotifyIcon(a.mw)
	if err != nil {
		return err
	}
	a.ni = ni
	trayIcon, err := walk.NewIconFromResourceId(1)
	if err != nil {
		trayIcon = walk.IconApplication()
	}
	_ = ni.SetIcon(trayIcon)
	_ = ni.SetToolTip("pipewright 集群客户端")

	addAction := func(text string, fn func()) *walk.Action {
		act := walk.NewAction()
		_ = act.SetText(text)
		act.Triggered().Attach(func() { fn() })
		_ = ni.ContextMenu().Actions().Add(act)
		return act
	}
	a.actOpen = addAction("打开主窗口", a.showWindow)
	a.actConnect = addAction("连接", a.setEnabled(true))
	a.actDisconnect = addAction("断开", a.setEnabled(false))
	sep := walk.NewSeparatorAction()
	_ = ni.ContextMenu().Actions().Add(sep)
	addAction("退出(组网不受影响)", a.exitApp)

	// 左键单击显示主窗口。
	ni.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			a.showWindow()
		}
	})
	_ = ni.SetVisible(true)
	return nil
}

func (a *trayApp) showWindow() {
	a.mw.Show()
	a.mw.Activate()
	a.refreshOnce()
}

func (a *trayApp) exitApp() {
	a.exiting = true
	_ = a.ni.Dispose()
	a.mw.Close()
	walk.App().Exit(0)
}

// ---- 状态轮询与呈现 ----

// pollLoop 周期拉取状态/日志并调度到 UI 线程(3s)。
func (a *trayApp) pollLoop() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for range t.C {
		a.refreshOnce()
	}
}

func (a *trayApp) refreshOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	var snap *pwclient.StatusSnapshot
	var err error
	if cli, cerr := a.client(); cerr == nil {
		snap, err = cli.Status(ctx)
	}
	logs, _ := a.clientLogs(ctx)
	cancel()
	a.mw.Synchronize(func() { a.apply(snap, err, logs) })
}

// client 返回可用的 IPC 客户端(每次尝试重拨:服务可能刚启动/重启换了 token)。
func (a *trayApp) client() (*pwclient.IPCClient, error) {
	cli, err := pwclient.DialIPC(dataDir())
	if err != nil {
		a.ipcDown = true
		return nil, err
	}
	a.ipc, a.ipcDown = cli, false
	return cli, nil
}

func (a *trayApp) clientLogs(ctx context.Context) ([]string, error) {
	cli, err := a.client()
	if err != nil {
		return nil, err
	}
	return cli.Logs(ctx, 200)
}

// apply 把快照/错误渲染到控件(仅 UI 线程调用)。
func (a *trayApp) apply(snap *pwclient.StatusSnapshot, err error, logs []string) {
	a.snap = snap
	if err != nil || snap == nil {
		a.ipcDown = true
		a.stateLabel.SetText("● 服务未运行")
		a.stateLabel.SetTextColor(walk.Color(0xB0551D))
		a.detailLabel.SetText("核心以 Windows 服务运行;点「安装并启动服务」(需管理员)或管理员终端执行:pwclient service install")
		// 服务不在,依赖服务的操作全部禁用,只留安装引导(否则按钮可点但只会报错)。
		a.connectBtn.SetEnabled(false)
		a.actConnect.SetEnabled(false)
		a.actDisconnect.SetEnabled(false)
		a.installBtn.SetEnabled(true)
		a.installBtn.SetVisible(true)
		a.downloadBtn.SetEnabled(false)
		a.downloadBtn.SetVisible(false)
		a.importBtn.SetEnabled(false)
		a.applyRoutes.SetEnabled(false)
		a.socks5ApplyBtn.SetEnabled(false)
		a.svcLabel.SetText("")
		_ = a.ni.SetToolTip("pipewright 客户端 — 服务未运行")
		a.renderLogs(logs)
		return
	}
	a.installBtn.SetEnabled(false)
	a.installBtn.SetVisible(false) // 服务在,安装按钮整体退场
	// 服务就绪,恢复操作可用性(连接/下载等再按快照细调)。
	a.importBtn.SetEnabled(true)
	a.applyRoutes.SetEnabled(true)
	a.socks5ApplyBtn.SetEnabled(true)
	a.svcLabel.SetText(fmt.Sprintf("服务运行中(PID %d)", snap.Service.PID))

	switch {
	case !snap.Configured:
		a.stateLabel.SetText("● 未配置")
		a.stateLabel.SetTextColor(walk.Color(0xB0551D))
		a.detailLabel.SetText("请导入平台导出的 pipewright-client.yaml(「集群配置 → 导入配置文件」)")
	case snap.Supervisor.Running && snap.EasyTier.SelfIP != "":
		a.stateLabel.SetText("● 已连接  " + snap.EasyTier.SelfIP)
		a.stateLabel.SetTextColor(walk.Color(0x1E7E34))
		online := len(snap.EasyTier.Links)
		a.detailLabel.SetText(fmt.Sprintf("网络 %s · %d 个对端在线 · 通告网段 %d 个",
			orDash(snap.Join.NetworkName), online, len(snap.EasyTier.ProxyCIDRs)))
	case snap.Enabled:
		a.stateLabel.SetText("● 连接中…")
		a.stateLabel.SetTextColor(walk.Color(0x666666))
		hint := ""
		if snap.Binaries.CorePath == "" {
			hint = "等待 EasyTier 二进制就位"
		} else if msg := snap.EasyTier.Err; msg != "" {
			hint = firstLine(msg)
		} else if msg := snap.Supervisor.LastErr; msg != "" {
			hint = firstLine(msg)
		} else {
			hint = "正在组网(打洞/连接对端)"
		}
		a.detailLabel.SetText(fmt.Sprintf("网络 %s · %s", orDash(snap.Join.NetworkName), hint))
	default:
		a.stateLabel.SetText("● 已断开")
		a.stateLabel.SetTextColor(walk.Color(0x666666))
		a.detailLabel.SetText(fmt.Sprintf("网络 %s(配置保留,点「连接」恢复)", orDash(snap.Join.NetworkName)))
	}

	// EasyTier 二进制行 + 下载按钮(兜底:随包自带时二进制已就位,按钮整体隐藏)。
	if snap.Binaries.Downloading {
		a.binLabel.SetText("EasyTier:正在自动下载…")
		a.downloadBtn.SetEnabled(false)
		a.downloadBtn.SetVisible(true)
	} else if snap.Binaries.CorePath == "" {
		msg := "EasyTier:未就位(随包 bin/ 缺失时点此从网络获取)"
		if snap.Binaries.LastErr != "" {
			msg += "(上次失败:" + firstLine(snap.Binaries.LastErr) + ")"
		}
		a.binLabel.SetText(msg)
		a.downloadBtn.SetEnabled(true)
		a.downloadBtn.SetVisible(true)
	} else {
		v := snap.Binaries.Version
		if v == "" {
			v = "已就位"
		}
		a.binLabel.SetText("EasyTier:" + v)
		a.downloadBtn.SetVisible(false)
	}

	// 连接/断开可用性。
	enabled := snap.Enabled
	a.connectBtn.SetEnabled(snap.Configured && !enabled)
	a.connectBtn.SetText("断开")
	if snap.Configured && !enabled {
		a.connectBtn.SetText("连接")
	}
	a.actConnect.SetEnabled(snap.Configured && !enabled)
	a.actDisconnect.SetEnabled(enabled)

	// 可用网段列表:每行附网关节点(谁通告谁网关,流量经它转发);仅展示,不覆盖编辑中内容。
	a.routeItems = a.routeListItems(snap)
	if len(a.routeItems) > 0 {
		_ = a.availList.SetModel(a.routeItems)
	} else {
		_ = a.availList.SetModel([]string{"(集群尚未通告任何网段)"})
	}

	// 首次(或配置重载后)填充 routes 编辑框:仅在编辑框为空或与已存 routes 一致时回填,
	// 避免覆盖用户正在编辑的内容。
	if text := a.routesEdit.Text(); text == "" || text == strings.Join(snap.Routes, "\r\n") || text == strings.Join(snap.Routes, "\n") {
		a.routesEdit.SetText(strings.Join(snap.Routes, "\r\n"))
	}

	// 对端地址只读回显(来自导入的配置;入口变更 → 平台重新导出粘贴)。
	if len(snap.Join.Peers) > 0 {
		a.peersLabel.SetText("对端地址(入网入口):" + strings.Join(snap.Join.Peers, ", "))
	} else {
		a.peersLabel.SetText("对端地址:未配置 —— 粘贴导入集群配置即可")
	}

	// SOCKS5 区:回显生效值(启用中显示监听地址;未启用显示预览地址)。
	port := snap.Socks5.Port
	if port == 0 {
		port = 1080
	}
	if a.socks5Port.Value() != float64(port) {
		a.socks5Port.SetValue(float64(port))
	}
	if snap.Socks5.Enabled {
		a.socks5Addr.SetText("监听中:socks5://127.0.0.1:" + fmt.Sprintf("%d", port) + "(保存后随组网生效)")
		a.copyProxyBtn.SetEnabled(true)
	} else {
		a.socks5Addr.SetText("未启用(勾选并保存后,本机该端口即可经集群访问)")
		a.copyProxyBtn.SetEnabled(false)
	}

	tooltip := "pipewright 客户端 — " + strings.TrimPrefix(a.stateLabel.Text(), "● ")
	_ = a.ni.SetToolTip(tooltip)
	a.renderLogs(logs)
}

func (a *trayApp) renderLogs(logs []string) {
	if len(logs) == 0 {
		return
	}
	a.logEdit.SetText(strings.Join(logs, "\r\n"))
}

// ---- 动作 ----

func (a *trayApp) setEnabled(on bool) func() {
	return func() {
		cli, err := a.client()
		if err != nil {
			a.actionErr(err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		v := on
		if _, err := cli.PutConfig(ctx, pwclient.ConfigSetRequest{Enabled: &v}); err != nil {
			a.actionErr(err)
			return
		}
		a.refreshOnce()
	}
}

func (a *trayApp) onToggleConnect() {
	if a.snap != nil && a.snap.Enabled {
		a.setEnabled(false)()
		return
	}
	a.setEnabled(true)()
}

func (a *trayApp) onAddRoute() {
	idx := a.availList.CurrentIndex()
	if idx < 0 || idx >= len(a.routeItems) {
		return
	}
	// 列表行是 "cidr ← name(ip)" 形态,取首列 CIDR。
	cidr := strings.Fields(a.routeItems[idx])[0]
	cur := strings.FieldsFunc(a.routesEdit.Text(), func(r rune) bool { return r == '\n' || r == '\r' })
	for _, line := range cur {
		if strings.TrimSpace(line) == cidr {
			return // 已在列表
		}
	}
	if text := strings.TrimSpace(a.routesEdit.Text()); text != "" {
		a.routesEdit.SetText(text + "\r\n" + cidr)
	} else {
		a.routesEdit.SetText(cidr)
	}
}

// routeListItems 组装通告网段列表行:有网关信息时显示 "cidr ← 节点名(ip)"。
func (a *trayApp) routeListItems(snap *pwclient.StatusSnapshot) []string {
	if snap == nil || snap.EasyTier == nil {
		return nil
	}
	via := map[string]string{}
	for _, pr := range snap.EasyTier.ProxyRoutes {
		label := pr.ViaIP
		if pr.ViaName != "" {
			label = pr.ViaName + "(" + pr.ViaIP + ")"
		}
		via[pr.CIDR] = label
	}
	items := make([]string, 0, len(snap.EasyTier.ProxyCIDRs))
	for _, cidr := range snap.EasyTier.ProxyCIDRs {
		if label := via[cidr]; label != "" {
			items = append(items, cidr+"  ←  "+label)
		} else {
			items = append(items, cidr)
		}
	}
	return items
}

func (a *trayApp) onApplyRoutes() {
	cli, err := a.client()
	if err != nil {
		a.actionErr(err)
		return
	}
	var routes []string
	for _, line := range strings.FieldsFunc(a.routesEdit.Text(), func(r rune) bool { return r == '\n' || r == '\r' }) {
		if line = strings.TrimSpace(line); line != "" {
			routes = append(routes, line)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := cli.PutConfig(ctx, pwclient.ConfigSetRequest{Routes: &routes}); err != nil {
		a.actionErr(err)
		return
	}
	a.refreshOnce()
}

// onImportText 导入粘贴的 pipewright-client.yaml 全文(与文件导入同一通道)。
func (a *trayApp) onImportText() {
	content := strings.TrimSpace(a.importEdit.Text())
	if content == "" {
		a.actionErr(fmt.Errorf("请先把从平台复制的 pipewright-client.yaml 粘贴到文本框"))
		return
	}
	if err := a.importConfig(content); err != nil {
		a.actionErr(err)
		return
	}
}

// onImport 经文件选择器导入(粘贴导入的补充;平台亦可下载 yaml 文件)。
func (a *trayApp) onImport() {
	dlg := new(walk.FileDialog)
	dlg.Title = "导入 pipewright-client.yaml"
	dlg.Filter = "pipewright 客户端配置 (*.yaml;*.yml)|*.yaml;*.yml|所有文件 (*.*)|*.*"
	ok, err := dlg.ShowOpen(a.mw)
	if err != nil || !ok {
		return
	}
	raw, err := os.ReadFile(dlg.FilePath)
	if err != nil {
		a.actionErr(err)
		return
	}
	if err := a.importConfig(string(raw)); err != nil {
		a.actionErr(err)
	}
}

// importConfig 走 IPC 导入并回填界面。
func (a *trayApp) importConfig(content string) error {
	cli, err := a.client()
	if err != nil {
		return fmt.Errorf("服务未运行,无法导入:%w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	view, err := cli.Import(ctx, content, true)
	if err != nil {
		return err
	}
	a.refreshOnce()
	_ = a.ni.ShowInfo("pipewright 客户端", "已导入集群 "+view.Join.NetworkName+"(已自动连接)")
	a.showWindow()
	return nil
}

func (a *trayApp) onDownload() {
	cli, err := a.client()
	if err != nil {
		a.actionErr(err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := cli.TriggerDownload(ctx); err != nil {
		a.actionErr(err)
		return
	}
	a.refreshOnce()
}

// onApplySocks5 保存 SOCKS5 开关与端口并立即对账。
func (a *trayApp) onApplySocks5() {
	cli, err := a.client()
	if err != nil {
		a.actionErr(err)
		return
	}
	enabled := a.socks5Check.Checked()
	port := int(a.socks5Port.Value())
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := cli.PutConfig(ctx, pwclient.ConfigSetRequest{Socks5Enabled: &enabled, Socks5Port: &port}); err != nil {
		a.actionErr(err)
		return
	}
	a.refreshOnce()
}

// onCopyProxy 把当前 SOCKS5 地址(127.0.0.1:port)写入剪贴板。
func (a *trayApp) onCopyProxy() {
	addr := fmt.Sprintf("127.0.0.1:%d", int(a.socks5Port.Value()))
	if err := walk.Clipboard().SetText(addr); err != nil {
		a.actionErr(err)
		return
	}
	a.socks5Addr.SetText("已复制 " + addr + " — 粘贴到应用或系统代理设置(SOCKS5)")
}

// syncAutoStart 按注册表实际状态回显勾选框,并自愈过期的自启路径(exe 被移动/改名后)。
func (a *trayApp) syncAutoStart() {
	stored := autoStartValue()
	if cmd, err := autoStartCommand(); err == nil && stored != "" && stored != cmd {
		if err := setAutoStart(true); err == nil {
			stored = cmd
		}
	}
	a.autoStartCheck.SetChecked(stored != "")
}

// onToggleAutoStart 勾选/取消时写/删 HKCU Run 自启项(当前用户级,无需管理员)。
func (a *trayApp) onToggleAutoStart() {
	if err := setAutoStart(a.autoStartCheck.Checked()); err != nil {
		a.actionErr(err)
	}
	a.syncAutoStart()
}

// onInstallService 经 UAC 提权运行 `pwclient service install`,随后轮询服务就绪。
func (a *trayApp) onInstallService() {
	exe, err := os.Executable()
	if err != nil {
		a.actionErr(err)
		return
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	args, _ := windows.UTF16PtrFromString("service install")
	cwd, _ := windows.UTF16PtrFromString("")
	if err := windows.ShellExecute(0, verb, file, args, cwd, windows.SW_SHOWNORMAL); err != nil {
		a.actionErr(fmt.Errorf("提权启动安装失败(用户取消或被策略拦截):%w", err))
		return
	}
	a.installBtn.SetEnabled(false)
	a.installBtn.SetText("正在安装…")
	go func() {
		// 安装(含 UAC 确认 + 创建 + 启动)最多等 90s;就绪后由轮询自动切到在线态。
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := pwclient.DialIPC(dataDir()); err == nil {
				a.mw.Synchronize(func() {
					a.installBtn.SetText("安装并启动服务")
					a.refreshOnce()
				})
				return
			}
			time.Sleep(2 * time.Second)
		}
		a.mw.Synchronize(func() {
			a.installBtn.SetEnabled(true)
			a.installBtn.SetText("安装并启动服务(重试)")
		})
	}()
}

// actionErr 统一错误反馈:状态区第二行 + 日志。
func (a *trayApp) actionErr(err error) {
	msg := firstLine(err.Error())
	log.Printf("[tray] 操作失败:%s", msg)
	a.detailLabel.SetText("操作失败:" + msg)
	_ = a.ni.ShowError("pipewright 客户端", msg)
}
