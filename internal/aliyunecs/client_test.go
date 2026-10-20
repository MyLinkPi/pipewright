package aliyunecs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// rtFunc 把函数适配为 http.RoundTripper(拦截请求,不触真网络)。
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// textResp 构造一个 JSON 文本响应。
func textResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// pinnedClient 构造钉死时间/nonce 的客户端(签名可复现)。
func pinnedClient(rt http.RoundTripper) *ecsClient {
	c := newEcsClient("TestAKID", "TestSK", "cn-hangzhou")
	c.hc = &http.Client{Transport: rt}
	c.nowFn = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	c.nonceFn = func() string { return "nonce123" }
	return c
}

func TestV3PercentEncode(t *testing.T) {
	cases := map[string]string{
		"a":       "a",
		"Z9-_.~":  "Z9-_.~",
		"a b":     "a%20b",
		"a/b":     "a%2Fb",
		"中":       "%E4%B8%AD",
		"i+1=*":   "i%2B1%3D%2A",
		"key=val": "key%3Dval",
	}
	for in, want := range cases {
		if got := v3PercentEncode(in); got != want {
			t.Errorf("v3PercentEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientSignatureGolden(t *testing.T) {
	var captured *http.Request
	var auth string
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		captured = r
		auth = r.Header.Get("Authorization")
		return textResp(200, `{"TotalCount":0,"PageNumber":1,"PageSize":1,"Instances":{"Instance":[]}}`), nil
	})
	c := pinnedClient(rt)
	if err := c.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// 请求形态:业务参数在 query(Action/Version 只在签名头里)。
	if got := captured.URL.RawQuery; got != "PageNumber=1&PageSize=1" {
		t.Errorf("RawQuery = %q, want PageNumber=1&PageSize=1", got)
	}
	for k, want := range map[string]string{
		"X-Acs-Action":          "DescribeInstances",
		"X-Acs-Version":         ecsAPIVersion,
		"X-Acs-Date":            "2026-01-02T03:04:05Z",
		"X-Acs-Signature-Nonce": "nonce123",
	} {
		if got := captured.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}

	// 手写的规范化请求字面量(空 body 的 SHA256 是已知常量):校验签名输入的拼装。
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	canonical := "GET\n/\nPageNumber=1&PageSize=1\n" +
		"host:ecs.cn-hangzhou.aliyuncs.com\n" +
		"x-acs-action:DescribeInstances\n" +
		"x-acs-content-sha256:" + emptySHA + "\n" +
		"x-acs-date:2026-01-02T03:04:05Z\n" +
		"x-acs-signature-nonce:nonce123\n" +
		"x-acs-version:" + ecsAPIVersion + "\n" +
		"\n" +
		emptySHA
	stringToSign := "ACS3-HMAC-SHA256\n" + sha256Hex([]byte(canonical)) + "\n"
	mac := hmac.New(sha256.New, []byte("TestSK"))
	mac.Write([]byte(stringToSign))
	wantSig := hex.EncodeToString(mac.Sum(nil))
	wantAuth := fmt.Sprintf("ACS3-HMAC-SHA256 Credential=TestAKID,AdditionalHeaders=,Signature=%s", wantSig)
	if auth != wantAuth {
		t.Errorf("Authorization =\n  %s\nwant\n  %s", auth, wantAuth)
	}
}

func TestListInstancesPagination(t *testing.T) {
	pages := map[string]string{
		"PageNumber=1&PageSize=100": `{"TotalCount":3,"PageNumber":1,"PageSize":100,"Instances":{"Instance":[
			{"InstanceId":"i-1","InstanceName":"a"},
			{"InstanceId":"i-2","InstanceName":"b"}
		]}}`,
		"PageNumber=2&PageSize=100": `{"TotalCount":3,"PageNumber":2,"PageSize":100,"Instances":{"Instance":[
			{"InstanceId":"i-3","InstanceName":"c"}
		]}}`,
	}
	var queries []string
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.RawQuery
		queries = append(queries, q)
		body, ok := pages[q]
		if !ok {
			t.Errorf("unexpected query %q", q)
			return textResp(400, `{"Code":"MissingParameter"}`), nil
		}
		return textResp(200, body), nil
	})
	c := pinnedClient(rt)
	insts, err := c.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(insts) != 3 {
		t.Fatalf("got %d instances, want 3", len(insts))
	}
	if len(queries) != 2 || queries[0] != "PageNumber=1&PageSize=100" || queries[1] != "PageNumber=2&PageSize=100" {
		t.Errorf("page queries = %v, want 2 pages in order", queries)
	}
}

func TestInstanceParsingIPSources(t *testing.T) {
	body := `{"TotalCount":2,"PageNumber":1,"PageSize":100,"Instances":{"Instance":[
		{
			"InstanceId":"i-vpc","InstanceName":"web","HostName":"iZwebZ","OSType":"linux","Status":"Running",
			"InstanceType":"ecs.c7.large","RegionId":"cn-hangzhou",
			"VpcAttributes":{"PrivateIpAddress":{"IpAddress":["192.168.1.10"]}},
			"EipAddress":{"IpAddress":"5.6.7.8"}
		},
		{
			"InstanceId":"i-classic","OSType":"windows","Status":"Stopped",
			"InnerIpAddress":{"IpAddress":["10.1.1.1"]},
			"PublicIpAddress":{"IpAddress":["1.2.3.4"]},
			"EipAddress":{"IpAddress":"1.2.3.4"}
		}
	]}}`
	rt := rtFunc(func(r *http.Request) (*http.Response, error) { return textResp(200, body), nil })
	insts, err := pinnedClient(rt).ListInstances(context.Background())
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("got %d instances, want 2", len(insts))
	}
	vpc, classic := insts[0], insts[1]
	if len(vpc.PrivateIPs) != 1 || vpc.PrivateIPs[0] != "192.168.1.10" {
		t.Errorf("vpc private ips = %v", vpc.PrivateIPs)
	}
	if len(vpc.PublicIPs) != 1 || vpc.PublicIPs[0] != "5.6.7.8" {
		t.Errorf("vpc public ips = %v (EIP 应并入)", vpc.PublicIPs)
	}
	if len(classic.PrivateIPs) != 1 || classic.PrivateIPs[0] != "10.1.1.1" {
		t.Errorf("classic private ips = %v", classic.PrivateIPs)
	}
	// EIP 与分配公网 IP 重复时去重:只剩一个公网地址。
	if len(classic.PublicIPs) != 1 || classic.PublicIPs[0] != "1.2.3.4" {
		t.Errorf("classic public ips = %v (重复 EIP 应去重)", classic.PublicIPs)
	}
	if classic.OSType != "windows" || classic.Status != "Stopped" {
		t.Errorf("classic os/status = %s/%s", classic.OSType, classic.Status)
	}
}

func TestClientErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		wantCk bool // 是否应映射为 ErrListFailed
		noLeak string
	}{
		{403, `{"Code":"InvalidAccessKeyId.NotFound","Message":"bad"}`, true, ""},
		{400, `{"code":"SignatureDoesNotMatch","message":"lowercase v3 envelope"}`, true, ""}, // V3 小写键也应落位
		{400, `{"Code":"InvalidRegionId.NotFound"}`, true, ""},
		{500, `{"Code":"InternalError"}`, true, ""},
		{500, ``, true, ""},
	}
	for i, tc := range cases {
		rt := rtFunc(func(r *http.Request) (*http.Response, error) { return textResp(tc.status, tc.body), nil })
		err := pinnedClient(rt).Verify(context.Background())
		if err == nil {
			t.Fatalf("case %d: Verify 应失败", i)
		}
		if tc.wantCk && !strings.Contains(err.Error(), ErrListFailed.Error()) {
			t.Errorf("case %d: err = %v, want ErrListFailed 家族", i, err)
		}
		if strings.Contains(err.Error(), "TestAKID") || strings.Contains(err.Error(), "TestSK") {
			t.Errorf("case %d: 错误泄漏凭据片段: %v", i, err)
		}
	}
}

func TestClientCredsMissing(t *testing.T) {
	c := newEcsClient("", "sk", "cn-hangzhou")
	if err := c.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "AccessKeyId") {
		t.Errorf("空 AK 应报凭据缺失人话, got %v", err)
	}
}

// TestVerifyJSONRoundtrip 保证 DescribeInstances 成功响应可被解码(冒烟)。
func TestVerifyJSONRoundtrip(t *testing.T) {
	var out describeInstancesResp
	if err := json.Unmarshal([]byte(`{"TotalCount":7,"PageNumber":1,"PageSize":1,"Instances":{"Instance":[]}}`), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.TotalCount != 7 {
		t.Errorf("TotalCount = %d, want 7", out.TotalCount)
	}
}

func TestPowerActionsRequestShape(t *testing.T) {
	var paths []string
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.Header.Get("X-Acs-Action")+" "+r.URL.RawQuery)
		if r.Header.Get("X-Acs-Action") == "DescribeInstanceStatus" {
			return textResp(200, `{"TotalCount":1,"InstanceStatuses":{"InstanceStatus":[{"InstanceId":"i-1","Status":"Stopped"}]}}`), nil
		}
		return textResp(200, `{}`), nil
	})
	c := pinnedClient(rt)
	ctx := context.Background()

	if err := c.StartInstance(ctx, "i-1"); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	if err := c.StopInstance(ctx, "i-1", false); err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	if err := c.StopInstance(ctx, "i-2", true); err != nil {
		t.Fatalf("StopInstance(force): %v", err)
	}
	st, err := c.InstanceStatuses(ctx, []string{"i-1", "i-2"})
	if err != nil {
		t.Fatalf("InstanceStatuses: %v", err)
	}

	want := []string{
		"StartInstance InstanceId=i-1",
		// 关机固定节省停机模式(StoppedMode=StopCharging)。
		"StopInstance InstanceId=i-1&StoppedMode=StopCharging",
		// 强制关机附 ForceStop。
		"StopInstance ForceStop=true&InstanceId=i-2&StoppedMode=StopCharging",
		"DescribeInstanceStatus InstanceId=i-1%2Ci-2&PageNumber=1&PageSize=100",
	}
	if len(paths) != len(want) {
		t.Fatalf("calls = %v", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("call[%d] =\n  %s\nwant\n  %s", i, paths[i], want[i])
		}
	}
	if st["i-1"] != "Stopped" {
		t.Errorf("statuses = %v", st)
	}
}

func TestInstanceTagsParsing(t *testing.T) {
	body := `{"TotalCount":1,"PageNumber":1,"PageSize":100,"Instances":{"Instance":[{
		"InstanceId":"i-1",
		"Tags":{"Tag":[{"Key":"env","Value":"staging"},{"Key":"team","Value":""}]}
	}]}}`
	rt := rtFunc(func(r *http.Request) (*http.Response, error) { return textResp(200, body), nil })
	insts, err := pinnedClient(rt).ListInstances(context.Background())
	if err != nil || len(insts) != 1 {
		t.Fatalf("ListInstances: %v %d", err, len(insts))
	}
	if insts[0].Tags["env"] != "staging" || insts[0].Tags["team"] != "" || len(insts[0].Tags) != 2 {
		t.Errorf("tags = %v", insts[0].Tags)
	}
}
