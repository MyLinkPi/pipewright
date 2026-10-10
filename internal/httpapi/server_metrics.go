package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/target"
)

// Story 6.1(FR-15):多机状态总览 —— 服务器层资源指标(CPU 负载/核数、内存 used/total、
// 磁盘 used/total;GPU 机型另加每张显卡的利用率/显存/温度/功耗),经 SSH 跑**固定白名单
// 只读命令**采集,解析为结构化指标。
//
// 采集形态:每台**一次** SSH 连接跑一个合并脚本(metricsCollectArgs),脚本按 ##PW: 标记行
// 分段输出,解析层逐段取值 —— 把原来 ~5 条命令 × 各自一次完整 TCP+SSH 握手压成 1 次握手。
//
// 缓存形态:snapshotServerMetrics 提供 TTL 快照(stale-while-revalidate,后台单飞刷新),
// 两个 metrics HTTP 端点、异常检测采集器(anomaly.go)、历史采样器(main.go tick)共享 ——
// 慢主机/不可达主机只拖后台刷新,不阻塞任何调用方。
//
// AC-SEC-02 核心:采集脚本是**纯静态文本**,绝不接受任何用户输入拼接 —— 无注入面。
// 指标无敏感信息。
//
// 容错纪律:
//   - 某台不可达 / 认证失败 → 该台 reachable:false + 人读 error,**不 500**,不连累其它台。
//   - 单个指标段缺失 / 输出格式异常 → 该指标 null(指针为 nil),不报错、不影响其它指标
//     (跨平台 best-effort:回退逻辑写在脚本里,Linux 优先,macOS → 对应段空 → 该指标 null)。
//   - 批量端点逐台并行采集,有界并发(信号量防 N 台同时 SSH 打爆)。

const (
	// metricsConcurrency 是批量采集 / 后台刷新的最大并发 SSH 数(有界,防打爆)。
	metricsConcurrency = 6
	// metricsCollectTimeout 是单台一次合并采集的整体超时(拨号 + 握手 + 1 条脚本)。
	// 合并前是 15s(~5 条命令各自串行握手);合并后只剩 1 条命令,10s 足够且让不可达
	// 主机更快失败(配合快照缓存,失败只发生在后台,不阻塞接口)。
	metricsCollectTimeout = 10 * time.Second
	// metricsCacheTTL 是指标快照的保鲜期。对齐前端 12s 轮询(ServerStatus.vue):热路径
	// 几乎每拍命中,或恰好触发一轮后台刷新;异常检测 / 采样器的 60s tick 读到的至多是
	// 一个采集周期前的数据,对阈值判定无感。
	metricsCacheTTL = 10 * time.Second
	// metricsOutMax 是脚本 stdout 解析前的截断上限(防超大输出撑爆内存)。GPU 机型的
	// `nvtop -s` 段是 JSON,含每进程明细(cmdline 可能很长;解析层只取显卡级字段却无法在
	// 远端剔除),故留 256KiB 余量 —— 截断会打断 JSON,宁可多留。磁盘/内存段在 GPU 段之前,
	// 万一真被截断也只损失 GPU 一行(降级为「不可用」),其余指标不受影响。
	metricsOutMax = 256 * 1024
)

// ─── 合并采集脚本(AC-SEC-02:纯静态文本,绝不含任何用户输入)────────────────────
//
// 各段以 ##PW:<名> 标记行分隔;段内是该台命令的 stdout(命令缺失/失败 → 段为空 → 该指标 null)。
//   - loadavg:`cat /proc/loadavg`(Linux);macOS 无 /proc → `|| uptime` 回退,解析层两种
//     格式都试。
//   - cores:`nproc`;缺失回退 `getconf _NPROCESSORS_ONLN`(跨平台,含 macOS)。
//   - memory:`free -b`;macOS 无 free → 段空 → 该指标 null(契约允许)。
//   - disk:`df -B1 /`(字节)与 `df -k /`(KiB)**两段总是都跑** —— 不按数字大小猜口径
//     (KiB 数当字节解析会差 1024 倍),解析层先取字节段、缺失再取 KiB 段换算。
//   - physmem:`dmidecode -t 17` 物理/分配内存(SMBIOS Type 17 容量之和)。需 root;非 root /
//     无 dmidecode / 虚拟化未暴露 SMBIOS → 段空 → 0(不展示)。静态量,经 physMem 缓存,
//     仅缓存未命中时才把该段拼进脚本(成功值长期复用,失败 10min 冷却)。
//   - gpu:`nvtop -s`(JSON,含每张卡的利用率/显存/温度/功耗/时钟,支持多卡、兼容 NVIDIA 与
//     AMD)。**仅勾选「GPU 机型」的服务器才拼进脚本**(登记开关,见 target.Server.Gpu);
//     未装 nvtop / 无显卡 / 输出不可解析 → 段空 → 该维度 null。前缀 `timeout 5` 兜底卡死
//     (见 metricsCollectArgs)。
//   - end:收尾标记,保证脚本恒以 0 退出(段命令失败不影响整体执行)。
const metricMarker = "##PW:"

const (
	secLoadavg = "loadavg"
	secCores   = "cores"
	secMemory  = "memory"
	secDiskB   = "diskb"
	secDiskK   = "diskk"
	secPhysMem = "physmem"
	secGpu     = "gpu"
	secEnd     = "end"
)

// metricsCollectArgs 返回单台一次性采集命令(sh -c 脚本;sh 在 Linux/macOS 恒在,
// 与 target.Upload / build.DetectRemoteCLI 的 sh -c 组合命令同款做法)。
// probePhys = 需要把 dmidecode 段拼进脚本;gpu = 该机为 GPU 机型(需要 nvtop 段)。
func metricsCollectArgs(probePhys, gpu bool) []string {
	parts := []string{
		`echo "` + metricMarker + secLoadavg + `"`,
		`cat /proc/loadavg 2>/dev/null || uptime`,
		`echo "` + metricMarker + secCores + `"`,
		`nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null`,
		`echo "` + metricMarker + secMemory + `"`,
		`free -b 2>/dev/null`,
		`echo "` + metricMarker + secDiskB + `"`,
		`df -B1 / 2>/dev/null`,
		`echo "` + metricMarker + secDiskK + `"`,
		`df -k / 2>/dev/null`,
	}
	if probePhys {
		parts = append(parts,
			`echo "`+metricMarker+secPhysMem+`"`,
			`dmidecode -t 17 2>/dev/null`)
	}
	if gpu {
		// `timeout 5` 兜底:驱动异常时 nvtop 可能长时间卡住,不设上限会把整台机的 CPU/内存/
		// 磁盘采集一起拖进 10s 总超时(该台显示「采集超时」);有上限则最坏只是 GPU 段为空 →
		// 该行「不可用」。timeout 缺失(核心工具被裁)同样只让 GPU 段为空,不影响其它维度。
		parts = append(parts,
			`echo "`+metricMarker+secGpu+`"`,
			`timeout 5 nvtop -s 2>/dev/null`)
	}
	parts = append(parts, `echo "`+metricMarker+secEnd+`"`)
	return []string{"sh", "-c", strings.Join(parts, "; ")}
}

// splitMetricSections 按 ##PW:<名> 标记行切分脚本 stdout(段名 → 段文本;标记后无输出
// 即空段,段命令失败/缺失的常态)。首个标记之前的内容(正常不该出现)丢弃。
func splitMetricSections(out string) map[string]string {
	sections := map[string]string{}
	cur := ""
	var buf strings.Builder
	flush := func() {
		if cur != "" {
			sections[cur] = buf.String()
		}
	}
	// 去掉尾部换行:Split 末尾的空元素会在段尾多拼一个空行(解析无碍,但段内容不确定)。
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, metricMarker) {
			flush()
			cur = strings.TrimSpace(strings.TrimPrefix(line, metricMarker))
			buf.Reset()
			continue
		}
		if cur != "" {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
	flush()
	return sections
}

// cpuMetric / memoryMetric / diskMetric 是各维度指标 DTO(冻结契约字段形状)。
// 任一维度采集/解析失败 → 整段为 null(指针 nil),不影响其它维度。
type cpuMetric struct {
	Loadavg1 *float64 `json:"loadavg1"`
	Cores    *int     `json:"cores"`
}

type memoryMetric struct {
	UsedBytes int64 `json:"usedBytes"` // 不含可回收页缓存(进程真实占用 / 内存压力口径)
	// 含页缓存(total - free);与 cgroup 总用量 / PVE 等容器面板的「已用」一致。
	UsedWithCacheBytes int64 `json:"usedWithCacheBytes"`
	TotalBytes         int64 `json:"totalBytes"` // free 的 MemTotal:内核**可用**总量
	// 物理/分配总量(dmidecode SMBIOS);0 表示采集不到。通常 ≥ TotalBytes,与宿主
	// 面板(PVE 等)显示的总量一致,用作「含缓存」口径的分母以对齐其百分比。
	PhysicalTotalBytes int64 `json:"physicalTotalBytes"`
	// 交换分区 used/total(free 的 Swap 行);SwapTotalBytes 为 0 表示未配置 swap。
	SwapUsedBytes  int64 `json:"swapUsedBytes"`
	SwapTotalBytes int64 `json:"swapTotalBytes"`
}

type diskMetric struct {
	Path       string `json:"path"`
	UsedBytes  int64  `json:"usedBytes"`
	TotalBytes int64  `json:"totalBytes"`
}

// gpuDeviceMetric 是**单张显卡**的指标(nvtop -s 一条记录)。字段全部为指针:采不到的
// 维度为 null(NVIDIA / AMD 暴露的字段本就不同 —— AMD 无显存字节总量与编解码利用率)。
type gpuDeviceMetric struct {
	Index         int      `json:"index"`         // nvtop 输出顺序(多卡时 0..N-1)
	Name          string   `json:"name"`          // device_name
	GpuUtil       *float64 `json:"gpuUtil"`       // GPU 利用率 %
	MemUtil       *float64 `json:"memUtil"`       // 显存利用率 %
	MemTotalBytes *int64   `json:"memTotalBytes"` // 显存总量(字节;AMD 无 → null)
	MemUsedBytes  *int64   `json:"memUsedBytes"`
	MemFreeBytes  *int64   `json:"memFreeBytes"`
	TempC         *float64 `json:"tempC"`
	FanSpeedPct   *float64 `json:"fanSpeedPct"`
	PowerDrawW    *float64 `json:"powerDrawW"`
	GpuClockMHz   *float64 `json:"gpuClockMhz"`
	MemClockMHz   *float64 `json:"memClockMhz"`
	EncodeUtil    *float64 `json:"encodeUtil"` // 编码器利用率 %(NVIDIA 可见)
	DecodeUtil    *float64 `json:"decodeUtil"` // 解码器利用率 %
}

// gpuMetric 是该机的显卡集合(多卡:devices 按 nvtop 输出顺序;至少 1 张才会非 null 挂到
// serverMetricsDTO 上 —— 见 gpuFromSections)。
type gpuMetric struct {
	Devices []gpuDeviceMetric `json:"devices"`
}

// serverMetricsDTO 是单台服务器指标响应体(冻结契约)。
//   - reachable:false 时 cpu/memory/disk 为 null,error 人读非空。
//   - reachable:true 时各指标独立:解析失败的维度为 null,其余正常。
type serverMetricsDTO struct {
	ServerID  string        `json:"serverId"`
	Reachable bool          `json:"reachable"`
	Error     string        `json:"error"`
	CPU       *cpuMetric    `json:"cpu"`
	Memory    *memoryMetric `json:"memory"`
	Disk      *diskMetric   `json:"disk"`
	// GPU 是显卡指标(仅勾选「GPU 机型」的服务器采集;未勾选 / 未装 nvtop / 无显卡 / 输出
	// 不可解析 → null)。多卡为一个 devices 数组。
	GPU         *gpuMetric `json:"gpu"`
	CollectedAt string     `json:"collectedAt"`
}

// isLocateError 判定是否为「定位类」错误(服务器/凭据不存在、保险库未配)——这类该映射
// 422/503 而非 reachable:false。
func isLocateError(err error) bool {
	return errors.Is(err, target.ErrNotFound) ||
		errors.Is(err, target.ErrCredentialNotFound) ||
		errors.Is(err, target.ErrVaultUnconfigured)
}

// runMetricCmd 跑一条采集命令并返回截断后的 stdout。第二个返回值是「连接/定位类」错误
// (供 reachable 判定);命令非零退出不算连接错误(返回 stdout,由解析层据空/异常输出降级)。
func runMetricCmd(ctx context.Context, svc target.Service, id string, cmd []string) (string, error) {
	res, err := svc.Exec(ctx, id, cmd)
	if err != nil {
		return "", err
	}
	out := res.Stdout
	if len(out) > metricsOutMax {
		out = out[:metricsOutMax]
	}
	return out, nil
}

// ─── 段解析(合并 stdout → 各维度 DTO)────────────────────────────────────────

// cpuFromSections 解析 loadavg / cores 段。段空或格式异常 → 对应子字段 nil(cpu 段仍返回)。
// loadavg 兼容两种源:/proc/loadavg 格式优先,不匹配再按 uptime 输出解析(macOS 回退)。
func cpuFromSections(sections map[string]string) *cpuMetric {
	m := &cpuMetric{}
	if v, ok := parseLoadavg(sections[secLoadavg]); ok {
		m.Loadavg1 = &v
	} else if v, ok := parseUptimeLoadavg(sections[secLoadavg]); ok {
		m.Loadavg1 = &v
	}
	if c, ok := parseInt(sections[secCores]); ok {
		m.Cores = &c
	}
	return m
}

// memoryFromSections 解析 memory 段(`free -b`)。段空/解析失败(如 macOS 无 free)→ nil。
func memoryFromSections(sections map[string]string) *memoryMetric {
	used, usedWithCache, total, ok := parseFreeBytes(sections[secMemory])
	if !ok {
		return nil
	}
	m := &memoryMetric{UsedBytes: used, UsedWithCacheBytes: usedWithCache, TotalBytes: total}
	// Swap 与内存来自同一份 `free -b`:解析 Swap 行(未配置 swap → 0/0)。
	if su, st, sok := parseSwapBytes(sections[secMemory]); sok {
		m.SwapUsedBytes, m.SwapTotalBytes = su, st
	}
	return m
}

// diskFromSections 解析磁盘两段:字节段(`df -B1 /`)优先,KiB 段(`df -k /`)回退换算。
// 两段皆空/皆解析失败 → nil。
func diskFromSections(sections map[string]string) *diskMetric {
	if used, total, ok := parseDf(sections[secDiskB], 1); ok {
		return &diskMetric{Path: "/", UsedBytes: used, TotalBytes: total}
	}
	if used, total, ok := parseDf(sections[secDiskK], 1024); ok {
		return &diskMetric{Path: "/", UsedBytes: used, TotalBytes: total}
	}
	return nil
}

// ─── GPU 段解析(nvtop -s)──────────────────────────────────────────────────────

// nvtopDeviceRaw 是 `nvtop -s` JSON 里一张显卡的原始记录:值多为**带单位的字符串**
// ("926MHz" / "44C" / "14%" / "19W"),显存字节是纯十进制串;缺项为空串。processes 等
// 未列出的字段被忽略(本需求只看显卡级指标,不展示进程明细)。
type nvtopDeviceRaw struct {
	DeviceName string `json:"device_name"`
	GpuClock   string `json:"gpu_clock"`
	MemClock   string `json:"mem_clock"`
	Temp       string `json:"temp"`
	FanSpeed   string `json:"fan_speed"`
	PowerDraw  string `json:"power_draw"`
	GpuUtil    string `json:"gpu_util"`
	MemUtil    string `json:"mem_util"`
	Encode     string `json:"encode"`
	Decode     string `json:"decode"`
	MemTotal   string `json:"mem_total"`
	MemUsed    string `json:"mem_used"`
	MemFree    string `json:"mem_free"`
}

// parseNVTopDevices 解析 `nvtop -s` 的 JSON 数组。先按整串解析(常态:输出就是纯 JSON);
// 失败再取**首个 `[` 到末个 `]`** 之间的片段重试 —— 容忍命令在 JSON 前后夹带的提示/告警文本。
// 空 / 无数组 / 解析失败 / 0 张卡 → false。
func parseNVTopDevices(s string) ([]nvtopDeviceRaw, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, false
	}
	var raw []nvtopDeviceRaw
	// 常态:输出就是纯 JSON,整串直接解析(进程 cmdline 里出现 `]` 也不影响)。
	if err := json.Unmarshal([]byte(t), &raw); err != nil {
		// 退一步:取首个 `[` 到末个 `]` 之间的片段重试(容忍 JSON 前后夹带的提示/告警文本)。
		start := strings.Index(t, "[")
		end := strings.LastIndex(t, "]")
		if start < 0 || end <= start {
			return nil, false
		}
		if err := json.Unmarshal([]byte(t[start:end+1]), &raw); err != nil {
			return nil, false
		}
	}
	if len(raw) == 0 {
		return nil, false
	}
	return raw, true
}

// parseNVTopNum 解析 nvtop 指标值的**前缀数值**(单位后缀直接忽略):
// "926MHz" → 926、"350MHz" → 350、"44C" → 44、"14%" → 14、"19W" → 19、"0%" → 0。
// 空 / "N/A" / 非数字开头 / 负数 → false(该字段留 null,不影响同一张卡的其它字段)。
func parseNVTopNum(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	end := 0
	for end < len(t) {
		c := t[end]
		if (c >= '0' && c <= '9') || c == '.' || (end == 0 && (c == '+' || c == '-')) {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(t[:end], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// parseNVTopBytes 解析 nvtop 的显存字节字段(mem_total / mem_used / mem_free,纯十进制串)。
// 空(AMD 输出没有这些字段)/ 非整数 / 负数 → false(该字段留 null,由 mem_util 百分比兜底)。
func parseNVTopBytes(s string) (int64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// gpuFromSections 解析 gpu 段(`timeout 5 nvtop -s` 的 JSON)为多卡 DTO。
// 段空(非 GPU 机型不拼该段 / 未装 nvtop / 命令失败)/ 无显卡 / 解析失败 → nil(卡片该行
// 「不可用」,不报错、不影响其它维度)。
func gpuFromSections(sections map[string]string) *gpuMetric {
	raw, ok := parseNVTopDevices(sections[secGpu])
	if !ok {
		return nil
	}
	m := &gpuMetric{Devices: make([]gpuDeviceMetric, 0, len(raw))}
	for i, d := range raw {
		dev := gpuDeviceMetric{Index: i, Name: d.DeviceName}
		if v, ok := parseNVTopNum(d.GpuUtil); ok {
			dev.GpuUtil = &v
		}
		if v, ok := parseNVTopNum(d.MemUtil); ok {
			dev.MemUtil = &v
		}
		if v, ok := parseNVTopBytes(d.MemTotal); ok {
			dev.MemTotalBytes = &v
		}
		if v, ok := parseNVTopBytes(d.MemUsed); ok {
			dev.MemUsedBytes = &v
		}
		if v, ok := parseNVTopBytes(d.MemFree); ok {
			dev.MemFreeBytes = &v
		}
		if v, ok := parseNVTopNum(d.Temp); ok {
			dev.TempC = &v
		}
		if v, ok := parseNVTopNum(d.FanSpeed); ok {
			dev.FanSpeedPct = &v
		}
		if v, ok := parseNVTopNum(d.PowerDraw); ok {
			dev.PowerDrawW = &v
		}
		if v, ok := parseNVTopNum(d.GpuClock); ok {
			dev.GpuClockMHz = &v
		}
		if v, ok := parseNVTopNum(d.MemClock); ok {
			dev.MemClockMHz = &v
		}
		if v, ok := parseNVTopNum(d.Encode); ok {
			dev.EncodeUtil = &v
		}
		if v, ok := parseNVTopNum(d.Decode); ok {
			dev.DecodeUtil = &v
		}
		m.Devices = append(m.Devices, dev)
	}
	return m
}

// ─── 物理内存缓存 ──────────────────────────────────────────────────────────────
//
// dmidecode 取的物理/分配内存是静态量(不随负载变),但采集页可能每 10s 轮询一次。
// 按 serverID 缓存「是否已探测」:成功值(>0)长期复用且不再进脚本;失败(非 root /
// 无 dmidecode / SSH 抖动)记 0 并冷却,冷却期内也不重拨。进程重启即清空,自然容纳
// 极少见的换内存。
type physMemEntry struct {
	bytes    int64 // >0:已知物理总量;0:探测过但取不到
	probedAt time.Time
}

var (
	physMemMu    sync.Mutex
	physMemCache = map[string]physMemEntry{}
)

// physMemRetryCooldown:对「取不到」的主机多久重试一次 dmidecode(成功值不受此限,永久缓存)。
const physMemRetryCooldown = 10 * time.Minute

// physMemLookup 返回 (cachedBytes, needsProbe):已有成功缓存 → 直接用、不再探测;
// 失败冷却期内 → 用 0、不探测;其余 → 需要在采集脚本里带上 dmidecode 段。
func physMemLookup(id string) (int64, bool) {
	now := time.Now()
	physMemMu.Lock()
	defer physMemMu.Unlock()
	if e, hit := physMemCache[id]; hit && (e.bytes > 0 || now.Sub(e.probedAt) < physMemRetryCooldown) {
		return e.bytes, false
	}
	return 0, true
}

// physMemStore 记录一轮 dmidecode 探测结果(成功值 >0;失败/不合理记 0 进冷却)。
func physMemStore(id string, phys int64) {
	physMemMu.Lock()
	physMemCache[id] = physMemEntry{bytes: phys, probedAt: time.Now()}
	physMemMu.Unlock()
}

// collectServerMetrics 对单台服务器**直采**(无缓存;HTTP/异常检测/采样一律走
// snapshotServerMetrics)。永不返回 error:不可达/失败均落到 DTO.reachable=false + 人读
// error(批量端点据此让单台失败不连累全局)。
// 第二个返回值是「定位类」错误(服务器/凭据不存在、保险库未配),仅供单台端点映射 422/503;
// 批量端点忽略它(逐台独立,定位类对某台亦只表现为该台 reachable:false)。
//
// 采集 = 1 次 SSH 连接:跑一个合并脚本,按段解析。唯一一次 Exec 的连接/认证类失败决定
// reachable;某段缺失/异常仅该指标 null。
func collectServerMetrics(ctx context.Context, svc target.Service, id string) (serverMetricsDTO, error) {
	out := serverMetricsDTO{ServerID: id, CollectedAt: time.Now().UTC().Format(time.RFC3339)}

	// 先取登记信息:GPU 机型才把 nvtop 段拼进采集脚本(非 GPU 机型脚本与历史完全一致)。
	// 一次本地库读,便宜;定位类错误语义与原来从 Exec 内部抛出时一致(单台端点映射 422/503)。
	srv, err := svc.Get(ctx, id)
	if err != nil {
		out.Reachable = false
		out.Error = humanMetricsError(err)
		if isLocateError(err) {
			return out, err
		}
		return out, nil
	}

	cctx, cancel := context.WithTimeout(ctx, metricsCollectTimeout)
	defer cancel()

	cachedPhys, probePhys := physMemLookup(id)
	outStr, err := runMetricCmd(cctx, svc, id, metricsCollectArgs(probePhys, srv.Gpu))
	if err != nil {
		out.Reachable = false
		out.Error = humanMetricsError(err)
		if isLocateError(err) {
			return out, err
		}
		return out, nil
	}
	out.Reachable = true
	sections := splitMetricSections(outStr)
	out.CPU = cpuFromSections(sections)
	out.Memory = memoryFromSections(sections)
	out.Disk = diskFromSections(sections)
	out.GPU = gpuFromSections(sections)
	// 物理/分配内存:本轮带探测段才解析入库(成功值长期复用);无探测段直接用缓存值。
	// 内存段失败时整块跳过(物理量以内核可用量做合理性校验,没 total 无从校验)。
	if out.Memory != nil {
		switch {
		case probePhys:
			var phys int64
			if p, ok := parseDmidecodeMemBytes(sections[secPhysMem]); ok && p >= out.Memory.TotalBytes {
				phys = p
			}
			physMemStore(id, phys)
			out.Memory.PhysicalTotalBytes = phys
		case cachedPhys > 0:
			out.Memory.PhysicalTotalBytes = cachedPhys
		}
	}
	return out, nil
}

// ─── 指标快照缓存(stale-while-revalidate,单飞)────────────────────────────────
//
// 包级全局(进程内);按 serverID 存最后一次采集结果。TTL 内直接命中(0 SSH);过期则
// **先回旧值**、后台单飞刷新 —— 轮询端点、异常检测、历史采样共享同一份,慢主机/不可达
// 主机的重试被自然压到每 TTL 最多一次,且只发生在后台。进程重启即冷(首请求同步采集)。
type metricsSnapshotEntry struct {
	dto      serverMetricsDTO
	at       time.Time
	updating bool // 后台刷新单飞标志
}

var (
	metricsSnapMu    sync.Mutex
	metricsSnapshots = map[string]*metricsSnapshotEntry{}
	// metricsNow 可注入时钟(单测 TTL 过期路径,仿 build/remote.go remoteCLICache)。
	metricsNow = time.Now
	// metricsRefreshSem 限后台刷新并发(与采集并发同界),防一批同时过期把 SSH 打爆。
	metricsRefreshSem = make(chan struct{}, metricsConcurrency)
	// metricsRefreshed 测试钩子:每次后台刷新收尾后调用(单测等待异步刷新完成)。
	metricsRefreshed func()
)

// resetMetricsSnapshotCache 清空快照缓存(测试隔离用:缓存是包级全局,跨用例会串扰)。
func resetMetricsSnapshotCache() {
	metricsSnapMu.Lock()
	metricsSnapshots = map[string]*metricsSnapshotEntry{}
	metricsSnapMu.Unlock()
}

// snapshotServerMetrics 返回该台指标快照。第二个返回值仅在冷启动(缓存无条目)同步采集时
// 可能是「定位类」错误(服务器/凭据不存在、保险库未配),供单台端点映射 422/503;
// 缓存命中 / 后台刷新路径恒为 nil(定位类错误不入缓存,见 metricsRefresh)。
func snapshotServerMetrics(ctx context.Context, svc target.Service, id string) (serverMetricsDTO, error) {
	if dto, ok := metricsSnapshotLookup(svc, id); ok {
		return dto, nil
	}
	// 冷启动:同步采集一次,保证首屏真数据。用 WithoutCancel 脱钩请求生命周期 ——
	// 请求方中途断开也不浪费已完成大半的采集,结果照常入缓存供下一请求使用。
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), metricsCollectTimeout)
	defer cancel()
	dto, locErr := collectServerMetrics(cctx, svc, id)
	if locErr != nil {
		return dto, locErr
	}
	metricsSnapshotStore(dto)
	return dto, nil
}

// metricsSnapshotLookup 命中:TTL 内原样返回;过期则触发后台单飞刷新并返回旧值
// (stale-while-revalidate)。未命中 → false(调用方冷启动同步采集)。
func metricsSnapshotLookup(svc target.Service, id string) (serverMetricsDTO, bool) {
	now := metricsNow()
	metricsSnapMu.Lock()
	e, ok := metricsSnapshots[id]
	if !ok {
		metricsSnapMu.Unlock()
		return serverMetricsDTO{}, false
	}
	dto := e.dto
	fresh := now.Sub(e.at) < metricsCacheTTL
	inFlight := e.updating
	if !fresh && !inFlight {
		e.updating = true
	}
	metricsSnapMu.Unlock()
	if fresh || inFlight {
		return dto, true
	}
	// 过期且无人刷新:后台单飞刷新(不持锁、不用请求 ctx),先回旧值。
	go func() {
		metricsRefreshSem <- struct{}{}
		defer func() { <-metricsRefreshSem }()
		metricsSnapshotRefresh(svc, id)
	}()
	return dto, true
}

func metricsSnapshotStore(dto serverMetricsDTO) {
	metricsSnapMu.Lock()
	metricsSnapshots[dto.ServerID] = &metricsSnapshotEntry{dto: dto, at: metricsNow()}
	metricsSnapMu.Unlock()
}

// metricsSnapshotRefresh 后台刷新一台的指标。结果无条件落缓存(可达与否都算数,把重试
// 频率自然压到每 TTL 最多一次);定位类错误则删条目 —— 下次请求冷启动同步采集,让单台
// 端点把凭据/保险库问题正确映射为 422/503,而不是吃过期 200。
func metricsSnapshotRefresh(svc target.Service, id string) {
	defer func() {
		metricsSnapMu.Lock()
		if e, ok := metricsSnapshots[id]; ok {
			e.updating = false
		}
		metricsSnapMu.Unlock()
		if metricsRefreshed != nil {
			metricsRefreshed()
		}
	}()
	// 后台刷新不用请求 ctx(响应返回后会被取消),独立超时兜底。
	ctx, cancel := context.WithTimeout(context.Background(), metricsCollectTimeout)
	defer cancel()
	dto, locErr := collectServerMetrics(ctx, svc, id)
	metricsSnapMu.Lock()
	defer metricsSnapMu.Unlock()
	if locErr != nil {
		delete(metricsSnapshots, id)
		return
	}
	metricsSnapshots[id] = &metricsSnapshotEntry{dto: dto, at: metricsNow()}
}

// --- 解析器(纯函数,可单测;空/格式异常一律 ok=false,绝不 panic) ---

// parseLoadavg 解析 `/proc/loadavg`,取第一个字段(1 分钟负载)。
// 例:`0.42 0.35 0.30 1/234 5678` → 0.42。
func parseLoadavg(s string) (float64, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// parseUptimeLoadavg 从 `uptime` 输出解析 1 分钟负载(macOS/Linux 通用)。
// 例:`... load averages: 1.23 1.10 1.05`(macOS)或 `... load average: 1.23, 1.10, 1.05`(Linux)。
func parseUptimeLoadavg(s string) (float64, bool) {
	low := strings.ToLower(s)
	idx := strings.Index(low, "load average")
	if idx < 0 {
		return 0, false
	}
	rest := s[idx:]
	// 跳到冒号后。
	if c := strings.Index(rest, ":"); c >= 0 {
		rest = rest[c+1:]
	}
	// 逗号/空白都当分隔。
	rest = strings.ReplaceAll(rest, ",", " ")
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// parseInt 解析单个整数(nproc / getconf 输出),裁剪空白。
func parseInt(s string) (int, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	// 取第一行第一个 token(防多余输出)。
	fields := strings.Fields(t)
	v, err := strconv.Atoi(fields[0])
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// parseFreeBytes 解析 `free -b` 输出,取 Mem 行的 total 与两种「已使用」口径(字节)。
// 形如:
//
//	              total        used        free      shared  buff/cache   available
//	Mem:    17179869184   2854748160   706924544  ...
//
// 返回两口径(均常见、各有用途,不绑定任何虚拟化平台):
//   - used:free 的「used」列(第 2 列)—— **不含可回收页缓存**,反映进程真实占用 /
//     内存压力(htop、node_exporter 同口径)。
//   - usedWithCache:total - free(= used + buff/cache)—— **含页缓存**,与 cgroup 总用量 /
//     PVE 等容器面板的「已用」一致(它们把页缓存算进已用)。
//
// 取 Mem 行第 1 列 total、第 2 列 used、第 3 列 free。容错:列不足/非数字/越界 → false。
func parseFreeBytes(s string) (used, usedWithCache, total int64, ok bool) {
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(fields[0]), "mem") {
			continue
		}
		t, err1 := strconv.ParseInt(fields[1], 10, 64)
		u, err2 := strconv.ParseInt(fields[2], 10, 64)
		f, err3 := strconv.ParseInt(fields[3], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || t <= 0 || u < 0 || f < 0 || f > t {
			return 0, 0, 0, false
		}
		return u, t - f, t, true
	}
	return 0, 0, 0, false
}

// parseSwapBytes 解析 `free -b` 的 Swap 行,取 total/used(字节)。形如:
//
//	Swap:    2147483648    536870912   1610612736
//
// 取第 1 列 total、第 2 列 used。无 Swap 行(部分系统)或列不足/非数字 → false;
// total=0(未配置 swap)仍算成功(used/total 均 0,UI 据此不渲染 swap 行)。
func parseSwapBytes(s string) (used, total int64, ok bool) {
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(fields[0]), "swap") {
			continue
		}
		t, err1 := strconv.ParseInt(fields[1], 10, 64)
		u, err2 := strconv.ParseInt(fields[2], 10, 64)
		if err1 != nil || err2 != nil || t < 0 || u < 0 {
			return 0, 0, false
		}
		return u, t, true
	}
	return 0, 0, false
}

// parseDmidecodeMemBytes 解析 `dmidecode -t 17` 输出,累加各「已装」内存设备的 Size
// 得物理/分配总量(字节)。形如:
//
//	Memory Device
//	        Size: 8 GiB
//	Memory Device
//	        Size: No Module Installed
//
// 单位大小写不敏感,兼容 dmidecode 各版本写法:kB/MB/GB/TB(十进制千)与 KiB/MiB/GiB/TiB
// (二进制 1024)。"No Module Installed" / "Unknown" / 非数字 → 跳过该设备。
// 一个有效 Size 都没有 → false(交由上层留 0、不展示)。
func parseDmidecodeMemBytes(s string) (int64, bool) {
	var total int64
	found := false
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Size:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Size:"))
		if len(fields) < 2 {
			continue // "No Module Installed"/"Unknown" 等 → 跳过
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		mult, uok := memUnitMultiplier(fields[1])
		if !uok {
			continue
		}
		total += n * mult
		found = true
	}
	if !found || total <= 0 {
		return 0, false
	}
	return total, true
}

// memUnitMultiplier 把内存单位换算为字节倍数。二进制单位(KiB/MiB/...)按 1024 进位,
// 十进制单位(kB/MB/...)按 1000 进位;dmidecode 现版本用二进制(GiB),旧版用 MB/GB。
func memUnitMultiplier(unit string) (int64, bool) {
	switch strings.ToLower(unit) {
	case "kb":
		return 1000, true
	case "mb":
		return 1000 * 1000, true
	case "gb":
		return 1000 * 1000 * 1000, true
	case "tb":
		return 1000 * 1000 * 1000 * 1000, true
	case "kib":
		return 1024, true
	case "mib":
		return 1024 * 1024, true
	case "gib":
		return 1024 * 1024 * 1024, true
	case "tib":
		return 1024 * 1024 * 1024 * 1024, true
	}
	return 0, false
}

// parseDf 解析 `df` 输出的根分区行,取 total(第 2 列)/ used(第 3 列),乘以 unit 化为字节。
// `df -B1 /` 时 unit=1(已是字节);`df -k /` 时 unit=1024(KiB)。
// 形如:
//
//	Filesystem     1B-blocks       Used   Available Use% Mounted on
//	/dev/disk1  494384795648  ...
//
// df 可能把长设备名折行;故扫描所有非表头行,取**首个含 ≥4 个数值列**的数据行。容错 → false。
func parseDf(s string, unit int64) (used, total int64, ok bool) {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// 跳过表头(首列 Filesystem)。
		if i == 0 || strings.EqualFold(fields[0], "Filesystem") {
			continue
		}
		// df 折行时数据可能在下一行,字段数少；规整后期望:[fs] total used avail use% mounted
		// 或折行后:total used avail use% mounted(无 fs 列)。统一找连续两个可解析为大整数的列。
		t, u, found := extractDfTotalsUsed(fields)
		if found {
			if t <= 0 || u < 0 {
				return 0, 0, false
			}
			return u * unit, t * unit, true
		}
	}
	return 0, 0, false
}

// extractDfTotalsUsed 从 df 数据行字段里取 total/used。标准布局:
// Filesystem total used avail capacity ... → total=fields[1], used=fields[2]。
// 折行布局(首列已被折到上一行):total used avail ... → total=fields[0], used=fields[1]。
// 用启发式:找首个连续两列都是纯数字(且后续还有列)的位置当 total/used。
func extractDfTotalsUsed(fields []string) (total, used int64, ok bool) {
	for i := 0; i+1 < len(fields); i++ {
		t, e1 := strconv.ParseInt(fields[i], 10, 64)
		u, e2 := strconv.ParseInt(fields[i+1], 10, 64)
		if e1 == nil && e2 == nil {
			return t, u, true
		}
	}
	return 0, 0, false
}

// humanMetricsError 把领域错误映射为人读文案(绝不含凭据明文/内部栈)。
func humanMetricsError(err error) string {
	switch {
	case errors.Is(err, target.ErrAuth):
		return "SSH 认证失败:密钥或口令无效,或无登录权限"
	case errors.Is(err, target.ErrUnreachable):
		return "无法连接服务器:端口未开放、主机不可达或超时"
	case errors.Is(err, target.ErrInvalidCredential):
		return "凭据不是可用的 SSH 私钥或口令"
	case errors.Is(err, context.DeadlineExceeded):
		return "采集超时"
	default:
		return "采集指标失败:连接或命令执行错误"
	}
}

// --- HTTP handlers ---

// makeServerMetricsHandler 返回 GET /api/servers/{id}/metrics(认证,只读)。
// 服务器不存在/凭据不存在/保险库未配 → 标准状态码;连接/认证/采集失败 → 200 + reachable:false,不 500。
// 数据走 TTL 快照缓存(stale-while-revalidate):热路径 0 SSH,慢主机只拖后台刷新。
func makeServerMetricsHandler(svc target.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务器服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		// 先确认服务器存在(404 在写任何 200 体之前)。
		if _, err := svc.Get(r.Context(), id); err != nil {
			writeServerError(w, err)
			return
		}
		out, locErr := snapshotServerMetrics(r.Context(), svc, id)
		// 定位类错误(凭据不存在 / 保险库未配)→ 走标准映射(422/503),而非 reachable:false。
		if locErr != nil {
			writeServerError(w, locErr)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// makeAllServerMetricsHandler 返回 GET /api/servers/metrics(认证,只读;批量)。
// 逐台并行(有界并发),各自独立:某台失败仅该台 reachable:false,不连累其它台、不 500。
// 数据走 TTL 快照缓存:热路径即时返回(可能略陈旧,collectedAt 自描述数据年龄);
// 仅冷启动(进程刚起/新登记服务器)同步采集,首屏仍给真数据。
func makeAllServerMetricsHandler(svc target.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务器服务未初始化")
			return
		}
		servers, err := svc.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}

		items := make([]serverMetricsDTO, len(servers))
		sem := make(chan struct{}, metricsConcurrency)
		var wg sync.WaitGroup
		for i, srv := range servers {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				// 批量逐台独立:定位类错误对某台亦只表现为该台 reachable:false(忽略 locErr)。
				items[i], _ = snapshotServerMetrics(r.Context(), svc, id)
			}(i, srv.ID)
		}
		wg.Wait()
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}
