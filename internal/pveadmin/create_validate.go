// create_validate.go — 建机入参白名单校验与缺省归一(保存期风格:只校形态,不触网)。
package pveadmin

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

// machineRe 是 PVE 机型白名单(q35 / pc-q35-9.0 / virt 等)。
var machineRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,31}$`)

// volidLikeRe 是存储卷/绝对路径的粗形校验(无空白与控制字符)。
var volidLikeRe = regexp.MustCompile(`^[^\s\x00-\x1f]{1,512}$`)

func validateCreateInput(in CreateInput) (CreateInput, error) {
	switch in.Mode {
	case CreateModeClone, CreateModeImage, CreateModeLXC:
	default:
		return in, fmt.Errorf("%w: 未知建机模式 %q(clone|image|lxc)", ErrInvalidInput, in.Mode)
	}
	in.Name = strings.TrimSpace(in.Name)
	if !nameRe.MatchString(in.Name) {
		return in, fmt.Errorf("%w: 实例名不合规", ErrInvalidInput)
	}
	if in.Count <= 0 {
		in.Count = 1
	}
	if in.Count > MaxCreateBatch {
		return in, fmt.Errorf("%w: 批量台数上限 %d", ErrInvalidInput, MaxCreateBatch)
	}
	if in.VMID != 0 && (in.VMID < 100 || in.VMID > 999999999) {
		return in, fmt.Errorf("%w: vmid 越界", ErrInvalidInput)
	}
	if in.Node != "" && !nameRe.MatchString(in.Node) {
		return in, fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}
	// clone+模板:节点取模板所在节点(管线内解析);其余形态节点必填。
	if in.Node == "" && !(in.Mode == CreateModeClone && in.TemplateVMID > 0) {
		return in, fmt.Errorf("%w: 节点不能为空", ErrInvalidInput)
	}
	if in.Storage != "" && !nameRe.MatchString(in.Storage) {
		return in, fmt.Errorf("%w: 存储名不合规", ErrInvalidInput)
	}

	switch in.Mode {
	case CreateModeClone:
		if (in.TemplateVMID > 0) == (in.BackupVolid != "") {
			return in, fmt.Errorf("%w: 克隆源须为模板 VMID 或 .vma 备份卷二选一", ErrInvalidInput)
		}
		if in.TemplateVMID != 0 && (in.TemplateVMID < 100 || in.TemplateVMID > 999999999) {
			return in, fmt.Errorf("%w: 模板 vmid 越界", ErrInvalidInput)
		}
		if in.BackupVolid != "" && !volidLikeRe.MatchString(in.BackupVolid) {
			return in, fmt.Errorf("%w: 备份卷/路径不合规", ErrInvalidInput)
		}
	case CreateModeImage:
		if (in.ImageURL != "") == (in.ImageVolid != "") {
			return in, fmt.Errorf("%w: 镜像源须为 URL 或已有卷二选一", ErrInvalidInput)
		}
		if in.ImageURL != "" {
			u, err := url.Parse(in.ImageURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return in, fmt.Errorf("%w: 镜像 URL 须为 http(s) 地址", ErrInvalidInput)
			}
		}
		if in.ImageVolid != "" && !volidLikeRe.MatchString(in.ImageVolid) {
			return in, fmt.Errorf("%w: 镜像卷不合规", ErrInvalidInput)
		}
		if in.Storage == "" {
			return in, fmt.Errorf("%w: 云镜像建机须指定目标存储", ErrInvalidInput)
		}
	case CreateModeLXC:
		if (in.OSTemplate != "") == (in.AplDownload != nil) {
			return in, fmt.Errorf("%w: LXC 源须为已有模板卷或在线下载二选一", ErrInvalidInput)
		}
		if in.OSTemplate != "" && !volidLikeRe.MatchString(in.OSTemplate) {
			return in, fmt.Errorf("%w: 模板卷不合规", ErrInvalidInput)
		}
		if in.AplDownload != nil {
			if !nameRe.MatchString(in.AplDownload.Storage) || !aplTemplateRe.MatchString(in.AplDownload.Template) {
				return in, fmt.Errorf("%w: 在线下载存储/模板标识不合规", ErrInvalidInput)
			}
		}
	}

	if in.Cores == 0 {
		in.Cores = 2
	}
	if in.Cores < 1 || in.Cores > 256 {
		return in, fmt.Errorf("%w: CPU 核数须 1..256", ErrInvalidInput)
	}
	if in.Memory == 0 {
		in.Memory = 2048
	}
	if in.Memory < 128 || in.Memory > 1048576 {
		return in, fmt.Errorf("%w: 内存须 128..1048576 MB", ErrInvalidInput)
	}
	if in.DiskGB < 0 || in.DiskGB > 65536 {
		return in, fmt.Errorf("%w: 系统盘须 0..65536 GB", ErrInvalidInput)
	}
	if in.Mode == CreateModeImage && in.DiskGB == 0 {
		in.DiskGB = 10
	}
	if in.Mode == CreateModeLXC {
		if in.Storage == "" {
			in.Storage = "local"
		}
		if in.DiskGB == 0 {
			in.DiskGB = 8
		}
	}
	if in.DataGB < 0 || in.DataGB > 65536 {
		return in, fmt.Errorf("%w: 数据盘须 0..65536 GB", ErrInvalidInput)
	}
	if in.DataGB > 0 && in.DataStorage == "" {
		in.DataStorage = in.Storage
	}
	if in.DataStorage != "" && !nameRe.MatchString(in.DataStorage) {
		return in, fmt.Errorf("%w: 数据盘存储名不合规", ErrInvalidInput)
	}

	if in.Bridge == "" {
		in.Bridge = "vmbr0"
	}
	if !nameRe.MatchString(in.Bridge) {
		return in, fmt.Errorf("%w: 网桥名不合规", ErrInvalidInput)
	}
	if in.IPMode == "" {
		in.IPMode = "dhcp"
	}
	if in.IPMode != "dhcp" && in.IPMode != "static" {
		return in, fmt.Errorf("%w: IP 模式须为 dhcp|static", ErrInvalidInput)
	}
	if in.IPMode == "static" {
		addr, err := netip.ParseAddr(strings.TrimSpace(in.IP))
		if err != nil || !addr.Is4() {
			return in, fmt.Errorf("%w: 静态 IP 须为合法 IPv4 基址", ErrInvalidInput)
		}
		in.IP = addr.String()
		if in.Prefix == 0 {
			in.Prefix = 24
		}
		if in.Prefix < 1 || in.Prefix > 32 {
			return in, fmt.Errorf("%w: 掩码须 1..32", ErrInvalidInput)
		}
		if in.Gateway != "" && net.ParseIP(in.Gateway) == nil {
			return in, fmt.Errorf("%w: 网关地址非法", ErrInvalidInput)
		}
	}
	for _, d := range dnsList(in.DNS) {
		if net.ParseIP(d) == nil {
			return in, fmt.Errorf("%w: DNS 服务器地址非法 %q", ErrInvalidInput, d)
		}
	}

	if in.CIUser != "" && !ciSafeRe.MatchString(in.CIUser) {
		return in, fmt.Errorf("%w: cloud-init 用户名不合规", ErrInvalidInput)
	}
	if strings.ContainsAny(in.CIPassword, "\r\n\x00") || len(in.CIPassword) > 256 {
		return in, fmt.Errorf("%w: cloud-init 密码含换行/控制字符或超长", ErrInvalidInput)
	}
	for _, k := range sshKeyList(in.SSHKeys) {
		if strings.ContainsAny(k, "\r") || !hasSSHKeyPrefix(k) {
			return in, fmt.Errorf("%w: SSH 公钥格式非法(须单行且以合法算法前缀开头)", ErrInvalidInput)
		}
	}
	if in.Timezone != "" && !ciTZRe.MatchString(in.Timezone) {
		return in, fmt.Errorf("%w: 时区不合规", ErrInvalidInput)
	}
	if in.Mirror != "" && in.Mirror != "none" {
		if _, ok := mirrorHosts[in.Mirror]; !ok {
			return in, fmt.Errorf("%w: 未知换源预设 %q", ErrInvalidInput, in.Mirror)
		}
	}
	if in.BIOS != "" && in.BIOS != "seabios" && in.BIOS != "ovmf" {
		return in, fmt.Errorf("%w: BIOS 须为 seabios|ovmf", ErrInvalidInput)
	}
	if in.Machine != "" && !machineRe.MatchString(in.Machine) {
		return in, fmt.Errorf("%w: 机型不合规", ErrInvalidInput)
	}
	if strings.ContainsAny(in.LXCPassword, "\r\n\x00") || len(in.LXCPassword) > 256 {
		return in, fmt.Errorf("%w: LXC 密码含换行/控制字符或超长", ErrInvalidInput)
	}
	if in.Register != nil {
		if err := validateRegisterInput(in.Register); err != nil {
			return in, err
		}
	}
	return in, nil
}
