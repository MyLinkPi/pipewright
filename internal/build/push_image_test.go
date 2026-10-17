package build

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
)

// push_image_test.go 覆盖「推送镜像节点」:推送目标声明解析、远端镜像名/tag 默认值,
// 以及 build_image 按声明目标推送(跳过环境 registry 自动推送)的行为。

func pushTestRun() *run.Run {
	return &run.Run{ID: "run-1", ProjectName: "proj", Trigger: run.Trigger{Branch: "main", Commit: "abcdef1234"}}
}

func buildImageJobSet(name string) pipeline.Job {
	return pipeline.Job{ID: "j-build", Name: name, Type: "build_image", Config: map[string]any{}}
}

func pushImageJob(cfg map[string]any) pipeline.Job {
	return pipeline.Job{ID: "j-push", Name: "推送镜像", Type: "push_image", Config: cfg}
}

// --- 推送目标声明解析 ---

func TestPushPlansByBuildNode(t *testing.T) {
	b := &Builder{builtinRegistry: func(context.Context) (string, bool) { return "192.168.1.10:5000", true }}
	r := pushTestRun()

	// 节点显式声明 registry/imageName/tag(含占位)+ credentialId。
	jobs := []pipeline.Job{
		buildImageJobSet("构建镜像"),
		pushImageJob(map[string]any{
			"artifactJob": "构建镜像", "registry": "reg2.example.com", "imageName": "org/app", "tag": "v-{{commit}}", "credentialId": "cred-2",
		}),
	}
	plans := b.pushPlansByBuildNode(context.Background(), jobs, &pipeline.Settings{}, r)
	p := plans["构建镜像"]
	if !p.Referenced {
		t.Fatalf("应标记为被 push_image 引用: %+v", plans)
	}
	if len(p.Specs) != 1 {
		t.Fatalf("应有 1 个推送目标: %+v", p.Specs)
	}
	s := p.Specs[0]
	if s.RegistryURL != "reg2.example.com" || s.ImageName != "org/app" || s.CredentialID != "cred-2" {
		t.Fatalf("节点声明解析不符: %+v", s)
	}
	if s.Tag != "v-abcdef1234" {
		t.Fatalf("tag 占位未渲染: %q", s.Tag)
	}

	// 节点未配 registry → 回退内置 registry(无凭据)。
	plans = b.pushPlansByBuildNode(context.Background(), []pipeline.Job{
		buildImageJobSet("构建镜像"),
		pushImageJob(map[string]any{"artifactJob": "构建镜像"}),
	}, &pipeline.Settings{}, r)
	s = plans["构建镜像"].Specs[0]
	if s.RegistryURL != "192.168.1.10:5000" || s.CredentialID != "" {
		t.Fatalf("应回退内置 registry: %+v", s)
	}

	// 无 artifactJob → 不产生计划(节点自身校验会拒绝)。
	plans = b.pushPlansByBuildNode(context.Background(), []pipeline.Job{pushImageJob(map[string]any{})}, &pipeline.Settings{}, r)
	if len(plans) != 0 {
		t.Fatalf("无 artifactJob 不应产生计划: %+v", plans)
	}

	// 有 artifactJob 但仓库不可解析(无内置)→ Referenced=true、Specs 为空。
	plans = (&Builder{}).pushPlansByBuildNode(context.Background(), []pipeline.Job{
		buildImageJobSet("构建镜像"),
		pushImageJob(map[string]any{"artifactJob": "构建镜像"}),
	}, &pipeline.Settings{}, r)
	p = plans["构建镜像"]
	if !p.Referenced || len(p.Specs) != 0 {
		t.Fatalf("仓库不可解析时应 Referenced 且无目标: %+v", p)
	}
}

// --- 默认镜像名 / tag ---

func TestDefaultImageNameAndTag(t *testing.T) {
	art := run.Artifact{Type: run.ArtifactImage, Name: "app", Reference: "pipewright/app:abc1234"}
	if got := defaultImageName(art); got != "app" {
		t.Fatalf("默认镜像名: %q", got)
	}
	if got := defaultImageName(run.Artifact{Reference: "localhost:5000/ns/app:tag"}); got != "app" {
		t.Fatalf("从引用取镜像名: %q", got)
	}
	if got := defaultImageTag("pipewright/app:abc1234"); got != "abc1234" {
		t.Fatalf("默认 tag: %q", got)
	}
	if got := defaultImageTag("localhost:5000/ns/app"); got != "latest" {
		t.Fatalf("无 tag 应回退 latest: %q", got)
	}
}

// --- push_image 节点执行:校验目标并播报 ---

func TestRunPushImageJobLogsTarget(t *testing.T) {
	b := &Builder{}
	rep := &fakeReporter{}
	jb := pushImageJob(map[string]any{"artifactJob": "构建镜像", "registry": "https://reg2.example.com", "imageName": "org/app", "tag": "v1"})
	if err := b.runPushImageJob(context.Background(), rep, jb, pushTestRun(), nil); err != nil {
		t.Fatalf("配了 registry 应成功: %v", err)
	}
	joined := strings.Join(rep.logs, "\n")
	if !strings.Contains(joined, "reg2.example.com/org/app:v1") {
		t.Fatalf("应播报目标仓库: %s", joined)
	}
}

func TestRunPushImageJobRequiresArtifactJob(t *testing.T) {
	b := &Builder{}
	rep := &fakeReporter{}
	if err := b.runPushImageJob(context.Background(), rep, pushImageJob(map[string]any{}), pushTestRun(), nil); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("缺 artifactJob 应 ErrBuildFailed, got %v", err)
	}
}

func TestRunPushImageJobRequiresRegistry(t *testing.T) {
	b := &Builder{}
	rep := &fakeReporter{}
	jb := pushImageJob(map[string]any{"artifactJob": "构建镜像"})
	if err := b.runPushImageJob(context.Background(), rep, jb, pushTestRun(), &pipeline.Settings{}); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("无可用仓库应 ErrBuildFailed, got %v", err)
	}
}

// --- build_image 按 push_image 声明推送(跳过内置 registry 自动推送)---

func TestRunBuildImageJobPushesPerPushSpec(t *testing.T) {
	cmdr := newFakeCommander()
	cmdr.script("build", fakeCmd{stdoutLines: []string{"built"}, exitCode: 0})
	cmdr.script("inspect", fakeCmd{stdoutLines: []string{`{"Id":"sha256:x","RepoDigests":["reg2.example.com/org/app@sha256:cafe"],"Size":10}`}, exitCode: 0})
	cmdr.script("tag", fakeCmd{exitCode: 0})
	cmdr.script("push", fakeCmd{stdoutLines: []string{"pushed"}, exitCode: 0})

	proj := &project.Project{ID: "p1", Name: "app"}
	// 内置 registry 自动推送应被 push_image 节点的声明取代(按声明目标推送)。
	settings := imageSettings()
	b := newTestBuilder(t, cmdr, proj, settings, nil)

	plan := imagePushPlan{Referenced: true, Specs: []pushSpec{{RegistryURL: "reg2.example.com", ImageName: "org/app", Tag: "v1"}}}
	rep := &fakeReporter{}
	jb := pipeline.Job{ID: "j-build", Name: "构建镜像", Type: "build_image", Config: map[string]any{}}
	if err := b.runBuildImageJob(context.Background(), &reporterSink{rep: rep}, rep, jb, "构建", proj, settings, t.TempDir(), "abc1234", plan); err != nil {
		t.Fatalf("runBuildImageJob: %v", err)
	}
	if len(rep.arts) != 1 {
		t.Fatalf("应 emit 1 件产物: %+v", rep.arts)
	}
	if got := rep.arts[0].Reference; got != "reg2.example.com/org/app:v1" {
		t.Fatalf("产物引用应为节点声明的远端 tag, got %q", got)
	}
	if rep.arts[0].Metadata["digest"] != "sha256:cafe" {
		t.Fatalf("应取推送后 digest: %+v", rep.arts[0].Metadata)
	}
	args := cmdr.allArgsText()
	if !strings.Contains(args, "reg2.example.com/org/app:v1") {
		t.Fatalf("应按节点声明的 tag 推送:\n%s", args)
	}
	if strings.Contains(args, "env.example.com") {
		t.Fatalf("不应再推环境绑定的仓库:\n%s", args)
	}
}

// 被引用但仓库不可解析 → 不推送(镜像留本地),由 push_image 节点在运行期报错。
func TestRunBuildImageJobReferencedWithoutRegistrySkipsPush(t *testing.T) {
	cmdr := newFakeCommander()
	cmdr.script("build", fakeCmd{stdoutLines: []string{"built"}, exitCode: 0})
	cmdr.script("inspect", fakeCmd{stdoutLines: []string{`{"Id":"sha256:x","Size":10}`}, exitCode: 0})
	cmdr.script("push", fakeCmd{stdoutLines: []string{"pushed"}, exitCode: 0})

	proj := &project.Project{ID: "p1", Name: "app"}
	settings := imageSettings()
	b := newTestBuilder(t, cmdr, proj, settings, nil)

	plan := imagePushPlan{Referenced: true}
	rep := &fakeReporter{}
	jb := pipeline.Job{ID: "j-build", Name: "构建镜像", Type: "build_image", Config: map[string]any{}}
	if err := b.runBuildImageJob(context.Background(), &reporterSink{rep: rep}, rep, jb, "构建", proj, settings, t.TempDir(), "abc1234", plan); err != nil {
		t.Fatalf("runBuildImageJob: %v", err)
	}
	if got := rep.arts[0].Reference; !strings.HasPrefix(got, localTagPrefix+"/") {
		t.Fatalf("应保留本地 tag, got %q", got)
	}
	if strings.Contains(cmdr.allArgsText(), "push") {
		t.Fatalf("不应执行 push:\n%s", cmdr.allArgsText())
	}
}

// 推送失败 → 阶段失败(强失败语义)。
func TestRunBuildImageJobPushFailure(t *testing.T) {
	cmdr := newFakeCommander()
	cmdr.script("build", fakeCmd{stdoutLines: []string{"built"}, exitCode: 0})
	cmdr.script("inspect", fakeCmd{stdoutLines: []string{`{"Id":"sha256:x","Size":10}`}, exitCode: 0})
	cmdr.script("tag", fakeCmd{exitCode: 0})
	cmdr.script("push", fakeCmd{stderrLines: []string{"denied"}, exitCode: 1})

	proj := &project.Project{ID: "p1", Name: "app"}
	settings := imageSettings()
	b := newTestBuilder(t, cmdr, proj, settings, nil)

	plan := imagePushPlan{Referenced: true, Specs: []pushSpec{{RegistryURL: "reg2.example.com"}}}
	rep := &fakeReporter{}
	jb := pipeline.Job{ID: "j-build", Name: "构建镜像", Type: "build_image", Config: map[string]any{}}
	err := b.runBuildImageJob(context.Background(), &reporterSink{rep: rep}, rep, jb, "构建", proj, settings, t.TempDir(), "abc1234", plan)
	if !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("推送失败应 ErrBuildFailed, got %v", err)
	}
}
