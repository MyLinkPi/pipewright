package build

// ai_node_test.go 覆盖 ai_test 执行器:glob 匹配器、发现 0 需求即失败(防静默绿灯)、
// 裁决一律 advisory(节点不因裁决失败)、报告同构落库、AI 未配置 fail/skip 两档、
// 服务未注入跳过(specs/ai-test-node.md)。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/huangchengsir/pipewright/internal/ai"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// fakeAITester 记录 AgentTest 入参并回放配置的结果/错误。
type fakeAITester struct {
	gotIn  ai.AITestInput
	result *ai.AITestResult
	err    error
}

func (f *fakeAITester) AgentTest(_ context.Context, in ai.AITestInput) (*ai.AITestResult, error) {
	f.gotIn = in
	return f.result, f.err
}

// fakeAITestSink 记录落库的测试报告。
type fakeAITestSink struct{ saved []run.TestReport }

func (f *fakeAITestSink) SaveTestReport(_ context.Context, tr run.TestReport) (*run.TestReport, error) {
	f.saved = append(f.saved, tr)
	out := tr
	return &out, nil
}

// aiTestSourceDir 造一个带需求文件的临时仓库根。
func aiTestSourceDir(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func aiTestJobWith(cfg map[string]any) pipeline.Job {
	if cfg == nil {
		cfg = map[string]any{}
	}
	return pipeline.Job{ID: "a1", Name: "AI 验收", Type: "ai_test", Config: cfg}
}

func aiTestRun() *run.Run {
	return &run.Run{ID: "r-ai", Trigger: run.Trigger{Params: map[string]string{"BASE_URL": "staging.example.com"}}}
}

func TestAITestGlobMatcher(t *testing.T) {
	m, err := aiTestGlobMatcher("")
	if err != nil || !m.MatchString("web/checkout.md") {
		t.Fatalf("缺省 glob 应匹配任意 md:%v %v", m, err)
	}
	m, _ = aiTestGlobMatcher(".pipewright/tests/requirements/**/*.md")
	if !m.MatchString("web/checkout.md") {
		t.Fatal("全路径前缀应被剥掉后匹配")
	}
	m, _ = aiTestGlobMatcher("**/smoke/**")
	if !m.MatchString("smoke/login.md") || m.MatchString("web/checkout.md") {
		t.Fatal("smoke 子集过滤不符")
	}
}

func TestRunAITestJobZeroRequirementsFails(t *testing.T) {
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	b.aiTester = &fakeAITester{result: &ai.AITestResult{}}
	root := aiTestSourceDir(t, map[string]string{
		"README.md": "无关文件",
	})
	rep := &fakeReporter{}
	err := b.runAITestJob(context.Background(), rep, aiTestJobWith(nil), aiTestRun(), scriptStage(aiTestJobWith(nil)), localAITestSource{root: root}, nil)
	if !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("0 需求应判失败(防路径 typo 静默绿灯),得到:%v", err)
	}
	if !containsLog(rep, "发现 0 个需求") {
		t.Fatalf("应日志「发现 0 个需求」:%v", rep.logs)
	}
}

func TestRunAITestJobAdvisoryAndReport(t *testing.T) {
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	tester := &fakeAITester{result: &ai.AITestResult{
		Model: "test-model",
		Sessions: []ai.AITestSessionResult{
			{Requirement: ai.AITestRequirement{ID: "web-checkout", Severity: ai.AITestSeverityBlocker},
				Verdict: &ai.AITestVerdict{Pass: false, Reason: "无提示", Evidence: "未见文案"}, Rounds: 3, InputTokens: 100, OutputTokens: 20, ConsumedAmount: 0.5},
		},
		Failed: 1, TotalRounds: 3, InputTokens: 100, OutputTokens: 20, ConsumedAmount: 0.5,
	}}
	b.aiTester = tester
	root := aiTestSourceDir(t, map[string]string{
		".pipewright/tests/requirements/web/checkout.md": "登录测试账号,加购结算,期望金额立减。",
	})
	sink := &fakeAITestSink{}
	rep := &fakeReporter{}
	job := aiTestJobWith(map[string]any{"target": "http://${BASE_URL}"})
	stage := scriptStage(job)
	err := b.runAITestJob(context.Background(), rep, job, aiTestRun(), stage, localAITestSource{root: root}, sink)
	if err != nil {
		t.Fatalf("裁决一律 advisory:失败裁决不应令节点失败,得到:%v", err)
	}
	// 需求发现与目标解析(${BASE_URL} 已展开为节点 target;需求缺省继承)。
	if len(tester.gotIn.Requirements) != 1 || tester.gotIn.Requirements[0].Target != "http://staging.example.com" {
		t.Fatalf("需求发现/目标继承不符:%+v", tester.gotIn.Requirements)
	}
	// 裁决呈现 + 用量费用日志。
	if !containsLog(rep, "❌ web-checkout") || !containsLog(rep, "花费") {
		t.Fatalf("应呈现失败裁决与费用:%v", rep.logs)
	}
	// 同构落库:Format=ai-acceptance、GateEnabled=false(机器不放行)。
	if len(sink.saved) != 1 || sink.saved[0].Format != "ai-acceptance" || sink.saved[0].GateEnabled || sink.saved[0].Total != 1 || sink.saved[0].Failed != 1 {
		t.Fatalf("报告落库不符:%+v", sink.saved)
	}
	if sink.saved[0].StageName != stage.Name {
		t.Fatalf("报告应带阶段名,得到 %q", sink.saved[0].StageName)
	}
}

func TestRunAITestJobAINotConfigured(t *testing.T) {
	root := aiTestSourceDir(t, map[string]string{
		".pipewright/tests/requirements/x.md": "正文",
	})
	// fail(缺省):无可信报告即停。
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	b.aiTester = &fakeAITester{err: ai.ErrAINotConfigured}
	rep := &fakeReporter{}
	failJob := aiTestJobWith(map[string]any{"target": "http://t.example.com"})
	if err := b.runAITestJob(context.Background(), rep, failJob, aiTestRun(), scriptStage(failJob), localAITestSource{root: root}, nil); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("onAiUnavailable=fail(缺省)应失败,得到:%v", err)
	}
	// skip:记「未测试」放行。
	b2 := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	b2.aiTester = &fakeAITester{err: ai.ErrAINotConfigured}
	rep2 := &fakeReporter{}
	job := aiTestJobWith(map[string]any{"onAiUnavailable": "skip", "target": "http://t.example.com"})
	if err := b2.runAITestJob(context.Background(), rep2, job, aiTestRun(), scriptStage(job), localAITestSource{root: root}, nil); err != nil {
		t.Fatalf("onAiUnavailable=skip 应放行:%v", err)
	}
	if !containsLog(rep2, "未测试") {
		t.Fatalf("skip 应日志「未测试」:%v", rep2.logs)
	}
}

func TestRunAITestJobNilTesterSkips(t *testing.T) {
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	rep := &fakeReporter{}
	job := aiTestJobWith(nil)
	if err := b.runAITestJob(context.Background(), rep, job, aiTestRun(), scriptStage(job), localAITestSource{root: t.TempDir()}, nil); err != nil {
		t.Fatalf("未注入应跳过:%v", err)
	}
	if !containsLog(rep, "未注入,跳过") {
		t.Fatalf("应日志「未注入,跳过」:%v", rep.logs)
	}
}

func TestRunAITestJobTargetTwoLevelResolution(t *testing.T) {
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	tester := &fakeAITester{result: &ai.AITestResult{}}
	b.aiTester = tester
	root := aiTestSourceDir(t, map[string]string{
		".pipewright/tests/requirements/a.md": "---\ntarget: http://override.example.com\n---\n正文A",
		".pipewright/tests/requirements/b.md": "正文B(继承节点 target)",
	})
	job := aiTestJobWith(map[string]any{"target": "http://node.example.com"})
	rep := &fakeReporter{}
	if err := b.runAITestJob(context.Background(), rep, job, aiTestRun(), scriptStage(job), localAITestSource{root: root}, nil); err != nil {
		t.Fatalf("执行失败:%v", err)
	}
	byPath := map[string]string{}
	for _, r := range tester.gotIn.Requirements {
		byPath[r.Path] = r.Target
	}
	if byPath[".pipewright/tests/requirements/a.md"] != "http://override.example.com" {
		t.Fatalf("需求 target 应覆盖节点:%+v", byPath)
	}
	if byPath[".pipewright/tests/requirements/b.md"] != "http://node.example.com" {
		t.Fatalf("缺省应继承节点 target:%+v", byPath)
	}
	// 两级皆空 → 发现期失败。
	root2 := aiTestSourceDir(t, map[string]string{".pipewright/tests/requirements/c.md": "正文C"})
	b2 := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	b2.aiTester = &fakeAITester{result: &ai.AITestResult{}}
	if err := b2.runAITestJob(context.Background(), &fakeReporter{}, aiTestJobWith(nil), aiTestRun(), scriptStage(aiTestJobWith(nil)), localAITestSource{root: root2}, nil); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("两级皆空应失败,得到:%v", err)
	}
}

func TestRunAITestJobUnresolvedPlaceholderFails(t *testing.T) {
	// ${VAR} 未解析(运行参数缺失/deployJob 未绑定)时字面量不该交给 agent——fail fast。
	b := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	tester := &fakeAITester{result: &ai.AITestResult{}}
	b.aiTester = tester
	root := aiTestSourceDir(t, map[string]string{
		".pipewright/tests/requirements/a.md": "正文",
	})
	job := aiTestJobWith(map[string]any{"target": "http://${MISSING_VAR}/api"})
	rep := &fakeReporter{}
	if err := b.runAITestJob(context.Background(), rep, job, aiTestRun(), scriptStage(job), localAITestSource{root: root}, nil); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("未解析占位符应 fail fast,得到:%v", err)
	}
	if !containsLog(rep, "未解析占位符") {
		t.Fatalf("应日志「未解析占位符」:%v", rep.logs)
	}
	// 已解析的占位符照常放行(aiTestRun 提供 BASE_URL)。
	b2 := newDAGTestBuilder(&recordingDriver{}, &markerCloner{})
	tester2 := &fakeAITester{result: &ai.AITestResult{}}
	b2.aiTester = tester2
	job2 := aiTestJobWith(map[string]any{"target": "http://${BASE_URL}/api"})
	if err := b2.runAITestJob(context.Background(), &fakeReporter{}, job2, aiTestRun(), scriptStage(job2), localAITestSource{root: root}, nil); err != nil {
		t.Fatalf("已解析占位符应放行:%v", err)
	}
	if tester2.gotIn.Requirements[0].Target != "http://staging.example.com/api" {
		t.Fatalf("占位符应展开:%q", tester2.gotIn.Requirements[0].Target)
	}
}

// fakeBridgeCLI 回放 docker port 输出序列(前 N 次空,之后给端口行)。
type fakeBridgeCLI struct {
	calls  int
	script []string // 每次调用的 stdout 行;空串 = 无输出
	codes  []int
}

func (f *fakeBridgeCLI) AITestStream(_ context.Context, _ []string, onLine func(string, string)) (int, error) {
	i := f.calls
	f.calls++
	if i >= len(f.script) {
		return 0, nil
	}
	if f.script[i] != "" {
		onLine("stdout", f.script[i])
	}
	code := 0
	if i < len(f.codes) {
		code = f.codes[i]
	}
	return code, nil
}

func TestAITestContainerHostPort(t *testing.T) {
	// 立即返回端口行。
	cli := &fakeBridgeCLI{script: []string{"127.0.0.1:34567"}}
	p, err := aiTestContainerHostPort(context.Background(), cli, "c")
	if err != nil || p != 34567 {
		t.Fatalf("应解析出 34567,得到 %d err=%v", p, err)
	}
	// 前两次空输出(启动竞态),第三次给出 → 重试成功。
	cli2 := &fakeBridgeCLI{script: []string{"", "", "127.0.0.1:49152"}}
	p2, err2 := aiTestContainerHostPort(context.Background(), cli2, "c")
	if err2 != nil || p2 != 49152 {
		t.Fatalf("重试后应解析出 49152,得到 %d err=%v", p2, err2)
	}
	if cli2.calls != 3 {
		t.Fatalf("应恰好在第 3 次拿到端口,调用 %d 次", cli2.calls)
	}
	// 一直无输出 → 报错(调用方降级为纯 HTTP)。
	if _, err3 := aiTestContainerHostPort(context.Background(), &fakeBridgeCLI{}, "c"); err3 == nil {
		t.Fatal("始终查不到端口应报错")
	}
}
