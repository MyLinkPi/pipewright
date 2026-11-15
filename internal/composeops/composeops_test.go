package composeops

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/huangchengsir/pipewright/internal/target"
)

type deployServiceFake struct {
	target.Service
	stderr   string   // 每次 up 命令固定以该 stderr 失败(旧用例形态)
	upStderr []string // up 逐次脚本:第 n 次为非空则失败(ExitCode 1),为空或越界则成功
	upCalls  int
	onUp     func() // 非空时每次 up 命令后回调(模拟执行期间取消请求上下文)
}

func (f *deployServiceFake) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	if cmd[0] == "mkdir" || (len(cmd) >= 3 && cmd[0] == "docker" && cmd[1] == "compose" && cmd[2] == "version") {
		return &target.ExecResult{}, nil
	}
	f.upCalls++ // up 命令
	if f.onUp != nil {
		f.onUp()
	}
	if len(f.upStderr) > 0 {
		if n := f.upCalls; n <= len(f.upStderr) && f.upStderr[n-1] != "" {
			return &target.ExecResult{ExitCode: 1, Stderr: f.upStderr[n-1]}, nil
		}
		return &target.ExecResult{ExitCode: 0, Stdout: "Started"}, nil
	}
	return &target.ExecResult{ExitCode: 1, Stderr: f.stderr}, nil
}

func (f *deployServiceFake) Upload(ctx context.Context, serverID string, content io.Reader, remotePath string) error {
	return nil
}

func TestDeployFailureKeepsErrorTail(t *testing.T) {
	svc := &deployServiceFake{stderr: "early pull detail\n" + strings.Repeat("拉取镜像中...\n", 100) + "Error: root cause"}

	res := Deploy(context.Background(), svc, "srv-1", "demo", "services: {}\n")
	if res.OK || !res.Dispatched {
		t.Fatalf("部署结果不符: %+v", res)
	}
	if !strings.Contains(res.Err, "Error: root cause") {
		t.Fatalf("失败日志应保留末尾根因: %q", res.Err)
	}
	if strings.Contains(res.Err, "early pull detail") {
		t.Fatalf("超限日志不应继续保留开头: %q", res.Err)
	}
	if !strings.HasPrefix(res.Err, "…(前文已截断，仅保留末尾日志)\n") {
		t.Fatalf("应明确标注日志已截断: %q", res.Err)
	}
}

func TestDeployRetriesTransientPullFailure(t *testing.T) {
	old := upRetryBackoffs
	upRetryBackoffs = [maxUpAttempts - 1]time.Duration{}
	defer func() { upRetryBackoffs = old }()

	const transient = "failed to copy: read tcp 192.168.70.160:41732->150.171.69.10:443: read: connection reset by peer"
	svc := &deployServiceFake{upStderr: []string{transient}}

	res := Deploy(context.Background(), svc, "srv-1", "demo", "services: {}\n")
	if !res.OK || !res.Dispatched {
		t.Fatalf("瞬态失败一次后重试应成功: %+v", res)
	}
	if svc.upCalls != 2 {
		t.Fatalf("up 应执行 2 次,实际 %d 次", svc.upCalls)
	}
	if !strings.Contains(res.Out, "自动重试 1 次成功") {
		t.Fatalf("成功输出应注明自动重试: %q", res.Out)
	}
}

func TestDeployRetriesExhaustedOnTransientPullFailure(t *testing.T) {
	old := upRetryBackoffs
	upRetryBackoffs = [maxUpAttempts - 1]time.Duration{}
	defer func() { upRetryBackoffs = old }()

	const transient = "failed to copy: read tcp 10.0.0.1:1234->150.171.69.10:443: read: connection reset by peer"
	svc := &deployServiceFake{upStderr: []string{transient, transient, transient}}

	res := Deploy(context.Background(), svc, "srv-1", "demo", "services: {}\n")
	if res.OK || !res.Dispatched {
		t.Fatalf("重试耗尽后应失败且标记已投递: %+v", res)
	}
	if svc.upCalls != maxUpAttempts {
		t.Fatalf("up 应执行 %d 次,实际 %d 次", maxUpAttempts, svc.upCalls)
	}
	for _, want := range []string{"已自动重试 2 次", "connection reset by peer", "registry-mirrors"} {
		if !strings.Contains(res.Err, want) {
			t.Fatalf("失败信息应包含 %q: %q", want, res.Err)
		}
	}
}

func TestDeployRetryStopsOnContextCanceledDuringBackoff(t *testing.T) {
	old := upRetryBackoffs
	upRetryBackoffs = [maxUpAttempts - 1]time.Duration{time.Hour} // 走到退避等待即说明 ctx 取消未生效
	defer func() { upRetryBackoffs = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const transient = "failed to copy: read tcp 10.0.0.1:1234->150.171.69.10:443: read: connection reset by peer"
	svc := &deployServiceFake{upStderr: []string{transient}, onUp: cancel}

	res := Deploy(ctx, svc, "srv-1", "demo", "services: {}\n")
	if res.OK || !res.Dispatched {
		t.Fatalf("请求中断应失败且标记已投递: %+v", res)
	}
	if res.Err != upInterruptedMsg {
		t.Fatalf("应复用中断文案: %q", res.Err)
	}
	if svc.upCalls != 1 {
		t.Fatalf("退避期间取消不应再执行 up,实际 %d 次", svc.upCalls)
	}
}

func TestDeployNoRetryOnNonTransientError(t *testing.T) {
	old := upRetryBackoffs
	upRetryBackoffs = [maxUpAttempts - 1]time.Duration{}
	defer func() { upRetryBackoffs = old }()

	svc := &deployServiceFake{upStderr: []string{"Error response from daemon: invalid compose config", "x", "x"}}

	res := Deploy(context.Background(), svc, "srv-1", "demo", "services: {}\n")
	if res.OK || !res.Dispatched {
		t.Fatalf("非瞬态错误应失败且标记已投递: %+v", res)
	}
	if svc.upCalls != 1 {
		t.Fatalf("非瞬态错误不应重试,up 实际执行 %d 次", svc.upCalls)
	}
	if !strings.Contains(res.Err, "invalid compose config") || strings.Contains(res.Err, "已自动重试") {
		t.Fatalf("非瞬态错误应原样保留根因且不提自动重试: %q", res.Err)
	}
}

func TestTruncateTail(t *testing.T) {
	t.Run("短日志原样保留", func(t *testing.T) {
		const log = "Error: root cause"
		if got := truncateTail(log, 1024); got != log {
			t.Fatalf("短日志被修改: %q", got)
		}
	})

	t.Run("不切断 UTF-8 字符", func(t *testing.T) {
		got := truncateTail("前置"+strings.Repeat("中", 10), 4)
		if !utf8.ValidString(got) {
			t.Fatalf("截断结果不是有效 UTF-8: %q", got)
		}
		if !strings.HasSuffix(got, "中") || len(got)-len("…(前文已截断，仅保留末尾日志)\n") != len("中") {
			t.Fatalf("应对齐到完整字符并保留末尾: %q", got)
		}
	})
}
