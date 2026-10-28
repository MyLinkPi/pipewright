# pwclient 升级能力(托盘 GUI + CLI)

## 概要(Summary)

为已安装的 Win11 集群客户端 `pwclient` 增加「检查更新 + 一键升级」能力:

- **升级源**:复用 `internal/version` 的 GitHub Releases 检查/下载机制,下载 goreleaser 产出的
  `pwclient_<版本>_windows_<arch>.zip`,用同版本 `checksums.txt` 做 sha256 硬校验。
  **未配置镜像时,GitHub 不可达自动回退到 `https://gh-proxy.com/https://github.com`**
  (前缀式代理,现有 `splitPrefixProxy` 已支持),用户无需配置任何环境变量;
  显式配置了 `PIPEWRIGHT_RELEASE_MIRROR` 则只走该镜像,不再泄漏到公共代理。
- **执行方式(已确认)**:托盘点「升级」经 UAC 提权启动**独立** `pwclient upgrade` 进程;
  该进程停服务 → 旧 exe 改名让位 → 落地新 exe 与随包 EasyTier `bin/` → 重启服务。
  (Windows 不能覆盖正在运行的 exe,用「同目录改名 + 写入原路径」的标准自更新手法。)
- **入口(已确认)**:托盘 GUI「检查更新 / 升级到 vX」按钮 + CLI
  `pwclient upgrade [--check] [--tag vX.Y.Z] [--force]`;**不做**后台自动检查。
- **进度/结果**:升级进程把阶段写入 `<dataDir>/upgrade-status.json`,托盘轮询展示;
  服务状态快照新增客户端版本/路径,供界面显示与升级完成判定。

## 现状分析(Current State Analysis)

- 打包:[.goreleaser.yaml](file:///home/xufan/trea_project/pipewright/.goreleaser.yaml) 的 `pwclient`
  build 仅 `windows/amd64|arm64`,归档名 `pwclient_{{ .Version }}_{{ .Os }}_{{ .Arch }}`,
  zip 内含根级 `pwclient.exe`、`bin/easytier-*`(随包 EasyTier)、`licenses/easytier/*`、`LICENSE`;
  另有根级 `checksums.txt`。
- 版本与升级源:[update.go](file:///home/xufan/trea_project/pipewright/internal/version/update.go)
  的 `Checker.Check` 已支持 GitHub API + 重定向兜底 + 15 分钟缓存 + 镜像;
  [source.go](file:///home/xufan/trea_project/pipewright/internal/version/source.go) 的
  `ResolveSource("")` 统一解析 `PIPEWRIGHT_RELEASE_MIRROR`,且 `splitPrefixProxy` 已能把
  `https://gh-proxy.com/https://github.com` 这类前缀式代理拆成 API/网页/下载三个 base。
  **但当前只解析出「单一源」**:未配置镜像时只有 GitHub 官方,不可达即失败,没有自动回退。
  [selfupdate.go](file:///home/xufan/trea_project/pipewright/internal/version/selfupdate.go) 的
  `ApplyBinaryUpdate` 只处理 **pipewright** 的 tar.gz 且仅 linux/darwin 可自替换,不能用于 pwclient。
- 客户端安装形态:[svc_windows.go](file:///home/xufan/trea_project/pipewright/cmd/pwclient/svc_windows.go)
  的 `stageToProgramFiles` 把 exe + `bin/` 复制到
  `%ProgramFiles%\Pipewright Client`,服务以 LocalSystem 从该路径运行;托盘 GUI 是独立进程,
  与服务的 exe 路径可能不同(用户可能直接从下载目录双击运行)。
- IPC:[ipc.go](file:///home/xufan/trea_project/pipewright/internal/pwclient/ipc.go) 为
  127.0.0.1 随机端口 + Bearer token 的 loopback HTTP;`StatusSnapshot` 已有服务 PID 等元信息,
  但**没有客户端版本**。
- EasyTier 二进制查找优先级([supervisor.go](file:///home/xufan/trea_project/pipewright/internal/mesh/easytier/supervisor.go#L40-L63)):
  `PIPEWRIGHT_EASYTIER_BIN` > `<dataDir>/bin` > exe 同级 `bin/` > PATH。故升级时除更新
  安装目录 `bin/` 外,若 `<dataDir>/bin` 已存在(历史自动下载落点),也需一并刷新,否则实际运行的仍是旧 EasyTier。
- 现有命令入口:[main.go](file:///home/xufan/trea_project/pipewright/cmd/pwclient/main.go)
  的 `switch cmd`,新增 `upgrade` 分支即可;非 Windows 由 [svc_other.go](file:///home/xufan/trea_project/pipewright/cmd/pwclient/svc_other.go)
  同款占位。

## 修改方案(Proposed Changes)

### 1. `internal/version`:多源候选 + 自动回退 + 测试接缝

[source.go](file:///home/xufan/trea_project/pipewright/internal/version/source.go):

```go
// FallbackPrefixProxy 是未配置镜像时 GitHub 不可达的公共前缀式代理兜底。
const FallbackPrefixProxy = "https://gh-proxy.com/https://github.com"

// CandidateSources 返回本次升级按序尝试的源:
//   - 显式配置了镜像(config/env)→ 仅该镜像(尊重用户选择,不回落公共代理);
//   - 未配置 → GitHub 官方,其次 FallbackPrefixProxy。
func CandidateSources(mirror string) []Source
```

[update.go](file:///home/xufan/trea_project/pipewright/internal/version/update.go):

```go
func NewCheckerWithClient(client *http.Client) *Checker   // 测试注入,行为同 NewChecker
func (c *Checker) CheckSource(ctx context.Context, src Source) UpdateInfo // 现有 Check 主体拆出
func (c *Checker) CheckAny(ctx context.Context, sources []Source) UpdateInfo
```

- `Check` 改为 `CheckSource(ctx, c.src(ctx))`,行为/缓存(key = APIBase+Repo)不变;
- `CheckAny` 按序调用 `CheckSource`:**首个 `CheckError == ""` 的结果即返回**
  (含「源上暂无 release」的 404 成功态);全部失败返回最后一个结果(保留可读错误)。
  网络不可达/限流 403 会自然落到下一个候选源。

### 2. 新增 `internal/pwclient/upgrade.go`(平台无关、可单测)

- `ErrUpgrade` 哨兵 + 包内注释(下载/校验/解包/落位语义)。
- `type Upgrader struct`:
  - `Checker *version.Checker`
  - `Sources func(context.Context) []version.Source`(默认 `version.CandidateSources("")`,
    即 GitHub 官方 + gh-proxy 兜底;测试注入单源)
  - `GOOS/GOARCH string`(测试注入;零值取 runtime)
  - `Client *http.Client`(零值 10 分钟超时)
- `func NewUpgrader() *Upgrader`:默认 checker/sources(runtime 平台)。
- `func (u *Upgrader) Check(ctx) version.UpdateInfo`:`u.Checker.CheckAny(ctx, u.Sources(ctx))`。
- `func (u *Upgrader) Stage(ctx, tag string) (stageDir string, err error)`:
  1. `clientAssetName(tag, goos, arch)` =
     `pwclient_<tag 去 v>_<goos>_<arch>.zip`;
  2. **按候选源顺序尝试**(每个源独立完整走完下载+校验+解包,成功即返回,
     失败保留最后一个错误并继续下一个源):
     - 下载 `{DLBase}/{Repo}/releases/download/{tag}/checksums.txt`,按 `strings.Fields`
       取本资产 sha256(缺失即报错,校验是硬门槛);
     - 下载 zip 到系统临时文件并流式算 sha256,不匹配即删并报错;
     - 解包到 `os.MkdirTemp("", "pwclient-upgrade-*")`,路径穿越/绝对路径条目拒绝;
       要求解出根级 `pwclient.exe`(Windows 文件名大小写不敏感);
  3. 全部候选源失败 → 返回最后一个错误(错误体注明已尝试的源数量)。
  成功时记录 `[pwclient] 升级包下载成功(源 <APIBase>,版本 <tag>)` 日志,便于排障。
- `func ApplyStaged(stageDir, installDir, exeName string) (oldExe string, err error)`:
  - 目标 exe 已存在 → 改名为 `<exe>.old-<unixnano>`(同卷改名对运行中的镜像可行);
    改名失败给出「程序被占用/无权限,请关闭托盘或管理员运行」的明确错误;
  - 新 exe 先写 `<exe>.new-<ts>` 再 rename 到位;失败则把旧 exe 改回;
  - `stage/bin/*` → `installDir/bin/`(逐文件 temp+rename);
  - `stage/licenses/**`、根 `LICENSE` 存在则同步(随包许可文件);
  - 返回旧 exe 路径供稍后清理。
- `func CopyStagedBinaries(stageDir, destBinDir string) error`:把 `stage/bin/*` 复制到
  指定 `bin` 目录(用于刷新 `<dataDir>/bin`;目录不存在则跳过)。
- `func CleanupStaleExecutables(installDir, exeName string)`:尽力删除
  `<exe>.old-*` / `<exe>.new-*`,失败忽略(运行中的托盘仍占用时留到下次启动再删)。
- 升级状态文件(供托盘点按钮后跨进程展示进度):
  - `type UpgradeStatus struct { Phase, Target, Current, Message, Error, Time string }`
  - `UpgradeStatusPath(dataDir)`, `WriteUpgradeStatus(dir, st)`(tmp+rename 原子写,0644),
    `ReadUpgradeStatus(dir)`。
  - 阶段常量:`downloading` / `applying` / `restarting` / `done` / `error`。

### 3. `internal/pwclient/runtime.go` + `ipc.go`:状态与 IPC 检查端点

- `Runtime` 新增字段 `checkUpgrade func(context.Context) version.UpdateInfo`;
  `NewRuntime` 里用同一个 `version.NewChecker()` 的 `CheckAny` + `CandidateSources("")`
  装配(共享 15 分钟缓存;测试可替换)。
- `StatusSnapshot` 新增:
  - `ClientVersion string`(`version.Version`,服务进程构建版本)
  - `ClientPath string`(`os.Executable()`,服务实际运行的 exe 路径)
  `Snapshot` 中填充(取不到路径则空串)。
- `Controller` 接口新增 `CheckUpgrade(ctx) version.UpdateInfo`;`Runtime` 实现(委托字段)。
- IPC 新增 `GET /api/upgrade/check` → `s.ctl.CheckUpgrade(r.Context())`(鉴权同其他端点)。
- `IPCClient` 新增 `CheckUpgrade(ctx) (*version.UpdateInfo, error)`。

### 4. `cmd/pwclient/upgrade_windows.go` + `upgrade_other.go`

`runUpgrade(args []string)` 支持:

| 参数 | 含义 |
|---|---|
| `--check` | 只检查并打印当前/最新/是否有更新;不下载、不改动文件;不需要管理员 |
| `--tag vX.Y.Z` | 升级到指定版本(缺省 = 最新) |
| `--force` | 忽略版本比较/同版本,强制重装指定/最新版本 |

流程:

1. 解析参数;`up := pwclient.NewUpgrader()`;`info := up.Check(ctx)`;
   `--check` 时打印后退出(检查失败退出码 1)。
2. 目标版本:显式 `--tag` 优先,否则 `info.Latest`;两者皆空 → 报错「升级源暂无发布」。
3. 版本判定:`isComparable(version.Version)` 为假(dev/本地构建)且无显式 tag/force →
   提示用 `--tag` 或 `--force`;否则无更新且未 `--force` → 打印「已是最新」退出 0。
4. 解析升级目标:
   - 服务已安装:`mgr.OpenService` + `s.Config()` 取 `BinaryPathName`(解析引号/参数,
     取 exe 路径),目标 = 其目录;要求当前进程已提权,否则报错并提示管理员运行;
   - 未安装:目标 = `os.Executable()` 所在目录。
5. 写 `upgrade-status`(`downloading`)→ `Stage` → `applying`;
   记录服务是否在运行 → `stopServiceAndWait(30s)`(未安装跳过);
   `ApplyStaged` → 若 `<dataDir>/bin` 存在则 `CopyStagedBinaries` → `restarting`;
   原在运行则 `startService()`。
6. `CleanupStaleExecutables`;写 `done`(含新旧版本);打印结果(新版本 + 安装目录 +
   「请重新启动托盘客户端以使用新版本」)。
7. 任一步失败:尽力保证旧 exe 已回位、服务按原状态恢复;写 `error`;退出码 1。

`upgrade_other.go`(`!windows`):打印「升级当前仅支持 Windows(Win11)」退出 2。
提权判定用 `windows.GetCurrentProcessToken().IsElevated()`。

### 5. `cmd/pwclient/svc_windows.go`:服务生命周期小工具

新增(供 upgrade 复用,不改 install/stop/uninstall 现有行为):

- `serviceBinaryPath() (string, bool, error)`:取服务配置里的 exe 路径(`BinaryPathName`
  首个参数,去掉引号/`service run` 参数)。
- `serviceState()`:返回 `svc.State` + 是否安装(复用 `serviceQueryState` 的查询逻辑)。
- `stopServiceAndWait(timeout time.Duration) error`:发 Stop 后每 200ms 轮询直到 STOPPED,
  超时报错(避免 bin/ 仍被占用就复制)。
- `startService() error`:`s.Start()`(失败给明确错误)。

### 6. `cmd/pwclient/main.go`

- 顶部包注释与 `usage()` 增加 `upgrade` 子命令说明。
- `switch cmd` 增加 `case "upgrade": runUpgrade(args[1:])`。
- 启动时调用 `pwclient.CleanupStaleExecutables(filepath.Dir(exe), filepath.Base(exe))`
  (尽力清理上次升级留下的 `.old-*`;失败忽略)。
- `printSnapshot` 增加一行「客户端版本:」(`snap.ClientVersion`,空则 `—`)。

### 7. `cmd/pwclient/tray_windows.go`:托盘入口

- 新增控件字段:`verLabel *walk.Label`、`upgradeBtn *walk.PushButton`、`upgrading bool`、
  `upgradeTag string`、`upgradeStarted time.Time`、`updateInfo *version.UpdateInfo`。
- 「状态」页在服务状态行附近增加一行:
  `Label{verLabel}` + `PushButton{upgradeBtn, "检查更新"}`。
- `apply()`:
  - 版本行:`客户端 <guiver> · 服务 <snap.ClientVersion>`(路径不同时附加「托盘副本较旧」提示);
  - `verLabel` 逐拍刷新;IPC 未就绪时禁用检查按钮;
  - 正在升级时按钮文案 `正在升级…(<阶段>)` 且禁用。
- `onCheckUpdate()`:异步(20s 超时)`cli.CheckUpgrade`;结果存 `updateInfo`:
  - 有更新 → 按钮 `升级到 <latest>`;无更新 → 文案回「检查更新」+ 版本行提示「已是最新」;
  - `CheckError` → `actionErr`(不改按钮)。
- `onUpgrade()`:无 `updateInfo` 就先检查;确认有目标 tag 后,
  经 `ShellExecute("runas", exe, "upgrade --tag <tag>", ...)` 提权启动升级进程
  (与 `onInstallService` 同款),置 `upgrading=true` 并提示「已请求管理员权限」。
- `refreshOnce()` 在 `upgrading` 时额外读取 `<dataDir>/upgrade-status.json`:
  `done` → `ShowInfo("升级完成", ...)`、复位按钮、清 `updateInfo`、刷新状态;
  `error` → `actionErr` + 复位;超过 10 分钟仍无终态 → 超时提示(状态文件可继续观察)。
- `apply()` 发现 `snap.ClientVersion` 变化时清空旧 `updateInfo`(服务已被别处升级)。

### 8. 测试

- 新增 `internal/pwclient/upgrade_test.go`(linux 可跑):
  - 资产名拼装(带/不带 `v`、amd64/arm64);
  - checksums 解析(命中/缺失);
  - `extractClientZip`:正常包(`pwclient.exe` + `bin/` + `licenses/`)解出正确;
    缺少 `pwclient.exe` 报错;含 `../evil` 路径穿越条目报错;
  - `Stage`:httptest 提供 `releases/latest`、`checksums.txt`、zip;校验 staged 内容;
    校验和不匹配 → 报错且临时目录被清理;
  - `Stage` 回退:候选源 1 返回 500/不可达、候选源 2 正常 → 仍成功;
    全部候选失败 → 报错;
  - `ApplyStaged`:建旧 exe + 旧 `bin/`,应用后内容更新、旧 exe 以 `.old-*` 存在;
    `CleanupStaleExecutables` 清除;`CopyStagedBinaries` 刷新指定 bin 目录;
  - `WriteUpgradeStatus/ReadUpgradeStatus` 原子读写回路。
- `internal/pwclient/ipc_test.go`:新增 `TestIPCUpgradeCheck`(替换
  `rt.checkUpgrade` 为固定返回,断言 `GET /api/upgrade/check` 与 `ClientVersion` 字段)。
- `internal/version/source_test.go`:新增 `TestCandidateSources`:
  - 无 env 镜像 → `[GitHub 官方, gh-proxy]`,且第二源 API/网页/下载 base 为
    `https://gh-proxy.com/https://api.github.com` 与 `https://gh-proxy.com/https://github.com`;
  - `PIPEWRIGHT_RELEASE_MIRROR` 显式配置 → **仅 1 个源**(不回落公共代理),含配置为
    gh-proxy 时不重复。
- `internal/version/update_test.go`:新增 `TestCheckAnyFallback`(第一源 500、第二源 200 →
  返回第二源结果并缓存;全部失败 → `CheckError` 非空);为 `NewCheckerWithClient` 补最小断言。
- Windows 专属编排(SCM/改名)不做 Linux 单测;靠 CI 的 `GOOS=windows go build ./cmd/pwclient`
  保证可编译,并在 Windows 上手工验证(见下)。

### 9. 文档

- [README.md](file:///home/xufan/trea_project/pipewright/README.md) ⑤ pwclient 小节补第 6 点
  `Upgrades`(托盘按钮 + `pwclient upgrade --check/--tag/--force` + UAC + 服务重启),
  并说明「无需配置镜像:GitHub 不可达时自动回退 gh-proxy;要固定走镜像再设
  `PIPEWRIGHT_RELEASE_MIRROR`」。
- [README.zh-CN.md](file:///home/xufan/trea_project/pipewright/README.zh-CN.md)「更新」小节
  增加一条 Windows 客户端升级说明(中文仓无客户端专节,放这里最易发现),含自动回退说明。
- [config.go](file:///home/xufan/trea_project/pipewright/internal/pwclient/config.go) 包注释
  职责列表补「升级(检查/下载/替换/重启服务)」。

## 假设与决策(Assumptions & Decisions)

- **升级源** = 候选源列表(GitHub 官方 → `https://gh-proxy.com/https://github.com`)。
  **默认零配置即可用**:GitHub 不可达/限流时自动回退,用户不必设置环境变量。
- **显式镜像优先且独占**:只要 `PIPEWRIGHT_RELEASE_MIRROR`(config 库内配置 / env)非空,
  就只走该镜像,不再自动回退公共代理 —— 避免内网部署把流量发往第三方。
- 回退仅用于**本次 pwclient 升级**的检查与下载;不改动平台服务端既有的
  `ResolveSource` 单源语义(避免影响设置页升级),也不改动 EasyTier 二进制下载器(本次范围外)。
- 完整性:无论走官方还是代理,`checksums.txt` 的 sha256 必须匹配(代理只是传输通道,不是信任源);
  zip 解包拒绝路径穿越;解出的 `pwclient.exe` 必须存在。
- **升级目标** = 已安装服务的 exe 所在目录;未安装服务时 = 当前运行 exe 所在目录。
  不会顺带替换用户在下载目录里的另一份旧副本。
- **提权**:仅当服务已安装(停服务/写 Program Files)才强制管理员;未安装时可普通权限升级。
- `--check` 不改动任何文件、不需要管理员;检查结果与 `version` 子命令共用 `internal/version`。
- 版本比较按语义版本;`dev`/本地构建无法可靠比较时要求 `--tag` 或 `--force`,不误判。
- 服务原为停止状态时升级后保持停止;原为运行则升级后自动启动。
- 升级是显式动作,**无后台自动检查/静默升级**。
- 托盘重启:升级完成后提示用户重启托盘;不在升级进程里强杀/自动重启 GUI(避免单实例
  互斥与旧镜像句柄竞争)。服务已使用新版本。
- 不改动 goreleaser/安装脚本(现有产物已含所需文件与 checksums)。

## 验证(Verification)

1. `go test ./internal/pwclient ./internal/version`
2. `go vet ./internal/pwclient ./internal/version ./cmd/...`
3. `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/pwclient`
   (CI 另有 arm64 交叉编译)
4. Windows 手工验证:
   - `pwclient upgrade --check`(普通权限)打印当前/最新;断网/限流时给出可读错误;
   - **回退验证**:不设任何镜像,在 GitHub 不可达的网络(或 hosts 屏蔽 api.github.com/
     github.com)执行 `--check` 与升级,应自动经 `gh-proxy.com` 成功且日志可见所用源;
     再设 `PIPEWRIGHT_RELEASE_MIRROR` 指向一个不可用地址,应**只**报该镜像的错误(不回落);
   - `pwclient upgrade`(管理员):下载 → 校验 → 停服务 → 替换 → 新版本启动,
     `pwclient version`/`pwclient status`/托盘版本行显示新版本;`bin/easytier-*.exe`
     被刷新;旧 `.old-*` 下次启动被清理;
   - 托盘「检查更新 → 升级到 vX」:UAC → 状态文件阶段可见 → 完成提示 → 服务重启;
   - 服务原本停止时升级后仍停止;`--tag` 指定版本与 `--force` 行为正确;
   - 校验和损坏(临时改 checksums)时升级失败且旧版本仍可运行。
