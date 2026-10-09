# 部署节点目标数量上限(maxTargets):按主机负载选最空的 N 台

## 概要(Summary)

为流水线的 **deploy_container** 部署节点新增 `maxTargets` 配置(目标机数量上限):
标签选择器命中 M 台机器时,若 `maxTargets = N` 且 M > N,则按**主机 CPU/内存负载**
探得「最空」的 N 台机器部署,其余机器本次不部署。未配置 / 非法值 / N ≥ M 时行为完全不变。

示例:选择器 `env=prod` 命中 5 台,`maxTargets=3` → 探测 5 台负载 → 选负载最低的 3 台滚动部署。

## 现状分析(Current State)

- 部署节点配置是 `pipeline.Job.Config` 自由键值([pipeline.go](file:///c:/Users/xufan/Trae/pipewright/internal/pipeline/pipeline.go#L68));
  [runDeployJob](file:///c:/Users/xufan/Trae/pipewright/internal/build/dag_stage_exec.go#L617) 把白名单键透传进
  `cfg map[string]string`(dag_stage_exec.go:649-656),调 `deploy.Service.DeployForStage(ctx, runID, selector, cfg, strategy)`。
- 目标圈选在 [selector_targets.go](file:///c:/Users/xufan/Trae/pipewright/internal/deploy/selector_targets.go#L43)
  `resolveTargets`:显式 serverIDs 优先 → `server:<id>` 钉单机 → 标签选择器(all/any)全表匹配;零命中 = 跳过即成功。
- [DeployForStage](file:///c:/Users/xufan/Trae/pipewright/internal/deploy/deploy.go#L339) 流程:
  resolveTargets → 算 allServerIDs(注册表清理基准)→ 恢复运行增量过滤(resumeSucceededServers)
  → 取产物 → deployRolling 分批部署 → 持久化 deploy_targets。
- 滚动编排在 [strategy.go](file:///c:/Users/xufan/Trae/pipewright/internal/deploy/strategy.go#L48),
  批次参数 `firstBatchSize`/`batchSize` 从 cfg 解析(非法值 → 默认),`forEachServer` 提供有界并发(cap 4)。
- 主机负载采集能力已在 [server_metrics.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/server_metrics.go):
  一条静态 `sh -c` 合并脚本(`##PW:` 分段标记)采集 `cat /proc/loadavg`、`nproc`、`free -b`,
  单次 SSH 完成;但代码在 httpapi 包,deploy 包不可复用(依赖方向错误),需在 deploy 包实现**最小化**探针。
- 前端 deploy_container 字段 schema 在 [jobConfigSchema.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/components/pipeline/jobConfigSchema.ts#L415)
  `DEPLOY_CONTAINER_FIELDS`;i18n 有 8 个语言目录(de/en/es/fr/ja/ko/zh-CN/zh-HK),各有 `pipelineJob.ts`,
  `keyParity.test.ts` 强制键对齐。
- deploy 层日志经 `cmdLogFrom(ctx)`(cmdlog.go)回流到节点日志,runDeployJob 已注入 `deploy.WithCmdLog`。

## 修改方案(Proposed Changes)

### 1. 后端:负载探针(新文件 `internal/deploy/host_load.go`)

新增最小化主机负载探针,语义对齐 server_metrics.go 但只取 CPU/内存两个维度:

- `probeHostLoad(ctx, serverID) (fullness float64, ok bool)`:
  - 经 `s.targets.Exec` 跑一条**纯静态** `sh -c` 脚本(沿用 `##PW:` 分段标记模式,AC-SEC-02 无注入面):
    `cat /proc/loadavg || uptime`、`nproc || getconf _NPROCESSORS_ONLN`、`free -b`。
  - 单台超时 10s(对齐 metricsCollectTimeout)。
  - 计算饱和度:`cpuRatio = loadavg1 / cores`(cores ≤ 0 视为失败);
    `memRatio = memUsed / memTotal`(used = 不含缓存口径,对齐 serverMetrics.UsedBytes);
    **`fullness = max(cpuRatio, memRatio)`** —— 任一资源接近打满即视为「不空」,保守。
  - Exec 失败 / 输出解析失败 / 缺段 → `ok = false`。
- `limitServersByLoad(ctx, servers, limit) []*target.Server`:
  - `limit <= 0` 或 `len(servers) <= limit` → 原样返回(零开销,不探测)。
  - 否则用 `forEachServer`(有界并发)逐台探测;**稳定排序**:探测成功的按 fullness 升序,
    失败的(`ok=false`)排最后(相对顺序不变);取前 `limit` 台。
  - 经 `cmdLogFrom(ctx)` 打一行人读日志,例如:
    `选择器命中 5 台,目标上限 3:按主机负载(CPU/内存饱和度)选取最空的 3 台 → web-a(0.42), web-c(0.55), web-b(0.61);未选:web-d(0.83), web-e(探测失败)`。
- `maxTargetsLimit(cfg map[string]string) int`:解析 `cfg["maxTargets"]`,空/非整数/≤0 → 0(无上限),
  与 `rollingBatchSize` 同款容错。

### 2. 后端:接入 DeployForStage(`internal/deploy/deploy.go`)

在 `DeployForStage` 的 `resolveTargets` 之后、`allServerIDs` 计算**之前**插入:

```go
if limit := maxTargetsLimit(cfg); limit > 0 {
    servers = s.limitServersByLoad(ctx, servers, limit)
}
```

- 上限裁切后的集合即本次部署拓扑:`allServerIDs`(注册表 PruneInstances 基准)、
  resumeSucceededServers 增量过滤、deployRolling、deploy_targets 持久化全部基于裁切后的集合,语义一致。
- 不改 `resolveTargets`(显式 serverIDs 的 HTTP 部署 API / 回滚回放路径不受影响);
  不改 `Deploy` / `RetryFailed` / `DeployInput`(手工部署 API 不暴露此参数,保持最小范围)。

### 3. 后端:节点透传(`internal/build/dag_stage_exec.go`)

`runDeployJob` 的透传键清单中,`maxTargets` **仅当 `jb.Type == "deploy_container"`** 时透传
(用户决策:仅 deploy_container 生效;deploy_ssh / deploy_frontend 即使配了也不透传、不生效)。
deploy_container 走 `NewStageExecutorWithRunner` 时非 script 节点仍归本地执行器 → runDeployJob,
单条透传路径,无遗漏。

### 4. 前端(`web/src/components/pipeline/jobConfigSchema.ts` + i18n)

- `DEPLOY_CONTAINER_FIELDS` 在 `batchSize` 之后新增:

```ts
{
  key: 'maxTargets',
  get label() { return t('pipelineJob.fieldMaxTargetsLabel') },
  kind: 'number',
  placeholder: '0',
  get hint() { return t('pipelineJob.fieldMaxTargetsHint') },
},
```

- 8 个语言目录的 `pipelineJob.ts` 各加 `fieldMaxTargetsLabel` / `fieldMaxTargetsHint`
  (zh-CN:「目标数量上限」/「选择器命中的机器多于上限时,按主机 CPU/内存负载选取最空的 N 台部署;0 或留空 = 不限」,其余语言对应翻译)。
- `DEPLOY_SSH_FIELDS` **不加**(仅 deploy_container)。

### 5. 测试

- **`internal/deploy/host_load_test.go`**(新增):
  - 探针解析:正常输出 → fullness = max(cpu, mem);缺段/格式异常/Exec 失败 → ok=false。
  - `limitServersByLoad`:limit≤0 与 limit≥len 不探测原样返回;limit<len 按负载升序选取;
    探测失败排最后;同分保持原顺序(稳定);fake 实现 `target.Service`(deploy 测试已有 fake 模式可参考 deploy_test.go)。
  - `maxTargetsLimit`:空/非法/0/负数/正常值。
- **`internal/deploy/deploy_test.go`**(或 selector_targets 既有测试文件):DeployForStage 端到端(fake targets + fake runs):
  5 台命中 + maxTargets=3 + 预制负载 → 只对最空的 3 台执行并落 3 行 deploy_targets;不带 maxTargets 回归不变。
- **`internal/build/dag_stage_exec_test.go`**:deploy_container 节点透传 maxTargets;deploy_ssh 节点不透传(即使 Config 里有)。
- **前端**:`keyParity.test.ts` 自动校验 8 语言键对齐(无需新写);若 jobConfigSchema 有既有快照/字段测试则同步更新。

### 6. 文档

- 更新 [deploy-target-selector.md](file:///c:/Users/xufan/Trae/pipewright/specs/deploy-target-selector.md):
  新增 maxTargets 语义段落(上限裁切发生在圈选后、滚动前;排序指标与失败排最后规则;
  仅 deploy_container;显式 serverIds / 钉单机不受影响)与覆盖矩阵条目。

## 与滚动轮转(rolling)的交互逻辑

上限裁切发生在**滚动编排之前**:`deployRolling` 看到的就是裁切后的 N 台,
既有滚动语义在 N 台内**完全不变**。完整时序(以命中 5 台、maxTargets=3、firstBatchSize=1、batchSize=0 为例):

```
resolveTargets          → 命中 5 台:[a, b, c, d, e](选择器匹配顺序)
limitServersByLoad      → 一次性探测 5 台负载,选最空的 3 台:[a, c, b](按饱和度升序)
                           ※ 负载快照只在选机时用一次,滚动期间不再重新探测/换机
allServerIDs            → [a, c, b](注册表清理基准 = 裁切后集合)
resume 过滤             → 恢复运行时,已 success 的机器从 [a, c, b] 中跳过
deployRolling(3 台内滚动):
  1) 预检排序           → 配了健康检查时对这 3 台逐台预检,预检不过的排队首先修
  2) 首批               → firstBatchSize=1:先部署第 1 台(预检故障机优先,否则负载最低者)
  3) 后续批             → batchSize=0(默认):剩余 2 台一批并行推完(批内并发 cap 4)
  4) 批失败停止         → 任一批有失败 → 立即停止,3 台中未轮到的记 pending
                           (仍运行旧版本,RetryFailed 可继续推进;与既有语义一致)
持久化                  → deploy_targets 只写这 3 台的行;未入选的 d/e 不产生任何记录
```

要点:

1. **选机与滚动是两步、不嵌套**:负载排序只决定「哪 N 台进入本次部署」;进入后由
   firstBatchSize/batchSize/预检排序决定「N 台内部的推进顺序」。滚动中途**不会**因负载变化
   重新选机或把未入选的机器换进来。
2. **预检排序优先级高于负载排序的展示序**:进入滚动后,预检不过的机器仍按既有规则排到队首
   先修(它已在入选的 N 台之内);负载升序只影响「入选资格」,不影响滚动批次编排。
3. **未入选的机器完全不动**:不预检、不部署、不写 deploy_targets、不进注册表清理的保留集
   (其既有实例行在全成功时会被 PruneInstances 摘除,见决策 6)。
4. **边界取值**:maxTargets=1 + firstBatchSize 默认 1 → 等价于「只部署负载最低的一台」;
   maxTargets ≥ 命中数 → 不探测、不裁切,滚动行为与现状逐字节一致(零开销回归)。

## 假设与决策(Assumptions & Decisions)

1. **排序指标 = 主机负载**:`fullness = max(loadavg1/cores, memUsed/memTotal)`(用户选定 CPU/内存;
   max 取法保守——任一资源饱和即算「不空」)。同分按选择器命中顺序(稳定排序)。
2. **仅 deploy_container**:runDeployJob 按 job.Type 门控透传;deploy_ssh/deploy_frontend 不生效。
3. **探测失败的机器排最后**:上限裁切优先保留可确认空闲的机器(用户选定)。
4. **只对选择器圈选结果生效**:显式 serverIds(HTTP 部署 API / 环境回滚回放)不经过本逻辑;
   `server:<id>` 钉单机只有一台,上限实际无效果。
5. **非法值容错**:`maxTargets` 空/非整数/≤0 = 不限(与 batchSize 容错风格一致,不阻断流水线)。
6. **注册表清理基准 = 裁切后集合**:PruneInstances 会摘除不在本次目标集合内的实例行
   (上限语义即「服务拓扑就是这 N 台」);仅全成功时触发,与既有语义一致。
7. **恢复运行(resume)每次重新解析+重新探测**:负载是瞬时值,恢复时选取的子集可能与原运行不同,
   已 success 的机器仍由 resumeSucceededServers 跳过——接受该语义,文档注明。
8. **不做的事**:不改 HTTP 手工部署 API;不引入缓存(部署是低频动作,每次实时探测);
   不持久化当时的负载快照(deploy_targets 形状冻结,不动)。

## 验证(Verification)

1. `go test ./internal/deploy/... ./internal/build/...` 全绿(含新增用例)。
2. `go build ./...` 通过。
3. 前端:`cd web && npm run test`(至少 keyParity / pipeline 相关)与 `npm run build`(tsc)通过。
4. 手工冒烟(可选):deploy_container 节点选择器命中多台 → 设 maxTargets=2 → 运行流水线,
   节点日志出现「按主机负载选取最空的 2 台」行,run-detail targets 仅 2 行。
