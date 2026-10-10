## 目标

给「GPU 机型」加显卡监控：服务器登记/编辑增加 **GPU 勾选**；勾选后多机状态总览卡片额外经 `nvtop -s` 采集显卡信息（支持多卡、NVIDIA 与 AMD）。

**已确认范围**（按你的选择）：仅实时监控展示，不落库、不进异常检测规则与历史趋势；**不展示进程列表**，只取显卡级指标。

## 现状（已核实）

- 采集：`internal/httpapi/server_metrics.go` 每台机 **1 次 SSH** 跑一个纯静态 `sh -c` 合并脚本，按 `##PW:<名>` 标记行分段，逐段解析成 DTO（AC-SEC-02：命令是静态文本，绝不拼用户输入）。已有 TTL 10s 快照缓存（stale-while-revalidate + 单飞），前端 12s 轮询。
- 展示：`web/src/views/ServerStatus.vue` → `web/src/components/ops/ServerMetricsCard.vue`（`dl` 里 CPU / 内存 / 交换 / 磁盘 四行，某维度 null 即整行「不可用」）。
- 服务器登记：`servers` 表 + `internal/target/target.go`（原生 SQL + `scanServer`），HTTP 层 `internal/httpapi/servers.go`，前端 `web/src/views/settings/SettingsServers.vue`。已有 `pac_enabled` 这类布尔列的先例（`INTEGER/TINYINT NOT NULL DEFAULT 0`，`scan` 成 int 再 `!= 0` 转 bool）。
- 最新迁移是 `0070_*`，且 `sqlite/` 与 `mysql/` 两个目录必须成对（`internal/store/migrate_mysql_test.go` 强制校验）。

## 实施步骤

### 1. 迁移：`servers.gpu` 布尔列

- 新增 `internal/store/migrations/sqlite/0071_servers_gpu.sql`：`ALTER TABLE servers ADD COLUMN gpu INTEGER NOT NULL DEFAULT 0;`
- 新增 `internal/store/migrations/mysql/0071_servers_gpu.sql`：`ALTER TABLE servers ADD COLUMN gpu TINYINT NOT NULL DEFAULT 0;`
- 仿 `0039_project_pac_enabled.sql`；旧行默认 0（未勾选）。

### 2. 领域层：`internal/target/target.go`

- `Server` 加 `Gpu bool`（注释：该机带 GPU，勾选后状态总览额外采集 `nvtop -s` 显卡指标；**纯监控开关，不参与构建/部署调度**，调度仍用 labels）。
- `CreateInput.Gpu bool`、`UpdateInput.Gpu *bool`（nil = 不修改）。
- 逐点串联：`Create` 的 INSERT、`List`/`Get`/`Update` 取当前行的 SELECT（`COALESCE(gpu, 0)`）、`Update` 的 UPDATE 与指针赋值段、`scanServer`（仿 `scanProject`：扫进 `int` 再 `!= 0` 转 bool，且扫描列顺序与 SELECT 严格对齐）。

### 3. HTTP 层：`internal/httpapi/servers.go`

- `serverDTO` 加 `Gpu bool \`json:"gpu"\``；`toServerDTO` 拷贝。
- 创建请求体加 `Gpu bool`、更新请求体加 `Gpu *bool`，各自透传进 `target.CreateInput` / `target.UpdateInput`（部分更新语义与既有字段一致）。

### 4. 采集：`internal/httpapi/server_metrics.go`（核心）

- 新段常量 `secGpu = "gpu"`；`metricsCollectArgs(probePhys bool)` → `metricsCollectArgs(probePhys, gpu bool)`，仅当 `gpu == true` 时在磁盘段之后追加：
  ```
  echo "##PW:gpu"
  timeout 5 nvtop -s 2>/dev/null
  ```
  - 用 `timeout 5` 兜底：显卡驱动异常时 `nvtop` 可能卡住，若不设上限会把整台机的 CPU/内存/磁盘一起拖到 10s 总超时（该台变成「采集超时」）；有上限则最坏只是 GPU 段为空。`timeout` 缺失（极少见）→ 段空 → GPU 显示「不可用」，其余指标不受影响。
  - 非 GPU 机型脚本完全不变（不跑 nvtop）。
- `collectServerMetrics` 里先 `svc.Get(ctx, id)` 拿 `srv.Gpu`（一次本地 SQLite 读；`Exec` 内部本来也会 `Get`）。`Get` 失败按现有定位类错误语义处理（`isLocateError` → 返回错误供单台端点映射 422/503；批量端点忽略）。
- `metricsOutMax` 64KiB → 256KiB：`nvtop -s` 的 JSON 含每进程 `cmdline`（我们不用，但字节仍在流里），截断会把 JSON 打断导致解析失败；CPU/内存/磁盘段在 GPU 段之前，尾部截断也只影响 GPU。
- 新 DTO（指针字段，缺项序列化为 null）：
  ```go
  type gpuDeviceMetric struct {
      Index int; Name string
      GpuUtil, MemUtil *float64            // %
      MemTotalBytes, MemUsedBytes, MemFreeBytes *int64  // NVIDIA 才有；AMD → null
      TempC, FanSpeedPct, PowerDrawW *float64
      GpuClockMHz, MemClockMHz *float64
      EncodeUtil, DecodeUtil *float64      // NVIDIA 才有
  }
  type gpuMetric struct{ Devices []gpuDeviceMetric `json:"devices"` }
  ```
  `serverMetricsDTO` 追加 `GPU *gpuMetric \`json:"gpu"\``（仅 GPU 机型非 null 且解析出 ≥1 张卡时才非 nil；否则 null = 卡片显示「不可用」）。
- 纯函数解析器（空/异常一律返回「不可用」，绝不 panic）：
  - `parseNVTopDevices(s)`：取首个 `[` 到末个 `]` 之间的 JSON（容忍 stderr 噪声），按 `device_name / gpu_clock / mem_clock / temp / fan_speed / power_draw / gpu_util / mem_util / encode / decode / mem_total / mem_used / mem_free` 原始标签反序列化；`processes` 等未知字段自动忽略；0 张卡 → 不可用。
  - `parseNVTopNum(s)`：取前缀数字，"926MHz"→926、"44C"→44、"14%"→14、"19W"→19、"82%"→82；"N/A"/空 → 失败（该字段 null）。
  - `gpuFromSections(sections)`：段空 → nil；否则 `parseNVTopDevices` → 逐卡映射（AMD 无 mem_total/encode/decode → 对应字段 null）。

### 5. 前端

- `web/src/api/servers.ts`：`Server.gpu: boolean`、`CreateServerInput.gpu?`、`UpdateServerInput.gpu?`；新增 `GpuDeviceMetric` / `GpuMetric` 接口，`ServerMetrics.gpu: GpuMetric | null`。
- `web/src/views/settings/SettingsServers.vue`：`form.gpu`（`openAddModal` 重置为 false、`openEditModal` 取 `s.gpu ?? false`）、create/update 两个 payload、表单里在 maxBuilds/priority 那一行附近加一个复选框（含 hint：需目标机已装 nvtop）、列表行徽标 `v-if="s.gpu"`（如 `🎮 GPU`，title 用同一 hint）。
- `web/src/views/ServerStatus.vue`：`poolById` 的值加 `gpu`（取自登记列表），给卡片传新 prop `:gpu="poolById.get(m.serverId)?.gpu ?? false"`。
- `web/src/components/ops/ServerMetricsCard.vue`：新可选 prop `gpu?: boolean`；仅当为 true 时渲染 GPU 区块（未勾选机型卡片保持原样）：
  - 每张卡一个小块：`#0 device_name` → GPU 利用率进度条（>90 红 / >75 黄，复用 `usageVariant`）→ 显存行（有 `memTotalBytes` 就画进度条 + 人读字节 + 百分比；AMD 只有百分比就只画百分比）→ 一行次级信息：温度 / 功耗 / 风扇 / 核心与显存时钟（仅为非 null 的项），NVIDIA 的编码/解码仅在 >0 时显示。
  - `metrics.gpu === null` → 一行「不可用」，文案点明「未安装 nvtop 或未检出 GPU」。
- i18n：8 个 locale（de/en/es/fr/ja/ko/zh-CN/zh-HK）各加 `opsServer.metrics.*`（gpu、gpuUnavailable、gpuUtilLabel、gpuMemUsageLabel、温度/功耗/风扇/时钟/编码/解码等）与 `settingsServers.*`（fieldGpu、gpuHint、gpuBadge）。

### 6. 测试

- Go `internal/httpapi/server_metrics_test.go`：更新 `metricsCollectArgs` 调用的签名与 `TestMetricsCollectScript`（非 GPU 脚本不含 `nvtop`；GPU 脚本含 `##PW:gpu` + `nvtop -s`；dmidecode 与 nvtop 段互不干扰）；新增 `parseNVTopNum` 表驱动测试、`gpuFromSections` 测试（用你给的 NVIDIA 样例含 `processes` 与 AMD 样例，验证多卡、AMD 缺项为 null、空/`[]`/垃圾 → nil）；新增 HTTP 集成测试：GPU 机型假机回 GPU 段 → `gpu.devices` 解析正确，非 GPU 机型 → `gpu == nil` 且发给假拨号器的脚本不含 nvtop（需要给 `servers_test.go` 的建服务器测试 helper 加带 gpu 的变体）。
- Go `internal/httpapi/servers_test.go`：仿 `TestServerRunnerPoolFields` 加 `gpu` 往返测试（create 带 gpu → 响应 true；PUT 部分更新可开可关；不带该字段不修改）。
- Go `internal/target/target_test.go`：`gpu` 建/取/改往返（含 `*bool` 局部更新）。
- 前端 `web/src/components/ops/ServerMetricsCard.test.ts`：gpu 机型 + 多卡数据渲染、gpu 机型 + `gpu: null` 显示不可用、非 gpu 机型无 GPU 行。构造 `Server` 的 3 个 fixture（`LabelSelectorEditor.test.ts`、`JobDrawer.labelSelector.test.ts`、`selectorMatch.test.ts`）补 `gpu: false`。

### 7. 验证

- `go build ./...` + `go test ./internal/httpapi/... ./internal/target/... ./internal/store/...`
- `cd web && npm run typecheck && npm test`
- 可选：按仓库惯例加一份简短设计说明 `.trae/documents/server-gpu-monitoring.md`。

## 明确不做

- 不改异常检测指标枚举 / `metric_samples` 表 / 历史趋势图 / 采样器（GPU 不落库）。
- 不展示每卡进程列表（`processes` 字段解析时忽略）。
- 不做 nvtop 缺失时回退 `nvidia-smi`/`rocm-smi`（严格按需求只用 `nvtop -s`；缺失即该行「不可用」）。
- GPU 勾选不参与调度（构建机池仍由 labels/槽位/优先级决定），Dashboard 紧凑卡与 AI 运维 `host_resources` 工具也不改。