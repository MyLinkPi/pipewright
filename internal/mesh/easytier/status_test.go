// status_test.go 用注入的 fake CLI runner 验证状态采集的关键行为:
//   - 形态探测:新版旗子前置(-o json node info)优先,旧版旗子后置兜底,命中后缓存;
//   - stderr 透出与提示:连不上 RPC → 追加「可能未运行」提示;参数不识别 → 追加版本提示;
//   - 解析归并:node/peer/route 三路数据合成 Status(p2p/中继判定走 route 交叉验证);
//   - Get 缓存语义:真后台刷新(调用即回、采集不随请求取消),取消类失败不清缓存。
package easytier

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCLI 按「形态签名 → 响应」模拟 easytier-cli:未匹配的形态返回退出码 2 风格错误
// (stderr 即原因),模拟真实 clap 行为。
type fakeCLI struct {
	responses map[string]string // 形态签名(动词串) → JSON 输出
	stderr    map[string]string // 形态签名 → 失败 stderr
	calls     []string          // 记录实际调用形态(验证缓存命中)
}

func shapeKey(args []string) string { return describeArgs(args) }

func (f *fakeCLI) run(_ context.Context, args []string) ([]byte, error) {
	key := shapeKey(args)
	f.calls = append(f.calls, key)
	if out, ok := f.responses[key]; ok {
		return []byte(out), nil
	}
	if se, ok := f.stderr[key]; ok {
		return nil, errors.New("easytier-cli " + key + ": " + se)
	}
	// 未配置的形态一律视为 clap 用法错误(退出码 2 的等价模拟)。
	return nil, errors.New("easytier-cli " + key + ": error: unexpected argument '--output' found")
}

func TestStatusProbesShapesAndCaches(t *testing.T) {
	fake := &fakeCLI{responses: map[string]string{
		// 只配置「新版旗子前置 + 子动作」形态 → 探测应命中它并缓存。
		"node info": `{"hostname":"alpha","version":"v2.6.4","virtual_ipv4":{"addr":"10.126.126.1"}}`,
		"peer list": `[
			{"peer_id":"p2","hostname":"beta","version":"v2.6.4","virtual_ipv4":"10.126.126.2","lat_ms":12.5},
			{"peer_id":"p3","hostname":"gamma","virtual_ipv4":"10.126.126.3","tunnel":"relay(10.126.126.2)"}
		]`,
		"route list": `[
			{"ipv4":"10.126.126.2","next_hop_ipv4":""},
			{"ipv4":"10.126.126.3","next_hop_ipv4":"10.126.126.2"}
		]`,
	}}
	p := newStatusProviderForTest(fake.run)

	st := p.collect(context.Background())
	if st.Err != "" {
		t.Fatalf("unexpected err: %s", st.Err)
	}
	if st.SelfIP != "10.126.126.1" || st.Hostname != "alpha" || st.Version != "v2.6.4" {
		t.Fatalf("node fields: %+v", st)
	}
	if len(st.Links) != 2 {
		t.Fatalf("links = %d, want 2: %+v", len(st.Links), st.Links)
	}
	// beta:next_hop 为空 → p2p;gamma:tunnel=relay(...) 直接采信,且 route 交叉印证。
	var beta, gamma *Link
	for i := range st.Links {
		switch st.Links[i].Hostname {
		case "beta":
			beta = &st.Links[i]
		case "gamma":
			gamma = &st.Links[i]
		}
	}
	if beta == nil || beta.Relay || beta.LatencyMS != 12.5 {
		t.Fatalf("beta link wrong: %+v", beta)
	}
	if gamma == nil || !gamma.Relay || gamma.RelayVia == "" {
		t.Fatalf("gamma link should be relay with via: %+v", gamma)
	}

	// 二次采集应复用已缓存形态:不再出现探测期的失败调用计数增长。
	before := len(fake.calls)
	_ = p.collect(context.Background())
	if len(fake.calls) != before+3 { // 恰好 node/peer/route 各一次
		t.Fatalf("cached shape not reused: calls %d -> %d", before, len(fake.calls))
	}
}

func TestStatusFallsBackToOldStyleFlags(t *testing.T) {
	// 模拟旧版 CLI:不认旗子前置,只认「node --output json」后置形态。
	fake := &fakeCLI{responses: map[string]string{
		"node":  `{"hostname":"old","ipv4":"10.1.1.1"}`,
		"peer":  `[]`,
		"route": `[]`,
	}}
	p := newStatusProviderForTest(fake.run)
	st := p.collect(context.Background())
	if st.SelfIP != "10.1.1.1" {
		t.Fatalf("old-style fallback failed: %+v (err=%s)", st, st.Err)
	}
}

// TestStatusCollectsProxyCIDRs 验证 route 表 proxy_cidrs 汇总:多路由行去重、字典序稳定、
// 无该字段时为空数组而非 null。
func TestStatusCollectsProxyCIDRs(t *testing.T) {
	fake := &fakeCLI{responses: map[string]string{
		"node info": `{"hostname":"alpha","virtual_ipv4":{"addr":"10.126.126.1"}}`,
		"peer list": `[]`,
		"route list": `[
			{"ipv4":"10.126.126.2","next_hop_ipv4":""},
			{"ipv4":"192.168.30.0","next_hop_ipv4":"10.126.126.2","proxy_cidrs":["192.168.30.0/24"]},
			{"ipv4":"10.5.0.0","next_hop_ipv4":"10.126.126.2","proxy_cidrs":["10.5.0.0/16","192.168.30.0/24"]},
			{"ipv4":"10.9.0.0","proxy_cidrs":["10.9.0.0/24",{"addr":"10.10.0.0/24"},""]}
		]`,
	}}
	p := newStatusProviderForTest(fake.run)
	st := p.collect(context.Background())
	want := []string{"10.10.0.0/24", "10.5.0.0/16", "10.9.0.0/24", "192.168.30.0/24"}
	if len(st.ProxyCIDRs) != len(want) {
		t.Fatalf("proxyCidrs = %v, want %v", st.ProxyCIDRs, want)
	}
	for i := range want {
		if st.ProxyCIDRs[i] != want[i] {
			t.Fatalf("proxyCidrs[%d] = %q, want %q (full: %v)", i, st.ProxyCIDRs[i], want[i], st.ProxyCIDRs)
		}
	}

	// 无 proxy_cidrs 字段 → 空数组(非 nil),JSON 序列化为 []。
	empty := newStatusProviderForTest((&fakeCLI{responses: goodResponses()}).run)
	st2 := empty.collect(context.Background())
	if st2.ProxyCIDRs == nil || len(st2.ProxyCIDRs) != 0 {
		t.Fatalf("proxyCidrs should be empty slice, got %#v", st2.ProxyCIDRs)
	}
}

func TestStatusHintsOnConnectAndVersionErrors(t *testing.T) {
	// 全形态失败 + 连接类 stderr → 错误带「可能未运行」提示。
	fake := &fakeCLI{stderr: map[string]string{
		"node info": "failed to connect to rpc portal 127.0.0.1:15888",
		"node":      "failed to connect to rpc portal",
	}}
	p := newStatusProviderForTest(fake.run)
	st := p.collect(context.Background())
	if st.Err == "" || !strings.Contains(st.Err, "可能 easytier-core 未运行") {
		t.Fatalf("connect hint missing: %q", st.Err)
	}

	// 全形态失败 + 参数不识别(版本过旧) → 错误带版本提示。
	fake2 := &fakeCLI{}
	p2 := newStatusProviderForTest(fake2.run)
	st2 := p2.collect(context.Background())
	if st2.Err == "" || !strings.Contains(st2.Err, "版本可能过旧") {
		t.Fatalf("version hint missing: %q", st2.Err)
	}
}

// goodResponses 是一份可正常采集的最小响应(node 有 SelfIP 便于断言)。
func goodResponses() map[string]string {
	return map[string]string{
		"node info":  `{"hostname":"alpha","virtual_ipv4":{"addr":"10.126.126.1"}}`,
		"peer list":  `[]`,
		"route list": `[]`,
	}
}

// waitRefreshSettled 等待后台刷新单飞结束并返回当前缓存(测试与刷新 goroutine 经
// p.mu 建立先后关系,可安全读取 runnerCtx 等旁路写入)。
func waitRefreshSettled(t *testing.T, p *StatusProvider) *Status {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		st := p.cached
		busy := p.refreshing
		p.mu.Unlock()
		if !busy && st != nil {
			return st
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("background refresh did not settle in time")
	return nil
}

func TestStatusGetRefreshesInBackgroundAndSurvivesRequestCancel(t *testing.T) {
	fake := &fakeCLI{responses: goodResponses()}
	// 首个 CLI 调用阻塞到 release:让采集「正在进行」跨越请求取消的时刻。
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var runnerCtx context.Context
	run := func(ctx context.Context, args []string) ([]byte, error) {
		once.Do(func() {
			runnerCtx = ctx
			close(started)
		})
		select {
		case <-ctx.Done(): // 请求取消若传导进刷新 context,采集即失败(测试据此失败)
			return nil, ctx.Err()
		case <-release:
		}
		return fake.run(ctx, args)
	}
	p := newStatusProviderForTest(run)

	reqCtx, cancel := context.WithCancel(context.Background())
	// 真后台:首次调用立即返回占位(尚无缓存),不内联等待采集。
	if st := p.Get(reqCtx); st.SelfIP != "" {
		t.Fatalf("first Get should not block on collect: %+v", st)
	}
	<-started
	cancel()
	time.Sleep(20 * time.Millisecond) // 给「误随请求取消」留暴露窗口
	if runnerCtx.Err() != nil {
		t.Fatalf("refresh ctx must outlive request cancel: %v", runnerCtx.Err())
	}
	// 放行采集:请求已取消,刷新仍应正常完成并落缓存。
	close(release)
	if got := waitRefreshSettled(t, p); got.SelfIP != "10.126.126.1" {
		t.Fatalf("refresh after request cancel should fill cache: %+v", got)
	}
}

func TestStatusCanceledRefreshKeepsLastGood(t *testing.T) {
	fake := &fakeCLI{responses: goodResponses()}
	p := newStatusProviderForTest(fake.run)
	_ = p.Get(context.Background())
	if got := waitRefreshSettled(t, p); got.SelfIP != "10.126.126.1" {
		t.Fatalf("no good snapshot cached: %+v", got)
	}

	// CLI 开始返回取消类错误(所有形态);TTL 过期后 Get:立即回旧值。
	fake.stderr = map[string]string{}
	for _, k := range []string{"node info", "node", "peer list", "peer", "route list", "route"} {
		fake.stderr[k] = "context canceled"
	}
	p.mu.Lock()
	p.fetched = time.Now().Add(-statusTTL - time.Second)
	p.mu.Unlock()

	if st := p.Get(context.Background()); st.SelfIP != "10.126.126.1" {
		t.Fatalf("Get should return last good snapshot: %+v", st)
	}
	waitRefreshSettled(t, p) // 等这轮失败刷新结束
	p.mu.Lock()
	got := p.cached
	p.mu.Unlock()
	if got == nil || got.SelfIP != "10.126.126.1" || got.Err != "" {
		t.Fatalf("canceled refresh must not evict last good snapshot: %+v", got)
	}
}

// TestStatusProxyRoutes 验证 网段→网关节点 映射:同网段多通告者全收(互为备份)、
// 主网关按 直连优先→path_len 小→IP 字典序 确定性选取、peer 主机名补全 ViaName、
// 本机通告行跳过。旧版中继行形态(ipv4 为被代理网段地址,next_hop 即通告者)亦覆盖。
func TestStatusProxyRoutes(t *testing.T) {
	fake := &fakeCLI{responses: map[string]string{
		"node info": `{"hostname":"alpha","virtual_ipv4":{"addr":"10.126.126.1"}}`,
		"peer list": `[
			{"peer_id":"p2","hostname":"beta","virtual_ipv4":"10.126.126.2"},
			{"peer_id":"p4","hostname":"delta","virtual_ipv4":"10.126.126.4"}
		]`,
		"route list": `[
			{"ipv4":"10.126.126.2","next_hop_ipv4":"","proxy_cidrs":["10.5.0.0/16","192.168.30.0/24"]},
			{"ipv4":"192.168.30.0","next_hop_ipv4":"10.126.126.4","proxy_cidrs":["192.168.30.0/24"]},
			{"ipv4":"10.126.126.1","next_hop_ipv4":"","proxy_cidrs":["172.16.9.0/24"]}
		]`,
	}}
	p := newStatusProviderForTest(fake.run)
	st := p.collect(context.Background())

	if len(st.ProxyRoutes) != 2 {
		t.Fatalf("proxyRoutes = %+v, want 2 条(本机通告跳过)", st.ProxyRoutes)
	}
	got := map[string]ProxyRoute{}
	for _, pr := range st.ProxyRoutes {
		got[pr.CIDR] = pr
	}
	if pr := got["10.5.0.0/16"]; pr.ViaIP != "10.126.126.2" || pr.ViaName != "beta" || len(pr.Vias) != 1 {
		t.Fatalf("10.5.0.0/16 → %+v, want beta/10.126.126.2 单网关", pr)
	}
	// 192.168.30.0/24 两节点重复通告:互为备份,Vias 全收;主网关确定性选取
	//(两行均直连且无 path_len → IP 字典序,beta 在前)。
	pr := got["192.168.30.0/24"]
	if pr.ViaIP != "10.126.126.2" || pr.ViaName != "beta" {
		t.Fatalf("192.168.30.0/24 主网关 → %+v, want beta/10.126.126.2", pr)
	}
	if len(pr.Vias) != 2 || pr.Vias[0].IP != "10.126.126.2" || pr.Vias[1].IP != "10.126.126.4" || pr.Vias[1].Name != "delta" {
		t.Fatalf("192.168.30.0/24 Vias = %+v, want [beta, delta](备份网关全收)", pr.Vias)
	}
	// 本机(alpha)通告的 172.16.9.0/24 不进映射(客户端不给自己当网关)。
	if pr, ok := got["172.16.9.0/24"]; ok {
		t.Fatalf("本机通告应跳过,却得到 %+v", pr)
	}
	if st.ProxyRoutes[0].CIDR != "10.5.0.0/16" {
		t.Fatalf("应按 CIDR 字典序:%+v", st.ProxyRoutes)
	}
}

// TestStatusProxyRoutesFailoverShape 用实测 v2.6.4 双通告者形态(每通告节点一行,
// 直连行 next_hop=DIRECT,中继行 next_hop=中继节点)验证:同网段多通告者全部收录,
// 主网关 = 直连通告者,中继通告者标记 Relay/RelayVia;网关失联后 EasyTier 数据面
// 自动切换到其余通告者(实测秒级),平台仅感知不干预。
func TestStatusProxyRoutesFailoverShape(t *testing.T) {
	fake := &fakeCLI{responses: map[string]string{
		"node info": `{"peer_id":3,"ipv4_addr":"10.188.0.13/24","hostname":"obsC","version":"2.6.4-8428a89d"}`,
		"peer list": `[
			{"peer_id":1,"hostname":"advA","virtual_ipv4":"10.188.0.11"},
			{"peer_id":2,"hostname":"advB","virtual_ipv4":"10.188.0.12"}
		]`,
		"route list": `[
			{"ipv4":"10.188.0.13/24","hostname":"obsC","proxy_cidrs":"","next_hop_ipv4":"-","path_len":0},
			{"ipv4":"10.188.0.11/24","hostname":"advA","proxy_cidrs":"198.18.30.0/24","next_hop_ipv4":"DIRECT","path_len":1},
			{"ipv4":"10.188.0.12/24","hostname":"advB","proxy_cidrs":"198.18.30.0/24","next_hop_ipv4":"10.188.0.11/24","path_len":2}
		]`,
	}}
	p := newStatusProviderForTest(fake.run)
	st := p.collect(context.Background())

	if len(st.ProxyRoutes) != 1 || st.ProxyRoutes[0].CIDR != "198.18.30.0/24" {
		t.Fatalf("proxyRoutes = %+v, want 仅 198.18.30.0/24", st.ProxyRoutes)
	}
	pr := st.ProxyRoutes[0]
	if pr.ViaIP != "10.188.0.11" || pr.ViaName != "advA" {
		t.Fatalf("主网关 → %+v, want advA/10.188.0.11(直连优先)", pr)
	}
	if len(pr.Vias) != 2 {
		t.Fatalf("Vias = %+v, want 2 个通告网关", pr.Vias)
	}
	if v := pr.Vias[0]; v.IP != "10.188.0.11" || v.Name != "advA" || v.Relay {
		t.Fatalf("Vias[0] = %+v, want advA 直连", v)
	}
	if v := pr.Vias[1]; v.IP != "10.188.0.12" || v.Name != "advB" || !v.Relay || v.RelayVia != "10.188.0.11" {
		t.Fatalf("Vias[1] = %+v, want advB 经 10.188.0.11 中继", v)
	}
}

// TestStatusRouteDirectSentinelNotRelay 验证新版 route 表直连占位符(v2.6.4+ 对 cost==1
// 行输出 "DIRECT",旧版为 "-")不被误当中继节点:直连对端应判 p2p,而非「经 DIRECT 中继」。
func TestStatusRouteDirectSentinelNotRelay(t *testing.T) {
	fake := &fakeCLI{responses: map[string]string{
		"node info": `{"peer_id":1,"ipv4_addr":"10.126.126.1/24","hostname":"alpha","version":"2.7.0"}`,
		"peer list": `[
			{"cidr":"10.126.126.1/24","ipv4":"10.126.126.1","hostname":"alpha","cost":"Local","tunnel_proto":"-"},
			{"cidr":"10.126.126.2/24","ipv4":"10.126.126.2","hostname":"beta","cost":"p2p","tunnel_proto":"tcp","lat_ms":"3.20"}
		]`,
		"route list": `[
			{"ipv4":"10.126.126.1/24","hostname":"alpha","proxy_cidrs":"","next_hop_ipv4":"-","next_hop_hostname":"Local","path_len":0},
			{"ipv4":"10.126.126.2/24","hostname":"beta","proxy_cidrs":"","next_hop_ipv4":"DIRECT","next_hop_hostname":"DIRECT","path_len":1}
		]`,
	}}
	p := newStatusProviderForTest(fake.run)
	st := p.collect(context.Background())

	if len(st.Links) != 1 {
		t.Fatalf("links = %+v, want 1(peer 表本机 Local 行应被排除)", st.Links)
	}
	l := st.Links[0]
	if l.VirtualIP != "10.126.126.2" || l.Relay || l.RelayVia != "" || l.LatencyMS != 3.2 {
		t.Fatalf("直连对端判定错误:%+v(next_hop \"DIRECT\" 是直连占位符,非中继节点标识)", l)
	}
}

// TestStatusTransientFailureNotMisreportedAsOldCli 复现并锁定误报修复:新版 CLI +
// core 瞬时不可达(重启窗口)时,新版形态报连接错误、旧形态必然报 clap 用法错误;
// 最终错误应是运行类原因(未运行提示),而非「版本过旧」。恢复后自愈。
func TestStatusTransientFailureNotMisreportedAsOldCli(t *testing.T) {
	fake := &fakeCLI{responses: goodResponses()}
	p := newStatusProviderForTest(fake.run)
	if st := p.collect(context.Background()); st.Err != "" {
		t.Fatalf("健康基线失败: %s", st.Err)
	}

	// core 重启窗口:新版形态连不上 RPC;旧形态未配置 → 默认 clap 用法错误(噪声)。
	fake.responses = nil
	fake.stderr = map[string]string{
		"node info": "failed to connect to rpc portal 127.0.0.1:15888",
		"node":      "failed to connect to rpc portal 127.0.0.1:15888",
	}
	st := p.collect(context.Background())
	if st.Err == "" || !strings.Contains(st.Err, "可能 easytier-core 未运行") {
		t.Fatalf("应报运行类原因(未运行提示),got: %q", st.Err)
	}
	if strings.Contains(st.Err, "版本可能过旧") {
		t.Fatalf("瞬时不可达被误报为版本过旧: %q", st.Err)
	}

	// 窗口结束:错误清除(形态缓存仍有效)。
	fake.responses = goodResponses()
	fake.stderr = nil
	if st := p.collect(context.Background()); st.Err != "" {
		t.Fatalf("应自愈,got: %s", st.Err)
	}
}

// TestStatusParsesRealV26Output 用 easytier v2.6.4 实录 JSON(node/peer/route 三路)
// 验证解析:proxy_cidrs 为逗号分隔字符串、ipv4 带 /24 后缀、next_hop 直连为 "-"、
// lat_ms/rx_bytes 为字符串。此前夹具为臆造数组形态,线上恒空。
func TestStatusParsesRealV26Output(t *testing.T) {
	fake := &fakeCLI{responses: map[string]string{
		// 2026-10-29 Windows 客户端实录:node info 为单个大对象(config 内嵌整段 TOML、
		// ip_list 嵌套 {addr:数字}),虚拟 IP 在顶层 ipv4_addr 且带 /24 后缀。
		"node info": `{ "peer_id": 723949080, "ipv4_addr": "10.126.126.7/24", "proxy_cidrs": [], "hostname": "DESKTOP-C3D6OPL", "stun_info": { "udp_nat_type": 6, "public_ip": [ "112.17.98.146" ] }, "inst_id": "d9d62a48-4da8-4a17-bec4-fb78bea66ee0", "listeners": [ "tcp://0.0.0.0:11010" ], "config": "hostname = \"DESKTOP-C3D6OPL\"\nipv4 = \"10.126.126.7/24\"\ndhcp = true\n", "version": "2.6.4-8428a89d", "feature_flag": { "is_public_server": false, "kcp_input": true }, "ip_list": { "public_ipv4": { "addr": 1880187538 }, "interface_ipv4s": [ { "addr": 2852046250 } ], "public_ipv6": null }, "public_ipv6_addr": null }`,
		// peer list 实录:本机行 cost=Local 且 ipv4 等于自身(须被剔掉),对端行 cost=p2p/relay(N)、
		// tunnel_proto 才是链路协议、lat_ms/rx_bytes 为字符串或 "-"。
		"peer list": `[
			{ "cidr": "10.126.126.7/24", "ipv4": "10.126.126.7", "hostname": "DESKTOP-C3D6OPL", "cost": "Local", "lat_ms": "-", "loss_rate": "-", "rx_bytes": "-", "tx_bytes": "-", "tunnel_proto": "-", "nat_type": "Symmetric", "id": "723949080", "version": "2.6.4-8428a89d" },
			{ "cidr": "10.126.126.1/24", "ipv4": "10.126.126.1", "hostname": "Shanghai01", "cost": "p2p", "lat_ms": "17.04", "loss_rate": "0.0%", "rx_bytes": "31.36 kB", "tx_bytes": "35.17 kB", "tunnel_proto": "tcp", "nat_type": "PortRestricted", "id": "322781623", "version": "2.6.4-8428a89d" },
			{ "cidr": "10.126.126.3/24", "ipv4": "10.126.126.3", "hostname": "zhijiang", "cost": "relay(2)", "lat_ms": "18.00", "loss_rate": "0.0%", "rx_bytes": "0 B", "tx_bytes": "0 B", "tunnel_proto": "", "nat_type": "Symmetric", "id": "4243914704", "version": "2.6.4-8428a89d" }
		]`,
		"route list": `[
			{"ipv4":"10.126.126.7/24","hostname":"DESKTOP-C3D6OPL","proxy_cidrs":"","next_hop_ipv4":"-","next_hop_hostname":"Local","path_len":0},
			{"ipv4":"10.126.126.1/24","hostname":"Shanghai01","proxy_cidrs":"","next_hop_ipv4":"DIRECT","next_hop_hostname":"DIRECT","path_len":1},
			{"ipv4":"10.126.126.3/24","hostname":"zhijiang","proxy_cidrs":"10.101.16.35","next_hop_ipv4":"10.126.126.1/24","next_hop_hostname":"Shanghai01","path_len":2}
		]`,
	}}
	p := newStatusProviderForTest(fake.run)
	st := p.collect(context.Background())

	if st.SelfIP != "10.126.126.7" {
		t.Fatalf("SelfIP = %q, want 10.126.126.7(ipv4_addr 带 /24 应剥后缀)", st.SelfIP)
	}
	if st.Hostname != "DESKTOP-C3D6OPL" || st.Version != "2.6.4-8428a89d" {
		t.Fatalf("node 字段:%+v", st)
	}
	// 本机行(cost=Local,ipv4=自身)必须剔除;对端按 route 表交叉验证直连/中继。
	if len(st.Links) != 2 {
		t.Fatalf("links = %+v, want 2(本机行应剔除)", st.Links)
	}
	if st.Links[0].VirtualIP != "10.126.126.1" || st.Links[0].LatencyMS != 17.04 || st.Links[0].Tunnel != "tcp" || st.Links[0].Relay {
		t.Fatalf("直连对端解析 = %+v", st.Links[0])
	}
	if st.Links[1].VirtualIP != "10.126.126.3" || !st.Links[1].Relay || st.Links[1].RelayVia != "10.126.126.1" {
		t.Fatalf("中继对端解析 = %+v, want relay via 10.126.126.1", st.Links[1])
	}
	want := []string{"10.101.16.35"}
	if len(st.ProxyCIDRs) != len(want) || st.ProxyCIDRs[0] != want[0] {
		t.Fatalf("proxyCidrs = %v, want %v(逗号分隔字符串形态汇总至全部通告行)", st.ProxyCIDRs, want)
	}
	// 中继下的通告网关:通告者 = 目的节点 zhijiang,到达它经 Shanghai01 中继。
	if len(st.ProxyRoutes) != 1 {
		t.Fatalf("proxyRoutes = %+v", st.ProxyRoutes)
	}
	if pr := st.ProxyRoutes[0]; pr.CIDR != "10.101.16.35" || pr.ViaIP != "10.126.126.3" || pr.ViaName != "zhijiang" || !pr.Vias[0].Relay {
		t.Fatalf("通告路由 = %+v", st.ProxyRoutes[0])
	}
}

// TestStatusParsesRealV26OutputLegacyAdv 用旧版实录形态(next_hop "-" 的直连通告行、
// 同网段多 CIDR 逗号串)验证网关回落与 proxy_cidrs 拆分。
func TestStatusParsesRealV26OutputLegacyAdv(t *testing.T) {
	fake := &fakeCLI{responses: map[string]string{
		"node info": `{"peer_id":2593298815,"ipv4_addr":"10.126.126.61/24","hostname":"cli","version":"2.6.4-8428a89d"}`,
		"peer list": `[
			{"cidr":"10.126.126.61/24","ipv4":"10.126.126.61","hostname":"cli","cost":"Local","lat_ms":"-","tunnel_proto":"-","id":"2593298815"},
			{"cidr":"10.126.126.60/24","ipv4":"10.126.126.60","hostname":"adv","cost":"p2p","lat_ms":"0.12","loss_rate":"0.0%","rx_bytes":"1.81 kB","tx_bytes":"2.17 kB","tunnel_proto":"tcp","id":"3919550984"}
		]`,
		"route list": `[
			{"ipv4":"10.126.126.61/24","hostname":"cli","proxy_cidrs":"","next_hop_ipv4":"-"},
			{"ipv4":"10.126.126.60/24","hostname":"adv","proxy_cidrs":"192.168.50.0/24,10.77.0.0/16","next_hop_ipv4":"-"}
		]`,
	}}
	p := newStatusProviderForTest(fake.run)
	st := p.collect(context.Background())

	if st.SelfIP != "10.126.126.61" {
		t.Fatalf("SelfIP = %q, want 10.126.126.61", st.SelfIP)
	}
	if len(st.Links) != 1 || st.Links[0].VirtualIP != "10.126.126.60" || st.Links[0].LatencyMS != 0.12 {
		t.Fatalf("links = %+v(延迟字符串 \"0.12\" 应解析为数值,本机行剔除)", st.Links)
	}
	want := []string{"10.77.0.0/16", "192.168.50.0/24"}
	if len(st.ProxyCIDRs) != len(want) || st.ProxyCIDRs[0] != want[0] || st.ProxyCIDRs[1] != want[1] {
		t.Fatalf("proxyCidrs = %v, want %v(逗号分隔字符串形态)", st.ProxyCIDRs, want)
	}
	// 网关映射:adv 直连行(next_hop "-")→ 网关回落为该行 ipv4(剥后缀)。
	if len(st.ProxyRoutes) != 2 {
		t.Fatalf("proxyRoutes = %+v", st.ProxyRoutes)
	}
	for _, pr := range st.ProxyRoutes {
		if pr.ViaIP != "10.126.126.60" || pr.ViaName != "adv" {
			t.Fatalf("%s 网关 = %+v, want adv/10.126.126.60", pr.CIDR, pr)
		}
	}
}

// TestAbbrevOutput 验证输出压单行:换行/多空格归一,超长保尾部(错误信息在末尾)。
func TestAbbrevOutput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"压平空白", "a\n b   c\n", "a b c"},
		{"空输入", "", ""},
	}
	for _, c := range cases {
		if got := abbrevOutput(c.in); got != c.want {
			t.Fatalf("%s: abbrevOutput = %q, want %q", c.name, got, c.want)
		}
	}
	long := strings.Repeat("x", 5000) + "boom"
	got := abbrevOutput(long)
	if !strings.HasSuffix(got, "boom") || strings.Contains(got, strings.Repeat("x", 4097)) {
		t.Fatalf("超长输出应截断保尾部:%q", got[:40])
	}
}

// TestRpcAddrOfArgs 验证从 CLI 参数提取 --rpc-portal 值;无旗子回落平台默认。
func TestRpcAddrOfArgs(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--rpc-portal", "127.0.0.1:15889", "-o", "json", "node", "info"}, "127.0.0.1:15889"},
		{[]string{"node", "--output", "json"}, "127.0.0.1:15888"},
	}
	for _, c := range cases {
		if got := rpcAddrOfArgs(c.args); got != c.want {
			t.Fatalf("%v → %q, want %q", c.args, got, c.want)
		}
	}
}

// TestExecCLIRunnerLogsCommandAndOutput 用 sh 伪 CLI 验证真 exec 路径:完整命令行、
// stdout、失败 stderr 都进日志(前缀 easytier-cli),连接类失败另落 RPC 口可达性实测。
func TestExecCLIRunnerLogsCommandAndOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 sh 伪二进制,Windows 跳过")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-cli.sh")
	// 形参含 "fail" 走连接失败(stderr+退 1),否则输出 JSON;输出不夹带引号外的命令行串,便于断言。
	body := "#!/bin/sh\n" +
		"case \"$*\" in *fail*) echo 'failed to connect to rpc portal: Connection refused' >&2; exit 1;; esac\n" +
		"echo '{\"hostname\":\"alpha\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("写伪 CLI:%v", err)
	}
	origProbe := rpcListenProbe
	defer func() { rpcListenProbe = origProbe }()
	var probed []string
	rpcListenProbe = func(addr string) error {
		probed = append(probed, addr)
		return errors.New("dial tcp 127.0.0.1:15889: connect: connection refused")
	}

	var buf syncBuf
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer log.SetOutput(os.Stderr)

	run := execCLIRunner(script)
	args := []string{"--rpc-portal", "127.0.0.1:15889", "-o", "json", "node", "info"}
	out, err := run(context.Background(), args)
	if err != nil || !strings.Contains(string(out), "alpha") {
		t.Fatalf("成功路径: out=%q err=%v", out, err)
	}
	logged := buf.String()
	for _, want := range []string{
		"easytier-cli: 执行 " + script + " --rpc-portal 127.0.0.1:15889 -o json node info",
		`easytier-cli: 输出(node info): {"hostname":"alpha"}`,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("成功路径日志缺 %q:\n%s", want, logged)
		}
	}

	buf.mu.Lock()
	buf.buf.Reset()
	buf.mu.Unlock()
	failArgs := []string{"--rpc-portal", "127.0.0.1:15889", "-o", "json", "node", "fail"}
	if _, err := run(context.Background(), failArgs); err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("失败路径 err = %v,应携 stderr 摘要", err)
	}
	logged = buf.String()
	for _, want := range []string{
		"easytier-cli: 执行", "easytier-cli: stderr(node fail):", "failed to connect to rpc portal",
		"RPC 端口可达性实测 127.0.0.1:15889", // 连接类失败补一次端口实测
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("失败路径日志缺 %q:\n%s", want, logged)
		}
	}
	if len(probed) == 0 || probed[0] != "127.0.0.1:15889" {
		t.Fatalf("实测入口地址 = %v", probed)
	}
}
