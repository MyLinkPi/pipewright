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

// CandidateSources:未配置镜像 → GitHub 官方 + gh-proxy 兜底;显式镜像 → 仅该镜像。
func TestCandidateSources(t *testing.T) {
	t.Setenv("PIPEWRIGHT_RELEASE_REPO", "")
	t.Setenv("PIPEWRIGHT_RELEASE_MIRROR", "")

	srcs := CandidateSources("")
	if len(srcs) != 2 {
		t.Fatalf("未配置镜像应得 2 个候选源,得 %d: %+v", len(srcs), srcs)
	}
	if srcs[0].APIBase != githubAPIBase || srcs[0].DLBase != githubWebBase {
		t.Errorf("第一候选应为 GitHub 官方,得 %+v", srcs[0])
	}
	if srcs[1].APIBase != "https://gh-proxy.com/https://api.github.com" ||
		srcs[1].WebBase != "https://gh-proxy.com/https://github.com" ||
		srcs[1].DLBase != "https://gh-proxy.com/https://github.com" {
		t.Errorf("第二候选应为 gh-proxy 前缀式代理,得 %+v", srcs[1])
	}

	// 显式配置镜像(config):只走镜像,不回落公共代理。
	if got := CandidateSources("https://mirror.example.com"); len(got) != 1 ||
		got[0].APIBase != "https://mirror.example.com" {
		t.Errorf("显式镜像应独占,得 %+v", got)
	}
	// 显式配置成 gh-proxy:同样独占,不重复追加同源。
	if got := CandidateSources(FallbackPrefixProxy); len(got) != 1 {
		t.Errorf("显式 gh-proxy 不应重复追加兜底源,得 %+v", got)
	}
	// env 镜像也算显式配置。
	t.Setenv("PIPEWRIGHT_RELEASE_MIRROR", "https://env.example.com")
	if got := CandidateSources(""); len(got) != 1 || got[0].APIBase != "https://env.example.com" {
		t.Errorf("env 镜像应独占,得 %+v", got)
	}
}
