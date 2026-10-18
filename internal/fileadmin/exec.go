// exec.go — SSH 执行抽象与提权探测(仿 diskadmin/exec.go,回退语义不同)。
//
// Runner 把「在某台登记服务器上跑一条 argv 命令」抽象为最小接口,生产实现
// sshRunner 基于 target.Service。与 diskadmin 的差异:文件浏览的只读操作不应因
// 无提权整体不可用,故 root 直跑;否则探测免密 sudo(结果按服务器缓存 10 分钟)
// 通过后前缀 sudo -n,**探测失败回退登录用户身份执行**(写受限文件时自然得到
// 权限错误)。命令超时由调用方经 ctx 控制。
package fileadmin

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// privTTL 是提权探测结果的服务器级缓存时长。
const privTTL = 10 * time.Minute

// Runner 抽象「对指定服务器执行 argv 命令」(注入 fake 可离线单测)。
// 非零退出码返回 ErrExec 包装(stderr/stdout 尾部随错误,便于排障;命令不含凭据)。
type Runner interface {
	Run(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error)
	// RunWithStdin 同 Run,但把 stdin 接到远端命令标准输入(文件内容写入用;
	// stdin 内容绝不进 argv/日志)。
	RunWithStdin(ctx context.Context, serverID string, cmd []string, stdin io.Reader) (*target.ExecResult, error)
	// Stream 流式执行(文件下载用):返回 stdout 流,读到底或关闭即释放 SSH session。
	Stream(ctx context.Context, serverID string, cmd []string) (io.ReadCloser, error)
}

// privEntry 是单台服务器的提权探测缓存(ok=false 表示无 sudo,回退登录用户)。
type privEntry struct {
	ok bool
	at time.Time
}

// sshRunner 是生产 Runner:target.Service SSH 执行 + root/免密 sudo 提权(可回退)。
type sshRunner struct {
	servers target.Service

	mu   sync.Mutex
	priv map[string]privEntry
}

// newSSHRunner 构造生产 Runner。
func newSSHRunner(servers target.Service) *sshRunner {
	return &sshRunner{servers: servers, priv: map[string]privEntry{}}
}

// Run 对 serverID 执行 cmd(提权可用则提权)。
func (r *sshRunner) Run(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	full, err := r.privileged(ctx, serverID, cmd)
	if err != nil {
		return nil, err
	}
	return r.exec(ctx, serverID, full)
}

// RunWithStdin 同 Run,经 stdin 喂入内容。
func (r *sshRunner) RunWithStdin(ctx context.Context, serverID string, cmd []string, stdin io.Reader) (*target.ExecResult, error) {
	full, err := r.privileged(ctx, serverID, cmd)
	if err != nil {
		return nil, err
	}
	sv, err := r.server(ctx, serverID)
	if err != nil {
		return nil, err
	}
	res, err := r.servers.ExecWithStdin(ctx, serverID, full, stdin)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %v", ErrExec, sv.Name, cmd[0], err)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%w: %s 退出码 %d\n%s", ErrExec, cmd[0], res.ExitCode, tailOutput(res))
	}
	return res, nil
}

// Stream 流式执行(下载;提权可用则提权)。
func (r *sshRunner) Stream(ctx context.Context, serverID string, cmd []string) (io.ReadCloser, error) {
	full, err := r.privileged(ctx, serverID, cmd)
	if err != nil {
		return nil, err
	}
	if _, err := r.server(ctx, serverID); err != nil {
		return nil, err
	}
	return r.servers.ExecStream(ctx, serverID, full)
}

// exec 拼装服务器名后执行并统一映射非零退出码。
func (r *sshRunner) exec(ctx context.Context, serverID string, full []string) (*target.ExecResult, error) {
	sv, err := r.server(ctx, serverID)
	if err != nil {
		return nil, err
	}
	res, err := r.servers.Exec(ctx, serverID, full)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %v", ErrExec, sv.Name, full[0], err)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%w: %s 退出码 %d\n%s", ErrExec, full[0], res.ExitCode, tailOutput(res))
	}
	return res, nil
}

// server 取服务器视图(不存在 → ErrServerNotFound)。
func (r *sshRunner) server(ctx context.Context, serverID string) (*target.Server, error) {
	sv, err := r.servers.Get(ctx, serverID)
	if err != nil {
		return nil, fmt.Errorf("%w: 服务器 %s 不可用: %v", ErrServerNotFound, serverID, err)
	}
	return sv, nil
}

// privileged 返回带提权前缀的完整命令(无提权可用时原样返回,不报错)。
func (r *sshRunner) privileged(ctx context.Context, serverID string, cmd []string) ([]string, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("%w: 空命令", ErrInvalidInput)
	}
	sv, err := r.server(ctx, serverID)
	if err != nil {
		return nil, err
	}
	if sv.User == "root" {
		return cmd, nil
	}
	ok, err := r.sudoAvailable(ctx, serverID)
	if err != nil || !ok {
		// 无免密 sudo:回退登录用户身份(文件浏览只读操作多数仍可用)。
		return cmd, nil
	}
	return append([]string{"sudo", "-n"}, cmd...), nil
}

// sudoAvailable 探测免密 sudo(结果按服务器缓存 10 分钟;探测执行错误视同不可用)。
func (r *sshRunner) sudoAvailable(ctx context.Context, serverID string) (bool, error) {
	r.mu.Lock()
	if e, ok := r.priv[serverID]; ok && time.Since(e.at) < privTTL {
		r.mu.Unlock()
		return e.ok, nil
	}
	r.mu.Unlock()

	res, err := r.servers.Exec(ctx, serverID, []string{"sudo", "-n", "true"})
	ok := err == nil && res.ExitCode == 0
	r.mu.Lock()
	r.priv[serverID] = privEntry{ok: ok, at: time.Now()}
	r.mu.Unlock()
	return ok, err
}

// tailOutput 拼装 stderr/stdout 尾部(每段 ≤2000 字符;远端输出不含凭据)。
func tailOutput(res *target.ExecResult) string {
	tail := func(s string) string {
		s = strings.TrimRight(s, "\n")
		if len(s) > 2000 {
			s = "…" + s[len(s)-2000:]
		}
		return s
	}
	var b strings.Builder
	if t := tail(res.Stderr); t != "" {
		b.WriteString("stderr: " + t + "\n")
	}
	if t := tail(res.Stdout); t != "" {
		b.WriteString("stdout: " + t)
	}
	return strings.TrimRight(b.String(), "\n")
}
