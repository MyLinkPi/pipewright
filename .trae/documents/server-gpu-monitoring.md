# GPU 机型监控:服务器 GPU 开关 + 多机状态总览显卡指标(nvtop -s)

## 概要(Summary)

服务器登记/编辑新增 **「GPU 机型」勾选**(`servers.gpu`)。勾选后,多机状态总览
([ServerStatus.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/views/ServerStatus.vue))的该台卡片
额外展示**每张显卡**的利用率 / 显存 / 温度 / 功耗 / 风扇 / 时钟(NVIDIA 另有编码/解码),
数据经目标机上的 `nvtop -s`(JSON 输出)采集,**支持多卡**,NVIDIA 与 AMD 通用。

范围边界(有意不做):GPU 指标**只实时展示**,不落库、不进异常检测规则与历史趋势
(`metric_samples` / 趋势图 / 采样器不变);**不展示每卡进程明细**(nvtop JSON 里的 `processes`
字段解析时忽略);不因勾选改变构建机池 / 部署目标选择(调度仍看 labels/槽位/优先级)。

## 现状分析(Current State)

- 采集:[server_metrics.go](file:///c:/Users/xufan/Trae/pipewright/internal/httpapi/server_metrics.go)
  每台**一次** SSH 连接跑一个**纯静态** `sh -c` 合并脚本(`##PW:<段名>` 标记行分段,AC-SEC-02 无注入面):
  `cat /proc/loadavg || uptime`、`nproc || getconf`、`free -b`、`df -B1 /`、`df -k /`,解析层逐段取值;
  某段缺失/异常 → 该维度 null,不报错、不连累其它维度。整体 10s 超时,包级 TTL 10s 快照缓存
  (stale-while-revalidate + 单飞),HTTP 单台/批量端点、异常检测采集器、历史采样器共享。
- 展示:[ServerMetricsCard.vue](file:///c:/Users/xufan/Trae/pipewright/web/src/components/ops/ServerMetricsCard.vue)
  用 `dl` 逐行渲染 CPU / 内存 / 交换 / 磁盘;前端 12s 轮询批量端点。
- 登记:`servers` 表为原生 SQL + `scanServer` 手工映射([target.go](file:///c:/Users/xufan/Trae/pipewright/internal/target/target.go)),
  布尔列先例 `pac_enabled`(INTEGER/TINYINT 0/1,扫成 int 再 `!= 0`);迁移要求 sqlite/mysql 成对
  (`migrate_mysql_test.go` 强制版本集一致)。
- nvtop 输出:AMD 无 `mem_total/mem_used/mem_free` 与 `encode/decode`;NVIDIA 全有,且带 `processes` 明细。
  值多为带单位字符串("926MHz" / "44C" / "14%" / "19W"),显存字节是纯十进制串。

## 修改方案(Proposed Changes)

### 1. 数据:`servers.gpu`

- 迁移 `0071_servers_gpu.sql`(sqlite `INTEGER NOT NULL DEFAULT 0` / mysql `TINYINT NOT NULL DEFAULT 0`)。
- `target.Server.Gpu bool`、`CreateInput.Gpu`、`UpdateInput.Gpu *bool`;INSERT/SELECT/UPDATE/`scanServer` 逐点串联。
- DTO `serverDTO.gpu`(camelCase),创建/更新请求体 `gpu` / `gpu?`(部分更新语义)。
- 前端 `Server.gpu: boolean`、`CreateServerInput.gpu?`、`UpdateServerInput.gpu?`。

### 2. 采集:合并脚本新增 `##PW:gpu` 段(仅勾选机型)

`metricsCollectArgs(probePhys, gpu)` 在 `gpu == true` 时追加:

```
echo "##PW:gpu"
timeout 5 nvtop -s 2>/dev/null
```

两个关键取舍:

- **`timeout 5` 兜底**:驱动异常时 nvtop 可能长时间卡住,不设上限会把整台机的 CPU/内存/磁盘
  一起拖进 10s 总超时(该台显示「采集超时」,丢掉全部指标);有上限则最坏只是 GPU 段为空。
  `timeout` 缺失(核心工具被裁)同样只让 GPU 段为空 —— 降级为「不可用」,其余维度不受影响。
- **`metricsOutMax` 64KiB → 256KiB**:nvtop JSON 含每进程 `cmdline`(解析层不用,但远端无法剔除),
  截断会打断 JSON。磁盘/内存段在 GPU 段之前,真被截断也只损失 GPU 一行。

采集前 `svc.Get(ctx, id)` 读一次登记信息拿 `Gpu` 开关(本地库读,便宜);定位类错误语义与原来
从 `Exec` 内部抛出时一致(单台端点仍映射 422/503)。

### 3. 解析:多卡 + 双厂商容错

- `parseNVTopDevices`:取**首个 `[` 到末个 `]`** 之间的片段反序列化(容忍命令夹带的提示文本);
  空 / `[]` / 解析失败 → 不可用。
- `parseNVTopNum`(前缀数字,忽略单位后缀,`N/A`/空/负数 → null)、`parseNVTopBytes`(纯整数,AMD 空串 → null)。
- DTO(`gpuMetric{devices[]}`,字段全指针 —— 厂商差异即 null):`index/name/gpuUtil/memUtil/memTotalBytes/
  memUsedBytes/memFreeBytes/tempC/fanSpeedPct/powerDrawW/gpuClockMhz/memClockMhz/encodeUtil/decodeUtil`。
  至少解析出 1 张卡才把 `gpu` 挂到 `serverMetricsDTO`(否则 `null` = 卡片「不可用」)。

### 4. 前端

- 登记/编辑弹窗加「GPU 机型」勾选(含 hint:需目标机已装 nvtop;纯监控不改调度),列表行加 `🎮 GPU` 徽标。
- 卡片新 prop `gpu?: boolean`(`ServerStatus` 从登记列表注入):仅勾选机型渲染 GPU 区块,每张卡一小块
  —— `#index device_name` → 两条**带可见标签**的细进度条(利用率 / 显存,>90 红 / >75 黄)→
  显存字节行(仅 NVIDIA 有;AMD 靠百分比条)→ 遥测行(温度/功耗/风扇/时钟;编码/解码仅 >0 时显示)。
  显存百分比有字节口径时按 used/total 精算(与展示的字节一致,可能与 nvtop 的 `mem_util` 差 1% 取整),
  AMD 退回 `mem_util`。`metrics.gpu === null` → 「不可用(未安装 nvtop 或未检出 GPU)」。
- i18n 8 语言(`opsServer.metrics.gpu*` + `settingsServers.fieldGpu*`/`gpuHint`/`gpuBadge`),
  `keyParity.test.ts` 保证键对齐。

## 测试

- Go:`server_metrics_test.go` —— 脚本断言(非 GPU 机型不含 nvtop / GPU 机型含 `timeout 5 nvtop -s`,
  与 dmidecode 段互不干扰)、`parseNVTopNum`/`parseNVTopBytes` 表驱动、`gpuFromSections`
  (NVIDIA 双卡含 `processes`、AMD 缺项全 null、噪音容忍、空/`[]`/垃圾 → nil)、HTTP 集成
  (GPU 机型解析出两卡且 CPU/内存/磁盘照常;非 GPU 机型 `gpu == nil` —— 假机只在脚本含 nvtop 时回段,
  故同时证明其脚本没跑 nvtop)。
- Go:`servers_test.go` 契约往返(缺省 false / 勾选创建 / 显式 false 关闭 / 不带字段不修改)、
  `target_test.go` 领域往返(`*bool` 部分更新)。
- 前端:`ServerMetricsCard.test.ts` 多卡渲染 / AMD 百分比兜底 / 无卡「不可用」/ 非 GPU 机型不渲染。

## 已知限制

- 需要目标机已安装 `nvtop`;未装 / 权限不足(读不到 DRM/NVML)/ 无显卡 → 该行「不可用」,不报错。
- 显存字节口径仅 NVIDIA 有;AMD 只有 `mem_util` 百分比。