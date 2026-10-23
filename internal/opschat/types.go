// Package opschat owns local operation sessions; it never imports AI or HTTP adapters.
package opschat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
	"time"
)

var (
	ErrNotFound    = errors.New("opschat: not found")
	ErrConflict    = errors.New("opschat: version or task conflict")
	ErrInvalid     = errors.New("opschat: invalid input")
	ErrQuota       = errors.New("opschat: resource limit reached")
	ErrUnavailable = errors.New("opschat: service not configured")
	ErrDecrypt     = errors.New("opschat: content cannot be decrypted")
	ErrApproval    = errors.New("opschat: confirmation invalid or expired")
	ErrConsent     = errors.New("opschat: analysis consent changed")
	ErrReset       = errors.New("opschat: replay requires snapshot")
	ErrStorage     = errors.New("opschat: local persistence failed")
	ErrVerifyFirst = errors.New("opschat: verify remote effect before retry")
)

const (
	// ScopeServer 是机器维度(默认;历史会话密封体零值即它)。
	ScopeServer = ""
	// ScopeProject 是项目维度:会话绑定一个项目,可选关联目标机。
	ScopeProject = "project"
)

const (
	Queued                  = "queued"
	Planning                = "planning"
	Running                 = "running"
	AwaitingConfirmation    = "awaiting_confirmation"
	AwaitingAnalysisConsent = "awaiting_analysis_consent"
	Succeeded               = "succeeded"
	PartialFailed           = "partial_failed"
	Failed                  = "failed"
	Interrupted             = "interrupted"
	Unknown                 = "unknown"
	// 轮次预算用尽且任务未完:等待用户「继续」(可附建议)或终止。
	AwaitingContinuation = "awaiting_continuation"
	// 用户在确认卡上拒绝执行(终态;拒绝原因回流模型续轮)。
	Rejected = "rejected"

	// KindCheckpoint 是压缩摘要条目(kind 值):进模型上下文(以 user 角色 +
	// conversation-checkpoint 包裹),前端渲染为「已压缩历史」分界条。
	KindCheckpoint = "summary_checkpoint"
)

type Limits struct {
	Sessions        int `json:"sessions"`
	Entries         int `json:"entries"`
	SessionBytes    int `json:"sessionBytes"`
	MessageBytes    int `json:"messageBytes"`
	Targets         int `json:"targets"`
	Calls           int `json:"calls"`
	SSHBytes        int `json:"sshBytes"`
	SSHLines        int `json:"sshLines"`
	RunBytes        int `json:"runBytes"`
	ContextBytes    int `json:"contextBytes"`
	ContextMessages int `json:"contextMessages"`
	Replay          int `json:"replay"`
}

var resourceLimits = Limits{100, 2000, 8 << 20, 8 << 10, 8, 24, 64 << 10, 1000, 512 << 10, 32 << 10, 20, 4000}

// RuntimeLimits tunes worker execution. Zero values fall back to defaults;
// it is operational configuration, unlike the user-facing resource caps above.
type RuntimeLimits struct {
	SSHSlots       int           // global bounded SSH slots (default 4)
	ApprovalTTL    time.Duration // confirmation validity window (default 15m)
	WorkBudget     time.Duration // per granted round window (default 30m)
	ModelTimeout   time.Duration // per agent round model request (default 2m)
	MaxAgentRounds int           // rounds per grant (default 20; user may continue)
}

func (l RuntimeLimits) withDefaults() RuntimeLimits {
	if l.SSHSlots <= 0 {
		l.SSHSlots = 4
	}
	if l.ApprovalTTL <= 0 {
		l.ApprovalTTL = 15 * time.Minute
	}
	if l.WorkBudget <= 0 {
		l.WorkBudget = 30 * time.Minute
	}
	if l.ModelTimeout <= 0 {
		l.ModelTimeout = 2 * time.Minute
	}
	if l.MaxAgentRounds <= 0 {
		l.MaxAgentRounds = 20
	}
	return l
}

// ContextPolicy 是注入的上下文管理策略(来自 AI 设置;opschat 不 import ai,
// 由装配层把 ai.ContextSettings.Resolved 映射进来)。零值字段经 withDefaults
// 取默认;全部语义按 tokens 计(估算器见 tokenest 包)。
type ContextPolicy struct {
	WindowTokens     int   // 模型上下文窗口(默认 200_000)
	MaxOutputTokens  int   // 单请求输出上限(默认 8_192;每请求再按剩余窗口 clamp)
	HistoryMessages  int   // 会话历史条数上限(默认 100;checkpoint 之后起算)
	HistoryBytes     int   // 会话历史字节上限(默认 256KB)
	AutoCompact      *bool // 接近上限时自动把旧历史压缩为摘要 checkpoint(nil = 开)
	KeepRecentTokens int   // 压缩时原样保留的最近消息 tokens(默认 15_000)
	BufferTokens     int   // 触发预留;0 = 自动 max(窗口10%, 窗口≥32K 时 16K)
}

func (p ContextPolicy) withDefaults() ContextPolicy {
	if p.WindowTokens <= 0 {
		p.WindowTokens = 200_000
	}
	if p.MaxOutputTokens <= 0 {
		p.MaxOutputTokens = 8_192
	}
	if p.HistoryMessages <= 0 {
		p.HistoryMessages = 100
	}
	if p.HistoryBytes <= 0 {
		p.HistoryBytes = 256 << 10
	}
	if p.AutoCompact == nil {
		on := true
		p.AutoCompact = &on
	}
	if p.KeepRecentTokens <= 0 {
		p.KeepRecentTokens = 15_000
	}
	if p.KeepRecentTokens > p.WindowTokens/2 {
		p.KeepRecentTokens = p.WindowTokens / 2
	}
	return p
}

// ReserveTokens 是压缩触发前在窗口内预留的空间(输出 + buffer)。
func (p ContextPolicy) ReserveTokens() int {
	if p.BufferTokens > 0 {
		return p.BufferTokens
	}
	reserve := p.WindowTokens / 10
	if p.WindowTokens >= 32_000 && reserve < 16_000 {
		reserve = 16_000
	}
	return reserve
}

// Ceiling 是自动压缩触发阈值:估算 tokens ≥ Ceiling 时压缩。
func (p ContextPolicy) Ceiling() int {
	c := p.WindowTokens - p.ReserveTokens()
	if c < p.WindowTokens/2 {
		c = p.WindowTokens / 2
	}
	return c
}

// MaxRequestBytes 是单次模型请求的绝对字节上限(窗口按 CJK 最坏 6 字节/token
// 换算,下限 2MB 安全兜底)。
func (p ContextPolicy) MaxRequestBytes() int {
	cap := p.WindowTokens * 6
	if cap < 2<<20 {
		cap = 2 << 20
	}
	return cap
}

type ProviderInfo struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// ConfigHash changes whenever destination or configuration changes. No key or URL is sent to the model.
	ConfigHash string `json:"configHash"`
}
type Capabilities struct {
	Available      bool         `json:"available"`
	ModelAvailable bool         `json:"modelAvailable"`
	Provider       ProviderInfo `json:"provider"`
	Limits         Limits       `json:"limits"`
}
type Options struct {
	DB       *sql.DB
	Vault    vault.Vault
	Executor target.LimitedExecutor
	Model    Model
	// Projects 是项目维度工具的后端;nil 表示项目维度未配置(创建项目会话报不可用)。
	Projects ProjectBackend
	// Web 是网页搜索/抓取工具的后端;nil 表示网页工具未装配(目录与执行均不可用)。
	Web WebBackend
	// LocalRecorder must be audit.New(db, masker, nil), never the shared remote-sink recorder.
	LocalRecorder audit.Recorder
	Masker        *mask.Masker
	Runtime       RuntimeLimits
	// Context supplies the live context-management policy (mapped from AI
	// settings by the assembly layer; nil = defaults). Called per round so
	// settings changes apply without restart.
	Context func() ContextPolicy
}
type Session struct {
	ID        string   `json:"id"`
	Revision  int64    `json:"revision"`
	Title     string   `json:"title"`
	Draft     string   `json:"draft"`
	ServerIDs []string `json:"serverIds"`
	// Scope 是会话维度:""=机器(默认)/"project"=项目;项目会话必选关联机器可用机器工具。
	Scope string `json:"scope,omitempty"`
	// ProjectID 是项目维度会话绑定的项目;机器维度为空。
	ProjectID   string `json:"projectId,omitempty"`
	ActiveRunID string `json:"activeRunId"`
	Watermark   int64  `json:"watermark"`
	// StorageBytes is the tracked visible-entry byte counter, an approximation
	// of the session storage quota basis; use it only for near-quota hints.
	StorageBytes int64 `json:"storageBytes"`
	// ContextUsage 是最近一次模型请求的实测上下文占用(usage 缺失时为 nil;
	// 仅展示用,预算判定在发送前全量估算)。
	ContextUsage *ContextUsage `json:"contextUsage,omitempty"`
	CreatedAt    time.Time     `json:"createdAt"`
	UpdatedAt    time.Time     `json:"updatedAt"`
}
type CreateInput struct {
	Context   string   `json:"context,omitempty"`
	Title     string   `json:"title"`
	ServerIDs []string `json:"serverIds"`
	Scope     string   `json:"scope,omitempty"`
	ProjectID string   `json:"projectId,omitempty"`
}
type ActivateInput struct {
	Context string `json:"context,omitempty"`
}
type PatchInput struct {
	Revision  int64     `json:"revision"`
	Title     *string   `json:"title,omitempty"`
	Draft     *string   `json:"draft,omitempty"`
	ServerIDs *[]string `json:"serverIds,omitempty"`
	// ProjectID 仅项目维度会话有效:切换绑定的项目。
	ProjectID *string `json:"projectId,omitempty"`
}
type TurnInput struct {
	AnalyzeResults  bool         `json:"analyzeResults,omitempty"`
	Provider        ProviderInfo `json:"provider,omitempty"`
	ClientRequestID string       `json:"clientRequestId"`
	Revision        int64        `json:"revision"`
	Text            string       `json:"text"`
}
type RetryInput struct {
	ClientRequestID string   `json:"clientRequestId"`
	Revision        int64    `json:"revision"`
	CallIDs         []string `json:"callIds"`
}
type Run struct {
	Phase           string    `json:"phase,omitempty"`
	ID              string    `json:"id"`
	SessionID       string    `json:"sessionId"`
	ClientRequestID string    `json:"clientRequestId"`
	Status          string    `json:"status"`
	CancelRequested bool      `json:"cancelRequested"`
	CreatedAt       time.Time `json:"createdAt"`
}
type ToolArgs struct {
	Argv      []string `json:"argv,omitempty"`
	Container string   `json:"container,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Lines     int      `json:"lines,omitempty"`
	Action    string   `json:"action,omitempty"`
	// 项目维度工具参数。
	Path        string            `json:"path,omitempty"`
	Ref         string            `json:"ref,omitempty"`
	Branch      string            `json:"branch,omitempty"`
	RunID       string            `json:"runId,omitempty"`
	StepOrdinal *int              `json:"stepOrdinal,omitempty"`
	Status      string            `json:"status,omitempty"`
	Limit       int               `json:"limit,omitempty"`
	Params      map[string]string `json:"params,omitempty"`
	// 网页搜索/抓取工具参数。
	Query string `json:"query,omitempty"`
	URL   string `json:"url,omitempty"`
	Count int    `json:"count,omitempty"`
}
type Action struct {
	ToolID        string          `json:"toolId"`
	Args          json.RawMessage `json:"args"`
	TargetIndexes []int           `json:"targetIndexes"`
}
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}
type AgentRequest struct {
	Question string       `json:"question"`
	Provider ProviderInfo `json:"provider"`
	System   string       `json:"system"`
	History  []Message    `json:"messages"`
	Targets  []string     `json:"targets"`
	// TargetNote 是 target 枚举属性的自然语言说明(按维度生成:机器=服务器,
	// 项目=关联服务器且 target_0 是项目本身、对机器工具不可用)。
	TargetNote       string          `json:"targetNote,omitempty"`
	Tools            []Tool          `json:"tools"` // 收尾轮为空
	Rounds           []AgentExchange `json:"rounds,omitempty"`
	MaxResponseBytes int             `json:"maxResponseBytes"`
	WrapUp           bool            `json:"wrapUp,omitempty"` // 收尾轮:不带 tools,要求进展+建议
	// MaxOutputTokens 是单请求输出上限(0 = 模型层默认);发送前按剩余窗口 clamp。
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
	// MaxRequestBytes 是单请求编码后的字节上限(0 = 模型层默认兜底)。
	MaxRequestBytes int `json:"maxRequestBytes,omitempty"`
}
type AgentToolCall struct {
	ID      string          `json:"id"` // provider 侧调用 id(结果关联)
	ToolID  string          `json:"toolId"`
	Target  string          `json:"target"` // target_N 别名
	Purpose string          `json:"purpose,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	Args    json.RawMessage `json:"args"`
}
type AgentToolResult struct {
	CallID       string `json:"callId"`
	Output       string `json:"output"`
	Failed       bool   `json:"failed,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
	Denied       bool   `json:"denied,omitempty"`
	DenialReason string `json:"denialReason,omitempty"`
}
type AgentExchange struct {
	Calls   []AgentToolCall   `json:"calls"`
	Results []AgentToolResult `json:"results"`
}
type AgentReply struct {
	Text      string          `json:"text"` // 终答/收尾文本(与 ToolCalls 互斥)
	ToolCalls []AgentToolCall `json:"toolCalls,omitempty"`
	// Usage 是该请求的 provider 实测 token 用量(解析不到时为 nil;仅用于
	// 上下文占用展示与观测,不参与预算判定——判定走发送前全量估算)。
	Usage *AgentUsage `json:"usage,omitempty"`
}

// AgentUsage 是 provider 返回的请求用量(input 含 cache 读写在内的完整 prompt)。
type AgentUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	// ConsumedAmount 是自定网关(new-api 系)自报的费用金额:流式末 chunk 的
	// x_new_api_consumed_amount 字段或非流式 X-New-Api-Consumed-Amount 响应头。
	// 0 = 网关未回报(直连 OpenAI/Claude/Ollama);原样记录,平台不做单位换算。
	ConsumedAmount float64 `json:"consumedAmount,omitempty"`
}
type ContainerFact struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
	State string `json:"state"`
}
type ContainerObservation struct {
	TargetAlias string          `json:"targetAlias"`
	CollectedAt *time.Time      `json:"collectedAt,omitempty"`
	Containers  []ContainerFact `json:"containers"`
	Truncated   bool            `json:"truncated"`
	Unavailable string          `json:"unavailable,omitempty"`
}

type AnalysisItem struct {
	Truncated   bool       `json:"truncated"`
	CollectedAt *time.Time `json:"collectedAt,omitempty"`
	ExitCode    *int       `json:"exitCode,omitempty"`
	CallID      string     `json:"callId"`
	TargetAlias string     `json:"targetAlias"`
	ToolID      string     `json:"toolId"`
	Output      string     `json:"output"`
	Error       string     `json:"error"`
	Status      string     `json:"status"`
}
type AnalysisRequest struct {
	Question         string         `json:"question"`
	Provider         ProviderInfo   `json:"provider"`
	Items            []AnalysisItem `json:"items"`
	MaxResponseBytes int            `json:"maxResponseBytes"`
}

// Info is strictly local. Adapters MUST bound provider responses while reading, not after buffering.
type Model interface {
	Info() ProviderInfo
	AgentRound(context.Context, AgentRequest) (AgentReply, error)
	Analyze(context.Context, AnalysisRequest) (string, error)
}

// ProjectSnapshot 是项目维度会话提交轮次时冻结的项目目标快照:存在性校验 +
// 指纹闸门(运行中项目被改配置时拒绝再派发,与机器指纹同语义)。
type ProjectSnapshot struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	RepoURL       string    `json:"repoUrl"`
	DefaultBranch string    `json:"defaultBranch"`
	PacEnabled    bool      `json:"pacEnabled"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Fingerprint 摘要项目关键字段(摘要字段即派发闸门判定依据,不含时间戳)。
func (p ProjectSnapshot) Fingerprint() string {
	return digest(struct {
		ID, Name, RepoURL, DefaultBranch string
		PacEnabled                       bool
	}{p.ID, p.Name, p.RepoURL, p.DefaultBranch, p.PacEnabled})
}

// ProjectBackend 是项目维度工具的端口:装配层注入实现(opschat 不 import
// project/pipeline/run 适配包)。除 Snapshot 外各方法返回有界 JSON 文本,
// worker 仍会按会话读预算二次裁剪并掩码。
type ProjectBackend interface {
	// Snapshot 校验项目存在并返回当前快照(创建会话/提交轮次/再派发闸门用)。
	Snapshot(ctx context.Context, projectID string) (ProjectSnapshot, error)
	// Info 返回项目基本信息(名称/仓库/默认分支/pac 等,有界 JSON)。
	Info(ctx context.Context, projectID string) (string, error)
	// CodeTree 列仓库目录直接子项;ref 空取默认分支,path 空 取仓库根。
	CodeTree(ctx context.Context, projectID, ref, path string) (string, error)
	// CodeRead 读仓库文本文件(自文件头起,行数钳制;二进制检测)。
	CodeRead(ctx context.Context, projectID, ref, path string, lines int) (string, error)
	// PipelineConfig 返回平台库存的流水线配置(YAML+阶段摘要+状态)。
	PipelineConfig(ctx context.Context, projectID string) (string, error)
	// RepoConfig 返回仓库分支上的 .pipewright.yml(pac 语义,含回退原因)。
	RepoConfig(ctx context.Context, projectID, branch string) (string, error)
	// Runs 列项目近期运行(status 空为全部;limit 已由工具参数钳制)。
	Runs(ctx context.Context, projectID, status string, limit int) (string, error)
	// RunDetail 返回单个运行详情(节点状态/spec 来源/失败日志)。JSON 必须含
	// status 字段(变更效果校验依赖此契约)。
	RunDetail(ctx context.Context, runID string) (string, error)
	// RunLogs 返回运行(或 stepOrdinal 指定节点,-1 为运行级)日志尾部。
	RunLogs(ctx context.Context, runID string, stepOrdinal, lines int) (string, error)
	// TriggerRun 手动触发一次运行,返回新 run 概要(变更;经确认卡放行后调用)。
	// JSON 必须含 runId 字段(变更效果校验依赖此契约)。
	TriggerRun(ctx context.Context, projectID, branch string, params map[string]string) (string, error)
	// CancelRun 取消一次运行,返回取消后的 run 概要(变更;经确认卡放行后调用)。
	CancelRun(ctx context.Context, runID string) (string, error)
}

// WebBackend 是网页搜索/抓取工具的端口:装配层注入实现(opschat 不 import ai)。
// Search 返回有界 JSON 文本、Fetch 返回有界 Markdown 文本,worker 仍按会话读
// 预算二次裁剪并掩码。网页内容一律按 untrusted data 处理。
type WebBackend interface {
	// Available 报告网页工具当前是否可用(轻量本地读:开关+有效配置解析,
	// 不发网络请求)。单轮目录按它过滤,设置变更下轮生效。
	Available(ctx context.Context) bool
	// Search 按关键词搜索公网,返回有界结果 JSON(query 已由 parseWebArgs 校验)。
	Search(ctx context.Context, query string, count int) (string, error)
	// Fetch 抓取一个公网页面,返回有界 Markdown(rawURL 已由 parseWebArgs 校验)。
	Fetch(ctx context.Context, rawURL string) (string, error)
}

// Compacter is an optional model capability: generate a compaction summary of
// older conversation via a tool-less single call. When the model does not
// implement it, auto-compact degrades to deterministic trimming only.
type Compacter interface {
	Summarize(ctx context.Context, in SummarizeRequest) (string, error)
}

// SummarizeRequest is the compaction-summary call: flattened older transcript
// in Prompt, no tools, bounded output.
type SummarizeRequest struct {
	Provider         ProviderInfo
	System           string
	Prompt           string
	MaxOutputTokens  int
	MaxResponseBytes int
}

// OverflowClassifier is an optional model capability: classify whether an
// AgentRound error is a provider context-window overflow (request too long),
// enabling overflow compaction + one retry.
type OverflowClassifier interface {
	IsContextOverflow(err error) bool
}

type StreamChunk struct {
	Text     string
	Fallback bool
}

// StreamModel publishes only natural text for the analysis flow.
type StreamModel interface {
	AnalyzeStream(context.Context, AnalysisRequest, func(StreamChunk) error) (string, error)
}

// SecretRegistrar registers current (including disabled) provider keys with the
// shared Masker. It must perform only bounded local configuration reads.
type SecretRegistrar interface {
	RegisterSecrets(context.Context) error
}
type Call struct {
	Resources   *HostResources `json:"resources,omitempty"`
	ID          string         `json:"callId"`
	RunID       string         `json:"runId"`
	SessionID   string         `json:"sessionId"`
	ServerID    string         `json:"serverId"`
	ServerName  string         `json:"serverName"`
	ToolID      string         `json:"toolId"`
	Args        ToolArgs       `json:"args"`
	Object      string         `json:"object"`
	Purpose     string         `json:"purpose,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	Status      string         `json:"status"`
	ArgsHash    string         `json:"argsHash"`
	TargetHash  string         `json:"targetHash"`
	CollectedAt *time.Time     `json:"collectedAt,omitempty"`
	ExitCode    *int           `json:"exitCode,omitempty"`
	Output      string         `json:"output"`
	Error       string         `json:"error"`
	Truncated   bool           `json:"truncated"`
}
type BoundCall struct {
	CallID     string `json:"callId"`
	ServerID   string `json:"serverId"`
	ToolID     string `json:"toolId"`
	Object     string `json:"object"`
	Purpose    string `json:"purpose,omitempty"`
	Reason     string `json:"reason,omitempty"`
	ArgsHash   string `json:"argsHash"`
	TargetHash string `json:"targetHash"`
}
type Confirmation struct {
	RunID     string      `json:"runId"`
	Nonce     string      `json:"nonce"`
	ExpiresAt time.Time   `json:"expiresAt"`
	Calls     []BoundCall `json:"calls"`
	Valid     bool        `json:"valid"`
}
type ConfirmInput struct {
	RunID string      `json:"runId"`
	Nonce string      `json:"nonce"`
	Calls []BoundCall `json:"calls"`
}
type AnalysisPreview struct {
	CallIDs  []string       `json:"callIds"`
	Items    []AnalysisItem `json:"items"`
	Hash     string         `json:"hash"`
	Provider ProviderInfo   `json:"provider"`
}
type AnalysisInput struct {
	RunID           string       `json:"runId,omitempty"`
	AnalyzeResults  bool         `json:"analyzeResults,omitempty"`
	ClientRequestID string       `json:"clientRequestId"`
	Revision        int64        `json:"revision"`
	CallIDs         []string     `json:"callIds"`
	PreviewHash     string       `json:"previewHash"`
	Provider        ProviderInfo `json:"provider"`
	Consent         bool         `json:"consent"`
}

// ContextUsage 是会话上下文占用展示模型:最近一次模型请求实测 usage / 窗口。
type ContextUsage struct {
	UsedTokens   int `json:"usedTokens"`
	WindowTokens int `json:"windowTokens"`
	Percent      int `json:"percent"` // 0-100(向上取整)
}

type Entry struct {
	// TargetNames is reconstructed for local display, never persisted or sent to models.
	TargetNames  map[string]string `json:"targetNames,omitempty"`
	ReplaceSeq   int64             `json:"replaceSeq,omitempty"`
	RevisionSeq  int64             `json:"revisionSeq,omitempty"`
	RequestID    string            `json:"requestId,omitempty"`
	Call         *Call             `json:"call,omitempty"`
	Run          *Run              `json:"run,omitempty"`
	Session      *Session          `json:"session,omitempty"`
	Confirmation *Confirmation     `json:"confirmation,omitempty"`
	Summary      *RunSummary       `json:"summary,omitempty"`
	SessionID    string            `json:"sessionId"`
	Seq          int64             `json:"seq"`
	Kind         string            `json:"kind"`
	RunID        string            `json:"runId,omitempty"`
	CallID       string            `json:"callId,omitempty"`
	Text         string            `json:"text,omitempty"`
	Status       string            `json:"status,omitempty"`
	Usage        *AgentUsage       `json:"usage,omitempty"`
	CreatedAt    time.Time         `json:"createdAt"`
}
type EntryPage struct {
	Entries   []Entry `json:"entries"`
	Cursor    string  `json:"cursor"`
	Watermark int64   `json:"watermark"`
	Reset     bool    `json:"reset"`
}
type SessionPage struct {
	Sessions        []Session `json:"sessions"`
	Cursor          string    `json:"cursor"`
	ActiveSessionID string    `json:"activeSessionId"`
}
type Snapshot struct {
	Session       Session        `json:"session"`
	Runs          []Run          `json:"runs"`
	Calls         []Call         `json:"calls"`
	Confirmations []Confirmation `json:"confirmations"`
	Entries       EntryPage      `json:"entries"`
	Watermark     int64          `json:"watermark"`
}
type sessionBody struct {
	Title     string
	Draft     string
	ServerIDs []string
	Named     bool
	Scope     string
	ProjectID string
}
type runBody struct {
	Provider           ProviderInfo
	AnalysisAuthorized bool
	AnalysisStarted    bool
	Question           string
	SummarySaved       bool
	ReadBytes          int
	RetryActions       []Action
	RetryCallIDs       []string
	Turn               TurnInput
	Targets            []target.ConnectionSnapshot
	// Project 是项目维度会话提交时冻结的项目快照(机器维度为 nil)。
	Project      *ProjectSnapshot
	Analysis     *AnalysisPreview
	ActiveMillis int64
	Rounds       int // 已用轮次(persist-before-model 递增)
	RoundBudget  int // 已授予轮次(0 → 启动时取 runtime.MaxAgentRounds;继续 +20)
	OutputBytes  int
}
type callBody struct {
	ReadBytes     int
	ReadLines     int
	ReadTruncated bool
	Dispatched    bool
	View          Call
	Snapshot      target.ConnectionSnapshot
	Round         int
	DenialReason  string
}
type approvalBody struct {
	Nonce string
	Calls []BoundCall
}
