package aliyunecs

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ECS OpenAPI 常量:RPC 端点 ecs.<region>.aliyuncs.com + V3 请求结构(x-acs-* 头)+
// ACS3-HMAC-SHA256 签名(官方推荐;新服务已只认 V3)。测试经 RoundTripper 拦截,不触真网络。
const (
	ecsAPIVersion = "2014-05-26"
	ecsPageSize   = 100
	// ecsMaxPages/ecsMaxInstances 是拉取护栏(50 页 × 100 = 5000 台),防异常账户拖垮请求。
	ecsMaxPages    = 50
	ecsMaxInstance = ecsPageSize * ecsMaxPages
)

// 凭据分半段注入:accessKeyID(非机密,来自 aliyun_ecs_accounts.access_key_id)+
// accessKeySecret(来自 vault 解封)。任一半段为空 → 视为凭据无效(ErrListFailed 家族),
// 绝不把 ak/sk 任何片段回显到错误/日志/DTO。
type ecsClient struct {
	accessKeyID     string
	accessKeySecret string
	region          string
	hc              *http.Client
	host            string // 端点主机(默认 ecs.<region>.aliyuncs.com;测试可覆写)

	// 以下供测试注入,钉死签名(生产为 nil → 用 time.Now / crypto-rand nonce)。
	nowFn   func() time.Time
	nonceFn func() string
}

// newEcsClient 构建阿里云 ECS 客户端。transport 为 nil 用默认(带超时),测试可注入 mock
// RoundTripper;host 为空用正式端点。
func newEcsClient(accessKeyID, accessKeySecret, region string) *ecsClient {
	hc := &http.Client{Timeout: 20 * time.Second}
	host := fmt.Sprintf("ecs.%s.aliyuncs.com", region)
	return &ecsClient{
		accessKeyID:     strings.TrimSpace(accessKeyID),
		accessKeySecret: strings.TrimSpace(accessKeySecret),
		region:          strings.TrimSpace(region),
		hc:              hc,
		host:            host,
	}
}

// creds 校验两半段非空(空 → 固定人话,绝不回显凭据片段)。
func (c *ecsClient) creds() error {
	if c.accessKeyID == "" || c.accessKeySecret == "" {
		return fmt.Errorf("%w:阿里云须同时填写 AccessKeyId 与 AccessKeySecret", ErrListFailed)
	}
	return nil
}

func (c *ecsClient) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now().UTC()
}

func (c *ecsClient) nonce() string {
	if c.nonceFn != nil {
		return c.nonceFn()
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 退化:用时间纳秒拼一个仍唯一的 nonce(签名仍正确,只是熵略低)。
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// v3PercentEncode 按阿里云 V3 规范做 RFC3986 百分号编码:仅 A-Za-z0-9-_.~ 不编码,
// 其余字节逐个 %HH(大写十六进制)。
func v3PercentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// sha256Hex 返回 lowercase hex(SHA256(data))。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// v3CanonicalQuery 把业务参数按 key 排序、RFC3986 编码后拼 k=v&k=v(空参数 → 空串)。
// 同一编码用于签名与实际 RawQuery,保证服务端重算一致。
func v3CanonicalQuery(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(v3PercentEncode(k))
		b.WriteByte('=')
		b.WriteString(v3PercentEncode(params[k]))
	}
	return b.String()
}

// v3SignV3 据阿里云 V3 规范计算 ACS3-HMAC-SHA256 签名,返回 Authorization 头值:
//
//	CanonicalRequest = Method \n URI \n Query \n Headers(各 name:value\n 连接) \n
//	                  AdditionalHeaders(空) \n Hex(SHA256(payload))
//	StringToSign     = "ACS3-HMAC-SHA256" \n Hex(SHA256(CanonicalRequest)) \n ""
//	Signature        = lower-hex(HMAC-SHA256(secret, StringToSign))
//	Authorization    = ACS3-HMAC-SHA256 Credential=<AK>,AdditionalHeaders=,Signature=<sig>
//
// 参与签名的头固定为 host + 全部 x-acs-* 头(小写名排序),无额外 AdditionalHeaders。
func v3Sign(method, uri, canonicalQuery string, headers map[string]string, payload []byte, ak, sk string) string {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var hb strings.Builder
	for _, k := range names {
		hb.WriteString(k)
		hb.WriteByte(':')
		hb.WriteString(strings.TrimSpace(headers[k]))
		hb.WriteByte('\n')
	}
	canonicalRequest := method + "\n" + uri + "\n" + canonicalQuery + "\n" + hb.String() + "\n" + sha256Hex(payload)

	mac := hmac.New(sha256.New, []byte(sk))
	mac.Write([]byte("ACS3-HMAC-SHA256\n" + sha256Hex([]byte(canonicalRequest)) + "\n"))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("ACS3-HMAC-SHA256 Credential=%s,AdditionalHeaders=,Signature=%s", ak, sig)
}

// ecsErrResp 是阿里云 V3 错误信封(成功响应无 code/message;HTTP 4xx/5xx 才有)。
// Go json 解码大小写不敏感,V3 的 lowercase 键(code/message)与 RPC 的 PascalCase(Code)
// 都能落位。
type ecsErrResp struct {
	Code      string `json:"Code"`
	Message   string `json:"Message"`
	RequestID string `json:"RequestId"`
}

// ecsErr 把阿里云错误信封映射人话(绝不含 ak/sk;阿里云 Code/Message 不含凭据明文)。
// 未知错误码带上阿里云原始 Message(中文人话,便于开关机等运维操作排障;截长防噪声)。
func ecsErr(status int, ae ecsErrResp) error {
	switch ae.Code {
	case "InvalidAccessKeyId.NotFound", "InvalidAccessKeyId.Disabled", "SignatureDoesNotMatch",
		"Forbidden.AccessKeyDisabled", "Forbidden.RAM", "InvalidSecurityToken.Expired", "NoPermission":
		return fmt.Errorf("%w:阿里云凭据无效或无权限", ErrListFailed)
	case "InvalidRegionId.NotFound", "InvalidRegionId":
		return fmt.Errorf("%w:Region 不存在或账户无该 Region 权限", ErrListFailed)
	}
	if strings.HasPrefix(ae.Code, "IncorrectInstanceStatus") {
		return fmt.Errorf("%w:实例当前状态不允许该操作(%s)", ErrListFailed, truncateTail(ae.Message, 120))
	}
	if ae.Code != "" {
		if msg := truncateTail(ae.Message, 160); msg != "" {
			return fmt.Errorf("%w:阿里云 ECS API 返回错误(HTTP %d, %s: %s)", ErrListFailed, status, ae.Code, msg)
		}
		return fmt.Errorf("%w:阿里云 ECS API 返回错误(HTTP %d, %s)", ErrListFailed, status, ae.Code)
	}
	return fmt.Errorf("%w:阿里云 ECS API 拒绝(HTTP %d)", ErrListFailed, status)
}

// truncateTail 截断阿里云 Message,防长噪声进节点日志。
func truncateTail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// call 发一个 V3 签名后的 GET 请求(业务参数走 query;Action/Version 走签名头),
// 把成功响应体解到 out。HTTP 非 2xx → 解错误信封映射人话。
func (c *ecsClient) call(ctx context.Context, action string, params map[string]string, out any) error {
	if err := c.creds(); err != nil {
		return err
	}
	query := v3CanonicalQuery(params)
	emptyPayload := sha256Hex(nil)

	date := c.now().UTC().Format("2006-01-02T15:04:05Z")
	nonce := c.nonce()
	headers := map[string]string{
		"host":                  c.host,
		"x-acs-action":          action,
		"x-acs-version":         ecsAPIVersion,
		"x-acs-date":            date,
		"x-acs-signature-nonce": nonce,
		"x-acs-content-sha256":  emptyPayload,
	}
	authorization := v3Sign(http.MethodGet, "/", query, headers, nil, c.accessKeyID, c.accessKeySecret)

	urlStr := "https://" + c.host + "/"
	if query != "" {
		urlStr += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return fmt.Errorf("%w:构造阿里云请求失败", ErrListFailed)
	}
	for k, v := range headers {
		if k == "host" {
			continue // host 由 URL 派生,显式再设会被 Go 规范化路径绕开;签名已含。
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("User-Agent", "Pipewright/1.0 (aliyun-ecs@pipewright.local)")

	resp, err := c.hc.Do(req)
	if err != nil {
		// 绝不回显 err.Error()(可能含端点/网络细节);给固定人话。
		return fmt.Errorf("%w:无法连接阿里云 ECS API", ErrListFailed)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var ae ecsErrResp
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = json.Unmarshal(body, &ae)
		return ecsErr(resp.StatusCode, ae)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("%w:阿里云 ECS API 响应解析失败", ErrListFailed)
		}
	}
	return nil
}

// rawIPList / rawInstance / describeInstancesResp 是 DescribeInstances 响应的字段子集。
// 私网 IP 兼容三种来源:VPC(VpcAttributes.PrivateIpAddress)/ 经典 PrivateIpAddress /
// 经典 InnerIpAddress;公网合并分配公网 IP(PublicIpAddress)与 EIP(EipAddress)。
type rawIPList struct {
	IpAddress []string `json:"IpAddress"`
}

type rawEip struct {
	IpAddress string `json:"IpAddress"`
}

// rawTag / rawTagList 是 ECS 实例绑定的阿里云标签(Tags.Tag[])。
type rawTag struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

type rawTagList struct {
	Tag []rawTag `json:"Tag"`
}

type rawInstance struct {
	InstanceID       string     `json:"InstanceId"`
	InstanceName     string     `json:"InstanceName"`
	HostName         string     `json:"HostName"`
	InstanceType     string     `json:"InstanceType"`
	OSType           string     `json:"OSType"`
	Status           string     `json:"Status"`
	RegionID         string     `json:"RegionId"`
	PublicIpAddress  rawIPList  `json:"PublicIpAddress"`
	EipAddress       rawEip     `json:"EipAddress"`
	InnerIPAddress   rawIPList  `json:"InnerIpAddress"`
	PrivateIPAddress rawIPList  `json:"PrivateIpAddress"`
	Tags             rawTagList `json:"Tags"`
	VpcAttributes    struct {
		PrivateIPAddress rawIPList `json:"PrivateIpAddress"`
	} `json:"VpcAttributes"`
}

type describeInstancesResp struct {
	TotalCount int `json:"TotalCount"`
	PageNumber int `json:"PageNumber"`
	PageSize   int `json:"PageSize"`
	Instances  struct {
		Instance []rawInstance `json:"Instance"`
	} `json:"Instances"`
}

// toInstance 把响应原始实例折叠为领域视图(IP 去重保序;阿里云标签折为 map)。
func toInstance(r rawInstance) Instance {
	inst := Instance{
		InstanceID:   r.InstanceID,
		Name:         r.InstanceName,
		HostName:     r.HostName,
		OSType:       r.OSType,
		Status:       r.Status,
		InstanceType: r.InstanceType,
		RegionID:     r.RegionID,
		Tags:         make(map[string]string, len(r.Tags.Tag)),
	}
	for _, tg := range r.Tags.Tag {
		if k := strings.TrimSpace(tg.Key); k != "" {
			inst.Tags[k] = strings.TrimSpace(tg.Value)
		}
	}
	seen := make(map[string]bool)
	add := func(list []string) {
		for _, ip := range list {
			ip = strings.TrimSpace(ip)
			if ip != "" && !seen[ip] {
				seen[ip] = true
				inst.PrivateIPs = append(inst.PrivateIPs, ip)
			}
		}
	}
	add(r.VpcAttributes.PrivateIPAddress.IpAddress)
	add(r.PrivateIPAddress.IpAddress)
	add(r.InnerIPAddress.IpAddress)

	inst.PublicIPs = []string{}
	for _, ip := range r.PublicIpAddress.IpAddress {
		if ip = strings.TrimSpace(ip); ip != "" {
			inst.PublicIPs = append(inst.PublicIPs, ip)
		}
	}
	if eip := strings.TrimSpace(r.EipAddress.IpAddress); eip != "" {
		dup := false
		for _, ip := range inst.PublicIPs {
			if ip == eip {
				dup = true
				break
			}
		}
		if !dup {
			inst.PublicIPs = append(inst.PublicIPs, eip)
		}
	}
	return inst
}

// ListInstances 分页拉取该 Region 全部 ECS 实例(PageNumber 递进直到取满 TotalCount 或
// 空页;护栏 ecsMaxPages/ecsMaxInstances 防异常账户拖垮)。
func (c *ecsClient) ListInstances(ctx context.Context) ([]Instance, error) {
	all := make([]Instance, 0, ecsPageSize)
	for page := 1; page <= ecsMaxPages; page++ {
		var resp describeInstancesResp
		params := map[string]string{
			"PageSize":   fmt.Sprintf("%d", ecsPageSize),
			"PageNumber": fmt.Sprintf("%d", page),
		}
		if err := c.call(ctx, "DescribeInstances", params, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Instances.Instance {
			all = append(all, toInstance(r))
		}
		if len(all) >= resp.TotalCount || len(resp.Instances.Instance) == 0 {
			break
		}
		if len(all) >= ecsMaxInstance {
			break
		}
	}
	return all, nil
}

// Verify 用最小请求(PageSize=1)校验凭据与 Region:HTTP 200 且无错误码即通过。
func (c *ecsClient) Verify(ctx context.Context) error {
	var resp describeInstancesResp
	params := map[string]string{
		"PageSize":   "1",
		"PageNumber": "1",
	}
	return c.call(ctx, "DescribeInstances", params, &resp)
}

// ─── ECS 开关机(流水线 ecs_power 节点消费) ────────────────────────────────────

// StopChargingMode 是阿里云「停机不收费(节省停机模式)」的 StoppedMode 取值。
// 仅 VPC 实例支持;经典网络实例会被 API 拒绝并按逐台错误暴露。
const StopChargingMode = "StopCharging"

// StartInstance 开机一台实例(异步:发起成功即返回,到达 Running 由上层轮询等待)。
func (c *ecsClient) StartInstance(ctx context.Context, instanceID string) error {
	return c.call(ctx, "StartInstance", map[string]string{"InstanceId": instanceID}, nil)
}

// StopInstance 关机一台实例:固定节省停机模式(StoppedMode=StopCharging,需求要求)。
// forceStop=true 时附 ForceStop(强制断电式停机,可能丢未落盘数据)。
func (c *ecsClient) StopInstance(ctx context.Context, instanceID string, forceStop bool) error {
	params := map[string]string{
		"InstanceId":  instanceID,
		"StoppedMode": StopChargingMode,
	}
	if forceStop {
		params["ForceStop"] = "true"
	}
	return c.call(ctx, "StopInstance", params, nil)
}

// describeInstanceStatusResp 是 DescribeInstanceStatus 响应(InstanceId 逗号串查询)。
type describeInstanceStatusResp struct {
	TotalCount       int `json:"TotalCount"`
	InstanceStatuses struct {
		InstanceStatus []struct {
			InstanceID string `json:"InstanceId"`
			Status     string `json:"Status"`
		} `json:"InstanceStatus"`
	} `json:"InstanceStatuses"`
}

// InstanceStatuses 批量查询实例当前状态(≤ ecsMaxPowerBatch 台,PageSize=100 单页覆盖)。
// 返回 id → status;未出现在响应中的 id(已释放)由调用方按逐台错误处理。
func (c *ecsClient) InstanceStatuses(ctx context.Context, instanceIDs []string) (map[string]string, error) {
	var resp describeInstanceStatusResp
	params := map[string]string{
		"InstanceId": strings.Join(instanceIDs, ","),
		"PageSize":   "100",
		"PageNumber": "1",
	}
	if err := c.call(ctx, "DescribeInstanceStatus", params, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(resp.InstanceStatuses.InstanceStatus))
	for _, st := range resp.InstanceStatuses.InstanceStatus {
		out[st.InstanceID] = st.Status
	}
	return out, nil
}
