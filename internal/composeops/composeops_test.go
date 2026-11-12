package composeops

import (
	"context"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/huangchengsir/pipewright/internal/target"
)

type deployServiceFake struct {
	target.Service
	stderr string
}

func (f *deployServiceFake) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	if cmd[0] == "mkdir" || (len(cmd) >= 3 && cmd[0] == "docker" && cmd[1] == "compose" && cmd[2] == "version") {
		return &target.ExecResult{}, nil
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
