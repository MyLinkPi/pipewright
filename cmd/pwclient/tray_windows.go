//go:build windows

// tray_windows.go 是托盘 GUI(lxn/walk 原生 Win32):主窗口(状态/网段路由/集群配置/
// 服务管理/日志)+ 常驻托盘(状态提示/快捷菜单)。GUI 不直接碰运行时 —— 一律经服务
// IPC(服务未运行时引导安装;关闭主窗口仅隐藏到托盘,退出经托盘菜单,组网不受影响)。
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"strconv"
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

	socks5Check     *walk.CheckBox
	socks5Port      *walk.NumberEdit
	socks5Addr      *walk.Label
	copyProxyBtn    *walk.PushButton
	socks5ApplyBtn  *walk.PushButton
	socks5ExitCombo *walk.ComboBox
	proxyEdit       *walk.TextEdit
	proxyStatus     *walk.Label

	aliyunAkEdit     *walk.LineEdit
	aliyunSkEdit     *walk.LineEdit
	aliyunGroupsEdit *walk.TextEdit
	aliyunApplyBtn   *walk.PushButton
	aliyunStatus     *walk.Label

	// 出口下拉的最近一次回填(候选集合 + 已存值)。均未变化时跳过回填,避免 3s 轮询
	// 打断用户正在展开/选择的过程(选择未保存前轮询不得重置下拉)。
	exitItems  []string
	exitCur    string
	exitSynced bool

	// routesSynced/proxySynced/aliyunSynced 是三个列表编辑框的首拍回填标志:界面首次
	// 拿到快照时各填一次,之后编辑框完全归用户(轮询不再触碰);导入配置后重置重填。
	routesSynced bool
	proxySynced  bool
	aliyunSynced bool

	peerList       *walk.ListBox
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
	if startMinimized {
		a.mw.Hide() // --minimized(开机自启):仅托盘,点图标/双击再唤出
	} else {
		a.mw.Show() // 双击启动显示界面
	}
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
		Size:     Size{Width: 720, Height: 760},
		Layout:   VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}, Spacing: 8},
		Children: []Widget{
			TabWidget{
				Pages: []TabPage{
					{
						Title:  "状态",
						Layout: VBox{Margins: Margins{Left: 4, Top: 4, Right: 4, Bottom: 4}, Spacing: 8},
						Children: []Widget{
							GroupBox{
								Title:  "状态",
								Layout: VBox{MarginsZero: true, Spacing: 6},
								Children: []Widget{
									Label{AssignTo: &a.stateLabel, EllipsisMode: EllipsisEnd, Text: "正在连接服务…", TextColor: walk.Color(0x666666)},
									Label{AssignTo: &a.detailLabel, EllipsisMode: EllipsisEnd, Text: ""},
									Label{AssignTo: &a.binLabel, EllipsisMode: EllipsisEnd, Text: ""},
									Composite{
										Layout: HBox{MarginsZero: true, Spacing: 8},
										Children: []Widget{
											PushButton{AssignTo: &a.connectBtn, Text: "连接", OnClicked: a.onToggleConnect, MinSize: Size{Width: 96}},
											PushButton{AssignTo: &a.downloadBtn, Text: "下载 EasyTier", OnClicked: a.onDownload},
											PushButton{AssignTo: &a.installBtn, Text: "安装并启动服务", OnClicked: a.onInstallService},
										},
									},
									Label{AssignTo: &a.svcLabel, EllipsisMode: EllipsisEnd, TextColor: walk.Color(0x888888)},
									Label{Text: "对端节点(直连/中继):", TextColor: walk.Color(0x666666)},
									ListBox{AssignTo: &a.peerList, MinSize: Size{Height: 76}},
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
					},
					{
						Title:  "设置",
						Layout: VBox{Margins: Margins{Left: 4, Top: 4, Right: 4, Bottom: 4}, Spacing: 8},
						Children: []Widget{
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
									Label{AssignTo: &a.peersLabel, EllipsisMode: EllipsisEnd, TextColor: walk.Color(0x888888)},
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
									Label{AssignTo: &a.socks5Addr, EllipsisMode: EllipsisEnd, TextColor: walk.Color(0x666666)},
									Composite{
										Layout: HBox{MarginsZero: true, Spacing: 8},
										Children: []Widget{
											Label{Text: "出口节点:"},
											ComboBox{AssignTo: &a.socks5ExitCombo, MinSize: Size{Width: 220}},
											Label{Text: "经所选节点访问非虚拟网目标(需对端允许做出口)", TextColor: walk.Color(0x888888)},
										},
									},
									Label{Text: "多出口代理(可选,每行:本地端口 出口节点虚拟IP,如 1081 10.126.14.2;可填多个出口按序切换。每条 = 一个独立轻量组网实例,占用一个虚拟 IP,集群节点列表会多出 主机名-p端口 条目)", TextColor: walk.Color(0x666666)},
									TextEdit{AssignTo: &a.proxyEdit, MinSize: Size{Height: 64}, VScroll: true},
									Label{AssignTo: &a.proxyStatus, EllipsisMode: EllipsisEnd, TextColor: walk.Color(0x888888)},
								},
							},
							GroupBox{
								Title:  "连接前放行(阿里云安全组,可选)",
								Layout: VBox{MarginsZero: true, Spacing: 6},
								Children: []Widget{
									Label{Text: "连接/重连前自动把本机公网 IPv4 更新到下列安全组的既有规则:按规则描述精确匹配,协议/端口沿用,先授权新 IP 再回收旧 IP;连接期间每 5 分钟复查自愈。须先在阿里云控制台按所填描述创建放行规则(该描述在组内唯一)。", TextColor: walk.Color(0x666666)},
									Composite{
										Layout: HBox{MarginsZero: true, Spacing: 8},
										Children: []Widget{
											Label{Text: "AccessKeyId:"},
											LineEdit{AssignTo: &a.aliyunAkEdit, MinSize: Size{Width: 168}},
											Label{Text: "AccessKeySecret:"},
											LineEdit{AssignTo: &a.aliyunSkEdit, PasswordMode: true, CueBanner: "已配置时留空保持", MinSize: Size{Width: 168}},
											HSpacer{},
										},
									},
									Label{Text: "安全组列表,每行:地域 安全组ID 规则描述(如 cn-hangzhou sg-xxx pwclient-home;描述可含空格):", TextColor: walk.Color(0x666666)},
									TextEdit{AssignTo: &a.aliyunGroupsEdit, MinSize: Size{Height: 64}, VScroll: true},
									Composite{
										Layout: HBox{MarginsZero: true},
										Children: []Widget{
											HSpacer{},
											PushButton{AssignTo: &a.aliyunApplyBtn, Text: "保存并应用", OnClicked: a.onApplyAliyun, MinSize: Size{Width: 96}},
										},
									},
									Label{AssignTo: &a.aliyunStatus, EllipsisMode: EllipsisEnd, TextColor: walk.Color(0x888888)},
								},
							},
							GroupBox{
								Title:  "常规",
								Layout: VBox{MarginsZero: true, Spacing: 6},
								Children: []Widget{
									CheckBox{AssignTo: &a.autoStartCheck, Text: "开机自启(登录后自动运行托盘;后台组网服务本就随系统启动,与此开关无关)", OnClicked: a.onToggleAutoStart},
								},
							},
						},
					},
				},
			},
		},
	}).Create(); err != nil {
		return err
	}

	// 关闭主窗口 = 隐藏到托盘(组网/服务不受影响);退出经托盘菜单。隐藏/退出均
	// 保存窗口边界,下次启动恢复。
	a.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		a.saveWindowState()
		if !a.exiting {
			*canceled = true
			a.mw.Hide()
		}
	})
	a.restoreWindowState(Size{Width: 620, Height: 700})

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
	a.saveWindowState()
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
		a.aliyunApplyBtn.SetEnabled(false)
		a.svcLabel.SetText("")
		_ = a.peerList.SetModel([]string{"(无在线对端)"})
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
	a.aliyunApplyBtn.SetEnabled(true)
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

	// routes 编辑框:首拍填入已存 routes,之后归用户(轮询不回填)。
	if !a.routesSynced {
		a.routesSynced = true
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
	a.syncExitCombo(snap)
	a.syncProxyEdit(snap)
	a.syncAliyun(snap)

	_ = a.peerList.SetModel(peerListItems(snap))

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

// peerListItems 组装对端节点列表行:虚拟 IP · 主机名 · 延迟 · 直连/中继。
// 中继判定来自状态层(tunnel 含 relay,或路由表下一跳交叉验证);RelayVia 为
// 中继节点标识(虚拟 IP 或 peer id)。
func peerListItems(snap *pwclient.StatusSnapshot) []string {
	if snap == nil || snap.EasyTier == nil || len(snap.EasyTier.Links) == 0 {
		return []string{"(无在线对端)"}
	}
	items := make([]string, 0, len(snap.EasyTier.Links))
	for _, l := range snap.EasyTier.Links {
		name := l.Hostname
		if name == "" {
			name = "?"
		}
		lat := "—"
		if l.LatencyMS > 0 {
			lat = fmt.Sprintf("%.0fms", l.LatencyMS)
		}
		kind := "直连 " + l.Tunnel
		if l.Relay {
			kind = "中继"
			if l.RelayVia != "" {
				kind += " via " + l.RelayVia
			}
		}
		items = append(items, fmt.Sprintf("%s  %s  ·  %s  ·  %s", l.VirtualIP, name, lat, kind))
	}
	return items
}

// routeListItems 组装通告网段列表行:有网关信息时显示 "cidr ← 节点名(ip)";同网段
// 多通告者互为备份(主失联自动切换),行尾以 "(+N 备)" 提示;双击仍只取行首 CIDR。
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
		if n := len(pr.Vias) - 1; n > 0 {
			label += fmt.Sprintf(" (+%d 备)", n)
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
	a.routesSynced = false // 导入替换了 routes,下一拍按新配置重填编辑框
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

// syncExitCombo 回填出口节点下拉:候选 = 在线对端(虚拟 IP + 主机名);当前配置的
// 节点离线时仍保留展示;首项固定「不使用」。3s 轮询反复触达本函数,而用户可能正展开
// 或已选了尚未保存的项 —— 候选集合与已存值均未变化时必须跳过,否则会把选择重置回
// 已存值,保存时读到空列表,表现为「设置后再次进入看不到」。
func (a *trayApp) syncExitCombo(snap *pwclient.StatusSnapshot) {
	const noneItem = "(不使用出口节点)"
	items := []string{noneItem}
	cur := ""
	if len(snap.Socks5.ExitNodes) > 0 {
		cur = snap.Socks5.ExitNodes[0]
	}
	if snap.EasyTier != nil {
		for _, l := range snap.EasyTier.Links {
			label := l.VirtualIP
			if host := l.Hostname; host != "" {
				if len(host) > 12 { // 候选项宽度有界,避免下拉最小尺寸随主机名漂移
					host = host[:12] + "…"
				}
				label += "  " + host
			}
			items = append(items, label)
			if cur == l.VirtualIP {
				cur = label
			}
		}
	}
	if cur != "" && cur != noneItem {
		found := false
		for _, it := range items {
			if it == cur {
				found = true
				break
			}
		}
		if !found {
			items = append(items, cur+"  (离线)")
		}
	}
	if a.exitSynced && cur == a.exitCur && slices.Equal(items, a.exitItems) {
		return // 候选与已存值均未变化:不打断用户正在展开/选择的过程
	}
	a.exitItems, a.exitCur, a.exitSynced = items, cur, true
	_ = a.socks5ExitCombo.SetModel(items)
	if cur == "" {
		a.socks5ExitCombo.SetCurrentIndex(0)
		return
	}
	for i, it := range items {
		if it == cur || strings.HasPrefix(it, cur+"  ") {
			a.socks5ExitCombo.SetCurrentIndex(i)
			return
		}
	}
	a.socks5ExitCombo.SetCurrentIndex(0)
}

// selectedExitNodes 解析下拉选择为出口节点列表(空 = 不使用)。
func (a *trayApp) selectedExitNodes() []string {
	idx := a.socks5ExitCombo.CurrentIndex()
	if idx <= 0 {
		return []string{}
	}
	text := strings.TrimSpace(a.socks5ExitCombo.Text())
	if ip := strings.Fields(text); len(ip) > 0 && net.ParseIP(ip[0]) != nil {
		return []string{ip[0]}
	}
	return []string{}
}

// onApplySocks5 保存 SOCKS5 开关、端口、出口节点与多出口代理列表,并立即对账。
func (a *trayApp) onApplySocks5() {
	cli, err := a.client()
	if err != nil {
		a.actionErr(err)
		return
	}
	proxies, err := parseProxyLines(a.proxyEdit.Text())
	if err != nil {
		a.actionErr(err)
		return
	}
	enabled := a.socks5Check.Checked()
	port := int(a.socks5Port.Value())
	exits := a.selectedExitNodes()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := cli.PutConfig(ctx, pwclient.ConfigSetRequest{Socks5Enabled: &enabled, Socks5Port: &port,
		Socks5ExitNodes: &exits, Socks5Proxies: &proxies}); err != nil {
		a.actionErr(err)
		return
	}
	a.refreshOnce()
}

// parseProxyLines 解析多出口代理文本(每行:本地端口 [出口虚拟IP…],空白分隔;
// 端口必填,出口可多个按序优先)。非法行带原值报错,即时反馈。
func parseProxyLines(text string) ([]pwclient.ProxyConfig, error) {
	var out []pwclient.ProxyConfig
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		port, err := strconv.Atoi(fields[0])
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("多出口代理行 %q 的端口须为 1-65535 的数字", line)
		}
		p := pwclient.ProxyConfig{Port: port}
		for _, ip := range fields[1:] {
			if net.ParseIP(ip) == nil {
				return nil, fmt.Errorf("多出口代理行 %q 的 %q 不是合法 IP(填对端虚拟 IP)", line, ip)
			}
			p.ExitNodes = append(p.ExitNodes, ip)
		}
		out = append(out, p)
	}
	return out, nil
}

// proxyLinesText 是代理列表的文本形态(与 parseProxyLines 互逆;回显比对用)。
func proxyLinesText(proxies []pwclient.ProxyStatus) string {
	lines := make([]string, 0, len(proxies))
	for _, p := range proxies {
		lines = append(lines, strings.Join(append([]string{strconv.Itoa(p.Port)}, p.ExitNodes...), " "))
	}
	return strings.Join(lines, "\r\n")
}

// syncProxyEdit 回显多出口代理:首拍填入已存列表,之后归用户(轮询不回填);
// 状态行汇总各端口的运行/错误(来自快照的监督器状态)。
func (a *trayApp) syncProxyEdit(snap *pwclient.StatusSnapshot) {
	if !a.proxySynced {
		a.proxySynced = true
		a.proxyEdit.SetText(proxyLinesText(snap.Proxies))
	}
	if len(snap.Proxies) == 0 {
		a.proxyStatus.SetText("")
		return
	}
	parts := make([]string, 0, len(snap.Proxies))
	for _, p := range snap.Proxies {
		exit := "无出口"
		if len(p.ExitNodes) > 0 {
			exit = strings.Join(p.ExitNodes, "/")
		}
		state := "启动中"
		switch {
		case p.LastErr != "":
			state = "错误:" + firstLine(p.LastErr)
		case p.Running:
			state = "运行中"
		}
		parts = append(parts, fmt.Sprintf("127.0.0.1:%d→%s(%s)", p.Port, exit, state))
	}
	a.proxyStatus.SetText(strings.Join(parts, " · "))
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

// parseSecGroupLines 解析安全组列表文本(每行:地域 安全组ID 规则描述;前两段空白分隔,
// 其余整段作为描述 —— 描述可含空格。三段必填,非法行带原值报错)。
func parseSecGroupLines(text string) ([]pwclient.AliyunGroupConfig, error) {
	var out []pwclient.AliyunGroupConfig
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 3 {
			return nil, fmt.Errorf("安全组行 %q 须为「地域 安全组ID 规则描述」三段(空白分隔)", line)
		}
		out = append(out, pwclient.AliyunGroupConfig{
			RegionID:        fields[0],
			SecurityGroupID: fields[1],
			Description:     strings.Join(fields[2:], " "),
		})
	}
	return out, nil
}

// secGroupLinesText 是安全组列表的文本形态(与 parseSecGroupLines 互逆;回显比对用)。
func secGroupLinesText(groups []pwclient.AliyunGroupConfig) string {
	lines := make([]string, 0, len(groups))
	for _, g := range groups {
		lines = append(lines, g.RegionID+" "+g.SecurityGroupID+" "+g.Description)
	}
	return strings.Join(lines, "\r\n")
}

// onApplyAliyun 保存阿里云 AK 与安全组列表(Secret 只写不读:留空 = 保持已存),保存后
// 经 RestartNow 立即对账 —— 连接前的下一次同步即用新配置。
func (a *trayApp) onApplyAliyun() {
	cli, err := a.client()
	if err != nil {
		a.actionErr(err)
		return
	}
	groups, err := parseSecGroupLines(a.aliyunGroupsEdit.Text())
	if err != nil {
		a.actionErr(err)
		return
	}
	ak := strings.TrimSpace(a.aliyunAkEdit.Text())
	sk := strings.TrimSpace(a.aliyunSkEdit.Text())
	req := pwclient.ConfigSetRequest{AliyunAccessKeyID: &ak, AliyunGroups: &groups}
	if sk != "" {
		req.AliyunAccessKeySecret = &sk
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := cli.PutConfig(ctx, req); err != nil {
		a.actionErr(err)
		return
	}
	a.aliyunSkEdit.SetText("") // 只写不读:保存即清空,不留在界面
	a.refreshOnce()
}

// syncAliyun 回显安全组配置与同步状态:AK/列表首拍填一次,之后归用户(轮询不回填);
// Secret 永不回显;状态行汇总最近一次同步(时间/IP/逐组结果)。
func (a *trayApp) syncAliyun(snap *pwclient.StatusSnapshot) {
	sg := snap.SecurityGroup
	if !a.aliyunSynced {
		a.aliyunSynced = true
		a.aliyunAkEdit.SetText(sg.AccessKeyID)
		a.aliyunGroupsEdit.SetText(secGroupLinesText(sg.Groups))
	}
	if !sg.Configured {
		if len(sg.Groups) == 0 && sg.AccessKeyID == "" {
			a.aliyunStatus.SetText("")
		} else {
			a.aliyunStatus.SetText("未生效:须同时配置 AccessKeyId/Secret 且列表至少一组")
		}
		return
	}
	run := "尚未同步"
	if t, err := time.Parse(time.RFC3339, sg.LastRun); err == nil {
		run = t.Local().Format("01-02 15:04")
	}
	parts := make([]string, 0, len(sg.Results))
	for _, r := range sg.Results {
		mark := map[string]string{
			"update": "已更新", "skip": "最新", "warn": "无匹配规则", "fail": "失败", "pending": "待同步",
		}[r.Status]
		item := fmt.Sprintf("%s/%s:%s", r.RegionID, r.SecurityGroupID, mark)
		if r.LastErr != "" {
			item += "(" + firstLine(r.LastErr) + ")"
		}
		parts = append(parts, item)
	}
	line := fmt.Sprintf("最近同步 %s · 公网 IP %s", run, orDash(sg.LastIP))
	if len(parts) > 0 {
		line += " · " + strings.Join(parts, " · ")
	}
	if sg.LastErr != "" {
		line += " · 上次失败:" + firstLine(sg.LastErr)
	}
	a.aliyunStatus.SetText(line)
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
