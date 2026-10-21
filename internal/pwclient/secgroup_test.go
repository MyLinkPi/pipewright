// secgroup_test.go 覆盖「连接前自动更新阿里云安全组」:V3 签名客户端(请求形态 +
// 签名 golden + 错误映射 + 分页)、单组更新流转(锚点匹配/先加后删/失败中止)、对账
// 同步(指纹缓存/IP 变化自愈/退避/多组隔离)与公网 IP 探测容错。全部经注入 fake,
// 不触真网络。
package pwclient

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sgRTFunc 把函数适配为 http.RoundTripper(拦截请求,不触真网络)。
type sgRTFunc func(*http.Request) (*http.Response, error)

func (f sgRTFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// sgTextResp 构造一个 JSON 文本响应。
func sgTextResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// sgPinnedClient 构造钉死时间/nonce + 注入 transport 的客户端(签名可复现)。
func sgPinnedClient(rt http.RoundTripper) *sgClient {
	c := newSGClient("TestAKID", "TestSK", "cn-hangzhou")
	c.hc = &http.Client{Transport: rt}
	c.nowFn = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	c.nonceFn = func() string { return "nonce123" }
	return c
}

// TestSGClientRequestAndSignature 校验请求形态(业务参数在 query,Action/Version 只在
// 签名头)与 ACS3-HMAC-SHA256 签名拼装(手写规范化请求字面量独立复算,空 body 的
// SHA256 是已知常量),以及凭据错误码的人话映射。
func TestSGClientRequestAndSignature(t *testing.T) {
	var captured *http.Request
	var auth string
	rt := sgRTFunc(func(r *http.Request) (*http.Response, error) {
		captured = r
		auth = r.Header.Get("Authorization")
		return sgTextResp(200, `{"Permissions":{"Permission":[]}}`), nil
	})
	c := sgPinnedClient(rt)
	if _, err := c.describePermissions(context.Background(), "sg-1"); err != nil {
		t.Fatalf("describePermissions: %v", err)
	}
	if got := captured.URL.Host; got != "ecs.cn-hangzhou.aliyuncs.com" {
		t.Errorf("host = %q", got)
	}
	if got := captured.URL.RawQuery; got != "RegionId=cn-hangzhou&SecurityGroupId=sg-1" {
		t.Errorf("RawQuery = %q", got)
	}
	for k, want := range map[string]string{
		"X-Acs-Action":          "DescribeSecurityGroupAttribute",
		"X-Acs-Version":         "2014-05-26",
		"X-Acs-Date":            "2026-01-02T03:04:05Z",
		"X-Acs-Signature-Nonce": "nonce123",
	} {
		if got := captured.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	canonical := "GET\n/\nRegionId=cn-hangzhou&SecurityGroupId=sg-1\n" +
		"host:ecs.cn-hangzhou.aliyuncs.com\n" +
		"x-acs-action:DescribeSecurityGroupAttribute\n" +
		"x-acs-content-sha256:" + emptySHA + "\n" +
		"x-acs-date:2026-01-02T03:04:05Z\n" +
		"x-acs-signature-nonce:nonce123\n" +
		"x-acs-version:2014-05-26\n" +
		"\n" +
		emptySHA
	stringToSign := "ACS3-HMAC-SHA256\n" + sgSHA256Hex([]byte(canonical)) + "\n"
	mac := hmac.New(sha256.New, []byte("TestSK"))
	mac.Write([]byte(stringToSign))
	wantAuth := fmt.Sprintf("ACS3-HMAC-SHA256 Credential=TestAKID,AdditionalHeaders=,Signature=%s",
		hex.EncodeToString(mac.Sum(nil)))
	if auth != wantAuth {
		t.Errorf("Authorization =\n  %s\nwant\n  %s", auth, wantAuth)
	}

	// 错误信封 → 人话(凭据类错误码不透出原始报文)。
	c2 := sgPinnedClient(sgRTFunc(func(*http.Request) (*http.Response, error) {
		return sgTextResp(403, `{"Code":"InvalidAccessKeyId.NotFound","Message":"ak not found"}`), nil
	}))
	if _, err := c2.describePermissions(context.Background(), "sg-1"); err == nil || err.Error() != "阿里云凭据无效或无权限" {
		t.Errorf("凭据错误映射 = %v, want 阿里云凭据无效或无权限", err)
	}
}

// TestSGClientAuthorizeRevokeParams 校验授权/回收调用的参数拼装(授权带描述、回收不带)。
func TestSGClientAuthorizeRevokeParams(t *testing.T) {
	queries := map[string]string{}
	rt := sgRTFunc(func(r *http.Request) (*http.Response, error) {
		queries[r.Header.Get("X-Acs-Action")] = r.URL.RawQuery
		return sgTextResp(200, `{}`), nil
	})
	c := sgPinnedClient(rt)
	rule := sgPermission{IPProtocol: "TCP", PortRange: "22/22", SourceCidrIP: "1.2.3.4", Description: "pw"}
	if err := c.authorize(context.Background(), "sg-1", rule, "5.6.7.8"); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := c.revoke(context.Background(), "sg-1", rule); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := queries["AuthorizeSecurityGroup"]; got != "Description=pw&IpProtocol=TCP&PortRange=22%2F22&RegionId=cn-hangzhou&SecurityGroupId=sg-1&SourceCidrIp=5.6.7.8" {
		t.Errorf("authorize query = %q", got)
	}
	if got := queries["RevokeSecurityGroup"]; got != "IpProtocol=TCP&PortRange=22%2F22&RegionId=cn-hangzhou&SecurityGroupId=sg-1&SourceCidrIp=1.2.3.4" {
		t.Errorf("revoke query = %q", got)
	}
}

// TestSGDescribePermissionsPagination 校验 NextToken 分页合并。
func TestSGDescribePermissionsPagination(t *testing.T) {
	pages := map[string]string{
		"RegionId=cn-hangzhou&SecurityGroupId=sg-1": `{"Permissions":{"Permission":[
			{"IpProtocol":"TCP","PortRange":"22/22","SourceCidrIp":"1.1.1.1","Description":"a"}]},
			"NextToken":"t2"}`,
		"NextToken=t2&RegionId=cn-hangzhou&SecurityGroupId=sg-1": `{"Permissions":{"Permission":[
			{"IpProtocol":"UDP","PortRange":"53/53","SourceCidrIp":"2.2.2.2","Description":"b"}]}}`,
	}
	var queries []string
	rt := sgRTFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.RawQuery
		queries = append(queries, q)
		body, ok := pages[q]
		if !ok {
			t.Errorf("unexpected query %q", q)
			return sgTextResp(400, `{"Code":"MissingParameter"}`), nil
		}
		return sgTextResp(200, body), nil
	})
	perms, err := sgPinnedClient(rt).describePermissions(context.Background(), "sg-1")
	if err != nil {
		t.Fatalf("describePermissions: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("请求数 = %d, want 2", len(queries))
	}
	if len(perms) != 2 || perms[1].Description != "b" {
		t.Errorf("分页合并结果 = %+v", perms)
	}
}

// sgFake 是 sgAPI 的测试替身:按序记录调用,可注入失败。
type sgFake struct {
	perms       []sgPermission
	calls       []string // "describe sg-1" / "auth sg-1 TCP 22/22 5.6.7.8" / "revoke sg-1 TCP 22/22 1.2.3.4"
	describeErr error
	authErr     error
	revokeErr   error
}

func (f *sgFake) describePermissions(_ context.Context, sgID string) ([]sgPermission, error) {
	f.calls = append(f.calls, "describe "+sgID)
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return f.perms, nil
}

func (f *sgFake) authorize(_ context.Context, sgID string, rule sgPermission, sourceCidrIP string) error {
	f.calls = append(f.calls, fmt.Sprintf("auth %s %s %s %s", sgID, rule.IPProtocol, rule.PortRange, sourceCidrIP))
	return f.authErr
}

func (f *sgFake) revoke(_ context.Context, sgID string, rule sgPermission) error {
	f.calls = append(f.calls, fmt.Sprintf("revoke %s %s %s %s", sgID, rule.IPProtocol, rule.PortRange, rule.SourceCidrIP))
	return f.revokeErr
}

// TestUpdateGroupFlows 覆盖单组更新流转:无匹配告警不改、已最新跳过(裸 IP 与 /32 等价)、
// 过期先授权新再回收旧、授权失败即中止(不回收)。
func TestUpdateGroupFlows(t *testing.T) {
	rt := &Runtime{}
	g := AliyunGroupConfig{RegionID: "cn-hangzhou", SecurityGroupID: "sg-1", Description: "pw"}

	// 无匹配 → warn,零写操作。
	f := &sgFake{perms: []sgPermission{{Description: "other", IPProtocol: "TCP", PortRange: "22/22", SourceCidrIP: "0.0.0.0/0"}}}
	if st, err := rt.updateGroup(context.Background(), f, g, "5.6.7.8"); err != nil || st != "warn" {
		t.Errorf("无匹配: st=%q err=%v, want warn", st, err)
	}
	if len(f.calls) != 1 {
		t.Errorf("无匹配时不应有写操作: %v", f.calls)
	}

	// 已最新(裸 IP 与 /32 等价)→ skip。
	f = &sgFake{perms: []sgPermission{
		{Description: "pw", IPProtocol: "TCP", PortRange: "22/22", SourceCidrIP: "5.6.7.8/32"},
		{Description: "pw", IPProtocol: "UDP", PortRange: "53/53", SourceCidrIP: "5.6.7.8"},
	}}
	if st, err := rt.updateGroup(context.Background(), f, g, "5.6.7.8"); err != nil || st != "skip" {
		t.Errorf("已最新: st=%q err=%v, want skip", st, err)
	}
	if len(f.calls) != 1 {
		t.Errorf("已最新时不应有写操作: %v", f.calls)
	}

	// 过期 → 每条先授权新 IP 再回收旧 IP(顺序敏感)。
	f = &sgFake{perms: []sgPermission{
		{Description: "pw", IPProtocol: "TCP", PortRange: "22/22", SourceCidrIP: "1.2.3.4"},
		{Description: "pw", IPProtocol: "TCP", PortRange: "11010/11010", SourceCidrIP: "1.2.3.4"},
		{Description: "other", IPProtocol: "TCP", PortRange: "80/80", SourceCidrIP: "0.0.0.0/0"},
	}}
	if st, err := rt.updateGroup(context.Background(), f, g, "5.6.7.8"); err != nil || st != "update" {
		t.Errorf("过期: st=%q err=%v, want update", st, err)
	}
	want := []string{
		"describe sg-1",
		"auth sg-1 TCP 22/22 5.6.7.8",
		"revoke sg-1 TCP 22/22 1.2.3.4",
		"auth sg-1 TCP 11010/11010 5.6.7.8",
		"revoke sg-1 TCP 11010/11010 1.2.3.4",
	}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Errorf("调用序 = %v, want %v", f.calls, want)
	}

	// 授权失败 → 中止且不回收(不把自己挡在门外的反面:不删任何既有规则)。
	f = &sgFake{perms: []sgPermission{{Description: "pw", IPProtocol: "TCP", PortRange: "22/22", SourceCidrIP: "1.2.3.4"}},
		authErr: fmt.Errorf("boom")}
	if _, err := rt.updateGroup(context.Background(), f, g, "5.6.7.8"); err == nil {
		t.Error("授权失败应返回错误")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "revoke") {
			t.Errorf("授权失败后不应回收: %v", f.calls)
		}
	}
}

// TestSGSourceEqualsIP 校验源 CIDR 与目标 IP 的归一比较。
func TestSGSourceEqualsIP(t *testing.T) {
	cases := []struct {
		cidr string
		ip   string
		want bool
	}{
		{"5.6.7.8", "5.6.7.8", true},
		{"5.6.7.8/32", "5.6.7.8", true},
		{"5.6.7.8/32", "5.6.7.9", false},
		{"5.6.0.0/16", "5.6.7.8", false}, // 网段不算「已指向本机」
		{"0.0.0.0/0", "5.6.7.8", false},
		{"garbage", "5.6.7.8", false},
	}
	for _, c := range cases {
		if got := sgSourceEqualsIP(c.cidr, c.ip); got != c.want {
			t.Errorf("sgSourceEqualsIP(%q, %q) = %v, want %v", c.cidr, c.ip, got, c.want)
		}
	}
}

// sgTestEnv 组装注入了 fake 客户端/公网 IP 的运行时(配置落临时目录)。
func sgTestEnv(t *testing.T, groups []AliyunGroupConfig, ip string, fake *sgFake) (*Runtime, Config, *int) {
	t.Helper()
	dir := t.TempDir()
	store := &Store{Path: filepath.Join(dir, "config.yaml")}
	cfg := Config{Aliyun: AliyunConfig{AccessKeyID: "ak", AccessKeySecret: "sk", Groups: groups}}
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	fetches := 0
	rt := NewRuntime(store, dir)
	rt.newSGClient = func(ak, sk, region string) sgAPI { return fake }
	rt.sgIPFetch = func(context.Context) (string, error) {
		fetches++
		return ip, nil
	}
	return rt, cfg, &fetches
}

// TestReconcileSecurityGroupCaching 覆盖同步节拍:连接前强制(即使未到节拍)、IP 未变
// 不再调阿里云、IP 变化自愈、稳态按节流跳过。
func TestReconcileSecurityGroupCaching(t *testing.T) {
	g := AliyunGroupConfig{RegionID: "cn-hangzhou", SecurityGroupID: "sg-1", Description: "pw"}
	ip := "5.6.7.8"
	rt, cfg, fetches := sgTestEnv(t, []AliyunGroupConfig{g}, ip,
		&sgFake{perms: []sgPermission{{Description: "pw", IPProtocol: "TCP", PortRange: "22/22", SourceCidrIP: "1.2.3.4"}}})
	ctx := context.Background()

	// 连接前:强制同步(授权新 IP + 回收旧 IP)。
	rt.reconcileSecurityGroup(ctx, cfg, true)
	rt.mu.Lock()
	applied, lastErr := rt.sgApplied, rt.sgLastErr
	rt.mu.Unlock()
	if applied == "" || lastErr != "" {
		t.Fatalf("首轮应成功: applied=%q lastErr=%q", applied, lastErr)
	}
	callsAfterFirst := len(rt.sgClients["cn-hangzhou|ak|"+secretFingerprint("sk")].(*sgFake).calls)

	// 稳态(未到 5 分钟节拍):既不探测 IP 也不调阿里云。
	rt.reconcileSecurityGroup(ctx, cfg, false)
	if n := *fetches; n != 1 {
		t.Errorf("稳态不应重复探测 IP: fetches=%d", n)
	}
	callsAfterSteady := len(rt.sgClients["cn-hangzhou|ak|"+secretFingerprint("sk")].(*sgFake).calls)
	if callsAfterSteady != callsAfterFirst {
		t.Errorf("稳态不应调阿里云: calls %d -> %d", callsAfterFirst, callsAfterSteady)
	}

	// 连接重建立即触发:IP 未变 → 只探测,不调阿里云。
	rt.reconcileSecurityGroup(ctx, cfg, true)
	if n := *fetches; n != 2 {
		t.Errorf("重连应复查 IP: fetches=%d", n)
	}
	if n := len(rt.sgClients["cn-hangzhou|ak|"+secretFingerprint("sk")].(*sgFake).calls); n != callsAfterSteady {
		t.Errorf("IP 未变不应调阿里云: calls %d -> %d", callsAfterSteady, n)
	}

	// IP 变化 → 重新更新(即使未到节拍,经 connectPending)。
	rt.sgIPFetch = func(context.Context) (string, error) { *fetches++; return "9.9.9.9", nil }
	rt.reconcileSecurityGroup(ctx, cfg, true)
	if n := len(rt.sgClients["cn-hangzhou|ak|"+secretFingerprint("sk")].(*sgFake).calls); n <= callsAfterSteady {
		t.Errorf("IP 变化应重新更新: calls %d -> %d", callsAfterSteady, n)
	}
}

// TestReconcileSecurityGroupFailureAndIsolation 覆盖失败路径:单组失败不中断其余组、
// 失败记状态并退避(稳态立即再跑被节流)、未配置时清状态。
func TestReconcileSecurityGroupFailureAndIsolation(t *testing.T) {
	g1 := AliyunGroupConfig{RegionID: "cn-hangzhou", SecurityGroupID: "sg-1", Description: "pw"}
	g2 := AliyunGroupConfig{RegionID: "cn-shanghai", SecurityGroupID: "sg-2", Description: "pw"}
	broken := &sgFake{describeErr: fmt.Errorf("阿里云凭据无效或无权限")}
	ok := &sgFake{perms: []sgPermission{{Description: "pw", IPProtocol: "TCP", PortRange: "22/22", SourceCidrIP: "1.2.3.4"}}}
	rt, cfg, fetches := sgTestEnv(t, []AliyunGroupConfig{g1, g2}, "5.6.7.8", ok)
	clients := map[string]sgAPI{"cn-hangzhou": broken, "cn-shanghai": ok}
	rt.newSGClient = func(ak, sk, region string) sgAPI { return clients[region] }
	ctx := context.Background()

	rt.reconcileSecurityGroup(ctx, cfg, true)
	snap := rt.sgSnapshot(cfg)
	if snap.LastErr == "" {
		t.Fatal("有失败组应记录 LastErr")
	}
	if len(snap.Results) != 2 || snap.Results[0].Status != "fail" || snap.Results[1].Status != "update" {
		t.Errorf("逐组结果 = %+v(单组失败不应中断其余组)", snap.Results)
	}
	rt.mu.Lock()
	applied := rt.sgApplied
	next := rt.sgNextCheck
	rt.mu.Unlock()
	if applied != "" {
		t.Error("有失败组不应记成功指纹")
	}

	// 稳态退避:未到退避时间不再探测。
	rt.reconcileSecurityGroup(ctx, cfg, false)
	if n := *fetches; n != 1 {
		t.Errorf("退避期内稳态不应探测: fetches=%d", n)
	}
	if !next.After(time.Now()) {
		t.Error("失败后应设置退避节拍")
	}

	// 未配置(如清空 groups)→ 状态清空且零调用。
	empty := cfg
	empty.Aliyun = AliyunConfig{}
	rt.reconcileSecurityGroup(ctx, empty, true)
	if s := rt.sgSnapshot(empty); s.Configured || s.LastErr != "" || len(s.Results) != 0 {
		t.Errorf("未配置应清状态: %+v", s)
	}
}

// TestFetchPublicIPv4 覆盖公网 IP 探测:多服务容错、非法响应跳过、全失败报错、
// 成功须是合法 IPv4。
func TestFetchPublicIPv4(t *testing.T) {
	// 首个服务返回 HTML(浏览器反代场景)、次个返回带换行的合法 IP。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>ip</html>"))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("203.0.113.7\n"))
	}))
	defer good.Close()
	ip, err := fetchPublicIPv4From(context.Background(), []string{bad.URL, good.URL})
	if err != nil || ip != "203.0.113.7" {
		t.Errorf("容错探测 = %q, %v", ip, err)
	}

	// 全部非法 → 报错并聚合原因。
	_, err = fetchPublicIPv4From(context.Background(), []string{bad.URL})
	if err == nil || !strings.Contains(err.Error(), bad.URL) {
		t.Errorf("全失败应报错并含服务地址: %v", err)
	}

	// IPv6 响应不算(安全组源段走 IPv4)。
	v6 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("2001:db8::1"))
	}))
	defer v6.Close()
	if _, err := fetchPublicIPv4From(context.Background(), []string{v6.URL}); err == nil {
		t.Error("IPv6 响应应被拒绝")
	}
}
