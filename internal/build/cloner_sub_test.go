package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/submodule/submoduletest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// TestClonerClonesSubmodules 端到端:Clone 超仓库后应递归拉取子模块并检出 pinned 版本。
func TestClonerClonesSubmodules(t *testing.T) {
	subDir, shas := submoduletest.MakeCommits(t,
		map[string]string{"lib.txt": "v1"},
		map[string]string{"lib.txt": "v2"},
	)
	super, _ := submoduletest.MakeRepo(t,
		map[string]string{"main.txt": "hello"},
		map[string]submoduletest.SubDecl{"dep": {Path: "dep", URL: subDir, SHA: shas[0]}},
	)

	c := &Cloner{allowInsecure: true} // 本地路径夹具
	dest := t.TempDir()
	res, err := c.Clone(context.Background(), super, vault.GitAuth{}, "", "", dest)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if res.CommitShort == "" {
		t.Fatal("CommitShort 为空")
	}
	b, rerr := os.ReadFile(filepath.Join(dest, "dep", "lib.txt"))
	if rerr != nil {
		t.Fatalf("子模块未拉取: %v", rerr)
	}
	if string(b) != "v1" {
		t.Fatalf("dep/lib.txt = %q, want pinned 版本 %q", b, "v1")
	}
}

// TestClonerSubmoduleSSRFBlocked:.gitmodules 声明的子模块 URL 命中云元数据地址 →
// 生产模式(非 allowInsecure)整次克隆按 ErrRepoBlocked 失败。
func TestClonerSubmoduleSSRFBlocked(t *testing.T) {
	super, _ := submoduletest.MakeRepo(t, nil, map[string]submoduletest.SubDecl{
		"dep": {Path: "dep", URL: "http://169.254.169.254/latest/meta-data", SHA: strings.Repeat("1", 40)},
	})
	c := NewCloner()
	dest := t.TempDir()
	_, err := c.Clone(context.Background(), super, vault.GitAuth{}, "", "", dest)
	if !errors.Is(err, ErrRepoBlocked) {
		t.Fatalf("err = %v, want ErrRepoBlocked", err)
	}
}
