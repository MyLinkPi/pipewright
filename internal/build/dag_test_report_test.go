package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// fakeReportSink 记录 SaveTestReport 调用(替代真 run.Service)。
type fakeReportSink struct {
	saved []run.TestReport
}

func (s *fakeReportSink) SaveTestReport(_ context.Context, tr run.TestReport) (*run.TestReport, error) {
	s.saved = append(s.saved, tr)
	out := tr
	return &out, nil
}

// reportStage 构造一个声明了测试报告 + 门禁的阶段。
func reportStage(cfg map[string]any) pipeline.Stage {
	base := map[string]any{"image": "node:20", "commands": "echo run"}
	for k, v := range cfg {
		base[k] = v
	}
	return pipeline.Stage{ID: "s1", Name: "测试", Jobs: []pipeline.Job{{
		ID: "j1", Name: "test", Type: pipeline.StepTypeScript, Config: base,
	}}}
}

const junitPass = `<testsuites tests="3" failures="0" errors="0" skipped="0" time="1.0">
  <testsuite name="a" tests="3" failures="0" errors="0" skipped="0"/>
</testsuites>`

const junitFail = `<testsuites tests="3" failures="1" errors="0" skipped="0" time="1.0">
  <testsuite name="a" tests="3" failures="1" errors="0" skipped="0"/>
</testsuites>`

func writeReport(t *testing.T, ws, rel, content string) {
	t.Helper()
	p := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectStageReport_NoDeclaration(t *testing.T) {
	ws := t.TempDir()
	rep := &fakeReporter{}
	sink := &fakeReportSink{}
	stage := pipeline.Stage{ID: "s1", Name: "x", Jobs: []pipeline.Job{
		{Type: pipeline.StepTypeScript, Config: map[string]any{"image": "x", "commands": "y"}},
	}}
	if err := collectStageReport(context.Background(), sink, &run.Run{ID: "r1"}, stage, localReportReader(ws), rep); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(sink.saved) != 0 {
		t.Errorf("saved %d reports, want 0 (no declaration)", len(sink.saved))
	}
}

func TestCollectStageReport_PassPersists(t *testing.T) {
	ws := t.TempDir()
	writeReport(t, ws, "reports/junit.xml", junitPass)
	rep := &fakeReporter{}
	sink := &fakeReportSink{}
	stage := reportStage(map[string]any{
		"testReport": "junit", "reportPath": "reports/junit.xml", "gateMaxFailures": "0",
	})
	if err := collectStageReport(context.Background(), sink, &run.Run{ID: "r1"}, stage, localReportReader(ws), rep); err != nil {
		t.Fatalf("err = %v, want nil (gate passes)", err)
	}
	if len(sink.saved) != 1 {
		t.Fatalf("saved %d reports, want 1", len(sink.saved))
	}
	got := sink.saved[0]
	if got.Total != 3 || got.Passed != 3 || got.Failed != 0 {
		t.Errorf("summary = %+v, want total=3 passed=3 failed=0", got)
	}
	if !got.GateEnabled || !got.GatePassed {
		t.Errorf("gate = enabled=%v passed=%v, want both true", got.GateEnabled, got.GatePassed)
	}
}

func TestCollectStageReport_GateBlocks(t *testing.T) {
	ws := t.TempDir()
	writeReport(t, ws, "reports/junit.xml", junitFail)
	rep := &fakeReporter{}
	sink := &fakeReportSink{}
	stage := reportStage(map[string]any{
		"testReport": "junit", "reportPath": "reports/junit.xml", "gateMaxFailures": "0",
	})
	err := collectStageReport(context.Background(), sink, &run.Run{ID: "r1"}, stage, localReportReader(ws), rep)
	if !errors.Is(err, ErrQualityGate) {
		t.Fatalf("err = %v, want ErrQualityGate", err)
	}
	// 仍应持久化汇总(记录门禁不过)。
	if len(sink.saved) != 1 || sink.saved[0].GatePassed {
		t.Errorf("saved = %+v, want 1 report with GatePassed=false", sink.saved)
	}
}

func TestCollectStageReport_MissingFileWithGateBlocks(t *testing.T) {
	ws := t.TempDir() // 不写报告文件
	rep := &fakeReporter{}
	stage := reportStage(map[string]any{
		"testReport": "junit", "reportPath": "reports/junit.xml", "gateMaxFailures": "0",
	})
	err := collectStageReport(context.Background(), nil, &run.Run{ID: "r1"}, stage, localReportReader(ws), rep)
	if !errors.Is(err, ErrQualityGate) {
		t.Fatalf("err = %v, want ErrQualityGate (gate declared but no report)", err)
	}
}

func TestCollectStageReport_MissingFileNoGateSoftSkip(t *testing.T) {
	ws := t.TempDir()
	rep := &fakeReporter{}
	sink := &fakeReportSink{}
	stage := reportStage(map[string]any{
		"testReport": "junit", "reportPath": "reports/junit.xml", // 无门禁
	})
	if err := collectStageReport(context.Background(), sink, &run.Run{ID: "r1"}, stage, localReportReader(ws), rep); err != nil {
		t.Fatalf("err = %v, want nil (display-only, soft skip)", err)
	}
	if len(sink.saved) != 0 {
		t.Errorf("saved %d, want 0", len(sink.saved))
	}
}

func TestCollectStageReport_Coverage(t *testing.T) {
	ws := t.TempDir()
	writeReport(t, ws, "junit.xml", junitPass)
	writeReport(t, ws, "cobertura.xml", `<coverage line-rate="0.92"/>`)
	rep := &fakeReporter{}
	sink := &fakeReportSink{}
	stage := reportStage(map[string]any{
		"testReport": "junit", "reportPath": "junit.xml",
		"coverageReport": "cobertura", "coveragePath": "cobertura.xml",
		"gateMinCoverage": "80",
	})
	if err := collectStageReport(context.Background(), sink, &run.Run{ID: "r1"}, stage, localReportReader(ws), rep); err != nil {
		t.Fatalf("err = %v, want nil (92%% >= 80%%)", err)
	}
	if len(sink.saved) != 1 || sink.saved[0].Coverage < 91 || sink.saved[0].Coverage > 93 {
		t.Errorf("coverage = %v, want ~92", sink.saved[0].Coverage)
	}
}

func TestSafeJoinWorkspace_NoTraversal(t *testing.T) {
	got := safeJoinWorkspace("/ws", "../../etc/passwd")
	if got != filepath.Join("/ws", "etc", "passwd") {
		t.Errorf("safeJoinWorkspace traversal not contained: %q", got)
	}
}

// TestCollectStageReport_RemoteReaderContract:报告读取函数与文件系统解耦(远程构建机路径
// 经 SSH cat 把字节读回控制机)。内存 reader 模拟远程字节:失败报告 + 门禁 → 先照常采集落库、
// 再 ErrQualityGate 阻断;缺文件 + 声明门禁 → 同样阻断 —— 与本地路径同一套裁决。
func TestCollectStageReport_RemoteReaderContract(t *testing.T) {
	files := map[string][]byte{"reports/junit.xml": []byte(junitFail)}
	read := func(rel string) ([]byte, error) {
		if b, ok := files[rel]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
	stage := reportStage(map[string]any{
		"testReport": "junit", "reportPath": "reports/junit.xml", "gateMaxFailures": "0",
	})

	rep := &fakeReporter{}
	sink := &fakeReportSink{}
	err := collectStageReport(context.Background(), sink, &run.Run{ID: "r1"}, stage, read, rep)
	if !errors.Is(err, ErrQualityGate) {
		t.Fatalf("err = %v, want ErrQualityGate (1 失败 > 门禁 0)", err)
	}
	if len(sink.saved) != 1 || sink.saved[0].Failed != 1 {
		t.Fatalf("远程 reader 应照常采集并落库报告:%+v", sink.saved)
	}

	// 通过的报告 → 不阻断。
	files["reports/junit.xml"] = []byte(junitPass)
	if err := collectStageReport(context.Background(), sink, &run.Run{ID: "r1"}, stage, read, rep); err != nil {
		t.Fatalf("err = %v, want nil (通过报告)", err)
	}

	// 缺文件 + 门禁 → 阻断(诚实边界:声明了要求拿不到证据宁阻断)。
	delete(files, "reports/junit.xml")
	if err := collectStageReport(context.Background(), sink, &run.Run{ID: "r1"}, stage, read, rep); !errors.Is(err, ErrQualityGate) {
		t.Fatalf("err = %v, want ErrQualityGate", err)
	}
}

// TestRemoteJoinWorkspace:远程工作区路径拼接必须产 POSIX 斜杠(控制机可能是 Windows),
// 且 `..`/前导斜杠/反斜杠全部清洗,不逃出远程工作区根。
func TestRemoteJoinWorkspace(t *testing.T) {
	if got := remoteJoinWorkspace("/tmp/pw-remote/r1-s1", "reports/junit.xml"); got != "/tmp/pw-remote/r1-s1/reports/junit.xml" {
		t.Fatalf("普通拼接不符:%q", got)
	}
	if got := remoteJoinWorkspace("/tmp/ws", `..\..\etc\passwd`); got != "/tmp/ws/etc/passwd" {
		t.Fatalf("穿越未清洗:%q", got)
	}
	if got := remoteJoinWorkspace("/tmp/ws", "/abs/leading"); got != "/tmp/ws/abs/leading" {
		t.Fatalf("前导斜杠未归一:%q", got)
	}
}
