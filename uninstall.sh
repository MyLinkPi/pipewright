#!/bin/sh
# Pipewright 反安装(Linux / macOS)。清除 install.sh / 一键安装的全部落点:
# systemd 服务 → 运行中进程 → 二进制与源码部署标记 → /etc/pipewright 配置。
# 数据默认保留;PURGE_DATA=1 连数据一并删除(不可恢复)。
#
#   sh uninstall.sh                 # 交互确认(默认保留数据)
#   FORCE_YES=1 sh uninstall.sh     # 非交互执行(curl 管道必带)
#   PURGE_DATA=1 sh uninstall.sh    # 连同 /var/lib/pipewright 数据一并删除(不可恢复!)
#
# 可配环境变量:
#   INSTALL_DIR   二进制所在目录;缺省 /usr/local/bin(与 install.sh 一致)。
#   CONF_DIR      配置目录;缺省 /etc/pipewright。
#   DATA_DIR      数据目录;缺省 /var/lib/pipewright。
#
# 注:手动裸跑(非 systemd)形态的数据落在当初的启动目录(pipewright.db*),
# 不在标准路径,本脚本不猜不删 —— 请自行确认后手动清理。
# Windows 不适用。
set -eu

BIN="pipewright"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
CONF_DIR="${CONF_DIR:-/etc/pipewright}"
DATA_DIR="${DATA_DIR:-/var/lib/pipewright}"
UNIT=/etc/systemd/system/pipewright.service

info() { printf '\033[1;34m==>\033[0m %s\n' "$1"; }
warn() { printf '\033[1;33m提示:\033[0m %s\n' "$1" >&2; }
err() {
	printf '\033[1;31m错误:\033[0m %s\n' "$1" >&2
	exit 1
}

SUDO=""
if [ "$(id -u)" -ne 0 ]; then
	command -v sudo >/dev/null 2>&1 || err "反安装需 root 权限,且未找到 sudo。"
	SUDO="sudo"
fi

# 落点探测:一个标准落点都没有时多半是路径不对,交互模式下直接拦下。
if [ ! -x "${INSTALL_DIR}/${BIN}" ] && [ ! -f "$UNIT" ] && [ ! -d "$CONF_DIR" ]; then
	warn "未发现标准落点(${INSTALL_DIR}/${BIN} / ${UNIT} / ${CONF_DIR})——可能未安装或装在别的 INSTALL_DIR。"
	[ "${FORCE_YES:-}" = "1" ] || err "确认要继续请加 FORCE_YES=1 重跑(可用 INSTALL_DIR=<目录> 指定)。"
fi

# 确认:交互式询问;管道/非交互必须显式 FORCE_YES=1。
if [ "${FORCE_YES:-}" != "1" ] && [ -t 0 ]; then
	if [ "${PURGE_DATA:-}" = "1" ]; then
		data_desc="一并删除(不可恢复!)"
	else
		data_desc="保留"
	fi
	printf '将卸载 Pipewright:二进制 %s、服务 %s、配置 %s;数据 %s %s。继续?[y/N] ' \
		"${INSTALL_DIR}/${BIN}" "$UNIT" "$CONF_DIR" "$DATA_DIR" "$data_desc" >&2
	read -r ans
	case "$ans" in
	y | Y | yes | YES) ;;
	*) err "已取消,未做任何改动。" ;;
	esac
elif [ "${FORCE_YES:-}" != "1" ]; then
	err "非交互执行需 FORCE_YES=1(如:curl -fsSL <url>/uninstall.sh | FORCE_YES=1 sh)。"
fi

# 1) systemd 服务:停止 + 禁用 + 删 unit + 重载(macOS 无 systemd,跳过)。
if command -v systemctl >/dev/null 2>&1 && [ -f "$UNIT" ]; then
	info "停止并移除 systemd 服务…"
	$SUDO systemctl stop pipewright 2>/dev/null || true
	$SUDO systemctl disable pipewright 2>/dev/null || true
	$SUDO rm -f "$UNIT"
	$SUDO systemctl daemon-reload
	$SUDO systemctl reset-failed pipewright 2>/dev/null || true
fi

# 2) 裸跑进程(不经 systemd 的):按进程名精确匹配,杀掉后等文件锁释放。
if command -v pkill >/dev/null 2>&1; then
	if pkill -x "$BIN" 2>/dev/null; then
		info "已终止运行中的 ${BIN} 进程。"
		sleep 1
	fi
fi

# 3) 二进制与源码部署标记。
if [ -f "${INSTALL_DIR}/${BIN}" ]; then
	info "删除二进制 ${INSTALL_DIR}/${BIN}…"
	$SUDO rm -f "${INSTALL_DIR}/${BIN}"
fi
if [ -f "${INSTALL_DIR}/.pipewright-source" ]; then
	$SUDO rm -f "${INSTALL_DIR}/.pipewright-source"
fi

# 4) 配置目录(master key / env 文件)。删掉后重装会重新生成,凭据保险库将无法解密旧数据。
if [ -d "$CONF_DIR" ]; then
	info "删除配置 ${CONF_DIR}(master.key 与 pipewright.env)…"
	$SUDO rm -rf "$CONF_DIR"
fi

# 5) 数据目录:默认保留并给出手动清理命令;PURGE_DATA=1 显式删除。
if [ "${PURGE_DATA:-}" = "1" ]; then
	if [ -d "$DATA_DIR" ]; then
		info "删除数据 ${DATA_DIR}(数据库 / 产物;不可恢复)…"
		$SUDO rm -rf "$DATA_DIR"
	fi
elif [ -d "$DATA_DIR" ]; then
	warn "数据目录已保留:${DATA_DIR};确认不再需要后手动执行 rm -rf ${DATA_DIR}"
fi

info "反安装完成 ✓  重装:curl -fsSL <install.sh 地址> | sh(见 README)。"
