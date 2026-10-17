# 服务注册支持 path 与纯静态托管，实现前后端分离一体化部署

## 目标效果（用户视角）

一条流水线两个部署节点即可完成前后端分离项目的一体化发布，共用一个子域名：

- **后端**：`deploy_container` 节点，服务注册绑定 `app` + path `/api`（剥离前缀可选，默认保留）→ `app.efg.com/api` 反代后端容器池；
- **前端**：新增 **`deploy_static`（网关静态发布）** 节点，产物 = dist，**不选机器**，绑定静态服务 `app` + path `/` → dist 自动发布到全部网关主机，由网关 nginx 容器直接托管；
- 同一 FQDN 下不同 path 的服务自动合并进同一个 server 块共存；保存时相同 path 报 409 冲突。

## 设计决策（含已确认项）

1. **path 模型**：`RegisteredService` 新增 `Path`（归一化：必须 `/` 开头、去尾部 `/`，空串 = 根路径 `/`；字符集 `^[A-Za-z0-9._~/-]*$`，长度 ≤128）。FQDN 不变（`name.basedomain`），**同 FQDN = 同 (domain, name)**，多服务靠 path 区分。
2. **冲突规则**：同 FQDN 下 **path 完全相同** → `ErrPathTaken`（409 `path_taken`）；**前缀嵌套允许**（`/` 与 `/api` 必须能共存，nginx 最长前缀匹配天然正确）。DB 唯一约束 `(domain_id, name)` → `(domain_id, name, path)`。
3. **静态服务**：`UpstreamKind` 新增枚举值 `static`。约束：仅 http 协议、Upstream 恒空、UpstreamPort 恒 0（校验放行）、**禁止实例**（AddInstance/EnsureInstance/EnsureInstanceDetached 等返回新错误 `ErrStaticService`）、tcp 服务禁止 path。
4. **前缀语义（已确认）**：新增布尔字段 `StripPathPrefix`（默认 false = **保留前缀**，与现网行为零变化）。剥离时：实例池 `proxy_pass http://pw_x/;`（尾斜杠）；address 变量反代用 `rewrite ^<path>(/.*)?$ /$1 break;`。
5. **静态内容分发（已确认为专用节点）**：
   - 网关容器新增 bind mount：`-v <Settings.StaticRoot>:/etc/pipewright/static:ro`（默认 `/var/lib/pipewright-nginx/static`，Settings 新字段可改）。通过 `nginxBootstrapVer` 2→3 触发一次性容器重建（配置/证书在具名卷不丢）。
   - 每静态服务目录 = `<StaticRoot>/<serviceID>/`，复用现有 release 机制：`releases/<runId>/` + `current` 原子软链切换 + 失败回滚 + prune。nginx 按请求读盘，软链切换即生效，无需 reload。
   - `deploy_static` 节点：无 selector，目标 = 全部网关 ServerIDs（经适配器解析），并行发布；健康门控简化为 `test -f <base>/current/index.html`，失败走既有回滚；跳过 restartCommand 与实例摘挂；PruneInstances 跳过。
   - 静态服务未首发内容前网关返回 404（alias 指向不存在目录，可接受）。
6. **EnsureService 幂等键**：`(DomainID, Name, Path)`；协议冲突检查同此键。ensure 端点对 static 服务放行 port=0/缺省。

## 改动明细

### 1. DB 迁移 0081（sqlite + mysql 双份）
- `internal/store/migrations/{sqlite,mysql}/0081_service_reg_path.sql`
- 加列：`path`（sqlite TEXT / mysql VARCHAR(128)，NOT NULL DEFAULT ''）、`strip_path_prefix`（INTEGER/TINYINT NOT NULL DEFAULT 0）。
- 唯一约束改 `(domain_id, name, path)`：sqlite 照 0068 的表重建模式；mysql `DROP INDEX`（内联唯一索引自动名 `domain_id`）+ `ADD UNIQUE KEY`。
- 同步 `internal/servicereg/store.go`：`serviceCols`、insert/update SQL、`IsUniqueErr` 映射不变。

### 2. 领域层 internal/servicereg/servicereg.go
- `RegisteredService`/`CreateServiceInput`/`UpdateServiceInput` 加 `Path`、`StripPathPrefix`；新常量 `UpstreamKindStatic`；新错误 `ErrInvalidPath`、`ErrPathTaken`、`ErrStaticService`。
- `prepareService`（servicereg.go:636）：path 归一化+校验；static 形态校验分叉（port 0 合法、Upstream 强制空、protocol 仅 http）；唯一性循环重写（现同域 `continue` 于 servicereg.go:701 移除）：同 FQDN（含跨域撞名）且 path 相同 → `ErrPathTaken`；保留 TCP 端口全局唯一与 80/443 保留段检查。
- `EnsureService`（servicereg.go:542）：匹配键加 Path。
- `instance.go`：五个实例写操作对 static 服务返回 `ErrStaticService`。

### 3. nginx 渲染与容器 internal/servicereg/nginx.go
- `renderNginxConf`：http 服务按 **FQDN 分组合并**为一个 server 块（替代现在每服务一块，nginx.go:221-232），组内按 path 升序输出多个 location；单服务单根路径的输出与现状保持一致（golden 兼容）。
- location 路径 = `svc.Path`（空 = `/`）；static 分支输出 `alias /etc/pipewright/static/<svc.ID>/current/; index index.html; try_files $uri <path>/index.html;`；503 维护态仅适用反代形态。
- upstream/变量命名加入 path 段（`pw_<name>_<pathkey>`，`/`→`_` 净化，重名追加序号去重，排序保证确定性）。
- `Settings` 加 `StaticRoot`（默认 `/var/lib/pipewright-nginx/static`，`validateSettings` 校验绝对路径）；`runNginxContainer`（nginx.go:416）加 `:ro` bind mount；`nginxBootstrapVer` 升位触发重建。

### 4. HTTP API internal/httpapi/servicereg.go
- `serviceRegServiceRequest` 加 `path`/`stripPathPrefix` 指针字段；创建/更新必填集不变（static 时 `upstreamPort` 可为 0/缺省，servicereg.go:459-462、508-511 调整）；DTO 输出 path/stripPathPrefix。
- `writeServiceRegError` 加映射：`ErrPathTaken`→409 `path_taken`、`ErrInvalidPath`→400、`ErrStaticService`→409。
- settings DTO 加 `staticRoot`。

### 5. 部署链路
- `internal/deploy/instance_rolling.go`：`InstanceGateway` 接口加 `ResolveStaticDeploy(ctx, serviceRef) (serverIDs []string, baseDir string, err error)`；main.go 适配器实现（读服务 + Settings.StaticRoot 拼 `<StaticRoot>/<serviceID>`）。
- `internal/deploy/` 新增 static 发布路径：`deploy_static` 节点 cfg 进 `DeployForStage` 后跳过 selector 解析、目标 = 适配器返回的网关列表；单机执行 `deployReleaseOne` 变体（releaseBase=静态目录、跳过 restartCommand、健康门控 = current/index.html 存在性、失败回滚软链、不做摘挂/实例注册/PruneInstances）。产物类型非 dist → 明确报错。
- `internal/build/dag_stage_exec.go`：`isDeployJob`（:97-101）加 `deploy_static`；`runDeployJob`（:726）加分支：无 selector、透传 artifactJob/artifactName/regServiceId。

### 6. 流水线前后端
- `internal/pipeline/pipeline.go`：`collectRegServiceRefs`（:281-326）纳入 `deploy_static` 节点。
- `web/src/components/pipeline/jobConfigSchema.ts`：新 `DEPLOY_STATIC_FIELDS`（artifactJob/artifactName + REG_BINDING_FIELDS，无 selector）+ 类型 spec + `PICKABLE_TYPES` 收录。
- `web/src/components/pipeline/JobDrawer.vue`：`DEPLOY_REG_TYPES`（:429）加 `deploy_static`。
- `web/src/components/pipeline/ServiceRegBinding.vue`：静态模式（无端口必填，改必填可选 path），`generate()`（:63-94）静态分支调 ensureService(kind=static, port=0, path)。
- `web/src/views/ServiceRegistry.vue`：创建/编辑表单加 path 输入 + 「剥离路径前缀」开关 + upstreamKind 静态选项（选中时隐藏端口/上游/实例面板，展示"内容由流水线静态发布下发"提示）；列表行展示 `fqdn + path` 与 static/strip 标签；预览拼接 path。
- `web/src/api/servicereg.ts`：类型与请求体加 path/stripPathPrefix。
- i18n：`web/src/i18n/locales/*/serviceReg.ts`、`pipelineJob.ts` 共 9 语言补文案。

### 7. 文档与测试
- `README.zh-CN.md` 服务注册章节：path/静态托管/新节点说明。
- `internal/servicereg/servicereg_test.go`：golden 渲染（同 FQDN 合并多 location、static alias/try_files、剥离前缀尾斜杠、upstream 命名去重）；prepareService path 校验/冲突（同 path 拒绝、嵌套允许）；EnsureService 按 path 幂等；static 禁实例。
- `internal/httpapi/servicereg_test.go`：path 冲突 409、ensure 静态 port=0、settings staticRoot。
- `internal/deploy` 测试：静态节点目标来自适配器、releaseBase 覆盖、index.html 门控失败回滚、不触实例接口。
- 迁移经 `storetest.ForEachDialect` 双方言验证。

## 实施顺序

1. 迁移 0081 + store 层 → 2. 领域模型/校验/冲突 → 3. nginx 渲染 + 容器挂载 + Settings → 4. httpapi DTO/错误映射 → 5. deploy 静态路径 + 适配器 + main 装配 → 6. 流水线节点（后端 + 前端 schema/表单）→ 7. ServiceRegistry/ServiceRegBinding UI + i18n → 8. 测试补齐 + 文档。每层配套单测，`go test ./...` 与 web 构建/类型检查通过为准。

## 兼容性说明

- 存量服务 path=''、strip=false，渲染输出与现状等价（单服务单 FQDN 的 server 块结构不变）。
- 网关容器因 bootstrap 版本升位在下一次 apply 时一次性重建（数秒中断，与既有端口变更重建同一模式）。
- 前缀保留为默认，现有反代行为零变化。