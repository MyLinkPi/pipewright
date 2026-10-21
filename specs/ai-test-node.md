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
target: ${BASE_URL}      # 可选 HTTP URL;${VAR} 引用运行参数/构建变量
---
登录测试账号,加购任一商品并结算。期望:订单创建成功,金额与购物车一致。
```

- `target` 有值时控制面抓取响应文本(限长 + 脱敏后)作评审上下文;无 target 则纯需求文本评审(如评审上游产物)。
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
| `filter` | 需求 glob,缺省 `.pipewright/tests/requirements/**/*.md`(如 `**/smoke/**` 取冒烟子集)。 |
| `mode` | `auto`(缺省,按各文件 severity)/ `gate`(全部提升)/ `advisory`(全部降级)。 |
| `onAiUnavailable` | AI 未配置/不可达时:`fail`(缺省——测试静默跳过比失败更危险)/ `skip`(记警告放行)。 |
| `timeoutSeconds` | 节点整体超时,默认 300,上限 3600。 |
| `model` | 可选模型覆盖;不填用平台 AI 配置的默认模型。 |

保存期校验(`validateAITest`):mode/onAiUnavailable 枚举、timeout 范围、filter 非空可解析;路径存在性与文件合法性由**运行时发现步骤**裁决(保存期校验只看 spec JSON,看不见仓库)。

## 确定性回归(无需新节点,P1)

- Playwright:script 任务 + `mcr.microsoft.com/playwright:v1.x` + `npx playwright test --reporter=junit`;测试目标 = 已部署环境(`BASE_URL` 用类型化运行参数传入)或 sidecar 服务(network alias 互访,远程构建机同样适用);镜像走内置 registry 的 pull-through 缓存(约 2GB,值得缓存)。
- **Resource 扩展**(P1 唯一必改代码):`pipeline.Resource` 加 `Init bool`(Chromium 子进程收尸,无 init 会积僵尸)与 `ShmSize string`(/dev/shm 默认 64MB,复杂页面崩 Chrome),随 cpu/memory 同路流转:job 配置 → `shell_driver.go RunToolchain` → 远程透传(remote_stage_exec.go)。**不做任意 docker 参数透传**(守住命令数组化防注入基线)。
- 出厂一个「Playwright E2E」自定义节点预设(library 机制,零代码),预填镜像/命令/报告路径。
- 接口套件:runner 不在平台层锁定,只要能产 JUnit(pytest / hurl 均可),`api/suite.yaml` 自声明镜像与入口命令。

## 执行位置与密钥边界

- ai_test 控制面执行;**远程构建机场景 workspace 在远程** → 需求目录(纯文本,KB 级)经 SSH 取回后再评审。
- **AI key 永远留中控 vault,绝不下发构建容器**;构建容器内调 AI(如 Playwright 容器内做 AI 断言)留给 P4 的 run-scoped 代理端点(`/api/runner/ai/proxy` + 每-run 令牌与用量限额),首期不做。

## 落地顺序

1. **P1 确定性回归**:Resource init/shmSize + Playwright 在流水线跑通(用本仓库 `web/e2e-real` dogfood)+ 仓库约定文档化 + 预设节点。
2. **P2 ai_test 节点**:本 spec 核心;`.pipewright.yml` 经 `config:` 透传即刻可用,专用语法糖块可选。
3. **P3 失败诊断增强**:diagnoseHook 收集 `.pipewright/tests/reports/` 与 Playwright 失败产物喂 AI(mask-first 先行);截图裁决需给 `chatWithTokens` 增加多模态(图片)content 支持。
4. **P4 生成与代理**:AI 生成需求/用例 → PR(目标路径即约定目录,人审后合入);run-scoped AI 代理端点。

## 实现位置(预期)

- `internal/pipeline/ai_node.go`:`JobTypeAITest = "ai_test"` 常量 + `validateAITest` 挂 normalizeSpec 末尾(仿 `internal/pipeline/ecs_power.go`)。
- `internal/build/ai_node.go`:`isAITestJob` 谓词 + `runAITestJob`(窄接口 `stageAITester`,不直接依赖 internal/ai 包);分发共四处:dag_stage_exec 三处(类型分桶/串行循环/job 级 DAG switch)+ remote_stage_exec 控制面桶。
- `internal/ai`:新增 `JudgeRequirements`(复用 `chatWithTokens` 的流式消费/响应限长/SSRF 钉扎;temperature 0;严格 JSON + 单次重试);`ErrAINotConfigured` → 按 onAiUnavailable 裁决。
- 注入:`build.WithAITester`(BuilderOption,nil-guard 仿 deployer;服务未注入 → 记「未注入,跳过」,同 ecs_power/oss_upload 向后兼容款)。
- 前端:`jobConfigSchema.ts` 注册 spec(category quality;可选新 FieldKind `aiModel` 下拉,数据源 `/api/settings/ai/models`)+ `JobTypeIcon.vue` 图标 + 8 语言 `pipelineJob.ts`(zh-CN 先定稿)。
- 无 DB 迁移(spec 以 JSON 存储);自定义节点预设机制直接复用。

## 边界与取舍

- **前端脚本不挪进 `.pipewright/`**:尊重 Playwright 标准布局,本地 `npx playwright test` 与 IDE 插件照常工作;平台只约定「发现规则」,不为测试工具重造目录。
- **需求不内联在节点配置**:全部走仓库约定(既定约束);临时验证走运维终端的 AI 助手。
- **AI 实时驱动浏览器(agent 探索模式)不做**:慢、贵、不稳定;基于截图/HTML 的 advisory 裁决(多模态)放 P3。
- **裁决不确定性诚实暴露**:JSON 不可解析重试一次后判 fail;Ollama 等本地模型的格式可靠性按同一策略处理。
- **0 需求/空套件即失败**;advisory 失败不阻塞但必须可见(日志 + 运行摘要)。
- **`.pipewright/tests/reports/` 由用户 gitignore**(文档提示,不做 lint 强制)。
- **AI 配置保持平台级单 profile**:节点仅可覆盖模型名;多供应商/多 profile 是独立前置架构决策,本 spec 不裹挟。
