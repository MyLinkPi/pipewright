// Package secscan 是「安全中心」的领域层(Trivy 主机/镜像 CVE 扫描 + Nuclei 暴露面检测)。
//
// 三块职责:
//   - 扫描器登记:中心 trivy-server 接入点(host:port + 可选 token,vault 密封;
//     应用市场部署 trivy-server 模板后自动登记,也可在安全中心手动登记外部实例)。
//   - 扫描执行:经 SSH 在目标机上 docker run --rm 跑 trivy(rootfs + 运行中镜像)或
//     nuclei(按需,特征模板落具名卷复用);run 行先落 running,goroutine 异步执行,
//     完整报告 JSON 存 artifact store(内容寻址),run 行只存 summary 计数。目标机无
//     docker 时的代跑:Nuclei 走登记的常驻 runner 容器,trivy 主机包扫描由已登记
//     Trivy Server 的宿主机代跑(包管理元数据经面板中继,见 trivy_remote.go)。
//   - 聚合与调度:每机每类型最新结果聚合(Overview)、前端可配的定时规则
//     (5 段 cron,复用 internal/cron 校验)、HTML 审计报告数据组装。
//
// 纪律:token 绝不明文入库/出响应/进审计(仅 vault 密文 BLOB);扫描命令 argv 数组化
// 不拼 shell(AC-SEC-02);扫描不设主动超时(时长取决于镜像/模板下载,中断由服务
// 关停上下文兜底);目标机无 docker 且无可用代跑路径 → run 置 failed 并给人话原因。
package secscan

import (
	"context"
	"database/sql"
	"time"
)

// 扫描器类型枚举。
const (
	// KindTrivyServer 是中心 Trivy 服务(漏洞库缓存,目标机扫描时免各自下库)。
	KindTrivyServer = "trivy-server"
	// KindNucleiRunner 是常驻 Nuclei 执行容器(部署在任一有 docker 的机器;面板经
	// docker exec 在容器内执行扫描,目标机无需 docker,只要网络可达)。
	KindNucleiRunner = "nuclei-runner"
	// KindSafeline 是雷池 SafeLine WAF 管理面(host:port + API Token;防护态势与
	// 证书自动同步的接入点,见 safeline.go)。
	KindSafeline = "safeline"
)

// 扫描类型枚举。
const (
	ScanTypeTrivy  = "trivy"  // 主机包 + 运行中容器镜像的 CVE 扫描
	ScanTypeNuclei = "nuclei" // 暴露面/已知漏洞主动探测(按需运行,无常驻)
)

// Run 状态枚举。
const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Run 触发来源枚举。
const (
	TriggerManual    = "manual"    // 安全中心单机扫描按钮
	TriggerFleet     = "fleet"     // 全量扫描按钮
	TriggerScheduled = "scheduled" // 定时规则触发
)

// 来源枚举:manual = 安全中心手工登记;appstore = 应用市场部署 trivy-server 自动登记。
const (
	SourceManual   = "manual"
	SourceAppStore = "appstore"
)

// 严重度枚举(小写;trivy/nuclei 输出归一化到此集合;unknown 兜底无法识别的值)。
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
	SeverityInfo     = "info" // 仅 nuclei 产出
	SeverityUnknown  = "unknown"
)

// scheduleRowID 是定时规则单行的固定主键(全局唯一一份)。
const scheduleRowID = "default"

// 领域错误(人话与状态码由 httpapi 层映射;错误体绝不含 token)。
var (
	// ErrNotFound 表示扫描器实例/扫描记录不存在。
	ErrNotFound = &domainError{"secscan: not found"}
	// ErrInvalidKind 表示扫描器类型枚举非法。
	ErrInvalidKind = &domainError{"secscan: invalid kind"}
	// ErrEmptyName 表示名称为空。
	ErrEmptyName = &domainError{"secscan: name must not be empty"}
	// ErrEmptyHost 表示主机为空。
	ErrEmptyHost = &domainError{"secscan: host must not be empty"}
	// ErrInvalidPort 表示端口越界(1-65535)。
	ErrInvalidPort = &domainError{"secscan: port out of range"}
	// ErrEndpointTaken 表示同类型同地址扫描器已存在(唯一索引 kind+host+port)。
	ErrEndpointTaken = &domainError{"secscan: endpoint already registered"}
	// ErrVaultUnconfigured 表示保险库未配置 master key,token 无法密封/解封。
	ErrVaultUnconfigured = &domainError{"secscan: vault unconfigured"}
	// ErrInvalidInput 表示入参非法(扫描类型/cron 表达式/目标 URL 等)。
	ErrInvalidInput = &domainError{"secscan: invalid input"}
	// ErrRunnerUnavailable 表示扫描执行器未装配(裁剪部署),无法发起扫描。
	ErrRunnerUnavailable = &domainError{"secscan: scan runner unavailable"}
	// ErrServerNotFound 表示目标服务器不存在。
	ErrServerNotFound = &domainError{"secscan: server not found"}
	// ErrServerNotTarget 表示目标服务器不允许扫描(PVE 节点)。
	ErrServerNotTarget = &domainError{"secscan: server is not a scan target"}
)

// domainError 是本包 sentinel 错误的载体(errors.Is 按指针比较)。
type domainError struct{ msg string }

func (e *domainError) Error() string { return e.msg }

// Instance 是一个中心扫描服务的接入点(v1 仅 trivy-server)。token 只持密文 BLOB,
// 永不出包边界;对外仅暴露 TokenConfigured(扫描行时由密文存在性计算)。
type Instance struct {
	ID              string
	Kind            string // trivy-server
	Name            string
	Host            string
	Port            int
	TokenConfigured bool
	Source          string // manual | appstore
	ServerID        string // 应用市场部署来源的目标服务器(可空)
	StackName       string // 应用市场部署来源的栈名(可空)
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CreateInput 是安全中心手工登记扫描器的入参(Token 明文直达本层,由本层密封落库)。
type CreateInput struct {
	Name  string
	Host  string
	Port  int
	Token string // 可选
}

// UpdateInput 是编辑扫描器的入参;指针字段 nil 表示不修改(Token 非 nil 即轮换,空串 = 清除)。
type UpdateInput struct {
	Name  *string
	Host  *string
	Port  *int
	Token *string
}

// ConnectInput 是未保存接入点的直连测试入参(token 用完即弃,绝不持久化)。
type ConnectInput struct {
	Host  string
	Port  int
	Token string
}

// RegisterInput 是应用市场部署联动登记的入参(镜像 dbadmin.RegisterInput)。
// Kind 决定形态:trivy-server 需 host:port 服务;nuclei-runner 无服务端口(Port=0,
// 由 ServerID 定位执行容器)。
type RegisterInput struct {
	Kind      string
	Name      string
	Host      string
	Port      int
	Token     string // 可选;重新部署未填 = 清除既有令牌
	ServerID  string
	StackName string
}

// RegisterResult 是联动登记结果:实例视图 + 是否命中既有条目原地更新。
type RegisterResult struct {
	Instance *Instance
	Updated  bool // true = 命中同 kind+host+port 既有条目,原地轮换令牌
}

// TestResult 是扫描器连通性测试结果。
type TestResult struct {
	OK        bool
	LatencyMs int64
	Detail    string // 人话描述(如 "Trivy 服务健康检查通过")或错误摘要
	Err       error
}

// Summary 是一次扫描的计数聚合(存 run 行的 JSON)。
type Summary struct {
	Critical    int `json:"critical"`
	High        int `json:"high"`
	Medium      int `json:"medium"`
	Low         int `json:"low"`
	Info        int `json:"info"`
	Unknown     int `json:"unknown"`
	Total       int `json:"total"`
	ImageCount  int `json:"imageCount,omitempty"`  // trivy:扫描的容器镜像数
	TargetCount int `json:"targetCount,omitempty"` // nuclei:探测的目标数
}

// Add 累加另一个 Summary(严重度与总数;不含 image/target 计数)。
func (s *Summary) Add(o Summary) {
	s.Critical += o.Critical
	s.High += o.High
	s.Medium += o.Medium
	s.Low += o.Low
	s.Info += o.Info
	s.Unknown += o.Unknown
	s.Total += o.Total
}

// Count 返回指定严重度的计数。
func (s Summary) Count(severity string) int {
	switch severity {
	case SeverityCritical:
		return s.Critical
	case SeverityHigh:
		return s.High
	case SeverityMedium:
		return s.Medium
	case SeverityLow:
		return s.Low
	case SeverityInfo:
		return s.Info
	default:
		return s.Unknown
	}
}

// Run 是一次扫描(单机单类型)的运行记录。
type Run struct {
	ID          string
	ServerID    string
	ServerName  string // 快照(服务器删除后历史仍可读)
	ScanType    string // trivy | nuclei
	Status      string // running | done | failed
	Trigger     string // manual | fleet | scheduled
	Summary     Summary
	ArtifactKey string
	Error       string
	ScannerID   string // trivy 使用的中心服务实例(可空 = 本地缓存卷模式)
	StartedAt   time.Time
	FinishedAt  time.Time
}

// Finding 是统一漏洞/暴露面条目(trivy 与 nuclei 解析归一的产物,报告 JSON 的明细单位)。
type Finding struct {
	Severity         string `json:"severity"`
	Source           string `json:"source"` // host | image:<ref> | nuclei
	Target           string `json:"target"` // 包名 / 镜像探测点(matched-at)
	ID               string `json:"id"`     // CVE 编号 / 模板 ID
	Title            string `json:"title,omitempty"`
	InstalledVersion string `json:"installedVersion,omitempty"` // trivy 包当前版本
	FixedVersion     string `json:"fixedVersion,omitempty"`     // trivy 修复版本;nuclei 空
	URL              string `json:"url,omitempty"`              // CVE/详情外链
}

// StepLog 是一次扫描中单个执行步骤的原始输出(stdout/stderr 截断保留),供「查看日志」
// 诊断「扫不出结果」类问题。绝不记录命令行本身(本地模式 argv 带明文 token)。
type StepLog struct {
	Step     string `json:"step"`             // 步骤名(如「主机包扫描(trivy rootfs)」)
	ExitCode int    `json:"exitCode"`         // 远端命令退出码;SSH 失败为 -1
	Stdout   string `json:"stdout,omitempty"` // 原始标准输出(超长截头)
	Stderr   string `json:"stderr,omitempty"` // 原始标准错误(超长截头)
}

// RunLogView 是某次扫描的原始输出日志视图(HTTP 层 DTO 的数据源)。
type RunLogView struct {
	RunID    string    `json:"runId"`
	ScanType string    `json:"scanType"`
	Status   string    `json:"status"`
	Logs     []StepLog `json:"logs"`
}

// ReportDoc 是一次扫描的完整报告(artifact store 里的 JSON 结构;明细按需读取)。
type ReportDoc struct {
	ScanType    string    `json:"scanType"`
	GeneratedAt string    `json:"generatedAt"` // RFC3339
	Host        string    `json:"host"`
	ServerName  string    `json:"serverName"`
	Findings    []Finding `json:"findings"`
	Notes       []string  `json:"notes,omitempty"` // 部分失败/降级说明(如个别镜像扫描失败)
	Logs        []StepLog `json:"logs,omitempty"`  // 各执行步骤的原始输出(诊断用)

	// imageCount 是 trivy 成功扫描的容器镜像数(不序列化为独立字段;由 executor
	// 汇入 Summary.ImageCount)。
	imageCount int `json:"-"`
	// failed 标记整体失败(区别于带 notes 的部分成功);不序列化,executeAsync 据此
	// 置 failed 终态,同时把携带执行日志的 doc 存进 artifact 供「查看日志」。
	failed bool `json:"-"`
}

// Schedule 是定时扫描规则(全局单行)。
type Schedule struct {
	Enabled    bool
	Expression string // 5 段 cron(internal/cron Parse 校验)
	ScanTypes  []string
	LastRunAt  time.Time // 零值 = 从未触发
	UpdatedAt  time.Time
}

// ScheduleInput 是保存定时规则的入参。
type ScheduleInput struct {
	Enabled    bool
	Expression string
	ScanTypes  []string
}

// ScanInput 是发起一次扫描的入参。
type ScanInput struct {
	ServerID string
	ScanType string   // trivy | nuclei
	Targets  []string // 仅 nuclei:探测目标 URL;空 = 默认 http(s)://<宿主地址>
	Trigger  string   // manual | fleet | scheduled(留空 = manual)
}

// RunFilter 是扫描历史的查询条件。
type RunFilter struct {
	ServerID string
	ScanType string
	Limit    int // 上限 200,默认 50
}

// FindingFilter 是明细抽屉的过滤条件(内存过滤,artifact JSON 全量解析后应用)。
type FindingFilter struct {
	Severity string // 精确匹配某一严重度;空 = 全部
	Source   string // 前缀匹配(host / image / nuclei);空 = 全部
	Query    string // 对 ID/Title/Target 的不区分大小写包含
	Offset   int
	Limit    int // 上限 500,默认 100
}

// FindingsPage 是过滤后的明细页。
type FindingsPage struct {
	Total   int       `json:"total"`
	Items   []Finding `json:"items"`
	Summary Summary   `json:"summary"` // 过滤前的全量计数(供徽标)
}

// OverviewServer 是概览里一台服务器的聚合视图。
type OverviewServer struct {
	ServerID   string
	ServerName string
	Host       string
	Trivy      *RunLite // 最近一次 trivy 扫描(任意状态;nil = 从未扫描)
	Nuclei     *RunLite
}

// RunLite 是概览/历史行的运行摘要。
type RunLite struct {
	RunID      string    `json:"runId"`
	ScanType   string    `json:"scanType"`
	Status     string    `json:"status"`
	Trigger    string    `json:"trigger,omitempty"`
	Summary    Summary   `json:"summary"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"-"`
	FinishedAt time.Time `json:"-"`
}

// Overview 是安全中心概览(全员汇总 + 每机最新)。
type Overview struct {
	Servers    []OverviewServer
	Totals     Summary // 各机最新 done 结果的合计
	Scanned    int     // 至少有一台类型 done 结果的服务器数
	LastScanAt time.Time
}

// ServerReport 是 HTML 审计报告的单台服务器章节(该机最新 done 结果)。
type ServerReport struct {
	ServerName string
	Host       string
	Trivy      *RunReport
	Nuclei     *RunReport
}

// RunReport 是报告章节里一次扫描的完整数据。
type RunReport struct {
	FinishedAt time.Time
	Summary    Summary
	Doc        *ReportDoc
}

// Service 定义安全中心对外接口(httpapi 消费)。
type Service interface {
	// ---- 扫描器登记 ----
	ListScanners(ctx context.Context) ([]Instance, error)
	CreateScanner(ctx context.Context, in CreateInput) (*Instance, error)
	UpdateScanner(ctx context.Context, id string, in UpdateInput) (*Instance, error)
	DeleteScanner(ctx context.Context, id string) error
	// TestScanner 测试已存扫描器连通性(trivy-server: /healthz 带 token,回落 TCP 探活)。
	TestScanner(ctx context.Context, id string) (*TestResult, error)
	// TestScannerDirect 用未保存的接入点直连测试(token 用完即弃)。
	TestScannerDirect(ctx context.Context, in ConnectInput) (*TestResult, error)
	// Register 应用市场部署联动登记:同 kind+host+port 已有条目 → 原地轮换令牌,
	// 否则新建(source=appstore)。尽力而为语义由调用方负责。
	Register(ctx context.Context, in RegisterInput) (*RegisterResult, error)
	// ScannerLibInfo 查询实例规则库状态(漏洞库/特征模板;见 lib.go)。
	ScannerLibInfo(ctx context.Context, instanceID string) (*ScannerLibInfo, error)
	// ScannerLibUpdate 手动触发规则库更新(trivy 重启容器 / nuclei 更新模板;safeline 拒绝)。
	ScannerLibUpdate(ctx context.Context, instanceID string) (*ScannerLibInfo, error)

	// ---- 定时规则 ----
	GetSchedule(ctx context.Context) (*Schedule, error)
	SaveSchedule(ctx context.Context, in ScheduleInput) (*Schedule, error)
	// TouchScheduleLastRun 记录定时规则最近一次实际触发(调度器调用)。
	TouchScheduleLastRun(ctx context.Context, at time.Time) error

	// ---- 扫描执行(异步:run 行先落 running,结果稍后可见) ----
	// StartScan 对单台服务器发起一次扫描,立即返回 running 状态的 run。
	StartScan(ctx context.Context, in ScanInput) (*Run, error)
	// StartFleetScan 对全部纳管服务器(排除 PVE 节点)按所选类型各发起一次扫描。
	StartFleetScan(ctx context.Context, scanTypes []string, trigger string) ([]Run, error)

	// ---- 查询与聚合 ----
	ListRuns(ctx context.Context, f RunFilter) ([]Run, error)
	GetRun(ctx context.Context, id string) (*Run, error)
	// RunFindings 读某次扫描的明细(artifact JSON → 内存过滤分页)。
	RunFindings(ctx context.Context, id string, f FindingFilter) (*FindingsPage, error)
	// RunLogs 读某次扫描的原始命令输出(running/旧数据无 artifact → 空日志)。
	RunLogs(ctx context.Context, id string) (*RunLogView, error)
	// Overview 聚合全员最新状态(含从未扫描的服务器占位)。
	Overview(ctx context.Context) (*Overview, error)
	// WafOverview 汇总 SafeLine WAF 实例的连通与拦截态势(防护态势卡)。
	WafOverview(ctx context.Context) ([]WafInstanceStatus, error)
	// UpsertCert 经 SafeLine OpenAPI 原位同步证书(certdeploy.WafGateway 适配面)。
	UpsertCert(ctx context.Context, instanceID, name, certPEM, keyPEM string) error
	// WafInstanceServerID 返回 WAF 实例登记的部署宿主(certdeploy 归属用)。
	WafInstanceServerID(ctx context.Context, instanceID string) (string, error)
	// CollectReport 组装 HTML 审计报告数据(serverIDs 空 = 全员;取各机最新 done 结果。
	// 指定清单中的未知服务器跳过——报告是快照,个别机器中途被删不应让整份导出失败)。
	CollectReport(ctx context.Context, serverIDs []string) ([]ServerReport, error)

	// ---- 运维 ----
	// Sweep 清掉 before 之前的 run 行(保留策略;artifact 文件不回收)。
	Sweep(ctx context.Context, before time.Time) (int64, error)
	// MarkStaleRuns 把遗留 running 行置 failed(服务重启后调用一次)。
	MarkStaleRuns(ctx context.Context) error
}

// service 是 db + vault + runner 支撑的 Service 实现。
type service struct {
	db          *sql.DB
	vault       vaultService
	runner      Runner
	artifacts   ArtifactStore
	baseCtx     context.Context // 扫描 goroutine 的父上下文(服务关停取消在途扫描)
	sem         chan struct{}   // fleet 并发闸(单机扫描也过闸,防连环触发压垮目标机)
	trivyImage  string
	nucleiImage string
	trivyDBRepo string    // 客户端本地模式的漏洞库仓库(镜像源;空 = trivy 默认 ghcr.io)
	waf         WafClient // SafeLine OpenAPI 客户端(测试注入 fake)
}

// Option 装配选项(New 消费)。
type Option func(*service)

// WithVault 注入保险库(token 密封;缺省时配置 token 的登记报 ErrVaultUnconfigured)。
func WithVault(v vaultService) Option { return func(s *service) { s.vault = v } }

// WithRunner 注入扫描执行器(SSH 上的 docker run);缺省时扫描报 ErrRunnerUnavailable。
func WithRunner(r Runner) Option { return func(s *service) { s.runner = r } }

// WithArtifacts 注入报告存储(artifact store);扫描与明细读取依赖它。
func WithArtifacts(a ArtifactStore) Option { return func(s *service) { s.artifacts = a } }

// WithBaseContext 注入扫描 goroutine 的父上下文(服务关停时取消在途扫描)。
func WithBaseContext(ctx context.Context) Option {
	return func(s *service) {
		if ctx != nil {
			s.baseCtx = ctx
		}
	}
}

// WithWafClient 注入 SafeLine OpenAPI 客户端(缺省 = 内置 HTTP 实现)。
func WithWafClient(c WafClient) Option {
	return func(s *service) {
		if c != nil {
			s.waf = c
		}
	}
}

// WithTrivyDBRepository 设置客户端本地模式(未登记 trivy-server 时)的漏洞库仓库。
// 典型场景:目标机到不了 ghcr.io,换镜像源(如 ghcr.nju.edu.cn/aquasecurity/trivy-db,
// 逗号分隔多个逐个回退)。已走 --server 的扫描不受影响(DB 在服务端)。
func WithTrivyDBRepository(repo string) Option {
	return func(s *service) {
		if repo != "" {
			s.trivyDBRepo = repo
		}
	}
}

// WithImages 覆盖扫描器镜像(默认 aquasec/trivy:latest 与 projectdiscovery/nuclei:latest)。
func WithImages(trivy, nuclei string) Option {
	return func(s *service) {
		if trivy != "" {
			s.trivyImage = trivy
		}
		if nuclei != "" {
			s.nucleiImage = nuclei
		}
	}
}

// fleetConcurrency 是同时在跑的扫描数上限(单机 + fleet 合计)。
const fleetConcurrency = 3

// New 构造安全中心服务(runner/artifacts 经 Option 注入;缺省时扫描不可用、登记照常)。
func New(db *sql.DB, opts ...Option) Service {
	s := &service{
		db:          db,
		baseCtx:     context.Background(),
		sem:         make(chan struct{}, fleetConcurrency),
		trivyImage:  "aquasec/trivy:latest",
		nucleiImage: "projectdiscovery/nuclei:latest",
		waf:         httpWafClient{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}
