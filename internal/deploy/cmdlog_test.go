package deploy

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/huangchengsir/pipewright/internal/target"
)

func TestDisplayCmd(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		want string
	}{
		{"sh -c shows script", []string{"sh", "-c", "docker build -t app .\ndocker run app", "/cur"}, "docker build -t app .\ndocker run app"},
		{"mask -e KV", []string{"docker", "run", "-e", "DB_PASSWORD=s3cr3t", "-p", "8080:8080", "app"}, "docker run -e DB_PASSWORD=*** -p 8080:8080 app"},
		{"mask --env KV", []string{"docker", "run", "--env", "TOKEN=abc", "app"}, "docker run --env TOKEN=*** app"},
		{"mask --password value", []string{"docker", "login", "--password", "hunter2", "reg.io"}, "docker login --password *** reg.io"},
		{"port not masked", []string{"docker", "run", "-p", "443:443", "app"}, "docker run -p 443:443 app"},
		{"plain join", []string{"tar", "xf", "deployctx.tar"}, "tar xf deployctx.tar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := displayCmd(c.cmd); got != c.want {
				t.Fatalf("displayCmd(%v) = %q, want %q", c.cmd, got, c.want)
			}
		})
	}
}

// TestExecStreamsToCmdLog 验证:挂了 WithCmdLog → s.exec 把命令 + stdout/stderr 回流;未挂 → 不 panic、纯透传。
func TestExecStreamsToCmdLog(t *testing.T) {
	tgt := &stubTarget{
		servers: map[string]*target.Server{},
		execFn: func(_ string, _ []string) (*target.ExecResult, error) {
			return &target.ExecResult{Stdout: "BUILD SUCCESS\n", Stderr: "", ExitCode: 0}, nil
		},
	}
	svc := New(tgt, nil).(*service)

	var mu sync.Mutex
	var lines []string
	ctx := WithCmdLog(context.Background(), func(stream, machine, text string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, stream+"|"+text)
	})

	if _, err := svc.exec(ctx, "srv1", []string{"sh", "-c", "docker build -t app .", "/cur"}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "stdout|$ docker build -t app .") {
		t.Fatalf("command not echoed: %q", joined)
	}
	if !strings.Contains(joined, "stdout|BUILD SUCCESS") {
		t.Fatalf("stdout not streamed: %q", joined)
	}

	// 未挂 cmdlog:纯透传,不 panic、不记录。
	if _, err := svc.exec(context.Background(), "srv1", []string{"echo", "hi"}); err != nil {
		t.Fatalf("exec without sink: %v", err)
	}
}

// TestObservingTargetStreamsCmdLog ObservingTarget 包装的 target.Service(供 servicereg 等
// 第三方编排组件复用命令日志):命令/输出/退出码回流;无日志 ctx 纯透传;
// 机器归属沿用调用方 ctx 的单机作用域(部署路径 = 触发本动作的目标机)。
func TestObservingTargetStreamsCmdLog(t *testing.T) {
	inner := &stubTarget{execFn: func(_ string, _ []string) (*target.ExecResult, error) {
		return &target.ExecResult{Stdout: "syntax is ok\n", Stderr: "warn\n", ExitCode: 1}, nil
	}}
	obs := ObservingTarget(inner)
	if obs == nil {
		t.Fatal("ObservingTarget(nil 之外)不应返回 nil")
	}

	var mu sync.Mutex
	var lines []string
	ctx := WithCmdLog(context.Background(), func(stream, machine, text string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, stream+"|"+machine+"|"+text)
	})
	// 部署路径的 ctx 带目标机作用域 → 包装层回显沿用该归属(与部署命令同分组)。
	ctx = scopeCmdLog(ctx, "web-1")
	if _, err := obs.Exec(ctx, "gw-1", []string{"docker", "exec", "ng", "nginx", "-t"}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(inner.calls) != 1 {
		t.Fatalf("底层 target.Service 应收到命令, got %v", inner.calls)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "stdout|web-1|$ docker exec ng nginx -t") {
		t.Fatalf("命令回显应沿用作用域归属: %q", joined)
	}
	if !strings.Contains(joined, "stdout|web-1|syntax is ok") || !strings.Contains(joined, "stderr|web-1|warn") {
		t.Fatalf("stdout/stderr 应回流: %q", joined)
	}
	if !strings.Contains(joined, "✗ 退出码 1") {
		t.Fatalf("非零退出码应标注: %q", joined)
	}

	// 无日志 ctx:纯透传,不 panic。
	if _, err := obs.Exec(context.Background(), "gw-1", []string{"echo", "hi"}); err != nil {
		t.Fatalf("无日志 ctx 应透传: %v", err)
	}
	if len(inner.calls) != 2 {
		t.Fatalf("透传应仍执行命令, got %d 次", len(inner.calls))
	}
}
