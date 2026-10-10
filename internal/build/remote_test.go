package build

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/target"
)

// fakeRemoteExecer 是可控的 RemoteExecer:记录被投递的命令,按注入返回结果/错误。
type fakeRemoteExecer struct {
	gotServerID string
	gotCmds     [][]string
	result      *target.ExecResult
	err         error
}

func (f *fakeRemoteExecer) Exec(_ context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	f.gotServerID = serverID
	f.gotCmds = append(f.gotCmds, cmd)
	return f.result, f.err
}

func TestSSHCommanderRunMapsResult(t *testing.T) {
	f := &fakeRemoteExecer{result: &target.ExecResult{Stdout: "out", Stderr: "err", ExitCode: 7}}
	c := NewRemoteCommander(f, "srv-1")

	stdout, stderr, code, err := c.Run(context.Background(), "docker", []string{"build", "."}, "")
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if stdout != "out" || stderr != "err" || code != 7 {
		t.Fatalf("got (%q,%q,%d), want (out,err,7)", stdout, stderr, code)
	}
	// 命令必须 array 投递(program + args),且打到正确的远程机。
	if f.gotServerID != "srv-1" {
		t.Fatalf("serverID = %q, want srv-1", f.gotServerID)
	}
	if len(f.gotCmds) != 1 || len(f.gotCmds[0]) != 3 ||
		f.gotCmds[0][0] != "docker" || f.gotCmds[0][1] != "build" || f.gotCmds[0][2] != "." {
		t.Fatalf("cmd = %v, want [docker build .]", f.gotCmds)
	}
}

func TestSSHCommanderRunRejectsStdin(t *testing.T) {
	f := &fakeRemoteExecer{result: &target.ExecResult{}}
	c := NewRemoteCommander(f, "srv-1")
	_, _, code, err := c.Run(context.Background(), "docker", []string{"login"}, "secret-password")
	if !errors.Is(err, ErrRemoteStdinUnsupported) {
		t.Fatalf("err = %v, want ErrRemoteStdinUnsupported", err)
	}
	if code != -1 {
		t.Fatalf("code = %d, want -1", code)
	}
	// 关键:带 stdin(口令)的命令**绝不投递到远程**(避免 secret 处理面)。
	if len(f.gotCmds) != 0 {
		t.Fatalf("stdin 命令不应投递,实际投了 %v", f.gotCmds)
	}
}

func TestSSHCommanderRunSurfacesExecError(t *testing.T) {
	f := &fakeRemoteExecer{err: errors.New("ssh: connection refused")}
	c := NewRemoteCommander(f, "srv-1")
	_, _, code, err := c.Run(context.Background(), "docker", []string{"ps"}, "")
	if err == nil {
		t.Fatal("want exec error surfaced")
	}
	if code != -1 {
		t.Fatalf("code = %d, want -1 (无法在远程启动)", code)
	}
}

func TestSSHCommanderStreamReplaysLines(t *testing.T) {
	f := &fakeRemoteExecer{result: &target.ExecResult{
		Stdout:   "line1\nline2\n",
		Stderr:   "warn1\n",
		ExitCode: 0,
	}}
	c := NewRemoteCommander(f, "srv-1")

	type ev struct {
		stream, line string
	}
	var got []ev
	code, err := c.Stream(context.Background(), "sh", []string{"-c", "echo line1; echo line2"}, "", func(stream, line string) {
		got = append(got, ev{stream, line})
	})
	if err != nil {
		t.Fatalf("Stream err: %v", err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	want := []ev{{"stdout", "line1"}, {"stdout", "line2"}, {"stderr", "warn1"}}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %v, want %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestSSHCommanderStreamReturnsRemoteExitCode(t *testing.T) {
	f := &fakeRemoteExecer{result: &target.ExecResult{Stdout: "", Stderr: "boom", ExitCode: 2}}
	c := NewRemoteCommander(f, "srv-1")
	code, err := c.Stream(context.Background(), "false", nil, "", func(string, string) {})
	if err != nil {
		t.Fatalf("Stream err: %v", err)
	}
	if code != 2 {
		t.Fatalf("远程非零退出应原样取回,code = %d want 2", code)
	}
}

// NewRemoteDriver 应产出一个 Binary() 为指定 CLI 的 Driver,且其命令经远程 Commander 投递。
func TestNewRemoteDriverUsesRemoteCommander(t *testing.T) {
	f := &fakeRemoteExecer{result: &target.ExecResult{ExitCode: 0}}
	drv := NewRemoteDriver(f, "srv-1", "")
	if drv.Binary() != "docker" {
		t.Fatalf("默认 Binary = %q, want docker", drv.Binary())
	}
	// 触发一次 RunToolchain:应经远程机执行 `docker run ...`(命令打到 srv-1);
	// 资源规格(cpu/memory)应透传为 --cpus/--memory 投到远程 docker。
	_, err := drv.RunToolchain(context.Background(), "node:20", "/ws", "/src", nil, []string{"sh", "-c", "npm ci"}, pipeline.Resource{CPU: "2", Memory: "1g"}, func(string, string) {})
	if err != nil {
		t.Fatalf("RunToolchain err: %v", err)
	}
	if f.gotServerID != "srv-1" || len(f.gotCmds) == 0 || f.gotCmds[0][0] != "docker" {
		t.Fatalf("远程工具链命令未经 srv-1/docker 投递:server=%q cmds=%v", f.gotServerID, f.gotCmds)
	}
	if !cmdContainsPair(f.gotCmds[0], "--cpus", "2") || !cmdContainsPair(f.gotCmds[0], "--memory", "1g") {
		t.Fatalf("远程 docker run 未透传资源规格:%v", f.gotCmds[0])
	}

	// podman:Binary 与命令前缀应随之变化。
	drv2 := NewRemoteDriver(f, "srv-2", "podman")
	if drv2.Binary() != "podman" {
		t.Fatalf("Binary = %q, want podman", drv2.Binary())
	}
}

// cmdContainsPair 判断 args 中存在相邻的 (flag, value) 对。
func cmdContainsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// —— DetectRemoteCLI:远程容器 CLI 探测 + TTL 缓存(FR-8-19)——

// remoteExecFunc 把函数适配成 RemoteExecer(探测缓存测试需按调用次变化返回)。
type remoteExecFunc func(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error)

func (f remoteExecFunc) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	return f(ctx, serverID, cmd)
}

// resetRemoteCLICache 清空进程级探测缓存,保证用例间互不染指(缓存是包级变量)。
func resetRemoteCLICache(t *testing.T) {
	t.Helper()
	clear := func() {
		remoteCLIMu.Lock()
		remoteCLICache = map[string]remoteCLIAt{}
		remoteCLIMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// 探测按 nerdctl > docker > podman 顺序取首个命中;TTL 内第二次调用走缓存不再 SSH。
func TestDetectRemoteCLIPicksAndCaches(t *testing.T) {
	resetRemoteCLICache(t)
	var calls int32
	ex := remoteExecFunc(func(_ context.Context, _ string, _ []string) (*target.ExecResult, error) {
		atomic.AddInt32(&calls, 1)
		return &target.ExecResult{Stdout: "nerdctl\n", ExitCode: 0}, nil
	})
	if bin := DetectRemoteCLI(context.Background(), ex, "srv-1"); bin != "nerdctl" {
		t.Fatalf("bin = %q, want nerdctl", bin)
	}
	if bin := DetectRemoteCLI(context.Background(), ex, "srv-1"); bin != "nerdctl" {
		t.Fatalf("缓存命中应仍得 nerdctl, got %q", bin)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("TTL 内应只探测 1 次,实际 %d", n)
	}
}

// 超过 TTL 后缓存失效,应重新探测(机器换装 CLI 无需重启控制机即可感知)。
func TestDetectRemoteCLITTLExpiry(t *testing.T) {
	resetRemoteCLICache(t)
	now := time.Now()
	clock := func() time.Time { return now }
	var calls int32
	ex := remoteExecFunc(func(_ context.Context, _ string, _ []string) (*target.ExecResult, error) {
		atomic.AddInt32(&calls, 1)
		return &target.ExecResult{Stdout: "docker\n", ExitCode: 0}, nil
	})
	if bin := detectRemoteCLI(context.Background(), ex, "srv-1", clock, nil); bin != "docker" {
		t.Fatalf("bin = %q, want docker", bin)
	}
	now = now.Add(remoteCLITTL + time.Minute) // 越过 TTL
	if bin := detectRemoteCLI(context.Background(), ex, "srv-1", clock, nil); bin != "docker" {
		t.Fatalf("过期重探仍应得 docker, got %q", bin)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("过 TTL 应重新探测,实际调用 %d 次", n)
	}
}

// 探测失败(SSH 不通 / 输出无命中)回落 "docker",且失败结果不缓存(下次重新探测)。
func TestDetectRemoteCLIFallbackNotCached(t *testing.T) {
	resetRemoteCLICache(t)
	var calls int32
	ex := remoteExecFunc(func(_ context.Context, _ string, _ []string) (*target.ExecResult, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return nil, errors.New("ssh: connection refused")
		}
		if n == 2 {
			return &target.ExecResult{Stdout: "none\n", ExitCode: 0}, nil // 远程无任何容器 CLI
		}
		return &target.ExecResult{Stdout: "podman\n", ExitCode: 0}, nil
	})
	if bin := DetectRemoteCLI(context.Background(), ex, "srv-1"); bin != "docker" {
		t.Fatalf("SSH 失败应回落 docker, got %q", bin)
	}
	if bin := DetectRemoteCLI(context.Background(), ex, "srv-1"); bin != "docker" {
		t.Fatalf("无命中输出应回落 docker, got %q", bin)
	}
	if bin := DetectRemoteCLI(context.Background(), ex, "srv-1"); bin != "podman" {
		t.Fatalf("失败不缓存,第三次应真探测得 podman, got %q", bin)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("失败结果不得缓存,每次都应真探测,实际调用 %d 次", n)
	}
}

// TestDetectRemoteCLIWithLogEchoesProbe 探测命令仅在**真正执行**时经 log 回显;
// 命中 TTL 缓存直接返回 → 不输出(避免回显未执行的命令,误导排查)。
func TestDetectRemoteCLIWithLogEchoesProbe(t *testing.T) {
	resetRemoteCLICache(t)
	ex := remoteExecFunc(func(_ context.Context, _ string, _ []string) (*target.ExecResult, error) {
		return &target.ExecResult{Stdout: "docker\n", ExitCode: 0}, nil
	})
	var mu sync.Mutex
	var lines []string
	log := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, line)
	}
	if bin := DetectRemoteCLIWithLog(context.Background(), ex, "srv-echo", log); bin != "docker" {
		t.Fatalf("bin = %q, want docker", bin)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "$ sh -c ") {
		t.Fatalf("首次探测应回显探测命令, got %v", lines)
	}
	// TTL 缓存命中 → 不再执行探测,也不回显。
	if bin := DetectRemoteCLIWithLog(context.Background(), ex, "srv-echo", log); bin != "docker" {
		t.Fatalf("缓存命中应得 docker, got %q", bin)
	}
	if len(lines) != 1 {
		t.Fatalf("缓存命中不应重复回显, got %v", lines)
	}
}
