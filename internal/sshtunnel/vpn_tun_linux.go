//go:build linux

package sshtunnel

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TUNSETIFF 是 linux/if_tun.h 的 _IOW('T', 202, int),int 尺寸参数使其在
// 32/64 位架构上同值。IFF_TUN = L3 层 tun(非 tap);IFF_NO_PI 去掉 4 字节
// 包信息头 —— 与 OpenSSH tun 线上格式(自带地址族头)对齐。
const (
	tunSetIFF = 0x400454ca
	iffTun    = 0x0001
	iffNoPI   = 0x1000
)

// tunReq 是 ifreq 的 tun 子集(设备名 + 标志;与 linux/if_tun.h 的 ifreq 布局对齐)。
type tunReq struct {
	Name  [16]byte
	Flags uint16
	_     [22]byte
}

// createTunDevice 创建(或接管)本机 tun<idx> 设备:打开 /dev/net/tun 并
// TUNSETIFF 绑定指定设备名。需要 root 或 CAP_NET_ADMIN;设备名已被占用
//(其它隧道/进程)时报 EBUSY。返回的 *os.File 关闭即销毁设备。
func createTunDevice(idx int) (*os.File, string, error) {
	name := fmt.Sprintf("tun%d", idx)
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return nil, "", fmt.Errorf("打开 /dev/net/tun 失败(VPN 模式仅支持 Linux,且需要 root 或 CAP_NET_ADMIN):%v", err)
	}
	req := &tunReq{}
	copy(req.Name[:], name)
	req.Flags = iffTun | iffNoPI
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), tunSetIFF, uintptr(unsafe.Pointer(req))); errno != 0 {
		_ = unix.Close(fd)
		if errno == syscall.EBUSY {
			return nil, "", fmt.Errorf("tun 设备 %s 已被占用(其它隧道或进程正在使用)", name)
		}
		return nil, "", fmt.Errorf("TUNSETIFF %s 失败:%v", name, errno)
	}
	return os.NewFile(uintptr(fd), name), name, nil
}
