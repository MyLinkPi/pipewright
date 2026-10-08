package build

// remote_stage_exec.go 把「按选择器从构建机池派发到远程机器执行」接到 DAG 阶段执行器
// (FR-8-14 续 / FR-8-19 池化):stage 可用 runner 字段声明选择器覆盖项目默认;两者都空 → 本地执行。
//
// 远程执行模型(**token 全程只在控制机,绝不上远程**——比"远程克隆"更安全,且远程无需装 git):
//  1. 调度器选机并占槽(标签匹配 → 优先级 → 流水线亲和 → 负载;全忙 FIFO 排队;见 runner.Scheduler)。
//  2. 控制机本地克隆源码(复用 b.cloner;若装了 repocache 则走本地镜像增量,快)。
//  3. 把本地工作区打成 tar.gz,经 SSH(target.Upload = stdin 流,无 argv 长度限)传到远程临时目录并解包。
//  4. 在远程机用容器跑 script job(NewRemoteDriver:shellDriver 经 SSH Commander → 远程容器 CLI 运行,
//     nerdctl/docker/podman 按机探测),日志经 reporterSink 回流;阶段结束归还槽位。
//  5. 收尾删远程工作区(尽力)。
//
// 安全/语义沿用:命令 array 化(不拼 shell);secret 经 -e 注入容器(与本地同,不更差);失败映射 ErrBuildFailed。
// 边界(后续增量):远程测试报告/质量门禁采集(报告文件在远程,本期不回采,优雅跳过)。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/pipeline"
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
func NewStageExecutorWithRunner(b *Builder, reportSink TestReportSink, resolve RunnerResolver, tgt remoteExec) dagrun.StageExecutor {
	local := NewStageExecutor(b, reportSink)
	if resolve == nil || tgt == nil {
		return local
	}
	return func(ctx context.Context, r *run.Run, stage pipeline.Stage, rep dagrun.StageReporter) error {
		log := func(msg string) { _ = rep.Log(ctx, streamStdout, msg) }
		projSel := ""
		if s, ok := resolve.SelectorFor(ctx, r.ProjectID); ok {
			projSel = strings.TrimSpace(s)
		}
		stageSel := strings.TrimSpace(stage.Runner)

		scriptJobs := scriptJobsOf(stage)
	if len(scriptJobs) == 0 {
		// 无 script job 的阶段(纯部署/健康检查/通知等)只能由本地执行器真实执行:
		// 远程 runner 仅支持 script 类型,派过去只会逐节点「放行」空转——部署节点的目标
		// 选择器完全不生效(部署由控制机经 SSH 驱动目标机,与构建机池无关),阶段还白白
		// 占一个构建机槽位。一律本地,不看阶段/项目构建机选择器(那些只约束 script 节点)。
		return local(ctx, r, stage, rep)
	}

		// 逐节点判定有效选择器;全部为空 → 整体本地(行为不变,零额外开销)。
		effs := make([]string, len(scriptJobs))
		hasRemote := false
		for i, jb := range scriptJobs {
			effs[i] = effRunnerSelector(jb, stageSel, projSel)
			if effs[i] != "" {
				hasRemote = true
			}
		}
		if !hasRemote {
			return local(ctx, r, stage, rep)
		}

		// 本地侧先跑:无选择器的 script 节点 + 全部非 script 节点(部署/通知等只能由本地执行器执行)。
		var localJobs []pipeline.Job
		for _, jb := range stage.Jobs {
			if isScriptJob(jb.Type) {
				if effs[indexOfScriptJob(scriptJobs, jb)] != "" {
					continue
				}
			}
			localJobs = append(localJobs, jb)
		}
		if len(localJobs) > 0 && len(localJobs) < len(stage.Jobs) {
			if err := local(ctx, r, localSubsetStage(stage, localJobs), rep); err != nil {
				return err
			}
		}

		// 远程节点:逐节点独立取机(同选择器=同池各占一槽,可并行/排队),各自独立工作区。
		for i, jb := range scriptJobs {
			if canceled(ctx) {
				return run.ErrCanceled
			}
			sel := effs[i]
			if sel == "" {
				continue
			}
			serverID, serverName, release, err := resolve.Acquire(ctx, r.ProjectID, sel, log)
			if err != nil {
				return acquireFail(ctx, rep, sel, err)
			}
			// 派发与执行日志都归到该节点自己的 step(而非阶段首节点):点开单节点过滤日志时,
			// 能直接看到它被调度到哪台机、传输/容器执行/完成的全过程。
			jrep := rep.JobReporter(jb.ID)
			_ = jrep.Log(ctx, streamStdout, fmt.Sprintf("→ 节点「%s」→ 构建机:%s(选择器 %s)", jb.Name, serverName, sel))
			sub := stage
			sub.Jobs = []pipeline.Job{jb}
			if err := b.runStageRemote(ctx, r, sub, jrep, serverID, serverName, tgt, "-"+sanitizeRemoteSeg(jobRemoteKey(jb, i))); err != nil {
				release()
				return err
			}
			release()
		}
		return nil
	}
}

// scriptJobsOf 返回阶段内的 script job(保序)。
func scriptJobsOf(stage pipeline.Stage) []pipeline.Job {
	out := make([]pipeline.Job, 0, len(stage.Jobs))
	for _, jb := range stage.Jobs {
		if isScriptJob(jb.Type) {
			out = append(out, jb)
		}
	}
	return out
}

// indexOfScriptJob 返回 jb 在 scriptJobs 中的下标(-1 = 不在;stage.Jobs 与 scriptJobs 同序,故 O(n))。
func indexOfScriptJob(scriptJobs []pipeline.Job, jb pipeline.Job) int {
	for i := range scriptJobs {
		if scriptJobs[i].ID == jb.ID && scriptJobs[i].Name == jb.Name {
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
	_ = rep.Log(ctx, streamStderr, "构建机池调度失败("+sel+"):"+err.Error())
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

// runStageRemote 在远程 runner 上执行本阶段的 script job(见文件头模型)。
// serverName 是机器人读显示名(完成日志用);wsSuffix 是远程工作区路径后缀(节点级独立取机时
// 用 job id 区分同阶段各节点的路径,防同机碰撞)。
func (b *Builder) runStageRemote(ctx context.Context, r *run.Run, stage pipeline.Stage, rep dagrun.StageReporter, serverID, serverName string, tgt remoteExec, wsSuffix string) error {
	scriptJobs := make([]pipeline.Job, 0, len(stage.Jobs))
	for _, jb := range stage.Jobs {
		if isScriptJob(jb.Type) {
			scriptJobs = append(scriptJobs, jb)
		}
	}
	if len(scriptJobs) == 0 {
		for _, jb := range stage.Jobs {
			_ = rep.Log(ctx, streamStdout, fmt.Sprintf("· %s(%s)— 远程 runner 仅执行 script 类型;本阶段放行", jb.Name, jb.Type))
		}
		return nil
	}

	proj, _, perr := b.resolve(ctx, r)
	if perr != nil {
		_ = rep.Log(ctx, streamStderr, "无法加载项目构建配置:"+perr.Error())
		return ErrBuildFailed
	}

	// 1) 控制机本地克隆(token 只在控制机)。
	workspace, mkErr := mkTempWorkspace()
	if mkErr != nil {
		_ = rep.Log(ctx, streamStderr, "创建临时工作区失败:"+mkErr.Error())
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
		_ = rep.Log(ctx, streamStderr, "源码克隆失败(鉴权/网络/ref 不存在或被 SSRF 拒绝)")
		return ErrBuildFailed
	}
	if resolved != nil && resolved.CommitShort != "" && b.recordCommit != nil {
		b.recordCommit(ctx, r.ID, resolved.CommitShort)
	}

	// 2) 打包工作区 → 经 SSH 传到远程并解包。
	remoteWS := "/tmp/pipewright-remote/" + sanitizeRemoteSeg(r.ID) + "-" + sanitizeRemoteSeg(stage.ID) + wsSuffix
	remoteTar := remoteWS + ".tar.gz"
	_ = rep.Log(ctx, streamStdout, "→ 远程 runner:打包工作区并经 SSH 传输…")
	if err := uploadWorkspace(ctx, tgt, serverID, workspace, remoteTar); err != nil {
		_ = rep.Log(ctx, streamStderr, "传输工作区到远程失败:"+err.Error())
		return ErrBuildFailed
	}
	// 远程解包 + 收尾清理(尽力)。
	if out, eerr := tgt.Exec(ctx, serverID, []string{"sh", "-c", `mkdir -p "$0" && tar -xzf "$1" -C "$0" && rm -f "$1"`, remoteWS, remoteTar}); eerr != nil || (out != nil && out.ExitCode != 0) {
		_ = rep.Log(ctx, streamStderr, "远程解包工作区失败")
		return ErrBuildFailed
	}
	defer func() { _, _ = tgt.Exec(context.WithoutCancel(ctx), serverID, []string{"rm", "-rf", remoteWS}) }()

	// 3) 在远程机用容器跑 script job(远程 driver:按机探测 CLI —— nerdctl/docker/podman)。
	bin := DetectRemoteCLI(ctx, tgt, serverID)
	_ = rep.Log(ctx, streamStdout, "远程容器 CLI:"+bin)
	driver := NewRemoteDriver(tgt, serverID, bin)
	onLine := func(stream, line string) { _ = rep.Log(ctx, stream, line) }
	for _, jb := range scriptJobs {
		if canceled(ctx) {
			return run.ErrCanceled
		}
		step, verr := scriptStepFromJob(jb)
		if verr != nil {
			_ = rep.Log(ctx, streamStderr, fmt.Sprintf("script job「%s」配置无效:%v", jb.Name, verr))
			return ErrBuildFailed
		}
		step.Env = append(runParamsAsEnv(r.Trigger.Params), step.Env...)
		if err := b.runScriptOnDriver(ctx, driver, onLine, step, remoteWS); err != nil {
			return err
		}
	}

	_ = rep.Log(ctx, streamStdout, fmt.Sprintf("✓ 远程 runner(%s)执行完成;测试报告/质量门禁在远程模式暂不回采(后续增量)", serverName))
	return nil
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
