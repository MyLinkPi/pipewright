package build

import (
	"context"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/notify"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// notify_status_test.go 覆盖通知节点「状态来源节点」(config.statusFrom)的渲染:
// 配了 → {{status}}/默认文案 = 该节点当前状态(读运行内节点状态注册表);未配 → 沿用运行状态。

// fakeStageNotifier 记录 SendVia 入参(stageNotifier 最小面:Get/List/SendVia)。
type fakeStageNotifier struct {
	gotChannelID string
	gotPayload   notify.Payload
	sent         int
}

func (f *fakeStageNotifier) Get(_ context.Context, id string) (*notify.Channel, error) {
	return &notify.Channel{ID: id, Name: "测试渠道"}, nil
}

func (f *fakeStageNotifier) List(context.Context) ([]notify.Channel, error) { return nil, nil }

func (f *fakeStageNotifier) SendVia(_ context.Context, channelID string, p notify.Payload) error {
	f.gotChannelID, f.gotPayload, f.sent = channelID, p, f.sent+1
	return nil
}

func notifyStatusJob(statusFrom string) pipeline.Job {
	cfg := map[string]any{
		"channel":       "ch-1",
		"titleTemplate": "部署结果:{{status}}",
		"bodyTemplate":  "{{project}} {{branch}} {{status}}",
	}
	if statusFrom != "" {
		cfg["statusFrom"] = statusFrom
	}
	return pipeline.Job{ID: "j-n", Name: "构建通知", Type: "notify", Config: cfg}
}

func newNotifyStatusRun() *run.Run {
	return &run.Run{
		ID:          "run-1",
		ProjectName: "demo",
		Status:      run.StatusRunning, // 内联 notify 执行时运行恒为 running(既有语义)
		Trigger:     run.Trigger{Branch: "main", Commit: "abcdef1234567"},
	}
}

// TestRunNotifyJobStatusFromRendersNodeStatus 配了 statusFrom:{{status}} = 该节点当前状态
// (failed 终态也如实渲染),且默认字段透出 status/statusNode。
func TestRunNotifyJobStatusFromRendersNodeStatus(t *testing.T) {
	reg := dagrun.NewNodeStatusRegistry()
	reg.Add("j-deploy", "容器部署", run.StepSuccess)
	reg.Add("j-test", "单元测试", run.StepFailed)
	ctx := dagrun.WithNodeStatusRegistry(context.Background(), reg)

	fn := &fakeStageNotifier{}
	b := &Builder{notifier: fn}
	b.runNotifyJob(ctx, &fakeReporter{}, notifyStatusJob("单元测试"), newNotifyStatusRun())

	if fn.sent != 1 {
		t.Fatalf("应发 1 条通知, got %d", fn.sent)
	}
	if fn.gotPayload.Title != "部署结果:failed" {
		t.Errorf("标题 = %q, want 部署结果:failed", fn.gotPayload.Title)
	}
	if fn.gotPayload.Body != "demo main failed" {
		t.Errorf("正文 = %q, want demo main failed", fn.gotPayload.Body)
	}
	if got := fn.gotPayload.Fields["status"]; got != run.StepFailed {
		t.Errorf("fields.status = %q, want failed", got)
	}
	if got := fn.gotPayload.Fields["statusNode"]; got != "单元测试" {
		t.Errorf("fields.statusNode = %q, want 单元测试", got)
	}
	if got := fn.gotPayload.Fields["commit"]; got != "abcdef1" {
		t.Errorf("fields.commit = %q, want 短 sha abcdef1", got)
	}
}

// TestRunNotifyJobWithoutStatusFromKeepsRunStatus 未配 statusFrom:{{status}} 保持既有语义
// (运行状态;内联节点期间恒 running),且不注入 status/statusNode 字段。
func TestRunNotifyJobWithoutStatusFromKeepsRunStatus(t *testing.T) {
	fn := &fakeStageNotifier{}
	b := &Builder{notifier: fn}
	b.runNotifyJob(context.Background(), &fakeReporter{}, notifyStatusJob(""), newNotifyStatusRun())

	if fn.gotPayload.Title != "部署结果:running" {
		t.Errorf("标题 = %q, want 部署结果:running(运行状态)", fn.gotPayload.Title)
	}
	if _, ok := fn.gotPayload.Fields["status"]; ok {
		t.Error("未配 statusFrom 不应注入 status 字段")
	}
	if _, ok := fn.gotPayload.Fields["statusNode"]; ok {
		t.Error("未配 statusFrom 不应注入 statusNode 字段")
	}
}

// TestRunNotifyJobStatusFromUnresolvedRendersPending 配了 statusFrom 但注册表未命中
// (未执行/异常数据)→ 渲染 pending(如实反映「尚未有终态」),不退回误导性的 running。
func TestRunNotifyJobStatusFromUnresolvedRendersPending(t *testing.T) {
	// 注册表存在但没有该节点。
	reg := dagrun.NewNodeStatusRegistry()
	reg.Add("j-other", "别的东西", run.StepSuccess)
	ctx := dagrun.WithNodeStatusRegistry(context.Background(), reg)

	fn := &fakeStageNotifier{}
	b := &Builder{notifier: fn}
	b.runNotifyJob(ctx, &fakeReporter{}, notifyStatusJob("单元测试"), newNotifyStatusRun())
	if fn.gotPayload.Title != "部署结果:pending" {
		t.Errorf("未命中节点标题 = %q, want 部署结果:pending", fn.gotPayload.Title)
	}

	// 完全没有注册表(防御:执行器脱离 dagrun 单独跑)→ 同样 pending。
	fn2 := &fakeStageNotifier{}
	b2 := &Builder{notifier: fn2}
	b2.runNotifyJob(context.Background(), &fakeReporter{}, notifyStatusJob("单元测试"), newNotifyStatusRun())
	if fn2.gotPayload.Title != "部署结果:pending" {
		t.Errorf("无注册表标题 = %q, want 部署结果:pending", fn2.gotPayload.Title)
	}
}

// TestRunNotifyJobDefaultBodyMentionsNodeStatus 未配模板时,默认正文也应说明所选节点状态
// (否则配了 statusFrom 却看不到它,等于白配)。
func TestRunNotifyJobDefaultBodyMentionsNodeStatus(t *testing.T) {
	reg := dagrun.NewNodeStatusRegistry()
	reg.Add("j-test", "单元测试", run.StepSuccess)
	ctx := dagrun.WithNodeStatusRegistry(context.Background(), reg)

	fn := &fakeStageNotifier{}
	b := &Builder{notifier: fn}
	jb := notifyStatusJob("单元测试")
	jb.Config = map[string]any{"channel": "ch-1", "statusFrom": "单元测试"}
	b.runNotifyJob(ctx, &fakeReporter{}, jb, newNotifyStatusRun())

	if !strings.Contains(fn.gotPayload.Body, "节点「单元测试」状态:success") {
		t.Errorf("默认正文 = %q, 应含所选节点状态", fn.gotPayload.Body)
	}
	if !strings.Contains(fn.gotPayload.Body, "分支 main") {
		t.Errorf("默认正文 = %q, 应保留分支信息", fn.gotPayload.Body)
	}
	if got := fn.gotPayload.Fields["status"]; got != run.StepSuccess {
		t.Errorf("fields.status = %q, want success", got)
	}
}
