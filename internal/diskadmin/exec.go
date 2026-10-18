// exec.go — SSH 执行抽象与提权探测(与 ceph/exec.go 同形)。
//
// Runner 把「在某台登记服务器上以提权方式跑一条 argv 命令」抽象为最小接口,生产实现
// sshRunner 基于 target.Service(root 登录直跑;否则探测免密 sudo 通过后前缀 sudo -n)。
// 磁盘管理全部命令须提权(lsblk/pvs 全量视图、parted/mkfs/mount 均需 root),提权探测
// 结果按服务器缓存 10 分钟;命令超时由调用方经 ctx 控制。
package diskadmin

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

// Runner 抽象「对指定服务器提权执行 argv 命令」(注入 fake 可离线单测)。
// 非零退出码返回 ErrExec 包装(stderr/stdout 尾部随错误,便于排障;命令不含凭据)。
type Runner interface {
	Run(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error)
	// RunWithStdin 同 Run,但把 stdin 接到远端命令标准输入(fstab 内容写入用;
	// stdin 内容绝不进 argv/日志)。
	RunWithStdin(ctx context.Context, serverID string, cmd []string, stdin io.Reader) (*target.ExecResult, error)
}

// privEntry 是单台服务器的提权探测缓存。
type privEntry struct {
	ok  bool
	err error
	at  time.Time
}

// sshRunner 是生产 Runner:target.Service SSH 执行 + root/免密 sudo 提权。
type sshRunner struct {
	servers target.Service

	mu   sync.Mutex
	priv map[string]privEntry
}

// newSSHRunner 构造生产 Runner。
func newSSHRunner(servers target.Service) *sshRunner {
	return &sshRunner{servers: servers, priv: map[string]privEntry{}}
}

// Run 对 serverID 提权执行 cmd。提权判定(每服务器缓存):
//
//	登录用户为 root → 直接执行;
//	否则探测 sudo -n true(exit 0)→ 前缀 sudo -n 执行;
//	皆不可用 → ErrNoPrivilege(附 sudoers 配置指引)。
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

// privileged 返回带提权前缀的完整命令。
func (r *sshRunner) privileged(ctx context.Context, serverID string, cmd []string) ([]string, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("%w: 空命令", ErrInvalidInput)
	}
	sv, err := r.server(ctx, serverID)
	if err != nil {
		return nil, err
	}
	prefix, err := r.privilegePrefix(ctx, serverID, sv.User)
	if err != nil {
		return nil, err
	}
	if len(prefix) == 0 {
		return cmd, nil
	}
	return append(append([]string{}, prefix...), cmd...), nil
}

// privilegePrefix 返回该服务器的提权 argv 前缀(root → 空;免密 sudo → ["sudo","-n"])。
func (r *sshRunner) privilegePrefix(ctx context.Context, serverID, user string) ([]string, error) {
	if user == "root" {
		return nil, nil
	}
	r.mu.Lock()
	if e, ok := r.priv[serverID]; ok && time.Since(e.at) < privTTL {
		r.mu.Unlock()
		if e.err != nil {
			return nil, e.err
		}
		return []string{"sudo", "-n"}, nil
	}
	r.mu.Unlock()

	res, err := r.servers.Exec(ctx, serverID, []string{"sudo", "-n", "true"})
	if err != nil || res.ExitCode != 0 {
		e := privEntry{at: time.Now(), err: fmt.Errorf(
			"%w: 服务器登录用户 %s 非 root 且无免密 sudo;请改用 root 登录,或配置免密 sudo(如 /etc/sudoers.d/pipewright 写入「%s ALL=(ALL) NOPASSWD: ALL」)", ErrNoPrivilege, user, user)}
		r.mu.Lock()
		r.priv[serverID] = e
		r.mu.Unlock()
		return nil, e.err
	}
	r.mu.Lock()
	r.priv[serverID] = privEntry{ok: true, at: time.Now()}
	r.mu.Unlock()
	return []string{"sudo", "-n"}, nil
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
