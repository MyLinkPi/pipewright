package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// bootstrap 随机口令路径的行为测试:库为唯一事实源,文件是随机口令的唯一投递渠道。

func TestBootstrapRandomPassword(t *testing.T) {
	st := storetest.Open(t)
	t.Cleanup(func() { _ = st.Close() })
	svc := NewService(st.DB, nil)

	pwdFile := filepath.Join(t.TempDir(), "admin_password.txt")
	if err := svc.Bootstrap("admin", "", pwdFile); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	raw, err := os.ReadFile(pwdFile)
	if err != nil {
		t.Fatalf("password file not written: %v", err)
	}
	password := strings.TrimSpace(string(raw))
	if len(password) != passwordLen {
		t.Fatalf("password length = %d, want %d", len(password), passwordLen)
	}

	// 文件里的口令必须能登录(哈希入库与文件明文一致)。
	if _, err := svc.Login("admin", password); err != nil {
		t.Fatalf("login with generated password: %v", err)
	}

	// 0600:仅属主可读(Windows 无 POSIX 权限位,跳过)。
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(pwdFile); err == nil && fi.Mode().Perm() != 0o600 {
			t.Fatalf("password file mode = %o, want 600", fi.Mode().Perm())
		}
	}
}

func TestBootstrapExistingAdminKeepsPasswordFile(t *testing.T) {
	st := storetest.Open(t)
	t.Cleanup(func() { _ = st.Close() })
	svc := NewService(st.DB, nil)

	pwdFile := filepath.Join(t.TempDir(), "admin_password.txt")
	if err := svc.Bootstrap("admin", "", pwdFile); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	before, err := os.ReadFile(pwdFile)
	if err != nil {
		t.Fatalf("read password file: %v", err)
	}

	// 已有 admin 后再跑 Bootstrap(重启场景):env 口令被忽略,文件不被改写。
	if err := svc.Bootstrap("admin", "some-env-password", pwdFile); err != nil {
		t.Fatalf("re-bootstrap: %v", err)
	}
	after, err := os.ReadFile(pwdFile)
	if err != nil {
		t.Fatalf("re-read password file: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("password file rewritten after admin already exists")
	}
	// env 口令不得生效。
	if _, err := svc.Login("admin", "some-env-password"); err == nil {
		t.Fatal("env password must be ignored when admin exists")
	}
	if _, err := svc.Login("admin", strings.TrimSpace(string(after))); err != nil {
		t.Fatalf("original password must still work: %v", err)
	}
}

func TestBootstrapEnvPasswordNoFile(t *testing.T) {
	st := storetest.Open(t)
	t.Cleanup(func() { _ = st.Close() })
	svc := NewService(st.DB, nil)

	dir := t.TempDir()
	pwdFile := filepath.Join(dir, "admin_password.txt")
	if err := svc.Bootstrap("admin", "env-pass-123", pwdFile); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// env 口令已在用户手里,不额外落盘。
	if _, err := os.Stat(pwdFile); !os.IsNotExist(err) {
		t.Fatal("password file must not be written when env password provided")
	}
	if _, err := svc.Login("admin", "env-pass-123"); err != nil {
		t.Fatalf("login with env password: %v", err)
	}
}

func TestBootstrapRandomWithoutFileFails(t *testing.T) {
	st := storetest.Open(t)
	t.Cleanup(func() { _ = st.Close() })
	svc := NewService(st.DB, nil)

	// 无口令且无落盘路径:随机口令无处投递,应报错而非创建不可知口令的 admin。
	if err := svc.Bootstrap("admin", "", ""); err == nil {
		t.Fatal("expected error when random password has no file path")
	}
	// 库不应被写入(报错发生在入库前)。
	var count int
	if err := st.DB.QueryRow(`SELECT COUNT(1) FROM admin_user`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("admin created despite error, count = %d", count)
	}
}

func TestGeneratePasswordAlphabet(t *testing.T) {
	seen := make(map[rune]bool)
	for i := 0; i < 200; i++ {
		p, err := GeneratePassword()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(p) != passwordLen {
			t.Fatalf("length = %d, want %d", len(p), passwordLen)
		}
		for _, r := range p {
			if !strings.ContainsRune(passwordAlphabet, r) {
				t.Fatalf("char %q outside alphabet", r)
			}
			seen[r] = true
		}
	}
	// 字符集覆盖完整性(200×24 次采样后应覆盖全部 55 个字符;剔除混淆字符后无 0/O/1/l/I)。
	if len(seen) != len(passwordAlphabet) {
		t.Fatalf("alphabet coverage = %d/%d", len(seen), len(passwordAlphabet))
	}
	for _, bad := range "0O1lI" {
		if strings.ContainsRune(passwordAlphabet, bad) {
			t.Fatalf("confusable char %q in alphabet", bad)
		}
	}
}
