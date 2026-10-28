# pwclient 界面彻底重做（本地 Web UI + WebView2）

## 概要（Summary）

pwclient 现有界面是 [tray\_windows.go](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go) 里 1263 行单文件的 `lxn/walk` 原生 Win32 GUI：两个 Tab、五个 GroupBox 全是长段说明文字 + 手写文本行，没有滚动容器；3 秒轮询与用户编辑互相覆盖（靠 `routesSynced / proxySynced / aliyunSynced / exitSynced` 等补丁标志绕过）；错误只在状态区写一行文本且会被下一次轮询覆盖。这些问题无法靠局部修补解决。

本次重做：

* **前端**：新建独立前端工程 `webclient/`（Vue 3 + TypeScript + Vite + naive-ui），复用平台 [tokens.css](file:///home/xufan/trea_project/pipewright/web/src/styles/tokens.css) 的设计 token 与「深色优先」视觉语言；按功能拆页，表单一律「草稿 + 显式保存」，轮询只更新只读状态。

* **宿主**：新增 Go 包 `internal/pwclient/uiserver`，经 `go:embed` 提供前端静态资源，并把 `/api/*` 反向代理到现有 IPC 服务（注入 Bearer token）；同时提供 `/local/*` 端点承载 IPC 服务做不到的本机动作（UAC 安装/卸载服务、开机自启、退出客户端、打开日志、提权升级）。

* **Windows 形态**：托盘（walk `NotifyIcon`）保留，主窗口改为 WebView2 内嵌窗口（`github.com/jchv/go-webview2`，纯 Go 无 CGO）；WebView2 Runtime 缺失时自动回退系统浏览器/Edge `--app` 窗口。

* **Linux / macOS 形态**：`pwclient` / `pwclient ui` 前台运行核心 + uiserver 并打开系统浏览器；不做系统服务、托盘、开机自启（已与用户确认）。

* **后端接口扩展**：日志改为增量（`since` / `nextSeq`），新增升级进度查询；配置语义与「密钥只写不读」保持不变。

* **可验证性**：界面首次能在 Linux 上跑 `vitest` + Playwright 自动化回归——这是「bug 多」的根因之一（Win32 GUI 只能靠 Windows 实机手点）。

* **配置可查看、可单项修改（用户确认的最大痛点）**：新界面启动即调用 `GET /api/config`（该端点早已存在，但托盘 GUI 从未调用，这是「配置无法查看」的直接原因），完整展示当前配置；连接参数、对端地址、路由、SOCKS5/多出口代理、EasyTier、阿里云安全组均可逐项编辑保存，不再只能整份粘贴 YAML 或手写文本行。

交付后：配置格式（`<dataDir>/config.yaml`）、CLI 子命令、托盘图标、EasyTier 组网逻辑、升级机制（UAC 独立进程替换 exe）全部保持不变；仅界面层与必要 IPC 扩展改变。

## 现状分析（Current State Analysis）

### 现有进程与数据流

* 入口 [main.go](file:///home/xufan/trea_project/pipewright/cmd/pwclient/main.go#L61-L91)：`tray`（默认）/`service`/`console`/`status`/`doctor`/`upgrade`/`version`；Windows 服务形态经 [svc\_windows.go](file:///home/xufan/trea_project/pipewright/cmd/pwclient/svc_windows.go) 装配核心 + IPC。

* IPC [ipc.go](file:///home/xufan/trea_project/pipewright/internal/pwclient/ipc.go#L51-L101)：监听 `127.0.0.1:0`，端口与 token 落 `<dataDir>/ipc.json`；端点 `GET /api/status`、`GET|PUT /api/config`、`POST /api/import`、`POST /api/restart`、`POST /api/download`、`GET /api/upgrade/check`、`GET /api/logs`。

* 状态载荷 [runtime.go:463](file:///home/xufan/trea_project/pipewright/internal/pwclient/runtime.go#L463-L480)：`StatusSnapshot{Configured, Enabled, Join, Routes, Socks5, Proxies, Supervisor, EasyTier, Binaries, Service, ClientVersion, ClientPath, SecurityGroup}`，已覆盖 UI 所需数据。

* 配置模型 [config.go:93](file:///home/xufan/trea_project/pipewright/internal/pwclient/config.go#L93-L107)：`Enabled / Join / Routes / Socks5 / Proxies / EasyTier / Aliyun`；`networkSecret`、`credential`、`aliyun.accessKeySecret` 只写不读。

* **配置回显接口早已存在但 GUI 从未使用**：`GET /api/config`（[ipc.go:215](file:///home/xufan/trea_project/pipewright/internal/pwclient/ipc.go#L215-L222)）与 `IPCClient.LoadConfigView`（[ipc.go:545-552](file:///home/xufan/trea_project/pipewright/internal/pwclient/ipc.go#L545-L552)）在生产代码中**没有任何调用方**；[tray\_windows.go](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go) 只拉 `Status`（部分配置）并用 `Import`/`PutConfig` 写入。因此：网络名/对端/虚拟 IP/主机名只能看只读拼接行，路由/SOCKS5/多出口/安全组/EasyTier 目录与 RPC **完全看不到**，任何修改都必须重新导出整份 YAML 粘贴导入，或直接改写文本框。这是「配置无法查看、无法单项修改」的直接原因。

* 后端其实已具备逐项写入能力：`ConfigSetRequest` 覆盖 `networkName/networkSecret/credential/peers/virtualIp/hostname/routes/socks5*/proxies/easytier*/aliyun*` 全部字段（[ipc.go:226-247](file:///home/xufan/trea_project/pipewright/internal/pwclient/ipc.go#L226-L247)），且 `Store.Save → Normalize` 提供逐项校验（虚拟 IP、CIDR、出口 IP、代理端口冲突等，[config.go:167-217](file:///home/xufan/trea_project/pipewright/internal/pwclient/config.go#L167-L217)）。短板只在界面。

* 日志环 [logring.go](file:///home/xufan/trea_project/pipewright/internal/pwclient/logring.go)：定长行环，只有 `Tail(n)`，无序号，前端无法增量追加。

* 本机动作目前在 GUI 内直接实现：UAC 安装 [tray\_windows.go:1035-1070](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L1035-L1070)、开机自启 [autostart\_windows.go](file:///home/xufan/trea_project/pipewright/cmd/pwclient/autostart_windows.go)、提权升级 [tray\_windows.go:1132-1153](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L1132-L1153)、单实例 [instance\_windows.go](file:///home/xufan/trea_project/pipewright/cmd/pwclient/instance_windows.go)。

* 非 Windows 直接退出：[tray\_other.go](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_other.go#L12-L15)，因此现有 GUI 无法在 Linux 上开发或自动化验证。

* 构建：[Makefile:41-43](file:///home/xufan/trea_project/pipewright/Makefile#L41-L43) 的 `build-client` 明确「无前端依赖」；[.goreleaser.yaml:45-61](file:///home/xufan/trea_project/pipewright/.goreleaser.yaml#L45-L61) 与 [ci.yml:40-43](file:///home/xufan/trea_project/pipewright/.github/workflows/ci.yml#L40-L43) 只交叉编译 `windows/amd64|arm64`。

* 窗口边界持久化 [windowstate\_windows.go](file:///home/xufan/trea_project/pipewright/cmd/pwclient/windowstate_windows.go)：walk 无内建持久化，靠保存 `BoundsPixels` 补。

### 现存缺陷清单（重做必须一并消除）

| #  | 缺陷                                               | 位置                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       | 根因                                                 |
| -- | ------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------- |
| 1  | 设置页内容远超窗口高度且无滚动，底部「阿里云安全组/常规」被挤压                 | [tray\_windows.go:174-277](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L174-L277)                                                                                                                                                                                                                                                                                                                                                                                                                            | `TabPage` 直接用 `VBox` 堆 5 个 GroupBox，无 `ScrollView` |
| 2  | 轮询覆盖/打断用户编辑，靠「首拍回填」标志绕过                          | [tray\_windows.go:67-78](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L67-L78)、[505-509](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L505-L509)、[876-903](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L876-L903)、[974-982](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L974-L982)                                                                                                                                         | 状态轮询与表单共用同一控件                                      |
| 3  | 出口下拉在用户展开/已选未保存时被轮询重置                            | [tray\_windows.go:748-802](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L748-L802)                                                                                                                                                                                                                                                                                                                                                                                                                            | `exitItems/exitCur/exitSynced` 人工去抖                |
| 4  | 日志每 3 秒全量 `SetText`，滚动位置跳回顶部、闪烁                  | [tray\_windows.go:549-554](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L549-L554)                                                                                                                                                                                                                                                                                                                                                                                                                            | 无增量接口，全量替换                                         |
| 5  | 主按钮文案先写「断开」再按状态改回，逻辑绕且易错                         | [tray\_windows.go:487-495](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L487-L495)                                                                                                                                                                                                                                                                                                                                                                                                                            | 状态派生散落在赋值语句中                                       |
| 6  | 服务未运行时批量禁用控件、隐藏按钮，恢复完全依赖下一拍轮询                    | [tray\_windows.go:399-432](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L399-L432)                                                                                                                                                                                                                                                                                                                                                                                                                            | 无统一状态机                                             |
| 7  | 网段双击追加依赖 ListBox 行文本 `strings.Fields(...)[0]` 解析 | [tray\_windows.go:584-602](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L584-L602)                                                                                                                                                                                                                                                                                                                                                                                                                            | 列表只有字符串模型                                          |
| 8  | 多出口代理、安全组用自由文本行手写，非法行整体报错                        | [tray\_windows.go:842-865](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L842-L865)、[916-934](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L916-L934)                                                                                                                                                                                                                                                                                                                               | 无结构化表单                                             |
| 9  | 错误反馈只写 `detailLabel` + 托盘气泡，下一次轮询即被覆盖            | [tray\_windows.go:1256-1262](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L1256-L1262)                                                                                                                                                                                                                                                                                                                                                                                                                        | 无独立错误通道                                            |
| 10 | 导入成功用托盘气泡提示并强制 `showWindow`，界面无就地反馈              | [tray\_windows.go:715-731](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L715-L731)                                                                                                                                                                                                                                                                                                                                                                                                                            | 缺少页面内结果区                                           |
| 11 | 安装服务后按钮隐藏/退场，失败文案「重试」与真实服务状态不一致                  | [tray\_windows.go:1035-1070](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L1035-L1070)                                                                                                                                                                                                                                                                                                                                                                                                                        | 安装态未纳入状态机                                          |
| 12 | 版本行把托盘版本/服务版本/可升级版本拼成一行，易溢出                      | [tray\_windows.go:1174-1186](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L1174-L1186)                                                                                                                                                                                                                                                                                                                                                                                                                        | 信息层级未拆分                                            |
| 13 | 最小 620×700 + 内容密度高，在 125%/150% DPI 下挤压           | [tray\_windows.go:127-129](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L127-L129)、[windowstate\_windows.go:56-83](file:///home/xufan/trea_project/pipewright/cmd/pwclient/windowstate_windows.go#L56-L83)                                                                                                                                                                                                                                                                                                    | 原生窗口尺寸与内容耦合                                        |
| 14 | 非 Windows 直接报错退出，三平台不可用、无法自动化验证                  | [tray\_other.go:12-15](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_other.go#L12-L15)                                                                                                                                                                                                                                                                                                                                                                                                                                    | GUI 与 Windows 强绑定                                  |
| 15 | 首次使用要读完整段长说明（导入格式、路由语义、代理语义、安全组规则）               | [tray\_windows.go:182](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L182)、[199](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L199)、[216](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L216)、[237](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L237)、[246](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L246)、[257](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L257) | 没有分步引导/空状态                                         |

### 补充：配置不可查看、不可单项修改（用户确认的最大痛点）

* **现象**：界面上看不到当前完整配置；想改一个字段（例如主机名、虚拟 IP、某个对端地址、EasyTier RPC 端口）只能重新从平台导出整份 YAML 再粘贴导入，或直接在文本框里手写。
* **根因**：`GET /api/config` 与 `LoadConfigView` 从未被 GUI 调用（见上）；设置页只有「粘贴导入」入口，没有配置回显与逐项表单。
* **结论**：后端接口与逐项校验已具备（`ConfigSetRequest` + `Normalize`），新 UI 只需接入 `GET /api/config` 并做逐项编辑表单即可彻底解决；这是本次重做的一等目标（见「配置查看与单项修改」章节）。

## 目标与非目标

**目标**

1. 新界面覆盖现有全部能力：连接/断开、配置导入、网段路由、SOCKS5 与多出口代理、阿里云安全组、日志、版本/更新/升级、服务安装与卸载、开机自启、退出。
2. **配置可见、可逐项修改**：启动即回显 `GET /api/config` 全量配置（密钥只报存在性）；连接参数、对端列表、虚拟 IP、主机名、路由、代理、EasyTier、安全组均可独立编辑保存，导入 YAML 只是可选捷径而非唯一入口。
3. 彻底消除上表 15 项缺陷：分页 + 独立滚动、草稿式表单、结构化表格、统一通知、显式状态机。
4. Windows 保持单 exe + 托盘 + 后台服务 + UAC 升级；Linux/macOS 提供前台运行 + Web UI。
5. 界面可在 Linux 上通过 vitest 单测与 Playwright 端到端回归。
6. 同时补齐文档（README / CONTRIBUTING / Makefile 注释）与测试（Go 单测 + 前端单测 + e2e）。

**非目标**

* 不做 Linux/macOS 的系统服务（systemd/launchd）、托盘、开机自启（用户已确认）。

* 不做多语言（仅中文，用户已确认）。

* 不改 `config.yaml` 格式、不改 EasyTier 组网/路由语义、不新增业务能力（如新的代理协议、新的云厂商）。

* 不新增「一键诊断修复」之类新功能；`doctor` 仍是 CLI。

## 总体设计

### 运行形态矩阵

| 平台            | `pwclient`（默认）             | `pwclient ui` | `pwclient console` |
| ------------- | -------------------------- | ------------- | ------------------ |
| Windows       | 托盘 + WebView2 主窗口（核心由服务提供） | 同上            | 前台核心，无 UI（调试）      |
| Linux / macOS | 前台核心 + uiserver + 打开系统浏览器  | 同默认           | 前台核心，无 UI          |

> Windows 上 `pwclient ui` 与默认命令等价，仅为语义显式；Linux/macOS 上默认命令改为 UI 形态（不再报错退出）。

### 进程与数据流

```
Windows GUI 进程
  ├─ walk NotifyIcon（托盘菜单：打开主界面 / 连接 / 断开 / 退出）
  ├─ WebView2 窗口 ──(HTTP)──> uiserver (127.0.0.1:0, token)
  │                              ├─ GET /            静态资源（go:embed）
  │                              ├─ /api/*    ──代理──> 服务进程 IPC (127.0.0.1:0, Bearer)  ──> Runtime
  │                              └─ /local/*  ────────> LocalActions（UAC/自启/退出/升级/打开日志）
  └─ 退出仅关闭 GUI，不影响服务

Linux / macOS 前台进程（pwclient ui）
  ├─ Runtime + IPC（console 形态装配）
  ├─ uiserver
  └─ 打开系统浏览器（xdg-open / open）
```

前端从不接触 IPC 端口与 token：它只同源访问 uiserver，由 uiserver 读取 `ipc.json` 并注入 `Authorization`。

### 新增/修改文件布局

```
webclient/                              新增：独立前端工程（Vue3 + TS + Vite + naive-ui）
  package.json / vite.config.ts / tsconfig.json / playwright.config.ts / eslint.config.js
  src/…                                 见「前端信息架构」
  e2e/…                                 Playwright 用例
internal/pwclient/uiserver/             新增：静态资源 + 反向代理 + 本地动作
  embed.go                              //go:embed all:dist
  server.go                             启动/鉴权/路由
  proxy.go                              代理 /api/* → IPC
  local.go                              LocalActions 接口与 /local/* 处理
  dist/index.html                       构建产物占位（.gitignore，stub 生成）
internal/pwclient/logring.go            修改：日志带单调序号，支持 Since
internal/pwclient/runtime.go            修改：LogTail/Since 适配（保持 Tail 语义）
internal/pwclient/ipc.go                修改：/api/logs 增量、/api/upgrade/status
cmd/pwclient/main.go                    修改：新增 ui 子命令、非 Windows 默认走 UI
cmd/pwclient/ui.go                      新增：三平台 runUI 装配
cmd/pwclient/tray_windows.go            重写：只保留托盘（删除全部 walk 控件与 apply 逻辑）
cmd/pwclient/gui_windows.go             新增：WebView2 主窗口 + 浏览器回退
cmd/pwclient/local_windows.go           新增：LocalActions 的 Windows 实现
cmd/pwclient/local_other.go             新增：LocalActions 的非 Windows实现
cmd/pwclient/tray_other.go              重写：打开浏览器（不再报错退出）
cmd/pwclient/windowstate_windows.go     删除：原生窗口边界持久化改由前端 localStorage
go.mod / go.sum                         新增依赖：github.com/jchv/go-webview2（仅 Windows 构建路径）
cmd/pwclient/svc_windows.go             微调：导出服务状态查询供 local 实现复用（不改装/卸逻辑）
cmd/pwclient/autostart_windows.go       保持
cmd/pwclient/instance_windows.go        保持（窗口标题不变，FindWindow 仍可唤起）
cmd/pwclient/upgrade_windows.go         保持（升级进程逻辑不变）
internal/pwclient/uiserver/server_test.go   新增测试
internal/pwclient/logring_test.go       新增测试
internal/pwclient/ipc_test.go           扩充测试
Makefile / .gitignore / .goreleaser.yaml / .github/workflows/ci.yml   修改
README.md / README.zh-CN.md / CONTRIBUTING.md                         修改
```

### 安全模型

1. **uiserver 只绑** **`127.0.0.1:0`**；token 32 字节随机，落 `<dataDir>/ui.json`（0600），进程退出删除（与 `ipc.json` 同款）。
2. **Host 校验**：所有请求的 `Host` 必须等于实际监听地址（`127.0.0.1:<port>`），否则 403 —— 防 DNS rebinding。
3. **token 校验**：`/api/*` 与 `/local/*` 必须携带 `X-PW-Token`（常量时间比较）；静态资源 `GET /`、`/assets/*` 仅校验 Host。
4. **token 传递**：宿主把 URL 拼成 `http://127.0.0.1:<port>/#t=<token>`；fragment 不进入服务端日志与 `Referer`；前端读入后存 `sessionStorage`。
5. **IPC token 不暴露**：前端只访问 uiserver；uiserver 代理时注入 IPC Bearer。IPC 仍只绑 loopback。
6. **写操作防跨站**：自定义头 `X-PW-Token` 已强制预检；预检不通过 / Origin 不一致即拒。

## 前端信息架构与交互规范

左侧固定导航 + 顶部状态条，页面各自独立滚动；最小窗口尺寸 960×640，宽屏自适应。

| 页面     | 内容                                                                                                                   | 关键交互                                                   |
| ------ | -------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------ |
| 概览     | 连接状态卡（状态点/文案/虚拟 IP/网络名/在线对端/延迟）、服务卡（形态/PID/客户端版本/EasyTier 二进制状态）、更新卡（检查更新/升级进度/重启客户端）                                | 主按钮由状态机派生：服务未运行→「安装并启动服务」；未配置→「导入配置」；已连接→「断开」；已断开→「连接」 |
| 对端     | 表格：虚拟 IP / 主机名 / 延迟 / 链路（直连 tunnel 或 中继 via）                                                                         | 排序、复制 IP、空状态                                           |
| 网段路由   | 上表：集群通告网段（CIDR / 网关 / 备用数 / 是否启用开关）；下表：本机启用列表                                                                        | 开关直接改 draft；手动添加 CIDR（校验 + 查重）；「保存并应用」；留空=不路由任何网段      |
| 本地代理   | SOCKS5 开关/端口/出口节点下拉（在线对端，离线标注）、复制代理地址；多出口代理可编辑表格（端口/出口列表/运行状态/错误）、新增行                                                | 行内校验：端口 1–65535、端口不重复、不与 SOCKS5 端口冲突、出口为合法 IP；「保存并应用」  |
| 配置总览   | 当前配置全量只读展示：连接开关、网络名、认证方式（长期密钥/临时凭据）、配置虚拟 IP 与实际虚拟 IP、配置主机名与实际主机名、对端地址列表、路由数量（跳转）、SOCKS5（启用/端口/出口）、多出口代理条数、EasyTier 二进制版本/路径与 RPC 地址、阿里云 AK（掩码）/Secret 是否已配置/安全组数、数据目录与配置文件路径 | 每项旁「修改」跳到对应编辑区；「复制配置摘要」（脱敏） |
| 连接参数   | 可编辑表单：网络名、对端地址（可增删行）、虚拟 IP（空=自动分配）、主机名（空=计算机名）、长期密钥/临时凭据（只写，留空保持不变）；高级：EasyTier binDir / RPC 地址；导入捷径（粘贴 YAML/选择文件/拖拽 + 「用文件网段替换本机路由」） | 每张卡片独立「保存」（只提交变更字段，`PUT /api/config` 支持部分更新）；保存后提示「已应用，可能短暂重连」；导入成功就地 toast + 自动刷新；失败展示后端原文 |
| 阿里云安全组 | AK/SK 表单（SK 显示「已配置，留空保持不变」）；安全组表格（地域/ID/描述）行内校验；同步状态卡（最近时间/公网 IP/逐组结果）                                               | 保存后清空 SK 输入框；状态卡展示 update/skip/warn/fail/pending       |
| 日志     | 等宽字体日志流（级别着色）、自动滚动开关、复制、清空视图、打开日志文件                                                                                  | 2 秒增量拉取（`since`），用户手动上滚时暂停自动跟随                         |
| 设置     | 开机自启（Windows）、服务管理（安装/启动/停止/卸载，非 Windows 隐藏）、主题（深/浅/跟随）、数据目录/日志路径、关于（版本/提交/构建时间）、退出客户端                               | 服务动作显示 loading 与结果；退出仅关 GUI，提示「组网不受影响」                 |

**统一交互约定**

1. **草稿模型**：所有可编辑表单进入页面时从最新配置构造一次 draft；轮询只更新只读区域，绝不回写 draft；保存成功后以服务回显重建 draft；`dirty` 时切换页面/关闭窗口弹确认。
2. **轮询**：2 秒一次；`document.hidden` 时暂停；连续失败 3 次退避到 5 秒。
3. **错误**：API 错误信封 `{error: "..."}` 由统一 toast 呈现（可展开详情/复制）；离线（503）显示顶部固定横幅 + 重试/安装入口。
4. **空状态**：首次启动走三步引导（安装服务 → 粘贴导入 → 连接），不展示大段说明文字；语义提示以 `n-tooltip`/折叠说明承载。
5. **路由**：用 hash 路由（`#/overview` 等），避免服务器 rewrite 依赖。

### 配置查看与单项修改（核心痛点）

#### 数据来源与契约

* **回显**：前端启动与每次进入配置页时调用 `GET /api/config`（`configView`，[ipc.go:140-202](file:///home/xufan/trea_project/pipewright/internal/pwclient/ipc.go#L140-L202)），拿到完整配置而非仅状态里的片段。
* **JSON 键名统一（必需）**：`configView` 内嵌的 `Socks5Config`、`ProxyConfig`、`AliyunGroupConfig`、`EasyTierConfig` 目前**只有 yaml tag**，JSON 会序列化成 Go 字段名（`Enabled/Port/ExitNodes`、`RegionID/SecurityGroupID/Description`、`BinDir/RPCAddr`），与 `StatusSnapshot` 的 `enabled/port/exitNodes` 风格不一致。计划给这四个结构补 `json` tag（`enabled/port/exitNodes`、`port/exitNodes`、`regionId/securityGroupId/description`、`binDir/rpcAddr`），并新增 `configView` 的 JSON golden 单测锁定契约。只影响 IPC 响应，不影响磁盘 YAML。
* **密钥**：`join.hasSecret / hasCredential`、`aliyun.hasSecret` 只报存在性；前端显示「已配置 / 未配置」，输入框留空 = 保持原值。

#### 配置值 vs 实际值（只读总览必须区分）

| 展示项 | 配置值（`GET /api/config`） | 实际值（`GET /api/status`） |
|--------|------------------------------|------------------------------|
| 虚拟 IP | `join.virtualIp`（空 = DHCP 自动分配） | `easyTier.selfIP` |
| 主机名 | `join.hostname`（空 = 计算机名） | `join.hostname`（计算后实际值） |
| 认证方式 | `join.hasCredential` / `join.hasSecret` | `join.secureMode`（固定开启） |
| 连接开关 | `enabled`（配置意愿） | `enabled`（且 `configured` 才真正生效） |
| EasyTier | `easytier.binDir / rpcAddr` | `binaries.corePath / version` |
| 阿里云 | `aliyun.groups` 条数 | `securityGroup.lastRun / lastIp / results` |

#### 单项编辑与保存

* 每个卡片维护自己的 `ConfigDraft` 切片；保存时**只提交 dirty 字段**，构造 `ConfigSetRequest` 的指针字段（服务端已按指针部分更新，[ipc.go:249-317](file:///home/xufan/trea_project/pipewright/internal/pwclient/ipc.go#L249-L317)）。
* 字段映射：

| UI 字段 | 请求字段 |
|---------|----------|
| 连接开关 | `enabled` |
| 网络名 / 对端地址 / 虚拟 IP / 主机名 | `networkName` / `peers` / `virtualIp` / `hostname` |
| 长期密钥 / 临时凭据（只写） | `networkSecret` / `credential` |
| 路由 | `routes` |
| SOCKS5 开关 / 端口 / 出口节点 | `socks5Enabled` / `socks5Port` / `socks5ExitNodes` |
| 多出口代理 | `socks5Proxies` |
| EasyTier 目录 / RPC | `easytierBinDir` / `easytierRpc` |
| 阿里云 AK / SK / 安全组 | `aliyunAccessKeyId` / `aliyunAccessKeySecret` / `aliyunGroups` |

* **保存语义**：`PUT /api/config` 成功后服务端会 `RestartNow()` 立即对账（现有行为），可能短暂重连。UI 在保存前提示「保存将立即应用，组网可能短暂中断」，保存成功后用响应里的 `configView` 重建 draft 与总览，无需额外 GET。
* **校验**：前端即时校验（网络名非空长度、对端地址格式、虚拟 IP 合法、端口范围/重复、CIDR、出口 IP、安全组三段非空），后端 `Normalize`（[config.go:167-217](file:///home/xufan/trea_project/pipewright/internal/pwclient/config.go#L167-L217)）兜底并返回带原值的具体错误，前端原样展示在对应卡片。
* **只读项**：Secure Mode 固定开启；三类密钥只写不读；数据目录与配置文件路径（`local.info`）只读。

#### 导入入口的定位

* 仍保留「粘贴 YAML / 选择文件 / 拖拽导入」与「用文件网段替换本机路由」开关，但降级为**捷径**：文案明确「也可在下方逐项修改，无需重新导入」。

#### 测试

* vitest：`buildConfigPatch` 只包含变更字段；`describeConfig` 的配置值/实际值映射；密钥字段留空不变。
* e2e：修改主机名 → 保存 → 总览显示新值 → 刷新页面仍为新值；对端地址增删行；SOCKS5 端口修改生效；非法虚拟 IP 前端即时报错、后端错误原文展示；导入 YAML 后表单被新配置重建。
* Go：`configView` JSON golden；`PUT /api/config` 单项更新后其余字段不变（新增/扩充 `ipc_test.go`）。

## 详细改动（Proposed Changes）

### 1. 前端工程 `webclient/`

**做什么**：从零建立 Vue 3 + TS + Vite 工程，实现上表页面；产出静态资源供 `go:embed`。

**为什么**：当前 Win32 控件无法表达分页/表格/表单校验/通知，且无法在 Linux 自动化测试。

**怎么做**：

* `package.json` 依赖：`vue`、`vue-router`、`pinia`、`naive-ui`、`@vicons/tabler`、`@fontsource/inter`、`@fontsource/jetbrains-mono`；开发依赖：`vite`、`typescript`、`vue-tsc`、`vitest`、`@vue/test-utils`、`jsdom`、`@playwright/test`、`eslint`、`eslint-plugin-vue`、`typescript-eslint`、`prettier`。

* `vite.config.ts`：`base: './'`、`build.outDir: '../internal/pwclient/uiserver/dist'`、`build.emptyOutDir: true`；`server.proxy` 把 `/api`、`/local` 指向 `PW_UI_BASE`（开发时由 `pwclient ui --no-open --ui-listen` 打印的地址注入）。

* `src/styles/tokens.css`：从 [web/src/styles/tokens.css](file:///home/xufan/trea_project/pipewright/web/src/styles/tokens.css) 复制（保留 OKLCH 双主题与注释），`global.css` 仅保留字体、重置、滚动条。

* `src/App.vue`：`n-config-provider`（复用 `useNaiveTheme` 的深/浅色主题对象形态）+ `n-message-provider` + `n-dialog-provider` + `AppShell`。

* `src/api/http.ts`：`fetch` 封装 —— 注入 `X-PW-Token`（从 `sessionStorage`）、超时 15s、错误信封解析、503 归一为 `OfflineError`。

* `src/api/types.ts`：与 Go JSON 对齐的手写类型（`StatusSnapshot`、`ConfigView`、`LocalInfo`、`UpgradeStatus`、`ProxyConfig`、`AliyunGroupConfig`、`LogChunk`）。

* `src/stores/`：`status.ts`（快照轮询与离线态）、`config.ts`（`load()` = `GET /api/config`；draft/dirty；`save()` 只提交变更字段；保存后用响应重建）、`logs.ts`（since 增量）、`local.ts`（本地信息/服务状态/自启/升级）。

* `src/lib/configDraft.ts`（纯函数，可单测）：`fromConfigView(view)` → 各卡片 draft；`buildConfigPatch(draft, baseline)` → 只含 dirty 字段的 `ConfigSetRequest`；`describeConfig(view, snap)` → 只读总览的「配置值/实际值」行。

* `src/lib/derive.ts`（纯函数，可单测）：`connectionState(snap)` → `service-down | not-configured | connecting | connected | disconnected`；`primaryAction(state)`；`peers(links)`；`routeDiff(available, enabled)`。

* `src/lib/validate.ts`（纯函数，可单测）：`isCIDR`、`isIP`、`uniquePorts`、`validateProxies`、`validateSecGroups`。

* `src/lib/format.ts`：延迟、时间、错误首行截断（沿用现有 `firstLine` 语义）。

**关键语义保持**：路由留空 = 不路由任何通告网段（[runtime.go](file:///home/xufan/trea_project/pipewright/internal/pwclient/runtime.go) 现有 fail-closed 语义）；Secure Mode 固定开启且界面只读展示；SK/Secret 永不回显。

### 2. 日志增量：`logring.go` + `ipc.go`

**做什么**：让前端可以只拉新增日志，消除全量替换导致的滚动跳顶。

**怎么做**：

* `logRing` 内部存 `[]logEntry{seq uint64, line string}`，写入时 `seq++`；新增：

```go
// Since 返回 seq > after 的行、最新 seq 与「是否因环滚动丢失历史」标志。
func (r *logRing) Since(after uint64, max int) (lines []string, next uint64, reset bool)
```

* 保留 `Tail(n)`（按文本行）供 `pwclient status` 等原有调用方使用。

* `Controller` 接口（[ipc.go:30-39](file:///home/xufan/trea_project/pipewright/internal/pwclient/ipc.go#L30-L39)）增加 `LogSince(after uint64, max int) ([]string, uint64, bool)`；`Runtime` 实现。

* `GET /api/logs?since=<seq>&n=500` → `{"lines":[...],"nextSeq":123,"reset":false}`；未带 `since` 时沿用 `{"lines":[...]}` 并追加 `nextSeq`（向后兼容）。

* 新增 `GET /api/upgrade/status` → 读 `<dataDir>/upgrade-status.json`，返回 `{phase,error,time}`（文件缺失返回空 `phase`）；`ipcServer` 增加 `dataDir` 字段。

* 单测：`logring_test.go` 覆盖滚动、`since=0`、`since` 过期 `reset=true`；`ipc_test.go` 覆盖新端点。

**同时：`configView` JSON 契约统一（服务「配置查看」）**

* 给 `Socks5Config`、`ProxyConfig`、`AliyunGroupConfig`、`EasyTierConfig` 补 `json` tag（与 yaml 命名一致：`enabled/port/exitNodes`、`port/exitNodes`、`regionId/securityGroupId/description`、`binDir/rpcAddr`），不改 yaml tag，磁盘格式不变。**兼容性**：这四个结构也用于 `ConfigSetRequest` 的输入（`socks5Proxies`、`aliyunGroups`）；Go `encoding/json` 对字段名大小写不敏感，旧键名（`Port`/`RegionID`）仍可解码，旧调用方无需改动，新前端统一用 camelCase。
* `ipc_test.go` 增加 `configView` 的 JSON golden 断言（字段名与空数组归一），锁定前端契约；确认 `GET /api/config` 永不包含 `networkSecret`/`credential`/`aliyunAccessKeySecret`。

### 3. `internal/pwclient/uiserver`

**做什么**：宿主侧 HTTP 服务，承载前端与代理，并提供本地动作。

**怎么做**：

```go
// LocalActions 由宿主（cmd/pwclient）实现：IPC 服务进程做不到的本机操作。
type LocalActions interface {
    Info(ctx context.Context) (LocalInfo, error)
    InstallService(ctx context.Context) error            // Windows：UAC 提权
    ServiceAction(ctx context.Context, action string) error // start|stop|uninstall
    SetAutoStart(ctx context.Context, enabled bool) error
    Upgrade(ctx context.Context, tag string) error       // UAC 启动独立升级进程
    Quit(ctx context.Context) error                      // 退出 GUI，不影响服务
    OpenLog(ctx context.Context) error
    RevealDataDir(ctx context.Context) error
}

type Options struct {
    DataDir  string
    Local    LocalActions
    Listen   string // 默认 "127.0.0.1:0"
    Token    string // 空 = 随机生成（测试可注入）
}

func Start(ctx context.Context, opts Options) (*Server, error) // Server.URL() 返回带 #t= 的地址
```

* `embed.go`：`//go:embed all:dist`；`GET /` 与未命中的非 `/api`、`/local` 路径回落到 `index.html`。

* `proxy.go`：每次请求读 `ipc.json` 构造 IPC 客户端（失败则 503 `{"error":"后台服务未运行（服务未安装或未启动）"}`），转发方法/路径/查询/请求体，注入 `Authorization: Bearer <ipc token>`，原样回传状态码与 body；`X-PW-Token` 不向 IPC 透传。

* `local.go`：`GET /local/info`、`POST /local/service/{install|start|stop|uninstall}`、`POST /local/autostart`（`{enabled}`）、`POST /local/upgrade`（`{tag}`）、`POST /local/quit`、`POST /local/open-log`、`POST /local/reveal-data-dir`。

* `LocalInfo` 字段：`{platform, version, dataDir, logFile, service:{installed,state}, autoStart, canInstallService}`。

* 鉴权中间件与 Host 校验见「安全模型」；`server_test.go` 覆盖：Host 不匹配 403、缺 token 403、静态资源 200、`/api/status` 代理 503、`/local/info` 调用桩实现。

### 4. `cmd/pwclient` 装配

**做什么**：把 UI 宿主从 walk 主窗口迁移到「托盘 + WebView2 + uiserver」。

**怎么做**：

* `main.go`：新增 `ui` 子命令；`cmd == "tray"` 时非 Windows 转 `runUI()`；`usage()` 更新。`ui` 支持两个可选参数：`--no-open`（不自动打开窗口/浏览器，供 e2e 与开发联调）与 `--ui-listen <addr>`（覆盖 uiserver 监听地址，默认 `127.0.0.1:0`）。

* 新增 `ui.go`：

```go
// runUI 三平台通用：装配核心（console 形态）→ 启动 uiserver → Windows 走托盘，其余打开浏览器。
func runUI(noOpen bool) {
    c, err := runCore("console")
    ...
    srv, err := uiserver.Start(ctx, uiserver.Options{DataDir: dataDir(), Local: newLocalActions(c)})
    ...
    if runtime.GOOS == "windows" { runTray(srv.URL(), c) } else { openBrowser(srv.URL()); waitInterrupt() }
}
```

* `tray_windows.go` 重写：只保留 `walk.NotifyIcon`（宿主为不显示的 `walk.NewMainWindow()`）+ 菜单（打开主界面 / 连接 / 断开 / 分隔 / 退出）+ `pollLoop` 更新 tooltip；连接/断开经 `DialIPC + PutConfig{Enabled}`（从现有 `setEnabled` 精简）；删除所有控件字段、`apply`、`renderLogs`、`on*` 动作、`windowstate` 调用。

* 新增 `gui_windows.go`：

```go
// openMainWindow 打开（或唤起）WebView2 主窗口；Runtime 缺失时回退浏览器。
func openMainWindow(url string) // 已存在窗口则聚焦；否则 goroutine + runtime.LockOSThread 创建
```

* `webview2.NewWithOptions(webview2.WebViewOptions{WindowOptions{Title: "pipewright 集群客户端", Width: 1120, Height: 760, IconId: 1, Center: true}})`；`Navigate(url)`；`Run()`；关闭后清空句柄。

* 返回 `nil`（WebView2 不可用）→ `openInBrowser(url)`：优先 `msedge --app=<url> --window-size=1120,760`，失败用 `rundll32 url.dll,FileProtocolHandler`。

* 新增 `local_windows.go`：`LocalActions` 实现；`InstallService` 复用现有 [onInstallService](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L1035-L1070) 的 `ShellExecute runas` 逻辑；`Upgrade` 复用 [beginUpgrade](file:///home/xufan/trea_project/pipewright/cmd/pwclient/tray_windows.go#L1132-L1153) 的 `runas "upgrade --tag"`；`SetAutoStart` 调 [setAutoStart](file:///home/xufan/trea_project/pipewright/cmd/pwclient/autostart_windows.go#L49-L70)；`Info` 用 [serviceQueryState](file:///home/xufan/trea_project/pipewright/cmd/pwclient/svc_windows.go#L232-L260) 与 `autoStartEnabled`。

* 新增 `local_other.go`：`Info` 返回 `canInstallService:false`、`service:{installed:false,state:"unsupported"}`；其余动作返回 `ErrUnsupported`。

* `tray_other.go` 重写：`openBrowser(url)` 依次尝试 `xdg-open`（Linux）/`open`（macOS），失败时打印 URL 提示手动打开。

* 删除 `windowstate_windows.go`；前端用 `localStorage` 记忆主题与页面偏好，窗口固定初始尺寸（WebView2 的 `Hint` 由系统最大化/缩放处理）。

* `svc_windows.go`、`upgrade_windows.go`、`autostart_windows.go`、`instance_windows.go` 保持行为不变；`instance_windows.go` 的 `mainWindowTitle` 与 WebView2 标题一致，双开仍可唤起。

### 5. 构建、CI 与发布

* **Makefile**：

  * `CLIENT_WEB_DIR := webclient`

  * 新增 `client-web`：`cd $(CLIENT_WEB_DIR) && npm ci && npm run build`（首次需 `npm install` 生成并提交 `webclient/package-lock.json`，与 `web/` 同策略）

  * 新增 `client-web-stub`：确保 `internal/pwclient/uiserver/dist/index.html` 存在（供 `go test`/`vet` 编译 embed）

  * `build-client: client-web` 后执行 `CGO_ENABLED=0 GOOS=windows GOARCH=amd64|arm64 go build`

  * 新增 `build-client-all`：三平台 `windows/linux/darwin` 编译到 `dist-client/`

  * `test`/`vet`/`fmt-check` 前加 `client-web-stub`；`clean` 增加 `webclient/node_modules` 与 `internal/pwclient/uiserver/dist`

* **`.gitignore`**：新增 `/webclient/node_modules/`、`/internal/pwclient/uiserver/dist/`（与现有 `/web/dist/` 同策略：构建产物不入库）。

* **`.goreleaser.yaml`**：`before.hooks` 增加 `bash -c "cd webclient && npm ci && npm run build"`；`pwclient` build 仍为 `windows/amd64|arm64`（发布产物范围不变；Linux/macOS 由源码构建，README 说明）。

* **`.github/workflows/ci.yml`**：

  * backend job 的占位同时创建 `internal/pwclient/uiserver/dist/index.html`；

  * cross-compile 增加 `GOOS=linux|darwin GOARCH=amd64|arm64 ./cmd/pwclient`；

  * frontend job 增加 `cd webclient && npm ci && npm run lint && npm run typecheck && npm run test && npm run build`；

  * 新增 `client-e2e` job（可选但纳入计划）：起 `pwclient ui --no-open --ui-listen 127.0.0.1:18080`，跑 `npx playwright test`。

### 6. 文档

* [README.md](file:///home/xufan/trea_project/pipewright/README.md#L192-L232) 与 [README.zh-CN.md](file:///home/xufan/trea_project/pipewright/README.zh-CN.md) 的客户端章节：改为三平台说明（Windows 托盘 + WebView2；Linux/macOS `pwclient ui` 浏览器）、WebView2 Runtime 说明与回退行为、新界面能力清单、`make build-client` / `build-client-all`。

* [CONTRIBUTING.md](file:///home/xufan/trea_project/pipewright/CONTRIBUTING.md#L59-L89)：新增 `webclient/` 的构建、`npm run dev` 联调方式（`pwclient ui --no-open --ui-listen`）、lint/typecheck/test/e2e 命令。

* `Makefile` 注释与 `.goreleaser.yaml` 注释同步（去掉「客户端无前端依赖」的过时描述）。

* 不新建额外 spec/设计文档（遵循「不主动新建 md」）。

## 旧缺陷 → 新方案映射

| #  | 旧缺陷          | 新方案                                      |
| -- | ------------ | ---------------------------------------- |
| 1  | 设置页无滚动       | 左侧导航分页，每页独立滚动区                           |
| 2  | 轮询覆盖编辑       | 草稿模型：轮询只写只读 store，表单保存后才重建               |
| 3  | 出口下拉被重置      | 下拉候选由 store 派生，选中值为 draft 字段，轮询不触碰       |
| 4  | 日志全量替换       | `/api/logs?since` 增量 + 用户上滚暂停跟随          |
| 5  | 按钮文案先抑后扬     | `derive.ts` 纯函数状态机派生文案与可用性               |
| 6  | 控件批量禁用难恢复    | 统一 `connectionState` + 离线横幅 + 主按钮分派      |
| 7  | ListBox 文本解析 | 网段表格行携带结构化 CIDR，开关直接改 draft              |
| 8  | 自由文本行        | 可编辑表格 + 行内校验 + 只显示具体错误行                  |
| 9  | 错误被轮询覆盖      | 独立 toast + 可展开详情，不占用状态行                  |
| 10 | 导入只弹气泡       | 页面内结果区 + toast + 自动刷新                    |
| 11 | 安装态不一致       | 服务状态并入 `LocalInfo`，安装后按钮变「启动中」并轮询恢复      |
| 12 | 版本行过长        | 服务卡/更新卡/关于页分层展示                          |
| 13 | 高 DPI 挤压     | WebView2 + 响应式布局，最小 960×640，125%/150% 验收 |
| 14 | 非 Windows 退出 | `runUI` + `local_other.go`，三平台可运行        |
| 15 | 长段说明         | 首次三步引导 + 折叠说明 + tooltip                  |
| 16 | 配置不可查看、不可单项修改 | 「配置总览」接 `GET /api/config` 全量回显（区分配置值/实际值）；「连接参数」「路由」「代理」「EasyTier」「安全组」各自独立编辑并只提交变更字段；YAML 导入降级为捷径 |

## 假设与决策（Assumptions & Decisions）

1. **技术路线**：本地 Web UI + WebView2（用户确认）；Linux/macOS 用系统浏览器。
2. **改动边界**：允许扩展 IPC 与后端（用户确认），配置语义与「密钥只写不读」不变。
3. **平台深度**：核心 + UI 三平台；后台服务、托盘、开机自启、UAC 升级仅 Windows（用户确认）。
4. **语言**：仅中文（用户确认）。
5. **发布产物**：`pwclient` 归档仍仅 Windows（用户未要求变更）；Linux/macOS 走源码构建。
6. **托盘实现**：保留 `lxn/walk` 仅作 `NotifyIcon` + 消息循环宿主，删除全部主窗口控件；不引入自写 `Shell_NotifyIcon`。
7. **窗口状态**：删除原生边界持久化（`windowstate_windows.go`），改由前端 `localStorage` 记忆主题/布局；WebView2 使用系统默认窗口行为。
8. **日志传输**：2 秒增量轮询（不做 SSE/WebSocket），实现简单且足以满足本机 UI。
9. **升级执行**：仍由 UAC 提权启动独立 `pwclient upgrade --tag` 进程；进度经新增 `/api/upgrade/status` 读取，不再依赖 GUI 内存状态。
10. **WebView2 降级**：`NewWithOptions` 返回 `nil` → Edge `--app` 窗口 → 默认浏览器；arm64 若 loader 不可用，则该架构退化为浏览器打开（发布仍保留 arm64 二进制）。
11. **路由模式**：前端 hash 路由，避免 SPA 服务端 rewrite 与 WebView2 虚拟主机差异。
12. **e2e 启动方式**：`pwclient ui --no-open --ui-listen 127.0.0.1:18080` 把带 token 的 URL 打到 stdout，Playwright `globalSetup` 解析后复用；数据目录用临时目录（`PIPEWRIGHT_CLIENT_DATA`）。
13. **配置回显契约**：新前端接 `GET /api/config`；同时给 `Socks5Config / ProxyConfig / AliyunGroupConfig / EasyTierConfig` 补 `json` tag 并加 golden 单测，避免前端出现两套字段命名。
14. **单项保存语义**：沿用现有 `PUT /api/config → RestartNow()`（保存即生效，可能短暂重连），不新增「延迟应用/仅重连生效」的复杂分支；UI 在保存前明示。
15. **配置以服务端为准**：前端 draft 只在保存时提交；任何导入/保存成功后都用服务端回显重建，不做本地推断合并。

## 实施顺序

1. **脚手架**：`webclient/` 初始化 + `uiserver` 骨架 + `dist` 占位 + Makefile/CI/`.gitignore`。
2. **后端接口**：`logring` seq + `/api/logs` 增量 + `/api/upgrade/status` + `uiserver`（含单测）。
3. **前端**：stores、纯函数库（derive/validate/configDraft）、九个页面（含配置总览与连接参数逐项编辑）、交互规范（含 vitest）。
4. **宿主**：Windows 托盘重写 + WebView2 + LocalActions；Linux/macOS `runUI` + 浏览器。
5. **回归**：Playwright e2e + 三平台交叉编译 + Windows 实机验收 + 文档更新。

## 验证步骤

**自动化**

```bash
# 前端
cd webclient && npm ci && npm run lint && npm run typecheck && npm run test && npm run build

# Go：格式化、静态检查、单测（含 uiserver/logring/ipc 新增用例）
make fmt-check && make vet && make test

# e2e（Linux 即可；临时数据目录，未配置状态不启动 EasyTier）
PIPEWRIGHT_CLIENT_DATA=$(mktemp -d) ./pwclient ui --no-open --ui-listen 127.0.0.1:18080
cd webclient && npx playwright test

# 交叉编译（CI 同款）
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./cmd/pwclient
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./cmd/pwclient
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build ./cmd/pwclient
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build ./cmd/pwclient

# 产物
make build-client     # windows amd64（含前端嵌入）
make build-client-all # 三平台
```

**e2e 覆盖场景**

1. 服务离线：页面加载 → 顶部离线横幅 → 主按钮为「安装并启动服务」。
2. 未配置：三步引导可见；导入文本框为空时按钮禁用。
3. 已配置断开：概览显示「已断开」+ 网络名；主按钮为「连接」。
4. 路由页：勾选/取消、手动添加非法 CIDR 报错、保存后仅读区域更新且表单不被轮询覆盖。
5. 代理页：端口冲突/非法 IP 行内报错；保存成功后状态卡出现。
6. 安全组页：SK 空值时显示「已配置，留空保持不变」；非法行报错。
7. 日志页：连续两次轮询后旧行不重复、滚动位置保持；手动上滚后新行不抢滚动。
8. 设置页：非 Windows 隐藏服务管理；自启开关在 Windows 分支的 mock 下可切换。
9. 配置总览：启动即显示全量配置（网络名/对端/配置与实际虚拟 IP/配置与实际主机名/SOCKS5/EasyTier RPC/安全组条数）；密钥只显示「已配置」，页面任何位置都不出现明文。
10. 单项修改：改主机名 → 保存 → 总览与状态同步更新 → 刷新页面新值仍在；拦截请求断言 `PUT /api/config` 请求体**只含 `hostname`**；改对端地址增删行、改 SOCKS5 端口同理；非法虚拟 IP 前端即时拦截、后端错误逐字展示。

**Windows 实机手工验收**

1. 未安装服务启动 → 界面引导 → 点安装 → UAC → 服务运行、状态自动恢复，无需重启界面。
2. 导入平台导出 YAML → toast 成功 → 自动连接 → 概览/对端/链路类型正确。
3. **配置查看**：打开「配置总览」，逐项核对网络名、对端列表、配置/实际虚拟 IP、配置/实际主机名、认证方式、EasyTier 二进制版本与 RPC、SOCKS5、多出口条数、阿里云 AK 掩码与安全组条数、数据目录/配置文件路径 —— 与 `<dataDir>/config.yaml` 完全一致。
4. **单项修改**：分别修改主机名、虚拟 IP、对端地址（增删行）、EasyTier RPC 端口并单独保存 → 每次保存后总览与 `pwclient status` 一致、其他字段未被改动；每项保存会触发一次对账，界面明确提示且能自动恢复在线。
5. 路由留空 → `pwclient status` 显示「不路由任何通告网段」；勾选保存 → 与界面一致。
6. SOCKS5 开关/端口/出口、多出口代理保存 → 状态卡与实际端口监听一致（`pwclient doctor` 交叉验证）。
7. 阿里云安全组保存 → 同步状态卡显示逐组结果；SK 不出现在任何界面。
8. 日志实时追加、不跳顶、可复制。
9. 关闭主窗口 → 托盘保留；托盘「打开主界面/连接/断开/退出」均正常；退出不影响服务。
10. 检查更新 → 升级（UAC）→ 进度条随 `upgrade-status.json` 前进 → 完成后「重启客户端」。
11. 高 DPI 125%/150%、窗口 960×640 与最大化下布局不重叠/不截断。
12. 停止服务 / 断网 → 离线横幅与重试按钮；恢复后自动回在线态。
13. WebView2 Runtime 缺失环境（模拟）→ 自动以 Edge `--app` 打开同一界面。

## 风险与回退

| 风险                                         | 影响                     | 缓解                                                                                 |
| ------------------------------------------ | ---------------------- | ---------------------------------------------------------------------------------- |
| `go-webview2` 的 arm64 WebView2Loader 支持不确定 | windows/arm64 可能无法内嵌窗口 | 先做 amd64/arm64 最小 POC；arm64 失败则回退 Edge `--app` / 浏览器，编译与发布不受影响                     |
| walk 与 WebView2 双消息循环线程模型                  | 托盘与窗口互相阻塞/崩溃           | WebView2 在 `runtime.LockOSThread` 的独立 goroutine 创建与 `Run`；先做 POC 再替换正式代码           |
| WebView2 Runtime 未安装（Win10 旧机）             | 主窗口打不开                 | 自动回退 Edge `--app` → 默认浏览器；README 说明                                                |
| 前端构建拖慢 CI                                  | CI 时长增加                | `webclient` 依赖保持精简（naive-ui + vue 生态，无 monaco/xterm）；只在 frontend job 全量构建          |
| 大改动回归风险                                    | 功能遗漏                   | 保留 CLI（`console/status/doctor/service/upgrade`）作为无 UI 兜底；分阶段提交，每阶段可编译可回退           |
| 生成产物入错库                                    | 仓库 churn               | `uiserver/dist`、`webclient/node_modules` 加入 `.gitignore`，CI 用占位文件保证 `go:embed` 可编译 |

## 审批时请确认的两项默认值

* **发布范围**：`pwclient` 归档仍仅 Windows（Linux/macOS 由源码构建）。若希望 goreleaser 同时产出 Linux/macOS 客户端包，请说明，计划中会追加归档配置。

* **WebView2 降级链**：WebView2 → Edge `--app` → 默认浏览器。若希望缺失时明确报错而非静默降级，请说明。

