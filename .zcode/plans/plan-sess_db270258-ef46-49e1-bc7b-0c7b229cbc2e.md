# 流水线任务体系整改计划

## 〇、部署产物精确绑定(最优先,替换「自动」)

**现状**:`pickStageArtifact`(`internal/deploy/deploy.go:416`)按「文件优先、没文件才用镜像」从全 run 产物里瞎挑,多构建节点时不可控。

- 配置键(job.config 扁平 KV):`deployMode`(`artifact` 默认 | `command` 仅执行重启命令)、`artifactJob`(产物来源节点 job id,**必填**,严格匹配产物元数据 `sourceJob`)、`artifactName`(可选 glob 消歧;同源 >1 件且未填 → **报错列出候选**,0 件 → 明确报错,绝不静默换产物)。
- 前端 `JobDrawer.vue` 新增 prop:由 `PipelineCanvas` 传入上游产出节点清单(节点名+类型+声明产物),部署节点用下拉选择写 `artifactJob`,无「自动」;`deploy_container` 只列产镜像节点(build_image),`deploy_ssh` 只列产文件节点(脚本类+artifactPath)。
- 后端 `pickStageArtifact` 严格过滤;存量配置(无新键)保留旧兜底 + 弃用告警日志。`internal/ai/generate.go` 要求 AI 生成的部署节点必填 `artifactJob`。

## 一、构建任务按语言区分(替换「前端构建/后端构建」)

新增 4 个脚本类模板节点(复用 `script` 容器执行路径,后端仅扩 dispatch):

| 新类型 | 名称 | 预置配置 |
|---|---|---|
| `build_nodejs` | Node.js 构建 | `node:20`,`npm ci && npm run build`,产物 `dist` |
| `build_java` | Java 构建 | `maven:3.9-eclipse-temurin-21`,`mvn -B package`,产物 `*.jar`(注明 Gradle 改镜像+命令) |
| `build_golang` | Golang 构建 | `golang:1.22`,`CGO_ENABLED=0 go build -o bin/app ./...`,产物 `bin/app` |
| `build_python` | Python 构建 | `python:3.12`,`pip install -r requirements.txt && python -m build`,产物 `dist/*` |

- `jobConfigSchema.ts`:新增 4 spec(category=build),`PICKABLE_TYPES` 移除 `build_frontend`/`build_backend`/`deploy_frontend`(spec 保留供存量渲染);`SCRIPT_CLASS_TYPES` 加新键。
- `internal/build/dag_stage_exec.go` `isScriptJob` 加新 4 键(旧键兼容);`internal/ai/catalog.go`、`generate.go` 同步;`JobTypeIcon.vue` 补图标;README 更新。

## 二、部署重做

### 2.1 统一滚动策略:批次 + 预检故障机优先 + 网关联动(两种并存)

**裁剪**:删除 `interactive.go` 及 `POST /runs/{id}/deploy/continue|abort` 路由/handlers/前端续发中止 UI 与 API;canary/blue_green 策略代码删除;`NormalizeStrategy` 把旧策略值全部归一 rolling(存量兼容)。`servicereg` 包保留(独立功能页)。

**统一滚动执行序** `deployRolling(...)`:

1. **预检排序(故障机优先)**:配置了健康检查时,滚动开始前用该配置对所有目标机逐台预检(复用 `runHealthCheck`),**预检不过的机器排到队首**先修,其余按稳定顺序;预检结果逐机写入人读日志。未配健康检查 → 保持稳定顺序。
2. **分批**:首批 = 排序后前 `firstBatchSize` 台(默认 1),之后每批 `batchSize` 台(默认全部剩余);**任一批有失败立即停止后续批次**,未轮到机器保持 pending。
3. **每机执行(网关联动两种并存)**:
   - image 产物 + 网关托管(`ResolveInstances` 命中)→ **maxSurge 换实例**:pull → 起新容器 → 预热健康 → `SwapInstance`(upstream 原子换,单次 reload)→ 排空 → 停旧(`rollOneInstance` 吸收为内部路径;失败删新容器即可,无需回滚)。
   - 其余 → **摘→部署→健康→挂回**:网关托管机先 `SetInstanceAttached(false)` 摘除 + reload,再部署,健康通过后 `SetInstanceAttached(true)` 挂回;部署失败**保持摘除**并报 failed;挂回失败仅告警重试。非网关机器照旧直接部署。
   - `deploy.InstanceGateway` 接口增加 `DetachInstance/AttachInstance`(main.go 适配 `servicereg.SetInstanceAttached`)。
4. **回滚语义(每机独立,保持现状)**:失败机各自回退到自己的上一版本,已成功机器保留新版,批次失败只停止铺开。**修缺口:文件模式回滚切回软链后,在上一发布目录尽力重跑一次 `restartCommand`(失败仅记告警)**;网关托管机回滚成功(健康通过)后再挂回 upstream。
5. 批次参数入 `DeployInput` 与节点 config:`firstBatchSize`、`batchSize`。

### 2.2 修复 deploy 节点健康检查不生效
`DeployForStage` 现硬编码传 nil HealthCheck → 从 config 读 `healthUrl`/`healthRetries`/`healthIntervalSeconds` 构造,失败触发回滚(激活既有回滚链);部署节点表单加健康检查字段组。

### 2.3 新增「容器部署」节点 `deploy_container`
- 归入 `isDeployJob`,固定 image 产物,复用 image_release 链路(`docker pull → rm -f → run -d -p … [runArgs]`、健康门控、失败回滚上一版镜像)。
- 表单:目标机、产物来源(仅 build_image 节点)、容器名、端口、runArgs、健康检查、可选 registry 凭据;配了凭据先在目标机 `docker login`(vault reveal,不落日志)再 pull。

### 2.4 SSH 部署改名「主机部署」并修正
- 表单按「部署目标 → 产物来源 → 滚动批次 → 健康检查 → 重启命令」分组;描述明确 `<deployPath>/releases/<runId>/` + `current` 原子软链机制。
- `internal/target/target.go`:`Upload` 用独立长超时(默认 15 分钟,`PIPEWRIGHT_SSH_UPLOAD_TIMEOUT` 可调)。

### 2.5 `health_check` 节点真正接线(现为空转占位)
- 经 selector/serverId 解析目标机,复用 `deploy.HealthCheck` 在目标机探测(重试);无目标机且 http 模式时从平台本机探测;最终失败则 job/阶段失败。serial 与 job-DAG 两条 dispatch 路径都接。

### 2.6 目标选择器:选值控件 + 显式且/或

**选值控件(不再手敲语法)**:`jobConfigSchema.ts` 新增字段 kind `labelSelector`;`JobDrawer.vue` 复用 `GET /api/servers` 汇总标签 key 与各 key 取值集合,按「key 下拉 → value 下拉 → 可加多条件」组合,写回 `config.selector`(保持 `k=v,k=v` 格式);机器无标签时提示先打标签。`deploy_ssh`/`deploy_container`/`health_check` 表单统一替换;`serverId` 保留 server picker;RunDetail 手动部署同步替换。

**且/或显式选择**:当前逗号写死为且(`internal/runner/selector.go` AND 语义,构建机池共用,不动共享语法)。
- 新增配置键 `selectorMode`:`all`(满足全部条件,且,**默认** = 存量语义)| `any`(满足任一条件,或)。
- 后端:`internal/runner/selector.go` 增加纯函数 `MatchSelectorAny`(逐项任一命中);`internal/deploy/selector_targets.go` 标签圈选按 `cfg["selectorMode"]` 选 all/any;手动部署 `DeployInput` 同步加 `selectorMode`。
- 前端:标签条件 ≥2 时显示「匹配方式」单选(满足全部/满足任一,默认满足全部),单条件时隐藏;`server:<id>` 钉单机不受影响。
- AI 提示词补充 `selectorMode` 可选说明。

### 2.7 产物收集与打包显式化(替换硬编码)

**现状**(`internal/build/dag_stage_exec.go:827` + `artifact_persist.go`):类型按路径自动判(目录=dist、*.jar=jar、其它文件=archive);目录**一律** tar.gz;tar 包内**固定去顶层目录**(内容铺包根,部署解包直接铺 release 根),既不可见也不可选;jar 落盘名固定原文件名。

- 脚本类构建节点新增显式配置(表单 + 后端生效):
  - `artifactPackMode`:`tar`(目录产物打包 tar.gz,默认)| `none`(不打包:制品库按文件清单存储,metadata 记 `format=files` + 相对路径清单;部署端逐文件上传到 release 对应相对子路径;跨阶段 restore 按清单还原)。
  - `tarLayout`:`contents`(包内去顶层目录 = 现行为)| `top`(保留顶层目录,解包后文件在 `<release>/<目录名>/` 下)。部署端解包命令不变,语义由打包侧决定。
  - `artifactRename`(可选):jar/单文件部署落盘名。
- 构建日志明示实际行为:「已归档 dist → tar.gz(不含/含 dist/ 前缀)」/「已按文件清单归档(N 个文件)」。
- **存量默认值 = 现行为**(tar + contents),不悄悄改变已有流水线部署结果;产物类型判定维持自动,仅打包行为显式化。

## 三、阶段自由添加与命名(去掉「每类一个」限制)

**现状**:`PipelineCanvas.addStage()` 固定序列 `['build','deploy','notify','custom']` **每个 kind 只允许加一次**,之后全是「自定义N」;`StageDrawer` 无改名入口。后端本就无数量限制。

- `addStage()` 重做:点「添加阶段」→ 小型选择器(构建/部署/通知/通用,均可无限次选)→ 创建默认名「阶段 N」→ 自动打开 StageDrawer 聚焦名称输入。
- `StageDrawer.vue` 顶部新增「阶段名称」文本框与 kind 下拉,emit `update-name`/`update-kind` → `updateStage` 落库,画布列头即时刷新。
- `localizeName` 仅对未改名的默认名本地化;后端无需改。

## 四、阶段级审批门强化(不加新节点)

- `StageColumn.vue`:阶段 `gate=true` 时阶段头渲染审批徽标(盾牌+「审批」)。
- `StageDrawer.vue`:完善提示(运行暂停、运行详情页批准/拒绝、通知签名链接、24h 超时自动拒绝)。
- 核对 `RunDetail.vue` waiting_approval 审批条完整可用。

## 五、制品孤儿 GC(retention 补口子)

**现状**:retention Sweeper 删过期 run 连带删 `run_artifacts` 元数据行,但对 `artifacts/` 磁盘 blob **零清理**,孤儿文件永久累积。

- Sweeper 增加 GC 步骤:周期列出 `artifacts/` 全部 blob 句柄,与 `SELECT DISTINCT reference FROM run_artifacts` 引用集合做差,无引用 blob 删除。
- **竞态防护**:构建侧先 `Put` 落盘、后写 `run_artifacts` 行,存在短暂无引用窗口 → GC 只删「无引用且 mtime 超过宽限期(默认 24h)」的 blob,双重判断防误删进行中构建的产物。
- 删除失败仅记日志;与保留期天然对齐(run 过期 → 元数据删 → blob 下轮 GC 回收)。

## 六、i18n、测试与验证

- 新增/删除 key 同步 8 语言目录(`keyParity`/`componentKeys` 测试把关)。
- 后端测试:产物精确绑定(命中/多件报错/零件报错/command)、批次语义(首批数、失败中止、故障机排序)、预检排序、网关摘挂与换实例两路径、回滚后重跑重启命令、deploy HealthCheck 接线、health_check 节点、isScriptJob 新键、孤儿 GC、产物打包显式化(tar/none/tarLayout 两档、rename、存量默认不变)、标签选择器 any/all 两模式;删除 canary/blue_green/interactive 测试。
- 前端:`jobConfigSchema.test.ts` 不变量;排查 e2e 中旧标签/旧策略/添加阶段序列的引用并更新。
- 验证:`go build ./... && go vet ./... && go test ./...`;`cd web && npm run build` + vitest;起 dev 服务浏览器实测:任务目录、部署表单产物下拉、标签选值与且/或切换、打包选项、无限添加阶段并改名、画布审批徽标。