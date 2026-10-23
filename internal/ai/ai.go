// Package ai 是「可配置 AI 提供商」的领域层(FR-24 / NFR-10 / Story 7.1)。
//
// 平台持有一份单例 AI 配置:provider(claude|openai|ollama)+ baseUrl + model +
// apiKey(加密)+ budget 声明 + enabled 开关。apiKey 明文经 vault secretbox(master
// key)加密后仅以**密文 BLOB** 入库;DB dump 绝无明文 key(AC-SEC)。对外(REST)
// 常态只暴露掩码;明文仅在 Test 探测时进程内解密取用、用完即弃,绝不回显/日志。
//
// master key 缺失时,涉及读已存密钥的操作降级为明确错误(ErrVaultUnconfigured),
// 平台不 panic。未配置 / 未 enabled → 下游优雅禁用,核心 CI/CD 路径完全不依赖此服务
// (NFR-10)。
//
// 本期为「最小可用」:Test 向 provider 发一次真实推理请求(max_tokens=1)校验 连通 +
// 认证 + 模型有效 + 推理权限;budget 仅存声明(monthlyTokenLimit),不强制执行/计量
// (Epic7 用量统计后做)。
package ai

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// Provider 枚举(DB 存小写字串;JSON 同名)。空串 = 未配置。
const (
	// ProviderClaude 表示 Anthropic Claude(api.anthropic.com)。
	ProviderClaude = "claude"
	// ProviderOpenAI 表示 OpenAI(api.openai.com)。
	ProviderOpenAI = "openai"
)

// 各 provider 默认 baseUrl(provider 非空但 baseUrl 为空时后端兜底)。
const (
	defaultBaseURLClaude = "https://api.anthropic.com/v1"
	defaultBaseURLOpenAI = "https://api.openai.com/v1"
)

// 网页搜索/抓取工具供应商(与 LLM provider 枚举独立;首期仅 DeepSeek)。
const (
	// ProviderWebDeepSeek 表示 DeepSeek 网页搜索/抓取(/v1/tools/search|fetch,
	// 与 DeepSeek LLM 共用平台 API Key)。
	ProviderWebDeepSeek = "deepseek"
	// defaultWebBaseURLDeepSeek 是 DeepSeek 网页工具的官方前缀。
	defaultWebBaseURLDeepSeek = "https://api.deepseek.com/v1"
)

// 领域错误。错误体永不含 apiKey 明文/密文/master key。
var (
	// ErrInvalidProvider 表示 provider 枚举非法(非 claude|openai|ollama 且非空)。
	ErrInvalidProvider = errors.New("ai: invalid provider")
	// ErrBaseURLRequired 表示 provider 非空但 baseUrl 为空(且无默认兜底)。
	ErrBaseURLRequired = errors.New("ai: base url required")
	// ErrAPIKeyRequired 表示非 ollama provider 既无既有 key 又无新 key。
	ErrAPIKeyRequired = errors.New("ai: api key required")
	// ErrVaultUnconfigured 表示保险库未配置 master key,无法加密/读取 apiKey。
	ErrVaultUnconfigured = errors.New("ai: vault unconfigured")
	// ErrAINotConfigured 表示 AI 提供商未配置 / 未启用,生成不可用(优雅降级,不致命)。
	ErrAINotConfigured = errors.New("ai: provider not configured or disabled")
	// ErrInvalidWebConfig 表示网页工具配置非法(供应商枚举/地址形态/字段搭配)。
	ErrInvalidWebConfig = errors.New("ai: invalid web tools config")
	// ErrWebNotConfigured 表示网页工具未启用或有效配置不可解析(优雅降级,不致命)。
	ErrWebNotConfigured = errors.New("ai: web tools not configured or disabled")
	// ErrGenerateFailed 表示 LLM 调用 / 响应解析失败(人读错误,绝无密钥)。
	ErrGenerateFailed = errors.New("ai: generate failed")
)

// Budget 是预算声明(本期仅存,不强制执行)。MonthlyTokenLimit 为 nil 表示不限。
type Budget struct {
	MonthlyTokenLimit *int64 `json:"monthlyTokenLimit"`
}

// 上下文管理默认值(0/缺省字段回退到此;由 opschat 注入侧消费)。
const (
	DefaultContextWindowTokens     = 200_000 // 模型上下文窗口兜底
	DefaultMaxOutputTokens         = 8_192   // 单请求输出上限(替换旧硬编码 4096)
	DefaultHistoryMessages         = 100     // 会话历史条数上限(替换旧 LIMIT 20)
	DefaultHistoryBytes            = 256 << 10
	DefaultContextKeepRecentTokens = 15_000 // 压缩时原样保留的最近消息 tokens
)

// ContextSettings 是上下文管理声明(opschat agent 的窗口预算/压缩策略)。
// 全部字段 0/nil = 用默认;越界值在 normalizeContext 归一回默认。
type ContextSettings struct {
	WindowTokens     int   `json:"windowTokens"`     // 上下文窗口 tokens(1000–2000000)
	MaxOutputTokens  int   `json:"maxOutputTokens"`  // 单请求输出上限(512–65536)
	HistoryMessages  int   `json:"historyMessages"`  // 会话历史条数上限(1–2000)
	HistoryBytes     int   `json:"historyBytes"`     // 会话历史字节上限(4KB–4MB)
	AutoCompact      *bool `json:"autoCompact"`      // nil = 开
	KeepRecentTokens int   `json:"keepRecentTokens"` // 压缩保留最近 tokens(1000–窗口50%)
	BufferTokens     int   `json:"bufferTokens"`     // 触发阈值预留;0 = 自动 max(10% 窗口, 窗口≥32K 时 16K)
}

// ContextSettingsDefault 返回全默认上下文声明(未配置/存量行 '{}' 时用)。
func ContextSettingsDefault() ContextSettings {
	return ContextSettings{}
}

// Config 是 AI 配置领域模型。APIKeyMasked 为掩码;apiKey 明文绝不在此模型常驻。
// Configured = provider 非空 +(provider=ollama 无需 key / 否则有 key)。
type Config struct {
	Configured   bool
	Enabled      bool
	Provider     string
	BaseURL      string
	Model        string
	APIKeyMasked string
	Budget       Budget
	Context      ContextSettings
	// Web 是网页搜索/抓取工具的配置声明(独立开关+可选独立目的地,回退 LLM 配置)。
	Web       WebConfig
	UpdatedAt *time.Time
	// webSealed 是网页工具独立密钥的密文(仅包内取用,绝不序列化/外泄)。
	webSealed []byte
}

// WebConfig 是网页搜索/抓取工具配置。Provider 空串 = 跟随 LLM 配置回退
// (仅当 LLM 目的地为 DeepSeek 官方时可用);BaseURL 为存储值,空 = provider
// 官方默认。Available/Reason 是开关打开时的有效配置解析结果:不可用时 Reason
// 给人读原因(关闭态 Available=false 且 Reason 为空,是常态非故障)。
type WebConfig struct {
	Enabled      bool
	Provider     string
	BaseURL      string
	APIKeyMasked string
	Available    bool
	Reason       string
}

// SaveInput 是保存配置的入参。
// APIKey 为**只写**指针:nil/空串 = 保留既有密钥(不清空);非空 = 轮换为新密钥。
// Web 子配置同语义:WebAPIKey 只写;WebProvider/WebBaseURL 变更目的地时既有
// web 密钥按同目的地规则处置(见 saveWebSealed)。
type SaveInput struct {
	Provider    string
	BaseURL     string
	Model       string
	APIKey      *string
	Budget      Budget
	Context     ContextSettings
	Enabled     bool
	WebEnabled  bool
	WebProvider string
	WebBaseURL  string
	WebAPIKey   *string
}

// TestInput 是探测连接的入参(draft;省略字段回退已保存配置)。
// 指针字段为 nil 表示回退已存值;APIKey nil = 用已存密钥。
type TestInput struct {
	Provider *string
	BaseURL  *string
	Model    *string
	APIKey   *string
}

// TestResult 是探测结果(冻结契约形状)。Error 为人读错误(绝不含密钥);成功为空。
type TestResult struct {
	OK        bool
	LatencyMs int64
	Detail    string
	Error     string
}

// Service 定义 AI 配置领域对外接口。
type Service interface {
	// Get 返回单例 AI 配置(apiKey 仅掩码)。首次无配置时返回惰性空默认
	// (provider 空 / enabled false / configured false)。
	Get(ctx context.Context) (*Config, error)
	// Save 校验并持久化配置;apiKey 省略/空保留旧密文、非空轮换(vault.SealSecret 加密)。
	// 返回更新后配置(掩码)。
	Save(ctx context.Context, in SaveInput) (*Config, error)
	// Test 向 provider 发轻量探测(注入的 http.Client),回显 ok/延迟/错误(人读,绝无密钥)。
	Test(ctx context.Context, in TestInput) (*TestResult, error)
	// AgentTest 执行「AI 测试」节点(specs/ai-test-node.md):为每条仓库需求跑一个
	// agent 会话(HTTP/浏览器动作集操作被测系统 → 结构化裁决)。裁决一律 advisory,
	// 放行决策归人工审批门;用量与网关自报费用原样累计入结果。未配置 → ErrAINotConfigured
	// (调用方按节点 onAiUnavailable 裁决)。
	AgentTest(ctx context.Context, in AITestInput) (*AITestResult, error)
	// Generate 据仓库分析 + 自然语言补充,构 prompt 调 LLM chat 生成结构化流水线提案(Story 2.5)。
	// 未配置 / 未启用 → ErrAINotConfigured;调用 / 解析失败 → 人读错误(绝无密钥)。
	Generate(ctx context.Context, in GenerateInput) (*Proposal, error)
	// Diagnose 据失败日志构 prompt 调 LLM chat 生成失败诊断(根因假说+置信度+证据;Story 7.2)。
	// **出网前必脱敏**:入参 Masker 登记的 secret 在日志进 prompt / 取证据前一律 [MASKED]。
	// 优雅降级铁律:AI 未配 / 超时 / 不可解析 / 空 hypothesis / 低质 → status=unavailable + reason,
	// **绝不返回 error 致上层 500**(本方法对降级路径返回 (*Diagnosis, nil),仅极少内部错回 error)。
	Diagnose(ctx context.Context, in DiagnoseInput) (*Diagnosis, error)
	// AnnotateRisks 对脚本步骤命令做风险标注(护城河):确定性正则预扫 + 可选 LLM 语义增强。
	// **出网前必脱敏**:入参 Masker 登记的 secret 在脚本进 prompt / 取证据前一律 [MASKED];
	// 检测到明文密钥的标注本身绝不回显该密钥。优雅降级铁律:AI 未配 / 失败 / 不可解析 → 仅返回
	// 确定性规则结果 + AIEnhanced=false + 人读 reason,**永远返回 (*RiskReport, nil),绝不 500**。
	AnnotateRisks(ctx context.Context, in AnnotateRisksInput) (*RiskReport, error)
	// CommandSuggest 把用户中文描述转为单条可执行 shell 命令 + 解释 + 风险等级(运维终端 AI 助手)。
	// **出网前必脱敏**:NL / 上下文经 Masker 脱敏;模型回的命令/解释二次脱敏。生成需 AI:未配 /
	// 未启用 → ErrAINotConfigured(上层映射「去设置配 AI」)。风险等级出库前经 classifyCommandRisk
	// 确定性复核——破坏性命令无条件升级 danger,不依赖 AI 在线(护城河)。
	CommandSuggest(ctx context.Context, in CommandSuggestInput) (*CommandSuggestion, error)
	// ExplainCommand 用中文逐段解释一条既有命令(+ 确定性风险等级)。脱敏铁律同上;未配 →
	// ErrAINotConfigured。风险等级由确定性规则给(不依赖 AI)。
	ExplainCommand(ctx context.Context, in ExplainCommandInput) (*CommandExplanation, error)
	// CompleteCommand 据已输入命令前缀补全为一条完整命令(P2 智能补全;以前缀原样开头)。
	// 脱敏铁律同上;未配 → ErrAINotConfigured(前端可退化为本地字典兜底)。短回包、低 max_tokens。
	CompleteCommand(ctx context.Context, in CompleteCommandInput) (*CompletionResult, error)
	// GenerateCompose 据中文需求生成 docker-compose.yml(容器管理「AI 生成 compose」)。
	// 生成需 AI:未配 / 未启用 → ErrAINotConfigured。NL 出网前脱敏;输出去围栏 + 二次脱敏。
	GenerateCompose(ctx context.Context, in GenerateComposeInput) (*ComposeResult, error)
}

// service 是 store + vault + 注入 http.Client 支撑的 Service 实现。
type service struct {
	db     *sql.DB
	vault  vault.Vault
	client *http.Client
}

// New 构造 Service。
//   - db:经参数化 SQL 触库。
//   - v:凭据保险库,复用其 secretbox(master key)加解密 apiKey;v 为 nil 或未配置
//     master key 时,涉及密钥的读取/加密返回 ErrVaultUnconfigured。
//   - client:Test 探测用的 HTTP 客户端(应带超时,如 ~8s);为 nil 时回退默认带超时客户端。
//     单测可注入指向本地 stub server 的 client + 短超时。
//
// 不在此做任何重活(无 init() 副作用,避免抬高空载内存)。
func New(db *sql.DB, v vault.Vault, client *http.Client) Service {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &service{db: db, vault: v, client: client}
}

func (s *service) Get(ctx context.Context) (*Config, error) {
	cfg, _, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func (s *service) Save(ctx context.Context, in SaveInput) (*Config, error) {
	provider, err := normalizeProvider(in.Provider)
	if err != nil {
		return nil, err
	}

	// 读既有行(判 apiKey 保留语义 + budget 兜底);无行视为空默认。
	existing, existingSealed, err := s.load(ctx)
	if err != nil {
		return nil, err
	}

	baseURL := strings.TrimSpace(in.BaseURL)
	if provider != "" && baseURL == "" {
		baseURL = defaultBaseURL(provider)
	}
	if provider != "" && baseURL == "" {
		return nil, ErrBaseURLRequired
	}

	// apiKey 保留语义:nil/空 → 沿用既有密文;非空 → 轮换(SealSecret 加密)。
	sealed := existingSealed
	if provider != "" && !sameDestination(provider, baseURL, existing) {
		sealed = nil
	}
	if in.APIKey != nil && *in.APIKey != "" {
		if s.vault == nil {
			return nil, ErrVaultUnconfigured
		}
		newSealed, serr := s.vault.SealSecret([]byte(*in.APIKey))
		if serr != nil {
			if errors.Is(serr, vault.ErrVaultUnconfigured) {
				return nil, ErrVaultUnconfigured
			}
			return nil, fmt.Errorf("ai: seal api key: %w", serr)
		}
		sealed = newSealed
	}

	// 非 ollama provider 须有 key(既有或新轮换)。
	if provider != "" && len(sealed) == 0 {
		return nil, ErrAPIKeyRequired
	}

	// 网页工具子配置:供应商枚举 + 地址形态 + 密钥保留/轮换(目的地变更时清空)。
	webProvider, err := normalizeWebProvider(in.WebProvider)
	if err != nil {
		return nil, err
	}
	webBaseURL := strings.TrimSpace(in.WebBaseURL)
	if webProvider == "" {
		// 跟随 LLM 回退模式:独立地址/密钥无意义,显式提供即拒绝(不静默丢弃)。
		if webBaseURL != "" || (in.WebAPIKey != nil && *in.WebAPIKey != "") {
			return nil, ErrInvalidWebConfig
		}
	} else {
		if webBaseURL == "" {
			webBaseURL = defaultWebBaseURL(webProvider)
		}
		if !validWebBaseURL(webBaseURL) {
			return nil, ErrInvalidWebConfig
		}
	}
	webSealed, err := s.saveWebSealed(webProvider, webBaseURL, in.WebAPIKey, existing)
	if err != nil {
		return nil, err
	}

	budgetJSON, err := json.Marshal(normalizeBudget(in.Budget))
	if err != nil {
		return nil, fmt.Errorf("ai: marshal budget: %w", err)
	}
	contextJSON, err := json.Marshal(normalizeContext(in.Context))
	if err != nil {
		return nil, fmt.Errorf("ai: marshal context: %w", err)
	}

	enabled := 0
	if in.Enabled {
		enabled = 1
	}
	webEnabled := 0
	if in.WebEnabled {
		webEnabled = 1
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)
	// 单例 upsert:首存 INSERT(id=1),已存 ON CONFLICT 更新各列(created_at 保留)。
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO ai_config
		   (id, provider, base_url, model, api_key_ciphertext, budget_json, context_json, enabled, created_at, updated_at,
		    web_enabled, web_provider, web_base_url, web_api_key_ciphertext)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) `+
			store.UpsertSuffix(store.DialectOf(s.db), []string{"id"},
				[]string{"provider", "base_url", "model", "api_key_ciphertext", "budget_json", "context_json", "enabled", "updated_at",
					"web_enabled", "web_provider", "web_base_url", "web_api_key_ciphertext"}),
		provider, baseURL, strings.TrimSpace(in.Model), sealed, string(budgetJSON), string(contextJSON), enabled, nowStr, nowStr,
		webEnabled, webProvider, webBaseURL, webSealed,
	)
	if err != nil {
		return nil, fmt.Errorf("ai: upsert config: %w", err)
	}
	return s.Get(ctx)
}

func (s *service) Test(ctx context.Context, in TestInput) (*TestResult, error) {
	// 读既有行作为 draft 字段的回退值(apiKey 回退到已存密钥)。
	current, existingSealed, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	existing := *current

	provider := existing.Provider
	if in.Provider != nil {
		p, perr := normalizeProvider(*in.Provider)
		if perr != nil {
			return nil, perr
		}
		provider = p
	}
	if provider == "" {
		return nil, ErrInvalidProvider
	}

	baseURL := existing.BaseURL
	if in.BaseURL != nil {
		baseURL = strings.TrimSpace(*in.BaseURL)
	}
	if baseURL == "" {
		baseURL = defaultBaseURL(provider)
	}
	if baseURL == "" {
		return nil, ErrBaseURLRequired
	}

	// apiKey:draft 非空 → 用 draft;否则解密已存密钥(ollama 不需要)。
	apiKey := ""
	if in.APIKey != nil && *in.APIKey != "" {
		apiKey = *in.APIKey
	} else if len(existingSealed) > 0 && sameDestination(provider, baseURL, &existing) {
		plain, oerr := s.openSecret(existingSealed)
		if oerr != nil {
			return nil, oerr
		}
		apiKey = plain
	}
	if apiKey == "" {
		return nil, ErrAPIKeyRequired
	}

	// model:draft 非空 → 用 draft;否则回退已存配置。真实推理探测必须有模型。
	model := existing.Model
	if in.Model != nil {
		model = strings.TrimSpace(*in.Model)
	}

	res := s.probe(ctx, provider, baseURL, model, apiKey)
	// 明文 key 用完即弃(尽力清理本地副本)。
	apiKey = ""
	return res, nil
}

// probe 向 provider 发一次**真实推理请求**(max_tokens=1,与诊断路径同一请求形态):
// 一次调用同时验证 网络/TLS、鉴权、模型 ID 有效性与推理权限。2xx → ok+延迟;
// 否则 ok=false + 人读错误(绝无密钥)。
func (s *service) probe(ctx context.Context, provider, baseURL, model, apiKey string) *TestResult {
	return (&opsModel{service: s}).probe(ctx, provider, baseURL, model, apiKey)
}

func (m *opsModel) probe(ctx context.Context, provider, baseURL, model, apiKey string) *TestResult {
	if strings.TrimSpace(model) == "" {
		return &TestResult{OK: false, Error: "真实推理测试需要模型:请先选择或填写模型 ID"}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// SSRF 收口:baseUrl 用户可控,探测向其发请求——拒云元数据/链路本地/未指定;
	// 回环仅 ollama(本地默认 localhost:11434)放行,其余 provider 拒回环;私网放行(自托管,
	// 与仓库既定策略一致)。否则管理员可借 test 探测做内网/元数据端口探活(盲 SSRF)。
	client, cleanup, pinErr := m.pinnedClient(ctx, baseURL)
	if pinErr != nil {
		return &TestResult{OK: false, Error: "baseUrl 不被允许:仅 http/https,且不可指向云元数据/链路本地地址"}
	}
	defer cleanup()

	endpoint, header, payload := inferenceProbeRequest(provider, baseURL, model, apiKey)
	opsRequestCompatibility(&Config{Provider: provider, Model: model}, baseURL, payload)
	body, err := json.Marshal(payload)
	if err != nil {
		return &TestResult{OK: false, Error: "请求构造失败:baseUrl 可能非法"}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		// URL 构造失败(如 baseUrl 非法):人读错误,绝不含 key。
		return &TestResult{OK: false, Error: "请求构造失败:baseUrl 可能非法"}
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return &TestResult{OK: false, LatencyMs: latency, Error: mapTransportError(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	// 读弃少量响应体以复用连接;不解析内容(max_tokens=1 的回复无诊断价值)。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &TestResult{
			OK:        true,
			LatencyMs: latency,
			Detail:    fmt.Sprintf("推理成功(HTTP %d)", resp.StatusCode),
		}
	}
	return &TestResult{
		OK:        false,
		LatencyMs: latency,
		Error:     mapInferenceStatusError(resp.StatusCode),
	}
}

// inferenceProbeRequest 构造真实推理探测的 URL/header/payload:per-provider 与
// chat 路径同形态,但 max_tokens=1,不期望内容。
func inferenceProbeRequest(provider, baseURL, model, apiKey string) (string, map[string]string, map[string]any) {
	header := map[string]string{"Content-Type": "application/json"}
	messages := []map[string]string{{"role": "user", "content": "ping"}}
	switch provider {
	case ProviderClaude:
		header["x-api-key"] = apiKey
		header["anthropic-version"] = "2023-06-01"
		return apiEndpoint(provider, baseURL, "messages"), header, map[string]any{
			"model":      model,
			"max_tokens": 1,
			"stream":     true,
			"messages":   messages,
		}
	default:
		header["Authorization"] = "Bearer " + apiKey
		return apiEndpoint(provider, baseURL, "chat/completions"), header, map[string]any{
			"model":      model,
			"messages":   messages,
			"max_tokens": 1,
			"stream":     true,
		}
	}
}

// mapInferenceStatusError 把推理探测的非 2xx 映射为人读错误(绝无密钥)。
// 400/404 在推理端点上最常见的根因是模型 ID 不存在或地址前缀拼错,给出指向性提示。
func mapInferenceStatusError(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Sprintf("认证失败(HTTP %d):请检查 API 密钥", status)
	case status == http.StatusBadRequest || status == http.StatusNotFound:
		return fmt.Sprintf("模型或地址不可用(HTTP %d):请检查模型 ID 与接入地址前缀", status)
	default:
		return fmt.Sprintf("推理探测失败:HTTP %d", status)
	}
}

// probeRequest 按 provider 构造探测请求 URL + header(绝不日志 key)。
// 只打模型目录端点,不发任何推理请求:
//   - claude:GET {baseUrl}/models;header x-api-key + anthropic-version: 2023-06-01
//   - openai:GET {baseUrl}/models;header Authorization: Bearer <key>
//   - ollama:GET {baseUrl}/tags;无 key
func probeRequest(provider, baseURL, apiKey string) (string, map[string]string) {
	_, _, deepSeek := catalogDestination(provider, baseURL)
	if deepSeek {
		return apiEndpoint(provider, baseURL, "models"), map[string]string{"Authorization": "Bearer " + apiKey}
	}
	header := map[string]string{}
	switch provider {
	case ProviderClaude:
		header["x-api-key"] = apiKey
		header["anthropic-version"] = "2023-06-01"
		return apiEndpoint(provider, baseURL, "models"), header
	case ProviderOpenAI:
		header["Authorization"] = "Bearer " + apiKey
		return apiEndpoint(provider, baseURL, "models"), header
	default:
		return strings.TrimRight(strings.TrimSpace(baseURL), "/"), header
	}
}

// mapStatusError 把非 2xx 状态映射为人读错误(绝无密钥)。
func mapStatusError(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Sprintf("认证失败(HTTP %d):请检查 API 密钥", status)
	default:
		return fmt.Sprintf("探测失败:HTTP %d", status)
	}
}

// mapTransportError 把传输层错误映射为人读错误(超时/连接失败;绝无密钥/URL 细节泄漏 key)。
func mapTransportError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "连接超时:provider 无响应"
	}
	msg := err.Error()
	if strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "Client.Timeout") {
		return "连接超时:provider 无响应"
	}
	return "连接失败:无法连接到 provider"
}

// load 读取单例配置行 + 既有密文(密文绝不外泄,仅供保存/探测内部用)。
// 无行 → 返回惰性空默认 Config(configured/enabled false),nil 密文,无错误。
func (s *service) load(ctx context.Context) (*Config, []byte, error) {
	var (
		provider      string
		baseURL       string
		model         string
		sealed        []byte
		budgetJSON    string
		contextJSON   string
		enabledInt    int
		createdStr    string
		updatedStr    string
		webEnabledInt int
		webProvider   string
		webBaseURL    string
		webSealed     []byte
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT provider, base_url, model, api_key_ciphertext, budget_json, context_json, enabled, created_at, updated_at,
		        web_enabled, web_provider, web_base_url, web_api_key_ciphertext
		 FROM ai_config WHERE id = 1`,
	).Scan(&provider, &baseURL, &model, &sealed, &budgetJSON, &contextJSON, &enabledInt, &createdStr, &updatedStr,
		&webEnabledInt, &webProvider, &webBaseURL, &webSealed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 惰性空默认:从未配置。
			return &Config{
				Configured:   false,
				Enabled:      false,
				Provider:     "",
				BaseURL:      "",
				Model:        "",
				APIKeyMasked: "",
				Budget:       Budget{MonthlyTokenLimit: nil},
				Context:      ContextSettingsDefault(),
				UpdatedAt:    nil,
			}, nil, nil
		}
		return nil, nil, fmt.Errorf("ai: load config: %w", err)
	}

	var budget Budget
	if strings.TrimSpace(budgetJSON) != "" {
		if jerr := json.Unmarshal([]byte(budgetJSON), &budget); jerr != nil {
			return nil, nil, fmt.Errorf("ai: parse budget: %w", jerr)
		}
	}
	// 上下文声明:空/坏 JSON 一律回退默认,不让 GET 503(与 budget 不同,它只影响
	// opschat 预算策略,不涉密钥路径)。
	context := ContextSettingsDefault()
	if strings.TrimSpace(contextJSON) != "" {
		_ = json.Unmarshal([]byte(contextJSON), &context)
	}
	context = normalizeContext(context)

	// 一次性迁移存量 ollama 协议:本地模型统一走 Ollama 的 OpenAI 兼容端点(/v1);
	// 无密钥时 configured=false,由用户在设置页补任意 key(Ollama 忽略 Authorization)。
	provider, baseURL = migrateOllamaProvider(provider, baseURL)

	cfg := &Config{
		Enabled:  enabledInt != 0,
		Provider: provider,
		BaseURL:  migrateLegacyBaseURL(provider, baseURL),
		Model:    model,
		Budget:   budget,
		Context:  context,
	}

	// 掩码:有密钥时解密即时算掩码、明文用完即弃;无密钥(ollama / 未设)为空。
	// master key 缺失/轮换致密钥不可解密时**优雅降级**(NFR-10):掩码退化为占位,
	// 其余字段(provider/baseUrl/model/budget/enabled)照常返回,**不让整个 GET 503**;
	// 需要明文的 Save/Test 仍在各自解密路径报 ErrVaultUnconfigured。
	if len(sealed) > 0 {
		masked, merr := s.maskSealed(sealed)
		if merr != nil {
			masked = "••••" // 占位:有密钥但当前不可读
		}
		cfg.APIKeyMasked = masked
	}

	cfg.Configured = isConfigured(provider, len(sealed) > 0)

	// 网页工具子配置:掩码与可用性(密钥明文不解;可用性只判存在性)。
	cfg.webSealed = webSealed
	cfg.Web = WebConfig{Enabled: webEnabledInt != 0, Provider: webProvider, BaseURL: webBaseURL}
	if len(webSealed) > 0 {
		masked, merr := s.maskSealed(webSealed)
		if merr != nil {
			masked = "••••" // 占位:有密钥但当前不可读(与主密钥同降级语义)
		}
		cfg.Web.APIKeyMasked = masked
	}
	cfg.Web.Available, cfg.Web.Reason = webAvailability(cfg, sealed)

	if updated, perr := time.Parse(time.RFC3339, updatedStr); perr == nil {
		cfg.UpdatedAt = &updated
	}

	return cfg, sealed, nil
}

// openSecret 解密既有密文返回明文(供 Test 探测取用,用完即弃)。
func (s *service) openSecret(sealed []byte) (string, error) {
	if s.vault == nil {
		return "", ErrVaultUnconfigured
	}
	plain, err := s.vault.OpenSecret(sealed)
	if err != nil {
		if errors.Is(err, vault.ErrVaultUnconfigured) {
			return "", ErrVaultUnconfigured
		}
		// 解密失败(错误 master key / 篡改):按未配置态处理,不泄漏细节。
		return "", ErrVaultUnconfigured
	}
	return string(plain), nil
}

// maskSealed 解密密文并算掩码;明文用完即弃。vault 未配置 → ErrVaultUnconfigured。
func (s *service) maskSealed(sealed []byte) (string, error) {
	if s.vault == nil {
		return "", ErrVaultUnconfigured
	}
	plain, err := s.vault.OpenSecret(sealed)
	if err != nil {
		if errors.Is(err, vault.ErrVaultUnconfigured) {
			return "", ErrVaultUnconfigured
		}
		return "", ErrVaultUnconfigured
	}
	masked := maskKey(string(plain))
	for i := range plain {
		plain[i] = 0 // 明文用完清零
	}
	return masked, nil
}

// maskKey 生成 apiKey 掩码:保留末 4 位,前置点掩码。如 "••••a91f"。
// 不足以还原密钥;短 key 全掩码。
func maskKey(key string) string {
	const dots = "••••"
	r := []rune(key)
	if len(r) <= 4 {
		return dots
	}
	return dots + string(r[len(r)-4:]) // 按 rune 取末 4,避免切坏多字节 UTF-8
}

// isConfigured 判定配置是否「已配置」:provider 非空 +(ollama 无需 key / 否则有 key)。
func isConfigured(provider string, hasKey bool) bool {
	if provider == "" {
		return false
	}
	return hasKey
}

// normalizeProvider 校验并归一 provider;空串允许(表示清空配置)。
func normalizeProvider(p string) (string, error) {
	p = strings.TrimSpace(p)
	switch p {
	case "", ProviderClaude, ProviderOpenAI:
		return p, nil
	default:
		return "", ErrInvalidProvider
	}
}

// normalizeBudget 归一预算声明(负数视为 nil/不限)。
func normalizeBudget(b Budget) Budget {
	if b.MonthlyTokenLimit != nil && *b.MonthlyTokenLimit < 0 {
		return Budget{MonthlyTokenLimit: nil}
	}
	return b
}

// normalizeContext 归一上下文声明:越界值(含负数)归一回 0(=默认),避免静默放大
// 到危险值。AutoCompact 指针原样保留(nil=开)。
func normalizeContext(c ContextSettings) ContextSettings {
	if c.WindowTokens != 0 && (c.WindowTokens < 1_000 || c.WindowTokens > 2_000_000) {
		c.WindowTokens = 0
	}
	if c.MaxOutputTokens != 0 && (c.MaxOutputTokens < 512 || c.MaxOutputTokens > 65_536) {
		c.MaxOutputTokens = 0
	}
	if c.HistoryMessages != 0 && (c.HistoryMessages < 1 || c.HistoryMessages > 2_000) {
		c.HistoryMessages = 0
	}
	if c.HistoryBytes != 0 && (c.HistoryBytes < 4<<10 || c.HistoryBytes > 4<<20) {
		c.HistoryBytes = 0
	}
	halfWindow := c.WindowTokens / 2
	if halfWindow == 0 {
		halfWindow = DefaultContextWindowTokens / 2
	}
	if c.KeepRecentTokens != 0 && (c.KeepRecentTokens < 1_000 || c.KeepRecentTokens > halfWindow) {
		c.KeepRecentTokens = 0
	}
	if c.BufferTokens < 0 || (c.WindowTokens != 0 && c.BufferTokens >= c.WindowTokens) {
		c.BufferTokens = 0
	}
	return c
}

// Resolved 返回填好默认值的上下文声明(注入 opschat 策略时用;不影响落库形状)。
func (c ContextSettings) Resolved() ContextSettings {
	if c.WindowTokens <= 0 {
		c.WindowTokens = DefaultContextWindowTokens
	}
	if c.MaxOutputTokens <= 0 {
		c.MaxOutputTokens = DefaultMaxOutputTokens
	}
	if c.HistoryMessages <= 0 {
		c.HistoryMessages = DefaultHistoryMessages
	}
	if c.HistoryBytes <= 0 {
		c.HistoryBytes = DefaultHistoryBytes
	}
	if c.AutoCompact == nil {
		on := true
		c.AutoCompact = &on
	}
	if c.KeepRecentTokens <= 0 {
		c.KeepRecentTokens = DefaultContextKeepRecentTokens
	}
	return c
}

// defaultBaseURL 返回 provider 的默认 baseUrl(后端兜底)。
func defaultBaseURL(provider string) string {
	switch provider {
	case ProviderClaude:
		return defaultBaseURLClaude
	case ProviderOpenAI:
		return defaultBaseURLOpenAI
	default:
		return ""
	}
}

// migrateOllamaProvider 把存量 provider="ollama" 迁移为 openai 协议 + /v1 端点。
func migrateOllamaProvider(provider, baseURL string) (string, string) {
	if provider != "ollama" {
		return provider, baseURL
	}
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch {
	case trimmed == "":
		trimmed = "http://localhost:11434/v1"
	case trimmed == "http://localhost:11434":
		trimmed = "http://localhost:11434/v1"
	case trimmed == "http://127.0.0.1:11434":
		trimmed = "http://127.0.0.1:11434/v1"
	case strings.HasSuffix(trimmed, "/api/chat"):
		trimmed = strings.TrimSuffix(trimmed, "/api/chat") + "/v1"
	case strings.HasSuffix(trimmed, "/api"):
		trimmed = strings.TrimSuffix(trimmed, "/api") + "/v1"
	default:
		if !strings.HasSuffix(trimmed, "/v1") {
			trimmed += "/v1"
		}
	}
	return "openai", trimmed
}

// migrateLegacyBaseURL 一次性升级历史存量:原样拼接不再自动补版本段,旧默认裸根地址
// (曾依赖补 /v1、/api)与 DeepSeek 根形式升级为完整前缀。仅命中曾由本产品写入的
// 已知形态;其余自定义值原样保留,由测试连接暴露问题。
func migrateLegacyBaseURL(provider, base string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	switch {
	case provider == ProviderOpenAI && trimmed == "https://api.openai.com":
		return defaultBaseURLOpenAI
	case provider == ProviderClaude && trimmed == "https://api.anthropic.com":
		return defaultBaseURLClaude
	case provider == ProviderOpenAI && trimmed == "https://api.deepseek.com":
		return "https://api.deepseek.com/v1"
	case provider == ProviderClaude && trimmed == "https://api.deepseek.com/anthropic":
		return "https://api.deepseek.com/anthropic/v1"
	}
	return base
}
