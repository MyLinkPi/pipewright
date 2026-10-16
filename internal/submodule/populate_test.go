package submodule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/huangchengsir/pipewright/internal/submodule/submoduletest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

func TestResolveSubmoduleURL(t *testing.T) {
	cases := []struct {
		name, parent, sub, want string
	}{
		{"绝对 https 原样", "https://g.example/group/super.git", "https://other.com/lib.git", "https://other.com/lib.git"},
		{"绝对 SCP 原样", "https://g.example/group/super.git", "git@other.com:team/lib.git", "git@other.com:team/lib.git"},
		{"./ 同目录", "https://g.example/group/super.git", "./lib.git", "https://g.example/group/lib.git"},
		{"../ 上级", "https://g.example/group/sub/super.git", "../lib.git", "https://g.example/group/lib.git"},
		{"../../ 跨两级", "https://g.example/group/sub/super.git", "../../lib.git", "https://g.example/lib.git"},
		{"ssh:// 父保留端口", "ssh://git@g.example:2222/group/super.git", "../lib.git", "ssh://git@g.example:2222/lib.git"},
		{"SCP 父拼接", "git@g.example:group/sub/super.git", "../../lib.git", "git@g.example:lib.git"},
		{"file:// 父拼接", "file:///srv/repos/super.git", "./lib.git", "file:///srv/repos/lib.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveSubmoduleURL(tc.parent, tc.sub); got != tc.want {
				t.Fatalf("resolve(%q, %q) = %q, want %q", tc.parent, tc.sub, got, tc.want)
			}
		})
	}
}

func TestResolveSubmoduleURLLocalParent(t *testing.T) {
	// 本地路径父(测试夹具/镜像路径形态):按目录拼接。
	base := t.TempDir()
	parent := filepath.Join(base, "a", "super.git")
	if got := resolveSubmoduleURL(parent, "../dep.git"); got != filepath.Join(base, "dep.git") {
		t.Fatalf("resolve 本地父 = %q, want %q", got, filepath.Join(base, "dep.git"))
	}
	// 子模块是绝对本地路径 → 原样返回(不与父拼接)。
	abs := filepath.Join(base, "lib.git")
	if got := resolveSubmoduleURL(parent, abs); got != abs {
		t.Fatalf("绝对本地子路径应原样: %q", got)
	}
}

func TestSubmoduleAuth(t *testing.T) {
	tokenCred := vault.GitAuth{Type: vault.TypeGitToken, Username: "user", Secret: "tok"}
	cases := []struct {
		name        string
		sub, parent string
		cred        vault.GitAuth
		wantAuth    bool
		wantBasic   bool // wantAuth 且为 BasicAuth
	}{
		{"空凭据匿名", "https://h.example/lib.git", "https://h.example/super.git",
			vault.GitAuth{Type: vault.TypeGitToken}, false, false},
		{"同 host 转发", "https://h.example/lib.git", "https://h.example/group/super.git", tokenCred, true, true},
		{"跨 host 匿名", "https://evil.example/lib.git", "https://h.example/super.git", tokenCred, false, false},
		{"同 host 协议错配降级匿名", "git@h.example:lib.git", "https://h.example/super.git", tokenCred, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := submoduleAuth(tc.sub, tc.parent, tc.cred)
			if !tc.wantAuth {
				if got != nil {
					t.Fatalf("应匿名,得到 %T", got)
				}
				return
			}
			if got == nil {
				t.Fatal("应转发凭据,得到 nil")
			}
			if tc.wantBasic {
				if _, ok := got.(*githttp.BasicAuth); !ok {
					t.Fatalf("应为 BasicAuth,得到 %T", got)
				}
			}
		})
	}
}

func TestSafeSubPath(t *testing.T) {
	base := filepath.Join(string(filepath.Separator), "ws")
	cases := []struct {
		path   string
		wantOK bool
	}{
		{"dep", true},
		{"a/b/c", true},
		{"a/../b", true}, // 清洗后仍在工作区内
		{"", false},
		{".", false},
		{"..", false},
		{"../evil", false},
		{"a/../../evil", false},
	}
	for _, tc := range cases {
		got, ok := safeSubPath(base, tc.path)
		if ok != tc.wantOK {
			t.Fatalf("safeSubPath(%q) ok=%v, want %v(got=%q)", tc.path, ok, tc.wantOK, got)
		}
		if ok && !strings.HasPrefix(got, base) {
			t.Fatalf("safeSubPath(%q) = %q, 越出 %q", tc.path, got, base)
		}
	}
}

func TestAllowedSubmoduleURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://192.168.1.10/org/lib.git", true},         // 私网放行(自托管内网 Git)
		{"git@192.168.1.10:org/lib.git", true},             // SCP 私网
		{"http://169.254.169.254/latest/meta-data", false}, // 云元数据
		{"ssh://git@127.0.0.1/lib.git", false},             // 回环
		{`C:\Users\someone\secret`, false},                 // Windows 盘符本地路径(宿主目录外泄通道)
		{"/etc/passwd", false},                             // 绝对本地路径(无 scheme/SCP,ParseRepoURL 拒)
	}
	for _, tc := range cases {
		if got := allowedSubmoduleURL(tc.url); got != tc.want {
			t.Fatalf("allowedSubmoduleURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

// cloneFixture 把超仓库浅克隆到工作区(Populate 的前置:克隆+检出完成)。
func cloneFixture(t *testing.T, super string) string {
	t.Helper()
	ws := t.TempDir()
	if _, err := gogit.PlainCloneContext(context.Background(), ws, false, &gogit.CloneOptions{
		URL:  super,
		Tags: gogit.NoTags,
	}); err != nil {
		t.Fatalf("clone super: %v", err)
	}
	return ws
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestPopulatePinnedSubmodule(t *testing.T) {
	// 子仓库推两次;超仓库 pin 在第一版 → 检出的必须是 v1 而非 HEAD 的 v2。
	subDir, shas := submoduletest.MakeCommits(t,
		map[string]string{"lib.txt": "v1"},
		map[string]string{"lib.txt": "v2"},
	)
	super, head := submoduletest.MakeRepo(t,
		map[string]string{"main.txt": "hello"},
		map[string]submoduletest.SubDecl{"dep": {Path: "dep", URL: subDir, SHA: shas[0]}},
	)
	if head == "" {
		t.Fatal("夹具 HEAD 为空")
	}

	ws := cloneFixture(t, super)
	if err := Populate(context.Background(), ws, super, vault.GitAuth{}, true); err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if got := readFile(t, filepath.Join(ws, "dep", "lib.txt")); got != "v1" {
		t.Fatalf("dep/lib.txt = %q, want pinned 版本 %q", got, "v1")
	}
}

func TestPopulateNestedSubmodules(t *testing.T) {
	leaf, leafShas := submoduletest.MakeCommits(t, map[string]string{"leaf.txt": "L"})
	mid, midHead := submoduletest.MakeRepo(t, nil,
		map[string]submoduletest.SubDecl{"dep": {Path: "dep", URL: leaf, SHA: leafShas[0]}})
	super, _ := submoduletest.MakeRepo(t,
		map[string]string{"top.txt": "T"},
		map[string]submoduletest.SubDecl{"mid": {Path: "mid", URL: mid, SHA: midHead}},
	)

	ws := cloneFixture(t, super)
	if err := Populate(context.Background(), ws, super, vault.GitAuth{}, true); err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if got := readFile(t, filepath.Join(ws, "mid", "dep", "leaf.txt")); got != "L" {
		t.Fatalf("mid/dep/leaf.txt = %q, want %q", got, "L")
	}
}

func TestPopulateBlockedBySSRF(t *testing.T) {
	super, _ := submoduletest.MakeRepo(t, nil, map[string]submoduletest.SubDecl{
		"dep": {Path: "dep", URL: "http://169.254.169.254/latest/meta-data", SHA: strings.Repeat("1", 40)},
	})
	ws := cloneFixture(t, super)
	err := Populate(context.Background(), ws, super, vault.GitAuth{}, false)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
}

func TestPopulateSkipsUnregisteredSubmodule(t *testing.T) {
	// .gitmodules 声明了但 index 无 gitlink(git 未 add)→ 跳过而非报错。
	subDir, _ := submoduletest.MakeCommits(t, map[string]string{"lib.txt": "v1"})
	super, _ := submoduletest.MakeRepo(t,
		map[string]string{"main.txt": "hello"},
		map[string]submoduletest.SubDecl{"ghost": {Path: "ghost", URL: subDir}}, // SHA 空
	)
	ws := cloneFixture(t, super)
	if err := Populate(context.Background(), ws, super, vault.GitAuth{}, true); err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(ws, "ghost")); !os.IsNotExist(serr) {
		t.Fatal("未注册的子模块不应被拉取")
	}
}

func TestPopulateNoSubmodules(t *testing.T) {
	super, _ := submoduletest.MakeRepo(t, map[string]string{"a.txt": "x"}, nil)
	ws := cloneFixture(t, super)
	if err := Populate(context.Background(), ws, super, vault.GitAuth{}, false); err != nil {
		t.Fatalf("无 .gitmodules 应秒回 nil,得到 %v", err)
	}
}
