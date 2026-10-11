package ai

import (
	"strings"
	"testing"
)

// git_source 是通用节点(支持 GitHub / Gitee / GitLab 等任一 git 平台),
// 目录标签必须是「流水线源」——写成某个具体代码托管平台名会误导 LLM 与前端展示。
func TestBuiltinNodeCatalogSourceLabel(t *testing.T) {
	var label string
	for _, n := range BuiltinNodeCatalog() {
		if n.Type == "git_source" {
			label = n.Label
		}
	}
	if label != "流水线源" {
		t.Fatalf("git_source 标签 = %q, want 流水线源", label)
	}
}

// 目录不得把任何内置节点误标为具体托管平台名「Gitee」。
func TestBuiltinNodeCatalogNoPlatformName(t *testing.T) {
	for _, n := range BuiltinNodeCatalog() {
		if strings.Contains(n.Label, "Gitee") {
			t.Fatalf("节点 %q 的标签 %q 不该含平台名 Gitee", n.Type, n.Label)
		}
	}
}
