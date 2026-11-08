# 本机(控制节点)角色启用:构建机池 / 部署目标 / 网关主机 — 设计方案

> 状态:**已实现(2026-11-08;阶段 0-3 全量落地,见第九节勾选)**。
> 相关:`specs/shared-build-cluster.md`(集群共享构建)、`specs/deploy-target-selector.md`(部署目标选择器)、
> `specs/runner-pool-test-plan.md`(构建机池调度)、`internal/target/local.go`(本机执行基座)。

## 一、背景与目标

当前,构建机、网关主机、部署目标三个角色全部锚定在「登记服务器」(servers 表 + SSH 凭据)上;
运行 Pipewright 的机器(控制机/控制节点)**不能**充当其中任何一个角色:

- **构建机**:构建机池候选来自 servers 表全表扫描,本机不在其中;空选择器=本地构建是直连路径,
  不进池、无标签/槽位/排队/亲和,也不出现在命中预览。
- **部署目标**:选择器与显式 serverIds 都只从 servers 表解析,本机无法入选。
- **网关主机**:网关 = 用户显式勾选的登记服务器列表,本机无法入选。

目标:让控制机**可选启用**这三种角色,与登记服务器在同一套语义(标签、调度、选择器、网关编排)下
参与集群;硬性约束:**默认全部关闭,关闭态行为逐字节不变**。

## 二、现状盘点

### 2.1 三个角色的执行链路与阻断点

| 角色 | 候选/入选 | 执行 | 本机进不去的原因 |
|---|---|---|---|
| 构建机 | `internal/runner/scheduler.go` `candidates()` 直查 servers 表,按 labels/优先级/槽位调度,`pve_node` 排除 | 经 `target.Service`(remoteExec 接口:Exec + ExecWithStdin + Upload)SSH 下沉;工作区 tar 上传 `/tmp/pipewright-remote/`、`sh -c` 解包、远端 CLI 探测(nerdctl>docker>podman) | 本机不是登记服务器;调度器与派发层(`internal/build/remote_stage_exec.go`)拿到的都是裸 target.Service |
| 部署目标 | `internal/deploy/selector_targets.go` `resolveTargets()`:显式 serverIds 优先(任一不存在 → ErrServerNotFound)、否则选择器对 `targets.List()` 全量匹配、零命中=跳过成功、maxTargets 按主机负载裁切 | `deploy/cmdlog.go` 的 exec 包裹 target.Exec;Upload 落真字节;DockerLogin 拉私有镜像 | deploy 拿到裸 target.Service(`main.go` 装配),`__local__` 到不了它 |
| 网关主机 | `servicereg.Settings.ServerIDs` 显式勾选(集群级单行设置,`HTTPPort/HTTPSPort` 默认 80/443);apply 逐台编排 | 经 target.Exec/Upload:docker network/container/cp/exec、`nginx -t` 沙箱、优雅 reload;证书 docker cp 下发;静态发布复用 deployReleaseOne | servicereg 拿到的服务只包了日志装饰器(ObservingTarget),未包本地执行;本机无法入选 |

### 2.2 已有的本机执行基座(方案的地基)

- **`__local__` 哨兵**(`internal/target/local.go`):`LocalHostID = "__local__"`,`WithLocalHost` 装饰器把
  Get/Test/Exec/ExecWithStdin/ExecStream/ExecInteractive/Upload 路由到本机 os/exec(不经 shell、无凭据面);
  Update/Delete → `ErrLocalHostImmutable`;Test 返回 GOOS 信息(恒可达);Upload 本地写文件(自动建父目录,0644)。
  **未覆写**:DockerLogin、Download、List、Dial。
- **装配现状**:`WithLocalHost` 仅包进容器域/终端/指标(httpapi/router.go 的 `csv` 实例);平台 HTTPS、
  registry 栈、apt-cacher 栈(本地 docker compose)、EasyTier Supervisor(本地子进程)全部走
  「本机自托管惯用法 = os/exec,从不 SSH 自己」。
- **本机观测先例**:多机状态总览的本机卡片走独立端点 `GET /api/system/metrics`(server_metrics.go 本地采集);
  前端哨兵常量 `LOCAL_HOST_ID = '__local__'`(web/src/api/servers.ts)与后端对齐。
- **本地构建先例**:空选择器=本地构建(`internal/build` DetectDriver 本地探测 docker/nerdctl/podman;
  瘦控制机 `ErrNoContainerCLI` → errDriver + WithRemoteOnlyDriver,本地容器操作诚实报错)。

### 2.3 硬约束

- servers 表:`credential_id TEXT NOT NULL` 外键(0015 迁移)——本机没有 SSH 凭据可绑,「登记一条本机行」
  在 schema 层面就不成立;`kind` 域校验仅 standard|pve_node。
- 不变量(代码注释已明示):「本机(控制节点)不是登记服务器」——不进 servers 表、不可创建/改/删。
- pwclient 是纯组网/代理客户端(EasyTier 接入 + SOCKS5 + 安全组),不承载构建/网关/部署能力,不回归服务端 API。
- `WithLocalHost` 对真实 serverID **逐字透传**——这是「关闭态零行为变化」的结构性保证(已被容器域长期验证)。
- **选择器语法当前不认 `__local__`**:`runner.termOK` 要求 term 首字符为字母数字,`__local__` 以 `_` 开头 →
  `PinnedServer("server:__local__")` 返回 false、`ParseSelector` 判非法。任何「钉死本机」的设计必须先做语法豁免(见 4.6)。

## 三、方案选型

| 方案 | 结论 | 理由 |
|---|---|---|
| **A. 扩展 `__local__` 哨兵** | **采用** | 不触碰 servers 表结构与外键(仅新增单行配置表);透传性质保证默认关闭=逐字节不变;与本机自托管惯用法一致;复用容器域已验证的执行基座 |
| B. 在 servers 表登记一条本机行 | 否 | credential_id NOT NULL 外键无解;一旦落表,ssar 装机、daemon.json 下发、certdeploy、隧道绑定等几十个 SSH 型消费方全部把它当 SSH 机器;违背既有不变量;`GET /api/servers` 爆炸半径不可控 |
| C. 控制机跑 pwclient | 否 | pwclient 无构建/网关/部署能力,且服务端不能当自己的客户端,方向错误 |

**冻结决策**:

1. `__local__` 字面量保持三处对齐(target/local.go、web/src/api/servers.ts、httpapi/server_metrics.go),
   新增消费方一律引用 `target.LocalHostID`。
2. 本机**永不**落 servers 表;`GET /api/servers` **永不**包含本机。
3. `ErrLocalHostImmutable` 语义保留:本机仍不可作为登记服务器增删改。
4. 三角色三开关,默认全关;仅 admin 可配置(与服务器管理同权限级)。
5. `server:__local__` 是**唯一的哨兵语法豁免**,由 4.6 定义;不放宽其他选择器/标签项的字符集。

## 四、公共基建(三角色共用)

### 4.1 本机角色配置(新单行表 `local_node`)

仿 `mesh_settings` 单行模式,新包 `internal/localnode`:

```sql
CREATE TABLE local_node (
    id             TEXT PRIMARY KEY,        -- 恒 'local'
    build_enabled  INTEGER NOT NULL DEFAULT 0,
    build_labels   TEXT    NOT NULL DEFAULT '',
    build_max_builds INTEGER NOT NULL DEFAULT 0,  -- 0 = 调度器全局默认(与 servers.max_builds 同语义)
    build_priority INTEGER NOT NULL DEFAULT 0,    -- 0-100,与 servers.priority 同语义
    deploy_enabled  INTEGER NOT NULL DEFAULT 0,
    gateway_enabled INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL
);
```

- 迁移 sqlite + mysql 双方言(当前最新为 `0111`,本表取 `0112`);仅新增表,不动既有列。
- 校验:labels 语法与 `runner.ParseSelector` 同源(k=v 项/项数/长度);max_builds 0..64;priority 0..100;
  build 与 deploy/gateway 独立开关。
- HTTP API:`GET/PUT /api/local-node`(admin + CSRF,复用既有中间件);平台不支持的角色返回
  `422 local_node_platform_unsupported`(见 4.4)。
- 前端:服务器页本机卡片(已有)增加三个角色开关 + 池参数(labels/槽位/优先级)。

### 4.2 装配层包装(cmd/pipewright/main.go)

- **每角色一个包装实例(关键)**:`WithLocalHostRoles` 只回答「本角色开关是否启用」。deploy、servicereg(网关)、
  调度探活/地址解析各持**独立实例**,不能共用一个——否则 `deploy_enabled=false` 时仍可经显式 serverIds 命中
  本机执行,`gateway_enabled=false` 时仍可把 `__local__` 写进 `Settings.ServerIDs`。未启用角色对 `__local__`
  的 Get/Exec/Upload/List 一律返回显式错误(`ErrLocalRoleDisabled` → 422),绝不静默落到本机执行。
- deploy:`deploy.New(<部署角色包装>, ...)`(现传裸 targetSvc)。**不再外挂 `ObservingTarget`**——
  deploy 自身已由 `deploy/cmdlog.go` 的 `s.exec → execObserved` 逐条回显命令与输出,外层再包会双份回显,
  破坏「关闭态逐字节不变」。
- servicereg:`servicereg.New(st.DB, deploy.ObservingTarget(<网关角色包装>), credVault)`——顺序保持
  `ObservingTarget` 在外、本地包装在内:命令回显先于路由,本机执行同样进运行日志。
- 调度器健康探针:`runner.New(st.DB, targetExister{<构建角色包装>})` 与
  `runner.NewScheduler(..., targetProber{<构建角色包装>})` 都换成包装实例。注意 `targetExister` 目前用裸
  `targetSvc`(`main.go:398`),不改它则 `server:__local__` 在 `SaveSelector` 会被判「服务器不存在」。
- serverAddrResolver / serverHostResolver 注入网关角色包装实例。

### 4.3 `target/local.go` 补齐(唯一的坑:嵌入接口的方法走不到装饰器)

- `DockerLogin`:本地跑 `docker login [<url>] -u <user> --password-stdin`(口令经 stdin,不进 argv/日志)——
  部署拉私有镜像必需。**凭据来源需要新增契约**:装饰器只嵌入 `Service` 接口,拿不到 vault;需在 `target` 包
  新增可选接口(如 `OpenRegistryCredential(ctx, credentialID) (user, pass string, err error)`),由具体实现
  提供、装饰器类型断言调用;实现缺失时返回明确错误,不静默跳过。
- `Download`:本地文件流式读回(部署域不用,补齐以保持接口对称;集群共享构建回拉属延展项,见十)。
- `List` 扩展:**新构造器** `WithLocalHostRoles(base, rolesFn)`(旧 `WithLocalHost` 签名与行为冻结不变):
  `rolesFn` 返回「是否出现在服务器列表 + labels/max_builds/priority」,开启时在 List 尾部追加合成 Server
  (id=__local__,Kind 空 → `IsPVENode()` 天然 false,pve 拦截分支无需改动)。deploy / servicereg / 调度器包装
  使用该构造器;`GET /api/servers` 用的裸实例不受影响。
- Get 合成 Server 视图补齐展示字段(labels 等,只读);`CredentialID/CredentialName` 为空,前端显示为「-」。

### 4.4 OS 门禁

部署与网关的全部脚本都是 POSIX 假设(`sh -c`、`tar`、`bash /dev/tcp` 端口探测、curl、`ss`、`ip route`)。
**deploy_enabled / gateway_enabled 仅当控制机 `runtime.GOOS == "linux"` 时可开启**(保存与解析两处校验,
否则 422)。build_enabled 不受限(本地构建走本地 driver 路径,与平台运行平台无关)。

### 4.5 观测、审计与安全

- 本机执行的命令与输出照常经 cmdlog 回显 + mask 兜底(与远端同一口径,运行日志不区分对待)。
- `PUT /api/local-node` 落审计。
- **安全注记(UI + 文档强警告)**:启用部署/网关角色 = 把**控制面执行权**交给流水线作者——deploy 的
  「命令型部署」直接以 `sh -c <restartCommand>` 在本机执行(`deploy/deploy.go` `runCommandOnly`),
  gateway 角色则获得本机 docker 与宿主目录写权。控制机持有数据库、制品、证书、vault 与全部远端 SSH 凭据,
  **这不是「共享构建机接受对端命令」的同一信任级**,而是控制面 RCE。落地要求:
  - 仅 admin 可开,变更落审计,可随时关闭止血;
  - UI 首次开启需二次确认,并明示「流水线作者可在控制节点执行任意命令」;
  - 文档建议仅对内网/可信团队启用;项目级白名单作为后续增强项(本期只做告警)。
- **执行身份与 PATH**:本机命令继承平台进程用户与环境,`systemctl restart`、写 `/opt`、`ln -sfn` 等受其权限
  约束;PATH 也可能与 SSH 登录 shell 不同(docker/nerdctl 常不在默认 PATH)。权限/依赖不足一律走既有
  诚实失败路径,不隐式提权。

### 4.6 `server:__local__` 选择器豁免(新增,前置依赖)

三个域都依赖「用选择器钉死本机」,但当前语法不允许(见 2.3)。采用**最小白名单**,不通用放宽:

- `runner.PinnedServer` / `ParseSelector`:仅当 `server:` 项的值恰为 `target.LocalHostID` 时豁免字符集校验
  (`runner` 已 import `target`,无循环依赖);`peer:` 与标签项规则不变。
- `runner.Service.SaveSelector` 的存在性校验改为构建角色包装(见 4.2),使 `server:__local__` 通过
  `Exists`;`httpapi/deploy.go` 的前置 `ValidateSelector` 因语法豁免自然放行。
- **build 角色未启用时的可读报错(决策 B)**:`runner.ServerExister` 由 `Exists(ctx, id) bool` 扩为
  `(bool, error)`(runner 包自有接口,非冻结契约;全仓仅 `targetExister` 与测试 `fakeExister` 两个实现):
  false+nil = 机器不存在(→ 既有 `ErrServerNotFound`,文案与回归不变);false+非 nil = 缺失且带因。
  适配器映射(一律用 `errors.Is`,`target.Get` 的错误可能被包裹):`Get` 成功 → (true, nil);
  `ErrNotFound` → (false, nil);**`ErrLocalRoleDisabled` → (false, err)**;其余错误 → (false, nil)
  (维持既有伪装,不为 B 扩大变化面)。
- **错误在 runner 层转译,不穿透抽象**:`SaveSelector` 用 `errors.Is(err, target.ErrLocalRoleDisabled)`
  转成 `runner.ErrLocalRoleDisabled` 后返回;`httpapi/runner.go` 只在 `writeRunnerError` 增加
  `422 local_role_disabled`(人读:本机构建角色未启用)分支。这样既不再伪装成「指定的 runner 服务器
  不存在」,也不破坏 `ServerExister`「不直依赖 target」的初衷。
- **运行期对齐**:本机候选提供者恒注册,build_enabled=false 时返回 `(nil, runner.ErrLocalRoleDisabled)`。
  调度器侧的错误吞噬与判定顺序见 5.1(标签路径静默跳过,钉死本机才上抛);关闭态下标签选择器的
  候选集与改造前逐项相等。
- 豁免只对 `target.LocalHostID` 常量成立,不提供可配置 id。
- 明确否决的备选:放宽 `termOK` 允许下划线开头(影响全部标签/选择器校验);改用新的合法 id
  (破坏三处已冻结常量与前端哨兵)。

## 五、构建机角色(build_enabled)

### 5.1 调度候选注入

- `runner.Scheduler` 增加**恒注册**的本机候选提供者(main 装配注入,从 local_node 读取):
  `candidate{id: LocalHostID, name: "本机(控制节点)", labels, maxBuilds, priority, kind: standard, shared: false}`;
  `build_enabled=false` 时返回 `(nil, runner.ErrLocalRoleDisabled)`。
- **错误吞噬规则(关键,否则默认关闭态会被打破)**:`candidates()` 对 provider 错误必须区分路径——
  仅当 `isPinned && pinned == target.LocalHostID` 时上抛 `ErrLocalRoleDisabled`;标签路径(含
  `AcquireShared` 的标签路径)一律忽略该错误、按「不产候选」处理。否则三开关默认全关时,任意标签
  选择器构建都会失败,直接违反第 8 节。
- **判定顺序**:本机角色检查 → `pve_node` 拦截 → `sharedOnly/ErrServerNotShared` → 正常匹配。
  保证 `AcquireShared + server:__local__` 得到「本机构建角色未启用」而非误报 `ErrServerNotShared`
  (本机永不 shared,顺序决定错误可读性)。
- 开关开启 → 参与 priority 降序 → 流水线亲和 → 负载 的全套排序、槽位(max_builds)、FIFO 排队、
  release 唤醒,零特判。关闭态下钉死 `server:__local__` 的任务在调度期得到带因失败,而非泛化「无命中」。
- 空标签本机不参与标签圈选(与登记服务器同一语义),但可被 `server:__local__` 钉死(语法豁免见 4.6)。
- 探活:经包装的 `Test(__local__)` 恒 OK(5min 正缓存/30s 负缓存语义不变)。

### 5.2 执行分流(关键设计:不走远程 POSIX 链)

派发层(`internal/build/remote_stage_exec.go` dispatch / dispatchRemote)在 acquire 返回
`target.LocalHostID` 时**不进入**远程执行链(tar 上传 + `sh -c` 解包 + 远端 CLI 探测),改走本地执行原语
(`runScriptJobIsolated`(script)/ 本地 driver(build_image,含 build/push/save 全链));acquire 已发生、
闭包级 `defer release` 不变,槽位/排队语义保留。

**需要补齐的上下文(不能宣称「零新执行代码」)**:

- `runScriptJobIsolated` 需要 project/settings/upstreamEnv,而派发闭包当前只有 `r/stage/jb/rep`;
  需从 `runStageMixedTopo` / 本地执行器已有的装配处提取一个「单节点本地执行」薄封装。
- `svcNetwork` 传 nil:阶段 `services:` 网络只对本地组任务可用,**钉死到具体机器的任务(远端现状、本机同理)
  本来就访问不到**——这是既有语义,不是本机角色引入的回归。若未来要让钉死任务使用服务网络,需把服务
  生命周期提升到派发层,另立改造项。
- 上游产物/env 仍按 `needs` 解析结果注入,与远端节点口径一致。

**收益**:

1. 与「空选择器=本地构建」共享同一套本地实现,只多一层薄封装;
2. 不引入 POSIX 依赖——Windows/macOS 控制机也能作为池内构建机(能力与本地构建完全一致,
   无容器 CLI 时经 ErrNoContainerCLI 诚实失败);
3. 省掉本机 tar 打包/解包的往返开销。

**容量口径**:空选择器直连本地构建不进调度器、不占槽;池内本机(标签圈选/钉死)占槽。两者可叠加,
`build_max_builds` 只约束后者,不构成整机上限——文档与 UI 一并明示。

日志文案与既有口径呼应:「→ 构建机:本机(控制机)(选择器 …)」。

### 5.3 前端

- RunnerPanel 命中预览(selectorMatch)与 RunDetail 部署预览:前端经 `GET /api/local-node` 取本机角色与
  labels,**仅当开启时**在机器列表尾部追加本机条目参与匹配——共享的 `GET /api/servers` 不动。
- 标签字典(`labels.New(st.DB, targetSvc)` 用裸服务)不会出现本机标签:若需要选择器建议包含本机标签,
  前端在开启时额外合并 `/api/local-node` 的 build_labels(仅影响建议项,不改变字典存储)。
- 选择器输入框需允许 `server:__local__`:前端 match 本身不校验字符集,后端豁免(4.6)后即可保存/预览。
- runner 配置 DTO 会把 `server:__local__` 派生为 `runnerServerId = "__local__"`:前端对该 id 显示
  「本机(控制节点)」,**不要**用 serverId 反查 `GET /api/servers`(该接口永不含本机)。

### 5.4 语义冻结

- 「空选择器 = 本地(直连,不经调度器)」路径**保持不变**;池内本机是新增的第三条路(经调度器、占槽)。
  两者可叠加,`build_max_builds` 只约束池内本机任务,不构成整机并发上限(见 5.2 容量口径)。
- `peer:` 选择器仍显式 422(shared-build-cluster 冻结边界),与本机角色互不干扰。
- 项目级并发上限、矩阵展开、产物归档等上游语义零改动。

## 六、部署目标角色(deploy_enabled)

### 6.1 目标解析(解析主体不变,新增角色门禁)

- 显式 serverIds 含 `__local__`:存在性检查经部署角色包装 `Get` 返回合成 Server → 通过;回滚回放
  (Retry/Rollback 按 ServerIDs 回放)同样可用。**`deploy_enabled=false` 时必须显式拒绝**
  (`ErrLocalRoleDisabled` → 422 `local_role_disabled`),不能因为包装存在就静默执行。
- 钉单机 `server:__local__`:语法豁免见 4.6,通过后同显式 serverIds;合成 Server Kind 空 →
  pve 拦截分支天然不触发,`resolveTargets` 主体零改动。
- 标签圈选:经 4.3 的 List 扩展命中本机(AND 语义与登记服务器一致);未启用时 List 不追加,零命中语义不变。
- 零命中=跳过成功、maxTargets 负载裁切(经包装 exec 在本机跑同一静态 `sh -c` 采集脚本)、
  语法非法 422:全部语义不变。

### 6.2 执行路由

deploy 持有的服务换成**部署角色包装**(不套 `ObservingTarget`,命令回显仍由 deploy 自己的
`cmdlog.go` 负责,避免双份)后,部署链路自动落本机:

- exec(部署脚本、健康门、端口探测、prune)→ 本机 os/exec;
- Upload(tar.gz/jar/文件清单)→ 本地写文件(0644,自动建父目录);
- DockerLogin → 4.3 新增的本地实现(含 `OpenRegistryCredential` 凭据契约);
- current 软链原子切换(ln -sfn + mv -T)、restartCommand、回滚重切:同一批 POSIX 命令在本机执行。

### 6.3 门禁与降级

- OS 门禁见 4.4;开启后若主机缺 curl/bash 等依赖,按既有健康门/端口探测的诚实失败路径呈现,不加特判。
- 权限与 PATH 见 4.5:重启服务/写系统目录受平台进程用户约束,不隐式提权。
- 部署实例端口(20000-30000)在本机发布时与既有端口探测逻辑自洽;同机兼任网关时,upstream 只在
  「本机是唯一网关主机」时走 host.docker.internal(约束见 7.2),无端口交集。

## 七、网关角色(gateway_enabled)

### 7.1 主机入选与编排

- `Settings.ServerIDs` 接受 `__local__`:`UpdateSettings` 对 ServerIDs 只做去重归一、不做存在性校验
  (`normalizeServerIDs`)。**需新增角色门禁**:`gateway_enabled=false` 时拒绝 `__local__`
  (422 `local_role_disabled`),避免「包装存在即生效」;`PrimaryServerID = ServerIDs[0]` 可为本机。
- 编排(docker network inspect/create、`docker inspect` 比对、`docker rm -f` + `runNginxContainer`、
  证书 Upload→`docker cp`、`nginx -t` 沙箱、优雅 reload、TCP 上游 `docker network connect`、
  镜像升级 pull+digest 比对)经包装服务在本机原样执行——**路由语义零改动**。
- **端口映射是唯一例外**:共存模式需按 `serverID` 生成回环绕定与不同端口,涉及 `runNginxContainer` /
  `desiredPortMap` / `hostPortsBusy` 三个函数的分支(见 7.3);`inspectPortMap` 只读 HostPort、不读
  HostIp,绑定地址变化不会被比对发现——改动时需补 HostIp 比对(纵深防御:回环端口与集群端口互异的
  校验已保证 HostPort 必变、重建必被触发,HostIp 比对防的是未来两端口可能相等的回归)。
- 前置与 registry/apt-cacher 栈一致:控制机需 docker CLI;缺失时走既有诚实降级。
- 前端:网关主机选择器/网关状态页在本机开启角色后追加本机卡片。

### 7.2 上游地址解析特判

`serverAddrResolver.rawHost(__local__)` 返回 **`host.docker.internal`** 而非合成 Server 的 "localhost":
网关 nginx 容器启动时已带 `--add-host host.docker.internal:host-gateway`,从容器视角这是访问宿主
(即本机)实例的正确地址。mesh 路由规划用的 `resolve(__local__)` 返回本节点 EasyTier 虚拟 IP
(未启用 mesh 时报错)。

**约束(易错点)**:该地址只在「网关容器跑在本机」时成立。实例池 upstream 的地址会在**所有**网关主机上
渲染同一份 nginx 配置,若 `ServerIDs` 同时含远端网关主机,远端容器里的 `host.docker.internal` 指向的是
远端宿主机 → 静默错路由。因此:

- **适用面**:仅指「声明了实例注册(`regService` 非空,实例池 upstream 需按承载机解析地址)的部署」;
  普通部署到本机不受此限。
- `__local__` 作为**实例承载机**时,仅当它同时也是**唯一**的网关主机(即 `ServerIDs` 全为本机)才允许;
- 混合网关(本机 + 远端)场景下显式拒绝。**权威校验在 servicereg 渲染期**:`resolveInstanceAddrs`
  解析实例 upstream 时按「当前 ServerIDs + 当前目标机集合」复核,冲突 → apply 显式失败并落
  `LastApplyErrors`。这是唯一能覆盖「先部署实例、后把本机加进网关 ServerIDs」顺序的拦截点;
  保存期只能做 UI 提示,不能作为权威(流水线选择器与网关 ServerIDs 是两次独立保存)。
- deploy 侧可选前置校验:deploy 不直接读 servicereg settings,当前 `servicereg` 不 import `deploy`,
  反向依赖会倒置层次——注入 `InstanceHostGuard` 接口,仅在该部署声明实例注册时调用;回滚回放复用
  同一入口。若不做前置校验,以 servicereg 渲染期为准即可。
- 仅本机是网关主机时,本机自身 upstream 走 `host.docker.internal`,其余远端机器仍走 `serverHost` 原逻辑。

### 7.3 与宿主 nginx 的端口共存(核心)

冲突是真实的:平台 HTTPS 在宿主 nginx 上占 `listen 80` + `listen 443 ssl`;网关容器默认占宿主
80/443。三层处置:

**第 1 层(现状保留):探测 + 显式失败。** 新建网关前 `hostPortsBusy`(`ss -ltn`)探测生效端口,
占用 → `ErrPortConflict` 显式失败,绝不静默抢端口。

**第 2 层(零改造):网关宿主端口可配。** `HTTPPort/HTTPSPort` 是设置项,换成空闲高位端口即可起容器。
限制:集群级设置(所有网关主机一起换)+ 外部访问需带端口。适合内网/验证。

**第 3 层(正解):宿主 nginx 做域名入口,网关容器做后端。** 当**平台 HTTPS 已启用**且本机被启用为
网关主机时:

- **回环端口**:`servicereg.Settings` 新增 `localHTTPPort / localHTTPSPort`(默认 18080/18443;校验范围、互不相等、
  不得与集群 HTTPPort/HTTPSPort 相同),**仅作用于 `__local__` 网关主机**。共存模式下网关容器绑定
  `-p 127.0.0.1:<localHTTPPort>:80 -p 127.0.0.1:<localHTTPSPort>:443`(回环绑定,外网不可直达);
  其他网关主机不受影响,照旧绑集群端口。直连模式(宿主 80/443 空闲、平台 HTTPS 未启用)下本机与其余
  网关主机完全一致,不引入回环绑定。`hostPortsBusy` 对 `__local__` 探测「生效端口集」
  (共存模式查回环端口;直连模式查集群端口)。**共存模式下回环端口必须 >0**:未配置时
  `syncGatewayEntry` 显式失败(不渲染 `127.0.0.1:0`),`ApplyGateway` 侧再留一道端口范围守卫。
- **入口 vhost**(platformhttps 侧新增渲染,文件 `/etc/nginx/conf.d/pipewright-gateway.conf`,独立
  marker,与平台 vhost 并存):每个基域一组 server 块——

  ```nginx
  server {
      listen 443 ssl;
      server_name *.efg.com efg.com;            # 通配符;证书走 DNS-01(证书管理已支持)
      ssl_certificate     <certsBase>/<基域>/fullchain.pem;
      ssl_certificate_key <certsBase>/<基域>/privkey.pem;
      location / {
          proxy_pass https://127.0.0.1:18443;   # ★ 转发到网关容器 443 回环端口(再加密)
          proxy_ssl_verify off;                 # 回环自连;与 mesh 反代网关同款惯用法
          proxy_ssl_server_name on;
          proxy_ssl_name   $host;               # SNI 透传原域名,网关按 server_name 选证书/站点
          proxy_set_header Host $host;
          # WS 升级 + SSE 长读超时:复用 writeProxyLocation 模式
      }
  }
  server {
      listen 80;
      server_name *.efg.com efg.com;
      return 301 https://$host$request_uri;     # $host 保留原子域名
  }
  ```

- **为什么必须转发到 443(双重 TLS 终结)而不是 80**:网关 nginx 对证书就绪站点渲染的 80 块是
  `return 301 https://$host$request_uri`——若宿主 443 终结后转发到网关 80,网关会再回一个 301 给浏览器,
  形成无限重定向循环;转发到网关 443(自签证书 + verify off)则网关语义分毫未动。repo 已有同款先例
  (mesh 对网关的反代:「打 https + SNI,对端 80 是 301 跳转不能走,verify off」)。
- **vhost 匹配顺序安全**:nginx 精确域名优先于通配符——平台域名(`pw.efg.com`)不会被 `*.efg.com` 劫走;
  平台域名不在基域下时两者互不相交。
- **证书就绪门**:基域通配符证书签发并下发(`certs/<基域>/`)后才渲染入口 vhost;未就绪时不渲染,
  UI 明示先去证书管理签发(DNS-01 通配符)。
- **与 platformhttps 的接口(必须补齐;现有实现是单文件/单证书全量收敛)**:
  - 新增 `platformhttps.ApplyGateway(ctx, GatewayEntrySpec)` / `RemoveGateway(ctx)`:spec 含「基域列表 +
    每个基域的证书 PEM/私钥 + 回环端口」;由 platformhttps 写 `/etc/nginx/conf.d/pipewright-gateway.conf`
    与 `certs/<基域>/`,证书从 certmgmt 解密落盘(不复用网关容器的证书卷)。
  - **同一事务**:两个 conf 文件一起 `nginx -t`(失败整体回滚上一版)后再热加载,不得分别校验。
    **证书同事务回滚**:安装前把 `certs/<基域>/{fullchain,privkey}.pem` 备份到临时目录(原本不存在则记录
    「无备份」),`nginx -t` / `nginx -T` 失败时连证书一起还原/删除——只回滚 conf 会留下「conf 已还原、
    证书却已被换掉」的半态(平台 vhost 与本入口可能共用同名文件)。
  - `Disable()` / `RedeployCert()` 需覆盖网关 conf 与基域证书目录(仅在空目录时 rmdir,避免误删平台证书)。
  - **调用方向固定为 servicereg → platformhttps**(servicereg 持平台 HTTPS 句柄),不引入反向依赖。
- **联动**:证书续期(certmgmt 回调)、网关设置变更(基域/回环端口/本机进出 ServerIDs)、平台 HTTPS 开关
  变化 → 重新计算 `GatewayEntrySpec` 并调用上述接口;关闭本机网关 → `RemoveGateway` 删除该 vhost。
  实现落点:
  - servicereg → platformhttps:每次 apply 末尾 `syncGatewayEntry`(本机 apply 自身失败时**不发布**,
    避免把未收敛的后端挂上 443);
  - platformhttps → servicereg:`platformhttps.Service.SetChangeHook`(在释放本服务锁之后回调)接入
    `servicereg.ReconcileLocalGateway` —— 覆盖启用态切换/Apply/Disable;
  - localnode → servicereg:本机角色 PUT 成功后在 httpapi 装配层触发同一条 `ReconcileLocalGateway`
    (角色关闭 → 只摘入口 vhost,不把容器重配到回环)。
- **不采用 stream SNI 透传**:nginx 的 http 块与 stream 块不能同时 bind 443,平台 HTTPS 已占 http 443,
  SNI 路由需重构平台 HTTPS,改造面与风险不成比例。

**限界说明**:外部 80 流量统一 301 到 443;未配证书的站点在直连模式下可经网关 80 直接服务,共存模式下
该路径不可达(443 落网关 default_server 自签证书,浏览器告警)——证书管理自动签发下发使其成为常态外
场景,文档明示即可。

### 7.4 静态发布与镜像升级

- `deploy_frontend` 网关静态发布:`ResolveStaticDeploy` 返回的 ServerIDs 含 `__local__` → 复用
  deployReleaseOne 经包装服务在本机落 StaticRoot + current 软链原子切换(与远端同一套 POSIX 脚本)。
- 网关镜像一键升级:逐台 pull + digest 比对,本机照常参与。

### 7.5 meshroute(网关转发数据面)

`ip route show/replace/del` 在本机执行需 root/免密 sudo——复用 diskadmin/platformhttps 的提权探测口径,
未授权时该子功能诚实失败并提示,不影响网关反代本体。

**本机网关必须跳过路由对账**:`reconcileServerRoutes` 用 `localSourceIP(gwIP)` 当下一跳,而本机的
`gwIP` 就是自己的 mesh IP,会写出 `ip route ... via <自己>` 的错误路由。本机网关不做 route reconcile
(它本身就是本节点,不需要经自己转发);`GatewayProxyNetworks` 是否剔除本机自身 IP 需一并复核
(通告自己的 /32 无意义,对端路由影响需实测)。

## 八、兼容性保障(不影响现有功能的验收口径)

1. **默认关闭 = 逐字节不变**:三开关全关时,调度器候选集、deploy 目标解析、servicereg 编排、
   `GET /api/servers` 响应均与改造前完全一致;包装器对真实 serverID 逐字透传。
   **显式例外项(均受开关门禁,关闭态不生效)**:① deploy 装配换成包装实例(不加 `ObservingTarget`,
   回显链路不变);② servicereg 多一层本地包装(List 在关闭时不追加);③ 共存模式的端口映射分支;
   ④ `server:__local__` 语法豁免(对真实 serverID 与标签项无影响);⑤ 调度器恒注册本机候选提供者
   (禁用时按 5.1 吞噬错误、不产候选,候选集与改造前逐项相等)。
2. **共享面冻结**:`GET /api/servers` 不含本机;ssar 装机、servercmd 批量命令、certdeploy、daemon.json
   下发、SSH 隧道绑定、备份等 SSH 型消费方一律不见本机(无 DB 行,结构上不可能泄漏)。
3. **不变量保留**:本机不可作为登记服务器创建/更新/删除;`__local__` 恒为哨兵。
4. **既有单测零改动通过**:scheduler、deploy selector、servicereg、WithLocalHost 透传(local_test.go)
   等既有套件不加一行即绿;新增用例只覆盖开启态与特判分支。唯一例外:`ServerExister` 签名扩展(4.6 决策 B)
   连带 `fakeExister` 的机械更新——行为断言不变。
5. **审计与脱敏链路不变**:本机命令同过 cmdlog + mask;开关变更落审计。
6. **可回退**:任一角色关闭即回到关闭态语义;无不可逆迁移(仅新增单行表)。

## 九、测试矩阵

> 已由自动化用例覆盖的项已勾选;真机/HTTP 集成项(手工清单、meshroute 对账、重定向循环链路)按清单逐项验收。

### 域层

- [x] local_node:单行 CRUD 往返;labels 非法/越界 422;build/deploy/gateway 独立开关
- [x] OS 门禁:非 Linux 控制机开 deploy/gateway → `local_node_platform_unsupported`;build 不受限
- [x] 选择器豁免(4.6):`server:__local__` 语法合法、`SaveSelector` 通过、部署前置校验放行;
      非法 id/标签项仍 422;真实 serverID 的选择器行为逐字节回归;build 未启用时
      `SaveSelector(server:__local__)` → 422 `local_role_disabled`(非 `runner_server_not_found`),
      且错误经 `runner.ErrLocalRoleDisabled` 转译(httpapi 不感知 target 错误);
      真实不存在机仍 `runner_server_not_found`(错误映射分流回归)
- [ ] 角色门禁:未启用角色收到 `__local__`(deploy serverIds / gateway ServerIDs / Scheduler 候选)
      → `ErrLocalRoleDisabled` / 不进候选集,不得静默执行;钉死 `server:__local__` 且 build 未启用
      → 调度期带因失败(非泛化「无命中」),标签路径静默排除、候选集不变
- [x] 关闭态回归(关键):build_enabled=false 时任意标签选择器照常调度,provider 错误不得外溢;
      `AcquireShared + server:__local__` 得到 `ErrLocalRoleDisabled` 而非 `ErrServerNotShared`
- [x] Scheduler:注入本机候选后按 labels/优先级/槽位调度;禁用态候选集与注入前逐字节一致(回归)
- [ ] Scheduler:`server:__local__` 钉死可用;空标签本机不被标签选择器命中;全忙 FIFO/唤醒含本机槽位;
      `AcquireShared` 的 sharedOnly 分支不含本机
- [x] dispatch:`__local__` 分流走本地薄封装、保持占槽、defer release;`svcNetwork=nil`;
      script 与 build_image 双路径;`needs` 上游 env 注入与远端口径一致
- [ ] deploy:显式 serverIds 含 `__local__` 解析通过;`server:__local__` 钉死;标签圈选命中本机;
      未启用拒绝;零命中跳过/maxTargets 裁切/422 语义回归
- [x] local.go 新覆写:DockerLogin(口令经 stdin + `OpenRegistryCredential` 契约、实现缺失时报错)、
      Download、List 开关追加(旧 WithLocalHost 行为回归)
- [ ] resolver:`rawHost(__local__)` = host.docker.internal;`resolve(__local__)` = mesh 虚拟 IP;
      混合网关(本机+远端)下、且部署声明了实例注册时,`__local__` 作实例承载机在 servicereg 渲染期
      被拒绝(普通部署到本机放行;deploy 前置 guard 若实现则同口径)
- [x] servicereg:Settings.ServerIDs 含 `__local__` 往返;未启用时拒绝;apply/升级/LastApplyErrors 按机记录
- [x] 回环端口校验:范围/互异/不得等于集群端口;hostPortsBusy 按生效端口集探测;端口映射比对含 HostIp
- [ ] platformhttps:`ApplyGateway`/`RemoveGateway` 渲染与删除;两个 conf 同一 `nginx -t` 事务;
      失败整体回滚;`Disable`/`RedeployCert` 覆盖网关 conf 与基域证书目录
- [ ] meshroute:本机网关跳过 route reconcile;`GatewayProxyNetworks` 本机 IP 处理符合预期
- [x] 入口 vhost 渲染 golden:443 转发网关 443(verify off + SNI + $host)、80 301 $host、精确域名优先;
      证书未就绪不渲染;禁用即删除
- [ ] 重定向循环回归:共存模式下 https 请求经宿主 nginx → 网关 443 全链路无 301 环

### HTTP 层

- [ ] `GET/PUT /api/local-node`:admin-only、CSRF、422 映射、审计落账
- [ ] runner 保存接口:build 未启用时保存 `server:__local__` → 422 `local_role_disabled`(人读文案);
      启用后保存成功且 GET 往返
- [ ] 开启后 RunnerPanel/部署预览/网关选择器数据面(合成条目仅开关开启时出现)

### 手工清单(真机)

- [ ] Linux 控制机:三角色全开,标签选择器把构建派到本机占槽跑通;同时部署到本机(含回滚)与远端机
- [ ] 本机网关 + 平台 HTTPS 共存:子域名经宿主 nginx 访问网关服务(WS/SSE 正常);平台域名不受影响
- [ ] 关闭全部开关:构建/部署/网关行为与改造前一致(逐项回归)
- [ ] 安全与权限:非 root 平台进程执行部署重启/写系统目录时诚实失败;PATH 缺 docker 时给可读提示

## 十、非目标

- Windows 控制机的部署目标/网关角色(POSIX 依赖,明确拒绝并 422);
- SSH-to-self 把控制机登记为普通服务器;
- 控制机跑 pwclient(组网客户端不承载三角色);
- stream SNI 透传(见 7.3);
- 本机作为集群共享构建机(需扩展 AcquireShared 候选与 mesh 契约,延后另立 spec);
- 混合网关(本机 + 远端)下把 `__local__` 作为**实例承载机**(host.docker.internal 只在同机成立,见 7.2);
- 钉死到具体机器的任务使用阶段 `services:` 网络(远端现状即如此,如需支持要提升服务生命周期,见 5.2);
- 本机指标并入多机批量端点(现状独立端点 `/api/system/metrics` 保留)。

## 十一、实施阶段

| 阶段 | 内容 | 规模 |
|---|---|---|
| 0 | 公共基建:local_node 表/服务/API/UI、选择器豁免(4.6)、local.go 补齐(含凭据契约)、三角色独立包装与门禁 | 约 1.5 天 |
| 1 | 构建机角色:调度候选注入 + 单节点本地执行薄封装 + RunnerPanel | 约 1 天 |
| 2 | 部署目标角色:解析豁免 + DockerLogin 凭据契约落地验证 | 约 1 天 |
| 3 | 网关角色:ServerIDs/角色门禁/解析特判/回环端口 + platformhttps 入口 vhost 共存 + meshroute 跳过本机 | 约 3 天 |

每阶段独立可发布;发布前跑第九节矩阵;任一阶段问题可单独关闭对应开关止血。
