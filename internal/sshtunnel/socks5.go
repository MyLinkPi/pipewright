package sshtunnel

import (
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// serveSOCKS5 在单条连接上跑最小 SOCKS5 服务端(dynamic -D 模式):
//   - 握手:仅支持无认证(METHOD 0x00);
//   - 请求:仅支持 CONNECT(0x01),目标支持 IPv4 / 域名 / IPv6;
//   - 数据:成功回复后与远端(经 sshd 的 direct-tcpip)双向直通。
//
// 握手阶段设 30s 读超时防慢速挂死;管道阶段取消超时。不引第三方依赖(约 150 行)。
func serveSOCKS5(c net.Conn, client target.SSHClient) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))

	// --- 握手:VER NMETHODS METHODS ---
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return
	}
	if hdr[0] != 5 {
		return // 不是 SOCKS5
	}
	methods := make([]byte, int(hdr[1]))
	if hdr[1] > 0 {
		if _, err := io.ReadFull(c, methods); err != nil {
			return
		}
	}
	noAuth := false
	for _, m := range methods {
		if m == 0x00 {
			noAuth = true
			break
		}
	}
	if !noAuth {
		_, _ = c.Write([]byte{0x05, 0xFF}) // 无可接受认证法
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// --- 请求:VER CMD RSV ATYP DST.ADDR DST.PORT ---
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return
	}
	if req[0] != 5 || req[1] != 0x01 { // 仅 CONNECT
		_, _ = c.Write(socks5Reply(0x07)) // command not supported
		return
	}
	host, ok := readSOCKSAddr(c, req[3])
	if !ok {
		_, _ = c.Write(socks5Reply(0x08)) // address type not supported
		return
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(c, portBytes[:]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBytes[:])

	remote, err := client.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		_, _ = c.Write(socks5Reply(0x01)) // general SOCKS server failure
		return
	}
	defer func() { _ = remote.Close() }()

	// 成功:BND.ADDR = 0.0.0.0、BND.PORT = 0(RFC 1928 允许;实际端点由后续流量体现)。
	if _, err := c.Write(socks5Reply(0x00)); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{}) // 管道阶段不再超时
	pipeBoth(c, remote)
}

// readSOCKSAddr 按 ATYP 读取目标地址(IPv4 / 域名 / IPv6)。
func readSOCKSAddr(r io.Reader, atyp byte) (string, bool) {
	switch atyp {
	case 0x01: // IPv4
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", false
		}
		return net.IP(b[:]).String(), true
	case 0x03: // 域名(远端 sshd 解析)
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil || l[0] == 0 {
			return "", false
		}
		name := make([]byte, int(l[0]))
		if _, err := io.ReadFull(r, name); err != nil {
			return "", false
		}
		return string(name), true
	case 0x04: // IPv6
		var b [16]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", false
		}
		return net.IP(b[:]).String(), true
	default:
		return "", false
	}
}

// socks5Reply 构造 5 字节头 + 0.0.0.0:0 的回复(REP 由调用方给)。
func socks5Reply(rep byte) []byte {
	return []byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
}
