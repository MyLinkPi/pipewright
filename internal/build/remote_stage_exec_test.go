package build

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

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

func (f *fakeRunnerResolver) Acquire(_ context.Context, _, selector string, _ func(string)) (string, func(), error) {
	f.acquired = append(f.acquired, selector)
	if f.acquireErr != nil {
		return "", nil, f.acquireErr
	}
	pick := f.pick
	if f.pickBy != nil {
		pick = f.pickBy[selector]
	}
	return pick, func() { f.releases++ }, nil
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
}
