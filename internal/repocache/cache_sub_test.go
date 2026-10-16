package repocache

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/huangchengsir/pipewright/internal/submodule/submoduletest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// TestCloneSubmodulesFromMirror:镜像命中路径(本地镜像出工作区)同样递归拉取子模块
// 并检出 pinned 版本——镜像只含超仓库对象,子模块走真实 URL。
func TestCloneSubmodulesFromMirror(t *testing.T) {
	subDir, shas := submoduletest.MakeCommits(t,
		map[string]string{"lib.txt": "v1"},
		map[string]string{"lib.txt": "v2"},
	)
	super, _ := submoduletest.MakeRepo(t,
		map[string]string{"main.txt": "hello"},
		map[string]submoduletest.SubDecl{"dep": {Path: "dep", URL: subDir, SHA: shas[0]}},
	)

	c := newTestCache(t)
	ws := filepath.Join(t.TempDir(), "ws")
	if _, err := c.Clone(context.Background(), super, vault.GitAuth{}, "", "", ws); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	b, rerr := os.ReadFile(filepath.Join(ws, "dep", "lib.txt"))
	if rerr != nil {
		t.Fatalf("子模块未拉取: %v", rerr)
	}
	if string(b) != "v1" {
		t.Fatalf("dep/lib.txt = %q, want pinned 版本 %q", b, "v1")
	}
}

// TestCloneSubmodulesRelativeURL:相对子模块 URL(./dep.git)必须按**真实远端地址**解析
// (而非本地镜像路径,否则会去镜像目录里找 dep.git)。
func TestCloneSubmodulesRelativeURL(t *testing.T) {
	base := t.TempDir()
	_, shas := submoduletest.MakeCommitsAt(t, filepath.Join(base, "dep.git"),
		map[string]string{"lib.txt": "v1"})
	super, _ := submoduletest.MakeRepoAt(t, filepath.Join(base, "super"), nil,
		map[string]submoduletest.SubDecl{"dep": {Path: "dep", URL: "./dep.git", SHA: shas[0]}})

	c := newTestCache(t)
	ws := filepath.Join(t.TempDir(), "ws")
	if _, err := c.Clone(context.Background(), super, vault.GitAuth{}, "", "", ws); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, rerr := os.ReadFile(filepath.Join(ws, "dep", "lib.txt")); rerr != nil {
		t.Fatalf("相对 URL 子模块未按真实远端解析拉取: %v", rerr)
	}
}
