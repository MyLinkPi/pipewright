// cloudinit.go — 云镜像建机的 #cloud-config user-data 生成(纯函数,golden 可测)。
//
// PVE API 无法上传 snippet 文件,故 user-data 由本包生成后经 SSH(Runner.WriteFile)
// 落到节点 snippets 存储,再以 cicustom user=<sid>:snippets/<file> 引用。
//
// 行为要点(净室实现,不复制外部 GPL 代码):
//   - 恒定注入 hostname + preserve_hostname:false + runcmd hostnamectl 兜底,保证
//     「PVE 名称 == guest 主机名」(部分镜像 cloud.cfg 带 preserve_hostname:true);
//   - runcmd 顺序:DNS 健全性 → 换源(必须排在装 agent 之前,否则走国外官方源超时)
//     → hostnamectl → ssh 重启 → 装并启动 qemu-guest-agent(apt/dnf/yum 三路探测,
//     static 单元 start+enable)→ 关 SELinux/防火墙(可选)→ 扩根(可选)→ DHCP 固化(可选);
//   - 密码/密钥只进 YAML 与 SSH stdin,绝不进任务日志/审计/错误体。
package pveadmin

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// CloudInitOptions 是 cloud-init 定制开关集(云镜像建机与带 cloudinit 盘的模板克隆共用)。
type CloudInitOptions struct {
	Hostname        string   // 必填;guest 主机名(与 PVE 名称一致)
	User            string   // 登录用户(空 → root)
	Password        string   // 登录密码(空 → 锁定密码,仅密钥登录)
	SSHKeys         []string // SSH 公钥(每行一条)
	SSHPasswordAuth bool     // 允许密码登录(附 sshd 配置 + 重启 ssh)
	Timezone        string   // 空 → Asia/Shanghai
	DNS             []string // 空 → 223.5.5.5,119.29.29.29
	Mirror          string   // 换源预设:none/ustc/tsinghua/aliyun
	InstallAgent    bool     // 虚机内装并启动 qemu-guest-agent
	DisableSELinux  bool     // 关 SELinux(RHEL 系)
	DisableFirewall bool     // 停用 firewalld/ufw
	RootExpand      bool     // 首次开机扩根(分区/LVM/文件系统)
	FixStatic       bool     // DHCP 地址固化静态(仅 DHCP 模式有意义)
}

// mirrorHosts 是换源预设 → 镜像站域名。
var mirrorHosts = map[string]string{
	"ustc":     "mirrors.ustc.edu.cn",
	"tsinghua": "mirrors.tuna.tsinghua.edu.cn",
	"aliyun":   "mirrors.aliyun.com",
}

// ciSafeRe 是 hostname/用户名白名单(字母数字开头,常规字符)。
var ciSafeRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// ciTZRe 是时区白名单(Region/City 形态)。
var ciTZRe = regexp.MustCompile(`^[A-Za-z0-9_+/-]{1,64}$`)

// sshKeyPrefixes 是合法公钥算法前缀。
var sshKeyPrefixes = []string{"ssh-rsa ", "ssh-ed25519 ", "ecdsa-sha2-nistp256 ", "ecdsa-sha2-nistp384 ", "ecdsa-sha2-nistp521 ", "sk-ssh-ed25519@openssh.com ", "sk-ecdsa-sha2-nistp256@openssh.com "}

// cloudInitSnippetName 是任务级唯一的 snippet 文件名,避免覆盖已有 VM 的 cloud-init 源。
func cloudInitSnippetName(vmid int64, jobID string) string {
	return fmt.Sprintf("cloud-%s-%d-user.yaml", jobID, vmid)
}

// renderCloudConfig 生成 #cloud-config YAML。入参白名单校验;密码/密钥安全转义。
func renderCloudConfig(o CloudInitOptions) (string, error) {
	if !ciSafeRe.MatchString(o.Hostname) {
		return "", fmt.Errorf("%w: cloud-init 主机名不合规", ErrInvalidInput)
	}
	user := o.User
	if user == "" {
		user = "root"
	}
	if !ciSafeRe.MatchString(user) {
		return "", fmt.Errorf("%w: cloud-init 用户名不合规", ErrInvalidInput)
	}
	if strings.ContainsAny(o.Password, "\r\n\x00") || len(o.Password) > 256 {
		return "", fmt.Errorf("%w: cloud-init 密码含换行/控制字符或超长", ErrInvalidInput)
	}
	tz := o.Timezone
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	if !ciTZRe.MatchString(tz) {
		return "", fmt.Errorf("%w: 时区不合规", ErrInvalidInput)
	}
	dns := o.DNS
	if len(dns) == 0 {
		dns = []string{"223.5.5.5", "119.29.29.29"}
	}
	for _, d := range dns {
		if net.ParseIP(d) == nil {
			return "", fmt.Errorf("%w: DNS 服务器地址非法 %q", ErrInvalidInput, d)
		}
	}
	for _, k := range o.SSHKeys {
		if strings.ContainsAny(k, "\r\n") || !hasSSHKeyPrefix(k) {
			return "", fmt.Errorf("%w: SSH 公钥格式非法(须单行且以合法算法前缀开头)", ErrInvalidInput)
		}
	}
	mirror := o.Mirror
	if mirror != "" && mirror != "none" {
		if _, ok := mirrorHosts[mirror]; !ok {
			return "", fmt.Errorf("%w: 未知换源预设 %q", ErrInvalidInput, mirror)
		}
	}

	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("hostname: " + o.Hostname + "\n")
	b.WriteString("preserve_hostname: false\n")
	b.WriteString("timezone: " + tz + "\n")
	b.WriteString("manage_etc_hosts: true\n")
	b.WriteString("manage_resolv_conf: true\n")
	b.WriteString("resolv_conf:\n  nameservers:\n")
	for _, d := range dns {
		b.WriteString("    - " + d + "\n")
	}
	if o.SSHPasswordAuth {
		b.WriteString("ssh_pwauth: true\n")
	}
	b.WriteString("users:\n  - name: " + user + "\n")
	if o.Password != "" {
		b.WriteString("    lock_passwd: false\n")
	} else {
		b.WriteString("    lock_passwd: true\n")
	}
	b.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
	if len(o.SSHKeys) > 0 {
		b.WriteString("    ssh_authorized_keys:\n")
		for _, k := range o.SSHKeys {
			b.WriteString("      - " + yamlDoubleQuote(k) + "\n")
		}
	}
	if o.Password != "" {
		b.WriteString("chpasswd:\n  list: |\n")
		b.WriteString("    " + user + ":" + o.Password + "\n")
		b.WriteString("  expire: false\n")
	}
	if o.SSHPasswordAuth {
		b.WriteString("write_files:\n  - path: /etc/ssh/sshd_config.d/99-pipewright-pwauth.conf\n    content: |\n")
		b.WriteString("      PasswordAuthentication yes\n      PermitRootLogin yes\n")
	}

	// runcmd(顺序敏感:换源先于装 agent)。
	var cmds []string
	cmds = append(cmds, strings.ReplaceAll(ciDNSSanityScript, "__DNS__", strings.Join(dns, " ")))
	if mirror != "" && mirror != "none" {
		cmds = append(cmds, strings.ReplaceAll(ciMirrorScript, "__MIRROR__", mirrorHosts[mirror]))
	}
	cmds = append(cmds, "hostnamectl set-hostname "+o.Hostname+" >/dev/null 2>&1 || hostname "+o.Hostname)
	if o.SSHPasswordAuth {
		cmds = append(cmds, "systemctl restart ssh || service ssh restart")
	}
	if o.InstallAgent {
		cmds = append(cmds, ciAgentScript)
	}
	if o.DisableSELinux {
		cmds = append(cmds, ciSELinuxScript)
	}
	if o.DisableFirewall {
		cmds = append(cmds, ciFirewallScript)
	}
	if o.RootExpand {
		cmds = append(cmds, ciRootExpandScript)
	}
	if o.FixStatic {
		cmds = append(cmds, ciFixStaticScript)
	}
	if len(cmds) > 0 {
		b.WriteString("runcmd:\n")
		for _, cmd := range cmds {
			if strings.Contains(cmd, "\n") {
				b.WriteString("  - |\n")
				for _, line := range strings.Split(strings.TrimRight(cmd, "\n"), "\n") {
					b.WriteString("    " + line + "\n")
				}
			} else {
				b.WriteString("  - " + cmd + "\n")
			}
		}
	}
	return b.String(), nil
}

func hasSSHKeyPrefix(k string) bool {
	for _, p := range sshKeyPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// yamlDoubleQuote 把字符串包成 YAML 双引号标量(转义反斜杠与双引号)。
func yamlDoubleQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// ---- runcmd shell 片段(固定脚本,占位符仅 __DNS__/__MIRROR__;绝不拼用户输入) --------------

// ciDNSSanityScript DNS 解析失败时重置 resolv.conf(换源/装包的前置)。
const ciDNSSanityScript = `if ! getent hosts deb.debian.org >/dev/null 2>&1; then
  rm -f /etc/resolv.conf
  for d in __DNS__; do printf 'nameserver %s\n' "$d" >> /etc/resolv.conf; done
  echo '[net] DNS 解析失败,已重置 /etc/resolv.conf'
fi`

// ciMirrorScript apt/dnf/yum 三路换源(必须排在装 agent 之前)。
const ciMirrorScript = `if command -v apt-get >/dev/null 2>&1; then
  sed -i 's|deb.debian.org|__MIRROR__|g; s|security.debian.org|__MIRROR__|g; s|archive.ubuntu.com|__MIRROR__|g; s|security.ubuntu.com|__MIRROR__|g; s|ports.ubuntu.com|__MIRROR__|g' /etc/apt/sources.list /etc/apt/sources.list.d/*.sources /etc/apt/sources.list.d/*.list 2>/dev/null || true
  apt-get update -o Acquire::Retries=3 -o Acquire::http::Timeout=20 || true
elif command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
  sed -i 's|^mirrorlist=|#mirrorlist=|g; s|^#baseurl=|baseurl=|g; s|mirror.centos.org|__MIRROR__|g; s|dl.rockylinux.org|__MIRROR__|g; s|repo.almalinux.org|__MIRROR__|g' /etc/yum.repos.d/*.repo 2>/dev/null || true
fi`

// ciAgentScript 装并启动 qemu-guest-agent(包管理器三路探测;static 单元 start 拉起)。
const ciAgentScript = `ga_bin=$(command -v qemu-ga 2>/dev/null || ls /usr/bin/qemu-ga /usr/libexec/qemu-ga /usr/sbin/qemu-ga 2>/dev/null | head -1 || true)
if [ -z "$ga_bin" ]; then
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -o Acquire::Retries=3 -o Acquire::http::Timeout=20 || true
    apt-get install -y qemu-guest-agent || echo '[agent] 安装失败(apt)'
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y qemu-guest-agent || echo '[agent] 安装失败(dnf)'
  elif command -v yum >/dev/null 2>&1; then
    yum install -y qemu-guest-agent || echo '[agent] 安装失败(yum)'
  else
    echo '[agent] 未找到可用包管理器(apt/dnf/yum),跳过安装'
  fi
fi
systemctl start qemu-guest-agent 2>/dev/null || true
systemctl enable qemu-guest-agent >/dev/null 2>&1 || true
ga_bin=$(command -v qemu-ga 2>/dev/null || ls /usr/bin/qemu-ga /usr/libexec/qemu-ga /usr/sbin/qemu-ga 2>/dev/null | head -1 || true)
if [ -n "$ga_bin" ]; then echo '[agent] qemu-guest-agent 已安装并启动'; else echo '[agent] 未安装成功,PVE 将无法经代理读取虚机信息'; fi`

// ciSELinuxScript 关 SELinux(持久 + 立即)。
const ciSELinuxScript = `if [ -f /etc/selinux/config ]; then
  sed -i 's/^SELINUX=.*/SELINUX=disabled/' /etc/selinux/config
  setenforce 0 2>/dev/null || true
fi`

// ciFirewallScript 停用 firewalld(RHEL 系)/ufw(Debian 系)。
const ciFirewallScript = `systemctl disable --now firewalld 2>/dev/null || true
systemctl disable --now ufw 2>/dev/null || true`

// ciRootExpandScript 首次开机扩根(幂等):OpenWrt(overlay/ext4)专用分支 + 通用 Linux
// (growpart 扩分区 → pvresize/lvextend 扩 LVM → resize2fs/xfs_growfs 扩文件系统)。
const ciRootExpandScript = `if [ -f /etc/openwrt_release ] || command -v opkg >/dev/null 2>&1; then
  OW_TGT=$(awk '$2=="/overlay"{print $1; exit}' /proc/mounts 2>/dev/null)
  [ -n "$OW_TGT" ] || OW_TGT=$(awk '$2=="/"{print $1; exit}' /proc/mounts 2>/dev/null)
  case "$OW_TGT" in /dev/root|/dev/root/*) OW_TGT=$(readlink -f /dev/root 2>/dev/null || echo "$OW_TGT") ;; esac
  case "$OW_TGT" in /dev/*) ;; *) echo '[disk] openwrt: 无法定位底层分区,跳过'; exit 0 ;; esac
  OW_PART=${OW_TGT##*/}
  OW_NUM=$(cat "/sys/class/block/$OW_PART/partition" 2>/dev/null)
  OW_DISK=$(basename "$(dirname "$(readlink -f "/sys/class/block/$OW_PART" 2>/dev/null || echo "")")" 2>/dev/null)
  case "$OW_DISK" in ''|.) OW_DISK="" ;; esac
  [ -n "$OW_DISK" ] || OW_DISK=$(echo "${OW_TGT#/dev/}" | sed -E 's/p?[0-9]+$//')
  if [ -z "$OW_NUM" ] || [ -z "$OW_DISK" ] || [ ! -e "/dev/$OW_DISK" ]; then
    echo '[disk] openwrt: 无法确定磁盘/分区号,跳过'; exit 0
  fi
  if ! command -v growpart >/dev/null 2>&1 && ! command -v parted >/dev/null 2>&1; then
    command -v opkg >/dev/null 2>&1 && { opkg update >/dev/null 2>&1 || true; opkg install parted >/dev/null 2>&1 || true; }
  fi
  if command -v growpart >/dev/null 2>&1; then
    growpart "/dev/$OW_DISK" "$OW_NUM" >/dev/null 2>&1 || true
  elif command -v parted >/dev/null 2>&1; then
    parted -s "/dev/$OW_DISK" resizepart "$OW_NUM" 100% >/dev/null 2>&1 || true
    partx -u "/dev/$OW_DISK" >/dev/null 2>&1 || blockdev --rereadpt "/dev/$OW_DISK" >/dev/null 2>&1 || true
  fi
  OW_FS=$(awk -v d="$OW_TGT" '$1==d{print $3; exit}' /proc/mounts 2>/dev/null)
  [ -n "$OW_FS" ] || OW_FS=$(blkid "$OW_TGT" 2>/dev/null | sed -n 's/.*TYPE="\([^"]*\)".*/\1/p')
  case "$OW_FS" in
    ext2|ext3|ext4) command -v resize2fs >/dev/null 2>&1 || { command -v opkg >/dev/null 2>&1 && opkg install e2fsprogs >/dev/null 2>&1 || true; }; resize2fs "$OW_TGT" >/dev/null 2>&1 || true ;;
    f2fs) command -v resize.f2fs >/dev/null 2>&1 || { command -v opkg >/dev/null 2>&1 && opkg install f2fs-tools >/dev/null 2>&1 || true; }; resize.f2fs "$OW_TGT" >/dev/null 2>&1 || true ;;
    *) echo "[disk] openwrt: 未识别文件系统(${OW_FS:-未知}),分区已扩但文件系统未扩" ;;
  esac
  exit 0
fi
ROOT_SRC=$(findmnt -n -o SOURCE / | sed 's/\[.*//')
BEFORE=$(findmnt -n -o SIZE /)
case "$ROOT_SRC" in /dev/*) ;; *) echo "[disk] 跳过:无法识别根设备($ROOT_SRC)"; exit 0 ;; esac
PART=$(lsblk -nso NAME,TYPE "$ROOT_SRC" 2>/dev/null | awk '$2=="part"{print $1; exit}')
if [ -n "$PART" ]; then
  DISK=$(lsblk -nso PKNAME "/dev/$PART" 2>/dev/null | tail -1)
  NUM=$(cat "/sys/class/block/$PART/partition" 2>/dev/null)
  if [ -n "$DISK" ] && [ -n "$NUM" ]; then
    if ! command -v growpart >/dev/null 2>&1; then
      if command -v apt-get >/dev/null 2>&1; then apt-get install -y cloud-guest-utils >/dev/null 2>&1 || true
      elif command -v dnf >/dev/null 2>&1; then dnf install -y cloud-utils-growpart >/dev/null 2>&1 || true
      elif command -v yum >/dev/null 2>&1; then yum install -y cloud-utils-growpart >/dev/null 2>&1 || true; fi
    fi
    if command -v growpart >/dev/null 2>&1; then
      growpart "/dev/$DISK" "$NUM" >/dev/null 2>&1 || true
    elif command -v parted >/dev/null 2>&1; then
      parted -s "/dev/$DISK" resizepart "$NUM" 100% >/dev/null 2>&1 || true
      partx -u "/dev/$DISK" >/dev/null 2>&1 || true
    fi
  fi
fi
if command -v pvs >/dev/null 2>&1; then
  for pv in $(pvs --noheadings -o pv_name 2>/dev/null); do pvresize "$pv" >/dev/null 2>&1 || true; done
  case "$ROOT_SRC" in /dev/mapper/*) lvextend -l +100%FREE "$ROOT_SRC" >/dev/null 2>&1 || true ;; esac
fi
case "$(findmnt -n -o FSTYPE /)" in
  xfs) xfs_growfs / >/dev/null 2>&1 || true ;;
  ext2|ext3|ext4) resize2fs "$ROOT_SRC" >/dev/null 2>&1 || true ;;
esac
AFTER=$(findmnt -n -o SIZE /)
[ "$BEFORE" = "$AFTER" ] || echo "[disk] 根分区已扩容: $BEFORE -> $AFTER"`

// ciFixStaticScript 首次开机把 DHCP 地址固化静态(哨兵文件仅执行一次;netplan/nmcli 分派)。
const ciFixStaticScript = `[ -f /etc/.dhcp_to_static_done ] && exit 0
IFACE=$(ip -o -4 addr show scope global 2>/dev/null | head -1 | awk '{print $2}' | sed 's/://')
[ -n "$IFACE" ] || { echo '[static] 未找到全局 IPv4 接口,跳过'; exit 0; }
ADDR=$(ip -o -4 addr show scope global 2>/dev/null | head -1 | awk '{print $4}')
GW=$(ip route show default 2>/dev/null | head -1 | awk '{for(i=1;i<=NF;i++) if($i=="via") print $(i+1)}')
[ -n "$GW" ] || GW=$(ip route show default 2>/dev/null | head -1 | awk '{print $3}')
DNS=$(awk '/^nameserver/{print $2}' /etc/resolv.conf 2>/dev/null | paste -sd, -)
[ -n "$DNS" ] || DNS="223.5.5.5,119.29.29.29"
OS_ID=$(. /etc/os-release 2>/dev/null; echo "${ID:-linux}" | tr '[:upper:]' '[:lower:]')
case "$OS_ID" in
  ubuntu|debian)
    [ -f /etc/netplan/50-cloud-init.yaml ] && mv -f /etc/netplan/50-cloud-init.yaml /etc/netplan/50-cloud-init.yaml.bak 2>/dev/null || true
    {
      echo "network:"
      echo "  version: 2"
      echo "  ethernets:"
      echo "    $IFACE:"
      echo "      dhcp4: false"
      echo "      addresses: [$ADDR]"
      if [ -n "$GW" ]; then
        echo "      routes:"
        echo "        - to: default"
        echo "          via: $GW"
      fi
      echo "      nameservers:"
      echo "        addresses: [$DNS]"
    } > /etc/netplan/99-static.yaml
    netplan apply 2>/dev/null || true
    ;;
  openeuler|anolis|centos|rhel|rocky|fedora)
    CONN=$(nmcli -t -f NAME,DEVICE con show 2>/dev/null | awk -F: -v d="$IFACE" '$2==d{print $1; exit}')
    [ -n "$CONN" ] || CONN=$(nmcli -t -f NAME con show --active 2>/dev/null | head -1)
    if [ -n "$CONN" ]; then
      nmcli con mod "$CONN" ipv4.method manual ipv4.addresses "$ADDR" ipv4.gateway "$GW" ipv4.dns "$DNS" 2>/dev/null || true
      nmcli con up "$CONN" 2>/dev/null || true
    else
      echo "[static] 未找到 $IFACE 的 NetworkManager 连接,跳过"
    fi
    ;;
  *) echo "[static] 未识别发行版($OS_ID),跳过 DHCP->静态转换"; exit 0 ;;
esac
touch /etc/.dhcp_to_static_done
echo "[static] 已将 $IFACE 固定为静态 $ADDR (gw=$GW dns=$DNS)"`
