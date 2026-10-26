package build

// remote_stage_exec.go 把「按选择器从构建机池派发到远程机器执行」接到 DAG 阶段执行器
// (FR-8-14 续 / FR-8-19 池化):stage 可用 runner 字段声明选择器覆盖项目默认;两者都空 → 本地执行。
//
// 远程执行模型(**token 全程只在控制机,绝不上远程**——比"远程克隆"更安全,且远程无需装 git):
//  1. 调度器选机并占槽(标签匹配 → 优先级 → 流水线亲和 → 负载;全忙 FIFO 排队;见 runner.Scheduler)。
//  2. 控制机本地克隆源码(复用 b.cloner;若装了 repocache 则走本地镜像增量,快),并把上游阶段
//     已归档的产物字节恢复进工作区(跨阶段产物传递;产物在控制机制品库,远程无从自取)。
//  3. 把本地工作区打成 tar.gz,经 SSH(target.Upload = stdin 流,无 argv 长度限)传到远程临时目录并解包。
//  4. 在远程机用容器跑 script job(NewRemoteDriver:shellDriver 经 SSH Commander → 远程容器 CLI 运行,
//     nerdctl/docker/podman 按机探测),日志经 reporterSink 回流;槽位由闭包级 defer 必归还(panic 不泄漏)。
//  5. 收尾删远程工作区(尽力)。
//
// 安全/语义沿用:命令 array 化(不拼 shell);secret 经 -e 注入容器(与本地同,不更差);失败映射 ErrBuildFailed。
// env 注入顺序与本地 runScriptJobIsolated 完全对齐:运行参数 → 流水线级变量(settings.Build.Vars,
// 含 secret,vault 即取即用)→ job env → PIPEWRIGHT_ENV(上游 job 输出 env 在远程独立工作区无来源,不传)。
// 测试报告/质量门禁:script 节点声明 testReport=junit 时,报告文件经 SSH cat 回采控制机,解析/门禁/
// 持久化与本地同一套(门禁不过 → 阶段失败,阻断下游)。边界(后续增量):集群共享构建机(peer:选择器)
// 的报告仍在对端环境,不回采;远程模式不套 timeout/retry。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/huangchengsir/pipewright/internal/dag"
	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// RunnerResolver 把「选择器」变成「占住一台构建机」(FR-8-19):
// SelectorFor 取项目默认选择器(空/false = 未配);Acquire 按选择器选机并占槽,release 归还,
// 并返回机器的人读显示名(供派发日志明确「调度到哪台机」,而不是甩一个不可读 uuid)。
// main.go 用 runner.Service + runner.Scheduler 的组合适配(本包不直依赖 runner,结构化满足)。
type RunnerResolver interface {
	SelectorFor(ctx context.Context, projectID string) (selector string, ok bool)
	Acquire(ctx context.Context, pipelineID, selector string, log func(string)) (serverID, serverName string, release func(), err error)
}

// remoteExec 抽象远程执行所需的 target 能力(Exec + Upload;target.Service 即满足;便于 fake 单测)。
type remoteExec interface {
	RemoteExecer // Exec(ctx, serverID, cmd) (*target.ExecResult, error)
	Upload(ctx context.Context, serverID string, content io.Reader, remotePath string) error
}

// NewStageExecutorWithRunner 返回「按 job/阶段/项目选择器派发本地/远程池」的阶段执行器。
// resolve 或 tgt 为 nil → 退化为纯本地(NewStageExecutor)。
// 选择器优先级:**job.Config["runner"](节点级覆盖)> stage.Runner(阶段覆盖)> 项目默认**;
// 都空 = 本地。
//
// 产品语义:**节点(job)是工作单元,选择器是取机亲和约束,不是"共享某台机器"的指令**。
// 每个有选择器的节点独立占一个调度槽:同一选择器的多个节点从同一池子里各取一台
// (池内有空位即并行,满则按调度器 FIFO 排队,负载计数如实),各自独立克隆与远程
// 工作区 —— 与本地执行器「并行 job 各自独立工作区」语义一致,互不踩文件。
// 无选择器的节点与非 script job(deploy/notify/build_image 等,只能由本地执行器跑)
// 归入本地侧完整执行,不丢节点。
//
// 混合阶段执行策略:
//   - 无跨组依赖(默认):本地侧批量先跑,远程节点随后逐节点派发;节点级 JobRunning/JobDone
//     如实上报(成功/失败),因本地侧失败/取消而未执行的远程节点显式标 skipped(与 DAG 路径同口径)。
//   - 有 needs 边触及远程节点(本地 needs 远程 / 远程 needs 任意):批量路径的「本地先行 +
//     剔除指向远程的 needs」会使依赖失效(下游先跑),改走 runStageMixedTopo——按 needs 拓扑序
//     逐节点执行(本地节点复用 DAG 路径单 job 原语,远程节点 Acquire + runStageRemote + defer release)。
//   - post(阶段后置步骤)一律提升到本层:在本地+远程全部节点结束后,按**合并成败**在控制机
//     新工作区统一执行(runLiftedStagePost);全远程阶段本地侧为空,post 不再丢失。
func NewStageExecutorWithRunner(b *Builder, reportSink TestReportSink, resolve RunnerResolver, tgt remoteExec) dagrun.StageExecutor {
	local := NewStageExecutor(b, reportSink)
	if resolve == nil || tgt == nil {
		return local
	}
	return func(ctx context.Context, r *run.Run, stage pipeline.Stage, rep dagrun.StageReporter) error {
		log := func(msg string) { _ = rep.Log(ctx, streamStdout, msg) }
		// 流水线即代码变量:required 拦截 + ${NAME} 占位替换(远程/集群共享路径在此统一生效,
		// stage 的 config map 为共享引用,后续 local 回退再跑一次亦幂等)。
		if miss := b.applyStagePacVars(ctx, r, &stage); len(miss) > 0 {
			_ = rep.Log(ctx, streamError, "流水线变量缺失:"+strings.Join(miss, ", ")+"(required: true,未在「变量与缓存」填写)")
			return ErrBuildFailed
		}
		projSel := ""
		if s, ok := resolve.SelectorFor(ctx, r.ProjectID); ok {
			projSel = strings.TrimSpace(s)
		}
		stageSel := strings.TrimSpace(stage.Runner)

		rjobs := runnerJobsOf(stage)
		if len(rjobs) == 0 {
			// 无可远程构建节点的阶段(纯部署/健康检查/通知等)只能由本地执行器真实执行:
			// 远程 runner 仅支持 script / build_image 类型,派过去只会逐节点「放行」空转——
			// 部署节点的目标选择器完全不生效(部署由控制机经 SSH 驱动目标机,与构建机池无关),
			// 阶段还白白占一个构建机槽位。一律本地,不看阶段/项目构建机选择器(那些只约束构建节点)。
			return local(ctx, r, stage, rep)
		}

		// 逐节点判定有效选择器;全部为空 → 整体本地(行为不变,零额外开销)。
		effs := make([]string, len(rjobs))
		hasRemote := false
		for i, jb := range rjobs {
			effs[i] = effRunnerSelector(jb, stageSel, projSel)
			if effs[i] != "" {
				hasRemote = true
			}
		}
		if !hasRemote {
			return local(ctx, r, stage, rep)
		}

		// 节点有效选择器(jobID → 非空选择器 = 远程节点;仅 script / build_image 节点可远程)。
		selByID := make(map[string]string, len(rjobs))
		for i, jb := range rjobs {
			if effs[i] != "" && strings.TrimSpace(jb.ID) != "" {
				selByID[jb.ID] = effs[i]
			}
		}

		// 跨组依赖:任一 needs 边触及远程节点 → 批量路径会使依赖失效,改走拓扑序逐节点路径。
		// 全部节点带 ID 才能建图(生产保存期保证;无 ID 的异常数据退回批量路径,与本地执行器
		// DAG 回退口径一致)。
		if allJobsHaveIDs(stage.Jobs) && stageHasRemoteNeeds(stage.Jobs, selByID) {
			return b.runStageMixedTopo(ctx, r, stage, rep, reportSink, resolve, tgt, selByID, log)
		}

		// ── 批量路径(无跨组依赖,行为兼容):本地侧先跑,远程节点随后逐节点独立取机 ──
		// 本地侧:无选择器的构建节点(script / build_image)+ 全部其余节点(部署/通知等只能由本地执行器执行)。
		// post 剥离提升到本层:post 必须在本地+远程全部结束后按合并成败执行(全远程阶段本地侧
		// 为空,post 留在本地执行器里永远不会触发)。
		var localJobs []pipeline.Job
		for _, jb := range stage.Jobs {
			if isScriptJob(jb.Type) || isBuildImageJob(jb.Type) {
				if effs[indexOfRunnerJob(rjobs, jb)] != "" {
					continue
				}
			}
			localJobs = append(localJobs, jb)
		}
		var jobErr error
		if len(localJobs) > 0 {
			sub := localSubsetStage(stage, localJobs)
			sub.Post = nil
			if err := local(ctx, r, sub, rep); err != nil {
				jobErr = err
			}
		}

		// dispatch 派发单个远程节点:占槽(失败返回 started=false)→ JobRunning → 远程执行 →
		// 按结果 JobDone。release 由闭包级 defer 保证:即使执行 panic(worker 层有 recover,
		// 见 internal/run/pool.go)槽位也必归还,不再泄漏。
		// peer: 选择器(集群共享构建,0086)不占本端槽 —— 排队权威在对端环境的调度器,
		// 经 mesh 提交跟踪,产物回传后由本端回收(见 runJobShared);节点生命周期如实上报
		// (含对端解析失败等配置错误也显式 failed,不静默跳过)。
		dispatch := func(i int, jb pipeline.Job, sel string) (started bool, err error) {
			if peerScopedSelector(sel) {
				_ = rep.JobRunning(ctx, jb.ID)
				jrep := rep.JobReporter(jb.ID)
				if rerr := b.runJobShared(ctx, r, stage, jb, jrep, sel); rerr != nil {
					_ = rep.JobDone(ctx, jb.ID, run.StepFailed)
					return true, rerr
				}
				_ = rep.JobDone(ctx, jb.ID, run.StepSuccess)
				return true, nil
			}
			serverID, serverName, release, aerr := resolve.Acquire(ctx, r.ProjectID, sel, log)
			if aerr != nil {
				return false, acquireFail(ctx, rep, sel, aerr)
			}
			defer release()
			_ = rep.JobRunning(ctx, jb.ID)
			// 派发与执行日志都归到该节点自己的 step(而非阶段首节点):点开单节点过滤日志时,
			// 能直接看到它被调度到哪台机、传输/容器执行/完成的全过程。
			jrep := rep.JobReporter(jb.ID)
			_ = jrep.Log(run.WithLogMachine(ctx, serverName), streamStdout, fmt.Sprintf("→ 节点「%s」→ 构建机:%s(选择器 %s)", jb.Name, serverName, sel))
			if rerr := b.runJobRemote(ctx, r, stage, jb, jrep, reportSink, serverID, serverName, tgt, "-"+sanitizeRemoteSeg(jobRemoteKey(jb, i))); rerr != nil {
				_ = rep.JobDone(ctx, jb.ID, run.StepFailed)
				return true, rerr
			}
			_ = rep.JobDone(ctx, jb.ID, run.StepSuccess)
			return true, nil
		}

		// 远程节点:逐节点独立取机(同选择器=同池各占一槽,可并行/排队),各自独立工作区。
		// 本地侧已失败/取消时不再派发,未执行的远程节点统一在收尾标 skipped。
		executed := make(map[int]bool, len(rjobs))
		if jobErr == nil {
			for i, jb := range rjobs {
				if canceled(ctx) {
					jobErr = run.ErrCanceled
					break
				}
				sel := effs[i]
				if sel == "" {
					continue
				}
				started, err := dispatch(i, jb, sel)
				if started {
					executed[i] = true
				}
				if err != nil {
					jobErr = err
					break
				}
			}
		}
		// 因本地侧失败/取消/上游远程节点失败而从未执行的远程节点:显式标 skipped
		// (与 DAG 路径 dag_stage_exec.go 的 skipped 口径一致),避免执行中 UI 一直 pending、
		// 早退时被 finishRemaining 兜底成 failed。
		if jobErr != nil {
			for i, jb := range rjobs {
				if effs[i] != "" && !executed[i] {
					_ = rep.JobDone(ctx, jb.ID, run.StepSkipped)
				}
			}
		}

		// post:本地+远程全部节点结束后,按合并成败在控制机工作区统一执行。
		b.runLiftedStagePost(ctx, r, stage, rep, jobErr != nil)
		return jobErr
	}
}

// stageHasRemoteNeeds 报告阶段内是否存在「触及远程节点」的 needs 边:本节点是远程节点且声明了
// needs(远程 needs 本地/远程),或本节点的任一上游是远程节点(本地 needs 远程)。存在时批量
// 派发路径(本地侧先行 + 剔除指向远程的 needs)会使依赖顺序失效,调用方应改走拓扑序逐节点路径。
func stageHasRemoteNeeds(jobs []pipeline.Job, selByID map[string]string) bool {
	for _, jb := range jobs {
		if len(jb.Needs) == 0 {
			continue
		}
		if selByID[jb.ID] != "" {
			return true // 远程节点 needs 任何节点
		}
		for _, n := range jb.Needs {
			if selByID[n] != "" {
				return true // 本地节点 needs 远程节点
			}
		}
	}
	return false
}

// runnerJobsOf 返回阶段内可远程派发的构建节点(script 类 + build_image;保序)。
func runnerJobsOf(stage pipeline.Stage) []pipeline.Job {
	out := make([]pipeline.Job, 0, len(stage.Jobs))
	for _, jb := range stage.Jobs {
		if isScriptJob(jb.Type) || isBuildImageJob(jb.Type) {
			out = append(out, jb)
		}
	}
	return out
}

// indexOfRunnerJob 返回 jb 在 runnerJobs 中的下标(-1 = 不在;stage.Jobs 与 runnerJobs 同序,故 O(n))。
func indexOfRunnerJob(runnerJobs []pipeline.Job, jb pipeline.Job) int {
	for i := range runnerJobs {
		if runnerJobs[i].ID == jb.ID && runnerJobs[i].Name == jb.Name {
			return i
		}
	}
	return -1
}

// jobRemoteKey 远程工作区后缀键:优先 job ID(规范化后唯一),无 ID 用序号兜底。
func jobRemoteKey(jb pipeline.Job, i int) string {
	if id := strings.TrimSpace(jb.ID); id != "" {
		return id
	}
	return fmt.Sprintf("job%d", i+1)
}

// acquireFail 统一处理取机失败:取消/排队等槽超时(上游 ctx deadline)都是「运行被中止」语义,
// 统一归 ErrCanceled 不落成「构建失败」误报;其余 = 构建失败。流水线与本产品项目 1:1,
// Acquire 的 pipelineID 传 ProjectID 即流水线亲和。
func acquireFail(ctx context.Context, rep dagrun.StageReporter, sel string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
		return run.ErrCanceled
	}
	_ = rep.Log(ctx, streamError, "构建机池调度失败("+sel+"):"+err.Error())
	return ErrBuildFailed
}

// effRunnerSelector 计算节点的有效构建机选择器:job.Config["runner"] > stage.Runner > projSel;
// 三者皆空 = ""(本地)。
func effRunnerSelector(jb pipeline.Job, stageSel, projSel string) string {
	if v, ok := jobRunnerSelector(jb); ok {
		return v
	}
	if stageSel != "" {
		return stageSel
	}
	return projSel
}

// jobRunnerSelector 取节点级构建机选择器(FR-8-19:job.Config["runner"],语法与阶段级一致)。
// 非空字符串 = 覆盖;缺失/空/非字符串 = 未覆盖(("", false))。
func jobRunnerSelector(jb pipeline.Job) (string, bool) {
	raw, ok := jb.Config["runner"]
	if !ok {
		return "", false
	}
	s, ok := raw.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

// localSubsetStage 返回 stage 副本:jobs = 本地侧节点(无选择器 script + 全部非 script),
// 并把指向「不在本副本内节点」的 needs 剔除(上游被派去远程时,本地侧 DAG 不悬空;
// 上游失败会先于本地侧/以远程组错误终止阶段,此处只保建图安全)。
func localSubsetStage(stage pipeline.Stage, localJobs []pipeline.Job) pipeline.Stage {
	inCopy := make(map[string]bool, len(localJobs))
	for _, jb := range localJobs {
		if id := strings.TrimSpace(jb.ID); id != "" {
			inCopy[id] = true
		}
	}
	jobs := make([]pipeline.Job, len(localJobs))
	for i, jb := range localJobs {
		if len(jb.Needs) > 0 {
			needs := make([]string, 0, len(jb.Needs))
			for _, n := range jb.Needs {
				if inCopy[n] {
					needs = append(needs, n)
				}
			}
			jb.Needs = needs
		}
		jobs[i] = jb
	}
	out := stage
	out.Jobs = jobs
	return out
}

// runStageMixedTopo 按 needs 拓扑序逐节点执行「本地/远程混合且有跨组依赖」的阶段(见
// NewStageExecutorWithRunner 的策略说明)。复用与 DAG 路径相同的单 job 原语:script 无选择器 →
// runScriptJobIsolated;script 有选择器 → Acquire + runStageRemote(单子阶段)+ defer release;
// build_image/deploy/health_check/notify 等与 runStageJobsDAG 的 runJob 同口径。任一 needs 上游
// 非 success → 本节点 skipped(与 DAG 路径 skipped 口径一致);无依赖关系的分支在某节点失败后
// 仍继续跑(同 DAG 语义);取消后剩余节点全部 skipped。post 在全部节点结束后按合并成败统一执行。
//
// 诚实边界:远程节点的步骤输出 env 无回采通道(工作区在远程,runStageRemote 不捕获),其下游
// 注入的上游 env 仅来自本地节点;远程节点间 env 不传(与批量路径一致)。
func (b *Builder) runStageMixedTopo(
	ctx context.Context,
	r *run.Run,
	stage pipeline.Stage,
	rep dagrun.StageReporter,
	reportSink TestReportSink,
	resolve RunnerResolver,
	tgt remoteExec,
	selByID map[string]string,
	log func(string),
) error {
	// 建图校验(保存期已校验;此处为数据异常防御,与 runStageJobsDAG 同口径)。
	nodes := make([]dag.Node, 0, len(stage.Jobs))
	for _, jb := range stage.Jobs {
		nodes = append(nodes, dag.Node{ID: jb.ID, Needs: jb.Needs})
	}
	if _, gerr := dag.New(nodes); gerr != nil {
		_ = rep.Log(ctx, streamError, "阶段内 job 依赖图非法:"+gerr.Error())
		return ErrBuildFailed
	}

	proj, settings, perr := b.resolve(ctx, r)
	if perr != nil {
		_ = rep.Log(ctx, streamError, "无法加载项目构建配置:"+perr.Error())
		return ErrBuildFailed
	}

	// 旁挂服务(与本地执行器同语义;仅本地 script 节点入网,远程节点不支持容器网络)。
	var svcNetwork string
	if len(stage.Services) > 0 {
		net, ok := b.startStageServices(ctx, r, stage, rep)
		if !ok {
			return ErrBuildFailed
		}
		svcNetwork = net
		defer b.stopStageServices(context.WithoutCancel(ctx), r, stage, net, rep)
	}

	// 推送镜像节点声明:与本地执行器同语义(见 push_image.go)——被 push_image 引用的 build_image
	// 跳过「按环境 registry 自动推送」,改按该节点声明的目标推送(推送仍在 build_image 内完成)。
	pushPlans := b.pushPlansByBuildNode(ctx, stage.Jobs, settings, r)

	status := make(map[string]string, len(stage.Jobs))                 // jobID → run.StepXxx 终态
	jobEnvOut := make(map[string][]pipeline.BuildVar, len(stage.Jobs)) // jobID → 该 job 产出的 env(本地节点)

	// dispatchRemote 拓扑路径的远程节点派发:占槽 → 远程执行 → 闭包级 defer 必还槽(panic 不泄漏)。
	// script / build_image 按 runJobRemote 分发;wsSuffix 用 job ID(拓扑路径节点必带 ID)。
	// peer: 选择器(集群共享构建,0086)不占本端槽,经 mesh 派发到对端共享机(见 runJobShared)。
	dispatchRemote := func(jb pipeline.Job, sel string, jrep dagrun.StageReporter) error {
		if peerScopedSelector(sel) {
			return b.runJobShared(ctx, r, stage, jb, jrep, sel)
		}
		serverID, serverName, release, aerr := resolve.Acquire(ctx, r.ProjectID, sel, log)
		if aerr != nil {
			return acquireFail(ctx, rep, sel, aerr)
		}
		defer release()
		_ = jrep.Log(run.WithLogMachine(ctx, serverName), streamStdout, fmt.Sprintf("→ 节点「%s」→ 构建机:%s(选择器 %s)", jb.Name, serverName, sel))
		return b.runJobRemote(ctx, r, stage, jb, jrep, reportSink, serverID, serverName, tgt, "-"+sanitizeRemoteSeg(jb.ID))
	}

	// Kahn 拓扑序逐节点执行(同层按声明序)。顺序确定性 > 并发度:跨组依赖场景重在依赖正确。
	done := make(map[string]bool, len(stage.Jobs))
	var jobErr error
	for len(done) < len(stage.Jobs) {
		progressed := false
		for _, jb := range stage.Jobs {
			if done[jb.ID] {
				continue
			}
			ready := true
			for _, n := range jb.Needs {
				if !done[n] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			done[jb.ID] = true
			progressed = true

			// 跳过口径(与 DAG 路径一致):任一 needs 上游非 success,或运行已取消 → skipped。
			skip := false
			if canceled(ctx) {
				skip = true
				if jobErr == nil {
					jobErr = run.ErrCanceled
				}
			}
			for _, n := range jb.Needs {
				if status[n] != run.StepSuccess {
					skip = true
					break
				}
			}
			if skip {
				status[jb.ID] = run.StepSkipped
				_ = rep.JobDone(ctx, jb.ID, run.StepSkipped)
				continue
			}

			st, err := b.runMixedNode(ctx, r, stage, jb, rep, reportSink, proj, settings, svcNetwork, pushPlans, jobEnvOut, dispatchRemote, selByID)
			status[jb.ID] = st
			if err != nil && jobErr == nil {
				jobErr = err
			}
		}
		if !progressed {
			break // 防御:环已被 dag.New 拦截,正常到不了这里
		}
	}

	// post:全部节点(本地+远程)结束后按合并成败在控制机工作区执行。
	b.runLiftedStagePost(ctx, r, stage, rep, jobErr != nil)
	return jobErr
}

// runMixedNode 执行混合拓扑路径中的单个节点,上报 JobRunning/JobDone(成功/失败)并返回终态。
// 节点类型 → 原语映射与 runStageJobsDAG 的 runJob 保持一致。
func (b *Builder) runMixedNode(
	ctx context.Context,
	r *run.Run,
	stage pipeline.Stage,
	jb pipeline.Job,
	rep dagrun.StageReporter,
	reportSink TestReportSink,
	proj *project.Project,
	settings *pipeline.Settings,
	svcNetwork string,
	pushPlans map[string]imagePushPlan,
	jobEnvOut map[string][]pipeline.BuildVar,
	dispatchRemote func(jb pipeline.Job, sel string, jrep dagrun.StageReporter) error,
	selByID map[string]string,
) (string, error) {
	// 收集上游(needs)产出的 env,按 needs 声明序合并(仅本地 script 节点有回采;远程节点无输出)。
	var upstreamEnv []pipeline.BuildVar
	for _, dep := range jb.Needs {
		upstreamEnv = append(upstreamEnv, jobEnvOut[dep]...)
	}

	_ = rep.JobRunning(ctx, jb.ID)
	jrep := rep.JobReporter(jb.ID)
	jsink := &reporterSink{rep: jrep}

	var err error
	switch {
	case isScriptJob(jb.Type):
		if sel := selByID[jb.ID]; sel != "" {
			err = dispatchRemote(jb, sel, jrep)
		} else {
			var out []pipeline.BuildVar
			out, err = b.runScriptJobIsolated(ctx, jsink, jrep, r, jb, stage, proj, settings, svcNetwork, upstreamEnv, reportSink)
			if len(out) > 0 {
				jobEnvOut[jb.ID] = out
			}
		}
	case isBuildImageJob(jb.Type):
		if sel := selByID[jb.ID]; sel != "" {
			err = dispatchRemote(jb, sel, jrep)
		} else {
			err = b.runBuildImageJobIsolated(ctx, jsink, jrep, r, jb, stage, proj, settings, pushPlans[jb.Name])
		}
	case isDeployJob(jb.Type):
		err = b.runDeployJob(ctx, jrep, jb, r.ID, r.Trigger.Params)
	case strings.TrimSpace(jb.Type) == "health_check":
		err = b.runHealthCheckJob(ctx, jrep, jb, r.Trigger.Params)
	case isPushImageJob(jb.Type):
		err = b.runPushImageJob(ctx, jrep, jb, r, settings)
	case strings.TrimSpace(jb.Type) == "notify":
		b.runNotifyJob(ctx, jrep, jb, r)
	case isObjectUploadJob(jb.Type):
		err = b.runOssUploadJob(ctx, jrep, jb, r)
	case isAITestJob(jb.Type):
		// AI 测试恒在控制面执行(远程构建机场景:需求按 run revision 克隆到临时目录读取)。
		err = b.runAITestJobWithClone(ctx, jrep, jb, r, stage, reportSink)
	case isWaitJob(jb.Type):
		// 等待永远在控制机本地执行(不进构建机池);被手动跳过 → 哨兵(下方转 skipped、非失败)。
		var skipped bool
		skipped, err = b.runWaitJob(ctx, rep, jrep, jb, r.ID, stage.Name)
		if err == nil && skipped {
			err = errWaitSkipped
		}
	default:
		_ = jrep.Log(ctx, streamStdout, fmt.Sprintf("· %s(%s)— 真实执行未接入;本节点放行", jb.Name, jb.Type))
	}

	status := run.StepSuccess
	if err != nil {
		status = run.StepFailed
		if errors.Is(err, errWaitSkipped) {
			// 等待被手动跳过:step 如实落 skipped,但不算失败——下游节点照常放行。
			status = run.StepSkipped
			err = nil
		}
	}
	_ = rep.JobDone(ctx, jb.ID, status)
	return status, err
}

// runLiftedStagePost 为「含远程节点的阶段」执行后置步骤(post 提升到远程派发层):post 必须在
// 本地+远程全部节点结束后按合并成败执行,而本地执行器只会在本地侧结束时触发 post(全远程阶段
// 甚至完全不触发),故统一提到本层。post 在控制机新克隆的工作区里跑(恢复上游归档产物);
// best-effort:post 失败/工作区不可用只记日志,绝不改阶段结果(与本地执行器 post 同语义)。
func (b *Builder) runLiftedStagePost(ctx context.Context, r *run.Run, stage pipeline.Stage, rep dagrun.StageReporter, stageFailed bool) {
	if len(stage.Post) == 0 {
		return
	}
	// WithoutCancel:阶段被取消/失败后清理步骤仍能跑(与本地执行器 post 同语义)。
	pctx := context.WithoutCancel(ctx)
	proj, settings, perr := b.resolve(pctx, r)
	if perr != nil {
		_ = rep.Log(pctx, streamError, "阶段后置步骤:无法加载项目构建配置,跳过:"+perr.Error())
		return
	}
	ws, _, cleanup, err := b.cloneJobWorkspace(pctx, r, proj, rep)
	if err != nil {
		_ = rep.Log(pctx, streamError, "阶段后置步骤:工作区不可用,跳过")
		return
	}
	defer cleanup()
	b.runStagePost(pctx, &reporterSink{rep: rep}, r, stage, ws, stageFailed, rep, settings)
}

// runStageRemote 在远程 runner 上执行本阶段的 script job(见文件头模型)。
// runJobRemote 在远程构建机上执行单个节点(按类型分发):script 类 → runStageRemote(容器跑
// 命令);build_image → runBuildImageJobRemote(远程 docker build + 推送)。两类之外不应被
// 派发到远程(见 runnerJobsOf 的筛选),此处兜底拒绝而非静默放行。
// reportSink 供 script 节点回采测试报告(报告文件经 SSH cat 读回控制机解析,门禁同本地裁决)。
func (b *Builder) runJobRemote(ctx context.Context, r *run.Run, stage pipeline.Stage, jb pipeline.Job, jrep dagrun.StageReporter, reportSink TestReportSink, serverID, serverName string, tgt remoteExec, wsSuffix string) error {
	switch {
	case isScriptJob(jb.Type):
		sub := stage
		sub.Jobs = []pipeline.Job{jb}
		return b.runStageRemote(ctx, r, sub, jrep, reportSink, serverID, serverName, tgt, wsSuffix)
	case isBuildImageJob(jb.Type):
		return b.runBuildImageJobRemote(ctx, r, stage, jb, jrep, serverID, serverName, tgt, wsSuffix)
	default:
		_ = jrep.Log(ctx, streamError, fmt.Sprintf("节点「%s」(%s)不支持远程构建机执行", jb.Name, jb.Type))
		return ErrBuildFailed
	}
}

// runBuildImageJobRemote 在远程构建机上执行一个 build_image 节点:控制机克隆(含恢复上游
// 归档产物)→ tar 经 SSH 上传解包 → 远程 docker build → 按 push 计划(推送镜像节点声明 /
// 环境绑定 registry 兜底)在远程 Tag/Login/Push。镜像留在远程并推到仓库,部署节点按产物
// 引用从仓库 pull,与「构建在哪台机发生」解耦。
// 边界(诚实失败,不静默):artifactType 非 image(jar/dist)的产物文件会留在远程机、控制机
// 制品库收不到 → 明确报错;缓存与测试报告同远程 script 路径口径(build_image 不产测试报告)。
func (b *Builder) runBuildImageJobRemote(ctx context.Context, r *run.Run, stage pipeline.Stage, jb pipeline.Job, rep dagrun.StageReporter, serverID, serverName string, tgt remoteExec, wsSuffix string) error {
	lctx := run.WithLogMachine(ctx, serverName)

	// 产物类型前置检查:image(默认)全链路支持;jar/dist 远程不可回收 → 立即诚实失败。
	if at := strings.TrimSpace(cfgString(jb.Config, "artifactType")); at != "" && at != pipeline.ArtifactImage {
		_ = rep.Log(lctx, streamError, fmt.Sprintf("构建节点「%s」:远程构建机暂仅支持镜像(image)产物;jar/dist 产物请配置为本地执行", jb.Name))
		return ErrBuildFailed
	}

	proj, settings, perr := b.resolve(ctx, r)
	if perr != nil {
		_ = rep.Log(lctx, streamError, "无法加载项目构建配置:"+perr.Error())
		return ErrBuildFailed
	}

	// 1) 控制机本地克隆 + 恢复上游归档产物(与 runStageRemote 同骨架;token 只在控制机)。
	workspace, commitTag, cleanup, err := b.cloneJobWorkspace(ctx, r, proj, rep)
	if err != nil {
		return err
	}
	defer cleanup()

	// 2) 打包 → SSH 上传 → 远程解包(远程工作区按节点 ID 区分,同机多节点互不碰撞)。
	remoteWS := "/tmp/pipewright-remote/" + sanitizeRemoteSeg(r.ID) + "-" + sanitizeRemoteSeg(stage.ID) + wsSuffix
	remoteTar := remoteWS + ".tar.gz"
	// 远程清理必须在 upload 之前注册:传输中断只留半个 tar、解包失败在解包命令的 rm -f 之前退出,
	// 两条路径都会绕过"解包成功后才挂 defer"的写法;列表带上 tar 包,rm 对不存在路径幂等。
	defer func() {
		rmCmd := []string{"rm", "-rf", remoteWS, remoteTar}
		_ = rep.Log(lctx, streamStdout, "$ "+strings.Join(rmCmd, " "))
		_, _ = tgt.Exec(context.WithoutCancel(ctx), serverID, rmCmd)
	}()
	_ = rep.Log(lctx, streamStdout, "→ 远程构建机:打包工作区并经 SSH 传输…")
	if err := uploadWorkspace(ctx, tgt, serverID, workspace, remoteTar); err != nil {
		_ = rep.Log(lctx, streamError, "传输工作区到远程失败:"+err.Error())
		return ErrBuildFailed
	}
	unpackCmd := []string{"sh", "-c", `mkdir -p "$0" && tar -xzf "$1" -C "$0" && rm -f "$1"`, remoteWS, remoteTar}
	_ = rep.Log(lctx, streamStdout, "$ "+strings.Join(unpackCmd, " "))
	if out, eerr := tgt.Exec(ctx, serverID, unpackCmd); eerr != nil || (out != nil && out.ExitCode != 0) {
		_ = rep.Log(lctx, streamError, "远程解包工作区失败")
		return ErrBuildFailed
	}

	// 3) 远程执行 docker build / tag / login / push:Builder 浅拷贝换 remote driver ——
	//    runBuildImageJob 的 build/push 全走 b.driver,换成 SSH driver 后真实构建发生在
	//    远程机(工作区用远程路径);vault/项目/推送计划等仍在控制机解析。
	bin := DetectRemoteCLIWithLog(ctx, tgt, serverID, func(line string) { _ = rep.Log(lctx, streamStdout, line) })
	_ = rep.Log(lctx, streamStdout, "远程容器 CLI:"+bin)
	rb := *b
	rb.driver = NewRemoteDriver(tgt, serverID, bin)

	pushPlans := b.pushPlansByBuildNode(ctx, stage.Jobs, settings, r)
	sink := &reporterSink{rep: rep}
	berr := rb.runBuildImageJob(ctx, sink, rep, jb, stage.Name, proj, settings, remoteWS, commitTag, pushPlans[jb.Name])
	// 构建后清远程机悬空镜像(与本地路径同口径):rb.driver 是 SSH 驱动(shellDriver),prune 经 SSH
	// 落到构建机自己的 daemon;成败与否都尽力,WithoutCancel 保证取消后仍执行。
	rb.pruneImagesBestEffort(context.WithoutCancel(ctx), func(stream, line string) { _ = rep.Log(lctx, stream, line) })
	return berr
}

// serverName 是机器人读显示名(完成日志用);wsSuffix 是远程工作区路径后缀(节点级独立取机时
// 用 job id 区分同阶段各节点的路径,防同机碰撞)。
// 与本地路径对齐的语义:克隆后先在控制机工作区恢复上游阶段归档产物(随 tar 一起上传,远程无
// 制品库来源);env 注入顺序见统一口径(builder.go buildArgs 注释):运行参数 → 流水线级变量
// (settings.Build.Vars,含 secret)→ job 自身 env → PIPEWRIGHT_ENV(上游 job 输出 env 在远程
// 独立工作区无来源,省)。
// reportSink 非空时,script job 声明的测试报告/质量门禁在节点执行后回采:报告文件经 SSH cat
// 读回控制机,解析/裁决/持久化与本地路径完全同一套(门禁不过 → ErrQualityGate 阻断下游)。
func (b *Builder) runStageRemote(ctx context.Context, r *run.Run, stage pipeline.Stage, rep dagrun.StageReporter, reportSink TestReportSink, serverID, serverName string, tgt remoteExec, wsSuffix string) error {
	// 本节点的全部日志行都归属到该构建机(步骤 × 机器分组展示);控制流(克隆/传输/取消)仍用原 ctx。
	lctx := run.WithLogMachine(ctx, serverName)
	scriptJobs := make([]pipeline.Job, 0, len(stage.Jobs))
	for _, jb := range stage.Jobs {
		if isScriptJob(jb.Type) {
			scriptJobs = append(scriptJobs, jb)
		}
	}
	if len(scriptJobs) == 0 {
		for _, jb := range stage.Jobs {
			_ = rep.Log(lctx, streamStdout, fmt.Sprintf("· %s(%s)— 远程 runner 仅执行 script 类型;本阶段放行", jb.Name, jb.Type))
		}
		return nil
	}

	proj, settings, perr := b.resolve(ctx, r)
	if perr != nil {
		_ = rep.Log(lctx, streamError, "无法加载项目构建配置:"+perr.Error())
		return ErrBuildFailed
	}

	// 1) 控制机本地克隆(token 只在控制机)。
	workspace, mkErr := mkTempWorkspace()
	if mkErr != nil {
		_ = rep.Log(lctx, streamError, "创建临时工作区失败:"+mkErr.Error())
		return ErrBuildFailed
	}
	defer func() { _ = os.RemoveAll(workspace) }()

	auth := b.revealGitAuth(proj.CredentialID)
	resolved, cerr := b.cloner.Clone(ctx, proj.RepoURL, auth, r.Trigger.Branch, r.Trigger.Commit, workspace)
	auth = vault.GitAuth{}
	if cerr != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return run.ErrCanceled
		}
		_ = rep.Log(lctx, streamError, "源码克隆失败(鉴权/网络/ref 不存在或被 SSRF 拒绝)")
		return ErrBuildFailed
	}
	if resolved != nil && resolved.CommitShort != "" && b.recordCommit != nil {
		b.recordCommit(ctx, r.ID, resolved.CommitShort, resolved.CommitTime)
	}
	// 跨阶段产物传递:把上游阶段已归档的 jar/dist 真字节恢复到本地工作区原相对路径,
	// 随 tar 一起传远程(产物字节在控制机制品库;与本地两条路径同语义,best-effort)。
	b.restorePriorArtifacts(ctx, r, workspace, rep)

	// 2) 打包工作区 → 经 SSH 传到远程并解包。
	remoteWS := "/tmp/pipewright-remote/" + sanitizeRemoteSeg(r.ID) + "-" + sanitizeRemoteSeg(stage.ID) + wsSuffix
	remoteTar := remoteWS + ".tar.gz"
	// 同 runBuildImageJobRemote:清理在 upload 之前注册并带上 tar 包,传输中断/解包失败/取消全覆盖。
	defer func() {
		rmCmd := []string{"rm", "-rf", remoteWS, remoteTar}
		_ = rep.Log(lctx, streamStdout, "$ "+strings.Join(rmCmd, " "))
		_, _ = tgt.Exec(context.WithoutCancel(ctx), serverID, rmCmd)
	}()
	_ = rep.Log(lctx, streamStdout, "→ 远程 runner:打包工作区并经 SSH 传输…")
	if err := uploadWorkspace(ctx, tgt, serverID, workspace, remoteTar); err != nil {
		_ = rep.Log(lctx, streamError, "传输工作区到远程失败:"+err.Error())
		return ErrBuildFailed
	}
	// 远程解包 + 收尾清理(尽力)。命令回显经 rep.Log(远程命令不经过 build 的本地命令日志通道,
	// 与本地 driver 的 emitCmd 同口径:执行前先让日志看到「执行了什么」)。
	unpackCmd := []string{"sh", "-c", `mkdir -p "$0" && tar -xzf "$1" -C "$0" && rm -f "$1"`, remoteWS, remoteTar}
	_ = rep.Log(lctx, streamStdout, "$ "+strings.Join(unpackCmd, " "))
	if out, eerr := tgt.Exec(ctx, serverID, unpackCmd); eerr != nil || (out != nil && out.ExitCode != 0) {
		detail := ""
		if out != nil {
			if s := truncateLine(strings.TrimSpace(out.Stderr), remoteErrDetailLimit); s != "" {
				detail = ":" + s
			}
		}
		_ = rep.Log(lctx, streamError, "远程解包工作区失败"+detail)
		return ErrBuildFailed
	}

	// 3) 在远程机用容器跑 script job(远程 driver:按机探测 CLI —— nerdctl/docker/podman)。
	// 探测命令回显:仅真正执行探测(未命中 TTL 缓存)时输出,避免回显未执行的命令。
	bin := DetectRemoteCLIWithLog(ctx, tgt, serverID, func(line string) { _ = rep.Log(lctx, streamStdout, line) })
	_ = rep.Log(lctx, streamStdout, "远程容器 CLI:"+bin)
	driver := NewRemoteDriver(tgt, serverID, bin)
	onLine := func(stream, line string) { _ = rep.Log(lctx, stream, line) }
	for _, jb := range scriptJobs {
		if canceled(ctx) {
			return run.ErrCanceled
		}
		step, verr := scriptStepFromJob(jb)
		if verr != nil {
			_ = rep.Log(lctx, streamError, fmt.Sprintf("script job「%s」配置无效:%v", jb.Name, verr))
			return ErrBuildFailed
		}
		// 注入顺序与本地 runScriptJobIsolated 完全对齐:运行参数 → 流水线级变量(「变量与缓存」,
		// 含 secret,vault 即取即用)→ job 自身 env(后者覆盖同名)→ PIPEWRIGHT_ENV(系统,末位)。
		// 上游 job 输出 env 在远程独立工作区没有来源,远程节点间不传值(见文件头)。
		base := runParamsAsEnv(r.Trigger.Params)
		if settings != nil && len(settings.Build.Vars) > 0 {
			base = append(base, settings.Build.Vars...)
		}
		step.Env = append(base, step.Env...)
		step.Env = append(step.Env, pipewrightEnvVar())
		if err := b.runScriptOnDriver(ctx, driver, onLine, step, remoteWS); err != nil {
			return err
		}
	}

	// 4) 测试报告/质量门禁回采:报告文件在远程工作区,经 SSH cat 读回字节后在控制机走与
	//    本地完全同一套解析/门禁/持久化(门禁不过 → ErrQualityGate,阻断下游部署阶段)。
	//    collectStageReport 自身按 reportSpec 判定:未声明报告的阶段 no-op。
	readRemoteFile := func(rel string) ([]byte, error) {
		cmd := []string{"cat", remoteJoinWorkspace(remoteWS, rel)}
		out, eerr := tgt.Exec(ctx, serverID, cmd)
		if eerr != nil {
			return nil, eerr
		}
		if out == nil {
			return nil, errors.New("远程读取无结果")
		}
		if out.ExitCode != 0 {
			return nil, fmt.Errorf("远程读取退出码 %d", out.ExitCode)
		}
		return []byte(out.Stdout), nil
	}
	if err := collectStageReport(ctx, reportSink, r, stage, readRemoteFile, rep); err != nil {
		return err
	}

	_ = rep.Log(lctx, streamStdout, fmt.Sprintf("✓ 远程 runner(%s)执行完成", serverName))
	return nil
}

// remoteJoinWorkspace 把远程工作区根(POSIX 路径)与用户给的相对报告路径拼接并清洗
// `.`/`..`/前导斜杠/反斜杠防穿越(safeJoinWorkspace 的远程机版;控制机可能跑在 Windows,
// 不能用 filepath 拼远程路径,反斜杠按分隔符归一)。
func remoteJoinWorkspace(root, rel string) string {
	parts := []string{}
	for _, seg := range strings.Split(strings.ReplaceAll(rel, "\\", "/"), "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		parts = append(parts, seg)
	}
	return path.Join(root, strings.Join(parts, "/"))
}

// runScriptOnDriver 用给定 driver(本地或远程)跑一条 script 步骤;remoteWS 为容器挂载的工作区(远程机上的路径)。
// 与 runScriptStep 同语义(env 注入、workdir、多行 set -e 脚本、array 不拼 host shell),只是 driver 可注入。
func (b *Builder) runScriptOnDriver(ctx context.Context, driver Driver, onLine func(stream, line string), step pipeline.PipelineStep, remoteWS string) error {
	plain, secretEnv := b.buildArgs(step.Env)
	env := append(plain, secretEnv...)

	workdir := scriptWorkspaceMount
	if sub := strings.TrimSpace(step.WorkDir); sub != "" {
		workdir = joinContainerPath(scriptWorkspaceMount, sub)
	}
	script := "set -e\n" + strings.Join(step.Commands, "\n")
	cmd := []string{"sh", "-c", script}

	// 资源规格透传给远程 docker run(--cpus/--memory);远程 docker 不支持时由其自身报错(诚实边界)。
	// 远程模式本期不套 timeout/retry(留后续增量)。
	code, err := driver.RunToolchain(ctx, step.Image, remoteWS, workdir, env, cmd, step.Resource, onLine)
	if err != nil && code < 0 {
		if errors.Is(ctx.Err(), context.Canceled) {
			return run.ErrCanceled
		}
		return ErrBuildFailed
	}
	if code != 0 {
		return ErrBuildFailed
	}
	return nil
}

// uploadWorkspace 把本地工作区打成 tar.gz(流式)经 target.Upload 传到远程 remoteTar。
func uploadWorkspace(ctx context.Context, tgt remoteExec, serverID, workspace, remoteTar string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarGzDir(workspace, pw, false)) }()
	err := tgt.Upload(ctx, serverID, pr, remoteTar)
	_ = pr.Close()
	return err
}

// sanitizeRemoteSeg 把 id 净化为安全的远程路径段(仅字母数字 . _ -;其余替 _;空 → x)。
func sanitizeRemoteSeg(s string) string {
	s = strings.TrimSpace(s)
	var b bytes.Buffer
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

// remoteErrDetailLimit 是远程命令失败时回显 stderr 摘要的字节上限(定位失败原因足够,防爆量)。
const remoteErrDetailLimit = 300

// truncateLine 取文本首行并截断到 max 字节(远程错误摘要用;超出追加省略号)。空 → ""。
func truncateLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
