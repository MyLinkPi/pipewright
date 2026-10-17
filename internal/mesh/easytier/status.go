// status.go 经 easytier-cli(白名单子进程)查询组网状态,TTL 缓存 + 后台单飞刷新。
//
// 调用姿势(PR #882 起,JSON 输出为**根级**参数,必须置于子命令之前,且新版子命令带
// 子动作):`easytier-cli --rpc-portal <addr> -o json node info`。旧版 CLI 则是参数后置
// (`node --output json`)。两侧版本都可能存在 → 每个 verb 按候选形态**自动探测**,
// 命中后缓存(参数放错位置的用法错误在 clap 下退出码恰为 2,曾让状态查询全挂)。
//
// 可诊断性:调用失败必须携带 stderr 摘要(clap/连接错误的原因都写在 stderr,不透出
// 则无从排查);对「连不上 RPC」与「版本过旧不支持 JSON」两类常见原因追加人读提示。
//
// 输出解析对 EasyTier 各版本的字段差异保持宽容:字段可能为字符串/数字/内嵌对象
// (如虚拟 IP 形如 {"addr":"10.x.x.x"}),统一经宽容取值器提取;缺字段降级为空值,
// 绝不因单个字段形状变化整体失败。p2p/中继判定:
//   - peer 表 tunnel/conn 字段直接给出 "p2p" 或 "relay(<节点>)";
//   - route 表 next_hop 交叉验证(下一跳 ≠ 目标即经中继),两者取并集展示。
package easytier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Link 是一条对等链路(集群页每节点行)。
type Link struct {
	PeerID    string  `json:"peerId"`
	VirtualIP string  `json:"virtualIp"`
	Hostname  string  `json:"hostname"`
	Version   string  `json:"version"`
	LatencyMS float64 `json:"latencyMs"`
	LossRate  string  `json:"lossRate"`
	RxBytes   uint64  `json:"rxBytes"`
	TxBytes   uint64  `json:"txBytes"`
	Tunnel    string  `json:"tunnel"`   // 原始 tunnel/conn 字段(p2p / relay(x))
	Relay     bool    `json:"relay"`    // 是否经中继
	RelayVia  string  `json:"relayVia"` // 中继节点(虚拟 IP 或 peer 标识)
}

// Status 是一次组网状态快照。
type Status struct {
	OK       bool   `json:"ok"`
	Err      string `json:"err"`
	SelfIP   string `json:"selfIp"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
	Links    []Link `json:"links"`
}

// statusTTL 是快照缓存窗口;窗口内直接回缓存,过期后台单飞刷新(stale-while-revalidate)。
const statusTTL = 10 * time.Second

// refreshTimeout 是单轮后台采集的整体超时(node/peer/route 三路 + 形态探测共用,
// 防 CLI 挂死把单飞位占死)。
const refreshTimeout = 5 * time.Second

// verbShapes 是各查询的候选调用形态(追加在 ["--rpc-portal", addr] 之后):
// 新版(PR #882+,旗子前置 + 子动作)→ 次新版(旗子前置)→ 旧版(旗子后置)。
var (
	nodeShapes = [][]string{
		{"-o", "json", "node", "info"},
		{"-o", "json", "node"},
		{"node", "--output", "json"},
	}
	peerShapes = [][]string{
		{"-o", "json", "peer", "list"},
		{"-o", "json", "peer"},
		{"peer", "--output", "json"},
	}
	routeShapes = [][]string{
		{"-o", "json", "route", "list"},
		{"-o", "json", "route"},
		{"route", "--output", "json"},
	}
)

// cliRunner 抽象一次 easytier-cli 调用(测试注入 fake;错误文本应自带 stderr 摘要)。
type cliRunner func(ctx context.Context, args []string) ([]byte, error)

// execCLIRunner 返回基于 os/exec 的实现:stderr 摘要必进错误文本(截断到一行)。
func execCLIRunner(cliPath string) cliRunner {
	return func(ctx context.Context, args []string) ([]byte, error) {
		out, err := exec.CommandContext(ctx, cliPath, args...).Output()
		if err == nil {
			return out, nil
		}
		desc := describeArgs(args)
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("easytier-cli %s: %s", desc, oneLine(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("easytier-cli %s: %v", desc, err)
	}
}

// describeArgs 取人读的动词部分(剥掉旗子与其值):如 "-o json node info" → "node info"。
func describeArgs(args []string) string {
	verbs := make([]string, 0, 4)
	skipValue := false
	for _, a := range args {
		if skipValue {
			skipValue = false
			continue
		}
		if a == "--rpc-portal" || a == "-o" || a == "--output" {
			skipValue = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		verbs = append(verbs, a)
	}
	if len(verbs) == 0 {
		return "(query)"
	}
	return strings.Join(verbs, " ")
}

// oneLine 把 stderr 压成一行并截断(错误横幅/日志友好)。
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// StatusProvider 带缓存的组网状态查询器。线程安全;零值不可用,经 NewStatusProvider 构造。
type StatusProvider struct {
	cliPath string
	rpcAddr string
	run     cliRunner

	mu         sync.Mutex
	cached     *Status
	fetched    time.Time
	refreshing bool
	// shapeByVerb 缓存每个 verb 探测成功的调用形态(verb → suffix)。
	shapeByVerb map[string][]string
}

// NewStatusProvider 构造查询器。cliPath 为 easytier-cli 路径;rpcAddr 为其 --rpc-portal 目标。
func NewStatusProvider(cliPath, rpcAddr string) *StatusProvider {
	if rpcAddr == "" {
		rpcAddr = "127.0.0.1:15888"
	}
	return &StatusProvider{
		cliPath:     cliPath,
		rpcAddr:     rpcAddr,
		run:         execCLIRunner(cliPath),
		shapeByVerb: map[string][]string{},
	}
}

// newStatusProviderForTest 用注入 runner 构造(单测;不触真机 CLI)。
func newStatusProviderForTest(run cliRunner) *StatusProvider {
	return &StatusProvider{run: run, shapeByVerb: map[string][]string{}}
}

// Get 返回状态快照(缓存窗口内即时返回;过期即触发后台单飞刷新并立即回旧值 ——
// 真后台刷新:查询不随调用方请求生死,本调用永不等待采集)。
func (p *StatusProvider) Get(ctx context.Context) *Status {
	p.mu.Lock()
	if p.cached != nil && time.Since(p.fetched) < statusTTL {
		st := p.cached
		p.mu.Unlock()
		return st
	}
	stale := p.cached
	refreshing := p.refreshing
	if !refreshing {
		p.refreshing = true
	}
	p.mu.Unlock()
	if refreshing {
		return orUnavailable(stale)
	}
	go p.refresh(ctx)
	return orUnavailable(stale)
}

// refresh 在独立 context 下采集一轮并落缓存(单飞 goroutine):
//   - context 取 WithoutCancel + refreshTimeout:HTTP 客户端断开/代理超时不再杀死
//     执行中的 easytier-cli,「context canceled」不再经请求传导进状态;
//   - 取消类失败(context canceled;Err 经错误链拼接已是文本,按子串识别)不落缓存、
//     不顶掉最后一次成功快照 —— 缓存保持过期,下次 Get 自然重试;
//   - 其余失败照常落缓存(如 refreshTimeout 的 deadline exceeded),避免坏 CLI 状态
//     被每个调用反复触发。
func (p *StatusProvider) refresh(reqCtx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), refreshTimeout)
	defer cancel()
	fresh := p.collect(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshing = false
	if strings.Contains(fresh.Err, "context canceled") {
		return
	}
	p.cached = fresh
	p.fetched = time.Now()
}

func orUnavailable(stale *Status) *Status {
	if stale != nil {
		return stale
	}
	return &Status{OK: false, Err: "status refresh in progress"}
}

// runShaped 按 verb 的候选形态执行:命中(成功且 stdout 非空)后缓存形态;
// 全部失败返回最后一次错误,并对两类常见原因追加人读提示。
func (p *StatusProvider) runShaped(ctx context.Context, verb string, shapes [][]string) ([]byte, error) {
	p.mu.Lock()
	cached := p.shapeByVerb[verb]
	p.mu.Unlock()
	candidates := [][]string{}
	if cached != nil {
		candidates = append(candidates, cached)
	}
	candidates = append(candidates, shapes...)

	base := []string{"--rpc-portal", p.rpcAddr}
	var lastErr error
	for _, suffix := range candidates {
		out, err := p.run(ctx, append(append([]string{}, base...), suffix...))
		if err == nil && len(out) > 0 {
			p.mu.Lock()
			p.shapeByVerb[verb] = suffix
			p.mu.Unlock()
			return out, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("easytier-cli %s: empty output", describeArgs(append(base, suffix...)))
		}
	}
	return nil, withHints(lastErr)
}

// withHints 对常见失败原因追加人读提示(仅依据错误文本关键字,保守匹配)。
func withHints(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connect") || strings.Contains(msg, "transport") ||
		strings.Contains(msg, "timeout") || strings.Contains(msg, "refused"):
		return fmt.Errorf("%w(可能 easytier-core 未运行,或 RPC 端口不一致)", err)
	case strings.Contains(msg, "unexpected argument") || strings.Contains(msg, "unrecognized") ||
		strings.Contains(msg, "invalid") && strings.Contains(msg, "--"):
		return fmt.Errorf("%w(easytier-cli 版本可能过旧,不支持 JSON 输出)", err)
	}
	return err
}

// collect 依次跑 node/peer/route 三条查询并归并(任一失败记入 Err,不整体失败)。
func (p *StatusProvider) collect(ctx context.Context) *Status {
	st := &Status{Links: []Link{}}
	if p.cliPath == "" && p.run == nil {
		st.Err = fmt.Sprintf("easytier-cli not found (place %s under <data>/bin or PATH, or use auto-download)", CliBinary())
		return st
	}
	// 仅 exec 路径需要 cliPath;注入 runner 的测试路径 run 已就绪。
	if p.run == nil {
		p.run = execCLIRunner(p.cliPath)
	}

	var node obj
	if raw, err := p.runShaped(ctx, "node", nodeShapes); err == nil {
		_ = json.Unmarshal(raw, &node)
		st.SelfIP = pickStr(node, "virtual_ipv4", "ipv4", "ip", "tun_ip")
		st.Hostname = pickStr(node, "hostname", "name")
		st.Version = pickStr(node, "version")
	} else {
		st.Err = err.Error()
	}

	var peers []obj
	if raw, err := p.runShaped(ctx, "peer", peerShapes); err == nil {
		_ = json.Unmarshal(raw, &peers)
	} else if st.Err == "" {
		st.Err = err.Error()
	}

	nextHops := map[string]string{}
	if raw, err := p.runShaped(ctx, "route", routeShapes); err == nil {
		var routes []obj
		if json.Unmarshal(raw, &routes) == nil {
			for _, rt := range routes {
				ip := pickStr(rt, "ipv4", "destination", "dest")
				nh := pickStr(rt, "next_hop_ipv4", "next_hop", "nexthop")
				if ip != "" {
					nextHops[ip] = nh
				}
			}
		}
	}

	for _, pr := range peers {
		l := Link{
			PeerID:    pickStr(pr, "peer_id", "peerid", "id"),
			VirtualIP: pickStr(pr, "virtual_ipv4", "ipv4", "ip"),
			Hostname:  pickStr(pr, "hostname", "name"),
			Version:   pickStr(pr, "version"),
			LatencyMS: pickNum(pr, "lat_ms", "latency", "rtt_ms"),
			LossRate:  pickStr(pr, "loss_rate", "loss_ratio"),
			RxBytes:   uint64(pickNum(pr, "rx_bytes")),
			TxBytes:   uint64(pickNum(pr, "tx_bytes")),
			Tunnel:    pickStr(pr, "tunnel", "conn_type", "tunnel_proto"),
		}
		// relay 判定:tunnel 字段形如 relay(<x>) 直接采信;否则 route 下一跳交叉验证。
		if idx := strings.Index(l.Tunnel, "relay"); idx >= 0 {
			l.Relay = true
			l.RelayVia = extractParen(l.Tunnel)
		} else if nh, ok := nextHops[l.VirtualIP]; ok && nh != "" && nh != l.VirtualIP && nh != st.SelfIP {
			l.Relay = true
			if l.RelayVia == "" {
				l.RelayVia = nh
			}
		}
		if l.VirtualIP != "" && l.VirtualIP != st.SelfIP {
			st.Links = append(st.Links, l)
		}
	}
	st.OK = st.SelfIP != "" || len(st.Links) > 0 || st.Err == ""
	return st
}

// --- 解析器(纯函数,可单测;空/格式异常一律零值,绝不 panic) ---

// obj 是宽容 JSON 对象(值可为任意形状)。
type obj map[string]any

// pickStr 从对象取第一个存在的键,宽容 string/number/内嵌 {addr:...}/数组首元素。
func pickStr(o obj, keys ...string) string {
	for _, k := range keys {
		if v, ok := o[k]; ok {
			if s := stringify(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func pickNum(o obj, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := o[k]; ok {
			switch t := v.(type) {
			case float64:
				return t
			case string:
				if f, err := strconv.ParseFloat(strings.TrimSuffix(t, "ms"), 64); err == nil {
					return f
				}
			}
		}
	}
	return 0
}

// stringify 把任意 JSON 值宽容转为字符串(对象取 addr/ipv4 键;数组取首元素)。
func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		if len(t) > 0 {
			return stringify(t[0])
		}
	case map[string]any:
		if s := stringifyAny(t["addr"]); s != "" {
			return s
		}
		if s := stringifyAny(t["ipv4"]); s != "" {
			return s
		}
		if s := stringifyAny(t["ip"]); s != "" {
			return s
		}
	}
	return ""
}

func stringifyAny(v any) string {
	if v == nil {
		return ""
	}
	return stringify(v)
}

// extractParen 提取 "relay(xxx)" 括号内的中继标识。
func extractParen(s string) string {
	if i := strings.IndexByte(s, '('); i >= 0 {
		if j := strings.IndexByte(s[i:], ')'); j > 1 {
			return s[i+1 : i+j]
		}
	}
	return ""
}
