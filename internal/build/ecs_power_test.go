package build

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/aliyunecs"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// fakeEcsPower 记录 Power 入参并返回可配置结果(实现 stageEcsPowerController)。
type fakeEcsPower struct {
	got []aliyunecs.PowerInput
	res *aliyunecs.PowerResult
	err error
}

func (f *fakeEcsPower) Power(_ context.Context, in aliyunecs.PowerInput) (*aliyunecs.PowerResult, error) {
	f.got = append(f.got, in)
	if f.err != nil {
		return nil, f.err
	}
	if f.res != nil {
		return f.res, nil
	}
	return &aliyunecs.PowerResult{
		Total: len(in.InstanceIDs) + 1, // 占位;各用例自行配置 res
	}, nil
}

func ecsPowerTestRun() *run.Run {
	return &run.Run{ID: "run-1", ProjectName: "proj", Trigger: run.Trigger{Branch: "main"}}
}

func ecsPowerJob(cfg map[string]any) pipeline.Job {
	return pipeline.Job{ID: "ecs", Name: "开关机", Type: "ecs_power", Config: cfg}
}

// --- 直选实例:config 正确透传,逐台成功 ---

func TestRunEcsPowerDirectPassThrough(t *testing.T) {
	fake := &fakeEcsPower{
		res: &aliyunecs.PowerResult{
			Total: 2, OK: 2,
			Items: []aliyunecs.PowerItemResult{
				{InstanceID: "i-1", OK: true},
				{InstanceID: "i-2", OK: true, Skipped: true},
			},
		},
	}
	b := &Builder{ecsPower: fake}
	rep := &fakeReporter{}
	jb := ecsPowerJob(map[string]any{
		"accountId": "acc-1", "selector": "instances", "instanceIds": "i-1, i-2",
		"action": "start", "forceStop": "true", "waitSeconds": "120",
		"waitReachable": "true", "reachablePort": "2222",
	})

	if err := b.runEcsPowerJob(context.Background(), rep, jb, ecsPowerTestRun()); err != nil {
		t.Fatalf("runEcsPowerJob: %v", err)
	}
	if len(fake.got) != 1 {
		t.Fatalf("Power 调用 = %d 次", len(fake.got))
	}
	in := fake.got[0]
	if in.AccountID != "acc-1" || in.Selector != aliyunecs.SelectorInstances {
		t.Errorf("in = %+v", in)
	}
	if len(in.InstanceIDs) != 2 || in.InstanceIDs[0] != "i-1" || in.InstanceIDs[1] != "i-2" {
		t.Errorf("instanceIds = %v(应去空白)", in.InstanceIDs)
	}
	if in.Action != aliyunecs.PowerActionStart || !in.ForceStop || in.WaitSeconds != 120 {
		t.Errorf("action/force/wait = %s/%v/%d", in.Action, in.ForceStop, in.WaitSeconds)
	}
	if !in.WaitReachable || in.ReachablePort != 2222 {
		t.Errorf("waitReachable/reachablePort = %v/%d", in.WaitReachable, in.ReachablePort)
	}
	if in.Log == nil {
		t.Error("应注入流式日志回调")
	}
}

// --- 标签选择:tags 串折为条件 map,tagSource 透传 ---

func TestRunEcsPowerTagParsing(t *testing.T) {
	fake := &fakeEcsPower{
		res: &aliyunecs.PowerResult{Total: 1, OK: 1, Items: []aliyunecs.PowerItemResult{{InstanceID: "i-1", OK: true}}},
	}
	b := &Builder{ecsPower: fake}
	rep := &fakeReporter{}
	jb := ecsPowerJob(map[string]any{
		"accountId": "acc-1", "selector": "tags", "tagSource": "platform",
		"tags": "env=staging, team", "action": "start",
	})

	if err := b.runEcsPowerJob(context.Background(), rep, jb, ecsPowerTestRun()); err != nil {
		t.Fatalf("runEcsPowerJob: %v", err)
	}
	in := fake.got[0]
	if in.TagSource != aliyunecs.TagSourcePlatform {
		t.Errorf("tagSource = %q", in.TagSource)
	}
	if in.Tags["env"] != "staging" || in.Tags["team"] != "" || len(in.Tags) != 2 {
		t.Errorf("tags = %v(key-only 应折为空 value)", in.Tags)
	}
}

// --- 部分失败 → ErrBuildFailed;整批错误 → ErrBuildFailed ---

func TestRunEcsPowerFailures(t *testing.T) {
	fake := &fakeEcsPower{
		res: &aliyunecs.PowerResult{
			Total: 2, OK: 1, Failed: 1,
			Items: []aliyunecs.PowerItemResult{
				{InstanceID: "i-1", OK: true},
				{InstanceID: "i-2", Err: "等待超时(最后状态 Stopping)"},
			},
		},
	}
	b := &Builder{ecsPower: fake}
	rep := &fakeReporter{}
	jb := ecsPowerJob(map[string]any{
		"accountId": "acc-1", "instanceIds": "i-1,i-2", "action": "stop",
	})
	if err := b.runEcsPowerJob(context.Background(), rep, jb, ecsPowerTestRun()); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("部分失败应 ErrBuildFailed, got %v", err)
	}

	// 整批错误(账户不存在/0 命中)同样失败并写人话日志。
	fake2 := &fakeEcsPower{err: errors.New("aliyunecs: tag selector matched no instances:服务器标签 [env=nope] 未匹配到任何服务器")}
	b2 := &Builder{ecsPower: fake2}
	rep2 := &fakeReporter{}
	jb2 := ecsPowerJob(map[string]any{"accountId": "acc-1", "selector": "tags", "tags": "env=nope", "action": "start"})
	if err := b2.runEcsPowerJob(context.Background(), rep2, jb2, ecsPowerTestRun()); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("整批错误应 ErrBuildFailed, got %v", err)
	}
}

// --- 未注入控制器 → 跳过放行;config 缺关键字段 → 强失败 ---

func TestRunEcsPowerNilAndValidation(t *testing.T) {
	// nil 控制器:跳过(向后兼容)。
	b := &Builder{}
	rep := &fakeReporter{}
	jb := ecsPowerJob(map[string]any{"accountId": "acc-1", "action": "start", "instanceIds": "i-1"})
	if err := b.runEcsPowerJob(context.Background(), rep, jb, ecsPowerTestRun()); err != nil {
		t.Fatalf("nil 控制器应放行, got %v", err)
	}
	if len(rep.logs) == 0 || !strings.Contains(strings.Join(rep.logs, "\n"), "跳过") {
		t.Errorf("应记跳过日志: %v", rep.logs)
	}

	fake := &fakeEcsPower{res: &aliyunecs.PowerResult{}}
	b2 := &Builder{ecsPower: fake}
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"缺账户", map[string]any{"action": "start", "instanceIds": "i-1"}, "accountId"},
		{"非法动作", map[string]any{"accountId": "a", "action": "reboot", "instanceIds": "i-1"}, "动作"},
		{"直选缺实例", map[string]any{"accountId": "a", "action": "start"}, "instanceIds"},
		{"标签缺条件", map[string]any{"accountId": "a", "action": "start", "selector": "tags"}, "标签"},
		{"非法选择方式", map[string]any{"accountId": "a", "action": "start", "selector": "region", "tags": "x"}, "选择方式"},
	}
	for _, tc := range cases {
		r := &fakeReporter{}
		if err := b2.runEcsPowerJob(context.Background(), r, ecsPowerJob(tc.cfg), ecsPowerTestRun()); !errors.Is(err, ErrBuildFailed) {
			t.Errorf("%s: err = %v, want ErrBuildFailed", tc.name, err)
		}
		if !strings.Contains(strings.Join(r.logs, "\n"), tc.want) {
			t.Errorf("%s: 日志应含 %q: %v", tc.name, tc.want, r.logs)
		}
	}
	if len(fake.got) != 0 {
		t.Errorf("校验失败不应触达控制器: %v", fake.got)
	}
}
