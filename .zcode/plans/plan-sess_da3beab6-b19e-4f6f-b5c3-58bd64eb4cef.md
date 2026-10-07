## 目标

运行日志中明确显示每个节点（job）被调度到哪台机器上执行：
- **远程执行**：日志打机器的**人读名**（现在是不可读的 uuid），并归到该节点自己的日志流（点开单节点过滤时可见）。
- **本地执行**（如截图中的"仓库自检"）：打一行 `→ 执行机器：本机`，消除"到底跑在哪"的歧义。

仅改后端日志行，不动数据库、不动前端 API；新运行立即生效，历史运行日志不回填。

## 改动点

### 1. `internal/runner/scheduler.go` — 调度器把机器名带出来
- `candidate` 结构加 `host` 字段，`candidates()` 查询加 `COALESCE(host,'')`。
- `Acquire` 签名改为返回 `(serverID, serverName string, release func(), err error)`；`serverName` 为人读显示名：`name(host)`（host 空则只 name，name 空回退 id）。
- `pickOrEnqueue` 改为返回选中的 `candidate`（内部函数）。

### 2. `internal/build/remote_stage_exec.go` — 远程路径用机器名 + 归到节点
- `RunnerResolver` 接口的 `Acquire` 签名同步。
- 阶段级远程（无 script job）路径：`→ 构建机:<名字>(选择器 xxx)`。
- 节点级路径（每节点独立取机）：
  - 派发行 `→ 节点「xx」→ 构建机:<名字>(选择器 xxx)` 改经 `rep.JobReporter(jb.ID)` 上报，归到该节点自己的 step（现在归到阶段首节点，单节点过滤看不到）。
  - `runStageRemote` 按节点调用时改传 `rep.JobReporter(jb.ID)`，使打包传输/容器 CLI/完成日志都归到节点名下；`✓ 远程 runner(<uuid>)执行完成` 改用机器名。

### 3. `internal/build/dag_stage_exec.go` — 本地路径标注本机
- `NewStageExecutor` 闭包顶部（`len(stage.Jobs)>0` 时）打一行阶段级日志：`→ 执行机器:本机(控制机)`。
- 自动覆盖：纯本地阶段、混合阶段的本地子集、未装配 runner 的独立本地执行器。纯远程阶段不经过此路径，不会误打。

### 4. `cmd/pipewright/main.go` — 适配层
- `runnerPool.Acquire` 透传新签名（一行）。

### 5. 测试
- `internal/runner/scheduler_test.go`：约 40 处 `Acquire` 调用机械适配 4 返回值；新增用例断言 serverName 格式（name(host) / name / id 回退）。
- `internal/build/remote_stage_exec_test.go`：fake resolver 签名适配；补断言日志含机器名（非 uuid）且派发行归到节点 ordinal。
- 跑 `go build ./...`、`go vet ./...` 及 `internal/runner`、`internal/build`、`internal/dagrun` 相关测试；检查 `dag_stage_exec_test.go` 等既有日志断言是否需同步。

## 明确不做
- 不加 DB 迁移、不改 `stepDTO`/前端（按你的选择）。
- 不动部署/健康检查已有目标机名日志（`deploy_targets` 已有 server_name 快照展示）。
- 历史运行的日志保持原样（uuid 行），不做回填。