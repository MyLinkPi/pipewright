package easytier

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// TestMaskArgs 验证命令行打码:组网密钥/凭据的值替换为 ***(分离与内联两种形态),
// 其余参数(网络名、peers、网段…)原样保留 —— 日志要能看到完整命令。
func TestMaskArgs(t *testing.T) {
	cfg := Config{NetworkSecret: "super-secret", Credential: "cred-token"}
	in := []string{
		"/usr/bin/easytier-core",
		"--network-name", "mynet",
		"--network-secret", "super-secret",
		"--credential", "cred-token",
		"--network-secret=inline=secret",
		"-p", "tcp://1.2.3.4:11010",
		"--manual-routes", "10.5.0.0/16",
	}
	got := maskArgs(cfg, in)
	want := []string{
		"/usr/bin/easytier-core",
		"--network-name", "mynet",
		"--network-secret", "***",
		"--credential", "***",
		"--network-secret=***",
		"-p", "tcp://1.2.3.4:11010",
		"--manual-routes", "10.5.0.0/16",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("maskArgs = %v, want %v", got, want)
	}

	// 参数值内嵌敏感明文(如密钥出现在路径/其他值里)也要被替换。
	got = maskArgs(cfg, []string{"--hostname", "host-super-secret"})
	if strings.Contains(strings.Join(got, ""), "super-secret") {
		t.Fatalf("内嵌密钥未打码:%v", got)
	}
}

// TestScanOutputMasksSecrets 验证子进程输出逐行进日志且密钥明文被打码(含 stderr
// 文本里带凭据的场景)。
func TestScanOutputMasksSecrets(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer log.SetOutput(os.Stderr)

	secrets := Config{NetworkSecret: "super-secret", Credential: "cred-token"}.sensitive()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	done := make(chan struct{})
	go func() { scanOutput(pr, "stderr", secrets); close(done) }()

	_, _ = pw.WriteString("tunnel: established to peer super-secret\n")
	_, _ = pw.WriteString("credential cred-token rejected\n")
	_ = pw.Close() // 触发读端 EOF,scanOutput 退出
	<-done

	out := buf.String()
	if strings.Contains(out, "super-secret") || strings.Contains(out, "cred-token") {
		t.Fatalf("日志泄漏敏感明文:%s", out)
	}
	for _, want := range []string{"easytier-core(stderr): tunnel: established to peer ***", "easytier-core(stderr): credential *** rejected"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少打码行 %q,got:%s", want, out)
		}
	}
}

// TestSensitiveSkipsShort 验证过短值不参与全文替换(易误伤普通输出),仅靠旗子级打码。
func TestSensitiveSkipsShort(t *testing.T) {
	got := Config{NetworkSecret: "abc"}.sensitive()
	if len(got) != 0 {
		t.Fatalf("短密钥不应进全文替换列表:%v", got)
	}
}
