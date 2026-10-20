package build

import (
	"context"
	"fmt"
	"strings"

	"github.com/huangchengsir/pipewright/internal/aliyunecs"
	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// ecs_power.go 实现「阿里云 ECS 开关机节点」:对一台或若干台 ECS 实例执行开机/关机
// (关机固定节省停机模式 StoppedMode=StopCharging)。机器选择三种方式:实例直选 /
// 阿里云 ECS 标签 / 平台服务器标签(后两种运行时解析,见 internal/aliyunecs/power.go)。
// 该节点恒在控制机本地执行(HTTPS 调阿里云 OpenAPI,不依赖目标机在线)。失败令阶段失败、
// 阻断下游(强失败语义,对齐 deploy_ssh/oss_upload)。

// stageEcsPowerController 抽象「执行 ECS 开关机」的能力(单方法;main 注入 aliyunecs.Service)。
type stageEcsPowerController interface {
	Power(ctx context.Context, in aliyunecs.PowerInput) (*aliyunecs.PowerResult, error)
}

// isEcsPowerJob 判断是否「ECS 开关机」节点(画布 ecs_power 类型)。
func isEcsPowerJob(jobType string) bool {
	return strings.TrimSpace(jobType) == "ecs_power"
}

// splitConfigList 把逗号串 config 值拆为去空白非空段(instanceIds 复用)。
func splitConfigList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseEcsTags 把 "env=staging,team" 形态的标签串拆为条件 map(value 空 = key 存在即可)。
func parseEcsTags(raw string) map[string]string {
	terms := splitConfigList(raw)
	if len(terms) == 0 {
		return nil
	}
	tags := make(map[string]string, len(terms))
	for _, t := range terms {
		if k, v, ok := strings.Cut(t, "="); ok {
			tags[strings.TrimSpace(k)] = strings.TrimSpace(v)
		} else {
			tags[strings.TrimSpace(t)] = ""
		}
	}
	return tags
}

// runEcsPowerJob 执行一个 ecs_power 节点:解析 config(accountId/selector/instanceIds/
// tagSource/tags/action/forceStop/waitSeconds)→ 经注入的控制器发起开关机(等待状态由
// WaitSeconds 控制)。逐台结果写节点日志;任一台失败 → ErrBuildFailed(阻断下游)。
// 阿里云服务未注入(测试/未装配)→ 记日志跳过(向后兼容,与 objectUploader nil 同)。
func (b *Builder) runEcsPowerJob(ctx context.Context, rep dagrun.StageReporter, jb pipeline.Job, r *run.Run) error {
	if b.ecsPower == nil {
		_ = rep.Log(ctx, streamStdout, "· ECS 开关机节点:阿里云服务未注入,跳过")
		return nil
	}
	accountID := cfgString(jb.Config, "accountId")
	if accountID == "" {
		_ = rep.Log(ctx, streamError, "ECS 开关机节点:未选择阿里云账户(accountId)")
		return ErrBuildFailed
	}
	action := cfgString(jb.Config, "action")
	if action != aliyunecs.PowerActionStart && action != aliyunecs.PowerActionStop {
		_ = rep.Log(ctx, streamError, "ECS 开关机节点:动作非法(仅 start/stop)")
		return ErrBuildFailed
	}
	selector := cfgString(jb.Config, "selector")
	if selector == "" {
		selector = aliyunecs.SelectorInstances
	}
	in := aliyunecs.PowerInput{
		AccountID:   accountID,
		Selector:    selector,
		Action:      action,
		ForceStop:   cfgString(jb.Config, "forceStop") == "true",
		WaitSeconds: cfgNonNegInt(jb.Config, "waitSeconds"),
		Log: func(msg string) {
			_ = rep.Log(ctx, streamStdout, msg)
		},
	}
	switch selector {
	case aliyunecs.SelectorInstances:
		in.InstanceIDs = splitConfigList(cfgString(jb.Config, "instanceIds"))
		if len(in.InstanceIDs) == 0 {
			_ = rep.Log(ctx, streamError, "ECS 开关机节点:未选择实例(instanceIds)")
			return ErrBuildFailed
		}
	case aliyunecs.SelectorTags:
		in.TagSource = cfgString(jb.Config, "tagSource")
		if in.TagSource == "" {
			in.TagSource = aliyunecs.TagSourceAliyun
		}
		in.Tags = parseEcsTags(cfgString(jb.Config, "tags"))
		if len(in.Tags) == 0 {
			_ = rep.Log(ctx, streamError, "ECS 开关机节点:标签条件为空(tags)")
			return ErrBuildFailed
		}
	default:
		_ = rep.Log(ctx, streamError, "ECS 开关机节点:选择方式非法(selector)")
		return ErrBuildFailed
	}

	res, err := b.ecsPower.Power(ctx, in)
	if err != nil {
		_ = rep.Log(ctx, streamError, "ECS 开关机节点:"+err.Error())
		return ErrBuildFailed
	}
	skipped := 0
	for _, it := range res.Items {
		switch {
		case !it.OK:
			_ = rep.Log(ctx, streamError, fmt.Sprintf("  ✗ %s:%s", it.InstanceID, it.Err))
		case it.Skipped:
			skipped++
			_ = rep.Log(ctx, streamStdout, fmt.Sprintf("  · %s:已在目标状态,跳过", it.InstanceID))
		default:
			_ = rep.Log(ctx, streamStdout, fmt.Sprintf("  ✓ %s", it.InstanceID))
		}
	}
	actionLabel := "开机"
	if action == aliyunecs.PowerActionStop {
		actionLabel = "关机(节省停机模式)"
	}
	_ = rep.Log(ctx, streamStdout, fmt.Sprintf("· ECS %s完成:%d/%d 台成功(跳过 %d 台)", actionLabel, res.OK, res.Total, skipped))
	if res.Failed > 0 {
		return ErrBuildFailed
	}
	return nil
}
