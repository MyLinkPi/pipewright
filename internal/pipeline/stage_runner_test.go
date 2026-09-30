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
