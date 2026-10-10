package deploy

// host_load.go 实现「部署目标数量上限(maxTargets)」:标签选择器命中的机器数超过上限时,
// 按**主机实时负载**(CPU 负载率、内存使用率,取高者为饱和度)圈选「最空」的 N 台部署,
// 其余机器本次不部署(不预检、不落 deploy_targets、不进注册表保留集)。
//
// 采集形态与 Story 6.1 服务器指标同一纪律:一条**纯静态** sh -c 脚本(`##PW:` 分段标记)经
// target.Exec 单次 SSH 采 loadavg / cores / free,绝不接受任何用户输入拼接(AC-SEC-02)。
//
// 语义边界:
//   - 只对**标签选择器圈选**结果生效(显式 serverIDs 的回滚回放 / 手工部署 API 不经过此处);
//   - limit ≤ 0 或命中数 ≤ limit → 原样返回(零探测开销,行为与现状逐字节一致);
//   - 探测失败(不可达 / 输出不可解析 / 两维皆取不到)的机器排到最后 —— 宁可不用「确认不了
//     负载」的机器,也不把容器推到实际很满的机器上;单维取不到时用可得维度(部分可用仍算成功);
//   - 负载快照只在选机这一刻取一次;裁切后交给 deployRolling,滚动期间不再重新探测/换机
//     (选机与滚动是两步:上限决定「哪 N 台进入」,firstBatchSize/batchSize 决定「N 台内推进顺序」)。

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// hostLoadProbeTimeout 是单台负载探测的整体超时(拨号 + 握手 + 1 条脚本;对齐 Story 6.1)。
const hostLoadProbeTimeout = 10 * time.Second

// ─── 采集脚本(AC-SEC-02:纯静态文本,绝不含任何用户输入)──────────────────────
//
// 各段以 ##PW:<名> 标记行分隔;段空/格式异常 → 该维度不可用(两维皆不可用 = 探测失败)。
//   - loadavg:`cat /proc/loadavg`(Linux);macOS 无 /proc → `|| uptime` 回退,解析两种格式。
//   - cores:`nproc`;缺失回退 `getconf _NPROCESSORS_ONLN`(跨平台,含 macOS)。
//   - memory:`free -b`(取 Mem 行 total/used,used 为**不含可回收页缓存**口径 = 真实内存压力)。
//   - end:收尾标记,保证脚本恒以 0 退出(段命令失败不影响整体执行)。
const loadMarker = "##PW:"

const (
	loadSecLoadavg = "loadavg"
	loadSecCores   = "cores"
	loadSecMemory  = "memory"
	loadSecEnd     = "end"
)

// hostLoadArgs 返回单台一次性负载采集命令(sh -c 脚本;sh 在 Linux/macOS 恒在,与
// Story 6.1 metricsCollectArgs / build.DetectRemoteCLI 同款做法)。
func hostLoadArgs() []string {
	parts := []string{
		`echo "` + loadMarker + loadSecLoadavg + `"`,
		`cat /proc/loadavg 2>/dev/null || uptime`,
		`echo "` + loadMarker + loadSecCores + `"`,
		`nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null`,
		`echo "` + loadMarker + loadSecMemory + `"`,
		`free -b 2>/dev/null`,
		`echo "` + loadMarker + loadSecEnd + `"`,
	}
	return []string{"sh", "-c", strings.Join(parts, "; ")}
}

// splitLoadSections 按 `##PW:<名>` 标记行切分脚本 stdout(段名 → 段文本;标记后无输出即空段)。
// 首个标记之前的内容丢弃(正常不该出现)。
func splitLoadSections(out string) map[string]string {
	sections := map[string]string{}
	cur := ""
	var buf strings.Builder
	flush := func() {
		if cur != "" {
			sections[cur] = buf.String()
		}
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, loadMarker) {
			flush()
			cur = strings.TrimSpace(strings.TrimPrefix(line, loadMarker))
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

// parseLoadavg1 解析 1 分钟平均负载。先按 /proc/loadavg 首字段取值,不匹配再按 uptime 的
// 「load average: 0.52, 0.58, 0.59」取第一个数(macOS 回退)。负值视为不可用。
func parseLoadavg1(sec string) (float64, bool) {
	line := strings.TrimSpace(firstLine(sec))
	if line == "" {
		return 0, false
	}
	if f := strings.Fields(line); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil && v >= 0 {
			return v, true
		}
	}
	i := strings.Index(line, "load average:")
	if i < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(line[i+len("load average:"):])
	head := strings.FieldsFunc(rest, func(r rune) bool { return r == ',' || r == ' ' })
	if len(head) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(head[0], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// parseCores 解析核数段(首字段整数);≤0 视为不可用。
func parseCores(sec string) (int, bool) {
	f := strings.Fields(firstLine(sec))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.Atoi(f[0])
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// parseMemRatio 解析 `free -b` 的 Mem 行 → used/total(0..1+)。无 Mem 行 / 字段不足 /
// total ≤ 0 → 不可用。used 取第二列(不含可回收页缓存,与 Story 6.1 UsedBytes 同口径)。
func parseMemRatio(sec string) (float64, bool) {
	for _, line := range strings.Split(sec, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.EqualFold(strings.TrimSuffix(f[0], ":"), "Mem") {
			continue
		}
		total, err1 := strconv.ParseInt(f[1], 10, 64)
		used, err2 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil || total <= 0 || used < 0 {
			return 0, false
		}
		return float64(used) / float64(total), true
	}
	return 0, false
}

// firstLine 返回文本首个非空行(全空 → "")。
func firstLine(sec string) string {
	for _, l := range strings.Split(sec, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

// hostLoad 是一台机器的两维负载比率(0 = 完全空闲,1 = 打满,可 > 1 表示超载)。
// 任一维度采集/解析失败 → 该维不可用(cpuOK/memOK = false)。
type hostLoad struct {
	cpuRatio float64
	memRatio float64
	cpuOK    bool
	memOK    bool
}

// fullness 返回饱和度 = **可得维度里的最高值**(任一资源接近打满即算「不空」,保守)。
// 两维皆不可得 → ok=false(调用方把该机排到最后)。
func (h hostLoad) fullness() (float64, bool) {
	switch {
	case h.cpuOK && h.memOK:
		return math.Max(h.cpuRatio, h.memRatio), true
	case h.cpuOK:
		return h.cpuRatio, true
	case h.memOK:
		return h.memRatio, true
	default:
		return 0, false
	}
}

// parseHostLoad 解析负载采集脚本的 stdout → hostLoad(纯函数,便于单测)。
func parseHostLoad(out string) hostLoad {
	sec := splitLoadSections(out)
	var h hostLoad
	if cores, ok := parseCores(sec[loadSecCores]); ok {
		if la, lok := parseLoadavg1(sec[loadSecLoadavg]); lok {
			h.cpuRatio, h.cpuOK = la/float64(cores), true
		}
	}
	if mr, ok := parseMemRatio(sec[loadSecMemory]); ok {
		h.memRatio, h.memOK = mr, true
	}
	return h
}

// probeHostLoad 经单次 SSH 采集一台机器的实时负载。Exec 失败 / 命令非零退出 / 两维皆
// 不可解析 → ok=false(调用方按「探测失败」排到最后,绝不把未知负载当作 0 抢先用它)。
func (s *service) probeHostLoad(ctx context.Context, serverID string) (hostLoad, bool) {
	cctx, cancel := context.WithTimeout(ctx, hostLoadProbeTimeout)
	defer cancel()
	res, err := s.targets.Exec(cctx, serverID, hostLoadArgs())
	if err != nil || res == nil || res.ExitCode != 0 {
		return hostLoad{}, false
	}
	h := parseHostLoad(res.Stdout)
	if _, ok := h.fullness(); !ok {
		return h, false
	}
	return h, true
}

// maxTargetsLimit 解析目标数量上限(cfg["maxTargets"];空 / 非整数 / ≤0 → 0 = 不限)。
// 与 rollingBatchSize 同款容错:配置写错不阻断流水线,只是不生效。
func maxTargetsLimit(cfg map[string]string) int {
	raw := strings.TrimSpace(cfg["maxTargets"])
	if raw == "" {
		return 0
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// limitServersByLoad 按主机负载把圈选结果裁到 limit 台(见文件头语义)。
//
// 返回**饱和度升序**的前 limit 台(首批 firstBatchSize 默认 1 → 先打最空的一台);
// 未入选的机器直接从集合丢弃,后续滚动/持久化/注册表清理都只看这 limit 台。
// limit ≤ 0 或命中数 ≤ limit → 原样返回(一台都不探测,零额外开销)。
func (s *service) limitServersByLoad(ctx context.Context, servers []*target.Server, limit int) []*target.Server {
	if limit <= 0 || len(servers) <= limit {
		return servers
	}
	// 1) 有界并发逐台探测(forEachServer 各 goroutine 只写自己的下标槽,无竞争)。
	load := make([]hostLoad, len(servers))
	ok := make([]bool, len(servers))
	full := make([]float64, len(servers))
	s.forEachServer(servers, func(idx int, srv *target.Server) {
		h, hk := s.probeHostLoad(ctx, srv.ID)
		load[idx], ok[idx] = h, hk
	}, func(idx int, _ *target.Server) {
		ok[idx] = false // panic 兜底:按「探测失败」处理,排最后
	})
	for i := range load {
		if f, fk := load[i].fullness(); fk {
			full[i] = f
		}
	}

	// 2) 稳定排序:探测成功的按饱和度升序,失败的排最后(两类各自保持圈选原序)。
	order := make([]int, len(servers))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if ok[ia] != ok[ib] {
			return ok[ia] // 可确认负载的排前面
		}
		if !ok[ia] {
			return false // 都不可确认 → 保序
		}
		return full[ia] < full[ib]
	})

	// 3) 一行人读日志:选中/未选中都列出读数,让「为什么是这 N 台」在步骤日志里可核对。
	cmdLogFrom(ctx)(cmdStreamStdout, "", fmt.Sprintf(
		"· 目标数量上限 %d:选择器命中 %d 台,按主机负载(CPU 负载率/内存使用率取高者)选取最空的 %d 台 → %s;本次不部署:%s",
		limit, len(servers), limit, renderLoad(servers, ok, full, order[:limit]), renderLoad(servers, ok, full, order[limit:])))

	out := make([]*target.Server, 0, limit)
	for _, i := range order[:limit] {
		out = append(out, servers[i])
	}
	return out
}

// renderLoad 把下标集合渲染为「名字(0.42)」串;探测失败者标「负载未知」。
func renderLoad(servers []*target.Server, ok []bool, full []float64, idxs []int) string {
	if len(idxs) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(idxs))
	for _, i := range idxs {
		if !ok[i] {
			parts = append(parts, servers[i].Name+"(负载未知)")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s(%.2f)", servers[i].Name, full[i]))
	}
	return strings.Join(parts, ", ")
}
