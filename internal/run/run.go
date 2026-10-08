// Package run 是「流水线运行」的领域层(FR-13 / Story 3.1)。
//
// 它定义运行/步骤数据模型、状态机(合法转移校验)、进程内 worker pool(goroutine 池
// 调度)、可插拔 Runner 接口与本期桩实现。运行/步骤状态变更经 store 持久化到 SQLite,
// 并经内存事件总线发布,供 SSE 订阅(SSE 长连接不轮询 DB、不占用唯一 DB 连接)。
//
// 边界(本期不做,只留地基):
//   - 真实构建/部署执行 = Story 3-3/4-x(本期桩 runner 跑占位步骤→成功/可注入失败)。
//   - 真实触发创建 = Story 3-2(Create 供其调用;本期内部 + 测试用)。
//   - 日志内容流 = Story 3-6(本期 SSE 只推状态/步骤,不含日志正文)。
//   - targets 多机(Epic 4)/diagnosis 诊断(Epic 7)= run-detail DTO 前向声明的 null 块,
//     形状冻结于 httpapi 层;领域层本期不建模其内容。
//
// 多 run 并行、非事务、结果独立:单 run 失败/panic 不连累其它 run。
package run

import (
	"context"
	"errors"
	"time"
)

// 运行状态枚举(DB 存小写串;JSON 同值)。状态机:
//
//	queued → running → success | failed | partial_failed | rolled_back
//
// 取消复用 StatusFailed(进行中取消 → failed;经事件/步骤可观察)。
const (
	// StatusQueued 表示已入队、等待 worker 调度。
	StatusQueued = "queued"
	// StatusRunning 表示 worker 正在执行。
	StatusRunning = "running"
	// StatusWaitingApproval 表示运行阻塞在人工审批门(Story 8-4),等待批准/拒绝(非终态)。
	// worker 仍持有该 run(在审批门内阻塞);批准 → 回 running 继续,拒绝/超时 → failed。
	StatusWaitingApproval = "waiting_approval"
	// StatusSuccess 表示全部步骤成功(终态)。
	StatusSuccess = "success"
	// StatusFailed 表示执行失败或被取消(终态)。
	StatusFailed = "failed"
	// StatusPartialFailed 表示多机部分失败(终态;Epic 4 填充语义,本期仅为合法终态)。
	StatusPartialFailed = "partial_failed"
	// StatusRolledBack 表示已回滚(终态;Epic 4 填充语义,本期仅为合法终态)。
	StatusRolledBack = "rolled_back"
)

// 步骤状态枚举。
const (
	// StepPending 表示步骤待执行。
	StepPending = "pending"
	// StepRunning 表示步骤执行中。
	StepRunning = "running"
	// StepSuccess 表示步骤成功。
	StepSuccess = "success"
	// StepFailed 表示步骤失败。
	StepFailed = "failed"
	// StepSkipped 表示步骤被跳过。
	StepSkipped = "skipped"
	// StepWaitingApproval 表示步骤(审批门阶段)正等待人工批准(Story 8-4;非终态步骤状态)。
	StepWaitingApproval = "waiting_approval"
)

// 触发类型枚举。
const (
	// TriggerWebhook 表示 webhook 触发(真实接收=3-2)。
	TriggerWebhook = "webhook"
	// TriggerManual 表示手动触发。
	TriggerManual = "manual"
	// TriggerSchedule 表示定时(cron)触发(Story 8-6)。
	TriggerSchedule = "schedule"
	// TriggerChain 表示由上游流水线成功后串联触发(Story FR-8-11;由 internal/chain 串联钩子创建)。
	TriggerChain = "chain"
	// TriggerResume 表示由失败运行按节点恢复(派生重跑)创建。**落库为 manual**——恢复运行
	// 复用 manual 的 when 条件语义(when: type=manual 的阶段照常命中),Trigger.Type 不落此值;
	// 此常量仅供恢复链路内部语义标注/测试引用,恢复溯源走 Run.ResumeOfRunID。
	TriggerResume = "resume"
)

// 节点恢复动作枚举(ResumePlan.Actions 的元素;下标 = run_steps.ordinal)。
const (
	// NodeInherit 继承父运行结果为成功,不重跑(父运行该节点 success)。
	NodeInherit = "inherit"
	// NodeRun 正常执行(父运行该节点未执行过:skipped/pending 等)。
	NodeRun = "run"
	// NodeRetry 真实重跑该节点(父运行该节点 failed,用户选择重试)。
	NodeRetry = "retry"
	// NodeSkip 人工跳过该节点但放行下游(父运行该节点 failed,用户选择跳过继续)。
	NodeSkip = "skip"
)

// ResumePlan 是「失败运行按节点恢复」的计划:对派生运行里每个节点(ordinal 下标)的处置。
// 由 httpapi 层经 dagrun.BuildResumePlan 依当前 spec + 父运行步骤生成(全量、与布局等长),
// 持久化到 pipeline_runs.resume_plan_json,执行器(dagrun.Runner)纯按它应用,无需回读父运行。
type ResumePlan struct {
	ParentRunID string
	// Actions 下标 = step ordinal,长度必须等于当前 spec 的节点布局长度。
	Actions []string
}

// ResumeOf 报告本次运行是否为「按节点恢复」的派生运行。
func (r *Run) ResumeOf() bool { return r != nil && r.Resume != nil }

// 领域错误。错误体不含敏感数据。
var (
	// ErrNotFound 表示运行不存在。
	ErrNotFound = errors.New("run: not found")
	// ErrProjectNotFound 表示引用的项目不存在。
	ErrProjectNotFound = errors.New("run: project not found")
	// ErrInvalidTransition 表示非法状态转移(状态机拒绝)。
	ErrInvalidTransition = errors.New("run: invalid status transition")
	// ErrNotCancelable 表示运行处于终态、不可取消。
	ErrNotCancelable = errors.New("run: not cancelable")
	// ErrInvalidStatus 表示筛选/写入的状态枚举非法。
	ErrInvalidStatus = errors.New("run: invalid status")
	// ErrQueueFull 表示调度队列已满(突发创建过多),入队失败但 run 已入库。
	ErrQueueFull = errors.New("run: schedule queue full")
	// ErrPoolStopped 表示 worker pool 已停机,无法入队。
	ErrPoolStopped = errors.New("run: worker pool stopped")
	// ErrInvalidArtifactType 表示产物 type 非冻结枚举(image|jar|dist|archive)。
	ErrInvalidArtifactType = errors.New("run: invalid artifact type")
	// ErrInvalidTargetStatus 表示部署目标 status 非冻结枚举(pending|deploying|success|failed|rolled_back)。
	ErrInvalidTargetStatus = errors.New("run: invalid deploy target status")
	// ErrNotResumable 表示该运行不可按节点恢复(非失败/部分失败终态,或不存在)。
	ErrNotResumable = errors.New("run: not resumable")
	// ErrInvalidResumePlan 表示恢复计划非法(空 / 长度与布局不符 / 含未指定处置的失败节点)。
	ErrInvalidResumePlan = errors.New("run: invalid resume plan")
)

// Trigger 是运行触发上下文(冻结 DTO 的 trigger 块来源)。
//
// ResolvedEnvironment / ResolvedTargetServerIDs 是 Story 3-2 由 webhook 分支映射
// 解析出的**运行内部元数据**(供 Epic 4 的 targets 扇出消费);它们**不进入**冻结的
// run-detail DTO 输出(骨架所有权),仅持久化到 pipeline_runs 的内部列。
type Trigger struct {
	Type   string // webhook | manual
	Branch string
	Commit string
	Actor  string

	// ResolvedEnvironment 是解析出的目标环境名(手动触发为空)。
	ResolvedEnvironment string
	// ResolvedTargetServerIDs 是解析出的目标服务器引用 id 列表(手动触发为空)。
	ResolvedTargetServerIDs []string

	// Params 是参数化手动运行的 key=value 参数(Story 8-11);执行时注入 script 步骤容器作环境变量。
	// 非敏感明文(含密钥应走保险库引用);为空表示无参数。
	Params map[string]string

	// ChainSourceRunID 是串联触发(TriggerChain)时**触发本次运行的上游运行 id**(溯源;FR-8-11)。
	// 空 = 非串联触发(根运行)。持久化到 pipeline_runs.chain_source_run_id,经 Get 回读。
	ChainSourceRunID string
	// ChainDepth 是串联深度:根运行=0,每向下游串联一层 +1。由串联钩子据上游 depth+1 填入。
	// 用于环路安全(钩子拒绝超过上限的串联);持久化到 pipeline_runs.chain_depth,经 Get 回读。
	ChainDepth int
}

// Step 是穿珠时间线节点(运行步骤)。
//
// 节点级粒度(Story:运行详情下沉到 job 级):每个 step = 流水线里的一个 job(节点),
// Stage 记其所属阶段名,供前端按「阶段 → 节点」两级聚合(进度图阶段框 + 步骤详情分组)。
// 旧数据 / 非 DAG runner 的 step 其 Stage 可能为空(前端回退为单级展示)。
type Step struct {
	ID         string
	Name       string
	Stage      string // 所属阶段名(节点级分组;空 = 未分组,前端单级回退)
	Status     string // pending|running|success|failed|skipped
	Ordinal    int
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// StepDecl 是 Plan 声明一个步骤(节点)的入参:节点名 + 所属阶段名。
type StepDecl struct {
	Name  string
	Stage string
	// Initial 是该步骤的**初始状态**(可选):空/StepPending = 常规待执行;
	// StepSuccess / StepSkipped 供「按节点恢复」的继承/人工跳过节点在 Plan 时直接落终态
	// (执行器不会碰它们;带 finished_at,避免终态步骤悬挂无结束时刻)。其他值非法(Plan 报错)。
	Initial string
}

// Run 是一次流水线运行的领域模型。Steps 按 Ordinal 升序。
type Run struct {
	ID          string
	ProjectID   string
	ProjectName string // 冗余只读展示名(join projects),非持久列
	Status      string
	Trigger     Trigger
	Steps       []Step
	CreatedAt   time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time

	// FailureLog 是失败日志原文(脱敏前;桩 runner 合成,3-3/3-6 落地换真实日志)。
	// 空串 = 无失败日志。**绝不**原样出网:出网前必过 mask.Masker(诊断在 ai 层脱敏)。
	FailureLog string
	// Diagnosis 是已持久化的 AI 诊断(失败且已诊断时非 nil;否则 nil → run-detail diagnosis=null)。
	// 领域层只搬运形状(由 ai 层生成、httpapi 层落库),run 包不 import ai(经 hook 解耦)。
	Diagnosis *Diagnosis

	// SpecSource 是驱动本次运行的流水线配置来源(GitOps 配置来源可见性):
	// 仓库 `.pipewright.yml`(repo)或库内网页配置(stored)。由 Runner 在加载 spec 时经
	// StepSink.SetSpecSource 持久化,经 Get 回读供运行详情展示。
	SpecSource SpecSource

	// ResumeOfRunID 是「按节点恢复」溯源:本次运行由哪个失败运行派生而来(链式恢复各记直接父)。
	// 空串 = 普通运行。持久化到 pipeline_runs.resume_of_run_id,经 Get 回读供运行详情展示。
	ResumeOfRunID string
	// Resume 是本次恢复运行的节点处置计划(非恢复运行恒 nil):下标 = step ordinal。
	// 由 CreateResume 持久化(pipeline_runs.resume_plan_json),经 Get 解析回填,执行器纯按它应用。
	Resume *ResumePlan
}

// SpecSource 记录「本次运行的流水线 spec 来自哪里」(配置来源可见性)。
//   - Source="repo":由仓库根 `.pipewright.yml` 驱动;Ref=取用分支/ref、File=文件名。
//   - Source="stored":由库内(网页编辑)配置驱动;Fallback 非空表示「本想用仓库但回退了」,
//     其值为回退原因(disabled|no_file|invalid_yaml|degraded|empty|lookup_failed)。
//   - Source="":未记录(老运行 / 桩 runner 未上报),前端按未知处理(不展示徽标)。
type SpecSource struct {
	Source   string // "" | repo | stored
	Ref      string
	File     string
	Fallback string
}

// 配置来源枚举(DB 存小写串)。
const (
	// SpecSourceRepo 表示由仓库 `.pipewright.yml` 驱动(GitOps)。
	SpecSourceRepo = "repo"
	// SpecSourceStored 表示由库内(网页编辑)配置驱动。
	SpecSourceStored = "stored"
)

// 诊断状态枚举(对齐冻结 run-detail diagnosis 子 DTO 的 status)。
const (
	// DiagnosisReady 表示诊断有效(有 hypothesis 等)。
	DiagnosisReady = "ready"
	// DiagnosisUnavailable 表示诊断不可用(AI 未配/超时/不可解析/低质);带 reason。
	DiagnosisUnavailable = "unavailable"
	// DiagnosisPending 表示诊断进行中(本期不主动用;前端据此显 loading)。
	DiagnosisPending = "pending"
)

// DiagnosisEvidence 是一条日志证据(取自**脱敏后**日志;绝无明文 secret)。
type DiagnosisEvidence struct {
	Line      int    // 失败日志行号(1-based)
	Text      string // 该行脱敏后文本
	Highlight bool   // 是否命中行(高亮)
}

// Diagnosis 是 AI 失败诊断的领域模型(对齐冻结 diagnosis 子 DTO,与 ai 层解耦)。
// run 包仅持有此搬运形状,不依赖 ai 包;ai 层产出结构经 httpapi 层映射后落库。
type Diagnosis struct {
	Status          string              // ready | unavailable | pending
	Reason          string              // status≠ready 时人读原因(绝无密钥)
	Hypothesis      string              // 根因假说(措辞「假说,非结论」)
	Confidence      string              // high | medium | low
	AlternateCauses []string            // 低置信时非空
	FixSuggestions  []string            // 修复建议
	FixScript       string              // 可执行修复脚本/补丁片段(护城河;脱敏后搬运;空串=无)
	Evidence        []DiagnosisEvidence // 脱敏后日志证据
	GeneratedAt     time.Time           // 生成时刻
}

// ListFilter 是运行列表筛选/分页入参(零值合理:不筛选、首页)。
type ListFilter struct {
	ProjectID string // 空 = 不按项目筛选
	Status    string // 空 = 不按状态筛选
	Page      int    // 1-based;<1 视为 1
	PageSize  int    // <1 时用默认页大小
}

// ListResult 是分页列表结果。
type ListResult struct {
	Items []Run
	Page  int
	Total int
}

// terminalStatuses 是不可再转移的终态集合。
var terminalStatuses = map[string]bool{
	StatusSuccess:       true,
	StatusFailed:        true,
	StatusPartialFailed: true,
	StatusRolledBack:    true,
}

// allowedTransitions 定义状态机合法转移图(from → 允许的 to 集合)。
var allowedTransitions = map[string]map[string]bool{
	StatusQueued: {
		StatusRunning: true,
		StatusFailed:  true, // 入队即取消 / 调度前失败
	},
	StatusRunning: {
		StatusSuccess:         true,
		StatusFailed:          true,
		StatusPartialFailed:   true,
		StatusRolledBack:      true,
		StatusWaitingApproval: true, // 进入审批门:running → waiting_approval(Story 8-4)
	},
	StatusWaitingApproval: {
		StatusRunning: true, // 批准:waiting_approval → running 继续
		StatusFailed:  true, // 拒绝/超时/取消:waiting_approval → failed
	},
}

// IsTerminal 报告状态是否为终态。
func IsTerminal(status string) bool { return terminalStatuses[status] }

// IsStepTerminal 报告步骤状态是否为终态(success|failed|skipped)。
func IsStepTerminal(status string) bool {
	switch status {
	case StepSuccess, StepFailed, StepSkipped:
		return true
	default:
		return false
	}
}

// isValidStatus 报告状态是否为已知运行状态枚举。
func isValidStatus(status string) bool {
	switch status {
	case StatusQueued, StatusRunning, StatusWaitingApproval, StatusSuccess, StatusFailed, StatusPartialFailed, StatusRolledBack:
		return true
	default:
		return false
	}
}

// canTransition 报告从 from 到 to 是否为合法状态转移。
func canTransition(from, to string) bool {
	tos, ok := allowedTransitions[from]
	if !ok {
		return false
	}
	return tos[to]
}

// ctxCanceled 报告 context 是否已被取消(用于桩 runner 与 worker 协作)。
func ctxCanceled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
