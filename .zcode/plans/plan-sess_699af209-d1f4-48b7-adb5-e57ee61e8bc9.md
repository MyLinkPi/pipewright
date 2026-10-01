# 部署目标改为标签选择器圈选 + 空目标跳过成功

## 语义变更(核心)

部署目标机的确定方式从「人工勾选 serverIds」改为「**标签选择器**」,完整复用构建机池的选择器语法(`internal/runner/selector.go` 的纯函数层:空串 / `server:<id>` 钉单机 / `linux,arch=arm64` AND 标签项):

- **空选择器、或非空但零命中(含 `server:<id>` 指向不存在的机器)→ 跳过即成功**:返回空结果、不写 `deploy_targets`、不动 run 终态(不触发 `SetDeployTerminal`、不触发失败诊断)。
- **选择器语法非法 → 422**(客户端错误,必须拦截)。
- **显式 `ServerIDs` 路径保留**(存在性校验不变):环境回滚(`makeRollbackEnvironmentHandler` 从 `deploy_targets` 历史回放)、RetryFailed 的可选过滤、以及 API 兼容都依赖它。`ServerIDs` 非空时优先于 selector。
- **不做项目级配置、不建新表、无迁移**——选择器是部署动作的参数,不与项目绑定。

## 后端改动

### 1. `internal/deploy/selector_targets.go`(新文件):目标解析

```go
// resolveTargets: 显式 IDs 优先;否则解析 selector(server:<id> → 单机;标签项 → MatchSelector
// 过滤 targets.List())。空/零命中 → (nil, nil) = 调用方跳过成功;语法非法 → ErrInvalidSelector。
func (s *service) resolveTargets(ctx context.Context, serverIDs []string, selector string) ([]*target.Server, error)
```

复用 `runner.PinnedServer / ParseSelector / MatchSelector`(deploy → runner 无环:runner 只依赖 store)。零命中时记一条日志/原因供上层展示。

### 2. `internal/deploy/deploy.go`

- `DeployInput` 增加 `Selector string`;`Deploy()` 删除 `len(ServerIDs)==0 → ErrNoServers` 检查,改为经 `resolveTargets` 解析;零目标 → 提前返回 `[]TargetResult{}, nil`(interactive 策略同样直接跳过,不产生 pending)。产物/Run 状态校验仍在目标解析**之后**照旧执行(顺序:run 状态 → 产物 → 目标解析)。
- `DeployForStage` 签名从 `serverIDs []string` 改为 `selector string`(`server:<id>` 即旧单机形式;唯一调用方本来只传 1 个 ID);空/零命中 → 返回空结果 nil(含 `runCommandOnly` command 型分支同样处理)。
- 删除 `ErrNoServers`(不再产生);新增 `ErrInvalidSelector`。

### 3. `internal/build/dag_stage_exec.go` — `runDeployJob`

- 节点配置读取:`selector` 键优先;旧 `serverId` 键映射为 `server:<id>`(向后兼容已有流水线)。
- 两者皆空:**删除现有「serverId 空 → 阶段失败」逻辑**,改为日志「未配置目标选择器,跳过部署」+ 节点成功;`DeployForStage` 返回空结果同样按跳过成功处理。
- 同步更新 `builder.go` 里 deployer 接口签名。

### 4. `internal/httpapi/deploy.go`

- `deployRequest` 增加 `Selector string \`json:"selector"\``;非空时 `runner.ValidateSelector` 校验 → 422 `invalid_deploy_selector`。
- `writeDeployError` 移除 `ErrNoServers → no_servers` 分支,增加 `ErrInvalidSelector` 分支。
- Retry/Continue/Abort 不动(Retry 的 `ServerIDs` 可选过滤语义不变)。

## 前端改动(web/)

- **新共享工具** `web/src/utils/selector.ts`:把 `RunnerPanel.vue` 内联的 `matchServers`(逗号 AND 项、纯 tag ≠ k=v、`server:<id>` 钉死)抽出来,RunnerPanel 与部署面板共用。
- **`web/src/views/RunDetail.vue` 部署面板**:删除服务器 checkbox 列表与 `selectedServerIds`;换成选择器输入框(等宽字体,placeholder 说明语法,留空=跳过)+ 实时「命中 N 台:机器名」预览;`canDeploy` 去掉「必须选机器」条件;`handleDeploy` 发 `selector`(按项目记 localStorage 上次值作默认);空结果返回时 toast「未命中目标机器,已跳过部署」。
- **`web/src/components/pipeline/jobConfigSchema.ts`**:`DEPLOY_SSH_FIELDS` 增加 `selector` 文本字段(说明:标签选择器,支持 `server:<id>`,留空跳过;`serverId` 字段保留标注为旧版单机写法)。
- **`web/src/api/runs.ts`**:`DeployRunInput.selector?: string`,`serverIds` 改可选。
- **i18n**:8 个语言包(de/en/es/fr/ja/ko/zh-CN/zh-TW)新增/调整 key——`keyParity.test.ts` 会强制对齐;清理不再用的 `noServers` 相关 key。

## 测试

- `internal/deploy`:resolveTargets 单测(空→nil,nil;`server:<id>` 命中/不存在→跳过;标签多机命中;语法非法→错误);Deploy 空目标→成功且不写 deploy_targets、不改终态;DeployForStage 零命中→空结果 nil。
- `internal/build`:runDeployJob 无 serverId/selector → 阶段成功 + 跳过日志(改掉现有「空 serverId 失败」的测试预期)。
- `internal/httpapi`:带 selector 的 deploy 请求往返;非法 selector → 422;空 selector → 200 空数组。
- 全库 grep `ErrNoServers`/`no_servers` 清理引用。

## 文档

- 新增 `specs/deploy-target-selector.md` 测试计划(沿用 specs/ 惯例),记录新语义:空/零命中跳过成功、显式 IDs 优先、语法非法 422、回滚/重试不受影响。
- README 中若有「勾选服务器部署」的描述则同步更新(实现时 grep 确认)。
