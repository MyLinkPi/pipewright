package sshtunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// OpenSSH tun@openssh.com(ssh -w 数据面)常量。线上格式(与 OpenSSH 源码
// session.c/tun.c 对齐):每条 session 通道消息 = 4 字节大端地址族 + 一个 IP 包,
// 消息边界即包边界;地址族取 OpenBSD tun(4) 规范值(Linux 等平台由 sshd 归一化)。
const (
	// tunModePointToPoint 是 tun@openssh.com 载荷的 mode 字段值(L3 点对点)。
	tunModePointToPoint = 1
	// wireAFInet/wireAFInet6 是线上地址族规范值(2=IPv4,24=OpenBSD AF_INET6);
	// wireAFInet6Linux 是 Linux 原生 AF_INET6(读侧宽松兼容非规范对端)。
	wireAFInet       = 2
	wireAFInet6      = 24
	wireAFInet6Linux = 10
	// vpnMTU 是两端 tun 设备的 MTU(SSH 包头开销留余量,避免分片放大)。
	vpnMTU = 1400
)

// startVPNForward 建立 ssh -w 点对点 VPN 隧道:
//  1. 经远端 sshd 发 tun@openssh.com 请求(需 PermitTunnel;sshd 创建 tun<remote>);
//  2. 经同一连接跑 `ip addr add … peer …` 配置远端 IP(目标机通常需 root);
//  3. 本机 /dev/net/tun 创建 tun<local>(仅 Linux,需 root/CAP_NET_ADMIN)并配 IP;
//  4. 双向包泵(4 字节地址族头 ↔ 原始 IP 包),任一侧故障通知守望循环重连。
//
// 不推荐用此模式:一般场景请用平台内置的集群自组网(EasyTier,免 root、自动分配
// 虚拟 IP、支持子网路由与故障切换);前端表单有对应警示。
func startVPNForward(client target.SSHClient, t *Tunnel) (*forward, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("VPN 隧道(-w)仅支持 Linux 控制节点(建议改用集群自组网)")
	}
	ch, err := client.OpenTunChannel(t.BindPort, t.DestPort)
	if err != nil {
		return nil, err
	}
	// 远端配 IP:同一 SSH 连接开 session 执行(命令只含已校验的 IP/设备名,仍逐段转义)。
	remoteTun := "tun" + strconv.Itoa(t.DestPort)
	remoteCmd := fmt.Sprintf("ip addr add %s peer %s/30 dev %s && ip link set %s mtu %d up",
		shQuote(t.DestHost), shQuote(t.BindHost), shQuote(remoteTun), shQuote(remoteTun), vpnMTU)
	if _, err := client.Exec(remoteCmd); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("配置远端 tun IP 失败(目标机执行 %q 通常需要 root):%v", remoteCmd, err)
	}
	// 本机 tun 设备(仅 Linux;Windows/macOS 在此报错,见 vpn_tun_*.go)。
	tunFile, localTun, err := createTunDevice(t.BindPort)
	if err != nil {
		_ = ch.Close()
		return nil, err
	}
	if out, err := runIP([]string{"addr", "add", t.BindHost, "peer", t.DestHost + "/30", "dev", localTun}); err != nil {
		_ = ch.Close()
		_ = tunFile.Close()
		return nil, fmt.Errorf("配置本机 tun IP 失败(控制节点通常需要 root):%v — %s", err, out)
	}
	if _, err := runIP([]string{"link", "set", localTun, "mtu", strconv.Itoa(vpnMTU), "up"}); err != nil {
		_ = ch.Close()
		_ = tunFile.Close()
		return nil, fmt.Errorf("拉起本机 %s 失败:%v", localTun, err)
	}
	f := &forward{
		endpoint:   fmt.Sprintf("%s(tun%d) ⇄ %s(tun%d)", t.BindHost, t.BindPort, t.DestHost, t.DestPort),
		actualPort: t.BindPort,
		dead:       make(chan error, 1),
		closers:    []io.Closer{ch, tunFile},
	}
	go pumpTunToChannel(tunFile, ch, f)
	go pumpChannelToTun(ch, tunFile, f)
	return f, nil
}

// runIP 在本机执行 `ip <args>`(二进制/systemd 部署的控制节点必带 iproute2;
// distroless 容器没有 ip 命令 —— VPN+Docker 本就是不推荐组合,报错即止)。
func runIP(args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	return string(out), err
}

// shQuote 复刻 target 包的 POSIX 单引号转义(该包未导出;命令只含 IP/设备名,
// 转义是纵深防御)。
func shQuote(s string) string {
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '=') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + replaceAllSingleQuotes(s) + "'"
}

func replaceAllSingleQuotes(s string) string {
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, []byte(`'\''`)...)
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

// pumpTunToChannel 从本机 tun 设备读原始 IP 包,补 4 字节线上地址族头后写入
// SSH 通道(一次 Write = 一条通道消息 = 一个包)。非 IPv4/6 帧(如 ARP)静默丢弃。
func pumpTunToChannel(tun io.Reader, ch io.WriteCloser, f *forward) {
	pkt := make([]byte, 65535)
	for {
		n, err := tun.Read(pkt)
		if err != nil {
			f.fail(err)
			return
		}
		if n == 0 {
			continue
		}
		fam := tunFamily(pkt[:n])
		if fam == 0 {
			continue
		}
		out := make([]byte, 4+n)
		binary.BigEndian.PutUint32(out, fam)
		copy(out[4:], pkt[:n])
		if _, err := ch.Write(out); err != nil {
			f.fail(err)
			return
		}
	}
}

// pumpChannelToTun 从 SSH 通道读线上格式(4 字节地址族 + IP 包)写入本机 tun 设备。
// IP 包长度从包头自解析(IPv4 total-length / IPv6 payload-length),不依赖
// 消息边界对齐 Read 的粒度(x/crypto/ssh 单次 Read 不跨消息,ReadFull 兜底)。
func pumpChannelToTun(ch io.Reader, tun io.Writer, f *forward) {
	hdr := make([]byte, 4)
	head := make([]byte, 40) // v4/v6 固定头大小
	for {
		if _, err := io.ReadFull(ch, hdr); err != nil {
			f.fail(err)
			return
		}
		fam := binary.BigEndian.Uint32(hdr)
		if fam != wireAFInet && fam != wireAFInet6 && fam != wireAFInet6Linux {
			f.fail(fmt.Errorf("tun 通道出现未知地址族 %d(对端不是 OpenSSH tun 格式?)", fam))
			return
		}
		headLen := 20
		if fam != wireAFInet {
			headLen = 40
		}
		if _, err := io.ReadFull(ch, head[:headLen]); err != nil {
			f.fail(err)
			return
		}
		total := 0
		if fam == wireAFInet {
			total = int(binary.BigEndian.Uint16(head[2:4]))
		} else {
			payload := int(binary.BigEndian.Uint16(head[4:6]))
			if payload == 0 {
				f.fail(errors.New("收到 IPv6 jumbo 包(超过 65535 字节),不支持"))
				return
			}
			total = 40 + payload
		}
		if total < headLen || total > 65535 {
			f.fail(fmt.Errorf("tun 通道包长度异常:%d", total))
			return
		}
		buf := make([]byte, total)
		copy(buf, head[:headLen])
		if _, err := io.ReadFull(ch, buf[headLen:]); err != nil {
			f.fail(err)
			return
		}
		if _, err := tun.Write(buf); err != nil {
			f.fail(err)
			return
		}
	}
}

// tunFamily 按 IP 版本号嗅探包的线上地址族(非 v4/v6 返回 0 = 丢弃)。
func tunFamily(pkt []byte) uint32 {
	if len(pkt) == 0 {
		return 0
	}
	switch pkt[0] >> 4 {
	case 4:
		return wireAFInet
	case 6:
		return wireAFInet6
	}
	return 0
}
