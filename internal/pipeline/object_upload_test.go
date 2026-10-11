package pipeline

import (
	"errors"
	"testing"
)

// object_upload_test.go 覆盖「OSS 上传节点产物来源节点」(config["artifactJob"])的保存期校验:
// artifactJob 必填,且必须命中上游节点(跨阶段 needs 闭包 / 线性声明序;同阶段 job needs 闭包),
// 否则 ErrInvalidJob。

func ossJob(id, name, artifactJob string) Job {
	cfg := map[string]any{"storageId": "oss-1"}
	if artifactJob != "" {
		cfg[objectUploadArtifactJobKey] = artifactJob
	}
	return Job{ID: id, Name: name, Type: objectUploadType, Config: cfg}
}

// TestObjectUploadCrossStageNeedsClosure 跨阶段:needs 传递闭包内的节点可引用(含跨两跳)。
func TestObjectUploadCrossStageNeedsClosure(t *testing.T) {
	spec := Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-build", Name: "构建", Kind: KindBuild, Needs: []string{"src"}, Jobs: []Job{
			{ID: "j-build", Name: "构建产物", Type: "build_image", Config: map[string]any{}},
		}},
		{ID: "stg-upload", Name: "上传", Kind: KindDeploy, Needs: []string{"stg-build"}, Jobs: []Job{
			ossJob("j-up", "上传 OSS", "构建产物"),
			ossJob("j-up2", "上传源码", "拉取源码"), // 两跳:stg-upload → stg-build → src
		}},
	}}
	if _, err := NormalizeSpec(spec); err != nil {
		t.Fatalf("上游产物节点引用应通过校验, err = %v", err)
	}
}

// TestObjectUploadNotUpstreamRejected 跨阶段:非上游(无 needs 路径)/未知名均被拒。
func TestObjectUploadNotUpstreamRejected(t *testing.T) {
	spec := Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-upload", Name: "上传", Kind: KindDeploy, Jobs: []Job{
			ossJob("j-up", "上传 OSS", "构建产物"),
		}},
		{ID: "stg-build", Name: "构建", Kind: KindBuild, Jobs: []Job{
			{ID: "j-build", Name: "构建产物", Type: "build_image", Config: map[string]any{}},
		}},
	}}
	// 任一阶段都没声明 needs → 线性退化:upload 声明在前 → build 不是上游 → 拒绝。
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("引用后序节点 err = %v, want ErrInvalidJob", err)
	}
	// 未知名 → 拒绝。
	spec.Stages[1].Jobs[0].Config[objectUploadArtifactJobKey] = "不存在的节点"
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("引用未知名 err = %v, want ErrInvalidJob", err)
	}
}

// TestObjectUploadLinearFallbackAcceptsEarlierStage 线性退化(全流水线无 needs):声明序在前即上游。
func TestObjectUploadLinearFallbackAcceptsEarlierStage(t *testing.T) {
	spec := Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-build", Name: "构建", Kind: KindBuild, Jobs: []Job{
			{ID: "j-build", Name: "构建产物", Type: "build_image", Config: map[string]any{}},
		}},
		{ID: "stg-upload", Name: "上传", Kind: KindDeploy, Jobs: []Job{
			ossJob("j-up", "上传 OSS", "构建产物"),
		}},
	}}
	if _, err := NormalizeSpec(spec); err != nil {
		t.Fatalf("线性前序阶段应通过校验, err = %v", err)
	}
}

// TestObjectUploadSameStageJobNeeds 同阶段:job needs 闭包内可引用;平铺(并行)邻居不可引用。
func TestObjectUploadSameStageJobNeeds(t *testing.T) {
	stage := Stage{ID: "stg-build", Name: "构建", Kind: KindBuild, Jobs: []Job{
		{ID: "j-a", Name: "构建产物", Type: "build_image", Config: map[string]any{}},
		{ID: "j-b", Name: "并行单元测试", Type: "script", Config: map[string]any{}},
		{ID: "j-up", Name: "上传 OSS", Type: objectUploadType, Needs: []string{"j-a"}, Config: map[string]any{
			"storageId": "oss-1", objectUploadArtifactJobKey: "构建产物",
		}},
	}}
	spec := Spec{Stages: []Stage{srcStage(), stage}}
	if _, err := NormalizeSpec(spec); err != nil {
		t.Fatalf("同阶段 needs 上游应通过校验, err = %v", err)
	}
	// 平铺(未声明 needs)的同阶段节点与上传节点并行执行,不是上游 → 拒绝。
	spec.Stages[1].Jobs[2].Config[objectUploadArtifactJobKey] = "并行单元测试"
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("引用并行邻居 err = %v, want ErrInvalidJob", err)
	}
}

// TestObjectUploadSameStageSelfRejected 自引用被拒(自己不是自己的上游)。
func TestObjectUploadSameStageSelfRejected(t *testing.T) {
	spec := Spec{Stages: []Stage{srcStage(), {
		ID: "stg-upload", Name: "上传", Kind: KindDeploy, Jobs: []Job{
			ossJob("j-up", "上传 OSS", "上传 OSS"),
		},
	}}}
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("自引用 err = %v, want ErrInvalidJob", err)
	}
}

// TestObjectUploadArtifactJobRequired 缺失 / 空串 / 非字符串 artifactJob 均被拒(必填)。
func TestObjectUploadArtifactJobRequired(t *testing.T) {
	base := func(cfg map[string]any) Spec {
		return Spec{Stages: []Stage{
			srcStage(),
			{ID: "stg-build", Name: "构建", Kind: KindBuild, Needs: []string{"src"}, Jobs: []Job{
				{ID: "j-build", Name: "构建产物", Type: "build_image", Config: map[string]any{}},
			}},
			{ID: "stg-upload", Name: "上传", Kind: KindDeploy, Needs: []string{"stg-build"}, Jobs: []Job{
				{ID: "j-up", Name: "上传 OSS", Type: objectUploadType, Config: cfg},
			}},
		}}
	}
	if _, err := NormalizeSpec(base(map[string]any{"storageId": "oss-1"})); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("缺失 artifactJob err = %v, want ErrInvalidJob", err)
	}
	if _, err := NormalizeSpec(base(map[string]any{"storageId": "oss-1", objectUploadArtifactJobKey: "  "})); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("空 artifactJob err = %v, want ErrInvalidJob", err)
	}
	if _, err := NormalizeSpec(base(map[string]any{"storageId": "oss-1", objectUploadArtifactJobKey: 123})); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("非字符串 artifactJob err = %v, want ErrInvalidJob", err)
	}
}
