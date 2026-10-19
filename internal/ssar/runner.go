// runner.go — SSH 执行抽象(与 diskadmin/exec.go 同形的提权探测 + 追加免提权通道)。
//
// sshRunner 基于 target.Service:Run 提权执行(安装包安装须 root:root 登录直跑,否则
// 探测免密 sudo 通过后前缀 sudo -n;探测结果按服务器缓存 10 分钟);RunPlain 免提权
// 执行(状态探测与 ssar 查询:ssar 读全可读的数据文件,systemctl is-active 亦免 root);
// Upload 经登录用户写远端文件(落 /tmp,后续提权安装)。
package ssar

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// 领域错误(语义化;httpapi 据此映射状态码/文案)。
var (
	// ErrServerNotFound 服务器不存在/不可用。
	ErrServerNotFound = target.ErrNotFound
	// ErrNoPrivilege 非 root 且无免密 sudo(安装需要)。
	ErrNoPrivilege = fmt.Errorf("ssar: 需要提权")
)

// privTTL 是提权探测结果的服务器级缓存时长。
const privTTL = 10 * time.Minute

// sshRunner 是生产 Runner。
type sshRunner struct {
	servers target.Service

	mu   sync.Mutex
	priv map[string]privEntry
}

// newSSHRunner 构造生产 Runner。
func newSSHRunner(servers target.Service) *sshRunner {
	return &sshRunner{servers: servers, priv: map[string]privEntry{}}
}

// privEntry 是单台服务器的提权探测缓存。
type privEntry struct {
	ok  bool
	err error
	at  time.Time
}

// Run 对 serverID 提权执行 cmd(非零退出码 → ErrExec 语义错误,附输出尾部)。
func (r *sshRunner) Run(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	full, err := r.privileged(ctx, serverID, cmd)
	if err != nil {
		return nil, err
	}
	return r.exec(ctx, serverID, full)
}

// RunPlain 免提权执行(非零退出码同样返回结果与错误)。
func (r *sshRunner) RunPlain(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	return r.exec(ctx, serverID, cmd)
}

// Upload 把 content 流式写到目标机 remotePath(登录用户权限;命令与路径 array 化)。
func (r *sshRunner) Upload(ctx context.Context, serverID string, content io.Reader, remotePath string) error {
	return r.servers.Upload(ctx, serverID, content, remotePath)
}

// exec 拼装服务器名后执行并统一映射非零退出码。
func (r *sshRunner) exec(ctx context.Context, serverID string, full []string) (*target.ExecResult, error) {
	if len(full) == 0 {
		return nil, fmt.Errorf("ssar: 空命令")
	}
	sv, err := r.servers.Get(ctx, serverID)
	if err != nil {
		return nil, fmt.Errorf("%w: 服务器 %s 不可用: %v", ErrServerNotFound, serverID, err)
	}
	res, err := r.servers.Exec(ctx, serverID, full)
	if err != nil {
		return nil, fmt.Errorf("ssar: %s %s: %v", sv.Name, full[0], err)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("ssar: %s %s 退出码 %d\n%s", sv.Name, full[0], res.ExitCode, tailOutput(res))
	}
	return res, nil
}

// privileged 返回带提权前缀的完整命令。
func (r *sshRunner) privileged(ctx context.Context, serverID string, cmd []string) ([]string, error) {
	sv, err := r.servers.Get(ctx, serverID)
	if err != nil {
		return nil, fmt.Errorf("%w: 服务器 %s 不可用: %v", ErrServerNotFound, serverID, err)
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
