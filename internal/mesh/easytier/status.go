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
//
// 同网段多通告者(互为备份):route 表每个通告节点一行,ProxyRoutes 按网段收录全部
// 网关;EasyTier 数据面对每网段择优单网关、失联秒级切换(实测 2.6.4),平台不干预。
package easytier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"sort"
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

// ProxyRouteVia 是某网段的一个通告网关(谁通告谁网关)。Relay=true 表示本机到达该
// 网关需经中继(RelayVia 为中继节点虚拟 IP;直连为空)。
type ProxyRouteVia struct {
	IP       string `json:"ip"`
	Name     string `json:"name"`
	Relay    bool   `json:"relay"`
	RelayVia string `json:"relayVia"`
}

// ProxyRoute 是一条子网代理通告。多节点可通告同一网段互为备份:EasyTier 数据面对每
// 网段择优单一网关(跳数最短优先),该网关失联后自动切换其余通告者(实测秒级,见
// README 集群章节)。ViaIP/ViaName 为主网关 —— route 表不暴露本机真实择优结果,此处
// 按 直连优先→path_len 小→IP 字典序 的确定性近似;Vias 为全部通告网关(含主,序同上)。
type ProxyRoute struct {
	CIDR    string          `json:"cidr"`
	ViaIP   string          `json:"viaIp"`
	ViaName string          `json:"viaName"`
	Vias    []ProxyRouteVia `json:"vias"`
}

// Status 是一次组网状态快照。
type Status struct {
	OK       bool   `json:"ok"`
	Err      string `json:"err"`
	SelfIP   string `json:"selfIp"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
	Links    []Link `json:"links"`
	// ProxyCIDRs 是全网通告的子网代理网段(route 表 proxy_cidrs 汇总;去重排序)。
	// 客户端据此展示「可路由的远端网段」清单;平台侧集群页亦可展示。
	ProxyCIDRs []string `json:"proxyCidrs"`
	// ProxyRoutes 是 网段→网关节点 映射(客户端展示「路由到哪去」;无通告时为空数组)。
	// 同网段多通告者(互为备份)时 Vias 收录全部网关,ViaIP/ViaName 为主网关。
	ProxyRoutes []ProxyRoute `json:"proxyRoutes"`
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

// rpcListenProbe 是连接类失败时的 RPC 口可达性实测(默认 RPCAddrReachable;测试注入)。
var rpcListenProbe func(addr string) error = RPCAddrReachable

// execCLIRunner 返回基于 os/exec 的实现:每次调用的完整命令行与 stdout/stderr 都进
// 日志(pwclient log / 平台日志,前缀 easytier-cli);stderr 摘要必进错误文本
// (clap/连接错误的原因都写在 stderr,不透出则无从排查)。
// CLI 命令行本身不含密钥类参数(敏感值只在 core 侧,落日志前经 maskArgs 打码)。
func execCLIRunner(cliPath string) cliRunner {
	return func(ctx context.Context, args []string) ([]byte, error) {
		log.Printf("easytier-cli: 执行 %s", strings.Join(append([]string{cliPath}, args...), " "))
		out, err := exec.CommandContext(ctx, cliPath, args...).Output()
		if err == nil {
			log.Printf("easytier-cli: 输出(%s): %s", describeArgs(args), abbrevOutput(string(out)))
			return out, nil
		}
		desc := describeArgs(args)
		errText := err.Error()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if len(ee.Stderr) > 0 {
				log.Printf("easytier-cli: stderr(%s): %s", desc, abbrevOutput(string(ee.Stderr)))
				errText += " " + string(ee.Stderr)
			}
			if len(ee.Stderr) > 0 {
				// 连接类失败(stderr 全文参与判定)附带 RPC 口实测,排查 core 端口是否开放。
				probeRPCIfNeeded(errText, args)
				return nil, fmt.Errorf("easytier-cli %s: %s", desc, oneLine(string(ee.Stderr)))
			}
		}
		probeRPCIfNeeded(errText, args)
		return nil, fmt.Errorf("easytier-cli %s: %v", desc, err)
	}
}

// probeRPCIfNeeded 错误文本呈连接类特征(Connect refused / timed out 等)时,
// 对 --rpc-portal 目标做一次 TCP 实测并落日志(开放与否直接给出结论,免猜)。
func probeRPCIfNeeded(errText string, args []string) {
	if rpcListenProbe == nil {
		return
	}
	lower := strings.ToLower(errText)
	if !strings.Contains(lower, "connect") && !strings.Contains(lower, "refused") && !strings.Contains(lower, "timeout") {
		return
	}
	addr := rpcAddrOfArgs(args)
	if err := rpcListenProbe(addr); err != nil {
		log.Printf("easytier-cli: RPC 端口可达性实测 %s: 未开放(%v)", addr, err)
		return
	}
	log.Printf("easytier-cli: RPC 端口可达性实测 %s: 端口可连(CLI 仍连不上多为 core RPC 服务未就绪,稍后自动恢复)", addr)
}

// rpcAddrOfArgs 从 CLI 参数里取 --rpc-portal 的值(缺省按平台默认口)。
func rpcAddrOfArgs(args []string) string {
	for i, a := range args {
		if a == "--rpc-portal" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return "127.0.0.1:15888"
}

// abbrevOutput 把子进程输出压成单行进日志(换行→空格;超长截断保尾部 ——
// 错误信息通常写在输出末尾)。
func abbrevOutput(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const cap = 4096
	if len(s) > cap {
		return "…(前文省略)" + s[len(s)-cap:]
	}
	return s
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

// runShaped 按 verb 的候选形态执行(缓存的命中形态优先,其余新→旧;与缓存重复的
// 候选去重):命中(成功且 stdout 非空)后缓存形态。全部失败时按「运行类错误优先」
// 择因外报:新版 CLI 上旧形态的 clap 用法错误是探测期必然出现的噪声,若作为最终
// 错误,瞬时的 RPC 未就绪(core 重启窗口)会被误报成「easytier-cli 版本过旧」;
// 参数类错误仅在没有任何运行类错误时才作为结论。
func (p *StatusProvider) runShaped(ctx context.Context, verb string, shapes [][]string) ([]byte, error) {
	p.mu.Lock()
	cached := p.shapeByVerb[verb]
	p.mu.Unlock()
	candidates := [][]string{}
	if cached != nil {
		candidates = append(candidates, cached)
	}
	for _, s := range shapes {
		if !equalArgs(s, cached) {
			candidates = append(candidates, s)
		}
	}

	base := []string{"--rpc-portal", p.rpcAddr}
	var runErr, argErr error
	for _, suffix := range candidates {
		out, err := p.run(ctx, append(append([]string{}, base...), suffix...))
		if err == nil && len(out) > 0 {
			p.mu.Lock()
			p.shapeByVerb[verb] = suffix
			p.mu.Unlock()
			return out, nil
		}
		if err == nil {
			err = fmt.Errorf("easytier-cli %s: empty output", describeArgs(append(base, suffix...)))
		}
		if isArgError(err) {
			if argErr == nil {
				argErr = err
			}
		} else if runErr == nil {
			runErr = err
		}
	}
	if runErr != nil {
		return nil, withHints(runErr)
	}
	return nil, withHints(argErr)
}

// isArgError 判定是否 clap 参数类错误(当前 CLI 版本不支持该调用形态)。
func isArgError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unexpected argument") || strings.Contains(msg, "unrecognized") ||
		strings.Contains(msg, "invalid") && strings.Contains(msg, "--")
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
	case isArgError(err):
		return fmt.Errorf("%w(easytier-cli 版本可能过旧,不支持 JSON 输出)", err)
	}
	return err
}

// equalArgs 比较两个参数切片是否相同(候选去重用)。
func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// collect 依次跑 node/peer/route 三条查询并归并(任一失败记入 Err,不整体失败)。
func (p *StatusProvider) collect(ctx context.Context) *Status {
	st := &Status{Links: []Link{}, ProxyCIDRs: []string{}, ProxyRoutes: []ProxyRoute{}}
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
		st.SelfIP = pickIP(node, "ipv4_addr", "virtual_ipv4", "ipv4", "ip", "tun_ip")
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
	proxySeen := map[string]struct{}{}
	// routeRow 是 route 表原始行的最小快照;通告者身份判别延迟到 peer 表解析后
	//(需要 peer IP 集合区分两种行形态,见下方 cidrVias 组装)。
	type routeRow struct {
		ip, nh, hostname string
		cidrs            []string
		pathLen          float64
	}
	var rows []routeRow
	if raw, err := p.runShaped(ctx, "route", routeShapes); err == nil {
		var routes []obj
		if json.Unmarshal(raw, &routes) == nil {
			for _, rt := range routes {
				ip := pickIP(rt, "ipv4", "destination", "dest")
				nh := stripCIDR(pickStr(rt, "next_hop_ipv4", "next_hop", "nexthop"))
				// 直连行占位归零:旧版输出 "-",v2.6.4+ 为 "DIRECT"(cost==1)。
				// 漏认会把字面量当中继节点标识,直连链路被误标「经 DIRECT 中继」。
				if nh == "-" || strings.EqualFold(nh, "direct") {
					nh = ""
				}
				if ip != "" {
					nextHops[ip] = nh
				}
				if len(pickList(rt, "proxy_cidrs")) == 0 {
					continue
				}
				rows = append(rows, routeRow{
					ip:       ip,
					nh:       nh,
					hostname: pickStr(rt, "hostname"),
					cidrs:    pickList(rt, "proxy_cidrs"),
					pathLen:  pickNum(rt, "path_len"),
				})
				for _, c := range pickList(rt, "proxy_cidrs") {
					if c != "" {
						proxySeen[c] = struct{}{}
					}
				}
			}
		}
	}
	st.ProxyCIDRs = sortedSet(proxySeen)

	for _, pr := range peers {
		l := Link{
			PeerID:    pickStr(pr, "peer_id", "peerid", "id"),
			VirtualIP: pickIP(pr, "virtual_ipv4", "ipv4", "ip"),
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
	// ProxyRoutes:网段→网关列表(用 peer 主机名补全展示名;无通告时空数组)。
	nameByIP := make(map[string]string, len(st.Links))
	peerIPs := make(map[string]struct{}, len(st.Links))
	for _, l := range st.Links {
		if l.VirtualIP == "" {
			continue
		}
		peerIPs[l.VirtualIP] = struct{}{}
		if l.Hostname != "" {
			nameByIP[l.VirtualIP] = l.Hostname
		}
	}
	// cidrVias:网段 → 通告网关候选(同网段多通告者全收 —— 互为备份,EasyTier 数据面
	// 择优单一网关、失联自动切换,此处不再首见丢弃)。
	type viaCand struct {
		ProxyRouteVia
		pathLen float64
	}
	cidrVias := map[string][]viaCand{}
	hostnameFallback := map[string]string{}
	for _, row := range rows {
		if row.ip == "" {
			continue
		}
		// 通告者身份按行形态判别:ipv4 是 peer(新版目的节点行)→ 通告者 = ipv4,
		// next_hop 非空则为到达它的中继;ipv4 非 peer(旧版行,ipv4 为被代理网段内
		// 地址)→ 通告者 = next_hop。本机通告行跳过(客户端不给自己当网关)。
		adv, relayVia := row.ip, ""
		if row.nh != "" && row.nh != row.ip && row.nh != st.SelfIP {
			if _, ok := peerIPs[row.ip]; !ok {
				adv = row.nh
			} else {
				relayVia = row.nh
			}
		}
		if adv == "" || adv == st.SelfIP {
			continue
		}
		if row.hostname != "" {
			hostnameFallback[adv] = row.hostname
		}
		for _, c := range row.cidrs {
			if c == "" {
				continue
			}
			cidrVias[c] = append(cidrVias[c], viaCand{
				ProxyRouteVia: ProxyRouteVia{IP: adv, Relay: relayVia != "", RelayVia: relayVia},
				pathLen:       row.pathLen,
			})
		}
	}
	for _, c := range st.ProxyCIDRs {
		cands := cidrVias[c]
		if len(cands) == 0 {
			continue
		}
		// 主网关排序:直连优先 → path_len 小 → IP 字典序(route 表不暴露本机真实
		// 择优结果,这是展示层的确定性近似);同一网关多行去重。
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].Relay != cands[j].Relay {
				return !cands[i].Relay
			}
			if cands[i].pathLen != cands[j].pathLen {
				return cands[i].pathLen < cands[j].pathLen
			}
			return cands[i].IP < cands[j].IP
		})
		vias := make([]ProxyRouteVia, 0, len(cands))
		seenIP := map[string]struct{}{}
		for _, cd := range cands {
			if _, dup := seenIP[cd.IP]; dup {
				continue
			}
			seenIP[cd.IP] = struct{}{}
			if cd.Name == "" {
				cd.Name = nameByIP[cd.IP]
			}
			if cd.Name == "" {
				cd.Name = hostnameFallback[cd.IP]
			}
			vias = append(vias, cd.ProxyRouteVia)
		}
		st.ProxyRoutes = append(st.ProxyRoutes, ProxyRoute{
			CIDR: c, ViaIP: vias[0].IP, ViaName: vias[0].Name, Vias: vias,
		})
	}
	if st.ProxyRoutes == nil {
		st.ProxyRoutes = []ProxyRoute{}
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
				if f, err := strconv.ParseFloat(leadingNumber(t), 64); err == nil {
					return f
				}
			}
		}
	}
	return 0
}

// pickList 取第一个存在的键,归一为字符串列表。真实输出(easytier v2.6 实测)该类
// 字段多为逗号分隔字符串("a,b"),数组形态作为宽容保留;数组元素可为 string 或
// {"addr": ...} 对象。
func pickList(o obj, keys ...string) []string {
	for _, k := range keys {
		if v, ok := o[k]; ok {
			var items []any
			switch t := v.(type) {
			case string:
				for _, f := range strings.Split(t, ",") {
					if f = strings.TrimSpace(f); f != "" {
						items = append(items, f)
					}
				}
			case []any:
				items = t
			default:
				continue
			}
			out := make([]string, 0, len(items))
			for _, item := range items {
				if m, ok := item.(map[string]any); ok { // {"addr": ...} 对象形态
					if s := stringify(m["addr"]); s != "" {
						out = append(out, s)
					}
					continue
				}
				if s := stringify(item); s != "" {
					out = append(out, s)
				}
			}
			return out
		}
	}
	return nil
}

// sortedSet 把集合按键序输出为切片(状态展示稳定有序)。
func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stringify 把任意 JSON 值宽容转为字符串(对象取 addr/ipv4 键;数组取首元素)。
// leadingNumber 取字符串前导数字部分("0.12"→"0.12";"1.81 kB"→"1.81";"-"→"")。
func leadingNumber(s string) string {
	s = strings.TrimSpace(s)
	end := 0
	for i, r := range s {
		if (r >= '0' && r <= '9') || r == '.' || (i == 0 && r == '-') {
			end = i + len(string(r))
			continue
		}
		break
	}
	return s[:end]
}

// stripCIDR 剥去 IP 的 /prefix 后缀("10.1.1.1/24"→"10.1.1.1";裸 IP 原样)。
func stripCIDR(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i]
	}
	return s
}

// pickIP 取 IP 字段并剥 /prefix(node/peer/route 的 ipv4 字段实测带后缀或裸 IP 两种)。
func pickIP(o obj, keys ...string) string {
	return stripCIDR(pickStr(o, keys...))
}

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
