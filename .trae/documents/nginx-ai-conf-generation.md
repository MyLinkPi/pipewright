# 服务注册网关：代码仓库样例驱动的 AI nginx 配置生成（conf.d 分文件 + 输入指纹复用）

## Summary（概要）

当前服务注册网关把全部服务**全量渲染进单个** `/etc/pipewright/nginx.conf`（Go 字符串硬编码拼接，每服务固定 upstream + 固定反代头），既无法满足定制化转发需求，单文件全量改写本身也是错误做法。

本计划改为 **主 conf + include 每服务独立 conf** 的结构，并新增 AI 生成能力：

1. **配置结构重构**：主 `nginx.conf` 只含全局指令与 `include /etc/pipewright/conf.d/http/*.conf`、`include /etc/pipewright/conf.d/stream/*.conf`；每个注册服务一个独立 conf 文件（`pw_<svcID>.conf`）。无样例的服务走**确定性按服务渲染**（现有渲染逻辑按服务拆分复用），有样例的服务可由 **AI 参考样例生成**。
2. **工作流配置样例路径**：流水线部署节点 config 新增 `nginxSampleConf` 键（代码仓库内相对路径，如 `nginx/petsuper.conf`），部署时自动回填到对应注册服务并刷新样例内容指纹；服务编辑界面也可直接维护。
3. **输入指纹复用**：AI 生成结果连同**输入指纹**（样例内容 + 服务关键字段 + prompt 契约版本 + extraPrompt 的 sha256）存库；指纹不变直接复用、零 AI 调用。实例成员变化（加机器/摘机器/换 IP）**永不触发重新生成**，由 apply 时的托管 upstream 注入处理。完整场景矩阵见第 3 节。
4. **预览确认后应用**：生成结果前端 diff 预览，确认后落库生效；`nginx -t` 整树校验失败不覆盖旧配置。

## Current State Analysis（现状分析）

### 渲染与下发链路

- [nginx.go](file:///home/xufan/trea_project/pipewright/internal/servicereg/nginx.go) `renderNginxConf`（L90 起，纯函数）：全量渲染**单个** `/etc/pipewright/nginx.conf`。upstream 命名约定 `pw_<svcID>`，成员 `server <ip>:<port> max_fails=2 fail_timeout=10s;` + `keepalive 32;`；http 服务按 FQDN 分 server 块、同域名多 path 合并 location；tcp 服务进 stream 块；`writeProxyLocation`（L466-517）写死一组反代头。无模板文件、无外部 conf 样例（全库 glob `**/*.conf` 零结果）。
- `applyWith`（servicereg.go:1143）→ 逐网关主机 `applyWithServer` → `ensureNginx` → `deployCerts` → `applyNginxConf`（nginx.go:994-1030：上传 → `docker cp` → `nginx -t -c` 校验 → 覆盖 → `nginx -s reload`，校验失败不覆盖）。
- 滚动部署经 `deploy.InstanceGateway`（deploy/instance_rolling.go:57）→ main.go:1366 `instanceRollGateway` → servicereg 摘挂实例（`SwapInstance`/`SetInstanceAttached` 等）→ 每次变更触发 apply 收敛。

### 样例来源：仓库文件读取能力（已有）

- 构建工作区构建结束即删，不能依赖。
- **`httpapi.SourceReader`**（httpapi/source.go:67-73）：`Blob(ctx, repoURL, cred, ref, path)`，go-git 内存浅克隆读单文件（20s 超时、512KB 截断、binary 检测），pacloader 读 `.pipewright.yml` 同款机制。
- `projects` 表（project.go:71-93）：`repo_url / default_branch / credential_id`，凭据经 `vault.GetGitAuth` 即用即弃。

### 工作流配置惯例

- `.pipewright.yml` 部署节点 `config:` 任意 KV 透传（pipelineyaml.go:105）；部署语义键在 build/dag_stage_exec.go L848-855 **白名单**登记后进 deploy cfg——新键零迁移零 schema 变更。
- `regServiceRef`（deploy/instance_rolling.go:95）：`regServiceId` 优先、`gatewayService` 兜底。

### AI 通道（已有）

- [internal/ai](file:///home/xufan/trea_project/pipewright/internal/ai)：`ai.Service`（main.go:265 装配），claude/openai 协议手写 net/http，`ai_config` 单例表；prompt 出网前经 `mask.Masker` 脱敏；`chatWithTokens`/`resolveChatCreds`（command.go:355-388）为现成 chat 通道。

### 配置存储惯例

- 新配置项 = 双方言迁移（`internal/store/migrations/{sqlite,mysql}/NNNN_*.sql` 同号各一份）+ 领域 store scan/update + DTO + 前端。`service_reg_services` 有列演进先例（0081）。

## Proposed Changes（变更方案）

### 1. 数据层（双方言迁移，新迁移号 NNNN = 当前最大+1，实现时确认）

`internal/store/migrations/sqlite/NNNN_service_reg_conf_files.sql` + `mysql/NNNN_service_reg_conf_files.sql`，`service_reg_services` 加列：

| 列 | 类型 | 说明 |
|---|---|---|
| `sample_project_id` | TEXT NULL | 样例所在 projects.id（提供 repoURL/branch/credential） |
| `sample_conf_path` | TEXT NOT NULL DEFAULT '' | 仓库内相对路径，如 `nginx/petsuper.conf` |
| `sample_content_hash` | TEXT NOT NULL DEFAULT '' | 最近一次成功读取的样例内容 sha256（部署回填/生成时刷新） |
| `conf_source` | TEXT NOT NULL DEFAULT 'auto' | `auto`=确定性按服务渲染 / `ai`=AI 生成 |
| `ai_conf` | TEXT NOT NULL DEFAULT '' | AI 生成并确认后的该服务独立 conf 全文 |
| `ai_conf_inputs_hash` | TEXT NOT NULL DEFAULT '' | 生成时输入指纹 sha256（复用判定，见第 3 节） |
| `ai_conf_sample_hash` | TEXT NOT NULL DEFAULT '' | 生成该 ai_conf 时所用样例的内容 sha256（样例变更判定基准） |
| `ai_conf_updated_at` | 方言时间类型 NULL | 仿同表既有时间列 |

**样例变更判定**（场景 11 的权威逻辑）：`sampleStale = conf_source=ai && ai_conf_sample_hash != '' && sample_content_hash != '' && sample_content_hash != ai_conf_sample_hash`。`sample_content_hash` 由部署回填和每次 generate 读取样例后刷新；`ai_conf_sample_hash` 只在 apply 落库时写入生成所用的样例 hash。

`internal/servicereg/store.go` / `servicereg.go`：`RegisteredService` 加对应字段；scan/insert/update、`ServiceUpdate` DTO 同步；`prepareService` 校验 `sample_conf_path`（非绝对路径、不含 `..`）。新增/修改方法：

- `SetServiceSampleConf(ctx, serviceRef, projectID, relPath, contentHash)`——部署回填，幂等（值全同 no-op）。
- `ApplyServiceAIConf(ctx, serviceID, conf, inputsHash, sampleHash)`——落库 ai_conf + 两个 hash + `conf_source=ai` + `applyBestEffort`。
- `RevertServiceAutoConf(ctx, serviceID)`——`conf_source=auto`（保留 ai_conf 文本仅作历史参考，清空 ai_conf_inputs_hash）+ `applyBestEffort`。
- `UpdateService` 增加**失效联动**：服务关键字段（名称/基域/path/protocol/upstreamKind/upstream/port/StripPathPrefix/证书状态）发生变化且 `conf_source=ai` 时，自动回退 `auto`（同 RevertServiceAutoConf 语义）并在响应中告知——AI 配置引用的 listen/端口/路径可能已失效，必须重新生成。

### 2. 渲染层：主 conf + 每服务独立 conf（重构核心）

**`internal/servicereg/nginx.go`**

- **拆分渲染**（纯函数，现有逻辑就近复用）：
  - `renderMainNginxConf(...)`：worker/events/http 全局指令、`map $http_upgrade $connection_upgrade` 等现有全局部分 + `include /etc/pipewright/conf.d/http/*.conf;`（http 块内）+ `include /etc/pipewright/conf.d/stream/*.conf;`（stream 块内，仅存在 tcp 服务时输出 stream 块）。**不含任何服务块**。
  - `renderServiceConf(svc, domain, cert, members, forwardPlans)`：单服务完整 conf 文本——upstream 块（`pw_<svcID>`）+ server/location（http，含证书引用）或 stream server（tcp）。从现有 `renderNginxConf` 按服务切出，行为与现状逐字节等价。
- **托管 upstream 注入**（新纯函数）：
  - `injectUpstreamMembers(conf string, svcID string, memberLines []string) (string, error)`：逐行扫描定位 `upstream pw_<svcID> {` … 按大括号深度配对的块，**仅替换块内 `server ...;` 行**，保留 `keepalive`/调度策略/注释等其余指令；找不到约定块返回错误，不静默跳过。
  - `conf_source=ai` 的服务 apply 时用它注入当前实例成员；`auto` 服务由 `renderServiceConf` 直接渲染成员，无需注入。
- **apply 产物从单文件变目录**：`applyWith` 产出 `map[relpath]string`：`nginx.conf`（主）+ `conf.d/http/pw_<svcID>.conf`（每 http 服务）+ `conf.d/stream/pw_<svcID>.conf`（每 tcp 服务）。
- `applyNginxConf` → 重构为 `applyNginxConfDir`：全部文件上传网关机临时目录 → `docker cp` 进容器临时位置 → **`nginx -t` 整树校验**（主 conf + 全部 include 就位后校验）→ 通过后同步进卷（写新文件、**删除本次产物集中不存在的 `pw_*.conf` 残留**——按 `pw_` 前缀识别托管文件，不删人工文件）→ `nginx -s reload`。任一文件校验失败整体不生效（保留旧树）。

### 3. AI 生成层与全场景复用/重生成矩阵

**新文件 `internal/servicereg/nginx_ai.go`**

依赖接口（晚绑防 import 环）：

```go
type NginxAIChat interface { Chat(ctx context.Context, system, user string) (string, error) }
type SampleFetcher interface { Fetch(ctx context.Context, projectID, relPath string) (string, error) }
```

**输入指纹**（复用判定的唯一权威）：

```go
inputsHash = sha256(sampleContent + "\x00" + svcKeyFields + "\x00" + promptContractVersion + "\x00" + extraPrompt)
```

- `svcKeyFields`：服务名/基域/path/protocol/upstreamKind/upstream/port/StripPathPrefix/域名证书有无 的序列化；**实例成员列表不参与**。
- `promptContractVersion`：代码常量（`v1`），prompt 契约变更时递增。

**两条路径的职责划分**（核心原则）：

- **apply 收敛路径**（`applyWith`，由实例摘挂/服务变更/证书下发等一切事件触发）：**只读库存 ai_conf + `injectUpstreamMembers` 注入当前成员，永不调 AI、永不读仓库**。保证收敛动作毫秒级、无外部依赖、可重入。
- **手动 generate 路径**（用户点"AI 生成配置"）：唯一会读仓库样例、算指纹、（必要时）调 AI 的路径。

**全场景矩阵**：

| # | 场景 | 触发 | 是否调 AI | 处理流程 |
|---|---|---|---|---|
| 1 | 首次配置样例后生成 | 手动 generate | 是 | 读样例 → 算指纹（无库存 ai_conf，复用条件不满足）→ 调 AI → 预检 → 预览 diff → 确认落库（conf+指纹+样例hash）→ apply 注入成员下发 |
| 2 | **样例不变**，再次点生成 | 手动 generate | **否** | 读样例 → 算指纹 == 库存 `ai_conf_inputs_hash` 且 ai_conf 非空 → 直接返回库存 conf，`reused=true`，响应含"样例未变化，复用现有配置" |
| 3 | **样例变了**（仓库里 conf 更新） | 手动 generate | 是 | 读样例 → 指纹 ≠ 库存 → 自动走重新生成（无需 force）→ 预览（与当前生效 conf 做 diff）→ 确认落库新指纹 |
| 4 | **加机器/扩容**（新实例注册/attach） | deploy → InstanceGateway → apply 收敛 | **否** | 读库存 ai_conf → `injectUpstreamMembers` 用新成员集重写 `server` 行 → 下发 reload。生成内容不变，秒级生效 |
| 5 | **临时摘机器**（滚动部署 Detach/Swap） | 同上 | **否** | 同 4：被摘实例不在 attached 成员集中，注入后其 `server` 行消失；部署完成挂回时再注入回来。整个滚动过程 ai_conf 文本不变 |
| 6 | **机器换 IP/服务器替换** | apply 收敛 | **否** | 同 4：成员行的地址来自当前 serverAddr 映射，注入时自然用新 IP |
| 7 | **服务关键字段变更**（改端口/path/域名/协议/upstreamKind/证书状态翻转） | UpdateService | 否（需人工重新生成） | 自动失效：`conf_source`→`auto`、清空指纹、保留 ai_conf 文本作参考；apply 立刻按新字段确定性渲染，保证线上配置与新字段一致；前端提示"AI 配置已失效回退，可重新生成"（重新生成走场景 1，新指纹） |
| 8 | **extraPrompt 变化** | 手动 generate | 是 | 指纹含 extraPrompt → 不同即重新生成 |
| 9 | **prompt 契约升级**（代码发布新版） | 手动 generate | 是 | `promptContractVersion` 变化 → 指纹必然不同 → 重新生成。存量 ai_conf 不主动失效（仍可注入下发），用户重新生成时自然升级 |
| 10 | **force 强制重新生成** | 手动 generate（force=true） | 是 | 跳过指纹复用判断，直接调 AI（用于对生成结果不满意想换一版） |
| 11 | **样例变更的可发现性**（不点生成怎么知道样例变了） | 工作流部署回填 / 手动 generate | 否 | 部署回填 `SetServiceSampleConf` 与每次 generate 读样例后刷新 `sample_content_hash`；`sampleStale = (sample_content_hash != ai_conf_sample_hash)`（见第 1 节判定式），为 true 时服务行显示"样例已变更，建议重新生成"徽标。**仅提示，不自动生成**；用户重新生成并 apply 后 `ai_conf_sample_hash` 刷新，徽标消失 |
| 12 | **证书文件更新**（续期重发，路径不变） | deployCerts | **否** | conf 引用固定证书路径，证书内容替换后 reload 即可，conf 无需变 |
| 13 | **异常态兜底**：`conf_source=ai` 但 ai_conf 为空 / 注入找不到 upstream 块 | apply 收敛 | 否 | 降级用 `renderServiceConf` 确定性渲染 + 记录 warning（返回给 API 调用方/日志），保证配置不缺失；下次成功生成后恢复 ai 路径 |
| 14 | **服务删除** | DeleteService → apply 收敛 | 否 | 该服务 `pw_<svcID>.conf` 不在产物集中，目录同步时按 `pw_` 前缀清理删除 |
| 15 | **tcp/forward/static 服务** | 同上各场景 | 同矩阵 | tcp：stream 目录独立 conf，注入逻辑相同；forward：成员=对端网关 IP，同场景 4/5/6；static：无 upstream 块，不注入，AI 生成仅影响 location 部分 |
| 16 | **多网关主机** | apply 收敛 | 否 | 同一份文件集逐台下发，成员全局一致（现状语义不变） |

**`GenerateServiceConf(ctx, serviceID, extraPrompt string, force bool) (conf string, reused bool, inputsHash string, warnings []string, err error)` 流程**：

1. 读服务；未配置 `sample_project_id + sample_conf_path` → 报错（样例是本功能前提，不降级）。
2. `SampleFetcher.Fetch` 读样例（~64KB 截断）；读失败 → 报错返回。
3. 算 `inputsHash`；`!force && conf_source=ai && ai_conf != "" && hash == ai_conf_inputs_hash` → 返回库存 conf，`reused=true`（场景 2/4-6 的手动确认都走这里）。
4. 否则组 prompt：服务拓扑描述（FQDN/path/协议/upstream 名 `pw_<svcID>`/当前成员示例/证书路径约定）+ 样例全文 + `renderServiceConf` 现状基线 + extraPrompt；system prompt 固化输出契约：只输出该服务独立 conf 全文、必须含 `upstream pw_<svcID>` 块、listen/证书路径按给定约定、不得 `include` 外部路径、不得定义其他服务的 server；全部经 `mask.Masker` 脱敏。
5. `NginxAIChat.Chat` → 剥离可能的 ```nginx 围栏 → `injectUpstreamMembers` 试注入预检（失败返回错误提示调整/重试）→ 返回 `{conf, reused:false, inputsHash}`（**不落库**，待确认）。

### 4. 工作流配置链路（`nginxSampleConf` 键）

- `internal/build/dag_stage_exec.go` L848-855 白名单加 `nginxSampleConf`。
- `internal/deploy/instance_rolling.go`：`InstanceGateway` 接口加 `SetSampleConf(ctx, serviceRef, projectID, relPath string) error`；`regServiceRef` 解析成功且 cfg 含该键时调用（projectID 从部署上下文取；必要时白名单同时透传 `projectId`，实现时确认 runDeployJob 可拿 run.project_id）。main.go `instanceRollGateway` 实现 → 内部 fetch 样例算 hash → `servicereg.SetServiceSampleConf`（同时刷新 `sample_content_hash`，支撑场景 11）。
- `.pipewright.yml` 写法：
  ```yaml
  config:
    regServiceId: "<服务ID>"
    nginxSampleConf: nginx/petsuper.conf
  ```

### 5. REST API（`internal/httpapi/servicereg.go` + `router.go` servicereg 路由组 1221-1244 附近）

| 端点 | 说明 |
|---|---|
| `POST /api/servicereg/services/{id}/nginx-conf/generate` | body `{extraPrompt?, force?}` → `{conf, reused, inputsHash, warnings[]}`；未配样例 400；AI 未启用 409 |
| `POST /api/servicereg/services/{id}/nginx-conf/apply` | body `{conf, inputsHash}` → 服务端预检 → `ApplyServiceAIConf`（同时刷新 sample_content_hash） |
| `POST /api/servicereg/services/{id}/nginx-conf/revert-auto` | 回退确定性渲染 |
| `GET /api/servicereg/services/{id}/nginx-conf` | 当前生效 conf 文本 + conf_source + inputsHash + sampleStale（样例是否已变更）+ updated_at（前端 diff 基准与场景 11 徽标数据源） |

服务 CRUD DTO 加 `sampleProjectId`/`sampleConfPath`/`confSource`/`aiConfUpdatedAt`；UpdateService 响应加 `aiConfInvalidated bool`（场景 7 提示）。

### 6. main.go 装配

- `SampleFetcher` 适配器：projectID → projectSvc 取 repoURL/defaultBranch/credentialID → vault.GetGitAuth → `httpapi.NewSourceReader().Blob`。
- `NginxAIChat` 适配器：包装 `ai.Service` chat 通道（未导出则 internal/ai 加导出薄封装 `Chat(ctx, system, user)`）。
- 函数选项注入 `servicereg.New(...)`。

### 7. 前端（web/）

- `web/src/api/servicereg.ts`：加上述 4 端点 + 服务 DTO 新字段。
- `web/src/views/ServiceRegistry.vue`：
  - 服务编辑表单加"样例配置"：项目下拉 + 仓库相对路径输入（占位 `nginx/petsuper.conf`）；
  - 服务行加"AI 生成配置"按钮；`conf_source=ai` 显示"AI 配置"徽标 + "回退自动渲染"；`sampleStale=true` 显示"样例已变更"徽标（场景 11）；UpdateService 响应 `aiConfInvalidated` 时 toast 提示（场景 7）；
  - 生成弹窗：补充说明 + "强制重新生成"勾选 → 生成 → diff 视图（当前生效 vs 生成结果；`reused=true` 提示"样例未变化，已复用现有配置"）→ "应用"/"重新生成"/"取消"。diff 优先复用 web 已有依赖。
- 画布部署节点配置表单加 `nginxSampleConf` 键（找 `regServiceId` 前端登记位置照抄）。

### 8. 测试（与代码同步交付）

- `internal/servicereg/nginx_test.go`（追加或新文件）：
  - `renderMainNginxConf`/`renderServiceConf`：现有 `TestRenderNginxConf*` golden 片段等价迁移（http 多 path、多实例、https、tcp、static、forward）；主 conf 含 include 且无服务块。
  - `injectUpstreamMembers` 表驱动：单/多成员替换、保留 keepalive 与注释、缺块报错、嵌套块不误伤、stream 内 upstream。
  - `applyNginxConfDir`：`nginx -t` 失败整体不生效、残留 `pw_*.conf` 清理、非 `pw_` 文件不删。
- `internal/servicereg/nginx_ai_test.go`——**逐场景矩阵覆盖**：
  - 场景 2：hash 相同 → reused=true 且 mock Chat 零调用；
  - 场景 3：样例内容变 → hash 不同 → 调 AI；
  - 场景 4/5/6：实例增/摘/换 IP → hash 不变仍复用 + `injectUpstreamMembers` 输出成员行正确（摘机后行消失、挂回恢复）；
  - 场景 7：UpdateService 改关键字段 → conf_source 自动回退 auto、指纹清空；
  - 场景 8/9/10：extraPrompt 变 / promptContractVersion 变 / force=true → 均重新生成；
  - 场景 11：sample_content_hash 不一致 → GET 响应 sampleStale=true；
  - 场景 13：ai_conf 空 / 注入失败 → 降级渲染 + warning；
  - prompt 组装：含样例全文、upstream 名约定、auto 基线、截断、脱敏被调用；输出围栏剥离、预检失败报错。
- `internal/servicereg/servicereg_test.go`（追加，storetest）：`SetServiceSampleConf` 幂等与解析优先级、`ApplyServiceAIConf`/`RevertServiceAutoConf`。
- `internal/httpapi`（追加）：generate 未配样例 400 / AI 未配置 409 / reused 透传、apply 预检失败 400、UpdateService 失效联动响应字段。
- 前端：servicereg api vitest。

### 9. 文档

- `specs/nginx-ai-conf.md`：设计文档（conf.d 结构与文件归属、样例配置链路、**输入指纹与全场景矩阵**、托管 upstream 注入契约、目录级原子下发与回退、错误处理），`specs/README.md` 索引登记。
- `internal/servicereg/nginx_ai.go` 包头与逐函数中文注释（含场景矩阵表，与代码同源维护）。
- `README.zh-CN.md` 服务注册章节补小节（功能入口、`nginxSampleConf` 写法、conf.d 结构、复用规则简述）。

## Assumptions & Decisions（假设与决策）

1. **废弃单文件全量渲染**：主 conf + `include conf.d/{http,stream}/*.conf`，每服务一文件。存量升级首次 apply 用新目录整体替换（`nginx -t` 整树校验通过后切换，失败保留旧文件）。
2. **复用判定唯一权威 = 输入指纹**；**实例成员的一切变化（加/摘/换 IP）永不触发重新生成**，由 apply 时托管注入处理——这保证滚动部署全程零 AI 调用、毫秒级收敛。
3. **样例变更不自动重新生成**：手动 generate 时指纹不同自然走 AI；部署回填刷新 `sample_content_hash` 提供"样例已变更"可发现性（徽标提示，人工决定何时重新生成）。
4. **服务关键字段变更即失效**：`conf_source` 自动回退 auto（线上立即与新字段一致），AI conf 文本保留作参考，人工重新生成升级。
5. AI 只在手动 generate 路径被调用；apply 收敛路径永不调 AI、永不读仓库。
6. **样例变更判定双列制**：`sample_content_hash`（最近一次读取的样例 hash，部署回填/generate 刷新）与 `ai_conf_sample_hash`（生成所用样例 hash，仅 apply 落库时写入）分离，`sampleStale` 为两者不等——职责清晰，无需从 inputs_hash 反推。
7. 未配置样例的服务保持 auto 确定性渲染；AI 是逐服务 `conf_source`，非全局开关。
8. 样例读取失败即报错不降级；样例仅作 AI 参考，不直接下发。
9. 迁移号、前端 diff 组件、`ai.Service` 薄封装形式、projectID 透传方式在实现时按现有代码就近确认，不改变方案结构。
10. 不做：生成配置的版本历史、AI 自动重生成、跨服务批量生成 UI。

## Verification（验证）

1. `go build ./...` + `go vet ./...` 通过。
2. `go test ./internal/servicereg/... ./internal/httpapi/... ./internal/deploy/... ./internal/build/...` 全绿（含场景矩阵新增用例；现有渲染测试等价迁移后仍绿）。
3. `cd web && npm run test` 与前端构建通过。
4. 手工验证（按 specs 文档）：sqlite 启动 → 配 AI → 注册服务设样例路径 → 工作流部署（验证回填 + conf.d 生效 + 旧单文件被替换）→ AI 生成 → diff 预览 → 应用 → 网关机 `nginx -t` 通过且 `conf.d/http/pw_<id>.conf` 含自定义指令 → **再次生成验证 reused=true（零 AI 调用）** → **改样例后生成验证自动重新生成** → **滚动部署验证摘/挂机器时 upstream 成员行随之增删且 conf 其余部分不变** → 改服务端口验证自动回退 auto + 提示 → 回退/重新生成恢复。
