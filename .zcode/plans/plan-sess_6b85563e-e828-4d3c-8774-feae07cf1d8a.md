# /api/servers/metrics 提速:合并 SSH 采集 + TTL 快照缓存 + 收紧超时

## 现状与根因(已确认)
- 每次 HTTP 请求逐台现场 SSH 采集,无指标缓存(`internal/httpapi/server_metrics.go:539`)
- 单台一次采集串行跑 ~5 条命令,每条命令都是一次完整 TCP+SSH 握手(`internal/target/ssh.go:31`,无连接复用)
- 整批延迟 = 最慢一台(单台超时 15s,`server_metrics.go:32`)
- `collectServerMetrics` 共 4 个消费方:两个 metrics handler、异常检测采集器(`anomaly.go:119`)、main.go 60s 历史采样器(经 `metricsCollector.Collect`)——都重复打 SSH

## 改动方案

### 1. 合并采集命令(1 台 = 1 次 SSH 连接)
`internal/httpapi/server_metrics.go`:
- 新增 `metricsCollectArgs(probePhys bool) []string`,返回 `{"sh","-c", 静态脚本}`。脚本纯静态白名单(无用户输入,延续 AC-SEC-02;`sh -c` 组合命令已有先例:`target.go:594` Upload、`build/remote.go:96`),用标记行分段输出:
  - `##PW:L` → `cat /proc/loadavg 2>/dev/null || uptime`(macOS 回退收进脚本)
  - `##PW:C` → `nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null`
  - `##PW:M` → `free -b 2>/dev/null`
  - `##PW:D1` → `df -B1 / 2>/dev/null`;`##PW:D2` → `df -k / 2>/dev/null`(双段避免 KiB/字节口径歧义)
  - `##PW:P` → `dmidecode -t 17 2>/dev/null`(仅 `physMemCache` 未命中时追加该段)
  - `##PW:E` 收尾保证退出码 0
- 新增 `splitMetricSections(stdout)`,把现有 `collectCPU/collectMemory/collectDisk` 的逐命令采集改写为对合并输出的分段解析(解析器 `parseLoadavg/parseUptimeLoadavg/parseInt/parseFreeBytes/parseSwapBytes/parseDf/parseDmidecodeMemBytes` 全部复用,不动)
- `physMemCache` 拆为 `physMemNeedsProbe(id)`(是否需要在脚本里带 dmidecode 段)+ `physMemStore(id, phys, memTotal)`(保留 `p >= memTotal` 校验与 10min 冷却),语义不变
- 可达性判定:唯一一次 `svc.Exec` 的连接/定位错误 → `reachable:false` / 422/503,与现状一致
- `svc.Get`+vault 解密从每命令一次降为每次采集一次(顺带减少 DB 读)

### 2. TTL 快照缓存(stale-while-revalidate,单飞)
`internal/httpapi/server_metrics.go` 新增(仿 `build/remote.go` remoteCLICache 模式,可注入时钟):
- `snapshotServerMetrics(ctx, svc, id) (serverMetricsDTO, error)`:
  - 缓存新鲜(≤ `metricsCacheTTL = 10s`,对齐前端 12s 轮询)→ 直接返回快照,0 SSH
  - 过期 → **立即返回旧快照**,后台 goroutine 单飞刷新(`updating` 标志防并发重复;全局 `metricsRefreshSem` cap 6 限流)
  - 冷启动(无条目)→ 同步采集并入库,保证首屏真数据
  - 刷新遇定位类错误(凭据/保险库)→ 删除条目,下次请求走同步采集 → 单台端点正确映射 422/503;普通失败照常入缓存(不可达主机每 TTL 最多重试一次,后台不空转)
- 改 3 处调用方为 `snapshotServerMetrics`:`makeServerMetricsHandler`(`:527`)、`makeAllServerMetricsHandler`(`:561`)、`anomaly.go:119`——异常检测与历史采样器一并受益
- `collectServerMetrics` 保留为无缓存直采,签名不变(`server_ops_e2e_test.go:28` 不受影响)
- DTO 契约冻结不动(`collectedAt` 本就自描述数据年龄,ServerStatus 卡片已展示);前端、e2e stub 零改动

### 3. 收紧采集超时
- `metricsCmdTimeout 15s` → `metricsCollectTimeout = 10s`(合并后单台只剩 1 条命令,15s 的"三条串行余量"不再需要;不可达主机最坏 10s 失败,且因 SWR 只发生在后台,不阻塞接口)
- 不改 `internal/target`:ctx deadline 同时约束拨号与执行,拆分独立拨号超时需动 frozen 契约,收益比不高,不做

### 测试
`internal/httpapi/server_metrics_test.go`:
- `linuxLikeDialer`/`macLikeDialer` 改为匹配 `sh -c` 脚本(按 `strings.Contains` 分段名),返回带标记的合并 stdout(mac 假机模拟回退输出)
- 新增:脚本构造单测(分段齐全、dmidecode 仅探测时出现)、`splitMetricSections` 单测
- 新增缓存单测(注入 `metricsNow` + 刷新完成钩子 + `resetMetricsSnapshotCache()`):TTL 内零 SSH、过期触发且仅触发一次后台刷新(单飞)、定位错误不入缓存
- 现有解析器单测、unreachable/404/401/batch 独立性用例保持通过(batch 相关用例 setup 加缓存重置)

## 验证
`go vet ./...` + `go build ./...` + `go test ./...`(重点 `internal/httpapi`、`internal/target`)

## 效果预期
- 热路径(12s 轮询、Dashboard、异常检测、采样器):≤1 次采集/台/10s,接口本身 ~0 SSH、毫秒级返回
- 冷路径:每台 1 次 SSH 连接(原 5 次),整批首屏从"最慢台 15s"降到"最慢台 10s",且此后不再阻塞
