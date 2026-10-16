// Package submoduletest 提供 .gitmodules/gitlink 测试夹具:纯 go-git 构造,不依赖宿主 git。
//
// 关键手法:go-git 的 Worktree.Commit 从 **index** 建树(worktree_commit.go BuildTree 把
// index 条目的 Mode/Hash 原样拷进 tree 条目),因此手工往 index 塞一条 160000
// (filemode.Submodule) 条目再提交,就得到真 gitlink——无需 git submodule add,也无需
// file:// 裸仓。夹具仓库以本地目录路径作克隆 URL(go-git PlainClone 直接支持)。
package submoduletest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// SubDecl 声明一个子模块。SHA 为超仓库 index 里 pinned 的 gitlink commit;空 = 仅在
// .gitmodules 声明、不注册进 index(对应 git 里"未 add 的子模块",Populate 应跳过)。
type SubDecl struct {
	Path string // 工作区相对路径(同时作 .gitmodules 的 name)
	URL  string
	SHA  string
}

var testSig = &object.Signature{Name: "t", Email: "t@t", When: time.Now()}

// MakeCommits 造一个普通仓库:每个 map 一轮提交(键值 → 文件名/内容)。
// 返回仓库路径与各提交 sha(按提交顺序,末位是 HEAD)。
func MakeCommits(t *testing.T, commits ...map[string]string) (string, []string) {
	t.Helper()
	return MakeCommitsAt(t, t.TempDir(), commits...)
}

// MakeCommitsAt 是 MakeCommits 的落位版本(仓库建在 dir,如相对子模块 URL 需与超仓库
// 同级摆放的测试)。
func MakeCommitsAt(t *testing.T, dir string, commits ...map[string]string) (string, []string) {
	t.Helper()
	if len(commits) == 0 {
		t.Fatal("至少一轮提交")
	}
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	shas := make([]string, 0, len(commits))
	for i, files := range commits {
		for name, content := range files {
			full := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			if _, err := wt.Add(name); err != nil {
				t.Fatalf("add %s: %v", name, err)
			}
		}
		h, cerr := wt.Commit(fmt.Sprintf("c%d", i+1), &gogit.CommitOptions{Author: testSig})
		if cerr != nil {
			t.Fatalf("commit: %v", cerr)
		}
		shas = append(shas, h.String())
	}
	return dir, shas
}

// MakeRepo 造一个超仓库:普通文件 + 子模块声明(.gitmodules 文件 + gitlink index 条目)。
// 返回仓库路径(作克隆 URL)与 HEAD 完整 sha。subs 为空则不写 .gitmodules。
func MakeRepo(t *testing.T, files map[string]string, subs map[string]SubDecl) (string, string) {
	t.Helper()
	return MakeRepoAt(t, t.TempDir(), files, subs)
}

// MakeRepoAt 是 MakeRepo 的落位版本(仓库建在 dir)。
func MakeRepoAt(t *testing.T, dir string, files map[string]string, subs map[string]SubDecl) (string, string) {
	t.Helper()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	add := func(name, content string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	for name, content := range files {
		add(name, content)
	}

	if len(subs) > 0 {
		var gm strings.Builder
		for _, s := range subs {
			// path/url 用 %q:git-config 语法里反斜杠是转义符,Windows 路径须转义后引号包裹。
			fmt.Fprintf(&gm, "[submodule %q]\n\tpath = %q\n\turl = %q\n", s.Path, s.Path, s.URL)
		}
		add(".gitmodules", gm.String())

		// 手工塞 gitlink index 条目(160000 模式);Commit 建树时原样进 tree。
		idx, ierr := repo.Storer.Index()
		if ierr != nil {
			t.Fatalf("index: %v", ierr)
		}
		for _, s := range subs {
			if s.SHA == "" {
				continue // 仅声明不注册
			}
			idx.Entries = append(idx.Entries, &index.Entry{
				Name: s.Path,
				Mode: filemode.Submodule,
				Hash: plumbing.NewHash(s.SHA),
			})
		}
		if serr := repo.Storer.SetIndex(idx); serr != nil {
			t.Fatalf("set index: %v", serr)
		}
	}

	h, cerr := wt.Commit("init", &gogit.CommitOptions{Author: testSig})
	if cerr != nil {
		t.Fatalf("commit: %v", cerr)
	}
	return dir, h.String()
}
