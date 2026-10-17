// status.go 经 easytier-cli(白名单子进程)查询组网状态,TTL 缓存 + 后台单飞刷新。
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
	Tunnel    string  `json:"tunnel"`  // 原始 tunnel/conn 字段(p2p / relay(x))
	Relay     bool    `json:"relay"`   // 是否经中继
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

// StatusProvider 带缓存的组网状态查询器。线程安全;零值不可用,经 NewStatusProvider 构造。
type StatusProvider struct {
	cliPath string
	rpcAddr string

	mu       sync.Mutex
	cached   *Status
	fetched  time.Time
	refreshing bool
}

// NewStatusProvider 构造查询器。cliPath 为 easytier-cli 路径;rpcAddr 为其 --rpc-portal 目标。
func NewStatusProvider(cliPath, rpcAddr string) *StatusProvider {
	if rpcAddr == "" {
		rpcAddr = "127.0.0.1:15888"
	}
	return &StatusProvider{cliPath: cliPath, rpcAddr: rpcAddr}
}

// Get 返回状态快照(缓存窗口内即时返回;过期触发后台刷新并回旧值)。
func (p *StatusProvider) Get(ctx context.Context) *Status {
	p.mu.Lock()
	if p.cached != nil && time.Since(p.fetched) < statusTTL {
		st := p.cached
		p.mu.Unlock()
		return st
	}
	stale := p.cached
	refreshing := p.refreshing
	p.refreshing = true
	p.mu.Unlock()

	if refreshing {
		return orUnavailable(stale)
	}
	fresh := collectStatus(ctx, p.cliPath, p.rpcAddr)
	p.mu.Lock()
	p.cached = fresh
	p.fetched = time.Now()
	p.refreshing = false
	p.mu.Unlock()
	return fresh
}

func orUnavailable(stale *Status) *Status {
	if stale != nil {
		return stale
	}
	return &Status{OK: false, Err: "status refresh in progress"}
}

// collectStatus 依次跑 node/peer/route 三条 CLI 查询并归并(任一失败记入 Err,不整体失败)。
func collectStatus(ctx context.Context, cliPath, rpcAddr string) *Status {
	st := &Status{Links: []Link{}}
	if cliPath == "" {
		st.Err = fmt.Sprintf("easytier-cli not found (place %s under <data>/bin or PATH)", CliBinary())
		return st
	}
	base := []string{"--rpc-portal", rpcAddr}

	var node obj
	if raw, err := runCLI(ctx, cliPath, append(base, "node", "--output", "json")...); err == nil {
		_ = json.Unmarshal(raw, &node)
		st.SelfIP = pickStr(node, "virtual_ipv4", "ipv4", "ip", "tun_ip")
		st.Hostname = pickStr(node, "hostname", "name")
		st.Version = pickStr(node, "version")
	} else {
		st.Err = err.Error()
	}

	var peers []obj
	if raw, err := runCLI(ctx, cliPath, append(base, "peer", "--output", "json")...); err == nil {
		_ = json.Unmarshal(raw, &peers)
	} else if st.Err == "" {
		st.Err = err.Error()
	}

	nextHops := map[string]string{}
	if raw, err := runCLI(ctx, cliPath, append(base, "route", "--output", "json")...); err == nil {
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

// runCLI 执行白名单子查询(超时 8s),返回 stdout。
func runCLI(ctx context.Context, cliPath string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, cliPath, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("easytier-cli %s: %v", args[len(args)-3], err)
	}
	return out, nil
}

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
