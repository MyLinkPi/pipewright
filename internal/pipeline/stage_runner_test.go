package pipeline

// stage_runner_test.go 直测阶段级构建机选择器(FR-8-19)的 normalize 行为:
// 合法选择器(标签/k=v/钉死)经 NormalizeSpec 保留不丢,非法语法归一 ErrInvalidStage。
// pipelineyaml 包另有端到端往返测试,本文件覆盖 pipeline 包内的规范化本体。

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeSpecPreservesStageRunner(t *testing.T) {
	cases := []string{
		"gpu",
		"linux,arch=arm64",
		"server:srv-1",
		" linux , arch=arm64 ", // 外围空白 trim;项内空白保留原样(与 runner 侧解析一致)
	}
	for _, in := range cases {
		out, err := NormalizeSpec(Spec{Stages: []Stage{
			{Name: "源", Kind: KindSource, Jobs: []Job{{Name: "s", Type: "git_source"}}},
			{Name: "构建", Kind: KindBuild, Runner: in, Jobs: []Job{}},
		}})
		if err != nil {
			t.Fatalf("NormalizeSpec(%q): %v", in, err)
		}
		want := strings.TrimSpace(in)
		if out.Stages[1].Runner != want {
			t.Errorf("Runner 经规范化应保留: in=%q got=%q want=%q", in, out.Stages[1].Runner, want)
		}
	}
}

func TestNormalizeSpecRejectsBadStageRunner(t *testing.T) {
	cases := map[string]string{
		"空值键":    "arch=",
		"空值项":    "linux,,gpu",
		"非法字符":   "gpu;rm -rf",
		"点开头":    ".hidden",
		"钉死空 id": "server:",
		"钉死坏 id": "server:bad id",
		"超长":     strings.Repeat("a", 256),
		"项数超限":   "a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q",
	}
	for name, in := range cases {
		_, err := NormalizeSpec(Spec{Stages: []Stage{
			{Name: "源", Kind: KindSource, Jobs: []Job{{Name: "s", Type: "git_source"}}},
			{Name: "构建", Kind: KindBuild, Runner: in, Jobs: []Job{}},
		}})
		if !errors.Is(err, ErrInvalidStage) {
			t.Errorf("%s(%q)应报 ErrInvalidStage, 得 %v", name, in, err)
		}
	}
}

// TestValidateSelectorTermCountDeduped 锁项数口径与 runner.ParseSelector 一致(去重后计数):
// 17 个重复项(去重后 ≤16)应放行;17 个互异项(去重后仍超限)应拒绝。
func TestValidateSelectorTermCountDeduped(t *testing.T) {
	// 17 个重复项:去重后仅 1 项,runner 侧合法,保存侧不应再 422。
	dup := "gpu,gpu,gpu,gpu,gpu,gpu,gpu,gpu,gpu,gpu,gpu,gpu,gpu,gpu,gpu,gpu,gpu"
	if err := validateSelectorSyntax(dup); err != nil {
		t.Errorf("17 个重复项(去重后 1 项)应放行, 得 %v", err)
	}
	// 混合:16 个互异项 + 重复项 → 去重后 16 项,恰上限放行。
	sixteen := "a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,p,p"
	if err := validateSelectorSyntax(sixteen); err != nil {
		t.Errorf("去重后恰 16 项应放行, 得 %v", err)
	}
	// 17 个互异项:去重后仍超限 → 拒绝。
	seventeen := "a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q"
	if err := validateSelectorSyntax(seventeen); !errors.Is(err, errSelectorTerms) {
		t.Errorf("17 个互异项应报 errSelectorTerms, 得 %v", err)
	}
}

func TestNormalizeSpecJobRunner(t *testing.T) {
	base := func(jobs []Job) Spec {
		return Spec{Stages: []Stage{
			{Name: "源", Kind: KindSource, Jobs: []Job{{Name: "s", Type: "git_source"}}},
			{Name: "构建", Kind: KindBuild, Jobs: jobs},
		}}
	}

	// 合法:合法选择器逐字保留;空串/缺失 = 未覆盖。
	ok, err := NormalizeSpec(base([]Job{
		{Name: "a", Type: "script", Config: map[string]any{"runner": " linux,arch=arm64 "}},
		{Name: "b", Type: "script", Config: map[string]any{"runner": "server:srv-1"}},
		{Name: "c", Type: "script", Config: map[string]any{"runner": ""}},
		{Name: "d", Type: "script", Config: map[string]any{}},
	}))
	if err != nil {
		t.Fatalf("legal job runner: %v", err)
	}
	if got := ok.Stages[1].Jobs[0].Config["runner"]; got != "linux,arch=arm64" {
		t.Fatalf("runner 应 trim 保留,得 %v", got)
	}
	if got := ok.Stages[1].Jobs[1].Config["runner"]; got != "server:srv-1" {
		t.Fatalf("钉死形式应保留,得 %v", got)
	}

	// 非法:语法错 → ErrInvalidJob;非字符串 → ErrInvalidJob。
	for name, bad := range map[string]any{
		"非法字符":  "gpu;rm -rf",
		"空值键":   "arch=",
		"钉死空id": "server:",
		"超长":    strings.Repeat("a", 256),
		"非字符串":  42,
	} {
		_, err := NormalizeSpec(base([]Job{{Name: "a", Type: "script", Config: map[string]any{"runner": bad}}}))
		if !errors.Is(err, ErrInvalidJob) {
			t.Errorf("%s(%v) 应报 ErrInvalidJob, 得 %v", name, bad, err)
		}
	}
}
