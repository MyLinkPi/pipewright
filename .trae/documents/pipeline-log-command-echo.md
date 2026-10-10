# 流水线日志命令回显补全(所有执行的命令可见)

## 概要(Summary)

流水线运行日志此前存在「执行了命令但日志里看不到执行了什么」的缺口(典型:健康检查节点
未圈选目标机时的平台本机 HTTP 探测,只输出结果行「健康检查通过/失败」,没有任何探测方式信息)。
本次对**流水线执行全链路的命令执行点**做了一次系统审计,把所有「实际执行命令但未回流日志」的
位置补齐为统一口径的命令回显(`$ <命令>`),让运行日志像 Jenkins 控制台一样能回答
「这一步到底执行了什么命令」。

回显口径(全局一致,沿用 [cmdlog.go](file:///c:/Users/xufan/Trae/pipewright/internal/deploy/cmdlog.go) 既有规范):

- `$ <命令>`(stdout 流):命令本身;`sh -c <script>` 直接展示脚本本体;敏感参数就地打码
  (`-e K=V` → `K=***`)。
- `  ✗ <原因>` / `  ✗ 退出码 N`(stderr 流):执行失败与非零退出。
- 文本最终经 sink 侧 per-run Masker 兜底脱敏,绝无明文 secret 落库/出网。

## 现状分析(Current State):审计发现的缺口

流水线各执行链路的命令入口分两类:**已带日志的**(deploy 的 `s.exec`、build 的
`emitCmd`/`shellDriver`)与**未带日志的**(直连 `target.Service`、Go http 客户端、远程
`tgt.Exec`)。本次逐点核查后确认的缺口:

| # | 位置 | 缺口 | 场景 |
|---|------|------|------|
| 1 | [health_probe.go](file:///c:/Users/xufan/Trae/pipewright/internal/deploy/health_probe.go#L71) `probeLocalHTTP` | 平台本机 HTTP 探测**无任何命令/方式日志** | health_check 节点未圈选目标机(最常见:仅填 URL) |
| 2 | [host_load.go](file:///c:/Users/xufan/Trae/pipewright/internal/deploy/host_load.go#L199) `probeHostLoad` | 直连 `s.targets.Exec`,采集命令不回显 | deploy 节点配 `maxTargets` 时的负载选机 |
| 3 | [image_release.go](file:///c:/Users/xufan/Trae/pipewright/internal/deploy/image_release.go#L131) docker login | 直连 `s.targets.DockerLogin`,登录命令不回显 | 私有仓库镜像部署 |
| 4 | servicereg 全部 `tg.Exec` | 网关 nginx 编排命令(ensure/渲染/校验/热加载)不进运行日志 | 部署时的网关联动(摘挂/轮转/清理) |
| 5 | [remote_stage_exec.go](file:///c:/Users/xufan/Trae/pipewright/internal/build/remote_stage_exec.go#L601) 解包/清理 | 远程工作区 tar 解包、收尾 `rm -rf` 不回显 | 远程 runner 派发 |
| 6 | [remote.go](file:///c:/Users/xufan/Trae/pipewright/internal/build/remote.go#L81) `DetectRemoteCLI` | 远程容器 CLI 探测命令不回显 | 远程 runner 派发 |
| 7 | [builder.go](file:///c:/Users/xufan/Trae/pipewright/internal/build/builder.go#L435) `InspectImage` | `docker inspect` 元数据采集不回显 | 构建/推送后取 digest |
| 8 | [services.go](file:///c:/Users/xufan/Trae/pipewright/internal/build/services.go#L119) 旁挂服务清理 | `StopService`/`RemoveNetwork` 传 `nil` onLine,清理命令不回显 | 阶段旁挂服务拆除(正常/失败回滚) |

其余执行点(build 的 driver 构建/运行/推送/清理、deploy 的 s.exec 部署命令/健康门控/回滚、
发布上传提示行等)已具备回显,未改动。

## 修改方案(Proposed Changes)

### 1. 健康检查本机探测回显(`internal/deploy/health_probe.go`)

`probeLocalHTTP` 每次尝试前回显**等效命令**,失败时回显原因:

```
$ curl -fsS --max-time 5 http://example.com/healthz  (平台本机 HTTP 探测,不经 SSH)
  ✗ HTTP 500
```

- 本机探测实际走 Go http 客户端(不经 SSH),回显的是语义等效的 curl 命令 —— 与目标机路径
  `runHealthCheck` 经 `s.exec` 输出 `$ curl -fsS --max-time <T> <url>` 完全同形,让
  「用什么检查的」可见。
- 失败明细:`HTTP 4xx/5xx` 直接输出;连接/DNS 类错误截断后输出(防爆量)。

### 2. 负载选机探测回显(`internal/deploy/host_load.go`)

`probeHostLoad` 从 `s.targets.Exec` 改走 `s.exec`(命令 + 输出回流),并在
`limitServersByLoad` 的逐机探测处包 `scopeCmdLog(ctx, srv.Name)`,命令归属到被探测机器
(与部署命令同分组)。命令回显为采集脚本本体(`sh -c` 直展,含 `##PW:` 分段标记)。

### 3. docker login 回显(`internal/deploy/image_release.go`)

`stageImageOne` 在调 `s.targets.DockerLogin` 前先回显:

```
$ docker login registry.example.com --password-stdin  (凭据经 stdin 注入,不回显)
```

用户名/口令由 target 层从凭据解密后经 stdin 注入,**绝不回显**(原安全纪律不变)。

### 4. 网关联动命令回显(`internal/deploy/cmdlog.go` + `cmd/main.go`)

新增导出装饰器 `deploy.ObservingTarget(inner target.Service) target.Service`:

- 匿名嵌入包装 `target.Service`,仅覆盖 `Exec`:命令以 `$ ` 前缀回显、stdout/stderr 原样回流、
  非零退出码显式标注(与 `s.exec` 共用抽取出的 `execObserved`,日志格式全局一致);
- 未挂命令日志的 ctx = 纯透传(行为与裸 `target.Service` 逐字节一致);
- 机器归属沿用调用方 ctx 的单机作用域(部署路径 = 触发本动作的目标机;管理页调用 = 运行级)。

`main.go` 装配 servicereg 时改注入 `deploy.ObservingTarget(targetSvc)`:servicereg 的
全部网关 nginx 编排命令(容器 ensure / 配置渲染下发 / `nginx -t` 校验 / `-s reload` /
上游接网 / 证书下发)在部署链路内即经部署注入的日志回调进入步骤日志。
servicereg 包本身零改动(不 import deploy,包间单向依赖不变)。

### 5. 远程 runner 回显(`internal/build/remote_stage_exec.go`、`remote.go`)

- 远程解包命令(`sh -c mkdir -p … tar -xzf …`)与收尾清理命令(`rm -rf <remoteWS>`)在
  `tgt.Exec` 前经 `rep.Log` 回显;解包失败额外回显 stderr 首行摘要(截断 300 字节,
  新增 `truncateLine`/`remoteErrDetailLimit`)。
- 新增 `DetectRemoteCLIWithLog(ctx, execer, serverID, log)`:探测命令**仅在真正执行
  (未命中 5 分钟 TTL 缓存)时**回显,避免把未执行的命令写进日志误导排查;
  原 `DetectRemoteCLI` 签名与行为不变(委托实现,log=nil)。

### 6. 镜像元数据采集回显(`internal/build/builder.go`)

`b.driver.InspectImage` 在构建后 / 推送后两处调用点回显
`$ <bin> inspect --format {{json .}} <ref>`(Driver 接口无 onLine,故在调用点输出;
远程模式该命令同样经 SSH 执行,回显口径一致)。

### 7. 旁挂服务清理回显(`internal/build/services.go`)

`startStageServices` 失败回滚与 `stopStageServices` 拆除处,`StopService`/`RemoveNetwork`
的 `nil` onLine 改为 `mkLineLogger(ctx, rep)`,`docker rm -f`/`network rm` 清理命令可见。

## 测试

- `internal/deploy/rolling_binding_test.go`:`TestCheckHealthLocalProbeEchoesCurlCommand`
  (500 服务 → 日志含等效 curl 命令 + `✗ HTTP 500`)。
- `internal/deploy/host_load_test.go`:`TestProbeHostLoadStreamsCmdLog`
  (采集脚本本体回显 + 输出回流 + 机器归属)。
- `internal/deploy/cmdlog_test.go`:`TestObservingTargetStreamsCmdLog`
  (命令/输出/退出码回流;scoped 归属沿用;无日志 ctx 纯透传)。
- `internal/deploy/stage_image_test.go`:`TestStageImageEchoesDockerLogin`
  (registryCredentialId 配置 → `$ docker login …--password-stdin` 回显)。
- `internal/build/remote_test.go`:`TestDetectRemoteCLIWithLogEchoesProbe`
  (探测命令首次回显、缓存命中不重复回显);既有 TTL 用例适配新签名。
- `internal/build/remote_stage_exec_test.go`:`TestStageRemoteEchoesUnpackAndCleanupCommands`
  (远程解包 + 清理命令回显)。
- `internal/build/builder_test.go`:`TestInspectImageCommandEchoed`(inspect 命令回显)。

## 风险与边界

- **无控制流变化**:所有改动只增加日志输出;`s.exec`/`execObserved`/装饰器均为原执行体
  的薄包裹,返回值与语义逐字节一致;无序性/并发语义不变。
- **脱敏不受影响**:命令展示复用 `displayCmd`(第一道打码),最终仍经 sink 侧 Masker 兜底;
  新增回显均不含明文 secret(login 仅回显命令形态,凭据仍走 stdin)。
- **不改线协议**:日志仍是 run_logs 的 stream(text)两元组,machine 归属机制不变。
- **servicereg 无侵入**:仅 main 装配处包一层 target.Service;servicereg 的包依赖、
  httpapi 网关管理页行为(无日志 ctx → 纯透传)不受影响。