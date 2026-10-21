# AI 测试节点(ai_test):AI 直接执行测试 — 设计

> 状态:**P1 已实现 + P2 核心已实现 + P3 大部已实现**(后端 `go test ./...` 与前端单测全绿;2026-10-21)。落点:`internal/pipeline/ai_node.go`(校验)、`internal/ai/agenttest.go`(AgentTest 领域服务:发现/needs 排序/agent 循环/submit_verdict 裁决契约/用量与网关费用)、`internal/build/ai_node.go`(执行器 + HTTP/浏览器动作集 + Playwright 容器桥)、`internal/notify`(ai_acceptance_failed 事件 + testSummary/failedTests/reportUrl 占位)、`internal/httpapi/notify_hook.go`(审批卡内嵌测试摘要)、`Resource` init/shmSize(`settings.go`+`shell_driver.go`+`workspace_copy.go`)、前端 `jobConfigSchema.ts`/`JobTypeIcon.vue`/8 语言。
> 已知缺口(诚实边界):`ai_acceptance_failed` 已注册但无发射点(需 build 侧持路由器;接线点 = runAITestJob 汇总后);`{{failedTests}}` 暂无数据源(报告存储只有计数,扩用例明细属独立增量);截图**多模态观察**(P4)未做——截图留档已随浏览器动作集落地;run-scoped AI 代理端点(P4)未做;浏览器桥协议与工具面有单测,未经真实容器 e2e。
> 本版纠正核心定位——AI 是**测试的执行者**,不是测试脚本的作者;早期"AI 生成脚本 + 确定性回放"方案已废弃。相关:`internal/ai`(AI 服务、`AgentRound` 工具调用循环,密钥仅存 vault)、`internal/build/ecs_power.go`(控制面节点执行器范本)、`internal/build/shell_driver.go`(工具链容器启动参数)、`internal/build/dag_test_report.go`(测试报告落库/门禁)、`internal/notify/router.go`(事件路由)、`web/src/components/pipeline/jobConfigSchema.ts`(节点 schema)。

## 定位:AI 测试,不是 AI 写测试

**AI 读需求、亲手操作被测系统、下裁决**。人写的唯一东西是测试需求(`.md`,提交进仓库);run 中 ai_test 节点为每条需求启动一个 agent 会话:理解需求 → 驱动真实浏览器(前端)或发 HTTP 请求(接口)按需求操作系统 → 观察实际结果 → 输出结构化裁决。**仓库里没有也不需要 `.spec.ts` 之类的测试脚本**;Playwright 在本体系中的角色是 **agent 的「手」**——浏览器动作执行层,不是测试资产格式。

- 需求(`.md`,人写)定义**什么算对**;
- AI agent 在运行时**直接执行验证**(理解 → 操作 → 观察 → 裁决);
- 裁决 → 同构测试报告(TestReportPanel)+ 通知;**裁决一律仅供参考,放行决策永远是人**(审批门,见「结果呈现与通知」)。

(可选共存:常规确定性测试套件仍可用既有 script 任务执行——零新代码、与本特性无关,不在此展开。)

## 仓库约定

需求是唯一入库的测试资产(与功能代码同 PR、同评审;平台不写仓库):

```
.pipewright/
  tests/
    requirements/          # 测试需求(*.md)——ai_test 的输入,人写
      web/                 #   按被测面分子目录(仅惯例,不强制)
        checkout.md
      api/
        coupon.md
```

- **需求文件 = Markdown + YAML frontmatter**(整体可省,缺省 id=文件路径 slug、severity=blocker、target=继承节点):

```markdown
---
id: checkout-flow
severity: blocker        # 呈现权重(缺省 blocker):blocker 失败 = 红色高亮并置顶审批卡;suggest 失败 = 黄色参考
target: ${BASE_URL}      # 可选;缺省继承节点 target;${VAR} 引用运行参数/构建变量/DEPLOY_HOST
---
登录测试账号,加购任一商品并用优惠券结算。
期望:有效券码应用后订单金额立减;无效券码提示「券码无效」且金额不变。
```

- 目标解析:**需求 frontmatter `target` > 节点 `target`**,两级皆空 → 发现期判 fail(无被测对象)。
- 冒烟/全量分层:子目录 + 节点 glob 过滤,不另造标签机制。
- **依赖与顺序**:生命周期成对的场景(创建→删除)**优先写成一条自包含需求**(创建→验证→删除→验证,测试数据生命周期自包含);确需拆分才用 frontmatter `needs`(如 `delete-resource.md` 声明 `needs: [create-resource]`);执行语义见「执行顺序与依赖」。前置动作(登录等)直接写进需求正文,由 agent 自理,不算依赖。

## ai_test 节点:agent 执行模型

分类 quality,控制面执行,逐需求一个独立 agent 会话:

1. **发现即校验**:约定路径不存在 / frontmatter 解析失败 / id 重复 → fail fast(错误带文件名);发现 0 个需求 → 判 fail(防路径 typo 静默绿灯);日志首行回显「发现 N 个需求(blocker X / suggest Y)+ 执行序」。
2. **执行顺序与依赖**:发现后按 frontmatter `needs`(依赖的需求 id 列表)构图,**Kahn 拓扑排序**——复用 `internal/dag`,与流水线 job 的 `needs` 同款语义,零学习成本;**成环 → 发现期 fail fast**;无依赖需求按路径字典序。**同一浏览器上下文顺序执行**:cookie/localStorage 跨需求保留(第一条需求登录后,后续需求中 agent 观察到已登录即径直继续,重复前置动作的成本自动消失)。**依赖失败 → 依赖它的需求标 ⏭ skipped**(不算失败,报告注明级联原因;不隐藏级联,也不放大成失败——最终人判)。
3. **agent 循环**:LLM 收到「需求正文 + 当前观察」→ 决定下一个动作 → 执行 → 新观察回传 → 循环直至产出裁决或预算耗尽。复用 `internal/ai` 的 AgentRound;temperature 0。会话上下文沿用平台的**自动压缩与溢出兜底**(逼近窗口自动摘要压缩、`context_window_exceeded` 兜底重试),窗口大小取节点 `modelContextTokens`(缺省 = 平台默认窗口)。**每会话累计 AI 用量与费用**(模型 / LLM 轮次 / 输入输出 token / 时长 / 实际步数 / 花费):provider 响应自带的 usage 优先,缺失时按字符量估算并如实标注「估算」;**费用取自定网关自报**——流式末 chunk 的 `x_new_api_consumed_amount` 字段或非流式 `X-New-Api-Consumed-Amount` 响应头,逐调用求和、原样记录不做换算;网关未回报(直连 OpenAI/Claude/Ollama)则费用栏缺省,退化为纯 token 展示,不编造。动作集:
   - **前端**:`goto / click / fill / select / hover / 读取可见文本(accessibility 摘要)/ 截图`(截图观察需多模态,P2);
   - **接口**:HTTP 请求(方法/URL/头/体)/ 读取响应(状态/头/截断体)。
4. **裁决契约**:严格 JSON `{verdict: pass|fail, reason, evidence}`;evidence 必须引用**实际观察到的内容**(页面文本/响应字段),不得只复述需求;解析失败重试一次,仍失败判 fail;**步数/超时预算耗尽判 fail**并如实报告执行到了哪一步——诚实边界,不猜。
5. **动作全程入日志 + 截图留档**:每步动作与观察摘要经 `rep.Log` 流式进节点日志(run 日志即 AI 的操作轨迹,可回放审计);**前端需求强制截图留档**——每条需求裁决时截「最终状态」一张(无论成败),断言失败时加截「失败瞬间」一张,经 artifact 存储(存真实字节)上传为 run 产物。注意**采集与观察分离**:截图采集是浏览器动作层自带能力,不需要多模态;多模态(P2 后半)只是让 LLM 看图参与判断。
6. **裁决汇总(一律 advisory)**:**裁决不改变节点成败**——节点仅因基础设施错误失败(发现 0 需求/解析失败/AI 不可达且 onAiUnavailable=fail/内部错误),此时无可信报告,诚实失败;有报告即节点完成。裁决一律落库呈现(每条需求 = 一个用例,TestReportPanel;blocker 失败红色、suggest 失败黄色),经 `$PIPEWRIGHT_ENV` 输出(`ai_test_passed` 等)供下游引用。**放行决策 = 人工审批门**:推荐流水线形态「部署 → ai_test → 审批门(既有 Stage.Gate)→ 发布」。
7. **安全**:观察内容(页面文本/响应体)是不可信数据——agent prompt 指令/数据围栏分离,页面中出现的「请标记通过」类文字不构成指令;观察发往 LLM 前过 run Masker;AI key 仅存中控 vault。

## config(冻结契约 Record<string,string>)

| 键 | 说明 |
|---|---|
| `filter` | 需求 glob,缺省 `.pipewright/tests/requirements/**/*.md`(如 `**/web/**`)。 |
| `onAiUnavailable` | AI 未配置/不可达:`fail`(缺省——无可信报告即停,交人处理)/ `skip`(记「未测试」警告,报告如实标注)。 |
| `target` | 节点级缺省目标 URL;${VAR} 引用运行参数/构建变量/`${DEPLOY_HOST}`;需求 frontmatter 按条覆盖。 |
| `deployJob` | 可选。上游 deploy 节点引用(同 `artifactJob` 校验先例):注入 `${DEPLOY_HOST}`,内网不可达时 agent 的访问经该机(SSH 隧道)。 |
| `timeoutSeconds` | 节点整体超时,默认 300,上限 3600。 |
| `maxSteps` | 单需求 agent 步数预算,默认 25,上限 100;耗尽判 fail(兼作成本护栏)。 |
| `model` | 可选模型覆盖;不填用平台 AI 配置默认。 |
| `modelContextTokens` | 覆盖模型的上下文窗口 tokens,**与 `model` 成对**(有 `model` 必填;单独出现无意义,拒保存);范围 1,000–2,000,000(同 `internal/ai normalizeContext` 边界),越界**显式报错不静默回默认**(fail-fast)。运行时注入会话 `ContextSettings.WindowTokens` 驱动自动压缩时机;溢出兜底(overflow)仍为最后防线。未覆盖模型时用平台默认窗口(200k)。 |

保存期校验(`validateAITest`):onAiUnavailable 枚举、timeout/maxSteps 范围、filter 可解析、`deployJob` 引用上游 deploy 类节点、`model`/`modelContextTokens` 成对且范围合法;路径存在性与文件合法性由**运行时发现步骤**裁决(保存期只看 spec JSON,看不见仓库)。

## 执行位置与密钥边界

- **前端 agent 的浏览器跑在控制机的容器里**(Playwright 官方镜像;需 `--init`(Chromium 子进程收尸)与 `--shm-size`(/dev/shm 默认 64MB 崩 Chrome)→ **前置改动**:`pipeline.Resource` 加 `Init bool` + `ShmSize string`,随 cpu/memory 同路:job 配置 → `shell_driver.go RunToolchain` → 远程透传;不做任意 docker 参数透传(防注入基线))。LLM 循环在控制面,经本地有限动作接口(HTTP/WS)驱动 agent 容器——实现期二选一:chromedp 直驱容器内浏览器,或容器内 sidecar 暴露动作集。
- **接口验收(P1)无需浏览器容器**:控制面直接发 HTTP 请求。
- 远程构建机场景:需求目录(纯文本,KB 级)经 SSH 取回;被测系统可达性与部署路径一致(部署由中控发起),内网不可达走 `deployJob`。
- **AI key 永远留中控 vault,绝不下发任何容器**;构建容器内直接调 AI 的 run-scoped 代理端点(`/api/runner/ai/proxy` + 每-run 限额)仍为后续可选。
- **平台不做 git 编辑器**:不 push 分支、不开 PR;仓库只进人写的需求。

## 结果呈现与通知

- **报告**:ai_test 裁决直写 run 测试报告存储(同 `dag_test_report.go` 落库路径,不经工作区文件),TestReportPanel 与 JUnit 报告同屏(每条需求 = 一个用例,reason + evidence 进失败明细,**evidence 附截图缩略图、点击放大——图片即 artifact,复用现有存储,无需另建**;**每行带 AI 用量与费用**(模型/轮次/输入输出 token/时长/步数/花费,紧凑格式如 `9 轮 · 6.2k/0.8k tok · 花费 0.0221`),表下节点用量汇总一行);动作轨迹在节点日志(SSE 实时 + 历史回放);会话截图/动作序列为 run 产物可下载;**skipped(⏭)需求行注明「依赖 X 失败/未执行」**,级联可见。**通知卡片**在支持图片的渠道(飞书/企微富卡片)内嵌失败截图。
- **放行 = 人工审批门(既有 Stage.Gate + approval_required 事件)**:推荐流水线「部署 → ai_test → 审批 → 发布」;审批通知卡片(飞书/企微富卡片,既有签名链接按钮)与审批页**内嵌测试摘要**——`{{testSummary}}` / `{{failedTests}}`(blocker 失败红色置顶、suggest 失败黄色)+ 失败截图 + `{{reportUrl}}`,人看证据放行/驳回;驳回 → run 失败 → 既有 `build_failed` 照发。TemplateVars(冻结契约,只增不改)补上述占位;`RenderText` 供流水线内 notify 节点复用。
- **参考性通知**:新增 `ai_acceptance_failed`(先例:节点级事件如 health_check_failed)——**不设审批门**的流水线(夜间 cron 全量)里出现失败时的黄色提醒,不阻塞任何事。
- job Summary 一行结论(N 通过/M 失败;总用量:R 轮 / 输入 X / 输出 Y tok / 花费 Z)。

## 示例:日常工作流(以「下单支持优惠券」为例)

1. **写需求(人唯一要"写"的东西)**:新建 `.pipewright/tests/requirements/web/checkout.md`,正文一段自然语言(如上例),与功能代码同一分支、同一 PR(frontmatter 可省)。
2. **PR 触发 run**(运行参数 `BASE_URL`=staging):部署后 ai_test 逐条执行——agent 打开登录页、登录测试账号、加购、输入券码、读取金额……**每步动作流式进节点日志**;「有效券立减金额」pass(evidence:金额由 ¥95.00 变为 ¥89.00);「无效券提示」fail(evidence:提交 INVALID 后页面无任何反馈,未见「券码无效」文案)。
3. **run 到审批门暂停**:审批卡片/审批页内嵌测试摘要——「✅ 有效券立减金额(¥95.00→¥89.00)」「⚠️ 无效券提示(未见文案)」+ 两张截图 + 用量;人点开证据,**确认放行或驳回**(驳回 → run 失败 → 修复重跑)。
4. **发布日**同一条流水线,`BASE_URL`=prod + `filter` 冒烟子集,同样人工放行;夜间 cron 全量**不设审批门**,失败走 `ai_acceptance_failed` 黄色提醒,不阻塞。

## 示例:测试报告

TestReportPanel(ai-acceptance):

| 需求 | severity | 裁决 | evidence(节选) | 截图 | 用量 |
|---|---|---|---|---|---|
| web/checkout › 有效券码立减金额 | blocker | ✅ pass | 应用券码 COUPON-1 后,订单合计由 ¥95.00 变为 ¥89.00,与券面立减 ¥6.00 一致 | 📷 checkout-pass.png(裁决时最终状态:合计 ¥89.00) | 9 轮 · 6.2k/0.8k tok · 花费 0.0221 |
| web/checkout › 无效券码提示 | suggest | ❌ fail | 提交 INVALID 后页面无任何反馈,未见「券码无效」文案 | 📷 checkout-fail.png(失败瞬间:无任何提示的结算页) | 5 轮 · 4.1k/0.6k tok · 花费 0.0125 |

节点用量汇总:14 轮 LLM 调用 · 输入 12.6k / 输出 1.9k tokens · 花费 0.0346(网关计价) · 模型 claude-sonnet-4 · 耗时 3m12s(步数预算 25,实际峰值 11)。

节点日志(动作轨迹,节选):

```
[ai_test] 发现 1 个需求(blocker 1 / suggest 1)
[agent] goto http://staging.example.com/login
[agent] fill #username=tester → click「登录」
[agent] goto /cart → click「加购」→ goto /checkout
[agent] fill 优惠券=COUPON-1 → click「应用」→ observe: 合计 ¥89.00
[agent] capture 📷 checkout-pass.png → verdict: pass (evidence: 95.00→89.00)
[agent] fill 优惠券=INVALID → click「应用」→ observe: 无反馈
[agent] capture 📷 checkout-fail.png → verdict: fail (evidence: 未出现「券码无效」)
[ai_test] 用量: 14 轮 · 输入 12.6k / 输出 1.9k tok · 花费 0.0346 · claude-sonnet-4 · 3m12s
```

审批门卡片(approval_required 增强:内嵌测试摘要,人在通知里看证据放行/驳回):

> ⏸ **等待审批 · 发布前测试验收** · shop-web #128
> AI 验收:2 条 — blocker 1/1 通过;建议 1 失败 · 14 轮 · 12.6k/1.9k tok · 花费 0.0346
> ✅ 有效券码立减金额 — ¥95.00→¥89.00
> ⚠️ 无效券码提示 — 未见「券码无效」文案 📷
> [查看报告] [✓ 放行发布] [✗ 驳回]

## 落地顺序

1. **P1 HTTP agent(接口验收)**:节点注册(后端校验/执行器/分发 + 前端 schema/图标/8 语言)、发现与校验、AgentRound 循环(纯 HTTP 动作集)、裁决同构落库 + TestReportPanel;`Resource` init/shmSize(为 P2 预置)。
2. **P2 浏览器 agent(前端验收)**:agent 容器 + 动作桥(chromedp 或 sidecar,实现期定);DOM 文本观察先行,截图观察(多模态,`chatWithTokens` 图片 content)随后;**截图证据留档随浏览器动作层即可用**(采集与观察分离),报告缩略图同步落地。
3. **P3 审批集成与通知**:审批卡片/审批页内嵌测试摘要与截图(approval_required 载荷增强)+ TemplateVars 增量 + `ai_acceptance_failed`;`deployJob` 绑定(SSH vantage)。
4. **P4 增强**:run-scoped AI 代理端点;对话式迭代需求草稿(opschat,仍走人写文件入库);确定性套件共存注记文档化。

## 与现有智能体(opschat AgentRound)的复用边界

**直接复用(`internal/ai` 现成,零改动)**:

- 双 provider 原生工具调用协议:请求构造(Claude `messages`+`tools` / OpenAI `chat/completions`+`tool_choice`)、消息构造(含 Claude 硬约束:首条 user、角色交替、content 非空、相邻同角色合并);
- SSE 流式消费 `consumeAgentStream`(wire/单帧/帧数限额、UTF-8 校验、finish_reason 白名单、incomplete 检测、tool_calls 按 index 合并);
- 安全 transport:`opsResolve` IP-pin、禁代理、禁重定向、TLS≥1.2 + ServerName 钉扎;
- usage 解析(`AgentUsage`,流式末帧尽力解析);
- 脱敏与限额(`safeText`、请求 256KB/响应 32KB、工具调用 ≤24 每轮)、provider 错误人类可读化(不泄 key);
- `wrapUpPrompt` 收尾轮:预算耗尽让模型诚实总结进度——与「预算耗尽判 fail 并如实报告走到哪一步」天然契合;
- 上下文管理:`ContextSettings` 归一/`Resolved`、自动摘要压缩(summarize)、溢出兜底(overflow);`modelContextTokens` 的注入点即 `WindowTokens`。

**复用框架、替换内容**:ops 工具集(host_resources / docker_* / systemd_* / host_exec)全部不用,换成 `browser_*`(goto/click/fill/读可见文本/截图)与 `http_*`(请求/读响应)动作集;工具 schema 注入框架照用,写操作(表单提交、POST/PUT/DELETE)复用 ops 变更工具的 `purpose`/`reason` 必填注入模式(`agentSchema` mut 分支),日志留痕。

**刻意不复用**:opschat 确认单(`ErrConsent`/调用绑定批准)——流水线中无人在线应答,写操作风险控制由「动作集白名单 + purpose 注入 + 全程日志 + 事后人工审批门」承担;opschat 会话与 history 持久化——ai_test 会话生命周期 = 单条需求,run 内即弃(裁决与轨迹已落库)。

**需小幅扩展**:usage 解析补网关费用字段(流式末 chunk `x_new_api_consumed_amount` / 非流式 `X-New-Api-Consumed-Amount` 响应头);工程上应把 `AgentRound` 中「协议构造 + 流消费 + 安全 transport」**下沉为共享组件**(ops 调用方行为不变),避免 ai_test 依赖 opschat 语义(`AgentRequest` 类型、确认单)。

## 实现位置(预期)

- `internal/pipeline/ai_node.go`:`JobTypeAITest = "ai_test"` + `validateAITest` 挂 normalizeSpec 末尾(仿 `internal/pipeline/ecs_power.go`)。
- `internal/build/ai_node.go`:`isAITestJob` 谓词 + `runAITestJob`(窄接口 `stageAITester`,不直接依赖 internal/ai 包;**needs 拓扑排序复用 `internal/dag`(Kahn)**);分发:dag_stage_exec 三处(类型分桶/串行循环/job 级 DAG switch)+ remote_stage_exec 控制面桶;`build.WithAITester` 注入(nil → 记「未注入,跳过」,同 ecs_power/oss_upload 兼容款)。
- `internal/ai`:`AgentTest`(复用 AgentRound / chatWithTokens 的流式、限长、SSRF 钉扎;动作集接口化:HTTP 集 P1、浏览器集 P2;**chatWithTokens/AgentRound 透出 provider usage 与网关费用**——流式末 chunk `x_new_api_consumed_amount` / 非流式响应头 `X-New-Api-Consumed-Amount`,缺失时字符估算并标注);`ErrAINotConfigured` → 按 onAiUnavailable 裁决。
- agent 容器编排:Playwright 镜像 + Resource 扩展(`shell_driver.go` + 远程透传)。
- 报告:裁决直写 `internal/run` 测试报告存储(**记录含 AI 用量字段**,JSON 承载,无重迁移);通知:approval_required 载荷增强(测试摘要/截图)、`internal/notify/router.go` 注册 `ai_acceptance_failed`、`template.go` 占位增量。
- 前端:`jobConfigSchema.ts`(category quality;可选 FieldKind `aiModel`,数据源 `/api/settings/ai/models`)+ `JobTypeIcon.vue` + 8 语言 `pipelineJob.ts`(zh-CN 先定稿);无 DB 迁移。

## 边界与取舍

- **机器不放行(设计原则)**:AI 裁决一律 advisory,不提供「AI 直接阻塞/放行」选项;流水线的闸门只有两种——人工审批门(推荐,放行 = 人点按钮,既有 Stage.Gate,零新闸门语义,全程审计)或不设闸(纯参考 + 通知)。裁决的非确定性因此不再传导为放行风险——**波动只影响人的注意力,最终由人消化,这正是「最终人判」的意义**(temperature 0 + 步数预算 + evidence 引用实际观察仍用于把噪声压到可读)。
- **成本显式化**:每 run 每需求一次 agent 会话(多轮 LLM + 浏览器操作),分钟级;maxSteps/timeout 即成本护栏;推荐节奏:发布冒烟 + 夜间全量。
- **会产生数据的操作**(下单/写库):agent 不自动清理——需求应自备测试数据并说明清理方式,平台不代清理(文档提示);只读操作优先。
- **0 需求/空发现即失败**;advisory 失败不阻塞但必须可见(报告 + 通知)。
- **AI 配置平台级单 profile**:节点仅可覆盖模型名;多供应商 profile 是独立前置决策。
- **费用原样透传,不做换算**:平台不维护价格表;自定网关回报的费用(`x_new_api_consumed_amount` / `X-New-Api-Consumed-Amount`)逐调用求和、原样入报告,不换算单位、不造数;直连 provider 无此字段时退化为 token-only 展示。计价单位标签(USD/CNY 等)可作 AI 设置里的纯展示项;跨功能(诊断/生成等)的平台级费用统计复用同一采集管道,另行立项。
- **不做跨需求数据传递**:`needs` 只管顺序与跳过;数据交接靠需求文本自描述(命名约定、「删除任一 X 资源」措辞)或合并为一条完整生命周期需求;并行执行(多浏览器上下文独立会话)留后续,首版顺序执行。
- **压缩策略不在节点逐项开配**:节点只声明窗口(`modelContextTokens`);AutoCompact / KeepRecentTokens / MaxOutputTokens 等继承平台默认(`internal/ai ContextSettings.Resolved`),防配置面膨胀;真有逐节点诉求再议。
- **平台不做 git 编辑器**:克隆凭据仅用于拉取;仓库变更只有人提交一条路。
