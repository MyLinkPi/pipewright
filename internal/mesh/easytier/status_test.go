// status_test.go 用注入的 fake CLI runner 验证状态采集的关键行为:
//   - 形态探测:新版旗子前置(-o json node info)优先,旧版旗子后置兜底,命中后缓存;
//   - stderr 透出与提示:连不上 RPC → 追加「可能未运行」提示;参数不识别 → 追加版本提示;
//   - 解析归并:node/peer/route 三路数据合成 Status(p2p/中继判定走 route 交叉验证);
//   - Get 缓存语义:真后台刷新(调用即回、采集不随请求取消),取消类失败不清缓存。
package easytier

import (
	"context"
	"errors"
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

// TestStatusProxyRoutes 验证 网段→网关节点 映射:next_hop 优先(中继场景可达)、
// 直连行回落 ipv4、peer 主机名补全 ViaName、本机通告行跳过。
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
		t.Fatalf("proxyRoutes = %+v, want 2 条(重复通告去重 + 本机通告跳过)", st.ProxyRoutes)
	}
	got := map[string]ProxyRoute{}
	for _, pr := range st.ProxyRoutes {
		got[pr.CIDR] = pr
	}
	if pr := got["10.5.0.0/16"]; pr.ViaIP != "10.126.126.2" || pr.ViaName != "beta" {
		t.Fatalf("10.5.0.0/16 → %+v, want beta/10.126.126.2", pr)
	}
	// 192.168.30.0/24 两节点重复通告:首见保留(beta 行在前)。
	if pr := got["192.168.30.0/24"]; pr.ViaIP != "10.126.126.2" || pr.ViaName != "beta" {
		t.Fatalf("192.168.30.0/24 → %+v, want beta/10.126.126.2(首见保留)", pr)
	}
	// 本机(alpha)通告的 172.16.9.0/24 不进映射(客户端不给自己当网关)。
	if pr, ok := got["172.16.9.0/24"]; ok {
		t.Fatalf("本机通告应跳过,却得到 %+v", pr)
	}
	if st.ProxyRoutes[0].CIDR != "10.5.0.0/16" {
		t.Fatalf("应按 CIDR 字典序:%+v", st.ProxyRoutes)
	}
}

// TestStatusParsesRealV26Output 用 easytier v2.6.4 实录 JSON(node/peer/route 三路)
// 验证解析:proxy_cidrs 为逗号分隔字符串、ipv4 带 /24 后缀、next_hop 直连为 "-"、
// lat_ms/rx_bytes 为字符串。此前夹具为臆造数组形态,线上恒空。
func TestStatusParsesRealV26Output(t *testing.T) {
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
		t.Fatalf("SelfIP = %q, want 10.126.126.61(ipv4_addr 带 /24 应剥后缀)", st.SelfIP)
	}
	if st.Hostname != "cli" || st.Version != "2.6.4-8428a89d" {
		t.Fatalf("node 字段:%+v", st)
	}
	if len(st.Links) != 1 || st.Links[0].VirtualIP != "10.126.126.60" || st.Links[0].LatencyMS != 0.12 {
		t.Fatalf("links = %+v(延迟字符串 \"0.12\" 应解析为数值)", st.Links)
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
