// Package deploy 是「SSH 部署执行」领域层(FR-10 / Story 4.2)。
//
// 它把一次成功运行的某个产物经 SSH 部署到一台或多台目标服务器,记录每机结果,并据结果
// 更新 run 终态。部署命令一律 **array 化([]string)**:经注入的 target.Service.Exec
// 执行(各参数由 target 层 shell 转义后再交 SSH session),绝不拼接原始 shell 字符串
// (AC-SEC-02,杜绝命令注入)。SSH 密钥经 vault 由 target 层即用即弃,本层绝不接触明文;
// 输出 / message 绝无明文密钥。
//
// 边界(本期不做):零停机切换 / 回滚 = 4-4;多机并行扇出细节 = 4-5(本期顺序执行,失败
// 不连累其它机)。本层 import run(取产物 + 写部署结果 + 更新终态)与 target(SSH 执行);
// run 包**不** import deploy(避免环)。
package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 领域错误。错误体永不含明文 / 私钥 / 口令 / 内部栈。
var (
	// ErrRunNotFound 表示运行不存在。
	ErrRunNotFound = errors.New("deploy: run not found")
	// ErrRunNotSuccessful 表示运行非成功态,不可部署。
	ErrRunNotSuccessful = errors.New("deploy: run is not in a successful state")
	// ErrArtifactNotFound 表示该 run 下无指定产物。
	ErrArtifactNotFound = errors.New("deploy: artifact not found for run")
	// ErrArtifactSourceEmpty 表示部署节点指定的产物来源节点未产出可部署产物(精确绑定)。
	ErrArtifactSourceEmpty = errors.New("deploy: artifact source job produced no deployable artifact")
	// ErrArtifactAmbiguous 表示部署节点的产物绑定有歧义(多件命中 / glob 无命中),需 artifactName 消歧。
	ErrArtifactAmbiguous = errors.New("deploy: artifact binding is ambiguous")
	// ErrServerNotFound 表示指定的目标服务器不存在。
	ErrServerNotFound = errors.New("deploy: target server not found")
	// ErrNoFailedTargets 表示该 run 当前无可推进目标(failed / rolled_back / pending 皆无;retry 专用)。
	ErrNoFailedTargets = errors.New("deploy: run has no failed targets to retry")
	// ErrRunNotDeployed 表示该 run 尚未部署过(无 deploy_targets),不可重试。
	ErrRunNotDeployed = errors.New("deploy: run has not been deployed yet")
)

// truncateLen 是写入 message 的命令输出最大长度(防超大输出撑爆响应 / 内存)。
const truncateLen = 800

// execTimeout 是单台部署的执行超时(防一台挂死拖垮整次部署;失败不连累其它机)。
const execTimeout = 60 * time.Second

// maxParallelDeploys 是多机扇出的有界并发上限(Story 4.5;信号量防同时打爆 N 台 SSH)。
// 每机独立 goroutine,信号量 cap 4:目标机再多也不会一次性建超过 4 条 SSH 连接。
const maxParallelDeploys = 4

// maxConcurrentDiagnoses 是部署失败 best-effort 自动诊断的并发上限(有界;NFR-4),对齐 run 侧
// (internal/run.maxConcurrentDiagnoses)。诊断钩子脱离请求在独立 goroutine 跑,若无界,批量部署失败
// (多机/多 run)会无界并发打 LLM + 多份日志/解密 key 同时驻留。超限则跳过本次自动诊断(用户仍可手动触发)。
const maxConcurrentDiagnoses = 2

// uploadTimeout 是单次制品上传(SSH stdin 流)的独立超时:此前上传随 60s 命令超时走,大 jar/dist
// 经慢链路必然超时。默认 15 分钟,env PIPEWRIGHT_SSH_UPLOAD_TIMEOUT(秒)可调,夹紧 [1min, 2h]。
var uploadTimeout = func() time.Duration {
	d := 15 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("PIPEWRIGHT_SSH_UPLOAD_TIMEOUT")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			d = time.Duration(n) * time.Second
		}
	}
	if d < time.Minute {
		d = time.Minute
	}
	if d > 2*time.Hour {
		d = 2 * time.Hour
	}
	return d
}()

// uploadCtx 给上传操作挂独立长超时(不占用 60s 命令超时预算)。
func uploadCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, uploadTimeout)
}

// DeployInput 是一次部署请求(对齐 POST /api/runs/{id}/deploy 请求体)。
type DeployInput struct {
	RunID      string
	ArtifactID string
	// ServerIDs 显式目标机(回滚历史回放 / 显式 API):非空时优先于 Selector,存在性从严。
	ServerIDs []string
	// Selector 目标选择器(语法复用构建机池,见 selector_targets.go):`server:<id>` 钉单机、
	// 逗号分隔标签项圈选。空 / 零命中 → 无目标,本次部署按「跳过即成功」处理。
	Selector string
	// SelectorMode 是标签匹配方式(all = 全部项命中(且,默认);any = 任一项命中(或))。
	SelectorMode string
	// Config 是可选部署参数(如目标路径);本期最小消费(dist 用 Path 决定部署目录)。
	Config map[string]string
	// HealthCheck 是可选的部署后健康门控(Story 4.3 / FR-12)。
	// nil 或 type=none → 跳过(向后兼容 4-2:部署命令成功即 success)。
	HealthCheck *HealthCheck
	// Strategy 是部署策略(Story 8-8 / FR-8-8):rolling(默认)| canary | blue_green。
	// 空 / 未知 → rolling(行为与未加策略前一致)。金丝雀批量经 Config["canaryCount"|"canaryPercent"]。
	Strategy string
}

// RetryInput 是一次「仅重试失败目标」请求(对齐 POST /api/runs/{id}/deploy/retry 请求体)。
//
// 无迁移、复用 deploy_targets:retry 不持久化原始部署产物/配置,故 ArtifactID + Config +
// HealthCheck 由调用方(前端已持有上次部署表单)随请求带回,复用既有 deployOne 链路。
// ServerIDs 可选:省略 → 重试该 run 当前所有 failed/rolled_back 目标;给定 → 只重试其中指定的
// (须是已有失败目标;不在失败集合内的忽略)。
type RetryInput struct {
	RunID       string
	ArtifactID  string
	ServerIDs   []string
	Config      map[string]string
	HealthCheck *HealthCheck
}

// TargetResult 是一台目标机的部署结果(与 run.DeployTarget 同形,供 HTTP 层映射回 DTO)。
type TargetResult struct {
	ServerID   string
	ServerName string
	Status     string // run.Target* 枚举:success | failed | …
	Message    string // 人读摘要(绝无明文密钥)
	StartedAt  time.Time
	FinishedAt *time.Time
}

// Service 定义部署执行对外接口(冻结点由 httpapi 层 DTO 承载;本接口可演进)。
type Service interface {
	// Deploy 取 run 的指定产物 → 校验成功态 / 产物 → 经 ServerIDs(显式)或 Selector(标签圈选)
	// 解析目标机 → 逐机经 SSH 执行该产物类型的部署命令 → 持久化每机结果 → 据结果更新 run 终态 →
	// 返回每机结果。无目标(选择器空 / 零命中)→ 返回空结果、不写 deploy_targets、不动终态(跳过即成功)。
	//
	// 定位类错误(run 非成功 / 无产物 / 服务器不存在 / 选择器语法非法)上抛,供 HTTP 层 422/404。
	// **执行失败不上抛**:该机 status=failed + 人读 message,整体仍返回结果(整体 200)。
	Deploy(ctx context.Context, in DeployInput) ([]TargetResult, error)

	// RetryFailed 重试该 run 当前**可推进**的目标:failed / rolled_back / pending
	// (Story 4.5;FR-13「仅重试失败」+ 统一滚动的「继续铺开未部署机」)。
	//
	// 查 run 既有 deploy_targets → 取可推进目标(可经 ServerIDs 进一步限定)→ 复用产物 + 配置
	// 对这些机并行重跑 deployOne → **逐目标 upsert**(成功目标不动,只覆盖被重试目标的行)→
	// 据全量目标重算 run 终态 → 返回该 run 全量最新 targets。
	//
	// 定位类错误(run 不存在 / 非失败态 / 无可推进目标 / 无产物 / 服务器不存在)上抛,供 HTTP 层 422/404。
	// 执行失败不上抛:重试目标置 failed + 人读 message,整体仍 200。
	RetryFailed(ctx context.Context, in RetryInput) ([]TargetResult, error)

	// DeployForStage 是「流水线 deploy_ssh 节点」用的中途部署:取该 run 已产出的首个可发布产物
	// (dist/jar/archive)→ 按策略部署到目标机 → 持久化每机结果(填 run-detail targets)。
	// **不校验 run 状态**(流水线执行中 run 仍 running)、**不置 run 终态**(终态由 dag 调度器控制)。
	// 目标经 selector 圈选(`server:<id>` 钉单机 / 标签项;见 selector_targets.go):空 / 零命中 →
	// 返回空结果 nil(节点按跳过成功处理)。无可发布产物 → ErrArtifactNotFound;选择器语法非法 →
	// ErrInvalidSelector;有目标失败 → 返回 error 令该阶段失败、阻断下游(复用 dagrun「阶段失败→下游不执行」)。
	DeployForStage(ctx context.Context, runID string, selector string, cfg map[string]string, strategy string) ([]TargetResult, error)

	// CheckHealth 是「流水线 health_check 节点」用的目标机健康探测:经 selector/selectorMode 圈选
	// 目标机(与部署节点同一套选择语义),用 hc 逐台探测(重试);全部通过 → (结果, nil);
	// 任一未通过 → (结果, 人读 error)。无目标机且 hc 为 http 型 → 从平台本机探测。
	CheckHealth(ctx context.Context, selector, selectorMode string, hc *HealthCheck) ([]HealthProbeResult, error)
}

// service 是 run + target 支撑的 Service 实现。
type service struct {
	targets target.Service
	runs    run.Service
	// diagnoseHook 是部署失败后的 best-effort 自动诊断钩子(Story 4.6;FR-22 种子)。
	// 由 main 注入(复用 7-2 NewDiagnoseHook);nil 则跳过。deploy 不 import ai(钩子解耦)。
	diagnoseHook func(ctx context.Context, runID string)
	// diagnoseSem 限流自动诊断 goroutine(有界并发,NFR-4;cap=maxConcurrentDiagnoses)。
	// try-acquire 满则跳过本次自动诊断(best-effort 语义不变)。在 New 中初始化。
	diagnoseSem chan struct{}
	// artStore 是制品库(Story 8-16):非 nil 时部署 release 类「已归档」产物会取真字节经 SSH 上传到
	// 目标机;nil 或产物非归档 → 旧占位路径(向后兼容)。由 main 注入(WithArtifactStore)。
	artStore *artifactstore.Store
	// instanceGateway 是服务注册网关(nginx)的实例级能力(instance_rolling 默认策略消费):
	// 反查部署容器名对应的网关实例 + 原子摘挂。main 经适配器晚绑(deploy 不 import servicereg);
	// nil → instance_rolling 自动回退既有滚动,行为不变。
	instanceGateway InstanceGateway
}

// Option 配置 deploy.Service(如注入诊断钩子)。
type Option func(*service)

// WithDiagnoseHook 注入部署失败后的 best-effort 自动诊断钩子(Story 4.6;FR-22 种子):
// 部署 failed/partial_failed → 合成失败日志(SetFailureLog)+ 触发钩子,让 7-2 诊断飞轮覆盖部署失败。
func WithDiagnoseHook(fn func(ctx context.Context, runID string)) Option {
	return func(s *service) { s.diagnoseHook = fn }
}

// WithArtifactStore 注入制品库(Story 8-16):部署 release 类已归档产物时取真字节上传目标机。
func WithArtifactStore(st *artifactstore.Store) Option {
	return func(s *service) {
		if st != nil {
			s.artStore = st
		}
	}
}

// New 构造部署 Service。
//   - targetSvc:通用 SSH 执行层(Story 4.1),部署命令经其 Exec 执行(array 不拼 shell)。
//   - runSvc   :运行领域层,取产物 / 写部署结果 / 更新终态。
//   - opts     :可选(如 WithDiagnoseHook 注入部署失败诊断,Story 4.6)。
//
// 不做任何重活(无 init() 副作用,避免抬高空载内存)。
func New(targetSvc target.Service, runSvc run.Service, opts ...Option) Service {
	s := &service{
		targets:     targetSvc,
		runs:        runSvc,
		diagnoseSem: make(chan struct{}, maxConcurrentDiagnoses),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// seedDiagnosisOnFailure 在部署终态为 failed/partial_failed 时合成失败日志并触发 best-effort 诊断
// (Story 4.6;FR-22 种子):让 7-2 的 AI 失败分析 + 7-5 反馈闭环覆盖部署失败,而非只覆盖构建失败。
// 失败日志取自各失败/回滚目标的 message(平台构造,无明文密钥)。绝不阻断部署结果返回。
func (s *service) seedDiagnosisOnFailure(ctx context.Context, runID, status string, targets []TargetResult) {
	if status != run.StatusFailed && status != run.StatusPartialFailed {
		return
	}
	var b strings.Builder
	b.WriteString("[部署失败 deploy] 以下目标机部署未成功:\n")
	for _, t := range targets {
		if t.Status == run.TargetFailed || t.Status == run.TargetRolledBack {
			fmt.Fprintf(&b, "- %s(%s):%s\n", t.ServerName, t.Status, t.Message)
		}
	}
	if err := s.runs.SetFailureLog(ctx, runID, b.String()); err != nil {
		return // best-effort:写失败日志失败则不诊断,不阻断
	}
	if s.diagnoseHook != nil {
		hook := s.diagnoseHook
		// 并发有界(NFR-4):try-acquire,满则跳过本次自动诊断(用户仍可手动触发)。
		select {
		case s.diagnoseSem <- struct{}{}:
		default:
			return // 已达上限:跳过自动诊断,不阻断部署结果返回。
		}
		go func() {
			defer func() { <-s.diagnoseSem }()
			defer func() { _ = recover() }()
			hook(context.WithoutCancel(ctx), runID)
		}()
	}
}

func (s *service) Deploy(ctx context.Context, in DeployInput) ([]TargetResult, error) {
	// 1) 校验 run 存在 + 成功态。
	rn, err := s.runs.Get(ctx, in.RunID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	if rn.Status != run.StatusSuccess && rn.Status != run.StatusPartialFailed {
		// 仅成功 / 部分成功的运行有可部署产物;进行中 / 失败 / 排队 → 不可部署。
		return nil, ErrRunNotSuccessful
	}

	// 2) 定位产物(必须属于该 run)。
	arts, err := s.runs.ListArtifacts(ctx, in.RunID)
	if err != nil {
		return nil, err
	}
	var artifact *run.Artifact
	for i := range arts {
		if arts[i].ID == in.ArtifactID {
			a := arts[i]
			artifact = &a
			break
		}
	}
	if artifact == nil {
		return nil, ErrArtifactNotFound
	}

	// 3) 解析目标机:显式 ServerIDs 优先(存在性从严,任一不存在 → 422 整次拒绝);否则按
	// Selector 标签圈选(匹配方式 in.SelectorMode)。无目标(选择器空 / 零命中)→ 跳过即成功:
	// 不写 deploy_targets、不动 run 终态。
	servers, err := s.resolveTargets(ctx, in.ServerIDs, in.Selector, normalizeSelectorMode(in.SelectorMode))
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return []TargetResult{}, nil
	}

	// 4) 统一滚动部署:预检故障机优先排序 → 按首批/每批台数分批 → 任一批失败停止铺开 →
	// 每机按产物类型 + 网关托管情况执行(换实例 / 摘挂 / 直接部署),失败机独立回滚。
	// 每机独立 goroutine + recover,有界信号量(cap 4)防同时打爆 N 台;结果按输入顺序独立收集。
	var results []TargetResult
	results = s.deployRolling(ctx, servers, *artifact, in.Config, fixedHealthSpec(in.HealthCheck))

	// 5) 持久化每机结果(填 run-detail targets slot)。
	dts := make([]run.DeployTarget, 0, len(results))
	for _, r := range results {
		dts = append(dts, run.DeployTarget{
			RunID:      in.RunID,
			ServerID:   r.ServerID,
			ServerName: r.ServerName,
			Status:     r.Status,
			Message:    r.Message,
			StartedAt:  r.StartedAt,
			FinishedAt: r.FinishedAt,
		})
	}
	if err := s.runs.SaveDeployTargets(ctx, in.RunID, dts); err != nil {
		return nil, err
	}

	// 6) 据结果置 run 终态:全成功 → success;有失败 → partial_failed;全失败 → failed。
	final := overallStatus(results)
	if err := s.runs.SetDeployTerminal(ctx, in.RunID, final); err != nil {
		return nil, err
	}

	// 7) 部署失败 → best-effort 触发 AI 诊断(Story 4.6;FR-22 种子;不阻断返回)。
	s.seedDiagnosisOnFailure(ctx, in.RunID, final, results)

	return results, nil
}

// DeployForStage 见接口注释:流水线部署节点的中途部署(不校验 run 状态、不置终态)。
//
// 产物定位(精确绑定优先):cfg["artifactJob"](产物来源节点)非空 → 严格按产物元数据
// sourceJob(可选 artifactName glob 消歧)选件,选不出/歧义 → 报错(绝不静默换别的产物);
// 旧配置(无 artifactJob)→ 按类型偏好挑选 + 记弃用告警。健康门控从 cfg["healthUrl"|"healthCommand"]
// 构造(此前硬编码 nil,流水线节点健康检查/回滚因此从未生效)。
func (s *service) DeployForStage(ctx context.Context, runID string, selector string, cfg map[string]string, strategy string) ([]TargetResult, error) {
	// 目标经 selector 圈选(`server:<id>` / 标签项,匹配方式 cfg["selectorMode"]):空 / 零命中 → 跳过。
	servers, err := s.resolveTargets(ctx, nil, selector, normalizeSelectorMode(cfg["selectorMode"]))
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return []TargetResult{}, nil
	}
	// 全量目标(恢复过滤前):注册表清理的基准 —— 恢复运行跳过的机器已成功,其实例必须保留。
	allServerIDs := make([]string, 0, len(servers))
	for _, srv := range servers {
		allServerIDs = append(allServerIDs, srv.ID)
	}
	// 恢复运行(按节点恢复)的部署节点走**增量语义**:本 run 的部署目标行(创建时已复制父运行)
	// 里已 success 的机器不再重复部署,只推 failed/rolled_back/pending 及选择器新增的机器;
	// 持久化改逐目标 upsert(保留未涉及机器的继承状态)。普通运行行为不变(整批 save)。
	persist := stagePersistFunc(s.runs.SaveDeployTargets)
	skipSucceeded, err := s.resumeSucceededServers(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(skipSucceeded) > 0 {
		filtered := make([]*target.Server, 0, len(servers))
		for _, srv := range servers {
			if !skipSucceeded[srv.ID] {
				filtered = append(filtered, srv)
			}
		}
		if len(filtered) == 0 {
			// 全部目标机都已成功:不落库(保留继承行原状),返回非空跳过结果——节点按成功放行,
			// 且调用方(build 层 runDeployJob)会逐机打出「已成功,跳过重部署」日志。
			return skippedSuccessResults(servers, skipSucceeded), nil
		}
		if len(filtered) < len(servers) {
			servers = filtered
		}
		persist = stagePersistFunc(s.runs.UpsertDeployTargets)
	}
	// 「命令型」部署(deployMode=command;旧键 artifactType=command 兼容):不取构建产物,
	// 直接在目标机执行 cfg["restartCommand"]。与产物发布完全隔离。allServerIDs 一并传入,
	// 供全成功后的注册表清理(与产物路径同语义)。
	if strings.TrimSpace(cfg["deployMode"]) == "command" || strings.TrimSpace(cfg["artifactType"]) == "command" {
		return s.runCommandOnly(ctx, runID, servers, allServerIDs, cfg, persist)
	}
	// 取该 run 已产出的可部署产物并精确挑选。
	arts, err := s.runs.ListArtifacts(ctx, runID)
	if err != nil {
		return nil, err
	}
	sourceJob := strings.TrimSpace(cfg["artifactJob"])
	var artifact *run.Artifact
	if sourceJob != "" {
		artifact, err = pickStageArtifactExplicit(arts, sourceJob, strings.TrimSpace(cfg["artifactName"]))
		if err != nil {
			return nil, err
		}
	} else {
		// 旧配置兜底:按类型偏好挑选(文件优先,image 兜底)。弃用路径 —— 仅存量流水线会走到。
		artifact = pickStageArtifact(arts, strings.TrimSpace(cfg["artifactType"]))
		if artifact == nil {
			return nil, ErrArtifactNotFound
		}
	}

	// 健康门控:从节点 config 构造**待解析 spec**(容器部署的端口要等自动分配完成才可知 ——
	// 仅填 healthPath 时探测地址 = http://127.0.0.1:<分配出的宿主端口><path>,deployOne 内解析)。
	// 容器名与部署本体同一解析(config 显式名 → 产物名 → app),docker exec 探测才打得中。
	hsp := healthCheckSpecFromCfg(cfg, imageContainerName(*artifact, cfg))

	// 服务注册联动(非容器产物):实例行的预注册(detached)+ 摘/挂在 deployWithGatewayDetach
	// 内逐机做(消除「先注册后部署」的未就绪接流窗口)。容器产物的注册在 instance_rolling
	// 内做(需感知端口分配与轮转)。
	regService := regServiceRef(cfg)

	results := s.deployRolling(ctx, servers, *artifact, cfg, hsp)

	// 部署全部成功 → 同步清理注册表(删除该服务下不再属于本次目标集合的实例,含遗留行)。
	// 部分失败/重试时不清(拓扑未定型;重试成功后下一轮自然收敛)。
	if regService != "" && s.instanceGateway != nil && allTargetSuccess(results) && len(allServerIDs) > 0 {
		if perr := s.instanceGateway.PruneInstances(ctx, regService, allServerIDs); perr != nil {
			cmdLogFrom(ctx)(cmdStreamStderr, "  ⚠ 同步清理注册实例失败:"+humanExecError(perr))
		}
	}

	// 持久化每机结果(填 run-detail targets slot);**不置 run 终态**(dag 调度器控制)。
	// 恢复运行为逐目标 upsert(见上方增量语义),普通运行为整批 save。
	dts := make([]run.DeployTarget, 0, len(results))
	for _, r := range results {
		dts = append(dts, run.DeployTarget{
			RunID: runID, ServerID: r.ServerID, ServerName: r.ServerName,
			Status: r.Status, Message: r.Message, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		})
	}
	if err := persist(ctx, runID, dts); err != nil {
		return nil, err
	}
	return results, nil
}

// stagePersistFunc 是 DeployForStage 的目标结果持久化函数形状(整批 save / 逐目标 upsert 二选一)。
type stagePersistFunc func(ctx context.Context, runID string, targets []run.DeployTarget) error

// resumeSucceededServers 在 run 为「恢复运行」时返回其部署目标里已 success 的机器集合
// (增量部署要跳过的机器);普通运行 / 读不到目标行 → 空集合(行为不变)。
func (s *service) resumeSucceededServers(ctx context.Context, runID string) (map[string]bool, error) {
	rn, err := s.runs.Get(ctx, runID)
	if err != nil || rn == nil || rn.Resume == nil {
		return nil, nil // 非恢复运行(Get 失败亦按普通运行走,后续持久化自会暴露问题)
	}
	existing, err := s.runs.ListDeployTargets(ctx, runID)
	if err != nil {
		return nil, err
	}
	skip := make(map[string]bool, len(existing))
	for _, t := range existing {
		if t.Status == run.TargetSuccess {
			skip[t.ServerID] = true
		}
	}
	return skip, nil
}

// skippedSuccessResults 为「全部目标机都已成功」的恢复部署合成跳过结果(逐机一条,
// 供节点日志展示;不落库,保留继承目标行的原状)。调用前提:servers ⊆ skipSucceeded。
func skippedSuccessResults(servers []*target.Server, _ map[string]bool) []TargetResult {
	results := make([]TargetResult, 0, len(servers))
	for _, srv := range servers {
		results = append(results, TargetResult{
			ServerID: srv.ID, ServerName: srv.Name, Status: run.TargetSuccess,
			Message: "恢复部署:该机上次已部署成功,跳过重部署",
		})
	}
	return results
}

// runCommandOnly 执行「命令型」部署:在每台目标机直接跑 cfg["restartCommand"](无构建产物)。
// 命令文本里的 {{param}} 已在 build 层(runDeployJob)用本次运行参数渲染好;此处逐机执行 +
// 经 s.exec 把命令/输出实时回流步骤日志(脱敏由 sink Masker 兜底)+ 持久化每机结果
// (persist 由调用方按「普通运行=整批 save / 恢复运行=逐目标 upsert」选定)。
// 服务注册联动与产物路径一致:逐机包「预注册(detached)→ 摘 → 执行 → 挂回」,
// 全部成功后按 allServerIDs(恢复过滤前的全量目标)同步清理注册表。
func (s *service) runCommandOnly(ctx context.Context, runID string, servers []*target.Server, allServerIDs []string, cfg map[string]string, persist stagePersistFunc) ([]TargetResult, error) {
	command := strings.TrimSpace(cfg["restartCommand"])
	if command == "" {
		return nil, fmt.Errorf("deploy: 命令型部署缺少 restartCommand")
	}
	results := make([]TargetResult, 0, len(servers))
	for _, srv := range servers {
		results = append(results, s.deployWithGatewayDetach(ctx, srv, cfg, func() TargetResult {
			started := time.Now().UTC()
			out, err := s.exec(ctx, srv.ID, []string{"sh", "-c", command})
			fin := time.Now().UTC()
			tr := TargetResult{ServerID: srv.ID, ServerName: srv.Name, StartedAt: started, FinishedAt: &fin}
			switch {
			case err != nil:
				tr.Status = run.TargetFailed
				tr.Message = humanExecError(err)
			case out != nil && out.ExitCode != 0:
				tr.Status = run.TargetFailed
				tr.Message = fmt.Sprintf("命令退出码 %d", out.ExitCode)
			default:
				tr.Status = run.TargetSuccess
				tr.Message = "命令执行成功"
			}
			return tr
		}))
	}
	// 部署全部成功 → 同步清理注册表(删除该服务下不再属于本次目标集合的实例,含遗留行)。
	// 部分失败/重试时不清(拓扑未定型;重试成功后下一轮自然收敛)。
	if regService := regServiceRef(cfg); regService != "" && s.instanceGateway != nil && allTargetSuccess(results) && len(allServerIDs) > 0 {
		if perr := s.instanceGateway.PruneInstances(ctx, regService, allServerIDs); perr != nil {
			cmdLogFrom(ctx)(cmdStreamStderr, "  ⚠ 同步清理注册实例失败:"+humanExecError(perr))
		}
	}
	dts := make([]run.DeployTarget, 0, len(results))
	for _, r := range results {
		dts = append(dts, run.DeployTarget{
			RunID: runID, ServerID: r.ServerID, ServerName: r.ServerName,
			Status: r.Status, Message: r.Message, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		})
	}
	if err := persist(ctx, runID, dts); err != nil {
		return nil, err
	}
	return results, nil
}

// deployableStageArtifact 判定产物在「流水线部署节点」可被部署:文件发布类(dist/jar/archive)
// 或镜像类(image)。其余类型不可部署(无对应编排)。
func deployableStageArtifact(a run.Artifact) bool {
	return releaseModeArtifact(a) || a.Type == run.ArtifactImage
}

// pickStageArtifactExplicit 按「产物来源节点」精确挑一件可部署产物(部署节点产物精确绑定):
//   - 只在 deployableStageArtifact 且元数据 sourceJob == sourceJob 的产物里选;
//   - nameGlob 非空 → 再按 glob 匹配产物名 / 工作区路径基名消歧;
//   - 命中 0 件 → ErrArtifactSourceEmpty(明确报「指定节点未产出」,绝不静默换别的产物);
//   - 命中 >1 件且未给 nameGlob → ErrArtifactAmbiguous(列出候选名,要求配 artifactName)。
func pickStageArtifactExplicit(arts []run.Artifact, sourceJob, nameGlob string) (*run.Artifact, error) {
	type cand struct {
		idx  int
		name string
	}
	var cands []cand
	for i := range arts {
		a := arts[i]
		if !deployableStageArtifact(a) {
			continue
		}
		if strings.TrimSpace(fmt.Sprint(a.Metadata["sourceJob"])) != sourceJob {
			continue
		}
		cands = append(cands, cand{idx: i, name: a.Name})
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("%w:产物来源节点 %q 未产出可部署产物(检查该节点 artifactPath / 上游阶段顺序)", ErrArtifactSourceEmpty, sourceJob)
	}
	if nameGlob != "" {
		var matched []cand
		for _, c := range cands {
			wsBase := ""
			if p, ok := arts[c.idx].Metadata["workspacePath"].(string); ok {
				wsBase = path.Base(p)
			}
			if ok, _ := path.Match(nameGlob, c.name); ok {
				matched = append(matched, c)
			} else if wsBase != "" {
				if ok2, _ := path.Match(nameGlob, wsBase); ok2 {
					matched = append(matched, c)
				}
			}
		}
		if len(matched) == 0 {
			names := make([]string, 0, len(cands))
			for _, c := range cands {
				names = append(names, c.name)
			}
			return nil, fmt.Errorf("%w:来源节点 %q 无产物匹配 %q(实际产物:%s)", ErrArtifactAmbiguous, sourceJob, nameGlob, strings.Join(names, ", "))
		}
		if len(matched) > 1 {
			names := make([]string, 0, len(matched))
			for _, c := range matched {
				names = append(names, c.name)
			}
			return nil, fmt.Errorf("%w:来源节点 %q 有 %d 件产物匹配 %q:%s", ErrArtifactAmbiguous, sourceJob, len(matched), nameGlob, strings.Join(names, ", "))
		}
		return &arts[matched[0].idx], nil
	}
	if len(cands) > 1 {
		names := make([]string, 0, len(cands))
		for _, c := range cands {
			names = append(names, c.name)
		}
		return nil, fmt.Errorf("%w:产物来源节点 %q 产出了 %d 件可部署产物(%s),请在部署节点配置 artifactName 指定要部署哪一件", ErrArtifactAmbiguous, sourceJob, len(cands), strings.Join(names, ", "))
	}
	return &arts[cands[0].idx], nil
}

// pickStageArtifact 旧配置兜底:从该 run 的产物里按类型偏好挑一件(文件优先,image 兜底)。
// 弃用路径 —— 仅未配 artifactJob 的存量流水线会走到;新表单一律精确绑定(pickStageArtifactExplicit)。
func pickStageArtifact(arts []run.Artifact, prefer string) *run.Artifact {
	prefer = strings.ToLower(prefer)
	var firstImage, firstRelease, firstExact *run.Artifact
	for i := range arts {
		a := arts[i]
		if !deployableStageArtifact(a) {
			continue
		}
		if firstExact == nil && prefer != "" && a.Type == prefer {
			firstExact = &arts[i]
		}
		if a.Type == run.ArtifactImage && firstImage == nil {
			firstImage = &arts[i]
		}
		if releaseModeArtifact(a) && firstRelease == nil {
			firstRelease = &arts[i]
		}
	}
	if firstExact != nil {
		return firstExact
	}
	if prefer == run.ArtifactImage {
		if firstImage != nil {
			return firstImage
		}
		return firstRelease
	}
	if firstRelease != nil {
		return firstRelease
	}
	return firstImage
}

// healthCheckFromCfg 从部署节点 config 立即构造健康门控(端口不可推导时端口型返回 nil)。
// 语义与 healthCheckSpecFromCfg 完全一致 —— 优先级 healthCommand > healthExec > healthUrl >
// 端口+路径;测试与无自动端口的消费方用它,主链路走 spec + 部署期端口推导。
func healthCheckFromCfg(cfg map[string]string, containerName string) *HealthCheck {
	return healthCheckSpecFromCfg(cfg, containerName).resolve(0)
}

// cfgNonNeg 解析 cfg 里的非负整数(缺省 / 非法 → 0,交由 HealthCheck 默认值兜底)。
func cfgNonNeg(cfg map[string]string, key string) int {
	raw := strings.TrimSpace(cfg[key])
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// allTargetSuccess 报告一批结果是否全部成功(注册表同步清理的门槛)。
func allTargetSuccess(results []TargetResult) bool {
	if len(results) == 0 {
		return false
	}
	for i := range results {
		if results[i].Status != run.TargetSuccess {
			return false
		}
	}
	return true
}

// retryableTargetStatus 报告某目标状态是否可被 RetryFailed 推进:
//   - failed / rolled_back:本机部署失败或已回滚(仍运行旧版本);
//   - pending:统一滚动在前一批失败后停止铺开,本机**从未部署**(仍运行旧版本)。
//
// 三者的共同点是「用户修好后希望把这台机推上去」,故都算可重试;success 不在其列(不重复部署)。
func retryableTargetStatus(status string) bool {
	switch status {
	case run.TargetFailed, run.TargetRolledBack, run.TargetPending:
		return true
	default:
		return false
	}
}

// RetryFailed 重试该 run 当前可推进的目标(failed / rolled_back / pending;Story 4.5;FR-13)。
// 见 Service 接口注释。
func (s *service) RetryFailed(ctx context.Context, in RetryInput) ([]TargetResult, error) {
	// 1) 校验 run 存在 + 处于失败/部分失败态(成功 run 无失败目标可重试)。
	rn, err := s.runs.Get(ctx, in.RunID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	if rn.Status != run.StatusFailed && rn.Status != run.StatusPartialFailed {
		// 仅失败/部分失败的部署有「失败目标」可重试;成功/进行中/排队 → 422 人读。
		return nil, ErrNoFailedTargets
	}

	// 2) 取该 run 既有部署目标;无 → 未部署过(不可重试)。
	existing, err := s.runs.ListDeployTargets(ctx, in.RunID)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return nil, ErrRunNotDeployed
	}

	// 3) 取可重试目标集合(failed / rolled_back / pending);若请求给定 ServerIDs,取其与可重试集合的
	// 交集。pending = 统一滚动在前一批失败后停止铺开、本机尚未部署(仍运行旧版本)—— 修复失败机后
	// 「重试」即把这些机继续推上去,否则它们会永久停在旧版本且 run 终态永远回不到 success。
	wantSet := map[string]struct{}{}
	for _, sid := range in.ServerIDs {
		wantSet[sid] = struct{}{}
	}
	retrySIDs := make([]string, 0, len(existing))
	for i := range existing {
		t := existing[i]
		if !retryableTargetStatus(t.Status) {
			continue
		}
		if len(wantSet) > 0 {
			if _, ok := wantSet[t.ServerID]; !ok {
				continue
			}
		}
		retrySIDs = append(retrySIDs, t.ServerID)
	}
	if len(retrySIDs) == 0 {
		return nil, ErrNoFailedTargets
	}

	// 4) 定位产物(复用上次部署的产物;由请求携带 artifactId,必须属于该 run)。
	arts, err := s.runs.ListArtifacts(ctx, in.RunID)
	if err != nil {
		return nil, err
	}
	var artifact *run.Artifact
	for i := range arts {
		if arts[i].ID == in.ArtifactID {
			a := arts[i]
			artifact = &a
			break
		}
	}
	if artifact == nil {
		return nil, ErrArtifactNotFound
	}

	// 5) 解析待重试服务器(任一不存在 → 422,整次拒绝)。
	servers := make([]*target.Server, 0, len(retrySIDs))
	for _, sid := range retrySIDs {
		srv, gerr := s.targets.Get(ctx, sid)
		if gerr != nil {
			if errors.Is(gerr, target.ErrNotFound) {
				return nil, ErrServerNotFound
			}
			return nil, gerr
		}
		servers = append(servers, srv)
	}

	// 6) 对这些机并行重跑 deployOne(复用产物 + 配置)。
	retried := s.deployFanout(ctx, servers, *artifact, in.Config, fixedHealthSpec(in.HealthCheck))

	// 7) **逐目标 upsert**:只更新被重试目标对应行,**保留**本次未重试的成功目标(别用整批删的 SaveDeployTargets)。
	dts := make([]run.DeployTarget, 0, len(retried))
	for _, r := range retried {
		dts = append(dts, run.DeployTarget{
			RunID:      in.RunID,
			ServerID:   r.ServerID,
			ServerName: r.ServerName,
			Status:     r.Status,
			Message:    r.Message,
			StartedAt:  r.StartedAt,
			FinishedAt: r.FinishedAt,
		})
	}
	if err := s.runs.UpsertDeployTargets(ctx, in.RunID, dts); err != nil {
		return nil, err
	}

	// 8) 据**全量**最新目标重算 run 终态(全成功 → success;仍有失败 → partial_failed/failed)。
	all, err := s.runs.ListDeployTargets(ctx, in.RunID)
	if err != nil {
		return nil, err
	}
	final := overallStatusOfTargets(all)
	if err := s.runs.SetDeployTerminal(ctx, in.RunID, final); err != nil {
		return nil, err
	}

	// 返回该 run 全量最新 targets(经 TargetResult 形状,供 HTTP 层映射;HTTP 层亦会回读权威结果)。
	out := make([]TargetResult, 0, len(all))
	for i := range all {
		t := all[i]
		out = append(out, TargetResult{
			ServerID:   t.ServerID,
			ServerName: t.ServerName,
			Status:     t.Status,
			Message:    t.Message,
			StartedAt:  t.StartedAt,
			FinishedAt: t.FinishedAt,
		})
	}

	// 重试后仍失败 → best-effort 触发 AI 诊断(Story 4.6;不阻断返回)。
	s.seedDiagnosisOnFailure(ctx, in.RunID, final, out)

	return out, nil
}

// deployFanout 并行扇出多机部署(Story 4.5):每机独立 goroutine + recover,有界信号量
// (maxParallelDeploys)限并发 SSH;单机 panic 不连累其它机(recover → 该机 failed 人读)。
// 结果按 servers 输入顺序回填(稳定可断言),失败台不阻断其它台。
func (s *service) deployFanout(ctx context.Context, servers []*target.Server, a run.Artifact, cfg map[string]string, hsp *healthSpec) []TargetResult {
	results := make([]TargetResult, len(servers))
	sem := make(chan struct{}, maxParallelDeploys)
	var wg sync.WaitGroup

	for i := range servers {
		wg.Add(1)
		go func(idx int, srv *target.Server) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// 单机 panic 兜底:绝不让一台崩溃带垮整次扇出(goroutine panic 会终止进程)。
			defer func() {
				if rec := recover(); rec != nil {
					finish := time.Now().UTC()
					results[idx] = TargetResult{
						ServerID:   srv.ID,
						ServerName: srv.Name,
						Status:     run.TargetFailed,
						Message:    "部署执行异常中断",
						StartedAt:  finish,
						FinishedAt: &finish,
					}
				}
			}()
			results[idx] = s.deployOne(ctx, srv, a, cfg, hsp)
		}(i, servers[i])
	}
	wg.Wait()
	return results
}

// deployOne 在一台目标机上构造并执行该产物类型的部署命令,返回该机结果。
//
// 健康门控:hsp 为待解析 spec —— 端口型探测的端口在部署期推导(容器 = 分配出的首个宿主端口,
// 在镜像路径内部解析;非容器 = regPort,此处静态解析)。执行错误**不上抛**:映射为 failed + 人读
// message(绝无明文密钥)。
//
// 网关联动(统一滚动,两条单机升级路径并存):
//   - image 产物:扩缩容轮转(deployInstanceRollingOne 内部:注册绑定命中 → 逐实例
//     「扩容起新(自动端口)→ 预热 → 原子切换 → 排空 → 缩容停旧」;未装配网关 / 反查不到 →
//     回退 pull→rm→run 硬切 + 回滚上一镜像)。
//   - 文件 / 命令部署:注册绑定反查到本机实例 → 「摘 → 部署 → 健康通过 → 挂回」;
//     部署失败保持摘除(故障机不回流量)。
func (s *service) deployOne(ctx context.Context, srv *target.Server, a run.Artifact, cfg map[string]string, hsp *healthSpec) TargetResult {
	// image 走扩缩容轮转(注册绑定)/ pull→停旧起新→健康→回滚上一镜像(非托管)。
	if a.Type == run.ArtifactImage {
		return s.deployInstanceRollingOne(ctx, srv, a, cfg, hsp)
	}

	// dist / jar / archive 走「发布目录 + current 软链原子切换 + 健康门控 + 失败回滚」零停机模式(Story 4.4);
	// 其余(理论上无)走扁平命令路径。两者都包一层网关摘/挂;非容器的探测端口 = regPort(静态)。
	regPort, _ := parsePortNumber(strings.TrimSpace(cfg["regPort"]))
	hc := hsp.resolve(regPort)
	if releaseModeArtifact(a) {
		return s.deployWithGatewayDetach(ctx, srv, cfg, func() TargetResult {
			return s.deployReleaseOne(ctx, srv, a, cfg, hc)
		})
	}
	return s.deployWithGatewayDetach(ctx, srv, cfg, func() TargetResult {
		return s.deployCommandsOne(ctx, srv, a, cfg, hc)
	})
}

// deployWithGatewayDetach 给单机部署包一层「预注册(detached)→ 摘 → 部署 → 挂回」
// (非容器实例的本机摘挂升级):
//   - 未装配网关 / 无注册绑定 → 直接执行 deployFn(行为不变);
//   - 配了 regPort 先经 EnsureInstanceDetached 预注册:实例行在部署前就存在(摘挂反查得到),
//     但以 detached 形态落库,未就绪不接流;部署成功后随摘除名单一起挂回;
//   - 反查到实例 → 逐实例摘除(失败仅记告警继续,摘除失败通常意味着网关不可达)→ 部署 →
//     成功则把**确实摘除成功**的实例挂回(一次重试;最终失败追加告警,状态保持 success 但 message 明示)→
//     失败/回滚则保持摘除并人读提示(摘除本身失败的实例照实说明,绝不谎称「已摘除」)。
//
// serviceRef:注册绑定的服务引用(regServiceId 优先;legacy 键 gatewayService = 服务名)。
// 本机在升级窗口内不接流量,集群可用性由其余机器实例保证(滚动批次编排不会同时摘光)。
func (s *service) deployWithGatewayDetach(ctx context.Context, srv *target.Server, cfg map[string]string, deployFn func() TargetResult) TargetResult {
	key := regServiceRef(cfg)
	if s.instanceGateway == nil || key == "" {
		return deployFn()
	}
	// 预注册(非容器,配了 regPort):实例行先以 detached 落库再进部署 —— 消除「先注册
	// attached 后部署」的未就绪接流窗口;失败仅告警(部署本体不受影响,注册表下轮收敛)。
	var ensured *InstanceRef
	if regPort, _ := parsePortNumber(strings.TrimSpace(cfg["regPort"])); regPort > 0 {
		if ref, eerr := s.instanceGateway.EnsureInstanceDetached(ctx, key, srv.ID, "", regPort, regPort); eerr != nil {
			cmdLogFrom(ctx)(cmdStreamStderr, "  ⚠ 注册实例失败("+srv.Name+"):"+humanExecError(eerr))
		} else {
			ensured = &ref
		}
	}
	instances, err := s.instanceGateway.ResolveInstances(ctx, srv.ID, key, "")
	if err != nil || (len(instances) == 0 && ensured == nil) {
		return deployFn()
	}

	// 摘除(尽力):摘除失败继续部署 —— 网关不可达时把部署也拦死只会更糟。
	// detached 只收**确实摘除成功**的实例:挂回侧据此恢复流量,失败侧据此给出与事实一致的文案。
	detachWarn := ""
	detached := make([]InstanceRef, 0, len(instances))
	for _, inst := range instances {
		if derr := s.instanceGateway.DetachInstance(ctx, inst.InstanceID); derr != nil {
			detachWarn += "从网关摘除实例 " + inst.Container + " 失败:" + humanExecError(derr) + ";"
			continue
		}
		detached = append(detached, inst)
	}

	res := deployFn()
	if res.Status == run.TargetSuccess {
		// 挂回名单 = 确实摘除的实例 + 本轮预注册的新实例(存量 attached 行已在摘除名单,
		// 按 InstanceID 去重防重复挂回)。
		toAttach := detached
		if ensured != nil && !instanceIDIn(detached, ensured.InstanceID) {
			toAttach = append(toAttach, *ensured)
		}
		attachWarn := ""
		for _, inst := range toAttach {
			if aerr := s.instanceGateway.AttachInstance(ctx, inst.InstanceID); aerr != nil {
				// 一次重试(挂回失败 = 机器已健康却不接流量,必须兜底再试)。
				if aerr2 := s.instanceGateway.AttachInstance(ctx, inst.InstanceID); aerr2 != nil {
					attachWarn += "部署成功但挂回网关实例 " + inst.Container + " 失败:" + humanExecError(aerr2) + ";"
				}
			}
		}
		if detachWarn != "" || attachWarn != "" {
			res.Message += "(网关联动告警:" + detachWarn + attachWarn + ")"
		}
		return res
	}

	// 部署失败 / 已回滚:已摘除的实例**保持摘除**(故障机不回流量);摘除失败的照实提示,
	// 避免运维误信「流量已隔离」而漏掉仍在接流量的实例。
	fact := ""
	if len(detached) > 0 {
		fact = strconv.Itoa(len(detached)) + " 个网关实例已保持摘除,流量不再进入;修复后重试部署或手动挂回"
	}
	if len(detached) < len(instances) {
		if fact != "" {
			fact += ";"
		}
		fact += "另有实例摘除失败,可能仍在接流量,请人工核对网关 upstream"
	}
	res.Message += "(网关联动:" + detachWarn + fact + ")"
	return res
}

// instanceIDIn 报告 refs 中是否已含 id(挂回去重:存量 attached 行可能同时出现在摘除与
// 预注册名单)。
func instanceIDIn(refs []InstanceRef, id string) bool {
	for _, r := range refs {
		if r.InstanceID == id {
			return true
		}
	}
	return false
}

// deployCommandsOne 旧扁平命令路径(非 release 类文件的兜底;现仅理论可达):逐条执行部署命令 +
// 健康门控。从 deployOne 拆出,使网关摘挂可包裹。
func (s *service) deployCommandsOne(ctx context.Context, srv *target.Server, a run.Artifact, cfg map[string]string, hc *HealthCheck) TargetResult {
	started := time.Now().UTC()
	res := TargetResult{
		ServerID:   srv.ID,
		ServerName: srv.Name,
		StartedAt:  started,
	}

	cmds, summary, berr := buildCommands(a, cfg)
	if berr != nil {
		finish := time.Now().UTC()
		res.Status = run.TargetFailed
		res.Message = berr.Error()
		res.FinishedAt = &finish
		return res
	}

	execCtx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	for _, cmd := range cmds {
		out, eerr := s.exec(execCtx, srv.ID, cmd)
		if eerr != nil {
			finish := time.Now().UTC()
			res.Status = run.TargetFailed
			res.Message = humanExecError(eerr)
			res.FinishedAt = &finish
			return res
		}
		if out != nil && out.ExitCode != 0 {
			finish := time.Now().UTC()
			res.Status = run.TargetFailed
			// 命令本身非零退出:回显 stderr 摘要(target 层执行的是平台构造的命令,
			// 不含凭据明文;仍截断防超大输出)。
			res.Message = fmt.Sprintf("部署命令退出码 %d:%s", out.ExitCode, truncate(strings.TrimSpace(out.Stderr)))
			res.FinishedAt = &finish
			return res
		}
	}

	// 部署命令全部成功 → 若配置了健康检查,做部署后健康门控(Story 4.3 / FR-12)。
	// 探测在部署命令成功之后跑;每机独立;经同一 target.Exec 链路(array 不拼 shell)。
	if hc.enabled() {
		if herr := s.runHealthCheck(execCtx, srv.ID, hc); herr != nil {
			finish := time.Now().UTC()
			res.Status = run.TargetFailed
			res.Message = herr.Error()
			res.FinishedAt = &finish
			return res
		}
		finish := time.Now().UTC()
		res.Status = run.TargetSuccess
		res.Message = summary + "(健康检查通过)"
		res.FinishedAt = &finish
		return res
	}

	finish := time.Now().UTC()
	res.Status = run.TargetSuccess
	res.Message = summary
	res.FinishedAt = &finish
	return res
}

// overallStatus 据每机结果聚合 run 终态:
//
//	全成功 → success;有成功有失败 → partial_failed;全失败 → failed。
func overallStatus(results []TargetResult) string {
	anySuccess, anyFailed := false, false
	for _, r := range results {
		switch r.Status {
		case run.TargetSuccess:
			anySuccess = true
		default:
			anyFailed = true
		}
	}
	switch {
	case anyFailed && anySuccess:
		return run.StatusPartialFailed
	case anyFailed:
		return run.StatusFailed
	default:
		return run.StatusSuccess
	}
}

// overallStatusOfTargets 据**全量持久化目标**聚合 run 终态(retry 后重算用)。
// 与 overallStatus 同语义,但作用于 run.DeployTarget(rolled_back 计为失败侧:有成功有失败 →
// partial_failed;全失败/全回滚 → failed;全成功 → success)。
func overallStatusOfTargets(targets []run.DeployTarget) string {
	anySuccess, anyFailed := false, false
	for i := range targets {
		switch targets[i].Status {
		case run.TargetSuccess:
			anySuccess = true
		default:
			anyFailed = true
		}
	}
	switch {
	case anyFailed && anySuccess:
		return run.StatusPartialFailed
	case anyFailed:
		return run.StatusFailed
	default:
		return run.StatusSuccess
	}
}

// truncate 截断字符串到 truncateLen(防超大输出)。
func truncate(s string) string {
	if len(s) <= truncateLen {
		return s
	}
	return s[:truncateLen] + "…(已截断)"
}

// humanExecError 把 target 层执行错误映射为人读文案(绝不含凭据明文 / 内部栈)。
// target.Exec 已把连接 / 认证类错误映射为领域错误,本层进一步人读化。
func humanExecError(err error) string {
	switch {
	case errors.Is(err, target.ErrAuth):
		return "SSH 认证失败:密钥或口令无效,或无登录权限"
	case errors.Is(err, target.ErrUnreachable):
		return "无法连接服务器:端口未开放、主机不可达或超时"
	case errors.Is(err, target.ErrVaultUnconfigured):
		return "保险库未配置 master key,无法取 SSH 凭据"
	case errors.Is(err, target.ErrCredentialNotFound):
		return "引用的 SSH 凭据不存在"
	case errors.Is(err, context.DeadlineExceeded):
		return "部署执行超时"
	default:
		// 兜底:不泄漏内部细节(target 层错误体已无凭据明文)。
		return "部署执行失败"
	}
}
