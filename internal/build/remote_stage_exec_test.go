package build

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/deploy"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// fakeRemoteTarget 记录远程 Exec 命令 + Upload 内容(校验派发 + 密钥安全)。
type fakeRemoteTarget struct {
	serverIDs map[string]bool
	cmds      [][]string
	uploaded  []byte
}

func (f *fakeRemoteTarget) Exec(_ context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	if f.serverIDs == nil {
		f.serverIDs = map[string]bool{}
	}
	f.serverIDs[serverID] = true
	f.cmds = append(f.cmds, cmd)
	return &target.ExecResult{ExitCode: 0}, nil
}

func (f *fakeRemoteTarget) Upload(_ context.Context, serverID string, content io.Reader, _ string) error {
	if f.serverIDs == nil {
		f.serverIDs = map[string]bool{}
	}
	f.serverIDs[serverID] = true
	b, _ := io.ReadAll(content)
	f.uploaded = append(f.uploaded, b...)
	return nil
}

// errCloner 恒定失败,用于触发远程阶段失败路径(校验 release 必被调)。
type errCloner struct{}

func (c *errCloner) Clone(_ context.Context, _ string, _ vault.GitAuth, _, _, _ string) (*CloneResolved, error) {
	return nil, errors.New("clone boom")
}

// fakeRunnerResolver 记录派发决策(选择器解析/选机/release),供断言 stage 覆盖与槽位归还。
type fakeRunnerResolver struct {
	selector   string            // SelectorFor 返回的项目默认选择器
	pick       string            // Acquire 返回的机器
	pickBy     map[string]string // 按选择器返回不同机器(节点级分组派发测试;非 nil 时优先于 pick)
	acquireErr error
	acquired   []string // 收到的选择器(先项目级,后 stage 覆盖)
	releases   int
}

func (f *fakeRunnerResolver) SelectorFor(_ context.Context, _ string) (string, bool) {
	return f.selector, f.selector != ""
}

func (f *fakeRunnerResolver) Acquire(_ context.Context, _, selector string, _ func(string)) (string, string, func(), error) {
	f.acquired = append(f.acquired, selector)
	if f.acquireErr != nil {
		return "", "", nil, f.acquireErr
	}
	pick := f.pick
	if f.pickBy != nil {
		pick = f.pickBy[selector]
	}
	return pick, pick + "-name", func() { f.releases++ }, nil
}

// routingReporter 在 fakeReporter 之上给每个 job 独立的子 reporter,记录「哪条日志归到哪个节点」,
// 供断言派发日志路由到节点自身的 step(生产实现在 dagrun.stageReporter.JobReporter)。
type routingReporter struct {
	*fakeReporter
	jobLogs     map[string][]string
	jobMachines map[string][]string // 与 jobLogs 一一对应:该节点日志行的来源机器
}

func (r *routingReporter) JobReporter(jobID string) dagrun.StageReporter {
	return &jobLineRecorder{routingReporter: r, jobID: jobID}
}

type jobLineRecorder struct {
	*routingReporter
	jobID string
}

func (j *jobLineRecorder) Log(ctx context.Context, _ string, line string) error {
	j.jobLogs[j.jobID] = append(j.jobLogs[j.jobID], line)
	j.jobMachines[j.jobID] = append(j.jobMachines[j.jobID], run.LogMachineFrom(ctx))
	return nil
}

// 带 git token 的测试 Builder(校验 token 绝不上远程)。
func newRemoteTestBuilder(drv Driver) *Builder {
	return &Builder{
		projects: fakeProjects{proj: &project.Project{ID: "p1", RepoURL: "https://example.com/r.git", CredentialID: "cred"}},
		settings: fakeSettings{settings: &pipeline.Settings{}},
		vault:    fakeVault{secrets: map[string]string{"cred": gitTokenSecret}},
		driver:   drv,
		cloner:   &markerCloner{file: "README.md", content: "hi"},
	}
}

const gitTokenSecret = "SUPERSECRET-GIT-TOKEN-xyz"

func TestStageExecutorDispatchesToRemote(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{selector: "server:srv-1", pick: "srv-1"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	stage := scriptStage(scriptJob("build", "node:20", "npm ci"))
	if err := exec(context.Background(), r, stage, rep); err != nil {
		t.Fatalf("remote exec: %v", err)
	}

	// 命令应打到远程 runner(srv-1),且本地 driver 绝不被调(构建下沉了)。
	if !tgt.serverIDs["srv-1"] {
		t.Fatalf("应派发到 srv-1;serverIDs=%v", tgt.serverIDs)
	}
	if local.callCount != 0 {
		t.Fatalf("远程派发时本地 driver 不应被调,实际 %d 次", local.callCount)
	}
	// 应上传工作区 tar.gz + 远端 untar + 远端 docker run。
	if len(tgt.uploaded) == 0 {
		t.Fatal("应上传工作区 tar.gz 到远程")
	}
	var sawUntar, sawDockerRun bool
	for _, c := range tgt.cmds {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "tar -xzf") {
			sawUntar = true
		}
		if len(c) >= 2 && c[0] == "docker" && c[1] == "run" {
			sawDockerRun = true
		}
	}
	if !sawUntar || !sawDockerRun {
		t.Fatalf("远程应 untar + docker run;untar=%v run=%v cmds=%v", sawUntar, sawDockerRun, tgt.cmds)
	}

	// 安全铁律:git token 绝不出现在任何远程命令 / 上传内容里(token 全程只在控制机)。
	for _, c := range tgt.cmds {
		if strings.Contains(strings.Join(c, " "), gitTokenSecret) {
			t.Fatalf("git token 泄漏进远程命令!%v", c)
		}
	}
	if bytes.Contains(tgt.uploaded, []byte(gitTokenSecret)) {
		t.Fatal("git token 泄漏进上传内容!")
	}
}

func TestStageExecutorLocalWhenNoRunner(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	// 无 runner(空)→ 走本地。
	exec := NewStageExecutorWithRunner(b, nil, &fakeRunnerResolver{}, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	if err := exec(context.Background(), r, scriptStage(scriptJob("build", "node:20", "echo hi")), rep); err != nil {
		t.Fatalf("local exec: %v", err)
	}
	if local.callCount != 1 {
		t.Fatalf("无 runner 应本地执行(driver 调 1 次),实际 %d", local.callCount)
	}
	if len(tgt.cmds) != 0 || len(tgt.uploaded) != 0 {
		t.Fatal("无 runner 不应有任何远程动作")
	}
}

// lookup/tgt 为 nil → 退化纯本地(不 panic)。
func TestStageExecutorNilRunnerIsLocal(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	exec := NewStageExecutorWithRunner(b, nil, nil, nil)
	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	if err := exec(context.Background(), r, scriptStage(scriptJob("b", "alpine", "true")), rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if local.callCount != 1 {
		t.Fatalf("nil runner 应纯本地,实际 driver 调 %d 次", local.callCount)
	}
}

// 纯部署阶段(无 script job)即使项目配了默认构建机池,也必须本地真实执行——远程 runner
// 只跑 script 类型,派发过去会逐节点「放行」空转,部署节点的目标选择器完全不生效
// (回归:部署阶段被派到构建机,deploy_container 日志「本阶段放行」,实际未部署)。
func TestStageExecutorDeployOnlyStageStaysLocal(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{selector: "test_build", pick: "srv-1"} // 项目默认构建机池非空
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	stage := pipeline.Stage{
		ID: "s-dep", Name: "部署", Kind: pipeline.KindDeploy,
		Jobs: []pipeline.Job{
			{ID: "j-dep", Name: "容器部署", Type: "deploy_container", Config: map[string]any{"selector": "test"}},
		},
	}
	if err := exec(context.Background(), r, stage, rep); err != nil {
		t.Fatalf("exec: %v", err)
	}

	// 不占构建机槽、零远程动作;节点由本地执行器走完(deployer 未注入 → 诚实跳过日志,仍成功)。
	if len(fr.acquired) != 0 {
		t.Fatalf("纯部署阶段不应占用构建机,实际 Acquire %v", fr.acquired)
	}
	if len(tgt.cmds) != 0 || len(tgt.uploaded) != 0 {
		t.Fatal("纯部署阶段不应有任何远程动作")
	}
	if joined := strings.Join(rep.logs, "\n"); !strings.Contains(joined, "执行机器:本机") {
		t.Fatalf("应本地执行(日志含「执行机器:本机」),实际日志:%s", joined)
	}
	if len(rep.jobDone) != 1 || rep.jobDone[0] != "j-dep="+run.StepSuccess {
		t.Fatalf("部署节点应本地执行成功,实际 jobDone=%v", rep.jobDone)
	}
}

// stage.Runner 覆盖项目默认选择器(FR-8-19 阶段级覆盖)。
func TestStageRunnerOverridesProjectSelector(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{selector: "linux", pick: "srv-gpu"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	st := scriptStage(scriptJob("train", "pytorch/pytorch", "python train.py"))
	st.Runner = "gpu"
	if err := exec(context.Background(), r, st, rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(fr.acquired) != 1 || fr.acquired[0] != "gpu" {
		t.Fatalf("stage 覆盖应传 gpu,实际 %v", fr.acquired)
	}
	if fr.releases != 1 {
		t.Fatalf("阶段完成后应恰好 release 一次,实际 %d", fr.releases)
	}
}

// 无 stage 覆盖时用项目默认选择器。
func TestProjectSelectorUsedWhenStageEmpty(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{selector: "linux,arch=arm64", pick: "srv-a"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)
	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	if err := exec(context.Background(), r, scriptStage(scriptJob("b", "alpine", "true")), rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(fr.acquired) != 1 || fr.acquired[0] != "linux,arch=arm64" {
		t.Fatalf("应传项目默认选择器,实际 %v", fr.acquired)
	}
}

// 远程执行失败也必须 release 槽位(defer 路径)。
func TestReleaseOnRemoteFailure(t *testing.T) {
	b := newRemoteTestBuilder(&recordingDriver{})
	// cloner 报错 → runStageRemote 失败(fakeCloner? 用坏 vault 让克隆失败:直接换 cloner)
	b.cloner = &errCloner{}
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{selector: "linux", pick: "srv-1"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)
	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	if err := exec(context.Background(), r, scriptStage(scriptJob("b", "alpine", "true")), rep); err == nil {
		t.Fatal("克隆失败应让阶段失败")
	}
	if fr.releases != 1 {
		t.Fatalf("失败路径必须 release,实际 %d", fr.releases)
	}
}

// 调度错误(无命中机器)→ 阶段失败 ErrBuildFailed + 日志明示,不进远程。
func TestAcquireErrorFailsStage(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{selector: "gpu", acquireErr: errors.New("runner: no server matches selector")}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)
	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	err := exec(context.Background(), r, scriptStage(scriptJob("b", "alpine", "true")), rep)
	if !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("调度失败应映射 ErrBuildFailed,实际 %v", err)
	}
	if local.callCount != 0 || len(tgt.cmds) != 0 {
		t.Fatal("调度失败不应有本地/远程执行")
	}
	if len(fr.acquired) != 1 {
		t.Fatal("应恰好尝试一次调度")
	}
}

// pickBy 支持「按选择器返回不同机器」(节点级分组派发测试用;非空时优先于 pick)。
func (f *fakeRunnerResolver) pickFor(selector string) string {
	if f.pickBy != nil {
		return f.pickBy[selector]
	}
	return f.pick
}

func TestStageExecutorJobLevelRunnerDispatch(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{pickBy: map[string]string{"server:srv-a": "srv-a", "server:srv-b": "srv-b"}}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	// 节点 A 自带 runner(钉 srv-a);节点 B 无覆盖 → 跟随项目默认(server:srv-b)。
	ja := scriptJobWithConfig("a", "node:20", "echo a", map[string]any{"runner": "server:srv-a"})
	jb := scriptJob("b", "node:20", "echo b")
	stage := scriptStage(ja, jb)
	stage.Runner = ""
	fr.selector = "server:srv-b"
	if err := exec(context.Background(), r, stage, rep); err != nil {
		t.Fatalf("exec: %v", err)
	}

	// 两次取机,各按其选择器;两台机器都收到工作区与容器执行。
	if len(fr.acquired) != 2 || fr.acquired[0] != "server:srv-a" || fr.acquired[1] != "server:srv-b" {
		t.Fatalf("acquired = %v, want [server:srv-a server:srv-b]", fr.acquired)
	}
	if !tgt.serverIDs["srv-a"] || !tgt.serverIDs["srv-b"] {
		t.Fatalf("两台机器都应被派发;serverIDs=%v", tgt.serverIDs)
	}
	if local.callCount != 0 {
		t.Fatalf("全部节点有选择器时不应走本地执行,实际本地 driver 调用 %d 次", local.callCount)
	}
	if fr.releases != 2 {
		t.Fatalf("槽位应全部归还,实际 %d", fr.releases)
	}
}

func TestStageExecutorSameSelectorIndependentSlots(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{selector: "linux", pick: "srv-1"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	// 两个节点同选 linux(标签项)→ 各自独立占槽(同池各取一台;池内同机也不共享工作区):
	// 两次取机、两次工作区传输,且远程工作区路径互不相同(防同机互踩)。
	stage := scriptStage(
		scriptJobWithConfig("a", "node:20", "echo a", map[string]any{"runner": "linux"}),
		scriptJobWithConfig("b", "node:20", "echo b", map[string]any{"runner": " linux "}),
	)
	if err := exec(context.Background(), r, stage, rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(fr.acquired) != 2 || fr.acquired[0] != "linux" || fr.acquired[1] != "linux" {
		t.Fatalf("同选择器的每个节点应独立取机,实际 %v", fr.acquired)
	}
	if fr.releases != 2 {
		t.Fatalf("槽位应全部归还,实际 %d", fr.releases)
	}
	untarPaths := make([]string, 0, 2)
	for _, c := range tgt.cmds {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "tar -xzf") {
			untarPaths = append(untarPaths, joined)
		}
	}
	if len(untarPaths) != 2 || untarPaths[0] == untarPaths[1] {
		t.Fatalf("每个节点应有独立远程工作区,实际 %v", untarPaths)
	}
}

func TestStageExecutorMixedLocalRemote(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{pickBy: map[string]string{"server:srv-a": "srv-a"}}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	// 混合:A 钉远程;B 无任何选择器(本地);C 部署节点(本地执行器路径)。
	ja := scriptJobWithConfig("a", "node:20", "echo a", map[string]any{"runner": "server:srv-a"})
	jb := scriptJob("b", "node:20", "echo b")
	jc := pipeline.Job{Name: "c", Type: "deploy_ssh", Config: map[string]any{}}
	stage := scriptStage(ja, jb, jc)
	if err := exec(context.Background(), r, stage, rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(fr.acquired) != 1 || fr.acquired[0] != "server:srv-a" {
		t.Fatalf("只有 A 应取机,实际 %v", fr.acquired)
	}
	if local.callCount != 1 {
		t.Fatalf("无选择器的 script 节点应走本地执行,实际本地 driver %d 次", local.callCount)
	}
	joined := strings.Join(rep.logs, "\n")
	if !strings.Contains(joined, "部署服务未注入") {
		t.Fatalf("部署节点应并入本地组执行(跳过日志);logs=%v", rep.logs)
	}
}

func TestStageExecutorAllEmptyStaysLocal(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	stage := scriptStage(scriptJob("a", "node:20", "echo a"))
	if err := exec(context.Background(), r, stage, rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(fr.acquired) != 0 {
		t.Fatalf("无选择器不应取机,实际 %v", fr.acquired)
	}
	if local.callCount != 1 {
		t.Fatalf("应整体本地执行,实际本地 driver %d 次", local.callCount)
	}
	// 本地执行也要在日志里明示机器:执行机器 = 本机(控制机)。
	if joined := strings.Join(rep.logs, "\n"); !strings.Contains(joined, "→ 执行机器:本机(控制机)") {
		t.Fatalf("本地执行应打「执行机器:本机」行;logs=%v", rep.logs)
	}
}

// 运行日志应明确显示每个节点被调度到哪台机器:派发行用人读机器名(而非 uuid),
// 且经节点级 reporter 归到该节点自己的日志流(单节点过滤日志时可见)。
func TestStageExecutorDispatchLogShowsMachineName(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{pickBy: map[string]string{"server:srv-a": "srv-a", "server:srv-b": "srv-b"}}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &routingReporter{fakeReporter: &fakeReporter{}, jobLogs: map[string][]string{}, jobMachines: map[string][]string{}}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	ja := scriptJobWithConfig("a", "node:20", "echo a", map[string]any{"runner": "server:srv-a"})
	ja.ID = "ja"
	jb := scriptJobWithConfig("b", "node:20", "echo b", map[string]any{"runner": "server:srv-b"})
	jb.ID = "jb"
	if err := exec(context.Background(), r, scriptStage(ja, jb), rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	wantA := "→ 节点「a」→ 构建机:srv-a-name(选择器 server:srv-a)"
	if got := strings.Join(rep.jobLogs["ja"], "\n"); !strings.Contains(got, wantA) {
		t.Fatalf("节点 a 的派发日志应含机器显示名并归到节点自身:got %q", rep.jobLogs["ja"])
	}
	wantB := "→ 节点「b」→ 构建机:srv-b-name(选择器 server:srv-b)"
	if got := strings.Join(rep.jobLogs["jb"], "\n"); !strings.Contains(got, wantB) {
		t.Fatalf("节点 b 的派发日志应含机器显示名并归到节点自身:got %q", rep.jobLogs["jb"])
	}
	// 该节点的完成行也归到节点自身,且打机器显示名而非 uuid。
	if got := strings.Join(rep.jobLogs["ja"], "\n"); !strings.Contains(got, "✓ 远程 runner(srv-a-name)执行完成") {
		t.Fatalf("完成日志应含机器显示名并归到节点自身:got %q", rep.jobLogs["ja"])
	}
	// 派发日志不得再出现裸机器 id(旧行为)。
	if joined := strings.Join(rep.jobLogs["ja"], "\n"); strings.Contains(joined, "构建机:srv-a(") {
		t.Fatalf("派发日志不应打裸机器 id:got %q", joined)
	}
	// 机器归属(步骤 × 机器分组的数据来源):该节点全部日志行的 machine 应 = 构建机显示名。
	for i, m := range rep.jobMachines["ja"] {
		if m != "srv-a-name" {
			t.Fatalf("节点 a 第 %d 行 machine 应为 srv-a-name, got %q(行:%q)", i, m, rep.jobLogs["ja"][i])
		}
	}
}

// ─── 远程执行语义修复(变量注入 / 产物恢复 / 槽位与生命周期 / 混合拓扑与 post)──────────────

// TestRemoteStageInjectsPipelineVarsAndEnvFile 证修复 1:远程 script job 与本地
// runScriptJobIsolated 同序注入 env——运行参数 → 流水线级变量(settings.Build.Vars,含
// secret,vault 即取即用)→ job env → PIPEWRIGHT_ENV。此前远程路径只注入运行参数,
// 「变量与缓存」里配的变量/secret 在远程节点全部丢失。git token 仍绝不上远程。
func TestRemoteStageInjectsPipelineVarsAndEnvFile(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	b.settings = fakeSettings{settings: &pipeline.Settings{Build: pipeline.BuildConfig{Vars: []pipeline.BuildVar{
		{Key: "APP_ENV", Value: "prod"},
		{Key: "RELEASE_TOKEN", Secret: true, CredentialID: "rel-cred"},
	}}}}
	b.vault = fakeVault{secrets: map[string]string{"cred": gitTokenSecret, "rel-cred": "release-secret-value"}}
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{selector: "linux", pick: "srv-1"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main", Params: map[string]string{"VER": "1.0"}}}
	if err := exec(context.Background(), r, scriptStage(scriptJob("build", "node:20", "echo ok")), rep); err != nil {
		t.Fatalf("exec: %v", err)
	}

	// 远程 docker run argv(-e K=V 注入,与本地同一机制)。
	var runCmd []string
	for _, c := range tgt.cmds {
		if len(c) >= 2 && c[0] == "docker" && c[1] == "run" {
			runCmd = c
		}
	}
	if runCmd == nil {
		t.Fatalf("远程应有 docker run;cmds=%v", tgt.cmds)
	}
	joined := strings.Join(runCmd, " ")
	for _, want := range []string{"-e VER=1.0", "-e APP_ENV=prod", "-e RELEASE_TOKEN=release-secret-value", "-e PIPEWRIGHT_ENV="} {
		if !strings.Contains(joined, want) {
			t.Errorf("远程容器 env 应含 %q;argv=%v", want, runCmd)
		}
	}
	// 安全铁律不变:git token(仓库凭据)绝不出现在任何远程命令/上传内容。
	if strings.Contains(joined, gitTokenSecret) {
		t.Fatal("git token 泄漏进远程命令!")
	}
	if bytes.Contains(tgt.uploaded, []byte(gitTokenSecret)) {
		t.Fatal("git token 泄漏进上传内容!")
	}
}

// TestRemoteStageRestoresPriorArtifacts 证修复 2:远程节点打包上传前,先在控制机工作区恢复
// 上游阶段归档产物(随 tar 过去;远程无制品库来源)。此前远程工作区只有纯源码,跨阶段产物传递断链。
func TestRemoteStageRestoresPriorArtifacts(t *testing.T) {
	st, err := artifactstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("artifactstore: %v", err)
	}
	key, _, err := st.Put(bytes.NewReader([]byte("PK-remote-jar-bytes")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	b := newRemoteTestBuilder(&recordingDriver{})
	b.artStore = st
	b.artifactLister = func(_ context.Context, _ string) ([]run.Artifact, error) {
		return []run.Artifact{{
			Name: "app.jar", Type: run.ArtifactJar, Reference: key,
			Metadata: map[string]any{"stored": true, "format": "file", "workspacePath": "target/app.jar"},
		}}, nil
	}
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{selector: "linux", pick: "srv-1"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	if err := exec(context.Background(), r, scriptStage(scriptJob("build", "node:20", "echo ok")), rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	// 恢复日志出现,且必须先于「打包工作区并经 SSH 传输」(先恢复再打包,产物才进 tar)。
	restoreIdx, packIdx := -1, -1
	for i, line := range rep.logs {
		if strings.Contains(line, "已恢复上游产物:app.jar") {
			restoreIdx = i
		}
		if strings.Contains(line, "打包工作区并经 SSH 传输") {
			packIdx = i
		}
	}
	if restoreIdx < 0 {
		t.Fatalf("远程路径应恢复上游产物;logs=%v", rep.logs)
	}
	if packIdx < 0 || restoreIdx > packIdx {
		t.Fatalf("产物恢复应先于打包上传;restore=%d pack=%d logs=%v", restoreIdx, packIdx, rep.logs)
	}
}

// lifecycleReporter 在 fakeReporter 之上记录 JobRunning(节点生命周期可见性断言)。
type lifecycleReporter struct {
	*fakeReporter
	running []string
}

func (r *lifecycleReporter) JobRunning(_ context.Context, jobID string) error {
	r.running = append(r.running, jobID)
	return nil
}

// TestRemoteNodeReportsJobLifecycle 证修复 3(上半):远程节点执行前 JobRunning、成功后
// JobDone(success)。此前远程节点从不上报,执行中 UI 一直 pending。
func TestRemoteNodeReportsJobLifecycle(t *testing.T) {
	b := newRemoteTestBuilder(&recordingDriver{})
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{selector: "linux", pick: "srv-1"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &lifecycleReporter{fakeReporter: &fakeReporter{}}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	jb := scriptJob("build", "node:20", "echo ok")
	jb.ID = "jr"
	if err := exec(context.Background(), r, scriptStage(jb), rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !reflect.DeepEqual(rep.running, []string{"jr"}) {
		t.Fatalf("远程节点应 JobRunning 一次,实际 %v", rep.running)
	}
	if !reflect.DeepEqual(rep.jobDone, []string{"jr=" + run.StepSuccess}) {
		t.Fatalf("远程节点应 JobDone(success),实际 %v", rep.jobDone)
	}
}

// panicUploadTarget 在上传工作区时 panic(模拟 runStageRemote 内部崩溃;worker 层有 recover)。
type panicUploadTarget struct{ fakeRemoteTarget }

func (p *panicUploadTarget) Upload(context.Context, string, io.Reader, string) error {
	panic("upload boom")
}

// TestRemoteSlotReleasedOnPanic 证修复 3(槽位):runStageRemote panic 时槽位也必须归还
// (闭包级 defer release),不再永久泄漏;panic 本身继续向上传播(由 worker 层 recover)。
func TestRemoteSlotReleasedOnPanic(t *testing.T) {
	b := newRemoteTestBuilder(&recordingDriver{})
	tgt := &panicUploadTarget{}
	fr := &fakeRunnerResolver{selector: "linux", pick: "srv-1"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)
	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("panic 应继续向上传播(worker 层 recover)")
		}
		if fr.releases != 1 {
			t.Fatalf("panic 后槽位必须归还,实际 releases=%d", fr.releases)
		}
	}()
	_ = exec(context.Background(), r, scriptStage(scriptJob("b", "alpine", "true")), rep)
}

// TestRemoteNodesSkippedWhenLocalSideFails 证修复 3(下半):本地侧失败时,未执行的远程
// 节点显式 JobDone(skipped)——不再被 finishRemaining 兜底成 failed;且不再占槽派发。
func TestRemoteNodesSkippedWhenLocalSideFails(t *testing.T) {
	local := &recordingDriver{code: 1} // 本地 script 节点非零退出 → 本地侧失败
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{pickBy: map[string]string{"server:srv-a": "srv-a"}}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	ja := scriptJobWithConfig("a", "node:20", "echo a", map[string]any{"runner": "server:srv-a"})
	ja.ID = "ja"
	jb := scriptJob("b", "node:20", "echo b")
	jb.ID = "jb"
	err := exec(context.Background(), r, scriptStage(ja, jb), rep)
	if !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("本地侧失败应令阶段失败,实际 %v", err)
	}
	if len(fr.acquired) != 0 {
		t.Fatalf("本地侧已失败,不应再派发远程节点,实际 Acquire %v", fr.acquired)
	}
	if !contains(rep.jobDone, "jb="+run.StepFailed) || !contains(rep.jobDone, "ja="+run.StepSkipped) {
		t.Fatalf("本地节点应 failed、未执行远程节点应 skipped;jobDone=%v", rep.jobDone)
	}
}

// orderTarget 在 fakeRemoteTarget 上记录远程 docker run 发生的顺序(跨组依赖/时序断言)。
type orderTarget struct {
	fakeRemoteTarget
	order *[]string
}

func (o *orderTarget) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	if len(cmd) >= 2 && cmd[0] == "docker" && cmd[1] == "run" {
		*o.order = append(*o.order, "remote:"+serverID)
	}
	return o.fakeRemoteTarget.Exec(ctx, serverID, cmd)
}

// orderDeployer 记录 DeployForStage 调用顺序(跨组依赖断言)。
type orderDeployer struct {
	stubStageDeployer
	order *[]string
}

func (d *orderDeployer) DeployForStage(ctx context.Context, runID, selector string, cfg map[string]string, strategy string) ([]deploy.TargetResult, error) {
	*d.order = append(*d.order, "deploy")
	return d.stubStageDeployer.DeployForStage(ctx, runID, selector, cfg, strategy)
}

// orderLocalDriver 记录本地 RunToolchain(本地 job 与 post 步骤)发生的顺序。
type orderLocalDriver struct {
	postRecDriver
	order *[]string
}

func (d *orderLocalDriver) RunToolchain(ctx context.Context, image, hostDir, workdir string, env, cmd []string, res pipeline.Resource, onLine func(string, string)) (int, error) {
	*d.order = append(*d.order, "local:"+image)
	return d.postRecDriver.RunToolchain(ctx, image, hostDir, workdir, env, cmd, res, onLine)
}

// failRunTarget 让远程 docker run 返回非零(模拟远程节点执行失败)。
type failRunTarget struct{ fakeRemoteTarget }

func (f *failRunTarget) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	if len(cmd) >= 2 && cmd[0] == "docker" && cmd[1] == "run" {
		_, _ = f.fakeRemoteTarget.Exec(ctx, serverID, cmd) // 仍记录命令
		return &target.ExecResult{ExitCode: 1}, nil
	}
	return f.fakeRemoteTarget.Exec(ctx, serverID, cmd)
}

// TestMixedStageLocalDeployNeedsRemoteBuildOrder 证修复 4(问题 a):本地 deploy 节点 needs
// 远程 build 节点时,必须按拓扑序先跑远程构建、再跑本地部署。此前本地侧无条件先行且 needs
// 被剔除,部署先于构建跑(依赖失效)。
func TestMixedStageLocalDeployNeedsRemoteBuildOrder(t *testing.T) {
	order := []string{}
	b := newRemoteTestBuilder(&recordingDriver{})
	tgt := &orderTarget{order: &order}
	b.deployer = &orderDeployer{order: &order}
	fr := &fakeRunnerResolver{pickBy: map[string]string{"server:srv-a": "srv-a"}}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	ja := scriptJobWithConfig("a", "node:20", "echo a", map[string]any{"runner": "server:srv-a"})
	ja.ID = "ja"
	jc := pipeline.Job{ID: "jc", Name: "c", Type: "deploy_ssh", Needs: []string{"ja"}, Config: map[string]any{"selector": "test"}}
	if err := exec(context.Background(), r, scriptStage(ja, jc), rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if want := []string{"remote:srv-a", "deploy"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("跨组依赖应按拓扑序执行(远程构建 → 本地部署),实际 %v", order)
	}
	if !contains(rep.jobDone, "ja="+run.StepSuccess) || !contains(rep.jobDone, "jc="+run.StepSuccess) {
		t.Fatalf("两节点都应成功;jobDone=%v", rep.jobDone)
	}
	if fr.releases != 1 {
		t.Fatalf("槽位应归还一次,实际 %d", fr.releases)
	}
}

// TestAllRemoteStagePostRunsOnController 证修复 4(问题 b·全远程):全部 script job 带选择器
// 时本地侧为空,post 此前永不执行;现提升到派发层,在远程节点完成后于控制机工作区执行,
// 且按合并成败选 condition(成功 → always + on_success,不跑 on_failure)。
func TestAllRemoteStagePostRunsOnController(t *testing.T) {
	order := []string{}
	drv := &orderLocalDriver{order: &order}
	b := newRemoteTestBuilder(drv)
	tgt := &orderTarget{order: &order}
	fr := &fakeRunnerResolver{selector: "linux", pick: "srv-1"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	stage := scriptStage(scriptJobWithConfig("a", "node:20", "echo a", map[string]any{"runner": "linux"}))
	stage.Post = postSet
	if err := exec(context.Background(), r, stage, rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	want := []string{"remote:srv-1", "local:post-always", "local:post-ok"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("全远程阶段 post 应在远程节点完成后于控制机执行,实际 %v", order)
	}
	if contains(drv.images, "post-fail") {
		t.Fatalf("阶段成功时 on_failure post 不应跑;images=%v", drv.images)
	}
}

// TestMixedStagePostRunsOnceAfterAllNodes 证修复 4(问题 b·混合):本地+远程混合阶段的 post
// 恰好执行一次(本地侧已剥离 post,不重复跑),且在远程节点完成之后。
func TestMixedStagePostRunsOnceAfterAllNodes(t *testing.T) {
	order := []string{}
	drv := &orderLocalDriver{order: &order}
	b := newRemoteTestBuilder(drv)
	tgt := &orderTarget{order: &order}
	fr := &fakeRunnerResolver{pickBy: map[string]string{"server:srv-a": "srv-a"}}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	ja := scriptJobWithConfig("a", "img-a", "echo a", map[string]any{"runner": "server:srv-a"})
	jb := scriptJob("b", "img-b", "echo b")
	stage := scriptStage(ja, jb)
	stage.Post = []pipeline.PostStep{{Condition: pipeline.PostAlways, Image: "post-cleanup", Commands: []string{"echo clean"}}}
	if err := exec(context.Background(), r, stage, rep); err != nil {
		t.Fatalf("exec: %v", err)
	}
	want := []string{"local:img-b", "remote:srv-a", "local:post-cleanup"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("混合阶段 post 应恰好一次且在全部节点之后,实际 %v", order)
	}
}

// TestAllRemoteStagePostMergedFailureCondition 证修复 4(post 成败条件合并·批量路径):
// 远程节点失败 → 阶段按失败论 → always + on_failure 跑,on_success 不跑。
func TestAllRemoteStagePostMergedFailureCondition(t *testing.T) {
	drv := &postRecDriver{}
	b := newRemoteTestBuilder(drv)
	tgt := &failRunTarget{}
	fr := &fakeRunnerResolver{selector: "linux", pick: "srv-1"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	stage := scriptStage(scriptJobWithConfig("a", "node:20", "echo a", map[string]any{"runner": "linux"}))
	stage.Post = postSet
	err := exec(context.Background(), r, stage, rep)
	if !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("远程节点失败应令阶段失败,实际 %v", err)
	}
	if !contains(drv.images, "post-always") || !contains(drv.images, "post-fail") {
		t.Fatalf("失败时 always+on_failure post 应跑;images=%v", drv.images)
	}
	if contains(drv.images, "post-ok") {
		t.Fatalf("失败时 on_success post 不应跑;images=%v", drv.images)
	}
	if !contains(rep.jobDone, "="+run.StepFailed) {
		t.Fatalf("远程节点应 JobDone(failed);jobDone=%v", rep.jobDone)
	}
}

// TestMixedTopoRemoteFailureSkipsDependents 证修复 4(拓扑路径跳过口径 + post 合并):
// 远程节点失败 → needs 它的本地节点 skipped(不执行)、阶段失败,post 按失败条件执行。
func TestMixedTopoRemoteFailureSkipsDependents(t *testing.T) {
	drv := &postRecDriver{}
	b := newRemoteTestBuilder(drv)
	tgt := &failRunTarget{}
	fr := &fakeRunnerResolver{pickBy: map[string]string{"server:srv-a": "srv-a"}}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	ja := scriptJobWithConfig("a", "node:20", "echo a", map[string]any{"runner": "server:srv-a"})
	ja.ID = "ja"
	jc := scriptJobID("jc", "img-local", "ja") // 本地节点 needs 远程节点
	stage := scriptStage(ja, jc)
	stage.Post = postSet
	err := exec(context.Background(), r, stage, rep)
	if !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("远程节点失败应令阶段失败,实际 %v", err)
	}
	if contains(drv.images, "img-local") {
		t.Fatalf("依赖失败远程节点的本地节点应 skipped 而非执行;images=%v", drv.images)
	}
	if !contains(rep.jobDone, "ja="+run.StepFailed) || !contains(rep.jobDone, "jc="+run.StepSkipped) {
		t.Fatalf("远程节点应 failed、下游本地节点应 skipped;jobDone=%v", rep.jobDone)
	}
	if !contains(drv.images, "post-fail") || contains(drv.images, "post-ok") {
		t.Fatalf("post 应按合并失败条件执行(on_failure 跑/on_success 不跑);images=%v", drv.images)
	}
}

// TestStageRemoteEchoesUnpackAndCleanupCommands 远程派发的解包 / 收尾清理命令(不经 driver 的
// 远程 Exec)也必须出现在步骤日志 —— 「流水线日志可见所有执行的命令」。
func TestStageRemoteEchoesUnpackAndCleanupCommands(t *testing.T) {
	local := &recordingDriver{}
	b := newRemoteTestBuilder(local)
	tgt := &fakeRemoteTarget{}
	fr := &fakeRunnerResolver{selector: "server:srv-1", pick: "srv-1"}
	exec := NewStageExecutorWithRunner(b, nil, fr, tgt)

	rep := &fakeReporter{}
	r := &run.Run{ID: "run-1", ProjectID: "p1", Trigger: run.Trigger{Branch: "main"}}
	if err := exec(context.Background(), r, scriptStage(scriptJob("build", "node:20", "npm ci")), rep); err != nil {
		t.Fatalf("remote exec: %v", err)
	}
	joined := strings.Join(rep.logs, "\n")
	if !strings.Contains(joined, "$ sh -c mkdir -p") || !strings.Contains(joined, "tar -xzf") {
		t.Fatalf("应回显远程解包命令, got:\n%s", joined)
	}
	if !strings.Contains(joined, "$ rm -rf /tmp/pipewright-remote/") {
		t.Fatalf("应回显远程工作区清理命令, got:\n%s", joined)
	}
}
