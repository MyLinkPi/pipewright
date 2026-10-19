package build

// wait_node_test.go 覆盖「等待」节点(wait):runWaitJob 的三路等待(到时/跳过/取消)、
// 配置校验与钳制,以及经 NewStageExecutor 分发路径的端到端行为(类型分组路径 + job 级 DAG
// 路径,跳过后下游照常放行)。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/waitnode"
)

// waitTestRun 是等待节点测试的运行上下文(仅需 runID 作协调器键)。
func waitTestRun() *run.Run { return &run.Run{ID: "r-wait"} }

// waitJob 构造一个等待节点(seconds 可为 0 表示缺失)。
func waitJob(id string, seconds int) pipeline.Job {
	cfg := map[string]any{}
	if seconds > 0 {
		cfg["seconds"] = float64(seconds) // JSON 反序列化后的存法(cfgNonNegInt 兼容)
	}
	return pipeline.Job{ID: id, Name: "等待" + id, Type: "wait", Config: cfg}
}

// resolveWhenWaiting 轮询协调器直到该 run 出现等待者再投递跳过,经 channel 回报是否成功
// (避免在非测试 goroutine 里调 t.Fatal;2s 超时)。
func resolveWhenWaiting(coord *waitnode.Coordinator, runID, jobID string) <-chan bool {
	done := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			for _, info := range coord.PendingByRun(runID) {
				if info.JobID == jobID {
					done <- coord.Resolve(waitnode.Key(runID, jobID), waitnode.Skip{Actor: "admin"})
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		done <- false
	}()
	return done
}

func TestRunWaitJobNaturalCompletion(t *testing.T) {
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	rep := &fakeReporter{}
	skipped, err := b.runWaitJob(context.Background(), rep, rep, waitJob("w1", 1), "r-wait", "部署")
	if err != nil || skipped {
		t.Fatalf("自然到时:skipped=%v err=%v,想要 false,nil", skipped, err)
	}
	if !containsLog(rep, "等待完成") {
		t.Fatalf("应打「等待完成」日志,得到:%v", rep.logs)
	}
}

func TestRunWaitJobManualSkip(t *testing.T) {
	coord := waitnode.New()
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	b.waitCoord = coord
	rep := &fakeReporter{}
	resolved := resolveWhenWaiting(coord, "r-wait", "w1")
	skipped, err := b.runWaitJob(context.Background(), rep, rep, waitJob("w1", 60), "r-wait", "部署")
	if err != nil || !skipped {
		t.Fatalf("手动跳过:skipped=%v err=%v,想要 true,nil", skipped, err)
	}
	if !<-resolved {
		t.Fatal("等待者注册/Resolve 未在 2s 内完成")
	}
	if !containsLog(rep, "手动跳过") {
		t.Fatalf("应打「手动跳过」日志,得到:%v", rep.logs)
	}
	// 跳过后等待者应已摘除。
	if len(coord.PendingByRun("r-wait")) != 0 {
		t.Fatal("跳过后 PendingByRun 应为空")
	}
}

func TestRunWaitJobCanceledWithRun(t *testing.T) {
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	rep := &fakeReporter{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	skipped, err := b.runWaitJob(ctx, rep, rep, waitJob("w1", 60), "r-wait", "部署")
	if !errors.Is(err, run.ErrCanceled) || skipped {
		t.Fatalf("运行取消:skipped=%v err=%v,想要 false,ErrCanceled", skipped, err)
	}
}

func TestRunWaitJobInvalidConfig(t *testing.T) {
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	rep := &fakeReporter{}
	skipped, err := b.runWaitJob(context.Background(), rep, rep, waitJob("w1", 0), "r-wait", "部署")
	if !errors.Is(err, ErrBuildFailed) || skipped {
		t.Fatalf("缺 seconds:skipped=%v err=%v,想要 false,ErrBuildFailed", skipped, err)
	}
	if !containsLog(rep, "未配置有效 seconds") {
		t.Fatalf("应打配置无效日志,得到:%v", rep.logs)
	}
}

func TestRunWaitJobClampsToMax(t *testing.T) {
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	rep := &fakeReporter{}
	// 25h 超上限:应打钳制日志并按 24h 执行(不断言等待本身,只断言日志即刻可见)。
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, _ = b.runWaitJob(ctx, rep, rep, waitJob("w1", 25*60*60), "r-wait", "部署")
	if !containsLog(rep, "超过上限") {
		t.Fatalf("应打钳制日志,得到:%v", rep.logs)
	}
}

// TestWaitOnlyStageGroupedPath:仅含一个等待节点的阶段不走「占位放行」,而是真实等待并落 success。
func TestWaitOnlyStageGroupedPath(t *testing.T) {
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	rep := &fakeReporter{}
	if err := exec(context.Background(), waitTestRun(), pipeline.Stage{ID: "s1", Name: "部署", Kind: pipeline.KindDeploy, Jobs: []pipeline.Job{waitJob("w1", 1)}}, rep); err != nil {
		t.Fatalf("wait-only 阶段应成功:%v", err)
	}
	if got := strings.Join(rep.jobDone, ","); got != "w1=success" {
		t.Fatalf("jobDone = %q,想要 w1=success", got)
	}
	if containsLog(rep, "真实执行未接入") {
		t.Fatalf("wait 节点不应走占位放行,日志:%v", rep.logs)
	}
}

// TestWaitSkipFreesDownstreamInJobDAG:job 级 DAG 路径里上游等待节点被手动跳过,
// 该节点落 skipped、下游 wait 节点照常执行(跳过非失败)。
func TestWaitSkipFreesDownstreamInJobDAG(t *testing.T) {
	coord := waitnode.New()
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	b.waitCoord = coord
	exec := NewStageExecutor(b, nil)
	rep := &fakeReporter{}
	up := waitJob("w1", 60)
	down := waitJob("w2", 1)
	down.Needs = []string{"w1"}
	resolved := resolveWhenWaiting(coord, "r-wait", "w1")
	err := exec(context.Background(), waitTestRun(), pipeline.Stage{ID: "s1", Name: "部署", Kind: pipeline.KindDeploy, Jobs: []pipeline.Job{up, down}}, rep)
	if err != nil {
		t.Fatalf("跳过等待不应令阶段失败:%v", err)
	}
	if !<-resolved {
		t.Fatal("等待者注册/Resolve 未在 2s 内完成")
	}
	got := strings.Join(rep.jobDone, ",")
	want := "w1=skipped,w2=success"
	if got != want {
		t.Fatalf("jobDone = %q,想要 %q", got, want)
	}
}

func containsLog(rep *fakeReporter, sub string) bool {
	for _, l := range rep.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}
