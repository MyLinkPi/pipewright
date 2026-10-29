package easytier

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf 是加锁日志缓冲(避免 -race 下轮询读与 log 写竞态)。
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSuperviseLogsCommandAndOutput 端到端验证:日志含完整命令行(密钥/凭据打码、
// 其余旗子原样)与子进程 stdout/stderr 全文,且任何一行都不出现敏感明文。
func TestSuperviseLogsCommandAndOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 sh 伪二进制,Windows 跳过")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-core.sh")
	// 伪二进制:--help 探测(Start 前经 probeHelp 跑一次)只列非敏感旗子并退 0;
	// 真启动时把密钥/凭据明文写进 stdout(模拟输出泄密),stderr 写一行普通日志,
	// 命令行尾两个位置参数就是注入的密钥/凭据。
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--help\" ]; then echo '--manual-routes --socks5 --secure-mode'; exit 0; fi\n" +
		"tail=$(printf '%s\n' \"$@\" | tail -2)\n" +
		"echo \"tunnel established $tail\"\n" +
		"echo \"listener ready on 11010\" >&2\n" +
		"exit 7\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("写伪二进制:%v", err)
	}

	var buf syncBuf
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer log.SetOutput(os.Stderr)

	cfg := Config{Mode: "managed", CorePath: script, NetworkName: "net",
		NetworkSecret: "super-secret", Credential: "cred-token",
		RPCAddr: "127.0.0.1:15899", Hostname: "h1", VirtualIP: "10.0.0.1",
		ManualRoutes: []string{"10.5.0.0/16"}}
	// 伪脚本按尾部位置参取密钥/凭据:经 argsFn 追加到命令行尾(buildArgs 不感知脚本形态,
	// 凭据/密钥的原生旗子形态另由 maskArgs 单测覆盖)。
	origArgs := cfg.Args()
	s := New(cfg)
	s.argsFn = func() []string {
		return append(append([]string{}, origArgs...), cfg.NetworkSecret, cfg.Credential)
	}
	if err := s.Start(t.Context()); err != nil {
		t.Fatalf("start:%v", err)
	}
	t.Cleanup(s.Stop)

	deadline := time.Now().Add(10 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = buf.String()
		if strings.Contains(out, "easytier-core(stderr): listener ready") &&
			strings.Contains(out, "easytier-core(stdout): tunnel established") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if strings.Contains(out, "super-secret") || strings.Contains(out, "cred-token") {
		t.Fatalf("日志泄漏敏感明文:\n%s", out)
	}
	for _, want := range []string{
		"easytier: 启动",
		"--credential ***",            // 命令行凭据旗子值打码(尾部位置参也打码)
		"--manual-routes 10.5.0.0/16", // 非敏感参数完整保留
		"--hostname h1",
		"--network-name net",
		"listener ready on 11010",                       // stderr 全文进日志
		"easytier-core(stdout): tunnel established ***", // stdout 明文密钥打码
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("日志缺少 %q,实际:\n%s", want, out)
		}
	}
}
