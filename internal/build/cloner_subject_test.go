package build

import "testing"

// TestCommitSubject:commit message → subject 首行(多行只留标题、CRLF 兼容、全空行回空串)。
// 运行详情「COMMIT 旁的 message」取的是这里的结果;空串时前端不渲染该行。
func TestCommitSubject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"单行", "fix: 修复回滚\n", "fix: 修复回滚"},
		{"多行取首行", "feat: 支持调宽\n\n正文忽略\n第二段", "feat: 支持调宽"},
		{"CRLF 兼容", "fix: windows\r\n\r\nbody", "fix: windows"},
		{"首行空白跳过", "\n\n  fix: 前置空行\nbody", "fix: 前置空行"},
		{"全空", "\n \n", ""},
		{"零值", "", ""},
	}
	for _, c := range cases {
		if got := CommitSubject(c.in); got != c.want {
			t.Errorf("%s: CommitSubject(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
