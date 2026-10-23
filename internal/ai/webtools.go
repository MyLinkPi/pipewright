// webtools.go 是「网页搜索/抓取」工具的适配层:把 AI 配置(独立 web 子配置 +
// 复用 LLM 配置)解析为有效目的地,并实现 opschat.WebBackend 端口。
// 供应商按协议各自适配(webProviderAdapter,当前 DeepSeek / Tavily / 博查),
// 均为 Bearer 认证;密钥 vault 解密即用即弃、登记共享 Masker;错误一律人读
// 中文,绝无密钥。

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
	webSearchTextLimit = 60 << 10 // 结果 JSON 上限(保 JSON 完整,不靠外层硬截)
	webSearchMaxRows   = 10       // 回灌给模型的结果条数上限(各供应商统一)
)

// normalizeWebProvider 校验并归一网页工具供应商;空串允许(= 默认 DeepSeek,
// 兼容「供应商类型未显式选择」的存量行)。
func normalizeWebProvider(p string) (string, error) {
	p = strings.TrimSpace(p)
	switch p {
	case "", ProviderWebDeepSeek, ProviderWebTavily, ProviderWebBocha, ProviderWebOllama, ProviderWebGLM:
		return p, nil
	default:
		return "", ErrInvalidWebConfig
	}
}

// defaultWebBaseURL 返回网页工具供应商的官方默认前缀。
func defaultWebBaseURL(provider string) string {
	switch provider {
	case ProviderWebDeepSeek:
		return defaultWebBaseURLDeepSeek
	case ProviderWebTavily:
		return defaultWebBaseURLTavily
	case ProviderWebBocha:
		return defaultWebBaseURLBocha
	case ProviderWebOllama:
		return defaultWebBaseURLOllama
	case ProviderWebGLM:
		return defaultWebBaseURLGLM
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

// webReusesLLMBase 列出工具接口与 LLM 同 base 同 Key 的供应商:地址留空时
// 默认复用 LLM 目的地(DeepSeek /v1/tools/*、GLM /web_search|/reader 均与各自
// LLM API 同源),密钥复用仍受同目的地校验收口。
var webReusesLLMBase = map[string]bool{
	ProviderWebDeepSeek: true,
	ProviderWebGLM:      true,
}

// webDestinationResult 是网页工具有效目的地的解析结果(纯本地判定,不解密)。
type webDestinationResult struct {
	provider string
	base     string
	useMain  bool // 密钥来源:true = 复用 LLM 主密钥,false = web 独立密钥
	ok       bool
	reason   string // ok=false 时的人读原因(关闭态由调用方判,不算故障)
}

// webDestination 解析网页工具有效目的地。供应商类型与地址/密钥三者相互独立:
//   - 类型:Web.Provider 决定协议方言与官方默认地址(空 = 默认 deepseek;
//     新供应商 = 扩枚举 + 默认地址 + 注册适配器,不改解析结构);
//   - 地址:Web.BaseURL 优先;留空时 DeepSeek / GLM 复用 LLM 目的地(工具接口
//     与 LLM 同源,密钥本就发往同一目的地,无新增暴露面;claude 协议仅 DeepSeek
//     /anthropic 官方形态映射到 /v1),其余供应商回退各自官方默认;
//   - 密钥:web 独立密文优先;否则当有效地址与 LLM 目的地一致时复用主密钥
//     (绝不把 LLM 密钥发往其他地址)。
func webDestination(cfg *Config, mainSealed []byte) webDestinationResult {
	if cfg == nil {
		return webDestinationResult{ok: false, reason: "配置不可读"}
	}
	provider := cfg.Web.Provider
	if provider == "" {
		provider = ProviderWebDeepSeek
	}
	// llmBase 是 LLM 目的地在网页工具协议下的等价地址(不可复用 = 空串)。
	llmBase := ""
	if cfg.Provider == ProviderOpenAI && strings.TrimSpace(cfg.BaseURL) != "" {
		llmBase = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	} else if b, ok := officialDeepSeekBase(cfg.Provider, cfg.BaseURL); ok {
		llmBase = b
	}
	base := strings.TrimSpace(cfg.Web.BaseURL)
	if base == "" {
		if webReusesLLMBase[provider] && llmBase != "" {
			base = llmBase
		} else {
			base = defaultWebBaseURL(provider)
		}
	}
	if len(cfg.webSealed) > 0 {
		return webDestinationResult{provider: provider, base: base, ok: true}
	}
	if len(mainSealed) > 0 && llmBase != "" && sameBaseIdentity(base, llmBase) {
		return webDestinationResult{provider: provider, base: base, useMain: true, ok: true}
	}
	if webReusesLLMBase[provider] {
		return webDestinationResult{provider: provider, base: base, ok: false, reason: "网页工具缺少 API 密钥:请填写密钥,或将接入地址留空(复用 LLM 地址)以复用其密钥"}
	}
	return webDestinationResult{provider: provider, base: base, ok: false, reason: "网页工具缺少 API 密钥:请填写该供应商的 API 密钥"}
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

// ─── 供应商协议适配器 ─────────────────────────────────────────────────────────

// webProviderAdapter 是网页工具供应商的协议适配器:请求构造 + 响应重排为有界
// 文本(认证统一 Bearer,由 post 注入)。新增供应商 = 实现本接口 + 在 webProviders
// 注册 + 设置页下拉加一项。
type webProviderAdapter interface {
	// searchRequest 构造搜索请求端点与 JSON 体(query 已校验;count 已钳制 1..10)。
	searchRequest(base, query string, count int) (endpoint string, payload []byte)
	// parseSearch 把搜索响应重排为紧凑有界 JSON(统一 webSearchRow 投影)。
	parseSearch(raw []byte) (string, error)
	// fetchRequest 构造抓取请求;不支持抓取的供应商返回 ok=false。
	fetchRequest(base, rawURL string) (endpoint string, payload []byte, ok bool)
	// parseFetch 把抓取响应重排为有界 Markdown。
	parseFetch(raw []byte) (string, error)
	// fetchUnsupported 非空 = 该供应商不提供抓取接口(值为人读错误文案)。
	fetchUnsupported() string
}

var webProviders = map[string]webProviderAdapter{
	ProviderWebDeepSeek: deepSeekWeb{},
	ProviderWebTavily:   tavilyWeb{},
	ProviderWebBocha:    bochaWeb{},
	ProviderWebOllama:   ollamaWeb{},
	ProviderWebGLM:      glmWeb{},
}

// ── DeepSeek:POST {base}/tools/search|fetch,与 LLM 平台共用 Key。 ──────────

type deepSeekWeb struct{}

func (deepSeekWeb) searchRequest(base, query string, count int) (string, []byte) {
	payload, _ := json.Marshal(map[string]any{"query": query, "count": count, "need_content": false})
	return strings.TrimRight(base, "/") + "/tools/search", payload
}

func (deepSeekWeb) parseSearch(raw []byte) (string, error) {
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
	return webSearchRowsJSON(rows)
}

func (deepSeekWeb) fetchRequest(base, rawURL string) (string, []byte, bool) {
	payload, _ := json.Marshal(map[string]any{"url": rawURL})
	return strings.TrimRight(base, "/") + "/tools/fetch", payload, true
}

func (deepSeekWeb) parseFetch(raw []byte) (string, error) {
	var body struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", errors.New("抓取响应解析失败")
	}
	return clipFetchMarkdown(body.Title, body.Content), nil
}

func (deepSeekWeb) fetchUnsupported() string { return "" }

// ── Tavily:POST {base}/search|extract,Bearer tvly-*(search+extract)。 ──────

type tavilyWeb struct{}

func (tavilyWeb) searchRequest(base, query string, count int) (string, []byte) {
	payload, _ := json.Marshal(map[string]any{"query": query, "max_results": count, "include_published_date": true})
	return strings.TrimRight(base, "/") + "/search", payload
}

func (tavilyWeb) parseSearch(raw []byte) (string, error) {
	var body struct {
		Results []struct {
			Title         string  `json:"title"`
			URL           string  `json:"url"`
			Content       string  `json:"content"`
			Score         float64 `json:"score"`
			PublishedDate string  `json:"published_date"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", errors.New("搜索响应解析失败")
	}
	rows := make([]webSearchRow, 0, len(body.Results))
	for _, r := range body.Results {
		rows = append(rows, webSearchRow{
			RerankScore: r.Score,
			Title:       clipWebText(r.Title, 256),
			URL:         clipWebText(r.URL, 2048),
			Snippet:     clipWebText(r.Content, 1024),
			Date:        clipWebText(r.PublishedDate, 64),
		})
	}
	return webSearchRowsJSON(rows)
}

func (tavilyWeb) fetchRequest(base, rawURL string) (string, []byte, bool) {
	payload, _ := json.Marshal(map[string]any{"urls": []string{rawURL}, "format": "markdown"})
	return strings.TrimRight(base, "/") + "/extract", payload, true
}

func (tavilyWeb) parseFetch(raw []byte) (string, error) {
	var body struct {
		Results []struct {
			URL        string `json:"url"`
			RawContent string `json:"raw_content"`
		} `json:"results"`
		FailedResults []struct {
			URL   string `json:"url"`
			Error string `json:"error"`
		} `json:"failed_results"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", errors.New("抓取响应解析失败")
	}
	if len(body.Results) == 0 {
		if len(body.FailedResults) > 0 {
			return "", errors.New("目标网页无法获取:" + clipWebText(body.FailedResults[0].Error, 256))
		}
		return "", errors.New("抓取响应为空")
	}
	return clipFetchMarkdown(body.Results[0].URL, body.Results[0].RawContent), nil
}

func (tavilyWeb) fetchUnsupported() string { return "" }

// ── 博查:POST {base}/web-search,Bing 兼容响应;仅搜索,无抓取接口。 ─────────

type bochaWeb struct{}

func (bochaWeb) searchRequest(base, query string, count int) (string, []byte) {
	payload, _ := json.Marshal(map[string]any{"query": query, "summary": true, "count": count})
	return strings.TrimRight(base, "/") + "/web-search", payload
}

func (bochaWeb) parseSearch(raw []byte) (string, error) {
	var body struct {
		WebPages *struct {
			Value []struct {
				Name            string `json:"name"`
				URL             string `json:"url"`
				Snippet         string `json:"snippet"`
				Summary         string `json:"summary"`
				SiteName        string `json:"siteName"`
				DatePublished   string `json:"datePublished"`
				DateLastCrawled string `json:"dateLastCrawled"`
			} `json:"value"`
		} `json:"webPages"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", errors.New("搜索响应解析失败")
	}
	rows := []webSearchRow{}
	if body.WebPages != nil {
		for _, r := range body.WebPages.Value {
			date := r.DatePublished
			if date == "" {
				date = r.DateLastCrawled
			}
			rows = append(rows, webSearchRow{
				Title:    clipWebText(r.Name, 256),
				URL:      clipWebText(r.URL, 2048),
				Snippet:  clipWebText(r.Snippet, 1024),
				Summary:  clipWebText(r.Summary, 2048),
				SiteName: clipWebText(r.SiteName, 128),
				Date:     clipWebText(date, 64),
			})
		}
	}
	return webSearchRowsJSON(rows)
}

func (bochaWeb) fetchRequest(string, string) (string, []byte, bool) { return "", nil, false }

func (bochaWeb) parseFetch([]byte) (string, error) {
	return "", errors.New("抓取不可用")
}

func (bochaWeb) fetchUnsupported() string {
	return "当前网页工具供应商(博查)不提供网页抓取接口:请仅用 web_search,或在设置页切换为支持抓取的供应商(DeepSeek / Tavily)"
}

// ── Ollama:POST {base}/api/web_search|web_fetch,Bearer ollama.com API Key。 ──

type ollamaWeb struct{}

func (ollamaWeb) searchRequest(base, query string, count int) (string, []byte) {
	payload, _ := json.Marshal(map[string]any{"query": query, "max_results": count})
	return strings.TrimRight(base, "/") + "/api/web_search", payload
}

func (ollamaWeb) parseSearch(raw []byte) (string, error) {
	var body struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", errors.New("搜索响应解析失败")
	}
	rows := make([]webSearchRow, 0, len(body.Results))
	for _, r := range body.Results {
		rows = append(rows, webSearchRow{
			Title:   clipWebText(r.Title, 256),
			URL:     clipWebText(r.URL, 2048),
			Snippet: clipWebText(r.Content, 1024),
		})
	}
	return webSearchRowsJSON(rows)
}

func (ollamaWeb) fetchRequest(base, rawURL string) (string, []byte, bool) {
	payload, _ := json.Marshal(map[string]any{"url": rawURL})
	return strings.TrimRight(base, "/") + "/api/web_fetch", payload, true
}

func (ollamaWeb) parseFetch(raw []byte) (string, error) {
	var body struct {
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Links   []string `json:"links"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", errors.New("抓取响应解析失败")
	}
	return clipFetchMarkdown(body.Title, body.Content), nil
}

func (ollamaWeb) fetchUnsupported() string { return "" }

// ── GLM:POST {base}/web_search|reader,Bearer 与 GLM LLM 同 Key。 ────────────

type glmWeb struct{}

// searchRequest 构造智谱搜索请求:search_query 上限 70 字符(超长按 rune 截断,
// 平台侧 schema 即如此);search_engine 必填,固定标准版 search_std。
func (glmWeb) searchRequest(base, query string, count int) (string, []byte) {
	payload, _ := json.Marshal(map[string]any{
		"search_query":  clipWebRunes(query, 70),
		"search_engine": "search_std",
		"search_intent": false,
		"count":         count,
		"content_size":  "medium",
	})
	return strings.TrimRight(base, "/") + "/web_search", payload
}

func (glmWeb) parseSearch(raw []byte) (string, error) {
	var body struct {
		SearchResult []struct {
			Title       string `json:"title"`
			Content     string `json:"content"`
			Link        string `json:"link"`
			Media       string `json:"media"`
			PublishDate string `json:"publish_date"`
		} `json:"search_result"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", errors.New("搜索响应解析失败")
	}
	if len(body.SearchResult) == 0 && body.Error != nil && body.Error.Message != "" {
		return "", errors.New("搜索失败:" + clipWebText(body.Error.Message, 256))
	}
	rows := make([]webSearchRow, 0, len(body.SearchResult))
	for _, r := range body.SearchResult {
		rows = append(rows, webSearchRow{
			Title:    clipWebText(r.Title, 256),
			URL:      clipWebText(r.Link, 2048),
			Snippet:  clipWebText(r.Content, 1024),
			SiteName: clipWebText(r.Media, 128),
			Date:     clipWebText(r.PublishDate, 64),
		})
	}
	return webSearchRowsJSON(rows)
}

func (glmWeb) fetchRequest(base, rawURL string) (string, []byte, bool) {
	payload, _ := json.Marshal(map[string]any{"url": rawURL, "return_format": "markdown", "retain_images": false})
	return strings.TrimRight(base, "/") + "/reader", payload, true
}

func (glmWeb) parseFetch(raw []byte) (string, error) {
	var body struct {
		ReaderResult *struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"reader_result"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", errors.New("抓取响应解析失败")
	}
	if (body.ReaderResult == nil || body.ReaderResult.Content == "") && body.Error != nil && body.Error.Message != "" {
		return "", errors.New("抓取失败:" + clipWebText(body.Error.Message, 256))
	}
	if body.ReaderResult == nil {
		return "", errors.New("抓取响应为空")
	}
	return clipFetchMarkdown(body.ReaderResult.Title, body.ReaderResult.Content), nil
}

func (glmWeb) fetchUnsupported() string { return "" }

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

// resolve 解析有效供应商/目的地并解密密钥(即用即弃;登记共享 Masker)。
func (w *webTools) resolve(ctx context.Context, cfg *Config, mainSealed []byte) (string, string, string, error) {
	if cfg == nil || !cfg.Web.Enabled {
		return "", "", "", ErrWebNotConfigured
	}
	dest := webDestination(cfg, mainSealed)
	if !dest.ok {
		return "", "", "", ErrWebNotConfigured
	}
	sealed := mainSealed
	if !dest.useMain {
		sealed = cfg.webSealed
	}
	if len(sealed) == 0 {
		return "", "", "", ErrWebNotConfigured
	}
	key, err := w.service.openSecret(sealed)
	if err != nil {
		return "", "", "", ErrWebNotConfigured
	}
	w.masker.RegisterSecret(key)
	return dest.provider, dest.base, key, nil
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
	provider, base, key, err := w.resolve(ctx, cfg, mainSealed)
	if err != nil {
		return "", err
	}
	adapter, ok := webProviders[provider]
	if !ok {
		return "", ErrInvalidWebConfig
	}
	count = clampWebCount(count)
	ctx, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()
	endpoint, payload := adapter.searchRequest(base, query, count)
	body, err := w.post(ctx, "search", endpoint, key, payload, webSearchTimeout)
	if err != nil {
		return "", err
	}
	return adapter.parseSearch(body)
}

func (w *webTools) fetch(ctx context.Context, cfg *Config, mainSealed []byte, rawURL string) (string, error) {
	provider, base, key, err := w.resolve(ctx, cfg, mainSealed)
	if err != nil {
		return "", err
	}
	adapter, ok := webProviders[provider]
	if !ok {
		return "", ErrInvalidWebConfig
	}
	if msg := adapter.fetchUnsupported(); msg != "" {
		return "", errors.New(msg)
	}
	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()
	endpoint, payload, supported := adapter.fetchRequest(base, rawURL)
	if !supported {
		return "", errors.New("当前网页工具供应商不提供网页抓取接口")
	}
	body, err := w.post(ctx, "fetch", endpoint, key, payload, webFetchTimeout)
	if err != nil {
		return "", err
	}
	return adapter.parseFetch(body)
}

// clampWebCount 钳制结果条数到统一区间(缺省 5;各供应商 API 上限不同,统一
// 收口到 10 以内控制上下文体积)。
func clampWebCount(count int) int {
	if count < 1 {
		return 5
	}
	if count > webSearchMaxRows {
		return webSearchMaxRows
	}
	return count
}

// post 发一次网页工具 API 请求:Bearer 认证、IP-pin + 禁代理 + 禁重定向
// (与 AgentRound 同款 transport;超时取网页工具预算,不受注入客户端 8s 上限)
// 、限界读取;非 2xx 映射为人读中文错误(绝无密钥)。
func (w *webTools) post(ctx context.Context, resource, endpoint, key string, payload []byte, timeout time.Duration) ([]byte, error) {
	destination, err := opsResolve(ctx, endpoint, w.pin.lookupIP)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
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
		return errors.New("接口不存在(HTTP 404):当前接入地址可能不支持网页工具接口,请检查接入地址或供应商设置")
	case status == http.StatusRequestTimeout:
		return errors.New("读取请求超时(HTTP 408):稍后可重试")
	case status == http.StatusTooManyRequests:
		return errors.New("触发频率限制(HTTP 429):稍后可重试")
	case status == 432 || status == 433:
		return fmt.Errorf("配额或套餐限额已超出(HTTP %d):请升级套餐或充值", status)
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

// clipWebRunes 按 rune 数上限截断文本(用于供应商对 query 的字符数限制,
// 如智谱 search_query ≤70)。
func clipWebRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}

// webSearchRow 是回灌给模型的结果投影(各供应商统一形状;字段按防御上限裁剪)。
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

// webSearchRowsJSON 把统一行投影序列化为紧凑有界 JSON:条数封顶 + 总体积
// 超限时从尾部丢整条(保 JSON 完整,不靠外层硬截)。
func webSearchRowsJSON(rows []webSearchRow) (string, error) {
	if len(rows) > webSearchMaxRows {
		rows = rows[:webSearchMaxRows]
	}
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

// clipFetchMarkdown 把抓取正文组装为有界 Markdown(标题 + 正文;截断加显式标记)。
func clipFetchMarkdown(title, content string) string {
	text := "# " + clipWebText(title, 512) + "\n\n" + content
	if len(text) > webFetchTextLimit {
		text = clipWebText(text, webFetchTextLimit) + "\n\n[...truncated]"
	}
	return text
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
// 鉴权/配额可用(按各供应商计费规则,通常仅成功请求计费)。
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
	// 地址留空原样传给 webDestination(与运行时解析一致:DeepSeek 复用 LLM 地址,
	// 其余供应商用官方默认)。
	base := strings.TrimSpace(saved.Web.BaseURL)
	if in.BaseURL != nil {
		base = strings.TrimSpace(*in.BaseURL)
	}
	if base != "" && !validWebBaseURL(base) {
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
