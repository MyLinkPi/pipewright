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

// ResolveMirror:生效源与来源标签一致(config > env > default),默认即 GitHub Releases 官方。
func TestResolveMirror(t *testing.T) {
	t.Setenv("PIPEWRIGHT_RELEASE_MIRROR", "")
	t.Setenv("PIPEWRIGHT_RELEASE_REPO", "")

	src, origin := ResolveMirror("")
	if origin != MirrorOriginDefault {
		t.Errorf("未配置应 default,得 %q", origin)
	}
	if src.APIBase != "https://api.github.com" || src.DLBase != "https://github.com" {
		t.Errorf("默认应为 GitHub Releases 官方地址,得 %+v", src)
	}

	t.Setenv("PIPEWRIGHT_RELEASE_MIRROR", "https://env.example.com")
	src, origin = ResolveMirror("")
	if origin != MirrorOriginEnv || src.APIBase != "https://env.example.com" {
		t.Errorf("env 兜底应生效,得 %q %+v", origin, src)
	}

	src, origin = ResolveMirror("https://db.example.com")
	if origin != MirrorOriginConfig || src.APIBase != "https://db.example.com" {
		t.Errorf("库内配置应优先,得 %q %+v", origin, src)
	}
}
