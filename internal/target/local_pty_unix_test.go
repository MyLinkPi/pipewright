//go:build linux || darwin || freebsd || dragonfly || netbsd || openbsd

package target

// 类 Unix 平台的本机 PTY 交互会话测试(容器内 `docker exec -it` 的本地等价物)。

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestLocalPTYInteractive 本机 PTY:命令输出可读、Resize 不报错、Close 幂等。
func TestLocalPTYInteractive(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := svc.ExecInteractive(ctx, LocalHostID, []string{"sh", "-c", "echo pty-ok"})
	if err != nil {
		t.Fatalf("ExecInteractive: %v", err)
	}
	if err := sess.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	// 进程退出后 PTY 主端读会以 EIO 结束 —— 数据已拿到,忽略该 error。
	raw, _ := io.ReadAll(sess)
	if !strings.Contains(string(raw), "pty-ok") {
		t.Fatalf("pty output = %q", raw)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
}

// TestLocalPTYRoundTrip 双向:向本机 sh 写命令并读回真实执行结果(与 PTY 回显区分)。
func TestLocalPTYRoundTrip(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := svc.ExecInteractive(ctx, LocalHostID, []string{"sh"})
	if err != nil {
		t.Fatalf("ExecInteractive: %v", err)
	}
	defer sess.Close()

	if _, err := sess.Write([]byte("echo MARK-$((40+2))\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var got strings.Builder
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, rerr := sess.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
		}
		// PTY 回显只会含 "MARK-$((40+2))";展开后的 "MARK-42" 必是真实执行结果。
		if strings.Contains(got.String(), "MARK-42") {
			return
		}
		if rerr != nil {
			break
		}
	}
	t.Fatalf("pty output = %q, want MARK-42", got.String())
}
