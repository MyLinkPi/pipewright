package pipeline

import (
	"errors"
	"testing"
)

// notify_status_test.go 覆盖「通知节点状态来源节点」(config["statusFrom"])的保存期校验:
// 引用必须命中上游节点(跨阶段 needs 闭包 / 线性声明序;同阶段 job needs 闭包),否则 ErrInvalidJob。

func srcStage() Stage {
	return Stage{ID: "src", Name: "流水线源", Kind: KindSource, Jobs: []Job{
		{ID: "j-src", Name: "拉取源码", Type: "git_source", Config: map[string]any{}},
	}}
}

func notifyJob(id, name, statusFrom string) Job {
	cfg := map[string]any{"channel": "ch-1"}
	if statusFrom != "" {
		cfg[notifyStatusFromKey] = statusFrom
	}
	return Job{ID: id, Name: name, Type: KindNotify, Config: cfg}
}

// TestNotifyStatusFromCrossStageNeedsClosure 跨阶段:needs 传递闭包内的节点可引用(含跨两跳)。
func TestNotifyStatusFromCrossStageNeedsClosure(t *testing.T) {
	spec := Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-build", Name: "构建", Kind: KindBuild, Needs: []string{"src"}, Jobs: []Job{
			{ID: "j-build", Name: "构建镜像", Type: "build_image", Config: map[string]any{}},
		}},
		{ID: "stg-notify", Name: "通知", Kind: KindNotify, Needs: []string{"stg-build"}, Jobs: []Job{
			notifyJob("j-notify", "构建通知", "构建镜像"),
			notifyJob("j-notify2", "源通知", "拉取源码"), // 两跳:stg-notify → stg-build → src
		}},
	}}
	if _, err := NormalizeSpec(spec); err != nil {
		t.Fatalf("上游节点引用应通过校验, err = %v", err)
	}
}

// TestNotifyStatusFromCrossStageNotUpstreamRejected 跨阶段:非上游(无 needs 路径)/未知名均被拒。
func TestNotifyStatusFromCrossStageNotUpstreamRejected(t *testing.T) {
	spec := Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-notify", Name: "通知", Kind: KindNotify, Jobs: []Job{
			notifyJob("j-notify", "通知", "构建镜像"),
		}},
		{ID: "stg-build", Name: "构建", Kind: KindBuild, Jobs: []Job{
			{ID: "j-build", Name: "构建镜像", Type: "build_image", Config: map[string]any{}},
		}},
	}}
	// 任一阶段都没声明 needs → 线性退化:notify 声明在前 → 构建不是上游 → 拒绝。
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("引用后序节点 err = %v, want ErrInvalidJob", err)
	}
	// 未知名 → 拒绝。
	spec.Stages[1].Jobs[0].Config[notifyStatusFromKey] = "不存在的节点"
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("引用未知名 err = %v, want ErrInvalidJob", err)
	}
}

// TestNotifyStatusFromLinearFallbackAcceptsEarlierStage 线性退化(全流水线无 needs):声明序在前即上游。
func TestNotifyStatusFromLinearFallbackAcceptsEarlierStage(t *testing.T) {
	spec := Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-build", Name: "构建", Kind: KindBuild, Jobs: []Job{
			{ID: "j-build", Name: "构建镜像", Type: "build_image", Config: map[string]any{}},
		}},
		{ID: "stg-notify", Name: "通知", Kind: KindNotify, Jobs: []Job{
			notifyJob("j-notify", "通知", "构建镜像"),
		}},
	}}
	if _, err := NormalizeSpec(spec); err != nil {
		t.Fatalf("线性前序阶段应通过校验, err = %v", err)
	}
}

// TestNotifyStatusFromSameStageJobNeeds 同阶段:job needs 闭包内可引用;平铺(并行)邻居不可引用。
func TestNotifyStatusFromSameStageJobNeeds(t *testing.T) {
	stage := Stage{ID: "stg-build", Name: "构建", Kind: KindBuild, Jobs: []Job{
		{ID: "j-a", Name: "构建产物", Type: "build_image", Config: map[string]any{}},
		{ID: "j-b", Name: "并行单元测试", Type: "script", Config: map[string]any{}},
		{ID: "j-n", Name: "构建通知", Type: KindNotify, Needs: []string{"j-a"}, Config: map[string]any{
			"channel": "ch-1", notifyStatusFromKey: "构建产物",
		}},
	}}
	spec := Spec{Stages: []Stage{srcStage(), stage}}
	if _, err := NormalizeSpec(spec); err != nil {
		t.Fatalf("同阶段 needs 上游应通过校验, err = %v", err)
	}
	// 平铺(未声明 needs)的同阶段节点与 notify 并行执行,不是上游 → 拒绝。
	spec.Stages[1].Jobs[2].Config[notifyStatusFromKey] = "并行单元测试"
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("引用并行邻居 err = %v, want ErrInvalidJob", err)
	}
}

// TestNotifyStatusFromSameStageSelfRejected 自引用被拒(自己不是自己的上游)。
func TestNotifyStatusFromSameStageSelfRejected(t *testing.T) {
	spec := Spec{Stages: []Stage{srcStage(), {
		ID: "stg-notify", Name: "通知", Kind: KindNotify, Jobs: []Job{
			notifyJob("j-n", "构建通知", "构建通知"),
		},
	}}}
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("自引用 err = %v, want ErrInvalidJob", err)
	}
}

// TestNotifyStatusFromAllSameNameNodesMustBeUpstream 同名多节点:必须全部为上游(存在同名非上游 → 拒绝)。
func TestNotifyStatusFromAllSameNameNodesMustBeUpstream(t *testing.T) {
	dup := func(id string) Job {
		return Job{ID: id, Name: "单元测试", Type: "script", Config: map[string]any{}}
	}
	// a、b 均被 notify 阶段 needs → 同名节点都在上游 → 通过。
	okSpec := Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-a", Name: "A", Kind: KindBuild, Needs: []string{"src"}, Jobs: []Job{dup("j-a")}},
		{ID: "stg-b", Name: "B", Kind: KindBuild, Needs: []string{"src"}, Jobs: []Job{dup("j-b")}},
		{ID: "stg-notify", Name: "通知", Kind: KindNotify, Needs: []string{"stg-a", "stg-b"}, Jobs: []Job{
			notifyJob("j-n", "通知", "单元测试"),
		}},
	}}
	if _, err := NormalizeSpec(okSpec); err != nil {
		t.Fatalf("同名节点全在上游应通过, err = %v", err)
	}
	// c 不被 needs → 存在同名非上游 → 拒绝(状态来源不确定)。
	badSpec := Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-a", Name: "A", Kind: KindBuild, Needs: []string{"src"}, Jobs: []Job{dup("j-a")}},
		{ID: "stg-c", Name: "C", Kind: KindBuild, Jobs: []Job{dup("j-c")}},
		{ID: "stg-notify", Name: "通知", Kind: KindNotify, Needs: []string{"stg-a"}, Jobs: []Job{
			notifyJob("j-n", "通知", "单元测试"),
		}},
	}}
	if _, err := NormalizeSpec(badSpec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("存在同名非上游 err = %v, want ErrInvalidJob", err)
	}
}

// TestNotifyStatusFromMatrixStageAccepted 矩阵阶段:cell 展开只改 id 不改名 → 下游按名引用仍通过校验。
func TestNotifyStatusFromMatrixStageAccepted(t *testing.T) {
	spec := Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-matrix", Name: "矩阵构建", Kind: KindBuild, Needs: []string{"src"},
			Matrix: map[string][]string{"os": {"linux", "darwin"}},
			Jobs: []Job{
				{ID: "j-mx", Name: "单元测试", Type: "script", Config: map[string]any{}},
			}},
		{ID: "stg-notify", Name: "通知", Kind: KindNotify, Needs: []string{"stg-matrix"}, Jobs: []Job{
			notifyJob("j-n", "矩阵通知", "单元测试"),
		}},
	}}
	if _, err := NormalizeSpec(spec); err != nil {
		t.Fatalf("矩阵阶段下游引用应通过校验, err = %v", err)
	}
}

// TestNotifyStatusFromTypeAndEmpty 非字符串被拒;未配 / 空串 = 不做节点级覆盖(合法)。
func TestNotifyStatusFromTypeAndEmpty(t *testing.T) {
	spec := Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-build", Name: "构建", Kind: KindBuild, Needs: []string{"src"}, Jobs: []Job{
			{ID: "j-build", Name: "构建镜像", Type: "build_image", Config: map[string]any{}},
		}},
		{ID: "stg-notify", Name: "通知", Kind: KindNotify, Needs: []string{"stg-build"}, Jobs: []Job{
			{ID: "j-n", Name: "通知", Type: KindNotify, Config: map[string]any{"channel": "ch-1", notifyStatusFromKey: 123}},
			{ID: "j-n2", Name: "通知2", Type: KindNotify, Config: map[string]any{"channel": "ch-1", notifyStatusFromKey: "  "}},
		}},
	}}
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("非字符串 statusFrom err = %v, want ErrInvalidJob", err)
	}
	// 空串/未配:合法(沿用运行状态)。
	spec.Stages[2].Jobs[0].Config = map[string]any{"channel": "ch-1"}
	if _, err := NormalizeSpec(spec); err != nil {
		t.Fatalf("空 statusFrom 应合法, err = %v", err)
	}
}
