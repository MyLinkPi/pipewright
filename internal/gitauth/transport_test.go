package gitauth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/huangchengsir/pipewright/internal/vault"
	"golang.org/x/crypto/ssh"
)

// newTestSSHKey 生成一把测试专用 Ed25519 OpenSSH 私钥(无 passphrase;每次随机,
// 绝无真实权限),验证解析路径不依赖任何固定密钥材料。
func newTestSSHKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成 ed25519 密钥: %v", err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal 私钥: %v", err)
	}
	var buf bytes.Buffer
	if err := pem.Encode(&buf, blk); err != nil {
		t.Fatalf("encode PEM: %v", err)
	}
	return buf.String()
}

func TestTransportAuthEmptySecretAnonymous(t *testing.T) {
	for _, u := range []string{"https://gitee.com/a/b.git", "git@gitee.com:a/b.git"} {
		a, err := TransportAuth(u, vault.GitAuth{Type: vault.TypeGitToken, Secret: "  "})
		if a != nil || err != nil {
			t.Fatalf("%s: 空凭据应为匿名(nil,nil), got (%v,%v)", u, a, err)
		}
	}
}

func TestTransportAuthHTTPSBranch(t *testing.T) {
	// git_token / git_http / 未登记类型(空)→ BasicAuth(历史行为)
	for _, typ := range []string{vault.TypeGitToken, vault.TypeGitHTTP, ""} {
		a, err := TransportAuth("https://gitee.com/a/b.git", vault.GitAuth{Type: typ, Username: "u", Secret: "tok"})
		if err != nil {
			t.Fatalf("type=%s: %v", typ, err)
		}
		if a == nil || a.Name() != "http-basic-auth" {
			t.Fatalf("type=%s: 应得 BasicAuth, got %#v", typ, a)
		}
	}
	// Gitee 用户名规则不变:gitee.com + 显式 username → 用之。
	a, _ := TransportAuth("https://gitee.com/a/b.git", vault.GitAuth{Type: vault.TypeGitToken, Username: "realuser", Secret: "tok"})
	if a == nil || !strings.Contains(a.String(), "realuser") {
		t.Fatalf("Gitee 应取显式用户名: %v", a)
	}
}

func TestTransportAuthSSHKey(t *testing.T) {
	for _, u := range []string{
		"ssh://git@gitee.com:22/a/b.git",
		"git@gitee.com:a/b.git",
	} {
		a, err := TransportAuth(u, vault.GitAuth{Type: vault.TypeSSHKey, Username: "", Secret: newTestSSHKey(t)})
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		pk, isPK := a.(*gogitssh.PublicKeys)
		if !isPK {
			t.Fatalf("%s: 应得 *ssh.PublicKeys, got %#v", u, a)
		}
		// 回归:go-git 不会用 URL user 段补空用户名(整体覆盖 ClientConfig),
		// 空用户名会导致服务端直接拒绝认证(Codeup 实测)。必须回退 "git"。
		if pk.User != "git" {
			t.Fatalf("%s: 空凭据用户名应回退为 git, got %q", u, pk.User)
		}
		if pk.HostKeyCallback == nil {
			t.Fatalf("%s: HostKeyCallback 不应为 nil(nil 会在 connect 时读 known_hosts 失败)", u)
		}
	}
	// 显式凭据用户名优先于 URL user 段。
	a, _ := TransportAuth("git@gitee.com:a/b.git", vault.GitAuth{Type: vault.TypeSSHKey, Username: "deployer", Secret: newTestSSHKey(t)})
	if pk := a.(*gogitssh.PublicKeys); pk.User != "deployer" {
		t.Fatalf("显式 username 应优先, got %q", pk.User)
	}
}

func TestTransportAuthSSHPassword(t *testing.T) {
	a, err := TransportAuth("git@host.example:a/b.git", vault.GitAuth{Type: vault.TypeSSHPassword, Username: "git", Secret: "pw"})
	if err != nil {
		t.Fatalf("ssh_password: %v", err)
	}
	if _, isPW := a.(*gogitssh.Password); !isPW {
		t.Fatalf("应得 *ssh.Password, got %#v", a)
	}
}

func TestTransportAuthSchemeMismatch(t *testing.T) {
	cases := []struct {
		url  string
		cred vault.GitAuth
	}{
		{"https://gitee.com/a/b.git", vault.GitAuth{Type: vault.TypeSSHKey, Secret: newTestSSHKey(t)}},
		{"http://gitee.com/a/b.git", vault.GitAuth{Type: vault.TypeSSHPassword, Secret: "pw"}},
		{"ssh://git@gitee.com/a/b.git", vault.GitAuth{Type: vault.TypeGitToken, Secret: "tok"}},
		{"git@gitee.com:a/b.git", vault.GitAuth{Type: vault.TypeGitHTTP, Username: "u", Secret: "pw"}},
	}
	for _, c := range cases {
		_, err := TransportAuth(c.url, c.cred)
		if !errors.Is(err, ErrSchemeMismatch) {
			t.Fatalf("%s + %s 应 ErrSchemeMismatch, got %v", c.url, c.cred.Type, err)
		}
	}
}

func TestTransportAuthInvalidKey(t *testing.T) {
	_, err := TransportAuth("git@host.example:a/b.git", vault.GitAuth{Type: vault.TypeSSHKey, Secret: "not a pem"})
	if !errors.Is(err, ErrInvalidSSHKey) {
		t.Fatalf("坏私钥应 ErrInvalidSSHKey, got %v", err)
	}
}

func TestTransportAuthBadURL(t *testing.T) {
	_, err := TransportAuth("file:///etc/passwd", vault.GitAuth{Type: vault.TypeGitToken, Secret: "tok"})
	if !errors.Is(err, ErrRepoBlocked) {
		t.Fatalf("file:// 应 ErrRepoBlocked, got %v", err)
	}
}

func TestIsSSHURL(t *testing.T) {
	cases := map[string]bool{
		"ssh://git@host/repo.git": true,
		"SSH://git@host/repo.git": true,
		"git@host:repo.git":       true,
		"host:path/repo.git":      true, // 无 user 的 scp 风格(git 同语义)
		"git@host:22:repo.git":    true,
		"https://host/repo.git":   false,
		"http://host/repo.git":    false,
		"git@host/repo.git":       false, // 无冒号:非 scp 风格
		"some/plain/path":         false,
		"file:///tmp/repo.git":    false,
		"git@host.example.com:/a": true,
		"git@host.example.com:22": true,
	}
	for u, want := range cases {
		if got := IsSSHURL(u); got != want {
			t.Errorf("IsSSHURL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestValidateRepoURL(t *testing.T) {
	allowed := []string{
		"https://gitee.com/a/b.git",
		"http://10.0.0.5/a/b.git",    // 私网放行
		"https://192.168.1.10/b.git", // 私网放行
		"ssh://git@10.0.0.6/a/b.git", // ssh + 私网放行
		"ssh://git@gitee.com:2222/b.git",
		"git@gitee.com:a/b.git", // SCP 风格
		"gitee.com:a/b.git",     // 无 user 的 scp 风格
	}
	for _, u := range allowed {
		if err := ValidateRepoURL(u); err != nil {
			t.Errorf("%s 应放行, got %v", u, err)
		}
	}
	blocked := []string{
		"",                           // 空
		"file:///etc/passwd",         // scheme 不允许
		"git://host/repo.git",        // git:// 不支持
		"ftp://host/x",               // 非 git 协议
		"https://127.0.0.1/repo.git", // 回环
		"http://169.254.169.254/x",   // 云元数据(链路本地)
		"http://[::1]/repo.git",      // IPv6 回环
		"ssh://git@127.0.0.1/b.git",  // ssh + 回环同样拒
		"git@127.0.0.1:a/b.git",      // scp 风格 + 回环
		"some/plain/path",            // 既非 URL 也非 scp
		"ssh://",                     // 无 host
	}
	for _, u := range blocked {
		if err := ValidateRepoURL(u); err == nil {
			t.Errorf("%s 应被拒", u)
		}
	}
}

func TestParseRepoURL(t *testing.T) {
	cases := []struct {
		in                 string
		scheme, host, user string
		ok                 bool
	}{
		{"https://gitee.com/a/b.git", "https", "gitee.com", "", true},
		{"ssh://git@host.example:2222/a.git", "ssh", "host.example", "git", true},
		{"git@host.example:a/b.git", "ssh", "host.example", "git", true},
		{"host.example:a/b.git", "ssh", "host.example", "", true},
		{"file:///tmp/x", "", "", "", false},
		{"plain/path", "", "", "", false},
	}
	for _, c := range cases {
		scheme, host, user, ok := ParseRepoURL(c.in)
		if ok != c.ok || scheme != c.scheme || host != c.host || user != c.user {
			t.Errorf("ParseRepoURL(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				c.in, scheme, host, user, ok, c.scheme, c.host, c.user, c.ok)
		}
	}
}
