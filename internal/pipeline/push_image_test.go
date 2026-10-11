package pipeline

import (
	"errors"
	"testing"
)

// push_image_test.go 覆盖「推送镜像」节点产物来源(config["artifactJob"])的保存期校验:
// 必填、命中同阶段节点、且该节点是 build_image 且产物为镜像。

func buildImgJob(id, name string, cfg map[string]any) Job {
	if cfg == nil {
		cfg = map[string]any{}
	}
	return Job{ID: id, Name: name, Type: "build_image", Config: cfg}
}

func pushImgJob(id, name, artifactJob string) Job {
	cfg := map[string]any{}
	if artifactJob != "" {
		cfg[pushImageArtifactJobKey] = artifactJob
	}
	return Job{ID: id, Name: name, Type: "push_image", Config: cfg}
}

func pushImageSpec(jobs ...Job) Spec {
	return Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-build", Name: "构建", Kind: KindBuild, Jobs: jobs},
	}}
}

// 同阶段 build_image 引用 → 通过。
func TestPushImageValidSameStage(t *testing.T) {
	spec := pushImageSpec(
		buildImgJob("j-b", "构建镜像", nil),
		pushImgJob("j-p", "推送镜像", "构建镜像"),
	)
	if _, err := NormalizeSpec(spec); err != nil {
		t.Fatalf("同阶段 build_image 引用应通过, err = %v", err)
	}
}

// artifactJob 缺失 / 空串 / 非字符串 → 拒绝(必填)。
func TestPushImageRequiresArtifactJob(t *testing.T) {
	spec := pushImageSpec(
		buildImgJob("j-b", "构建镜像", nil),
		pushImgJob("j-p", "推送镜像", ""),
	)
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("缺失 artifactJob err = %v, want ErrInvalidJob", err)
	}

	spec.Stages[1].Jobs[1].Config[pushImageArtifactJobKey] = "  "
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("空 artifactJob err = %v, want ErrInvalidJob", err)
	}

	spec.Stages[1].Jobs[1].Config[pushImageArtifactJobKey] = 123
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("非字符串 artifactJob err = %v, want ErrInvalidJob", err)
	}
}

// 引用未知名 → 拒绝。
func TestPushImageRejectsUnknownNode(t *testing.T) {
	spec := pushImageSpec(
		buildImgJob("j-b", "构建镜像", nil),
		pushImgJob("j-p", "推送镜像", "不存在的节点"),
	)
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("引用未知名 err = %v, want ErrInvalidJob", err)
	}
}

// 引用跨阶段节点 → 拒绝(镜像推送在来源节点内完成,须同阶段)。
func TestPushImageRejectsCrossStageSource(t *testing.T) {
	spec := Spec{Stages: []Stage{
		srcStage(),
		{ID: "stg-build", Name: "构建", Kind: KindBuild, Jobs: []Job{
			buildImgJob("j-b", "构建镜像", nil),
		}},
		{ID: "stg-push", Name: "推送", Kind: KindDeploy, Needs: []string{"stg-build"}, Jobs: []Job{
			pushImgJob("j-p", "推送镜像", "构建镜像"),
		}},
	}}
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("跨阶段引用 err = %v, want ErrInvalidJob", err)
	}
}

// 引用同阶段的非 build_image 节点 → 拒绝。
func TestPushImageRejectsNonBuildImageSource(t *testing.T) {
	spec := pushImageSpec(
		Job{ID: "j-s", Name: "跑测试", Type: "script", Config: map[string]any{}},
		pushImgJob("j-p", "推送镜像", "跑测试"),
	)
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("引用非 build_image err = %v, want ErrInvalidJob", err)
	}
}

// 引用产物类型显式非镜像的 build_image → 拒绝。
func TestPushImageRejectsNonImageArtifact(t *testing.T) {
	spec := pushImageSpec(
		buildImgJob("j-b", "构建 JAR", map[string]any{"artifactType": "jar"}),
		pushImgJob("j-p", "推送镜像", "构建 JAR"),
	)
	if _, err := NormalizeSpec(spec); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("非镜像产物 err = %v, want ErrInvalidJob", err)
	}
}
