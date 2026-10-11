# 本机(控制节点)状态卡牌:服务器状态页的本机指标

## 概要(Summary)

[ServerStatus.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/ServerStatus.vue)(`/server-status`)
的卡片网格**置首**新增一张**「本机(控制节点)」**卡牌,展示**运行 Pipewright 的这台主机自身**
的 CPU 负载 / 内存 / 磁盘 —— 无需登记为服务器、不占用 SSH 连接,直接在控制节点本地采集。

采集复用**与远程完全相同**的静态脚本([server_metrics.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/server_metrics.go)
的 `metricsCollectArgs` 与分段解析器),唯一差别是执行方式:远程经 SSH,本机经 `os/exec` 跑
`sh -c`。故指标口径与容错规则(某段缺失 → 该维度 null)和远程逐字一致。

范围边界(有意不做):

- **只读**。本机卡不参与「批量执行命令」勾选(它不是登记服务器),也没有单机 AI 助手入口。
- 不落库、不进异常检测规则与历史采样(`metric_samples` / 采样器不变)。
- 不采集 GPU(`nvtop`)与物理内存(`dmidecode`):本机没有「GPU 机型」这类登记开关,
  只取 CPU/内存/磁盘的最小可用子集(`physicalTotalBytes = 0`,前端已按此降级)。
- 不新增 TTL 快照缓存 —— 本地 `exec` 廉价(毫秒级),每次请求直采即可。

## 现状分析(Current State)

- 采集与展示:[server_metrics.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/server_metrics.go)
  经 `svc.Exec`(SSH)跑合并脚本;卡片由
  [ServerMetricsCard.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/components/ops/ServerMetricsCard.vue)
  渲染,前端 12s 轮询 `/api/servers/metrics`。
- 「本机/控制节点」概念在本仓已有先例:`internal/platformhttps` 的 `localHost`(基于 `os/exec`,
  不经 shell)负责平台自身主机的命令执行;acme.sh 签发引擎同样运行在控制节点。
- 路由:同前缀已有 `/api/system/config`([router.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/router.go)),
  故本机指标挂在 `/api/system/metrics`,语义清晰、与 `/servers/*`(登记服务器)分离。

## 修改方案(Proposed Changes)

### 1. 后端采集(本地执行同款脚本)

- `const localHostServerID = "__local__"`:本机 DTO 的固定 `serverId`(前端据此识别,不与真实 id 冲突)。
- `var localMetricsRun = runLocalMetricCmd`:可注入的本地执行器(单测注入 fake,不依赖真 `sh`)。
- `runLocalMetricCmd`:`os/exec` 跑 `sh -c <静态脚本>`,语义与远程 `runMetricCmd` 对齐 —— 命令
  **跑完但非零退出不算失败**(交解析层降级),仅「无法启动」(如无 `sh`)才是错误;超时/取消额外
  返回 `ctx.Err()`;stdout 同样按 `metricsOutMax` 截断。
- `collectLocalMetrics`:`metricsCollectArgs(false, false)`(不带 dmidecode / nvtop),10s 超时,
  逐段解析出 `CPU/Memory/Disk`;**永不返回 error** —— 失败落到 `reachable:false` + 人读 `error`。
- `humanLocalMetricsError`:超时 → 「本机采集超时」;`exec.ErrNotFound` → 「本机不可用:缺少 sh
  命令(仅支持类 Unix 控制节点)」;其余 → 「本机采集失败:无法执行采集命令」。

### 2. 路由与 handler

`GET /api/system/metrics`(认证、只读、豁免 CSRF):`makeLocalMetricsHandler()` 直接返回
`collectLocalMetrics(r.Context())`,采集失败仍是 **200 + reachable:false**(不 500,与远程一致)。
AC-SEC-02:脚本是纯静态文本,不接受任何用户输入 —— 无注入面。

### 3. 前端 API

[servers.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/api/servers.ts) 新增
`LOCAL_HOST_ID = '__local__'` 与 `getLocalHostMetrics(): Promise<ServerMetrics>`(复用同一
`ServerMetrics` 契约)。

### 4. 视图

[ServerStatus.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/ServerStatus.vue):

- `load()` 三路并行:登记列表 + 批量指标 + 本机指标;本机请求用 `.catch(() => null)` **容忍失败**
  —— 只让本机卡消失,绝不拖垮已登记服务器的展示。
- 网格**首位**渲染本机卡(`name = t('serverStatus.localName')`,`selectable` 缺省 = 不可勾选);
  本机 `reachable:false` 时按既有规则灰显 + 显示人读错误。
- 空态与骨架条件并入本机:两者皆无才显示空态/骨架;仅有本机数据时不再显示「尚无已登记服务器」。

### 5. i18n

8 语言 `serverStatus.ts` 新增 `localName`(如 zh-CN「本机(控制节点)」/ en「This host (control node)」),
`keyParity.test.ts` 保证键对齐。

## 测试

- Go [server_metrics_test.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/server_metrics_test.go):
  `TestCollectLocalMetrics`(注入 fake → 逐段解析正确、脚本为 `sh -c` 且不含 dmidecode/nvtop、
  不采 GPU/物理内存)、`TestCollectLocalMetricsUnavailable`(启动失败 → reachable:false + 人读错误、
  各指标 null)、`TestHumanLocalMetricsError`(三种失败文案)、`TestLocalMetricsEndpoint`
  (认证 200 契约 + 未认证 401)。
- 前端 [ServerStatus.local.test.ts](file:///c:/Users/xufan/Trae/pipewright/web/src/views/ServerStatus.local.test.ts):
  本机卡置首且只读(无复选框)、本机不可达时显示人读错误但不影响服务器卡、本机请求失败仅隐藏本机卡、
  无登记服务器时以本机卡替代空态。
- e2e:共享 stub 增加 `/api/system/metrics` 样例(裸对象,非 `{ items }` 信封),
  `ops-chat.spec.ts` 的 server-entry 布局用例断言更新为「本机卡 + 2 服务器卡」,且仅服务器卡有动作按钮。

## 已知限制

- 控制节点须为类 Unix(`sh` 可用)。生产形态满足(acme.sh / 宿主 nginx 均要求 `sh`);Windows
  控制节点会显示一张「不可用」灰卡 + 提示,不影响其它卡片。
- 卡牌标题固定为「本机(控制节点)」,不展示主机名(`hostname` 未纳入采集脚本)。
- 不做缓存:前端 12s 轮询会每拍在本地执行一次采集脚本(开销可忽略,且不落库)。
