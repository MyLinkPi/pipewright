package target

// 本机(控制节点)执行支持的单元测试。
//
// 本地命令刻意选用**跨平台且必存在**的 `go`(go test 环境必有),避免依赖 sh/echo 等
// 平台差异;PTY 交互会话单独在 local_pty_unix_test.go(仅类 Unix)覆盖。

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBaseService 是装饰器的透传目标:只实现本机测试会用到的几个方法。
// 嵌入 nil 接口 —— 未实现的方法被误调会 panic,便于发现「本机分支漏了」。
type fakeBaseService struct {
	Service
	gotID  string
	gotCmd []string
}

func (f *fakeBaseService) Get(_ context.Context, id string) (*Server, error) {
	if id == "real-1" {
		return &Server{ID: id, Name: "web-1", Host: "10.0.0.1", Port: 22, User: "root"}, nil
	}
	return nil, ErrNotFound
}

func (f *fakeBaseService) Exec(_ context.Context, id string, cmd []string) (*ExecResult, error) {
	f.gotID, f.gotCmd = id, cmd
	return &ExecResult{Stdout: "ssh-passthrough"}, nil
}

func (f *fakeBaseService) Update(_ context.Context, id string, _ UpdateInput) (*Server, error) {
	return &Server{ID: id}, nil
}

func (f *fakeBaseService) Delete(_ context.Context, id string) error { return nil }

// TestWithLocalHostNil 保持既有「服务未初始化 → 503」语义。
func TestWithLocalHostNil(t *testing.T) {
	if got := WithLocalHost(nil); got != nil {
		t.Fatalf("WithLocalHost(nil) = %v, want nil", got)
	}
}

// TestLocalHostGetSyntheticServer Get 对本机返回合成视图(供 handler 定位校验),对真实 id 透传。
func TestLocalHostGetSyntheticServer(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	srv, err := svc.Get(context.Background(), LocalHostID)
	if err != nil {
		t.Fatalf("Get(local): %v", err)
	}
	if srv.ID != LocalHostID {
		t.Fatalf("id = %q, want %q", srv.ID, LocalHostID)
	}
	if srv.Name == "" || srv.Host == "" || srv.User == "" {
		t.Fatalf("synthetic server must be displayable: %+v", srv)
	}
	// 合成视图绝不含凭据引用。
	if srv.CredentialID != "" || srv.SudoCredentialID != "" || len(srv.Jumps) != 0 {
		t.Fatalf("synthetic server must carry no credentials: %+v", srv)
	}

	if got, err := svc.Get(context.Background(), "real-1"); err != nil || got.Name != "web-1" {
		t.Fatalf("passthrough Get = %+v, %v", got, err)
	}
	if _, err := svc.Get(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}
}

// TestLocalHostImmutable 控制节点不是可登记实体:Update/Delete 必须拒绝,真实 id 透传。
func TestLocalHostImmutable(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	ctx := context.Background()
	if _, err := svc.Update(ctx, LocalHostID, UpdateInput{}); !errors.Is(err, ErrLocalHostImmutable) {
		t.Fatalf("Update(local) err = %v, want ErrLocalHostImmutable", err)
	}
	if err := svc.Delete(ctx, LocalHostID); !errors.Is(err, ErrLocalHostImmutable) {
		t.Fatalf("Delete(local) err = %v, want ErrLocalHostImmutable", err)
	}
	if _, err := svc.Update(ctx, "real-1", UpdateInput{}); err != nil {
		t.Fatalf("passthrough Update: %v", err)
	}
	if err := svc.Delete(ctx, "real-1"); err != nil {
		t.Fatalf("passthrough Delete: %v", err)
	}
}

// TestLocalExecInProcess 本机 Exec 真的在控制节点跑命令(argv,不经 shell)。
func TestLocalExecInProcess(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	res, err := svc.Exec(context.Background(), LocalHostID, []string{"go", "version"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "go version") {
		t.Fatalf("stdout = %q, want to contain 'go version'", res.Stdout)
	}
}

// TestLocalExecPassthrough 真实 serverID 一律透传到 base(不经本机)。
func TestLocalExecPassthrough(t *testing.T) {
	base := &fakeBaseService{}
	svc := WithLocalHost(base)
	cmd := []string{"docker", "ps"}
	res, err := svc.Exec(context.Background(), "real-1", cmd)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "ssh-passthrough" || base.gotID != "real-1" || len(base.gotCmd) != 2 {
		t.Fatalf("passthrough failed: res=%+v base=%+v", res, base)
	}
}

// TestLocalExecMissingBinary 命令不存在 → 伪退出码 127(而非 error),让上层走「运行时不可用」
// 的正常降级,而不是误判「服务器不可达」。
func TestLocalExecMissingBinary(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	res, err := svc.Exec(context.Background(), LocalHostID, []string{"pipewright-no-such-binary-xyz"})
	if err != nil {
		t.Fatalf("Exec should not error on missing binary: %v", err)
	}
	if res.ExitCode != localExitNotFound {
		t.Fatalf("exit = %d, want %d", res.ExitCode, localExitNotFound)
	}
	if res.Stderr == "" {
		t.Fatalf("stderr should explain the failure")
	}
}

// TestLocalExecNonZeroExit 命令跑完但非零退出 → 返回 ExitCode,不返回 error。
func TestLocalExecNonZeroExit(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	res, err := svc.Exec(context.Background(), LocalHostID, []string{"go", "definitely-not-a-subcommand"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("exit = 0, want non-zero")
	}
}

// TestLocalExecEmptyCommand 空命令 array → ErrEmptyCommand。
func TestLocalExecEmptyCommand(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	if _, err := svc.Exec(context.Background(), LocalHostID, nil); !errors.Is(err, ErrEmptyCommand) {
		t.Fatalf("err = %v, want ErrEmptyCommand", err)
	}
}

// TestLocalExecContextCanceled 已取消的 ctx → 返回 ctx.Err()(上层映射为「采集超时」)。
func TestLocalExecContextCanceled(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.Exec(ctx, LocalHostID, []string{"go", "version"}); err == nil {
		t.Fatalf("want ctx error")
	}
}

// TestLocalExecWithStdin 本机 ExecWithStdin(Unix 下用 cat 验证 stdin 真被接通)。
func TestLocalExecWithStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cat 非 Windows 命令;stdin 接线逻辑已由 PTY 与 Unix 用例覆盖")
	}
	svc := WithLocalHost(&fakeBaseService{})
	res, err := svc.ExecWithStdin(context.Background(), LocalHostID, []string{"cat"}, strings.NewReader("stdin-payload"))
	if err != nil {
		t.Fatalf("ExecWithStdin: %v", err)
	}
	if !strings.Contains(res.Stdout, "stdin-payload") {
		t.Fatalf("stdout = %q, want stdin payload", res.Stdout)
	}
}

// TestLocalStream 本机 ExecStream 返回 stdout 流;Close 幂等。
func TestLocalStream(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	rc, err := svc.ExecStream(context.Background(), LocalHostID, []string{"go", "version"})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "go version") {
		t.Fatalf("stream = %q", raw)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close must be idempotent: %v", err)
	}
}

// TestLocalStreamEmptyCommand 空命令 → ErrEmptyCommand。
func TestLocalStreamEmptyCommand(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	if _, err := svc.ExecStream(context.Background(), LocalHostID, nil); !errors.Is(err, ErrEmptyCommand) {
		t.Fatalf("err = %v, want ErrEmptyCommand", err)
	}
}

// TestLocalUpload 本机 Upload 把内容流式写到本地路径(自动建父目录)。
func TestLocalUpload(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	path := filepath.Join(t.TempDir(), "nested", "compose.yml")
	if err := svc.Upload(context.Background(), LocalHostID, strings.NewReader("services: {}\n"), path); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != "services: {}\n" {
		t.Fatalf("content = %q", raw)
	}
	if err := svc.Upload(context.Background(), LocalHostID, strings.NewReader("x"), ""); err == nil {
		t.Fatalf("empty path should fail")
	}
}

// TestLocalTest 本机 Test 直接本地探测(不经 SSH)。
func TestLocalTest(t *testing.T) {
	svc := WithLocalHost(&fakeBaseService{})
	res, err := svc.Test(context.Background(), LocalHostID)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !res.OK || res.Output == "" {
		t.Fatalf("local Test = %+v", res)
	}
}

// TestLocalInteractiveUnsupportedOnNonUnix 非类 Unix 平台(如 Windows)无 PTY:返回人读错误。
func TestLocalInteractiveUnsupportedOnNonUnix(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "dragonfly", "netbsd", "openbsd":
		t.Skip("类 Unix 平台由 local_pty_unix_test.go 覆盖")
	}
	svc := WithLocalHost(&fakeBaseService{})
	if _, err := svc.ExecInteractive(context.Background(), LocalHostID, []string{"sh"}); !errors.Is(err, ErrLocalInteractiveUnsupported) {
		t.Fatalf("err = %v, want ErrLocalInteractiveUnsupported", err)
	}
}

// 编译期断言:装饰器必须完整满足 Service(嵌入接口 + 覆盖方法)。
var _ Service = (*localHostService)(nil)
