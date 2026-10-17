// status_test.go 用注入的 fake CLI runner 验证状态采集的关键行为:
//   - 形态探测:新版旗子前置(-o json node info)优先,旧版旗子后置兜底,命中后缓存;
//   - stderr 透出与提示:连不上 RPC → 追加「可能未运行」提示;参数不识别 → 追加版本提示;
//   - 解析归并:node/peer/route 三路数据合成 Status(p2p/中继判定走 route 交叉验证)。
package easytier

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeCLI 按「形态签名 → 响应」模拟 easytier-cli:未匹配的形态返回退出码 2 风格错误
// (stderr 即原因),模拟真实 clap 行为。
type fakeCLI struct {
	responses map[string]string   // 形态签名(动词串) → JSON 输出
	stderr    map[string]string   // 形态签名 → 失败 stderr
	calls     []string            // 记录实际调用形态(验证缓存命中)
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
		"node": `{"hostname":"old","ipv4":"10.1.1.1"}`,
		"peer": `[]`,
		"route": `[]`,
	}}
	p := newStatusProviderForTest(fake.run)
	st := p.collect(context.Background())
	if st.SelfIP != "10.1.1.1" {
		t.Fatalf("old-style fallback failed: %+v (err=%s)", st, st.Err)
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
