# FR-8-19 多节点构建机池 + 标签调度 — 测试计划

FR-8-14(单机远程构建)的续篇:构建机从"项目绑定一台服务器"升级为"打标签的服务器池",
按 标签 → 优先级 → 流水线亲和 → 负载 调度;stage 可覆盖项目默认选择器。

## 覆盖矩阵(已落地的自动化用例)

### 选择器语法与匹配(internal/runner/selector_test.go)
- [x] 空串 = 本地(非选择器);`server:<id>` 钉死形式由 PinnedServer 单独识别
- [x] 标签项解析:trim/去重/保序;k=v 项;项数 ≤16、总长 ≤255;空项/非法字符 → ErrInvalidSelector
- [x] 匹配:AND 语义;纯 tag 不匹配 k=v;空标签集永不命中;空选择器不算匹配

### 调度器(internal/runner/scheduler_test.go)
- [x] 优先级:高者先;优先级胜过亲和
- [x] 亲和:同优先级复用本流水线最近使用机;亲和机忙 → 退让同优先级空闲机,不空等;跨流水线不串扰
- [x] 负载:同优先级无亲和时选 in-flight 最少机(最少负载兜底)
- [x] 槽位:max_builds=0 → 全局默认(默认 1)串行;多槽并发不超卖(12 goroutine × 3 槽压力)
- [x] 等待:全忙 FIFO 阻塞 → release 唤醒;ctx 显式取消/超时中断;放弃的等待者不饿死后继
- [x] 竞态回归:release 与「抢槽失败 → 入队」竞态不丢唤醒(100 轮压力)
- [x] 健康:可达缓存 5min(一次 Acquire 只拨号一次);不可达负缓存 30s 内不反复锤;ctx 取消造成的探测失败不写负缓存;全不可达 → ErrNoRunnerAvailable;无命中 → ErrNoRunnerMatch
- [x] release 幂等(重复调用不产生负计数/二次唤醒)

### 配置存取(internal/runner/runner_test.go)
- [x] SaveSelector 往返;旧入口 Save → `server:<id>`;RunnerFor 仅钉死形式有效
- [x] 校验:非法语法 → ErrInvalidSelector;钉死不存在机 → ErrServerNotFound;清空回本地
- [x] 迁移回填(0052):旧 runner_server_id 行 → selector(双方言重放验证)

### 派发(internal/build/remote_stage_exec_test.go)
- [x] stage.Runner 覆盖项目默认;无覆盖用项目默认;都空走本地
- [x] acquire → 执行 → defer release(成功/失败路径都归还)
- [x] 调度失败(无命中)→ 阶段失败 ErrBuildFailed + 日志明示,不触本地/远程;取消/等槽超时 → run.ErrCanceled
- [x] 远程 CLI 探测(internal/build/remote_test.go):nerdctl > docker > podman 顺序;成功结果 TTL 缓存(5min)、过期重探;失败回落 docker 且不缓存
- [x] 瘦控制机(internal/build/builder_test.go):errDriver 下 Builder 可构造,本地容器操作诚实报 ErrNoContainerCLI

### 流水线 schema(internal/pipeline、internal/pipelineyaml)
- [x] Stage.Runner 经 normalizeSpec 保留(字段字面量已同步);非法语法 → ErrInvalidStage(internal/pipeline/stage_runner_test.go 直测)
- [x] `.pipewright.yml` runner 字段双向往返不丢;非法值被拒

### HTTP API(internal/httpapi/servers_test.go、runner_test.go)
- [x] servers labels/maxBuilds/priority 写读往返;非法标签/越界 → 400
- [x] runner PUT selector 往返;非法选择器 → 422 invalid_runner_selector;钉死不存在机 → 422 runner_server_not_found;项目不存在 → 404;旧 runnerServerId 入参兼容

## 手工验证清单(需真机 SSH)
- [ ] 两台打标签构建机,项目配 `linux` 选择器:连续触发多个 run,观察阶段日志「→ 构建机:X(选择器 …)」
      按优先级/亲和分布,且每机同时构建数 ≤ 槽位
- [ ] 全忙时新 run 阶段日志出现「构建机池全忙(…),排队等待槽位…」,释放后自动继续
- [ ] 拔掉一台机的 SSH:调度自动避开(探测缓存 30s 内重试),全挂时阶段失败并明示
- [ ] RunnerPanel 标签模式:输入选择器实时显示「命中 N 台 — 机器名」;StageDrawer 阶段覆盖生效
