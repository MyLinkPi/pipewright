package build

import (
	"context"
	"fmt"
	"time"

	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// stage_post.go 执行阶段「后置步骤」(P1 · 对标 Jenkins post / GitLab after_script)。
//
// 阶段的 job 跑完后(无论成功失败),按 condition(always/on_success/on_failure)在**同一克隆工作区**
// 跑脚本类清理/通知/归档步骤。best-effort:post 步骤失败只记日志、**不改阶段结果**(jobErr 不被覆盖)。
// 调用方已用 context.WithoutCancel 传入 ctx,故阶段被取消/失败后清理仍能运行;此处再叠总超时上界。

// postTotalTimeout 是一个阶段全部 post 步骤的总超时上界(防清理脚本挂死拖住调度)。
const postTotalTimeout = 10 * time.Minute

// runStagePost 按 condition 顺序执行阶段后置步骤。stageFailed 表示阶段主体是否失败(on_failure/on_success 据此)。
// settings 供注入流水线级构建变量(统一口径见 builder.go buildArgs 注释);可为 nil(跳过该层注入)。
func (b *Builder) runStagePost(ctx context.Context, sink run.StepSink, r *run.Run, stage pipeline.Stage, workspace string, stageFailed bool, rep dagrun.StageReporter, settings *pipeline.Settings) {
	if len(stage.Post) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, postTotalTimeout)
	defer cancel()

	statusWord := "成功"
	if stageFailed {
		statusWord = "失败"
	}
	var pipelineVars []pipeline.BuildVar
	if settings != nil {
		pipelineVars = settings.Build.Vars
	}
	for i, ps := range stage.Post {
		if !pipeline.PostConditionMatches(ps.Condition, stageFailed) {
			continue
		}
		_ = rep.Log(ctx, streamStdout, fmt.Sprintf("→ 阶段后置步骤 #%d(condition=%s,阶段%s)…", i+1, ps.Condition, statusWord))
		step := pipeline.PipelineStep{
			Type:     pipeline.StepTypeScript,
			Image:    ps.Image,
			Commands: ps.Commands,
			WorkDir:  ps.WorkDir,
		}
		// env 按统一口径(见 builder.go buildArgs 注释):运行参数 → 流水线级构建变量;post 无自身
		// env,无覆盖问题。不接步骤输出捕获(post 不向下游传值)。
		step.Env = append(runParamsAsEnv(r.Trigger.Params), pipelineVars...)
		if err := b.runScriptStep(ctx, sink, 0, step, workspace); err != nil {
			// best-effort:记日志,继续后续 post 步骤,绝不覆盖阶段结果。
			_ = rep.Log(ctx, streamStderr, fmt.Sprintf("· 阶段后置步骤 #%d 未成功(best-effort,不影响阶段结果):%v", i+1, err))
		}
	}
}
