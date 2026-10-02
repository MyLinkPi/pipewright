package version

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsRepoDir(t *testing.T) {
	empty := t.TempDir()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	onlyGit := t.TempDir()
	if err := os.MkdirAll(filepath.Join(onlyGit, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		dir  string
		want bool
	}{
		{"", false},
		{empty, false},   // 空目录
		{repo, true},     // go.mod + .git
		{onlyGit, false}, // 只有 .git(裸仓库/其他项目)
	}
	for _, c := range cases {
		if got := isRepoDir(c.dir); got != c.want {
			t.Errorf("isRepoDir(%q) = %v,期望 %v", c.dir, got, c.want)
		}
	}
}

// SourceDir 的 env 通道:合法仓库返回路径;非法目录回落其他通道(测试环境 exe 不在仓库,得空)。
func TestSourceDir_Env(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PIPEWRIGHT_SOURCE_DIR", repo)
	if got := SourceDir(); got != repo {
		t.Errorf("合法仓库应原样返回,得 %q", got)
	}

	t.Setenv("PIPEWRIGHT_SOURCE_DIR", t.TempDir())
	if got := SourceDir(); got != "" {
		t.Errorf("非仓库目录应忽略并回落,得 %q", got)
	}
}

func TestMode_SourceExplicit(t *testing.T) {
	t.Setenv("PIPEWRIGHT_RUNTIME", "source")
	if Mode() != ModeSource {
		t.Errorf("PIPEWRIGHT_RUNTIME=source → 期望 ModeSource")
	}
}
