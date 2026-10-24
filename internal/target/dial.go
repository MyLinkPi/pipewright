package target

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHClient 是长连接 SSH 客户端的最小抽象(0099 SSH 隧道转发面;按 Dial/ExecStream
// 先例追加的只读扩展)。四个方法覆盖隧道数据面与活性探测:
//   - Dial:经远端 sshd 发起 direct-tcpip 连接(ssh -L / -D 的数据面);
//   - Listen:请求远端 sshd 监听端口(tcpip-forward,ssh -R 的数据面);
//   - Ping:keepalive 全局请求探活(检测 TCP 半开死连接;超时/失败 = 连接已死);
//   - Close:关闭整条连接链(含跳板链全部中间跳,防泄漏)。
//
// 抽象为接口便于隧道域注入 fake 做不触网单测(与 SSHDialer 同一测试纪律)。
type SSHClient interface {
	Dial(network, addr string) (net.Conn, error)
	Listen(network, addr string) (net.Listener, error)
	Ping(timeout time.Duration) error
	Close() error
}

// sshClientWrap 把 dialClient 的最外层 client 与整链收尾闭包包成一个 SSHClient:
// Close 走 closeChain(逆序关闭全部跳 + 最外层),绝不只关外层造成中间跳泄漏。
type sshClientWrap struct {
	client *ssh.Client
	chain  func()
}

func (c *sshClientWrap) Dial(network, addr string) (net.Conn, error) {
	return c.client.Dial(network, addr)
}

func (c *sshClientWrap) Listen(network, addr string) (net.Listener, error) {
	return c.client.Listen(network, addr)
}

// Ping 经 keepalive 全局请求探活;SendRequest 无超时参数,超时经 select 兜底
//(死连接上它可能永久阻塞)。
func (c *sshClientWrap) Ping(timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		_, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errors.New("ssh: keepalive timeout")
	}
}

func (c *sshClientWrap) Close() error {
	c.chain()
	return nil
}

// Dial 建立到已登记服务器 serverID 的长连接 SSH client(含其跳板链),供 SSH 隧道
// 转发面使用。凭据取用纪律同 Exec:明文仅进程内装配完立即 wipe;调用方用完 Close。
func (s *service) Dial(ctx context.Context, serverID string) (SSHClient, error) {
	srv, err := s.Get(ctx, serverID)
	if err != nil {
		return nil, err
	}
	if s.vault == nil {
		return nil, ErrVaultUnconfigured
	}
	cfg, err := s.resolveSSHConfig(srv)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("%s:%d", srv.Host, srv.Port)
	client, closeChain, err := dialClient(ctx, addr, cfg)
	wipeSSHConfig(&cfg)
	if err != nil {
		return nil, err
	}
	return &sshClientWrap{client: client, chain: closeChain}, nil
}

// DialDirect 以直连参数(host/port/user + vault 凭据引用)建立长连接 SSH client,
// 供 SSH 隧道的直连模式(目标机不在 servers 表)使用;无跳板链(需要跳板的机器请
// 登记为服务器)。凭据类型须为 ssh_key / ssh_password(与跳板凭据同一守卫语义);
// 明文即用即弃,错误体绝无凭据。
func (s *service) DialDirect(ctx context.Context, host string, port int, user, credentialID string) (SSHClient, error) {
	if host == "" || user == "" || credentialID == "" {
		return nil, ErrInvalidJump
	}
	if s.vault == nil {
		return nil, ErrVaultUnconfigured
	}
	secret, err := s.vault.Get(credentialID)
	if err != nil {
		return nil, vaultErr(err)
	}
	cfg := SSHConfig{User: user}
	if looksLikePEM(secret) {
		cfg.PrivateKey = secret
	} else {
		cfg.Password = secret
	}
	addr := fmt.Sprintf("%s:%d", host, normalizePort(port))
	client, closeChain, err := dialClient(ctx, addr, cfg)
	wipeSSHConfig(&cfg)
	if err != nil {
		return nil, err
	}
	return &sshClientWrap{client: client, chain: closeChain}, nil
}
