# 部署目标标签圈选(deploy target selector)— 测试计划

构建机池标签语义(FR-8-19,`internal/runner/selector.go`)的部署侧复用:部署目标从「人工勾选
serverIds」改为「选择器圈选」,与构建共用 `servers.labels` 同一套标签;**空目标 = 跳过即成功**,
自动化链路(环境晋升 / 流水线 deploy_ssh 节点)不因未配目标而中断。

## 语义(冻结)

- 目标解析顺序:显式 `serverIds`(回滚历史回放 / 显式 API)优先,存在性从严(任一不存在 →
  `ErrServerNotFound` 整次拒绝);否则按 `selector` 圈选。
- 选择器语法 = 构建机池:空 = 无目标;`server:<id>` 钉单机;`linux,arch=arm64` 逗号 AND 标签项。
- **空选择器 / 零命中(含 `server:<id>` 指向已删机器)→ 跳过即成功**:HTTP 200 空 targets;
  不写 `deploy_targets`、不动 run 终态、不触发失败诊断。
- 选择器语法非法 → 422 `invalid_deploy_selector`(客户端错误,必须拦截)。
- Retry / Continue / Abort / 环境回滚不受影响(全部工作在已持久化的 `deploy_targets` 上;
  回滚经显式 ServerIDs 回放到同一批机器)。
- 流水线 deploy_ssh / deploy_frontend 节点:配置 `selector` 键优先;旧 `serverId` 键映射为
  `server:<id>`(既有流水线零改动);两者皆空 / 零命中 → 节点按跳过成功,不再阶段失败。
- 无项目级绑定、无新表、无迁移 —— 选择器是部署动作的参数。

## 覆盖矩阵(已落地的自动化用例)

### 领域层(internal/deploy/selector_targets_test.go)
- [x] 标签圈选:AND 语义,只部署命中的机器;纯 tag 不匹配 k=v(`env` ≠ `env=prod`)
- [x] 显式 serverIDs 优先于 selector;任一不存在 → ErrServerNotFound(不留半截)
- [x] 空选择器 / 零命中 / 钉单机已删 → 空结果 + nil:不写 deploy_targets、run 终态不动
- [x] DeployForStage:`server:<id>` 钉单机与标签圈选走通;零命中 → 空结果 nil;语法非法 → ErrInvalidSelector
- [x] TestDeployValidationErrors 更新:无目标不再报 ErrNoServers(错误已删除)

### HTTP 层(internal/httpapi/deploy_test.go)
- [x] `selector` 字段圈选:只部署打了匹配标签的机器(2 台命中 / db 机器不进 targets)
- [x] 空目标三态(空选择器 / 零命中 / server:gone)→ 200 空 targets 数组;run-detail targets slot 保持 null
- [x] 语法非法 → 422 invalid_deploy_selector
- [x] 既有 serverIds 路径回归(存在性 422、CSRF、产物校验)不变
- [x] 空目标响应不回读旧 targets(TestDeployEmptyAfterSuccessReturnsEmptyTargets):同 run 先成功
  部署再空部署 → 响应为空数组(修复前回读上一次旧 targets,"跳过"伪装成"成功")

### 部署正确性加固(2026-10 全链路审查修复)
- [x] 可变 tag 假回滚修复:prevImage 改记镜像 digest(`{{.Image}}`),回滚按 digest 起容器
  (TestRollingImageRollbackTargetsDigest);此前 `latest` 被 pull 重指向后"回滚"起的还是坏镜像
- [x] 健康门控/回滚/docker pull 独立 ctx 预算,不再共用 60s execTimeout(pull 15min 对齐
  uploadTimeout);修复前健康耗尽后回滚首条命令即 DeadlineExceeded、current 软链滞留坏版本
- [x] 回滚命令失败(含非零退出)→ 状态记 failed + "回滚未确认"message,不再谎称 rolled_back;
  retryableTargetStatus 含 failed,重试不受影响。**行为变更**:部分原 rolled_back 场景现显示 failed
- [x] maxSurge 预热 healthExec/command 探测目标改为新实例容器名 `<base>-r<hex>`(修复前打旧
  容器假通过或 no such container 必失败)
- [x] 显式 serverIDs 保序去重(修复前同机部署两次、RetryFailed 同机竞态)

### 流水线派发(internal/build/dag_stage_exec_test.go)
- [x] 节点 `selector` 键优先,旧 `serverId` → `server:<id>` 规范形式透传
- [x] 两者皆空 → 不调部署服务、节点跳过成功(不再 ErrBuildFailed)
- [x] DeployForStage 返回空结果(零命中)→ 节点跳过成功
- [x] 纯部署阶段(无 script job)配了构建机池也强制本地执行(internal/build/remote_stage_exec_test.go
  TestStageExecutorDeployOnlyStageStaysLocal):不占构建机槽、零远程动作。修复前该场景被
  「远程放行」空转,deploy_container 的选择器完全不生效(部署由控制机 SSH 驱动,与构建机池无关)

### 前端(web/src/lib/selectorMatch.test.ts)
- [x] 与服务端同语义:AND 项、纯 tag ≠ k=v、`server:<id>` 钉单机、零命中/空 → 空数组
- [x] LabelSelectorEditor 回显(web/src/components/pipeline/LabelSelectorEditor.test.ts):
  挂载时 modelValue 已有值(重新打开已保存的选择器)→ 反解出全部条件行;空值 → 无行;
  自身 emit 回声不重建行、外部真变更才重解(修复:lastEmitted 以 undefined 哨兵起始,
  否则 immediate 首次回调被跳过,已保存选择器永远回显为空)
- [x] RunDetail 部署面板:选择器输入 + 实时命中预览(RunnerPanel 共用 lib/selectorMatch);
  空目标提交 → 200 后面板明示「已跳过」;上次选择器按项目记 localStorage

## 手工清单

- [ ] RunDetail 部署面板输入 `web,env=prod` → 预览「命中 N 台 — 机器名」;清空 → 提示留空跳过
- [ ] 留空提交 → 200,面板出现跳过提示,run 状态与 targets 不变
- [ ] 流水线编辑器 deploy_ssh 节点出现「目标选择器」字段;留空跑流水线 → 节点日志「未配置目标选择器,跳过部署」且成功
- [ ] 环境页回滚旧部署 → 仍部署到当年那批机器(显式 ServerIDs 回放)
