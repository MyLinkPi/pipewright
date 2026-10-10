package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/deploy"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// ─── 测试替身 ──────────────────────────────────────────────────────────────────

// fakeReporter 记录 dagrun.StageReporter 调用。
type fakeReporter struct {
	logs     []string
	machines []string // 与 logs 一一对应:该行的来源机器(run.LogMachineFrom(ctx),"":控制机/运行级)
	arts     []run.Artifact
	jobDone  []string // 记录 "jobID=status",验证节点级上报
}

func (r *fakeReporter) Log(ctx context.Context, _ string, line string) error {
	r.logs = append(r.logs, line)
	r.machines = append(r.machines, run.LogMachineFrom(ctx))
	return nil
}
func (r *fakeReporter) EmitArtifact(_ context.Context, a run.Artifact) error {
	r.arts = append(r.arts, a)
	return nil
}

// 节点级 step:测试 fake 记录 job 终态;JobReporter 返回自身(日志继续累计到同一 fake)。
func (r *fakeReporter) JobRunning(_ context.Context, _ string) error { return nil }
func (r *fakeReporter) JobDone(_ context.Context, jobID, status string) error {
	r.jobDone = append(r.jobDone, jobID+"="+status)
	return nil
}
func (r *fakeReporter) JobReporter(string) dagrun.StageReporter { return r }

// recordingDriver 记录 RunToolchain 调用并按配置回退码/日志(其余 Driver 方法不应被调到)。
type recordingDriver struct {
	code      int
	emit      []string
	gotImage  string
	gotCmd    []string
	gotWork   string
	gotEnv    []string
	gotRes    pipeline.Resource
	callCount int
}

func (d *recordingDriver) Binary() string { return "fake" }
func (d *recordingDriver) RunToolchain(_ context.Context, image, _, workdir string, env []string, cmd []string, res pipeline.Resource, onLine func(stream, line string)) (int, error) {
	d.callCount++
	d.gotImage = image
	d.gotCmd = cmd
	d.gotWork = workdir
	d.gotEnv = env
	d.gotRes = res
	for _, l := range d.emit {
		onLine("stdout", l)
	}
	return d.code, nil
}
func (d *recordingDriver) Build(context.Context, string, string, string, []string, []string, func(string, string)) (int, error) {
	panic("Build not expected")
}
func (d *recordingDriver) Tag(context.Context, string, string, func(string, string)) (int, error) {
	panic("Tag not expected")
}
func (d *recordingDriver) Login(context.Context, string, string, string, func(string, string)) (int, error) {
	panic("Login not expected")
}
func (d *recordingDriver) Push(context.Context, string, func(string, string)) (int, error) {
	panic("Push not expected")
}
func (d *recordingDriver) InspectImage(context.Context, string) (string, int64, error) {
	panic("InspectImage not expected")
}

// markerCloner 把一个标记文件写进工作区(替代真触网克隆)。
type markerCloner struct {
	file    string
	content string
}

func (c *markerCloner) Clone(_ context.Context, _ string, _ vault.GitAuth, _, _, destDir string) (*CloneResolved, error) {
	if c.file != "" {
		_ = os.WriteFile(filepath.Join(destDir, c.file), []byte(c.content), 0o644)
	}
	return &CloneResolved{CommitShort: "abc1234"}, nil
}

func newDAGTestBuilder(drv Driver, cl repoCloner) *Builder {
	return &Builder{
		projects: fakeProjects{proj: &project.Project{ID: "p1", RepoURL: "https://example.com/r.git"}},
		settings: fakeSettings{settings: &pipeline.Settings{}},
		vault:    fakeVault{secrets: map[string]string{}},
		driver:   drv,
		cloner:   cl,
	}
}

func scriptStage(jobs ...pipeline.Job) pipeline.Stage {
	return pipeline.Stage{ID: "s1", Name: "构建", Kind: pipeline.KindBuild, Jobs: jobs}
}

func scriptJob(name, image, commands string) pipeline.Job {
	return pipeline.Job{Name: name, Type: pipeline.StepTypeScript, Config: map[string]any{"image": image, "commands": commands}}
}

// ─── scriptStepFromJob / splitCommands ──────────────────────────────────────────

func TestScriptStepFromJob(t *testing.T) {
	jb := pipeline.Job{
		ID: "j1", Name: "test", Type: "script",
		Config: map[string]any{"image": " node:20 ", "commands": "npm ci\n\nnpm test\n", "workDir": "app"},
	}
	step, err := scriptStepFromJob(jb)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if step.Image != "node:20" {
		t.Errorf("image = %q", step.Image)
	}
	if len(step.Commands) != 2 || step.Commands[0] != "npm ci" || step.Commands[1] != "npm test" {
		t.Errorf("commands = %v", step.Commands)
	}
	if step.WorkDir != "app" {
		t.Errorf("workDir = %q", step.WorkDir)
	}
}

func TestScriptStepFromJobMissing(t *testing.T) {
	if _, err := scriptStepFromJob(pipeline.Job{Config: map[string]any{"commands": "x"}}); err == nil {
		t.Error("expected error for missing image")
	}
	if _, err := scriptStepFromJob(pipeline.Job{Config: map[string]any{"image": "x"}}); err == nil {
		t.Error("expected error for missing commands")
	}
}

// ─── NewStageExecutor(fake driver,无 Docker)───────────────────────────────────

func TestStageExecutorRunsScriptJobInContainer(t *testing.T) {
	drv := &recordingDriver{code: 0, emit: []string{"hello from container"}}
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	rep := &fakeReporter{}

	err := exec(context.Background(), &run.Run{ProjectID: "p1"},
		scriptStage(scriptJob("unit", "busybox", "echo hello")), rep)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if drv.callCount != 1 {
		t.Fatalf("RunToolchain called %d times, want 1", drv.callCount)
	}
	if drv.gotImage != "busybox" {
		t.Errorf("image = %q", drv.gotImage)
	}
	// 多行命令合成 sh -c set -e 脚本。
	if len(drv.gotCmd) != 3 || drv.gotCmd[0] != "sh" || drv.gotCmd[1] != "-c" || !strings.Contains(drv.gotCmd[2], "echo hello") {
		t.Errorf("cmd = %v", drv.gotCmd)
	}
	if !strings.Contains(strings.Join(rep.logs, "\n"), "hello from container") {
		t.Errorf("container log not forwarded to reporter: %v", rep.logs)
	}
}

func TestStageExecutorInjectsRunParams(t *testing.T) {
	drv := &recordingDriver{code: 0}
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	r := &run.Run{ProjectID: "p1", Trigger: run.Trigger{Params: map[string]string{"DEPLOY_ENV": "staging", "VER": "1.2"}}}
	if err := exec(context.Background(), r, scriptStage(scriptJob("unit", "busybox", "echo hi")), &fakeReporter{}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	env := strings.Join(drv.gotEnv, " ")
	if !strings.Contains(env, "DEPLOY_ENV=staging") || !strings.Contains(env, "VER=1.2") {
		t.Errorf("run params not injected into container env: %v", drv.gotEnv)
	}
}

// scriptJobWithConfig 构造一条带额外 config 键的 script job(timeout/retry/cpu/memory 等)。
func scriptJobWithConfig(name, image, commands string, extra map[string]any) pipeline.Job {
	cfg := map[string]any{"image": image, "commands": commands}
	for k, v := range extra {
		cfg[k] = v
	}
	return pipeline.Job{Name: name, Type: pipeline.StepTypeScript, Config: cfg}
}

// TestStageExecutorPassesResourceToDriver:job.Config 的 cpu/memory 应透传进 RunToolchain。
func TestStageExecutorPassesResourceToDriver(t *testing.T) {
	drv := &recordingDriver{code: 0}
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	job := scriptJobWithConfig("unit", "busybox", "echo hi", map[string]any{"cpu": "1.5", "memory": "512m"})
	if err := exec(context.Background(), &run.Run{ProjectID: "p1"}, scriptStage(job), &fakeReporter{}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if drv.gotRes.CPU != "1.5" || drv.gotRes.Memory != "512m" {
		t.Errorf("resource not passed to driver: %+v", drv.gotRes)
	}
}

// flakyDriver 在前 failUntil 次返回非零退出,其后返回 0;记录被调次数。可注入每次调用的阻塞时长(供超时测试)。
type flakyDriver struct {
	failUntil int // 前 failUntil 次返回非零(模拟失败)
	calls     int
	block     time.Duration // 每次调用阻塞时长(>0 时配合超时 ctx 测试)
}

func (d *flakyDriver) Binary() string { return "fake" }
func (d *flakyDriver) RunToolchain(ctx context.Context, _, _, _ string, _ []string, _ []string, _ pipeline.Resource, _ func(string, string)) (int, error) {
	d.calls++
	if d.block > 0 {
		select {
		case <-time.After(d.block):
		case <-ctx.Done():
			return -1, ctx.Err() // 被(超时)取消:无法完成,exitCode<0
		}
	}
	if d.calls <= d.failUntil {
		return 1, nil // 非零退出 → ErrBuildFailed
	}
	return 0, nil
}
func (d *flakyDriver) Build(context.Context, string, string, string, []string, []string, func(string, string)) (int, error) {
	panic("Build not expected")
}
func (d *flakyDriver) Tag(context.Context, string, string, func(string, string)) (int, error) {
	panic("Tag not expected")
}
func (d *flakyDriver) Login(context.Context, string, string, string, func(string, string)) (int, error) {
	panic("Login not expected")
}
func (d *flakyDriver) Push(context.Context, string, func(string, string)) (int, error) {
	panic("Push not expected")
}
func (d *flakyDriver) InspectImage(context.Context, string) (string, int64, error) {
	panic("InspectImage not expected")
}

// TestStageExecutorRetriesToSuccess:retries=2、前 2 次失败 → 第 3 次成功,共 3 次尝试,阶段成功。
func TestStageExecutorRetriesToSuccess(t *testing.T) {
	drv := &flakyDriver{failUntil: 2}
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	job := scriptJobWithConfig("unit", "busybox", "flaky", map[string]any{"retries": 2})
	if err := exec(context.Background(), &run.Run{ProjectID: "p1"}, scriptStage(job), &fakeReporter{}); err != nil {
		t.Fatalf("exec should succeed after retries: %v", err)
	}
	if drv.calls != 3 {
		t.Errorf("attempts = %d, want 3 (1 + 2 retries)", drv.calls)
	}
}

// TestStageExecutorRetriesExhausted:retries=1、始终失败 → 共 2 次尝试后判失败(ErrBuildFailed)。
func TestStageExecutorRetriesExhausted(t *testing.T) {
	drv := &flakyDriver{failUntil: 99}
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	job := scriptJobWithConfig("unit", "busybox", "always-fail", map[string]any{"retries": 1})
	err := exec(context.Background(), &run.Run{ProjectID: "p1"}, scriptStage(job), &fakeReporter{})
	if !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("err = %v, want ErrBuildFailed", err)
	}
	if drv.calls != 2 {
		t.Errorf("attempts = %d, want 2 (1 + 1 retry)", drv.calls)
	}
}

// TestStageExecutorTimeoutTriggersFailure:timeoutSeconds=1、容器阻塞 10s → 超时取消、判失败(非取消整次运行)。
func TestStageExecutorTimeoutTriggersFailure(t *testing.T) {
	drv := &flakyDriver{block: 10 * time.Second}
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	rep := &fakeReporter{}
	job := scriptJobWithConfig("unit", "busybox", "sleep 10", map[string]any{"timeoutSeconds": 1})
	start := time.Now()
	err := exec(context.Background(), &run.Run{ProjectID: "p1"}, scriptStage(job), rep)
	if !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("timeout should yield ErrBuildFailed (not run-canceled), got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout did not kick in promptly: %v", elapsed)
	}
	if !strings.Contains(strings.Join(rep.logs, "\n"), "超时") {
		t.Errorf("expected honest timeout log, got %v", rep.logs)
	}
}

func TestStageExecutorNonScriptPlaceholder(t *testing.T) {
	drv := &recordingDriver{}
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	rep := &fakeReporter{}

	// 未知类型 → 走诚实占位放行(script/build_image/deploy/health_check/notify 已支持,其余占位)。
	stage := pipeline.Stage{ID: "s", Name: "门禁", Kind: pipeline.KindCustom,
		Jobs: []pipeline.Job{{Name: "mystery", Type: "not_a_real_type", Config: map[string]any{}}}}
	if err := exec(context.Background(), &run.Run{ProjectID: "p1"}, stage, rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if drv.callCount != 0 {
		t.Error("placeholder job must not invoke RunToolchain")
	}
	if !strings.Contains(strings.Join(rep.logs, "\n"), "真实执行未接入") {
		t.Errorf("expected honest placeholder log, got %v", rep.logs)
	}
}

// imgDriver 是支持 Build/InspectImage 的测试驱动(build_image 真实化测试用)。
type imgDriver struct {
	buildCalls    int
	gotContextDir string
	gotDockerfile string
}

func (d *imgDriver) Binary() string { return "fake" }
func (d *imgDriver) RunToolchain(context.Context, string, string, string, []string, []string, pipeline.Resource, func(string, string)) (int, error) {
	return 0, nil
}
func (d *imgDriver) Build(_ context.Context, contextDir, dockerfile, _ string, _, _ []string, _ func(string, string)) (int, error) {
	d.buildCalls++
	d.gotContextDir = contextDir
	d.gotDockerfile = dockerfile
	return 0, nil
}
func (d *imgDriver) Tag(context.Context, string, string, func(string, string)) (int, error) {
	return 0, nil
}
func (d *imgDriver) Login(context.Context, string, string, string, func(string, string)) (int, error) {
	return 0, nil
}
func (d *imgDriver) Push(context.Context, string, func(string, string)) (int, error) { return 0, nil }
func (d *imgDriver) InspectImage(context.Context, string) (string, int64, error) {
	return "sha256:deadbeef", 4096, nil
}

func TestStageExecutorBuildImageReal(t *testing.T) {
	drv := &imgDriver{}
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	rep := &fakeReporter{}

	stage := pipeline.Stage{ID: "s", Name: "构建", Kind: pipeline.KindBuild,
		Jobs: []pipeline.Job{{Name: "镜像", Type: "build_image", Config: map[string]any{
			"artifactType": "image", "buildModel": "dockerfile", "dockerfilePath": "Dockerfile",
		}}}}
	if err := exec(context.Background(), &run.Run{ProjectID: "p1"}, stage, rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if drv.buildCalls != 1 {
		t.Fatalf("build_image 应调用 driver.Build 一次,实际 %d", drv.buildCalls)
	}
	if len(rep.arts) != 1 || rep.arts[0].Type != run.ArtifactImage {
		t.Fatalf("应 emit 一件 image 产物,实际 %+v", rep.arts)
	}
}

// TestStageExecutorBuildImageContextSubdir 验证 build_image 的「构建上下文(context)」配置接入:
// monorepo 子目录 Dockerfile(如 backend/Dockerfile 内 `COPY target/x.jar` 相对 backend/)
// 须把 docker build 的 context 设为该子目录,否则 docker 在仓库根找不到 COPY 源(走页面真部署
// 暴露的缺口:前端有 context 字段但后端从不消费,context 恒为仓库根)。
func TestStageExecutorBuildImageContextSubdir(t *testing.T) {
	drv := &imgDriver{}
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	rep := &fakeReporter{}

	stage := pipeline.Stage{ID: "s", Name: "构建", Kind: pipeline.KindBuild,
		Jobs: []pipeline.Job{{Name: "后端镜像", Type: "build_image", Config: map[string]any{
			"artifactType": "image", "buildModel": "dockerfile",
			"dockerfilePath": "backend/Dockerfile", "context": "backend",
		}}}}
	if err := exec(context.Background(), &run.Run{ProjectID: "p1"}, stage, rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if drv.buildCalls != 1 {
		t.Fatalf("应调用 driver.Build 一次,实际 %d", drv.buildCalls)
	}
	// context=backend → docker build 的 context 目录是 <workspace>/backend(非仓库根),
	// dockerfile 解析为相对仓库根的绝对路径 <workspace>/backend/Dockerfile(不被再 join 进子目录)。
	if !strings.HasSuffix(filepath.Clean(drv.gotContextDir), filepath.Join("backend")) {
		t.Fatalf("context 目录应为 <workspace>/backend,实际 %q", drv.gotContextDir)
	}
	if !strings.HasSuffix(filepath.Clean(drv.gotDockerfile), filepath.Join("backend", "Dockerfile")) {
		t.Fatalf("dockerfile 应为 <workspace>/backend/Dockerfile,实际 %q", drv.gotDockerfile)
	}
	if !filepath.IsAbs(drv.gotDockerfile) {
		t.Fatalf("context 非空时 dockerfile 应为绝对路径(避免被 driver 再 join),实际 %q", drv.gotDockerfile)
	}
}

func TestStageExecutorScriptFailureNonZero(t *testing.T) {
	drv := &recordingDriver{code: 1} // 容器非零退出
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	if err := exec(context.Background(), &run.Run{ProjectID: "p1"},
		scriptStage(scriptJob("unit", "busybox", "false")), &fakeReporter{}); err == nil {
		t.Error("expected ErrBuildFailed on nonzero container exit")
	}
}

func TestStageExecutorInvalidScriptConfig(t *testing.T) {
	drv := &recordingDriver{}
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	// script job 缺 image。
	bad := pipeline.Job{Name: "x", Type: "script", Config: map[string]any{"commands": "echo hi"}}
	if err := exec(context.Background(), &run.Run{ProjectID: "p1"}, scriptStage(bad), &fakeReporter{}); err == nil {
		t.Error("expected error for invalid script config")
	}
}

// ─── 真 Docker e2e(gated)─────────────────────────────────────────────────────

func TestE2EDockerStageExecutorParamsReal(t *testing.T) {
	drv := dockerReadyOrSkip(t)
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	rep := &fakeReporter{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 参数化运行:容器内 echo 注入的参数,断言真值被注入并执行。
	r := &run.Run{ProjectID: "p1", Trigger: run.Trigger{Branch: "main", Params: map[string]string{"PW_PARAM": "HELLO_FROM_PARAM"}}}
	job := scriptJob("verify", "busybox", `echo "p=$PW_PARAM"`)
	if err := exec(ctx, r, scriptStage(job), rep); err != nil {
		t.Fatalf("真 Docker 参数化执行失败: %v\n日志:\n%s", err, strings.Join(rep.logs, "\n"))
	}
	if !strings.Contains(strings.Join(rep.logs, "\n"), "p=HELLO_FROM_PARAM") {
		t.Errorf("容器未读到注入的运行参数;日志:\n%s", strings.Join(rep.logs, "\n"))
	}
}

func TestE2EDockerStageExecutorReal(t *testing.T) {
	drv := dockerReadyOrSkip(t)
	b := newDAGTestBuilder(drv, &markerCloner{file: "marker.txt", content: "PIPEWRIGHT_DAG_OK"})
	exec := NewStageExecutor(b, nil)
	rep := &fakeReporter{}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 真容器:busybox 读回工作区里的标记文件 + echo 命令输出。
	job := scriptJob("verify", "busybox", "cat marker.txt\necho RAN-IN-CONTAINER")
	if err := exec(ctx, &run.Run{ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}},
		scriptStage(job), rep); err != nil {
		t.Fatalf("真 Docker 阶段执行失败: %v\n日志:\n%s", err, strings.Join(rep.logs, "\n"))
	}
	logs := strings.Join(rep.logs, "\n")
	if !strings.Contains(logs, "PIPEWRIGHT_DAG_OK") {
		t.Errorf("容器未读到挂载的工作区文件;日志:\n%s", logs)
	}
	if !strings.Contains(logs, "RAN-IN-CONTAINER") {
		t.Errorf("容器命令未真实执行;日志:\n%s", logs)
	}
}

func TestRenderTemplate(t *testing.T) {
	ctx := map[string]string{"dir": "frontend", "cmd": "npm run build"}
	if got := renderTemplate("cd {{dir}} && {{cmd}}", ctx); got != "cd frontend && npm run build" {
		t.Fatalf("render = %q", got)
	}
	// 未知占位原样保留;无 {{ 零开销直返
	if got := renderTemplate("echo {{missing}} $HOME", ctx); got != "echo {{missing}} $HOME" {
		t.Fatalf("unknown placeholder = %q", got)
	}
}

func TestTemplateContextFreeParams(t *testing.T) {
	// 自由参数表(每行 key=value / key: value)+ config 字段都进上下文。
	cfg := map[string]any{"image": "node:20", "params": "dir=frontend\nbranch: main\n# 注释行无=跳过"}
	ctx := templateContext(cfg)
	if ctx["dir"] != "frontend" || ctx["branch"] != "main" || ctx["image"] != "node:20" {
		t.Fatalf("ctx = %+v", ctx)
	}
}

func TestScriptStepFromTemplatedJob(t *testing.T) {
	jb := pipeline.Job{ID: "j", Name: "自定义", Type: "templated", Config: map[string]any{
		"image": "node:{{ver}}", "ver": "20",
		"commandTemplate": "cd {{dir}}\nnpm ci", "dir": "web",
	}}
	step, err := scriptStepFromJob(jb)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if step.Image != "node:20" {
		t.Errorf("image = %q, want node:20", step.Image)
	}
	if len(step.Commands) != 2 || step.Commands[0] != "cd web" {
		t.Errorf("commands = %v", step.Commands)
	}
}

// stubStageDeployer 捕获 DeployForStage 的入参(只为断言 cfg / selector 透传)。
type stubStageDeployer struct {
	gotCfg      map[string]string
	gotStrategy string
	gotSelector string
}

func (d *stubStageDeployer) Deploy(context.Context, deploy.DeployInput) ([]deploy.TargetResult, error) {
	panic("Deploy not expected")
}
func (d *stubStageDeployer) RetryFailed(context.Context, deploy.RetryInput) ([]deploy.TargetResult, error) {
	panic("RetryFailed not expected")
}
func (d *stubStageDeployer) CheckHealth(context.Context, string, string, *deploy.HealthCheck) ([]deploy.HealthProbeResult, error) {
	panic("CheckHealth not expected")
}
func (d *stubStageDeployer) DeployForStage(_ context.Context, _ string, selector string, cfg map[string]string, strategy string) ([]deploy.TargetResult, error) {
	d.gotCfg = cfg
	d.gotStrategy = strategy
	d.gotSelector = selector
	return []deploy.TargetResult{{ServerName: "srv", Status: run.TargetSuccess, Message: "ok"}}, nil
}

// TestRunDeployJobPassesImageParams 证 #55:部署节点把镜像产物参数
// (artifactType/containerName/ports/runArgs + 结构化常用项 cpuLimit/memoryLimit/
// restartPolicy/envVars)透传给 deploy.DeployForStage,
// 使流水线部署节点能部署 #51 的镜像产物(而非只透传 releaseBase/restartCommand)。
func TestRunDeployJobPassesImageParams(t *testing.T) {
	dep := &stubStageDeployer{}
	b := &Builder{deployer: dep}
	rep := &fakeReporter{}
	jb := pipeline.Job{ID: "d", Name: "部署", Type: "deploy_ssh", Config: map[string]any{
		"serverId":      "srv-1",
		"artifactType":  "image",
		"containerName": "myapp",
		"ports":         "8080:80,9000:9000",
		"runArgs":       "-e KEY=v --restart always",
		"cpuLimit":      "1.0",
		"memoryLimit":   "512m",
		"restartPolicy": "unless-stopped",
		"envVars":       "KEY=v",
		"strategy":      "blue_green",
		"deployPath":    "/opt/app", // 文件态键仍应透传,不互斥
	}}
	if err := b.runDeployJob(context.Background(), rep, jb, "run-1", nil); err != nil {
		t.Fatalf("runDeployJob err: %v", err)
	}
	for k, want := range map[string]string{
		"artifactType":  "image",
		"containerName": "myapp",
		"ports":         "8080:80,9000:9000",
		"runArgs":       "-e KEY=v --restart always",
		"cpuLimit":      "1.0",
		"memoryLimit":   "512m",
		"restartPolicy": "unless-stopped",
		"envVars":       "KEY=v",
		"releaseBase":   "/opt/app",
	} {
		if got := dep.gotCfg[k]; got != want {
			t.Errorf("cfg[%q] = %q, want %q(完整 cfg=%+v)", k, got, want, dep.gotCfg)
		}
	}
	if dep.gotStrategy != "blue_green" {
		t.Errorf("strategy = %q, want blue_green", dep.gotStrategy)
	}
	// 旧 serverId 写法映射为钉单机规范形式 server:<id>(既有流水线零改动兼容)。
	if dep.gotSelector != "server:srv-1" {
		t.Errorf("selector = %q, want server:srv-1", dep.gotSelector)
	}
	// 空键不应混入(保持默认行为)。
	if _, ok := dep.gotCfg["restartCommand"]; ok {
		t.Errorf("空 restartCommand 不应入 cfg:%+v", dep.gotCfg)
	}
}

// TestRunDeployJobSelectorKey 证:节点 `selector` 键优先于旧 `serverId` 键原样透传
//(标签圈选多机部署的入口);两者皆空 → 不调部署服务、节点按跳过成功(不再失败)。
func TestRunDeployJobSelectorKey(t *testing.T) {
	dep := &stubStageDeployer{}
	b := &Builder{deployer: dep}
	rep := &fakeReporter{}

	// selector 优先:serverId 同时存在时被忽略。
	jb := pipeline.Job{ID: "d", Name: "部署", Type: "deploy_ssh", Config: map[string]any{
		"selector": "role=web",
		"serverId": "srv-1",
	}}
	if err := b.runDeployJob(context.Background(), rep, jb, "run-1", nil); err != nil {
		t.Fatalf("runDeployJob err: %v", err)
	}
	if dep.gotSelector != "role=web" {
		t.Fatalf("selector = %q, want role=web(selector 键应优先于 serverId)", dep.gotSelector)
	}

	// 两者皆空:跳过即成功(不阻断流水线),也不应调部署服务。
	dep2 := &stubStageDeployer{}
	b2 := &Builder{deployer: dep2}
	jb2 := pipeline.Job{ID: "d2", Name: "部署", Type: "deploy_ssh", Config: map[string]any{}}
	if err := b2.runDeployJob(context.Background(), rep, jb2, "run-1", nil); err != nil {
		t.Fatalf("空目标应跳过成功,got err: %v", err)
	}
	if dep2.gotSelector != "" || dep2.gotCfg != nil {
		t.Fatalf("空目标不应调部署服务,got selector=%q cfg=%v", dep2.gotSelector, dep2.gotCfg)
	}
}

// TestRunDeployJobMaxTargetsOnlyForContainer 证:目标数量上限 maxTargets **仅容器部署节点**
// 透传给部署层(部署层据此按主机负载裁到最空的 N 台);deploy_ssh / deploy_frontend 即使配了
// 该键也不透传——文件部署的多机语义是「全部命中目标都推」,不做负载裁切。
func TestRunDeployJobMaxTargetsOnlyForContainer(t *testing.T) {
	cases := []struct {
		name    string
		jobType string
		want    string // "" = 不应透传
	}{
		{"容器节点透传", "deploy_container", "3"},
		{"主机节点不透传", "deploy_ssh", ""},
		{"旧前端部署节点不透传", "deploy_frontend", ""},
	}
	for _, tc := range cases {
		dep := &stubStageDeployer{}
		b := &Builder{deployer: dep}
		jb := pipeline.Job{ID: "d", Name: "部署", Type: tc.jobType, Config: map[string]any{
			"selector":   "role=web",
			"maxTargets": "3",
		}}
		if err := b.runDeployJob(context.Background(), &fakeReporter{}, jb, "run-1", nil); err != nil {
			t.Fatalf("%s: runDeployJob err: %v", tc.name, err)
		}
		if got := dep.gotCfg["maxTargets"]; got != tc.want {
			t.Fatalf("%s: cfg[maxTargets] = %q, want %q(全 cfg=%v)", tc.name, got, tc.want, dep.gotCfg)
		}
	}

	// 空值不入 cfg(与其余键同口径:deploy 层按「未配 = 不限」处理)。
	dep := &stubStageDeployer{}
	b := &Builder{deployer: dep}
	jb := pipeline.Job{ID: "d", Name: "部署", Type: "deploy_container", Config: map[string]any{
		"selector": "role=web", "maxTargets": "",
	}}
	if err := b.runDeployJob(context.Background(), &fakeReporter{}, jb, "run-1", nil); err != nil {
		t.Fatalf("runDeployJob err: %v", err)
	}
	if _, ok := dep.gotCfg["maxTargets"]; ok {
		t.Fatalf("空 maxTargets 不应入 cfg:%+v", dep.gotCfg)
	}
}

// TestRunDeployJobZeroMatchSkips 证:部署服务返回空结果(选择器零命中)→ 节点按跳过成功。
func TestRunDeployJobZeroMatchSkips(t *testing.T) {
	b := &Builder{deployer: &zeroMatchDeployer{}}
	rep := &fakeReporter{}
	jb := pipeline.Job{ID: "d", Name: "部署", Type: "deploy_ssh", Config: map[string]any{"selector": "nope"}}
	if err := b.runDeployJob(context.Background(), rep, jb, "run-1", nil); err != nil {
		t.Fatalf("零命中应跳过成功,got err: %v", err)
	}
}

// zeroMatchDeployer 模拟「选择器未命中任何机器」:DeployForStage 返回空结果(nil error)。
type zeroMatchDeployer struct{ stubStageDeployer }

func (d *zeroMatchDeployer) DeployForStage(_ context.Context, _ string, _ string, _ map[string]string, _ string) ([]deploy.TargetResult, error) {
	return []deploy.TargetResult{}, nil
}

// ─── 阶段内 job 级 DAG 并发执行(横串竖并)─────────────────────────────────────────

// orderDriver 线程安全记录 RunToolchain 的调用顺序(按 image),并可按 image 配退出码 + 阻塞时长。
type orderDriver struct {
	mu    sync.Mutex
	order []string
	codes map[string]int // image → 退出码(缺省 0)
	block time.Duration  // >0 时每次调用阻塞,放大并发窗口
}

func (d *orderDriver) Binary() string { return "fake" }
func (d *orderDriver) RunToolchain(ctx context.Context, image, _, _ string, _ []string, _ []string, _ pipeline.Resource, onLine func(string, string)) (int, error) {
	if d.block > 0 {
		select {
		case <-time.After(d.block):
		case <-ctx.Done():
			return -1, ctx.Err()
		}
	}
	d.mu.Lock()
	d.order = append(d.order, image)
	code := d.codes[image]
	d.mu.Unlock()
	return code, nil
}
func (d *orderDriver) Build(context.Context, string, string, string, []string, []string, func(string, string)) (int, error) {
	panic("Build not expected")
}
func (d *orderDriver) Tag(context.Context, string, string, func(string, string)) (int, error) {
	panic("Tag not expected")
}
func (d *orderDriver) Login(context.Context, string, string, string, func(string, string)) (int, error) {
	panic("Login not expected")
}
func (d *orderDriver) Push(context.Context, string, func(string, string)) (int, error) {
	panic("Push not expected")
}
func (d *orderDriver) InspectImage(context.Context, string) (string, int64, error) {
	panic("InspectImage not expected")
}

func (d *orderDriver) ran(image string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, i := range d.order {
		if i == image {
			return true
		}
	}
	return false
}

// scriptJobID 构造一条带显式 ID + job 级 needs 的 script job(image 即用作顺序标记)。
func scriptJobID(id, image string, needs ...string) pipeline.Job {
	return pipeline.Job{
		ID:     id,
		Name:   id,
		Type:   pipeline.StepTypeScript,
		Config: map[string]any{"image": image, "commands": "echo " + id},
		Needs:  needs,
	}
}

// TestStageExecutorJobDAGRunsAllRespectingDeps:A、B 无依赖(并行),C needs [A,B](串行其后)。
// 三个 job 都执行,且 C 一定在 A、B 之后(DAG 路径生效)。
func TestStageExecutorJobDAGRunsAllRespectingDeps(t *testing.T) {
	drv := &orderDriver{codes: map[string]int{}, block: 20 * time.Millisecond}
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	stage := pipeline.Stage{ID: "s1", Name: "构建", Kind: pipeline.KindBuild, Jobs: []pipeline.Job{
		scriptJobID("A", "imgA"),
		scriptJobID("B", "imgB"),
		scriptJobID("C", "imgC", "A", "B"),
	}}
	if err := exec(context.Background(), &run.Run{ProjectID: "p1"}, stage, &fakeReporter{}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(drv.order) != 3 {
		t.Fatalf("应执行 3 个 job, got %v", drv.order)
	}
	if drv.order[len(drv.order)-1] != "imgC" {
		t.Errorf("C 必须在 A、B 之后(末位), got order=%v", drv.order)
	}
}

// TestStageExecutorJobDAGFailureSkipsDependents:A 失败 → 其下游 C 被跳过(不执行),阶段失败;
// 与 A 无依赖的 B 仍会执行。
func TestStageExecutorJobDAGFailureSkipsDependents(t *testing.T) {
	drv := &orderDriver{codes: map[string]int{"imgA": 1}} // A 非零退出 → 失败
	b := newDAGTestBuilder(drv, &markerCloner{})
	exec := NewStageExecutor(b, nil)
	stage := pipeline.Stage{ID: "s1", Name: "构建", Kind: pipeline.KindBuild, Jobs: []pipeline.Job{
		scriptJobID("A", "imgA"),
		scriptJobID("B", "imgB"),
		scriptJobID("C", "imgC", "A"),
	}}
	err := exec(context.Background(), &run.Run{ProjectID: "p1"}, stage, &fakeReporter{})
	if !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("A 失败应使阶段失败, err = %v", err)
	}
	if drv.ran("imgC") {
		t.Errorf("C 依赖失败的 A,应被跳过而非执行;order=%v", drv.order)
	}
	if !drv.ran("imgB") {
		t.Errorf("B 与 A 无依赖,应仍执行;order=%v", drv.order)
	}
}

// countingCloner 记录 Clone 调用次数:旧版类型分组路径全阶段共享一次克隆;job 级 DAG 路径每个需
// 工作区的 job 各克隆一次。用克隆次数确定性判定走了哪条路(不依赖并发时序)。
type countingCloner struct {
	mu sync.Mutex
	n  int
}

func (c *countingCloner) Clone(_ context.Context, _ string, _ vault.GitAuth, _, _, destDir string) (*CloneResolved, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return &CloneResolved{CommitShort: "abc1234"}, nil
}

// TestSameStageNoNeedsJobsRunParallel:同阶段两个【无 needs】但都带 ID 的 job(=画布「并行节点」),
// 应走 job 级 DAG 并行路径(各自独立克隆工作区),而非旧版串行单工作区。回归 fix(parallel-jobs-same-stage)。
func TestSameStageNoNeedsJobsRunParallel(t *testing.T) {
	drv := &orderDriver{codes: map[string]int{}}
	cl := &countingCloner{}
	b := newDAGTestBuilder(drv, cl)
	exec := NewStageExecutor(b, nil)
	stage := pipeline.Stage{ID: "s1", Name: "构建", Kind: pipeline.KindBuild, Jobs: []pipeline.Job{
		scriptJobID("fe", "imgFE"),
		scriptJobID("be", "imgBE"),
	}}
	if err := exec(context.Background(), &run.Run{ProjectID: "p1"}, stage, &fakeReporter{}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(drv.order) != 2 {
		t.Fatalf("两个 job 都应执行, got order=%v", drv.order)
	}
	if cl.n != 2 {
		t.Fatalf("无 needs 的两 job 应各自独立克隆工作区(DAG 并行路径);clone 次数=%d, want 2(=1 则退回了旧版串行单工作区)", cl.n)
	}
}

// TestSameStageSingleJobUsesLegacyPath:单 job 阶段仍走既有路径(共享单工作区,仅 1 次克隆),
// 不因多 job 并行改动引入额外克隆。
func TestSameStageSingleJobUsesLegacyPath(t *testing.T) {
	drv := &orderDriver{codes: map[string]int{}}
	cl := &countingCloner{}
	b := newDAGTestBuilder(drv, cl)
	exec := NewStageExecutor(b, nil)
	if err := exec(context.Background(), &run.Run{ProjectID: "p1"}, scriptStage(scriptJobID("only", "img1")), &fakeReporter{}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if cl.n != 1 {
		t.Fatalf("单 job 阶段应共享单一克隆;clone 次数=%d, want 1", cl.n)
	}
}
