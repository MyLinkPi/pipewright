// collect.go — 平台周期采样:一条合并 SSH 脚本拉机器侧 ssar 的最新快照并解析入库。
//
// 采集形态(与 6-1 metricsCollectArgs 同款纪律):每台**一次** SSH 连接跑一个合并
// sh -c 脚本,##PW: 分段,各段跑 `ssar --<view> --api -r 3 -P` 取**最近 3 分钟**历史
// (默认 60s 步长 → 2-3 行)—— 由机器侧 sresar 的本地采集供数,平台只读不扰;
// `-P` 丢弃含占位符的行(刚重启/首采未满一个区间的行),解析层取**尾行**(最新)。
//
// 视图与入库字段:
//   - --cpu:user/system/iowait/idle/steal(jiffies/s,全 CPU 汇总口径)
//   - --mem:memtotal/memfree/memavailable(kB)
//   - --tcp:currestab(瞬时连接数)+ retranssegs(每秒重传段数)
//   - load5s 子命令:load1/runq/threads(5s 粒度负载;无 -P 支持,占位符在解析层消化)
//
// 时间口径:入库时间用 pipewright 采样时刻(At),不采信 ssar 行内的 collect_datetime
// (机器本地时区,跨时区机器会错乱)。未装 ssar / sresar 未采到数据 → 各段为空 →
// 全字段 nil → 该台本轮跳过(不报错)。命令纯静态(AC-SEC-02)。
package ssar

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 采样脚本段名。
const (
	secCPU    = "cpu"
	secMem    = "mem"
	secTCP    = "tcp"
	secLoad5s = "load5s"
	secCEnd   = "cend"
)

// collectTimeout 是单台一轮合并采集的整体超时(拨号 + 5 条 ssar 查询)。
const collectTimeout = 30 * time.Second

// collectConcurrency 是批量采样 / 安装的最大并发 SSH 数(有界,防打爆)。
const collectConcurrency = 6

// ssarCollectArgs 返回单台一次性采样命令(sh -c 静态脚本)。
// 各 ssar 命令 stderr 丢弃(无数据时 ssar 打 "Exception: Data Paths …" 到输出,
// 噪声无害但无用);命令缺失(command not found)亦只让该段为空。
func ssarCollectArgs() []string {
	parts := []string{
		`echo "` + ssarMarker + secCPU + `"`,
		`ssar --cpu --api -r 3 -P 2>/dev/null`,
		`echo "` + ssarMarker + secMem + `"`,
		`ssar --mem --api -r 3 -P 2>/dev/null`,
		`echo "` + ssarMarker + secTCP + `"`,
		`ssar --tcp --api -r 3 -P 2>/dev/null`,
		`echo "` + ssarMarker + secRetranSection + `"`,
		`ssar --retran --api -r 3 -P 2>/dev/null`,
		`echo "` + ssarMarker + secLoad5s + `"`,
		`ssar load5s --api -r 3 2>/dev/null`,
		`echo "` + ssarMarker + secCEnd + `"`,
	}
	return []string{"sh", "-c", strings.Join(parts, "; ")}
}

// SampleAll 对全部登记服务器各跑一轮合并采集并入库,返回入库样本数。
// 逐台并行(有界并发),各自独立:不可达 / 未装 ssar / 全字段缺失 → 该台跳过,不报错。
func (s *service) SampleAll(ctx context.Context) (int, error) {
	servers, err := s.servers.List(ctx)
	if err != nil {
		return 0, err
	}
	at := time.Now().UTC()
	samples := make([]*Sample, len(servers))
	sem := make(chan struct{}, collectConcurrency)
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			samples[i] = s.collectOne(ctx, id, at)
		}(i, srv.ID)
	}
	wg.Wait()

	record := make([]Sample, 0, len(samples))
	for _, sm := range samples {
		if sm != nil {
			record = append(record, *sm)
		}
	}
	if len(record) == 0 {
		return 0, nil
	}
	if err := s.record(ctx, record); err != nil {
		return 0, err
	}
	return len(record), nil
}

// collectOne 采集单台(一次 SSH 跑合并脚本)。不可达 / 全字段缺失 → nil(该台跳过)。
func (s *service) collectOne(ctx context.Context, serverID string, at time.Time) *Sample {
	cctx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()
	res, err := s.runner.RunPlain(cctx, serverID, ssarCollectArgs())
	if err != nil && res == nil {
		return nil // 连接失败:跳过该台(下一轮自然重试)
	}
	out := res.Stdout
	if len(out) > 512*1024 {
		out = out[:512*1024] // 防超大输出(正常 5 段 JSON 各一行,余量充足)
	}
	sm := sampleFromSections(serverID, at, splitSections(out))
	if !sampleHasAny(sm) {
		return nil // 未装 ssar / 无数据:本轮跳过
	}
	return sm
}

// sampleFromSections 把分段 stdout 解析为 Sample(各视图独立:某段缺失/解析失败 →
// 该组字段 nil,不影响其它组)。纯函数,可单测。
func sampleFromSections(serverID string, at time.Time, sections map[string]string) *Sample {
	sm := &Sample{ServerID: serverID, At: at}
	if row, ok := lastJSONRow(sections[secCPU]); ok {
		sm.CPUUser = ssarNum(row["user"])
		sm.CPUSystem = ssarNum(row["system"])
		sm.CPUIowait = ssarNum(row["iowait"])
		sm.CPUIdle = ssarNum(row["idle"])
		sm.CPUSteal = ssarNum(row["steal"])
	}
	if row, ok := lastJSONRow(sections[secMem]); ok {
		sm.MemTotalKB = ssarNum(row["memtotal"])
		sm.MemFreeKB = ssarNum(row["memfree"])
		sm.MemAvailKB = ssarNum(row["memavailable"])
	}
	if row, ok := lastJSONRow(sections[secTCP]); ok {
		sm.TCPCurrEstab = ssarNum(row["currestab"])
		sm.TCPRetransSegs = ssarNum(row["retranssegs"])
	}
	if row, ok := lastJSONRow(sections[secRetranSection]); ok {
		sm.TCPSynRetrans = ssarNum(row["synretrans"])
		sm.TCPFastRetrans = ssarNum(row["fastretrans"])
		sm.TCPRetransFail = ssarNum(row["retransfail"])
	}
	if row, ok := lastJSONRow(sections[secLoad5s]); ok {
		sm.Load1 = ssarNum(row["load1"])
		sm.Runq = ssarNum(row["runq"])
		sm.Threads = ssarNum(row["threads"])
	}
	return sm
}

// secRetranSection 是 retran 段名(synretrans/fastretrans/retransfail 等重传细分)。
const secRetranSection = "retran"

// sampleHasAny 报告样本是否至少有一个可采字段(全 nil → 跳过入库)。
func sampleHasAny(sm *Sample) bool {
	if sm == nil {
		return false
	}
	vals := []*float64{sm.CPUUser, sm.CPUSystem, sm.CPUIowait, sm.CPUIdle, sm.CPUSteal,
		sm.MemTotalKB, sm.MemFreeKB, sm.MemAvailKB,
		sm.Load1, sm.Runq, sm.Threads,
		sm.TCPCurrEstab, sm.TCPRetransSegs, sm.TCPSynRetrans, sm.TCPFastRetrans, sm.TCPRetransFail}
	for _, v := range vals {
		if v != nil {
			return true
		}
	}
	return false
}

// lastJSONRow 解析一段 `ssar --api` 输出(JSON 数组,元素为扁平字符串 map)并取**尾行**
// (最新样本)。先整串解析(常态:输出就是单行 JSON);失败再取首个 `[` 到末个 `]`
// 之间的片段重试 —— 容忍命令在 JSON 前后夹带的提示文本。空 / 无数组 / 解析失败 /
// 0 行 → false。
func lastJSONRow(section string) (map[string]string, bool) {
	t := strings.TrimSpace(section)
	if t == "" {
		return nil, false
	}
	var rows []map[string]string
	if err := json.Unmarshal([]byte(t), &rows); err != nil {
		start := strings.Index(t, "[")
		end := strings.LastIndex(t, "]")
		if start < 0 || end <= start {
			return nil, false
		}
		if err := json.Unmarshal([]byte(t[start:end+1]), &rows); err != nil {
			return nil, false
		}
	}
	if len(rows) == 0 {
		return nil, false
	}
	return rows[len(rows)-1], true
}

// ssarNum 把 ssar --api 的字符串值解析为 float。占位符("-" 无数据 / "*" 重启边界 /
// "<" ">" 进程区间界)与空串 / 非数值 → nil;带 "%" 后缀的比率值剥后缀解析
// (load5s 的 act_rto 等列;入库字段本身不带该形态,容错统一处理)。
func ssarNum(s string) *float64 {
	t := strings.TrimSpace(s)
	if t == "" || t == "-" || t == "*" || t == "<" || t == ">" {
		return nil
	}
	t = strings.TrimSuffix(t, "%")
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}
