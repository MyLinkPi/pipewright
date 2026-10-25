package sshtunnel

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// forward 是一条已就绪的隧道转发面:实际监听端点 + 全部 listener 的收尾。
type forward struct {
	endpoint   string // 实际监听 host:port(bind_port=0 时为内核/远端分配的端口)
	actualPort int
	closers    []io.Closer
	// dead 非 nil 时,转发面内部死亡(vpn 包泵故障等)向其发送错误并等待外层
	// 重连 —— 端口转发无此通道:按连接粒度死亡,不影响 ssh 连接活性。
	dead chan error
}

func (f *forward) close() {
	for i := len(f.closers) - 1; i >= 0; i-- {
		_ = f.closers[i].Close()
	}
}

// fail 上报转发面内部故障(非阻塞;守望循环收到后走重连)。
func (f *forward) fail(err error) {
	if f.dead != nil {
		select {
		case f.dead <- err:
		default:
		}
	}
}

// startForward 按模式建立转发面(local:本机 listen + 远端 Dial;remote:远端
// Listen + 本机 Dial;dynamic:本机 listen 充 SOCKS5;vpn:点对点 tun)。
func startForward(client target.SSHClient, t *Tunnel) (*forward, error) {
	switch t.Mode {
	case ModeLocal:
		return startLocalForward(client, t)
	case ModeRemote:
		return startRemoteForward(client, t)
	case ModeDynamic:
		return startDynamicForward(client, t)
	case ModeVPN:
		return startVPNForward(client, t)
	default:
		return nil, fmt.Errorf("未知隧道模式 %q", t.Mode)
	}
}

// startLocalForward(-L):本机监听 bind,连接经远端 sshd 转发到 dest(远端侧可达)。
func startLocalForward(client target.SSHClient, t *Tunnel) (*forward, error) {
	ln, err := net.Listen("tcp", bindAddr(t))
	if err != nil {
		return nil, fmt.Errorf("本地监听 %s 失败:%v", bindAddr(t), err)
	}
	dest := net.JoinHostPort(t.DestHost, strconv.Itoa(t.DestPort))
	go acceptLoop(ln, func(c net.Conn) {
		remote, err := client.Dial("tcp", dest)
		if err != nil {
			_ = c.Close()
			return
		}
		pipeBoth(c, remote)
	})
	return newForward(ln), nil
}

// startRemoteForward(-R):请求远端 sshd 监听 bind,远端连接回传后在本机连 dest
//(本机侧可达)。绑定 0.0.0.0 需目标机 sshd 开启 GatewayPorts,否则被降级/拒绝。
func startRemoteForward(client target.SSHClient, t *Tunnel) (*forward, error) {
	ln, err := client.Listen("tcp", bindAddr(t))
	if err != nil {
		return nil, fmt.Errorf("远端监听 %s 请求失败(绑定 0.0.0.0 需 sshd 开启 GatewayPorts):%v", bindAddr(t), err)
	}
	dest := net.JoinHostPort(t.DestHost, strconv.Itoa(t.DestPort))
	go acceptLoop(ln, func(c net.Conn) {
		local, err := net.Dial("tcp", dest)
		if err != nil {
			_ = c.Close()
			return
		}
		pipeBoth(c, local)
	})
	return newForward(ln), nil
}

// startDynamicForward(-D):本机监听 bind 充当 SOCKS5 入口;每条 SOCKS CONNECT
// 经远端 sshd 连接目标(域名由远端解析,同 ssh -D)。无认证、仅 CONNECT——仅本机
// 使用,bind 默认 127.0.0.1(防代理裸奔)。
func startDynamicForward(client target.SSHClient, t *Tunnel) (*forward, error) {
	ln, err := net.Listen("tcp", bindAddr(t))
	if err != nil {
		return nil, fmt.Errorf("本地监听 %s 失败:%v", bindAddr(t), err)
	}
	go acceptLoop(ln, func(c net.Conn) {
		serveSOCKS5(c, client)
	})
	return newForward(ln), nil
}

// newForward 从 listener 提取实际端点(x/crypto 的远端 listener 同样以 *net.TCPAddr
// 汇报 sshd 实际绑定地址)。
func newForward(ln net.Listener) *forward {
	f := &forward{closers: []io.Closer{ln}}
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		f.actualPort = ta.Port
		host := ta.IP.String()
		if host == "" || host == "<nil>" {
			host = "127.0.0.1"
		}
		f.endpoint = net.JoinHostPort(host, strconv.Itoa(ta.Port))
	} else {
		f.endpoint = ln.Addr().String()
	}
	return f
}

// bindAddr 是监听地址(bind_port=0 = 自动分配)。
func bindAddr(t *Tunnel) string {
	host := t.BindHost
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(t.BindPort))
}

// acceptLoop 持续接收连接并逐连接开 goroutine 处理;listener 关闭即退出,
// 瞬时错误小睡重试(不因一阵 EMFILE 就整条转发面静默死亡)。
func acceptLoop(ln net.Listener, handle func(net.Conn)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handle(c)
	}
}

// pipeBoth 双向搬运字节,任一侧结束即关闭两侧(隧道转发的全双工管道)。
func pipeBoth(a, b net.Conn) {
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(a, b)
		done <- struct{}{}
	}()
	_, _ = io.Copy(b, a)
	<-done
}
