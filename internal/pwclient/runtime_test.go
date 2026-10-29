// runtime_test.go 覆盖 core 启动后的 RPC portal 端口确认(confirmRPCPort):
// 开放/未开放/ctx 取消三种结局的日志行为(注入探测与节拍,不触真机端口)。
package pwclient

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// rpcTestTiming 把确认节拍缩到毫秒级,返回 restore。
func rpcTestTiming(t *testing.T) {
	t.Helper()
	wait, interval, timeout := rpcPortWait, rpcPortProbeInterval, rpcPortProbeTimeout
	rpcPortWait, rpcPortProbeInterval, rpcPortProbeTimeout = 0, 5*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { rpcPortWait, rpcPortProbeInterval, rpcPortProbeTimeout = wait, interval, timeout })
}

// syncLogBuf 是加锁日志缓冲(-race 下轮询读与 log 写不竞态)。
type syncLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureRPCLog 把全局 log 重定向到内存缓冲(测试串行执行,安全)。
func captureRPCLog(t *testing.T) *syncLogBuf {
	t.Helper()
	buf := &syncLogBuf{}
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(0)
	})
	return buf
}

func TestConfirmRPCPort(t *testing.T) {
	cases := []struct {
		name   string
		probe  func(addr string) error
		cancel bool
		want   string // 期望日志含子串("" = 不应有任何结论行)
		deny   string // 期望日志不含子串
	}{
		{name: "端口开放", probe: func(string) error { return nil }, want: "RPC 端口已开放: 127.0.0.1:15889", deny: "未开放"},
		{name: "始终拒连", probe: func(string) error { return errors.New("connect: connection refused") },
			want: "警告:EasyTier RPC 端口未开放: 127.0.0.1:15889", deny: "已开放"},
		{name: "ctx 取消即退出", probe: func(string) error { return errors.New("refused") }, cancel: true, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rpcTestTiming(t)
			origProbe := rpcPortProbe
			rpcPortProbe = c.probe
			t.Cleanup(func() { rpcPortProbe = origProbe })
			buf := captureRPCLog(t)

			ctx, cancel := context.WithCancel(context.Background())
			if c.cancel {
				cancel() // 已取消:循环首屏 select 立即退出,不产出结论
			} else {
				defer cancel()
			}
			rt, _ := newTestRuntime(t)
			done := make(chan struct{})
			go func() { defer close(done); rt.confirmRPCPort(ctx, "127.0.0.1:15889") }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("confirmRPCPort 未按预期退出")
			}
			out := buf.String()
			if c.want == "" {
				if out != "" {
					t.Fatalf("取消场景不应有结论日志:%s", out)
				}
				return
			}
			if !strings.Contains(out, c.want) {
				t.Fatalf("日志缺 %q:\n%s", c.want, out)
			}
			if c.deny != "" && strings.Contains(out, c.deny) {
				t.Fatalf("日志不应含 %q:\n%s", c.deny, out)
			}
		})
	}
}
