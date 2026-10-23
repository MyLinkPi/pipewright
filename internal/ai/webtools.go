// webtools.go 是「网页搜索/抓取」工具的适配层:把 AI 配置(独立 web 子配置 +
// 回退 LLM 配置)解析为有效目的地,并实现 opschat.WebBackend 端口(DeepSeek
// Search & Fetch API:POST {base}/tools/search|fetch,Bearer 与平台 API Key 同源)。
// 密钥 vault 解密即用即弃、登记共享 Masker;错误一律人读中文,绝无密钥。

package ai

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/opschat"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 网页工具运行预算:搜索/抓取各自的单请求超时(opschat worker 的 30s 调用窗内)
// 与响应读取/输出上限(输出再由 worker 按 64KB 会话读预算裁剪)。
const (
	webSearchTimeout   = 15 * time.Second
	webFetchTimeout    = 25 * time.Second
	webResponseLimit   = 1 << 20 // 响应体读取上限(1MB)
	webFetchTextLimit  = 96 << 10
	webSearchTextLimit = 60 << 10 // 结果 JSON 上限(保 JSON 完整,不靠 worker 硬截)
)

// normalizeWebProvider 校验并归一网页工具供应商;空串允许(= 跟随 LLM 回退)。
func normalizeWebProvider(p string) (string, error) {
	p = strings.TrimSpace(p)
	switch p {
	case "", ProviderWebDeepSeek:
		return p, nil
	default:
		return "", ErrInvalidWebConfig
	}
}

// defaultWebBaseURL 返回网页工具供应商的官方默认前缀。
func defaultWebBaseURL(provider string) string {
	if provider == ProviderWebDeepSeek {
		return defaultWebBaseURLDeepSeek
	}
	return ""
}

// validWebBaseURL 限定网页工具接入地址:http/https、含 host、无 userinfo、≤2048B。
func validWebBaseURL(base string) bool {
	if len(base) == 0 || len(base) > 2048 {
		return false
	}
	u, err := url.Parse(base)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// sameBaseIdentity 判定两个 baseUrl 是否同一目的地(scheme/host 归一大小写、
// 忽略尾部斜杠;带用户信息/查询/片段即不等)。供 web 密钥安全复用判定。
func sameBaseIdentity(a, b string) bool {
	norm := func(s string) string {
		u, e := url.Parse(strings.TrimRight(strings.TrimSpace(s), "/"))
		if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return ""
		}
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		return u.String()
	}
	return norm(a) != "" && norm(a) == norm(b)
}

// officialDeepSeekBase 判定 LLM 目的地是否 DeepSeek 官方,返回网页工具基础地址:
// openai 协议沿用用户前缀(通常 /v1);claude 协议 /anthropic 形态映射到官方 /v1
// (与 catalogDestination 同思路,仅识别官方 host,绝不套用自定义网关前缀)。
func officialDeepSeekBase(provider, base string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "api.deepseek.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" {
		return "", false
	}
	if provider == ProviderOpenAI {
		return strings.TrimRight(strings.TrimSpace(base), "/"), true
	}
	if provider == ProviderClaude {
		if path := strings.TrimRight(u.Path, "/"); path == "/anthropic" || path == "/anthropic/v1" {
			return defaultWebBaseURLDeepSeek, true
		}
	}
	return "", false
}

// saveWebSealed 计算网页工具密钥的落库密文:目的地(供应商+有效地址)不变时保留
// 既有;变更时清空要求重填;显式提供新密钥则轮换(与主密钥 sameDestination 同语义)。
func (s *service) saveWebSealed(provider, base string, in *string, existing *Config) ([]byte, error) {
	sealed := existing.webSealed
	oldProvider, oldBase := existing.Web.Provider, strings.TrimSpace(existing.Web.BaseURL)
	if oldProvider != "" && oldBase == "" {
		oldBase = defaultWebBaseURL(oldProvider)
	}
	if provider != oldProvider || (provider != "" && !sameBaseIdentity(base, oldBase)) {
		sealed = nil
	}
	if in != nil && *in != "" {
		if s.vault == nil {
			return nil, ErrVaultUnconfigured
		}
		newSealed, err := s.vault.SealSecret([]byte(*in))
		if err != nil {
			if errors.Is(err, vault.ErrVaultUnconfigured) {
				return nil, ErrVaultUnconfigured
			}
			return nil, fmt.Errorf("ai: seal web api key: %w", err)
		}
		sealed = newSealed
	}
	return sealed, nil
}

// webDestinationResult 是网页工具有效目的地的解析结果(纯本地判定,不解密)。
type webDestinationResult struct {
	base    string
	useMain bool // 密钥来源:true = 复用 LLM 主密钥,false = web 独立密钥
	ok      bool
	reason  string // ok=false 时的人读原因(关闭态由调用方判,不算故障)
}

// webDestination 解析网页工具有效目的地:
//   - 显式供应商:base = 存储地址或官方默认;密钥优先用独立密文;无独立密钥且
//     与 LLM 目的地完全一致时可安全复用主密钥(绝不把 LLM 密钥发往其他地址);
//   - 跟随 LLM(默认):LLM 已启用已配置且目的地为 DeepSeek 官方时复用其配置。
func webDestination(cfg *Config, mainSealed []byte) webDestinationResult {
	if cfg == nil {
		return webDestinationResult{ok: false, reason: "配置不可读"}
	}
	if cfg.Web.Provider != "" {
		base := strings.TrimSpace(cfg.Web.BaseURL)
		if base == "" {
			base = defaultWebBaseURL(cfg.Web.Provider)
		}
		if len(cfg.webSealed) > 0 {
			return webDestinationResult{base: base, ok: true}
		}
		if len(mainSealed) > 0 && sameBaseIdentity(base, cfg.BaseURL) {
			return webDestinationResult{base: base, useMain: true, ok: true}
		}
		return webDestinationResult{ok: false, reason: "已选择网页工具供应商但缺少 API 密钥:请填写密钥,或将接入地址填成与 LLM 一致的 DeepSeek 官方地址以复用其密钥"}
	}
	if !cfg.Enabled || !cfg.Configured {
		return webDestinationResult{ok: false, reason: "未单独配置网页工具,且 LLM 未启用或未配置:请先配置并启用 LLM(DeepSeek),或在下方显式配置"}
	}
	if base, ok := officialDeepSeekBase(cfg.Provider, cfg.BaseURL); ok {
		return webDestinationResult{base: base, useMain: true, ok: true}
	}
	return webDestinationResult{ok: false, reason: "未单独配置网页工具,而 LLM 目的地不是 DeepSeek 官方地址:请在下方显式配置 DeepSeek"}
}

// webAvailability 供 load 填 Config.Web:关闭态 Available=false 且不报警;
// 打开后按 webDestination 解析,不可用时给人读原因。
func webAvailability(cfg *Config, mainSealed []byte) (bool, string) {
	if !cfg.Web.Enabled {
		return false, ""
	}
	dest := webDestination(cfg, mainSealed)
	return dest.ok, dest.reason
}

// ─── opschat.WebBackend 实现 ────────────────────────────────────────────────

type webTools struct {
	service *service
	masker  *mask.Masker
	pin     opsModel
}

// NewWebTools 把 AI 配置服务适配为 opschat 网页工具后端:existing 须为 ai.New
// 构造的实例,masker 为共享脱敏器(密钥登记防回显)。任一不满足返回 nil
// (装配层视 nil 为网页工具未装配)。
func NewWebTools(existing Service, masker *mask.Masker) opschat.WebBackend {
	s, ok := existing.(*service)
	if !ok || s == nil || masker == nil {
		return nil
	}
	return &webTools{service: s, masker: masker, pin: opsModel{service: s, masker: masker}}
}

func (w *webTools) Available(ctx context.Context) bool {
	cfg, mainSealed, err := w.service.load(ctx)
	if err != nil || cfg == nil || !cfg.Web.Enabled {
		return false
	}
	return webDestination(cfg, mainSealed).ok
}

// resolve 解析有效目的地并解密密钥(即用即弃;登记共享 Masker)。
func (w *webTools) resolve(ctx context.Context, cfg *Config, mainSealed []byte) (string, string, error) {
	if cfg == nil || !cfg.Web.Enabled {
		return "", "", ErrWebNotConfigured
	}
	dest := webDestination(cfg, mainSealed)
	if !dest.ok {
		return "", "", ErrWebNotConfigured
	}
	sealed := mainSealed
	if !dest.useMain {
		sealed = cfg.webSealed
	}
	if len(sealed) == 0 {
		return "", "", ErrWebNotConfigured
	}
	key, err := w.service.openSecret(sealed)
	if err != nil {
		return "", "", ErrWebNotConfigured
	}
	w.masker.RegisterSecret(key)
	return dest.base, key, nil
}

func (w *webTools) Search(ctx context.Context, query string, count int) (string, error) {
	cfg, mainSealed, err := w.service.load(ctx)
	if err != nil {
		return "", err
	}
	return w.search(ctx, cfg, mainSealed, query, count)
}

func (w *webTools) Fetch(ctx context.Context, rawURL string) (string, error) {
	cfg, mainSealed, err := w.service.load(ctx)
	if err != nil {
		return "", err
	}
	return w.fetch(ctx, cfg, mainSealed, rawURL)
}

func (w *webTools) search(ctx context.Context, cfg *Config, mainSealed []byte, query string, count int) (string, error) {
	base, key, err := w.resolve(ctx, cfg, mainSealed)
	if err != nil {
		return "", err
	}
	if count < 1 {
		count = 5
	}
	if count > 10 {
		count = 10
	}
	ctx, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"query": query, "count": count, "need_content": false})
	body, err := w.postJSON(ctx, "search", base, key, payload, webSearchTimeout)
	if err != nil {
		return "", err
	}
	return parseWebSearch(body)
}

func (w *webTools) fetch(ctx context.Context, cfg *Config, mainSealed []byte, rawURL string) (string, error) {
	base, key, err := w.resolve(ctx, cfg, mainSealed)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"url": rawURL})
	body, err := w.postJSON(ctx, "fetch", base, key, payload, webFetchTimeout)
	if err != nil {
		return "", err
	}
	return parseWebFetch(body)
}

// postJSON 发一次网页工具 API 请求:Bearer 认证、IP-pin + 禁代理 + 禁重定向
// (与 AgentRound 同款 transport;超时取网页工具预算,不受注入客户端 8s 上限)
// 、限界读取;非 2xx 映射为人读中文错误(绝无密钥)。
func (w *webTools) postJSON(ctx context.Context, resource, base, key string, payload []byte, timeout time.Duration) ([]byte, error) {
	destination, err := opsResolve(ctx, base, w.pin.lookupIP)
	if err != nil {
		return nil, errors.New("接入地址不被允许:仅 http/https,且不可指向云元数据/链路本地地址")
	}
	client := *w.service.client
	client.Timeout = timeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cleanup := func() {}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if original, ok := transport.(*http.Transport); ok {
		pinned := original.Clone()
		pinned.Proxy = nil
		pinned.DialContext = destination.dial(w.pin.dialContext)
		pinned.DialTLS = nil
		pinned.DialTLSContext = nil
		if pinned.TLSClientConfig == nil {
			pinned.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		pinned.TLSClientConfig.ServerName = destination.host
		pinned.DisableKeepAlives = true
		client.Transport = pinned
		cleanup = pinned.CloseIdleConnections
	}
	defer cleanup()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/tools/"+resource, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("请求构造失败:接入地址可能非法")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, mapWebTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, webResponseLimit+1))
	if err != nil {
		return nil, mapWebTransportError(err)
	}
	if len(raw) > webResponseLimit {
		return nil, errors.New("响应超出大小限制")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, mapWebStatusError(resource, resp.StatusCode)
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("响应不是有效的 UTF-8 文本")
	}
	return raw, nil
}

// mapWebTransportError 把传输层错误映射为人读中文(超时/连接失败;绝无密钥)。
func mapWebTransportError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "context deadline exceeded") || strings.Contains(err.Error(), "Client.Timeout") {
		return errors.New("连接超时:网页工具服务无响应,稍后可重试")
	}
	return errors.New("连接失败:无法连接到网页工具服务")
}

// mapWebStatusError 把网页工具 API 非 2xx 映射为人读中文(绝无密钥)。
func mapWebStatusError(resource string, status int) error {
	switch {
	case status == http.StatusUnauthorized:
		return errors.New("认证失败(HTTP 401):请检查 API 密钥")
	case status == http.StatusPaymentRequired:
		return errors.New("账号余额不足(HTTP 402):请充值后重试")
	case status == http.StatusForbidden:
		return errors.New("请求被安全策略拒绝(HTTP 403)")
	case status == http.StatusNotFound && resource == "fetch":
		return errors.New("目标网页无法获取(HTTP 404):地址不可用、被拦截或内容类型不支持")
	case status == http.StatusNotFound:
		return errors.New("接口不存在(HTTP 404):请检查接入地址前缀")
	case status == http.StatusRequestTimeout:
		return errors.New("读取请求超时(HTTP 408):稍后可重试")
	case status == http.StatusTooManyRequests:
		return errors.New("触发频率限制(HTTP 429):稍后可重试")
	case status >= 500:
		return fmt.Errorf("服务端错误(HTTP %d):稍后可重试", status)
	default:
		return fmt.Errorf("请求失败(HTTP %d)", status)
	}
}

// clipWebText 按字节上限截断 UTF-8 文本(不切坏多字节字符)。
func clipWebText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// webSearchRow 是回灌给模型的结果投影(content 恒不请求;字段按防御上限裁剪)。
type webSearchRow struct {
	RerankScore float64 `json:"rerank_score"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Summary     string  `json:"summary"`
	SiteName    string  `json:"site_name,omitempty"`
	Date        string  `json:"date,omitempty"`
	Authority   int     `json:"authority"`
}

// parseWebSearch 解析 Search 响应并重排为紧凑有界 JSON(丢 content/图标等无用
// 字段;结果条数与总体积封顶,保证 JSON 完整不靠外层硬截)。
func parseWebSearch(raw []byte) (string, error) {
	var body struct {
		Results []struct {
			RerankScore float64 `json:"rerank_score"`
			Title       string  `json:"title"`
			URL         string  `json:"url"`
			Snippet     string  `json:"snippet"`
			Summary     string  `json:"summary"`
			SiteName    *string `json:"site_name"`
			Date        *string `json:"date"`
			Authority   int     `json:"authority"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", errors.New("搜索响应解析失败")
	}
	rows := make([]webSearchRow, 0, len(body.Results))
	for _, r := range body.Results {
		if len(rows) >= 10 {
			break
		}
		row := webSearchRow{
			RerankScore: r.RerankScore,
			Title:       clipWebText(r.Title, 256),
			URL:         clipWebText(r.URL, 2048),
			Snippet:     clipWebText(r.Snippet, 1024),
			Summary:     clipWebText(r.Summary, 2048),
			Authority:   r.Authority,
		}
		if r.SiteName != nil {
			row.SiteName = clipWebText(*r.SiteName, 128)
		}
		if r.Date != nil {
			row.Date = clipWebText(*r.Date, 64)
		}
		rows = append(rows, row)
	}
	// 总体积兜底:超限时从尾部丢整条(保 JSON 完整),直到落回上限内。
	for len(rows) > 1 {
		encoded, err := json.Marshal(map[string]any{"results": rows})
		if err == nil && len(encoded) <= webSearchTextLimit {
			return string(encoded), nil
		}
		rows = rows[:len(rows)-1]
	}
	encoded, err := json.Marshal(map[string]any{"results": rows})
	if err != nil {
		return "", errors.New("搜索结果序列化失败")
	}
	return string(encoded), nil
}

// parseWebFetch 解析 Fetch 响应为有界 Markdown(标题 + 正文;截断加显式标记)。
func parseWebFetch(raw []byte) (string, error) {
	var body struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", errors.New("抓取响应解析失败")
	}
	text := "# " + clipWebText(body.Title, 512) + "\n\n" + body.Content
	if len(text) > webFetchTextLimit {
		text = clipWebText(text, webFetchTextLimit) + "\n\n[...truncated]"
	}
	return text, nil
}

// ─── 设置页测试(真实搜索探测,与 LLM 真实推理探测同哲学) ────────────────────

// WebTestInput 是网页工具探测入参(draft;省略字段回退已存 web 配置,密钥回退
// 到已存 web 密文或同目的地 LLM 密钥)。
type WebTestInput struct {
	Provider *string
	BaseURL  *string
	APIKey   *string
}

// WebTest 向解析出的网页工具目的地发一次 count=1 的真实搜索,验证 网络/TLS/
// 鉴权/余额可用。按 DeepSeek 计费规则,成功才计费(约千次 5 元)。
func (s *service) WebTest(ctx context.Context, in WebTestInput) (*TestResult, error) {
	saved, mainSealed, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	provider := saved.Web.Provider
	if in.Provider != nil {
		p, perr := normalizeWebProvider(*in.Provider)
		if perr != nil {
			return nil, perr
		}
		provider = p
	}
	base := strings.TrimSpace(saved.Web.BaseURL)
	if in.BaseURL != nil {
		base = strings.TrimSpace(*in.BaseURL)
	}
	if provider != "" && base == "" {
		base = defaultWebBaseURL(provider)
	}
	if provider == "" {
		if base != "" {
			return nil, ErrInvalidWebConfig
		}
	} else if !validWebBaseURL(base) {
		return nil, ErrInvalidWebConfig
	}

	// 构造探测用合成配置(Enabled=true:探测的是「打开后会用到的目的地」)。
	cfg := &Config{Provider: saved.Provider, BaseURL: saved.BaseURL, Enabled: saved.Enabled, Configured: saved.Configured,
		Web: WebConfig{Enabled: true, Provider: provider, BaseURL: base}}
	// 密钥来源:draft 显式提供 → 探测即用(临时密封进合成配置,不落库);否则仅当
	// 目的地与已存一致时沿用已存 web 密文(与主密钥 Test 的 sameDestination 语义
	// 对齐),换目的地即不携带旧密钥;LLM 主密钥复用交给 webDestination 判定。
	if in.APIKey != nil && *in.APIKey != "" {
		if s.vault == nil {
			return nil, ErrVaultUnconfigured
		}
		sealed, serr := s.vault.SealSecret([]byte(*in.APIKey))
		if serr != nil {
			return nil, ErrVaultUnconfigured
		}
		cfg.webSealed = sealed
	} else {
		oldProvider, oldBase := saved.Web.Provider, strings.TrimSpace(saved.Web.BaseURL)
		if oldProvider != "" && oldBase == "" {
			oldBase = defaultWebBaseURL(oldProvider)
		}
		if provider == oldProvider && (provider == "" || sameBaseIdentity(base, oldBase)) {
			cfg.webSealed = saved.webSealed
		}
	}

	w := &webTools{service: s, masker: mask.NewMasker(), pin: opsModel{service: s, masker: mask.NewMasker()}}
	dest := webDestination(cfg, mainSealed)
	if !dest.ok {
		return &TestResult{OK: false, Error: dest.reason}, nil
	}

	start := time.Now()
	_, err = w.search(ctx, cfg, mainSealed, "ping", 1)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return &TestResult{OK: false, LatencyMs: latency, Error: err.Error()}, nil
	}
	return &TestResult{OK: true, LatencyMs: latency, Detail: "搜索成功(已发出一次真实搜索请求)"}, nil
}
