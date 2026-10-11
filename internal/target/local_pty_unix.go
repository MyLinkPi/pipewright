//go:build linux || darwin || freebsd || dragonfly || netbsd || openbsd

package target

// 类 Unix 平台的本地 PTY 交互会话(容器内 `docker exec -it` 需要真 TTY)。
//
// 经 creack/pty 在控制节点分配伪终端:命令的 stdin/stdout/stderr 一并接到 PTY 从端
// (故 stdout/stderr 天然合流,与 SSH PTY 行为一致),Session 的 Read/Write 走 PTY 主端。

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// startLocalInteractive 在本机分配 PTY 并启动 cmd(array,不经 shell)。
func startLocalInteractive(ctx context.Context, cmd []string) (Session, error) {
	if len(cmd) == 0 {
		return nil, ErrEmptyCommand
	}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	f, err := pty.Start(c)
	if err != nil {
		return nil, err
	}
	s := &localPTYSession{cmd: c, f: f, done: make(chan struct{})}
	// ctx 取消即回收(与 SSH 版「ctx 取消亦关 session」的保证对齐),防泄漏。
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.done:
		}
	}()
	return s, nil
}

// localPTYSession 是控制节点本机的双向 PTY 会话。
type localPTYSession struct {
	cmd  *exec.Cmd
	f    *os.File
	done chan struct{}
	once sync.Once
}

func (s *localPTYSession) Read(p []byte) (int, error)  { return s.f.Read(p) }
func (s *localPTYSession) Write(p []byte) (int, error) { return s.f.Write(p) }

// Resize 调整本机 PTY 窗口尺寸;失败不致命(调用方忽略)。
func (s *localPTYSession) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return pty.Setsize(s.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Close 幂等:杀掉子进程、等待回收、关闭 PTY 主端。
func (s *localPTYSession) Close() error {
	s.once.Do(func() {
		close(s.done)
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.cmd.Wait()
		_ = s.f.Close()
	})
	return nil
}

// 编译期断言:localPTYSession 必须满足 Session。
var _ Session = (*localPTYSession)(nil)
var _ io.ReadWriteCloser = (*localPTYSession)(nil)
