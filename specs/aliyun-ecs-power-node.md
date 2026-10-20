# 阿里云 ECS 开关机节点(ecs_power)

> 状态:已实现。相关:`internal/aliyunecs/power.go`(领域)、`internal/build/ecs_power.go`(执行器)、`internal/pipeline/ecs_power.go`(保存期校验)、`web/src/components/pipeline/` 前端(schema + JobDrawer + EcsInstanceMultiSelect)、`internal/ai/catalog.go`(AI 生成目录)。

## 语义

流水线内置节点 `ecs_power`(分类 deploy):对一台或若干台阿里云 ECS 实例执行**开机**(StartInstance)或**关机**(StopInstance + `StoppedMode=StopCharging`,即**节省停机模式/停机不收费**,按需求固定,不暴露普通停机选项;仅 VPC 实例支持,经典网络实例被 API 拒绝并按逐台错误暴露)。

- **机器选择三种方式**:
  1. `selector=instances`:配置时多选实例(快照,逗号串 `instanceIds`);实例释放后运行时该台报「实例不存在」。
  2. `selector=tags + tagSource=aliyun`:按**阿里云 ECS 标签**(云控制台打的)运行时解析——拉该账户全部实例后客户端按标签过滤(多对 AND;仅 key = 任意值命中);不走 Tag.N 服务端过滤,统一语义。
  3. `selector=tags + tagSource=platform`:按**平台服务器标签**(`servers.labels`,与构建机池同一匹配语义 `runner.MatchSelector`,AND)匹配后经 `aliyun_ecs_instance_links` 反查实例 ID;非本账户导入的匹配机器跳过并日志告警。
- **0 命中即整节点失败**(两种标签来源均是):防标签拼错静默空跑。
- **幂等**:执行前批量查状态(DescribeInstanceStatus),已在目标状态(Running/Stopped)的台跳过并记日志;转换中(Starting/Stopping)不重复发起、直接进等待。
- **等待**:`waitSeconds`(默认 300,0 = 发起即成功,上限 3600)——每 5s 轮询直到全部到达目标态;超时的台改判失败(带最后已知状态);轮询瞬时失败下轮重试,超时统一裁决。等待过程经节点日志流式上报。
- **逐台容错**:单批 ≤100 台;一台失败(实例不存在/不支持节省停机/被 API 拒绝/等待超时)不阻断其它台,但**任一台失败节点即失败**(ErrBuildFailed,阻断下游)。
- **执行位置**:控制机本地(HTTPS 调阿里云 OpenAPI,不依赖目标机在线)——关机节点自身不受影响;但被关机机器若是后续节点的远程构建机或 SSH 部署/健康检查目标,按 DAG 顺序自然失败,编排时注意。

## config(冻结契约 `Record<string,string>`)

| 键 | 说明 |
|---|---|
| `accountId` | 必填。设置→阿里云 ECS 的账户 id(Region 随账户)。 |
| `action` | 必填。`start` 开机 / `stop` 关机(固定节省停机模式)。 |
| `selector` | `instances`(缺省)/ `tags`。 |
| `instanceIds` | selector=instances 必填。逗号分隔实例 ID(≤100)。 |
| `tagSource` | selector=tags 用。`aliyun`(缺省)/ `platform`。 |
| `tags` | selector=tags 必填。逗号分隔 `k=v` 或裸 `k`(≤32 项,AND)。 |
| `forceStop` | 可选(仅 stop)。`true` = 强制断电式停机。 |
| `waitSeconds` | 可选。默认 300;0 = 不等待。 |

保存期校验(`validateEcsPower`):枚举/非空/规模/标签项格式;账户与实例存在性由运行时裁决(账户可能后配;标签选择天然运行时解析)。

## 实现位置

- 客户端 `internal/aliyunecs/client.go`:`StartInstance` / `StopInstance(StoppedMode=StopCharging[, ForceStop])` / `InstanceStatuses`(DescribeInstanceStatus,逗号串 ≤100);`Instance.Tags` 解析(DescribeInstances 的 Tags.Tag[])。
- 领域 `internal/aliyunecs/power.go`:`Service.Power(PowerInput) → PowerResult`——选择解析(三种)→ 状态预检 → 逐台发起 → 可选等待;`Log func(string)` 流式日志回调。
- 执行器 `internal/build/ecs_power.go`:`isEcsPowerJob` + `runEcsPowerJob`(config 解析 → 窄接口 `stageEcsPowerController`);分发三处(dag_stage_exec.go 类型分桶/串行循环/job 级 DAG switch);`build.WithEcsPowerController` 注入,main 装配 `aliyunSvc`。
- 前端:`jobConfigSchema.ts` `ecs_power` spec(新 FieldKind `aliyunAccount` 账户下拉 + `ecsInstances` 实例多选新组件 `EcsInstanceMultiSelect.vue`,值序列化为逗号串单键,同 mesh_push items 先例;`tags` 复用 `labelSelector` 控件)+ `JobTypeIcon.vue` 电源图标 + 8 语言 `pipelineJob.ts`。
- AI 生成:`internal/ai/catalog.go` 登记 + `generate.go` prompt 组合指引/config 说明。

## 边界与取舍

- **关机无普通计费模式选项**(需求固定节省停机);不支持重启动作(reboot 可用 stop+start 组合)。
- **不做定时开关机**:现有 cron 触发器 + 流水线组合即可实现。
- **阿里云标签过滤在客户端**(全量拉取后过滤):≤5000 台护栏内可接受,规避 RPC 数组参数(Tag.N)格式分歧;平台标签匹配复用 runner 语义,与构建机选择完全一致。
- **服务未装配**(`WithEcsPowerController` 未注入,如测试)→ 节点记「未注入,跳过」放行(与 oss_upload nil 同款向后兼容)。
- **httpapi fake 同步**:`fakeAliyunSvc` 需实现 `Power`(测试桩)。
