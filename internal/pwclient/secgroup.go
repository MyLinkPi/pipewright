// secgroup.go 是「连接前自动更新阿里云安全组」:EasyTier 发起连接(含重连)前,把本机
// 当前公网 IPv4 授权进所配安全组的既有规则(按描述精确匹配锚点,协议/端口沿用旧规则,
// 只换源 IP),成功后回收旧 IP 的规则;连接保持期间定期复查公网 IP,变化即自愈。
// 更新逻辑移植自 init_script/update_security_group_ip(Node 版)。
//
// 同步是 best-effort:取 IP 或调用 API 失败只记状态与日志并按退避重试,绝不阻断组网
// (与 ensureFirewall 同款取舍)。AK/SK 明文落盘 config.yaml(0600),绝不入日志/状态回显。
package pwclient

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// 安全组同步节拍:连接保持期间每 secGroupRefreshInterval 复查一次公网 IP(IP 与配置
// 指纹都没变则不调阿里云);失败退避 secGroupRetryBackoff,防连接持续失败时刷接口。
const (
	secGroupRefreshInterval = 5 * time.Minute
	secGroupRetryBackoff    = 1 * time.Minute
	secGroupIPTimeout       = 10 * time.Second
	// sgMaxPages 是 DescribeSecurityGroupAttribute 的分页护栏(防异常响应拖垮对账)。
	sgMaxPages = 10
)

// sgIPServices 是公网 IP 探测服务(依次容错;浏览器 UA 会得到 HTML,故请求带 curl 风格 UA)。
var sgIPServices = []string{"https://ifconfig.me", "https://icanhazip.com", "https://ident.me"}

// ─── 阿里云 ECS V3 客户端(签名与 internal/aliyunecs 同款 ACS3-HMAC-SHA256;各包
// 内聚、错误语义自持 —— 与 dnsprovider 的取舍一致) ───────────────────────────────

// sgClient 是安全组更新用的阿里云 ECS 客户端(GET RPC + V3 签名;凭据来自本机配置)。
type sgClient struct {
	ak     string
	sk     string
	region string
	hc     *http.Client
	host   string // 端点主机 ecs.<region>.aliyuncs.com

	// 以下供测试注入,钉死签名(生产为 nil → time.Now / crypto-rand nonce)。
	nowFn   func() time.Time
	nonceFn func() string
}

// newSGClient 构造客户端(20s 超时;测试经注入 RoundTripper 拦截,不触真网络)。
func newSGClient(ak, sk, region string) *sgClient {
	return &sgClient{
		ak:     strings.TrimSpace(ak),
		sk:     strings.TrimSpace(sk),
		region: strings.TrimSpace(region),
		hc:     &http.Client{Timeout: 20 * time.Second},
		host:   fmt.Sprintf("ecs.%s.aliyuncs.com", strings.TrimSpace(region)),
	}
}

func (c *sgClient) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now().UTC()
}

func (c *sgClient) nonce() string {
	if c.nonceFn != nil {
		return c.nonceFn()
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// sgPercentEncode 按阿里云 V3 规范做 RFC3986 百分号编码(与 internal/aliyunecs 同实现)。
func sgPercentEncode(s string) string {
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

func sgSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sgCanonicalQuery 把业务参数按 key 排序、RFC3986 编码后拼 k=v&k=v(同一编码用于签名
// 与实际 RawQuery,保证服务端重算一致)。
func sgCanonicalQuery(params map[string]string) string {
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
		b.WriteString(sgPercentEncode(k))
		b.WriteByte('=')
		b.WriteString(sgPercentEncode(params[k]))
	}
	return b.String()
}

// sgSign 据阿里云 V3 规范计算 ACS3-HMAC-SHA256 签名,返回 Authorization 头值(参与签名
// 的头固定为 host + 全部 x-acs-* 头,小写名排序)。
func sgSign(method, uri, canonicalQuery string, headers map[string]string, payload []byte, ak, sk string) string {
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
	canonicalRequest := method + "\n" + uri + "\n" + canonicalQuery + "\n" + hb.String() + "\n" + sgSHA256Hex(payload)

	mac := hmac.New(sha256.New, []byte(sk))
	mac.Write([]byte("ACS3-HMAC-SHA256\n" + sgSHA256Hex([]byte(canonicalRequest)) + "\n"))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("ACS3-HMAC-SHA256 Credential=%s,AdditionalHeaders=,Signature=%s", ak, sig)
}

// sgErrResp 是阿里云 V3 错误信封(成功响应无 code/message;HTTP 4xx/5xx 才有)。
type sgErrResp struct {
	Code      string `json:"Code"`
	Message   string `json:"Message"`
	RequestID string `json:"RequestId"`
}

// sgErr 把阿里云错误信封映射人话(绝不回显 ak/sk;未知错误码带原始 Message 便于排障)。
func sgErr(status int, ae sgErrResp) error {
	switch ae.Code {
	case "InvalidAccessKeyId.NotFound", "InvalidAccessKeyId.Disabled", "SignatureDoesNotMatch",
		"Forbidden.AccessKeyDisabled", "Forbidden.RAM", "InvalidSecurityToken.Expired", "NoPermission":
		return fmt.Errorf("阿里云凭据无效或无权限")
	case "InvalidRegionId.NotFound", "InvalidRegionId":
		return fmt.Errorf("Region 不存在或账户无该 Region 权限")
	case "InvalidSecurityGroupId.NotFound":
		return fmt.Errorf("安全组不存在")
	}
	if ae.Code != "" {
		msg := strings.TrimSpace(ae.Message)
		if len(msg) > 160 {
			msg = msg[:160] + "…"
		}
		if msg != "" {
			return fmt.Errorf("阿里云 API 返回错误(HTTP %d, %s: %s)", status, ae.Code, msg)
		}
		return fmt.Errorf("阿里云 API 返回错误(HTTP %d, %s)", status, ae.Code)
	}
	return fmt.Errorf("阿里云 API 拒绝请求(HTTP %d)", status)
}

// call 发一个 V3 签名后的 GET 请求(业务参数走 query;Action/Version 走签名头),把成功
// 响应体解到 out。HTTP 非 2xx → 解错误信封映射人话。
func (c *sgClient) call(ctx context.Context, action string, params map[string]string, out any) error {
	if c.ak == "" || c.sk == "" {
		return fmt.Errorf("阿里云须同时填写 AccessKeyId 与 AccessKeySecret")
	}
	query := sgCanonicalQuery(params)
	emptyPayload := sgSHA256Hex(nil)
	headers := map[string]string{
		"host":                  c.host,
		"x-acs-action":          action,
		"x-acs-version":         "2014-05-26",
		"x-acs-date":            c.now().UTC().Format("2006-01-02T15:04:05Z"),
		"x-acs-signature-nonce": c.nonce(),
		"x-acs-content-sha256":  emptyPayload,
	}
	authorization := sgSign(http.MethodGet, "/", query, headers, nil, c.ak, c.sk)

	urlStr := "https://" + c.host + "/"
	if query != "" {
		urlStr += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return fmt.Errorf("构造阿里云请求失败")
	}
	for k, v := range headers {
		if k == "host" {
			continue // host 由 URL 派生;签名已含
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("User-Agent", "Pipewright/1.0 (pwclient-secgroup)")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接阿里云 API")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var ae sgErrResp
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = json.Unmarshal(body, &ae)
		return sgErr(resp.StatusCode, ae)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("阿里云 API 响应解析失败")
		}
	}
	return nil
}

// ─── 安全组 API ────────────────────────────────────────────────────────────────

// sgAPI 是安全组更新所需的阿里云 ECS 能力(sgClient 实现;测试可注入 fake)。
type sgAPI interface {
	describePermissions(ctx context.Context, securityGroupID string) ([]sgPermission, error)
	authorize(ctx context.Context, securityGroupID string, rule sgPermission, sourceCidrIP string) error
	revoke(ctx context.Context, securityGroupID string, rule sgPermission) error
}

// sgPermission 是安全组规则的字段子集(更新只关心协议/端口/源段/描述)。
type sgPermission struct {
	IPProtocol   string `json:"IpProtocol"`
	PortRange    string `json:"PortRange"`
	SourceCidrIP string `json:"SourceCidrIp"`
	Description  string `json:"Description"`
}

// sgAttrResp 是 DescribeSecurityGroupAttribute 响应(规则列表 + 分页游标)。
type sgAttrResp struct {
	NextToken   string `json:"NextToken"`
	Permissions struct {
		Permission []sgPermission `json:"Permission"`
	} `json:"Permissions"`
}

// describePermissions 拉取安全组全量规则(入+出方向;NextToken 分页合并,护栏 sgMaxPages)。
func (c *sgClient) describePermissions(ctx context.Context, securityGroupID string) ([]sgPermission, error) {
	var all []sgPermission
	next := ""
	for page := 0; page < sgMaxPages; page++ {
		params := map[string]string{
			"RegionId":        c.region,
			"SecurityGroupId": securityGroupID,
		}
		if next != "" {
			params["NextToken"] = next
		}
		var resp sgAttrResp
		if err := c.call(ctx, "DescribeSecurityGroupAttribute", params, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Permissions.Permission...)
		if resp.NextToken == "" {
			return all, nil
		}
		next = resp.NextToken
	}
	return all, nil
}

// authorize 为规则授权新源 IP(协议/端口/描述沿用旧规则,只换 SourceCidrIp)。
func (c *sgClient) authorize(ctx context.Context, securityGroupID string, rule sgPermission, sourceCidrIP string) error {
	return c.call(ctx, "AuthorizeSecurityGroup", map[string]string{
		"RegionId":        c.region,
		"SecurityGroupId": securityGroupID,
		"IpProtocol":      rule.IPProtocol,
		"PortRange":       rule.PortRange,
		"SourceCidrIp":    sourceCidrIP,
		"Description":     rule.Description,
	}, nil)
}

// revoke 回收旧源 IP 的规则(revoke 不按描述删,按协议/端口/源段定位)。
func (c *sgClient) revoke(ctx context.Context, securityGroupID string, rule sgPermission) error {
	return c.call(ctx, "RevokeSecurityGroup", map[string]string{
		"RegionId":        c.region,
		"SecurityGroupId": securityGroupID,
		"IpProtocol":      rule.IPProtocol,
		"PortRange":       rule.PortRange,
		"SourceCidrIp":    rule.SourceCidrIP,
	}, nil)
}

// ─── 公网 IP 探测 ──────────────────────────────────────────────────────────────

// fetchPublicIPv4 取本机公网 IPv4(依次容错 sgIPServices)。
func fetchPublicIPv4(ctx context.Context) (string, error) {
	return fetchPublicIPv4From(ctx, sgIPServices)
}

// fetchPublicIPv4From 依次尝试探测服务:强制 IPv4 拨号(安全组 SourceCidrIp 走 IPv4,
// Windows 双栈下默认拨号可能拿到 v6),响应整体须为一个合法 IPv4。全部失败才报错
// (聚合各服务原因,便于排障)。
func fetchPublicIPv4From(ctx context.Context, services []string) (string, error) {
	hc := &http.Client{
		Timeout: secGroupIPTimeout,
		Transport: &http.Transport{
			// 强制 tcp4:目标解析出的 v6 地址直接跳过。
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp4", addr)
			},
		},
	}
	var errs []string
	for _, svc := range services {
		ctx, cancel := context.WithTimeout(ctx, secGroupIPTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc, nil)
		if err == nil {
			req.Header.Set("User-Agent", "curl/8.0")
			var resp *http.Response
			resp, err = hc.Do(req)
			if err == nil {
				body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4096))
				_ = resp.Body.Close()
				if rerr != nil {
					err = rerr
				} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					err = fmt.Errorf("HTTP %d", resp.StatusCode)
				} else {
					ip := strings.TrimSpace(string(body))
					if v := net.ParseIP(ip); v != nil && v.To4() != nil {
						cancel()
						return ip, nil
					}
					err = fmt.Errorf("返回的不是合法 IPv4")
				}
			}
		}
		errs = append(errs, svc+": "+err.Error())
		cancel()
	}
	return "", fmt.Errorf("获取公网 IP 失败:%s", strings.Join(errs, "; "))
}

// ─── 同步逻辑(Runtime 侧) ────────────────────────────────────────────────────

// reconcileSecurityGroup 是安全组同步入口(对账循环每拍调用):
//   - connectPending(即将发起/重建连接):强制复查 —— 至少取一次公网 IP;IP 与配置指纹
//     均未变则不调阿里云,变了才逐组更新;
//   - 稳态:按 secGroupRefreshInterval 节流复查,公网 IP 变化即自愈;
//   - 失败 best-effort:记状态与日志、退避后随节拍重试,不返回错误、不阻断组网。
func (rt *Runtime) reconcileSecurityGroup(ctx context.Context, cfg Config, connectPending bool) {
	if !cfg.AliyunReady() {
		rt.mu.Lock()
		rt.sgApplied, rt.sgStatus = "", nil
		rt.sgLastIP, rt.sgLastRun, rt.sgLastErr = "", time.Time{}, ""
		rt.mu.Unlock()
		return
	}
	rt.mu.Lock()
	due := connectPending || !time.Now().Before(rt.sgNextCheck)
	rt.mu.Unlock()
	if !due {
		return
	}

	ip, err := rt.sgIPFetch(ctx)
	if err != nil {
		rt.sgRecordFailure(err)
		return
	}
	fp := sgConfigFingerprint(cfg) + "|" + ip
	rt.mu.Lock()
	unchanged := fp == rt.sgApplied
	rt.mu.Unlock()
	if unchanged {
		// IP 与配置都没变且上轮全部成功:本轮不触阿里云,只顺延复查节拍。
		rt.mu.Lock()
		rt.sgLastIP, rt.sgLastRun = ip, time.Now()
		rt.sgNextCheck = time.Now().Add(secGroupRefreshInterval)
		rt.mu.Unlock()
		return
	}

	upd, skip, warn, fail, firstErr := rt.updateAllGroups(ctx, cfg, ip)
	rt.mu.Lock()
	if fail == 0 {
		rt.sgApplied, rt.sgLastErr = fp, ""
		rt.sgNextCheck = time.Now().Add(secGroupRefreshInterval)
	} else {
		// 部分失败:不记指纹(下轮即使 IP 未变也重试失败组),按退避提前复查。
		rt.sgLastErr = firstErr
		rt.sgNextCheck = time.Now().Add(secGroupRetryBackoff)
	}
	rt.mu.Unlock()
	log.Printf("[pwclient] 安全组同步完成(公网 IP %s):更新 %d,跳过 %d,告警 %d,失败 %d",
		ip, upd, skip, warn, fail)
}

// updateAllGroups 逐组更新(单组失败不中断其余组),返回统计与首个错误。
func (rt *Runtime) updateAllGroups(ctx context.Context, cfg Config, ip string) (upd, skip, warn, fail int, firstErr string) {
	status := make([]SecGroupItemStatus, 0, len(cfg.Aliyun.Groups))
	for _, g := range cfg.Aliyun.Groups {
		item := SecGroupItemStatus{RegionID: g.RegionID, SecurityGroupID: g.SecurityGroupID,
			Description: g.Description, Status: "pending"}
		st, err := rt.updateGroup(ctx, rt.sgClientFor(cfg, g.RegionID), g, ip)
		switch {
		case err != nil:
			fail++
			item.Status, item.LastErr = "fail", err.Error()
			if firstErr == "" {
				firstErr = err.Error()
			}
			log.Printf("[pwclient] 安全组 %s(%s %s)更新失败:%v", g.Description, g.RegionID, g.SecurityGroupID, err)
		case st == "update":
			upd++
			item.Status = "update"
		case st == "warn":
			warn++
			item.Status = "warn"
		default:
			skip++
			item.Status = "skip"
		}
		status = append(status, item)
	}
	rt.mu.Lock()
	rt.sgStatus = status
	rt.sgLastIP, rt.sgLastRun = ip, time.Now()
	rt.mu.Unlock()
	return upd, skip, warn, fail, firstErr
}

// updateGroup 执行单组「查规则 → 按 description 锚点比对 → 先授权新 IP 再回收旧 IP」:
// 新规则复制旧规则的协议/端口/描述,只换源 IP;顺序先加后删 —— 中途失败最多残留一条
// 过期规则,不会把自己挡在门外。返回 update | skip | warn。
func (rt *Runtime) updateGroup(ctx context.Context, cli sgAPI, g AliyunGroupConfig, newIP string) (string, error) {
	perms, err := cli.describePermissions(ctx, g.SecurityGroupID)
	if err != nil {
		return "", err
	}
	var matched []sgPermission
	for _, p := range perms {
		if p.Description == g.Description {
			matched = append(matched, p)
		}
	}
	if len(matched) == 0 {
		log.Printf("[pwclient] [警告] %s(%s %s):安全组中不存在该描述的规则;请先在阿里云控制台按该描述创建放行规则", g.Description, g.RegionID, g.SecurityGroupID)
		return "warn", nil
	}
	var stale []sgPermission
	for _, p := range matched {
		if !sgSourceEqualsIP(p.SourceCidrIP, newIP) {
			stale = append(stale, p)
		}
	}
	if len(stale) == 0 {
		log.Printf("[pwclient] [跳过] %s(%s %s):规则已是最新 IP %s", g.Description, g.RegionID, g.SecurityGroupID, newIP)
		return "skip", nil
	}
	for _, old := range stale {
		if err := cli.authorize(ctx, g.SecurityGroupID, old, newIP); err != nil {
			return "", err
		}
		if err := cli.revoke(ctx, g.SecurityGroupID, old); err != nil {
			return "", err
		}
		log.Printf("[pwclient] [更新] %s(%s %s):%s %s 源 %s → %s",
			g.Description, g.RegionID, g.SecurityGroupID, old.IPProtocol, old.PortRange, old.SourceCidrIP, newIP)
	}
	return "update", nil
}

// sgClientFor 取 region 对应的 ECS 客户端(按 region+AK 缓存;AK 轮换后指纹变化触发
// 重建,不会复用旧凭据的客户端)。
func (rt *Runtime) sgClientFor(cfg Config, region string) sgAPI {
	key := region + "|" + cfg.Aliyun.AccessKeyID + "|" + secretFingerprint(cfg.Aliyun.AccessKeySecret)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if c, ok := rt.sgClients[key]; ok {
		return c
	}
	c := rt.newSGClient(cfg.Aliyun.AccessKeyID, cfg.Aliyun.AccessKeySecret, region)
	if rt.sgClients == nil {
		rt.sgClients = map[string]sgAPI{}
	}
	rt.sgClients[key] = c
	return c
}

// sgRecordFailure 记一次同步失败(状态 + 日志 + 退避)。
func (rt *Runtime) sgRecordFailure(err error) {
	rt.mu.Lock()
	rt.sgLastErr = err.Error()
	rt.sgNextCheck = time.Now().Add(secGroupRetryBackoff)
	rt.mu.Unlock()
	log.Printf("[pwclient] 安全组同步失败:%v", err)
}

// sgConfigFingerprint 把安全组配置压成指纹(含密钥哈希:AK 轮换后即使 IP 未变也强制
// 重查;指纹绝不回显密钥)。
func sgConfigFingerprint(cfg Config) string {
	parts := make([]string, 0, len(cfg.Aliyun.Groups)*3+2)
	parts = append(parts, cfg.Aliyun.AccessKeyID, secretFingerprint(cfg.Aliyun.AccessKeySecret))
	for _, g := range cfg.Aliyun.Groups {
		parts = append(parts, g.RegionID, g.SecurityGroupID, g.Description)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:4])
}

// sgSourceEqualsIP 判断规则的源 CIDR 是否已指向该 IP:阿里云回显可能是裸 IP 或 IP/32,
// 两者等价;其他 CIDR(如 0.0.0.0/0、网段)一律视为不同(锚点语义:该描述的规则始终
// 追踪本机 IP)。
func sgSourceEqualsIP(cidr, ip string) bool {
	if cidr == ip {
		return true
	}
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	ones, bits := network.Mask.Size()
	return bits == 32 && ones == 32 && network.IP.Equal(net.ParseIP(ip))
}

// ─── 状态回显 ──────────────────────────────────────────────────────────────────

// SecGroupItemStatus 是单个安全组的最近一次同步结果。
type SecGroupItemStatus struct {
	RegionID        string `json:"regionId"`
	SecurityGroupID string `json:"securityGroupId"`
	Description     string `json:"description"`
	Status          string `json:"status"` // update | skip | warn | fail | pending
	LastErr         string `json:"lastErr"`
}

// SecGroupStatus 是安全组同步的整体回显:前半是配置回显(GUI 编辑框回填;AccessKeyId
// 非机密可回显,Secret 只报存在性),后半是最近一次同步结果。绝不包含 AK/SK 明文。
type SecGroupStatus struct {
	Configured  bool                 `json:"configured"`
	AccessKeyID string               `json:"accessKeyId"`
	HasSecret   bool                 `json:"hasSecret"`
	Groups      []AliyunGroupConfig  `json:"groups"`
	LastIP      string               `json:"lastIp"`
	LastRun     string               `json:"lastRun"` // RFC3339;空 = 从未执行
	LastErr     string               `json:"lastErr"`
	Results     []SecGroupItemStatus `json:"results"`
}

// sgSnapshot 采集安全组同步状态(Snapshot 组装)。
func (rt *Runtime) sgSnapshot(cfg Config) SecGroupStatus {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	groups := cfg.Aliyun.Groups
	if groups == nil {
		groups = []AliyunGroupConfig{}
	}
	st := SecGroupStatus{
		Configured:  cfg.AliyunReady(),
		AccessKeyID: cfg.Aliyun.AccessKeyID,
		HasSecret:   cfg.Aliyun.AccessKeySecret != "",
		Groups:      groups,
		LastIP:      rt.sgLastIP,
		LastErr:     rt.sgLastErr,
		Results:     rt.sgStatus,
	}
	if !rt.sgLastRun.IsZero() {
		st.LastRun = rt.sgLastRun.Format(time.RFC3339)
	}
	if st.Results == nil {
		st.Results = []SecGroupItemStatus{}
	}
	return st
}
