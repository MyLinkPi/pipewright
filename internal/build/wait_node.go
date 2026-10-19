package build

// wait_node.go 是「等待」节点(type=wait)的执行体:节点执行期 sleep 固定秒数,用于解决部署
// 过程中的时序问题(典型:deploy 之后等上游服务就绪,再做 health_check)。期间可经等待协调器
// (internal/waitnode)被手动跳过——跳过 = 该节点 step 落 skipped(如实)、但按成功放行下游。
//
// 等待在控制机本地进行(不占构建机、不依赖容器 CLI);运行取消 → 立即中断(ctx);时长有上限
// (24h,与审批门超时一致),不会无限占用 worker。等待者只存内存:进程重启即丢,孤儿运行由
// 既有清理(FailOrphanedRuns)落失败——与审批门同语义,无需持久化。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/waitnode"
)

// jobTypeWait 是「等待」节点的类型 token(与前端 jobConfigSchema 目录、AI 节点目录对齐)。
const jobTypeWait = "wait"

// maxWaitSeconds 是单次等待上限(24h;防「sleep 炸弹」长期占用 worker,与审批门超时同值)。
const maxWaitSeconds = 24 * 60 * 60

// waitHeartbeatInterval 是长等待的心跳日志间隔(总时长 > 1 分钟才打,给运行日志留进度可见性)。
const waitHeartbeatInterval = time.Minute

// errWaitSkipped 哨兵:等待被手动跳过(**非失败**)。分发点据此把 step 落 skipped、按成功放行
// 下游(调用方统一 `errors.Is` 转换,再交给 job DAG 调度)。
var errWaitSkipped = errors.New("build: wait job skipped")

// isWaitJob 判断是否「等待」节点(画布 wait 类型)。
func isWaitJob(jobType string) bool {
	return strings.TrimSpace(jobType) == jobTypeWait
}

// runWaitJob 执行一个等待节点:sleep 指定秒数,期间可被协调器手动跳过、随运行取消中断。
// 返回 (是否被手动跳过, 错误):被跳过时 err=nil、skipped=true,由调用方落 skipped 终态。
//
// rep 是阶段级 reporter(用于取节点全局 ordinal,可选能力断言);jrep 是该节点的日志 reporter。
func (b *Builder) runWaitJob(
	ctx context.Context,
	rep dagrun.StageReporter,
	jrep dagrun.StageReporter,
	jb pipeline.Job,
	runID string,
	stageName string,
) (bool, error) {
	seconds := cfgNonNegInt(jb.Config, "seconds")
	if seconds <= 0 {
		_ = jrep.Log(ctx, streamError, fmt.Sprintf("等待节点「%s」未配置有效 seconds(1–%d),无法执行", jb.Name, maxWaitSeconds))
		return false, ErrBuildFailed
	}
	if seconds > maxWaitSeconds {
		_ = jrep.Log(ctx, streamStdout, fmt.Sprintf("等待 %ds 超过上限,按 %ds 执行", seconds, maxWaitSeconds))
		seconds = maxWaitSeconds
	}

	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	info := waitnode.WaitInfo{
		Ordinal:      -1,
		JobID:        jb.ID,
		JobName:      jb.Name,
		StageName:    stageName,
		TotalSeconds: seconds,
		Deadline:     deadline,
	}
	if oa, ok := rep.(dagrun.OrdinalAwareStageReporter); ok {
		if ord, ok2 := oa.JobOrdinal(jb.ID); ok2 {
			info.Ordinal = ord
		}
	}

	// 协调器未注入(测试/旧装配)→ 照常等待,只是不可跳过(nil channel 的 select 分支永不命中)。
	var skipCh <-chan waitnode.Skip
	if b.waitCoord != nil {
		key := waitnode.Key(runID, jb.ID)
		skipCh = b.waitCoord.Wait(key, info)
		defer b.waitCoord.Cancel(key)
		_ = jrep.Log(ctx, streamStdout, fmt.Sprintf("⏳ 等待 %ds(预计 %s 结束),可在运行详情手动跳过", seconds, deadline.Format("15:04:05")))
	} else {
		_ = jrep.Log(ctx, streamStdout, fmt.Sprintf("⏳ 等待 %ds(预计 %s 结束)", seconds, deadline.Format("15:04:05")))
	}

	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	var heartbeat <-chan time.Time
	if seconds > int(waitHeartbeatInterval.Seconds()) {
		ticker := time.NewTicker(waitHeartbeatInterval)
		defer ticker.Stop()
		heartbeat = ticker.C
	}
	start := time.Now()
	for {
		select {
		case <-timer.C:
			_ = jrep.Log(ctx, streamStdout, fmt.Sprintf("✅ 等待完成(共 %ds)", seconds))
			return false, nil
		case s := <-skipCh:
			_ = jrep.Log(ctx, streamStdout, fmt.Sprintf("⏭ 等待被 %s 手动跳过(已等 %ds / 共 %ds)", waitSkipActorLabel(s.Actor), int(time.Since(start).Seconds()), seconds))
			return true, nil
		case <-heartbeat:
			remain := int(time.Until(deadline).Seconds())
			if remain > 0 {
				_ = jrep.Log(ctx, streamStdout, fmt.Sprintf("… 等待中,剩余 %ds", remain))
			}
		case <-ctx.Done():
			return false, run.ErrCanceled
		}
	}
}

// waitSkipActorLabel 把跳过操作者转为日志可读名(空 → 「手动」)。
func waitSkipActorLabel(actor string) string {
	if strings.TrimSpace(actor) == "" {
		return "手动"
	}
	return actor
}
