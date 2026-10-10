## 目标

运行日志从"一条平铺到黑的流"改为按「步骤 × 机器」两个维度结构化展示：

1. **机器维度（数据模型）**：每条日志行带 `machine`（来源机器显示名快照；`""` = 控制机/运行级）。部署/健康检查在目标机上的输出逐行标注是哪台机；远程构建节点的输出标注构建机名。
2. **前端（展示）**：
   - 同一步骤内多台机器时，终端头部出现**机器切换标签**（全部｜控制机｜web-01｜web-02…），点机器名只看那台机；带归属的行首加机器名小徽标。
   - **「全部日志」视图按步骤插入标题分隔行**（阶段名·步骤名·状态点），日志自动归段，不点侧边栏也能看懂结构。

## 后端改动

### 1. 迁移 0070（sqlite + mysql 各一份）
`ALTER TABLE run_logs ADD COLUMN machine TEXT NOT NULL DEFAULT ''`（mysql 用 `VARCHAR(255)`），仿 0058/0067 范例，头部注明 additive only、旧行空串行为不变。

### 2. `internal/run` 包 — 机器归属的 ctx 通道 + 落库
- `bus.go`：`LogLine` 加 `Machine string`。
- 新增 ctx 辅助（service.go 或新文件）：`WithLogMachine(ctx, machine)` / `LogMachineFrom(ctx)`。
- `service.go`：`AppendLog` 加 machine 参数并写入新列；`GetLogs` SELECT 加 machine。
- `pool.go`：`dbStepSink.Log` 改为从**传入的 ctx** 读机器归属（现在它忽略 ctx 用 Background 落库——读值仍用传入 ctx，落库继续用 Background 容忍取消），透传给 AppendLog 和 EventLog。

### 3. `internal/deploy` 包 — 逐机输出标注
- `cmdlog.go`：`CmdLogFunc` 签名改为 `func(stream, machine, text string)`；`s.exec` 内 6 处 lg 调用与 9 处直接 `cmdLogFrom(ctx)(...)` 发射点机械适配（run 级传 `""`）。
- 新增 `scopeCmdLog(ctx, machine)`：返回子 ctx，深层发射自动带上机器名（包装 base 回调）。应用在单机作用域处：
  - `deployFanout` goroutine 体（deploy.go:846 附近，覆盖并行滚动/重试失败扇出）
  - `runCommandOnly` 逐机循环体
  - `precheckFailing` 的 forEachServer work 体（strategy.go）
  - `CheckHealth` 逐机探测循环（health_probe.go）
  - deploy.go:924 / image_release.go:135 / ports.go:285 等深层单机点经作用域自动获得归属，无需单独传
- 上传制品的三处静默点（release.go:199/220/284）补一行 `→ 上传制品到 <机器名>…`（srv 在作用域内，归属自动正确）。

### 4. `internal/build` 包 — 接线
- `dag_stage_exec.go`：`runDeployJob`/`runHealthCheckJob` 的 WithCmdLog 回调改为 `func(stream, machine, text)`，内部 `rep.Log(run.WithLogMachine(ctx, machine), stream, text)`（job 级 reporter 保证行归到部署节点自己的 step）。
- `remote_stage_exec.go`：`runStageRemote` 取机后 `lctx := run.WithLogMachine(ctx, serverName)`，该节点全部 rep.Log/onLine 改用 lctx（Exec/上传等控制流仍用原 ctx）。

### 5. `internal/httpapi` — DTO 透出
`logLineDTO` 加 `machine`（json:"machine"），`toLogLineDTO`/SSE `logPayload` 自动透出；REST `/logs` 与 SSE 两条链路同时生效。

## 前端改动（web/）

### 6. 类型与数据
`api/runs.ts`：`RunLogLine` 加 `machine?: string`（注释更新；SSE log 事件 payload 即本类型，自动生效）。

### 7. `RunTerminal.vue` — 机器标签 + 步骤分段 + 行首徽标
- 新 prop `steps?: RunStep[]`（ordinal→名称/阶段/状态，供分段标题）；RunDetail 的 6 个挂载点都传入（run.steps 本来就在）。
- **机器切换标签**：computed 从当前过滤范围内取 unique machine 桶（含 `""`=控制机）；≥2 个桶时头部下方渲染一排分段按钮（复用 TriggersPanel 的 `.segmented/.seg-btn` 样式），默认「全部」，点选过滤（与 filterOrdinal 过滤叠加）。切换 run/filter 时重置为全部。
- **行首机器徽标**：`line.machine` 非空时行号后渲染小型机器名 chip（mono、暗色底）。
- **全部日志步骤分段**：`filterOrdinal == null` 时，visibleLines 相邻行 stepOrdinal 变化处插入标题行 li（`── 阶段 · 步骤名 [状态点]`；ordinal=-1 的运行级行归入开头不设标题）。
- i18n：`web/src/i18n/locales/*/run.ts` 8 个目录加 key（machineAll=全部、machineCtrl=控制机、machineFilterAria 等），keyParity 测试自动校验。

## 测试

- `internal/deploy`：现有 cmdlog/deploy 测试闭包适配 3 参；新增用例：DeployForStage + WithCmdLog 捕获，断言并行滚动下各机输出行带对应 srv.Name、目标机拓扑行 machine=""。
- `internal/run`：AppendLog/GetLogs machine 往返；dbStepSink 从 ctx 读归属。
- `internal/build`：fakeReporter 扩展记录 `run.LogMachineFrom(ctx)`，断言远程节点日志行带构建机名、部署回调带目标机名。
- 全量 `go build && go vet && go test ./...`；前端跑 lint/构建与 keyParity。

## 边界与不做

- 历史运行：machine 全为空串 → 无机器标签（单桶不显示切换条），步骤分段正常工作（stepOrdinal 本来就有）。
- 不做虚拟滚动（维持全量渲染，当前量级可接受；大日志性能是既有边界，不在本次扩大）。
- 机器名取部署/取机时刻的显示名快照，机器改名不回溯历史行（与 deploy_targets.server_name 同策略）。