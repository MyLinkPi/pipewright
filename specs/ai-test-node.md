# AI 测试节点(ai_test)与仓库测试约定 — 设计

> 状态:草案(未实现;前置项与分期见「落地顺序」)。相关:`internal/ai`(AI 服务,密钥仅存 vault)、`internal/build/dag_test_report.go`(JUnit/质量门禁)、`internal/build/shell_driver.go`(工具链容器启动参数)、`internal/build/ecs_power.go`(控制面节点执行器范本)、`internal/pipeline/ecs_power.go`(保存期校验范本)、`web/src/components/pipeline/jobConfigSchema.ts`(节点 schema)。

## 目标与分层

测试资产(测试需求 + 测试脚本)一律放代码库约定位置,与业务代码同 commit、同评审、随分支演进(与 `.pipewright.yml` pipeline-as-code 同构);流水线节点只做**薄执行器**——声明套件类型/过滤/环境映射,不内联任何测试内容。执行与判定分三层:

- **确定性回归**(Playwright E2E / 接口套件):script 类任务在工具链容器执行,AI 不参与判定;
- **AI 验收**(新节点 `ai_test`,分类 quality):自然语言需求 → LLM 结构化裁决,gate/advisory;
- **AI 增强**(生成用例、失败诊断):只出现在「生成」与「分析」两端,中间执行路径不依赖任何 AI(NFR-10)。

## 仓库约定(test-as-code)

```
.pipewright/
  tests/
    requirements/        # 测试需求(*.md + frontmatter)——ai_test 节点输入
      smoke/login.md
      checkout.md
    api/                 # 接口套件:suite.yaml 声明 runner(image + 入口命令),cases/ 放用例
    reports/             # 统一 JUnit 输出 junit-<suite>.xml(运行产物,应 gitignore)
e2e/ + playwright.config.ts   # 前端沿用 Playwright 标准布局(生态约定优先,不挪进来)
```

- **统一报告路径**:无论何种 runner,JUnit 一律写 `.pipewright/tests/reports/junit-<suite>.xml` → 质量门禁的 reportPath 退化为常量(pytest `--junitxml`、Playwright `--reporter=junit` 均原生支持)。
- **需求文件 = Markdown + YAML frontmatter**(只此一种格式,不做 .yaml 双轨):

```markdown
---
id: checkout-flow        # 必填,仓库内唯一
severity: blocker        # blocker = gate(缺省,fail-closed)/ suggest = advisory
target: ${BASE_URL}      # 可选 HTTP URL;${VAR} 引用运行参数/构建变量;缺省继承节点 target
---
登录测试账号,加购任一商品并结算。期望:订单创建成功,金额与购物车一致。
```

- 目标解析顺序:**需求 frontmatter `target` > 节点 `target`**,两级皆空的需求 → 发现期判 fail(无被测对象,评审无从谈起)。有值时抓取响应文本(限长 + 脱敏后)作评审上下文;无 target 则纯需求文本评审(如评审上游产物)。抓取出发点:缺省控制面直接抓取;节点绑定 `deployJob` 时改**经 SSH 在被测机上执行**(curl,命令数组化),解决内网服务中控不可达的问题。
- 冒烟/全量分层靠子目录 + 节点 glob 过滤实现,不另造标签机制。

## ai_test 节点语义

控制面执行(同 ecs_power:不占 workspace,HTTPS 出网即可)。逐需求独立评审:

1. **发现即校验**:约定路径不存在 / frontmatter 解析失败 / id 重复 → fail fast(错误带文件名);发现 0 个需求 → 判 fail(防路径 typo 静默绿灯);日志首行回显「发现 N 个需求(blocker X / suggest Y)」。
2. **评审**:每条需求一次 LLM 调用(temperature 0),严格 JSON 裁决 `{verdict: pass|fail, reason, evidence}`;解析失败重试一次,仍失败按 fail 处理(诚实边界,不做宽松解析)。prompt 中指令与数据围栏分离——需求正文与抓取内容是不可信数据(防提示注入)。
3. **裁决**:任一 blocker fail → 节点失败(ErrBuildFailed,阻断下游);suggest fail 不阻塞,但进日志与运行摘要。结果经 `$PIPEWRIGHT_ENV` 输出(`ai_test_passed` 等)供下游节点消费。
4. **脱敏**:发往 LLM 前过 run Masker;${VAR} 展开后日志只回显键名不回显值。

## config(冻结契约 Record<string,string>)

| 键 | 说明 |
|---|---|
| `target` | 节点级缺省目标 URL(如 `http://${BASE_URL}` 或网关域名);${VAR} 引用运行参数/构建变量/`${DEPLOY_HOST}`。需求 frontmatter `target` 按条覆盖。 |
| `deployJob` | 可选。上游 deploy 节点引用(同 `artifactJob` 校验先例):绑定被测部署的目标机——注入 `${DEPLOY_HOST}`(目标机主机地址),URL 抓取改经 SSH 在被测机上执行。 |
| `filter` | 需求 glob,缺省 `.pipewright/tests/requirements/**/*.md`(如 `**/smoke/**` 取冒烟子集)。 |
| `mode` | `auto`(缺省,按各文件 severity)/ `gate`(全部提升)/ `advisory`(全部降级)。 |
| `onAiUnavailable` | AI 未配置/不可达时:`fail`(缺省——测试静默跳过比失败更危险)/ `skip`(记警告放行)。 |
| `timeoutSeconds` | 节点整体超时,默认 300,上限 3600。 |
| `model` | 可选模型覆盖;不填用平台 AI 配置的默认模型。 |

保存期校验(`validateAITest`):mode/onAiUnavailable 枚举、timeout 范围、filter 非空可解析、`deployJob`(若有)必须引用上游 deploy 类节点;路径存在性与文件合法性由**运行时发现步骤**裁决(保存期校验只看 spec JSON,看不见仓库)。

## 确定性回归(无需新节点,P1)

- Playwright:script 任务 + `mcr.microsoft.com/playwright:v1.x` + `npx playwright test --reporter=junit`;测试目标 = 已部署环境(`BASE_URL` 用类型化运行参数传入)或 sidecar 服务(network alias 互访,远程构建机同样适用);镜像走内置 registry 的 pull-through 缓存(约 2GB,值得缓存)。
- **Resource 扩展**(P1 唯一必改代码):`pipeline.Resource` 加 `Init bool`(Chromium 子进程收尸,无 init 会积僵尸)与 `ShmSize string`(/dev/shm 默认 64MB,复杂页面崩 Chrome),随 cpu/memory 同路流转:job 配置 → `shell_driver.go RunToolchain` → 远程透传(remote_stage_exec.go)。**不做任意 docker 参数透传**(守住命令数组化防注入基线)。
- 出厂一个「Playwright E2E」自定义节点预设(library 机制,零代码),预填镜像/命令/JUnit 报告路径与 artifactPath(HTML 报告 + trace)。
- 接口套件:runner 不在平台层锁定,只要能产 JUnit(pytest / hurl 均可),`api/suite.yaml` 自声明镜像与入口命令。

## 结果呈现与通知

**报告三层(全部复用现有管道,无新 UI)**:

1. **数值与失败明细**:套件 job 配 `testReport=junit` + `reportPath=.pipewright/tests/reports/junit-<suite>.xml` → 现有质量门禁 + run 详情 TestReportPanel 直接展示(通过/失败数、失败明细、覆盖率)。已知约束:阶段级报告采集只取**第一个**声明 job(`dag_test_report.go reportSpecFromStage`)——e2e 与接口套件分阶段放,或按「落地顺序」P2 扩展为多声明合并采集。
2. **富报告与取证**:script job 的 `artifactPath`(多行/glob/打包模式)上传 `playwright-report/` 与 `test-results/` 为 run 产物——运行详情与产物管理页可下载;trace.zip 本地 `npx playwright show-trace` 回放,HTML 报告自包含可直接打开。app 内在线预览不做(任意 HTML 注入面,首期下载为主)。
3. **AI 验收结果**:ai_test 裁决**直接持久化为同构测试报告**(控制面直写 run 测试报告存储,不经工作区文件——远程构建机场景无本地 workspace 可写),TestReportPanel 与 JUnit 报告同屏呈现(每条需求 = 一个用例,reason 进失败明细);job Summary 一行结论;逐条 verdict 流式进节点日志。

**通知(复用事件→通道路由与模板)**:

1. **零改动基线**:blocker fail → 节点失败 → run 失败 → 现有 `build_failed` 事件照发。
2. **新增两个节点级事件**(先例:`health_check_failed`):`test_gate_failed`(门禁/验收 gate 失败,默认路由随 build_failed 同渠道,便于区别于普通构建失败做定制路由)与 `ai_acceptance_failed`(advisory 失败——run 整体绿但建议项未过;这是**唯一必须新事件**的场景:run 不失败,现有事件不会触发)。
3. **模板变量增量**:TemplateVars(冻结契约,**只增不改**)补 `{{testSummary}}`(N 通过/M 失败)、`{{failedTests}}`(失败用例/需求名,超长截断)、`{{reportUrl}}`(run 详情锚点);`RenderText` 已被流水线内 notify 节点复用,占位增量自动惠及内联通知模板。

## 执行位置与密钥边界

- ai_test 控制面执行;**远程构建机场景 workspace 在远程** → 需求目录(纯文本,KB 级)经 SSH 取回后再评审。
- **AI key 永远留中控 vault,绝不下发构建容器**;构建容器内调 AI(如 Playwright 容器内做 AI 断言)留给 P4 的 run-scoped 代理端点(`/api/runner/ai/proxy` + 每-run 令牌与用量限额),首期不做。

## 落地顺序

1. **P1 确定性回归**:Resource init/shmSize + Playwright 在流水线跑通(用本仓库 `web/e2e-real` dogfood)+ 仓库约定文档化 + 预设节点;报告/取证即走 `testReport=junit` + `artifactPath`(见「结果呈现与通知」,零新代码)。
2. **P2 ai_test 节点**:本 spec 核心;裁决同构落库(报告面板可见)+ `test_gate_failed`/`ai_acceptance_failed` 事件与模板变量增量;可选:阶段多报告合并采集(解除「一阶段一报告」限制)。`.pipewright.yml` 经 `config:` 透传即刻可用,专用语法糖块可选。
3. **P3 失败诊断增强**:diagnoseHook 收集 `.pipewright/tests/reports/` 与 Playwright 失败产物喂 AI(mask-first 先行);截图裁决需给 `chatWithTokens` 增加多模态(图片)content 支持。
4. **P4 生成与代理**:AI 生成需求/用例 → PR(目标路径即约定目录,人审后合入);run-scoped AI 代理端点。

## 实现位置(预期)

- `internal/pipeline/ai_node.go`:`JobTypeAITest = "ai_test"` 常量 + `validateAITest` 挂 normalizeSpec 末尾(仿 `internal/pipeline/ecs_power.go`)。
- `internal/build/ai_node.go`:`isAITestJob` 谓词 + `runAITestJob`(窄接口 `stageAITester`,不直接依赖 internal/ai 包);`deployJob` 目标机解析与 SSH 抓取经窄接口注入(仿 `stageEcsPowerController` 模式);分发共四处:dag_stage_exec 三处(类型分桶/串行循环/job 级 DAG switch)+ remote_stage_exec 控制面桶。
- `internal/ai`:新增 `JudgeRequirements`(复用 `chatWithTokens` 的流式消费/响应限长/SSRF 钉扎;temperature 0;严格 JSON + 单次重试);`ErrAINotConfigured` → 按 onAiUnavailable 裁决。
- 注入:`build.WithAITester`(BuilderOption,nil-guard 仿 deployer;服务未注入 → 记「未注入,跳过」,同 ecs_power/oss_upload 向后兼容款)。
- 报告落库:ai_test 裁决直写 `internal/run` 测试报告存储(同 `dag_test_report.go` 采集后的落库路径),不经工作区文件。
- 通知:`internal/notify/router.go` 注册 `test_gate_failed` / `ai_acceptance_failed`;`template.go` TemplateVars 只增不改地补测试占位(`{{testSummary}}`/`{{failedTests}}`/`{{reportUrl}}`)。
- 前端:`jobConfigSchema.ts` 注册 spec(category quality;可选新 FieldKind `aiModel` 下拉,数据源 `/api/settings/ai/models`)+ `JobTypeIcon.vue` 图标 + 8 语言 `pipelineJob.ts`(zh-CN 先定稿)。
- 无 DB 迁移(spec 以 JSON 存储);自定义节点预设机制直接复用。

## 边界与取舍

- **前端脚本不挪进 `.pipewright/`**:尊重 Playwright 标准布局,本地 `npx playwright test` 与 IDE 插件照常工作;平台只约定「发现规则」,不为测试工具重造目录。
- **需求不内联在节点配置**:全部走仓库约定(既定约束);临时验证走运维终端的 AI 助手。
- **AI 实时驱动浏览器(agent 探索模式)不做**:慢、贵、不稳定;基于截图/HTML 的 advisory 裁决(多模态)放 P3。
- **裁决不确定性诚实暴露**:JSON 不可解析重试一次后判 fail;Ollama 等本地模型的格式可靠性按同一策略处理。
- **0 需求/空套件即失败**;advisory 失败不阻塞但必须可见(日志 + 运行摘要)。
- **`.pipewright/tests/reports/` 由用户 gitignore**(文档提示,不做 lint 强制)。
- **HTML 报告不做 app 内在线预览**:任意 HTML 有注入面;产物下载 + 本地 `show-trace` 回放已够用,预览(iframe sandbox)留待真有诉求再评估。
- **通知占位集只增不改**:TemplateVars 是冻结契约,新增占位不破坏既有模板;不做破坏性改名/删除。
- **AI 配置保持平台级单 profile**:节点仅可覆盖模型名;多供应商/多 profile 是独立前置架构决策,本 spec 不裹挟。
- **deployJob 多台目标机时 MVP 取第一台抓取**并日志明示;多机扇出对齐 health_check 的逐台语义留后续。被测机需有 curl(部署目标机均为 SSH 可达的 Linux,curl 基本普适,诚实边界)。
