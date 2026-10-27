#!/bin/sh
# Pipewright 安装/升级脚本(Linux / macOS)。
#
#   方式一(一键,联网下载 Release 二进制):
#     curl -fsSL https://raw.githubusercontent.com/<owner>/pipewright/<ref>/install.sh | sh
#     本地无 make build 产物时自动从 GitHub Release 下载当前平台资产(sha256 校验)。
#   方式二(本地编译):make build && sh install.sh
#
# 流程:定位二进制(本地产物优先,缺失自动下载)→ 装到 /usr/local/bin → 可选装为
# systemd 服务 + Docker 检测。
# 升级:检测到已有二进制或已装 systemd 服务时自动进入升级模式 —— 仅替换二进制并重启
# 已有服务,零交互(不询问 Docker 安装与服务安装);SETUP_SERVICE=1 可在升级时重建服务配置。
# 下载安装即「二进制部署」:会清除源码部署标记(.pipewright-source 与 env 里的
# PIPEWRIGHT_SOURCE_DIR),Web 端自升级转为直接下载 Release(小内存机器也能一键升级)。
# 可配环境变量:
#   BIN_PATH            本地二进制路径;缺省取脚本所在目录的 ./pipewright(即 make build 的产物)。
#   PIPEWRIGHT_VERSION  下载模式钉版本(如 v1.2.3,可省 v);缺省取最新正式版。
#   PIPEWRIGHT_RELEASE_REPO   发布仓库 owner/name;缺省 MyLinkPi/pipewright(上游/其它 fork 发版时覆盖)。
#   PIPEWRIGHT_RELEASE_MIRROR 镜像,两种形态:GitHub 路径兼容反代(须按 GitHub 路径布局转发)
#                      或前缀式代理(如 https://gh-proxy.com/https://github.com,自动派生下载/解析地址);
#                      缺省 GitHub 官方。
#   PIPEWRIGHT_NO_DOWNLOAD=1  禁止自动下载:本地无产物时报错(旧行为)。
#   INSTALL_DIR     安装目录;缺省 /usr/local/bin(不可写时自动 sudo)。
#   INSTALL_DOCKER  =1 时,Linux 下若缺 Docker 自动经官方 get.docker.com 安装(隔离构建/容器部署需要)。
#   SETUP_SERVICE   =1 装为 systemd 服务(开机自启 + 崩溃重启 + 自更新可用;Linux,需 root);=0 强制跳过;
#                   缺省在交互式终端下询问。会持久化 master key 到 /etc/pipewright/master.key、
#                   数据落 /var/lib/pipewright,配置写 /etc/pipewright/pipewright.env。
#   PIPEWRIGHT_ADDR 监听地址(装服务时写入 env 文件);缺省 :8080。
#   PIPEWRIGHT_DB_DRIVER / PIPEWRIGHT_DB_DSN
#                   装服务时选 DB 后端:缺省 sqlite(数据落 /var/lib/pipewright);
#                   传 mysql + DSN(user:pw@tcp(host:3306)/db?parseTime=true&charset=utf8mb4)则用 MySQL。
#
# Windows 不支持此脚本(服务装的是 systemd):请从 GitHub Release 下载
# pipewright_<版本>_windows_<架构>.zip 解压后直接运行。
set -eu

BIN="pipewright"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
# 缺省取脚本所在目录的构建产物;脚本复制到别处跑时用 BIN_PATH 显式指。
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_PATH="${BIN_PATH:-${SCRIPT_DIR}/${BIN}}"
SERVICE_INSTALLED=0

info() { printf '\033[1;34m==>\033[0m %s\n' "$1"; }
warn() { printf '\033[1;33m提示:\033[0m %s\n' "$1" >&2; }
err() {
	printf '\033[1;31m错误:\033[0m %s\n' "$1" >&2
	exit 1
}

# ── 下载模式:从 Release 获取平台资产 ─────────────────────────────────
# download_binary:下载当前平台资产并做 sha256 校验,置 BIN_PATH 指向解包产物。
# 仓库/镜像语义与后端 internal/version/source.go 一致:PIPEWRIGHT_RELEASE_MIRROR 须按
# GitHub 路径布局转发;PIPEWRIGHT_RELEASE_REPO 缺省上游仓库(fork 发版时指向自己仓库)。
# 完整性由 Release 附带的 checksums.txt 把关(与 Web 端自升级同一防线)。
download_binary() {
	RELEASE_REPO="${PIPEWRIGHT_RELEASE_REPO:-MyLinkPi/pipewright}"
	MIRROR="${PIPEWRIGHT_RELEASE_MIRROR:-}"
	DL_BASE="https://github.com"      # 官方下载 base(github.com 形态)
	API_BASE="https://api.github.com" # 官方 API base(api.github.com 形态)
	# 镜像形态识别:前缀式(base 内嵌完整上游 origin,如 https://gh-proxy.com/https://github.com,
	# gh-proxy 系公共代理的通用用法)拆出代理根 —— 下载走 github.com 形态、版本解析走
	# api.github.com 形态;其余按路径兼容反代处理(同一 base 直拼两类路径,须按 GitHub
	# 路径布局转发)。与后端 internal/version/source.go 的 buildSource 同语义。
	if [ -n "$MIRROR" ]; then
		MIRROR="${MIRROR%/}"
		rest="${MIRROR#*://}"
		prefix=0
		for s in https:// http://; do
			case "$rest" in
			*"$s"*)
				head="${MIRROR%"$rest"}"
				root="${head}${rest%%"$s"*}"
				while :; do case "$root" in */) root="${root%/}" ;; *) break ;; esac done
				# 前缀式:检查/下载一律经代理根重组,不绕过代理(双斜杠随之归一)。
				API_BASE="${root}/https://api.github.com"
				DL_BASE="${root}/https://github.com"
				prefix=1
				break
				;;
			esac
		done
		if [ "$prefix" = "0" ]; then
			DL_BASE="$MIRROR"
			API_BASE="$MIRROR"
		fi
	fi
	command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 ||
		err "自动下载需要 curl 或 wget(或在内存充足的机器 make build 后用 BIN_PATH=<路径> 安装)。"

	# 解析目标 tag:PIPEWRIGHT_VERSION 钉版本;缺省先试 /releases/latest 的 302 重定向
	# (免 API、无限流),失败再走 API JSON 兜底(前缀式代理的页面路径通常被拒,API 是可靠路径)。
	TAG="${PIPEWRIGHT_VERSION:-}"
	if [ -z "$TAG" ]; then
		if command -v curl >/dev/null 2>&1; then
			TAG="$(curl -fsSIL -o /dev/null -w '%{url_effective}' "${DL_BASE}/${RELEASE_REPO}/releases/latest" 2>/dev/null |
				sed 's|.*/tag/||' | tr -d '\r')"
		else
			TAG="$(wget --spider -S --max-redirect=0 "${DL_BASE}/${RELEASE_REPO}/releases/latest" 2>&1 |
				sed -n 's/^ *Location: //p' | tail -1 | tr -d '\r' | sed 's|.*/tag/||')"
		fi
		if [ "${TAG#v}" = "$TAG" ]; then
			# 重定向解析失败(空/非 v 开头):改从 releases/latest JSON 读 tag_name。
			if command -v curl >/dev/null 2>&1; then
				api_json="$(curl -fsSL "${API_BASE}/repos/${RELEASE_REPO}/releases/latest" 2>/dev/null || true)"
			else
				api_json="$(wget -qO- "${API_BASE}/repos/${RELEASE_REPO}/releases/latest" 2>/dev/null || true)"
			fi
			TAG="$(printf '%s' "$api_json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
		fi
		case "$TAG" in
		v*) ;;
		"") err "无法解析最新版本:检查网络与 ${DL_BASE} 可达性,或用 PIPEWRIGHT_VERSION=v1.2.3 钉版本。" ;;
		*) err "意外的最新版本引用:${TAG}" ;;
		esac
	else
		# 钉版本时容许省 v 前缀,统一成 tag 形态。
		case "$TAG" in v*) ;; *) TAG="v${TAG}" ;; esac
	fi
	VER="${TAG#v}"
	ASSET="pipewright_${VER}_${OS}_${ARCH}.tar.gz"

	info "下载 ${RELEASE_REPO} ${TAG}(${ASSET})"
	DL_DIR="$(mktemp -d)"
	trap 'rm -rf "$DL_DIR"' EXIT
	base_url="${DL_BASE}/${RELEASE_REPO}/releases/download/${TAG}"
	# 下载走进度条(curl --progress-bar / wget --show-progress,任何详细度下都显示),
	# 版本解析等元数据请求仍静默 —— 只有大文件才值得展示进度。
	if command -v curl >/dev/null 2>&1; then
		curl -fSL --progress-bar -o "${DL_DIR}/${ASSET}" "${base_url}/${ASSET}" || err "下载失败:${base_url}/${ASSET}"
		curl -fsSL -o "${DL_DIR}/checksums.txt" "${base_url}/checksums.txt" || err "下载失败:${base_url}/checksums.txt"
	else
		wget -q --show-progress -O "${DL_DIR}/${ASSET}" "${base_url}/${ASSET}" || err "下载失败:${base_url}/${ASSET}"
		wget -qO "${DL_DIR}/checksums.txt" "${base_url}/checksums.txt" || err "下载失败:${base_url}/checksums.txt"
	fi

	# sha256 校验(checksums.txt 行格式:<sha256>  <文件名>)。
	want="$(awk -v f="$ASSET" '$NF==f {print $1; exit}' "${DL_DIR}/checksums.txt")"
	[ -n "$want" ] || err "checksums.txt 中没有 ${ASSET} 的条目。"
	if command -v sha256sum >/dev/null 2>&1; then
		got="$(sha256sum "${DL_DIR}/${ASSET}" | cut -d' ' -f1)"
	elif command -v shasum >/dev/null 2>&1; then
		got="$(shasum -a 256 "${DL_DIR}/${ASSET}" | cut -d' ' -f1)"
	else
		err "需要 sha256sum 或 shasum 校验下载完整性。"
	fi
	[ "$got" = "$want" ] || err "sha256 校验失败:期望 ${want},实际 ${got}。"

	tar -xzf "${DL_DIR}/${ASSET}" -C "$DL_DIR" || err "解压 ${ASSET} 失败。"
	BIN_PATH="${DL_DIR}/pipewright"
	[ -f "$BIN_PATH" ] || err "资产里没有 ${BIN}。"
	chmod +x "$BIN_PATH"
}

# rm_env_line:从 env 文件删除匹配行(经临时文件重写并保留 600 权限;需要时经 sudo)。
rm_env_line() { # $1 = sed 表达式,如 /^PIPEWRIGHT_SOURCE_DIR=/d
	if [ "$(id -u)" -eq 0 ]; then
		sed "$1" "$ENV_FILE_PATH" >"${ENV_FILE_PATH}.pwtmp" &&
			mv "${ENV_FILE_PATH}.pwtmp" "$ENV_FILE_PATH" && chmod 600 "$ENV_FILE_PATH"
	elif command -v sudo >/dev/null 2>&1; then
		sudo sh -c "sed '$1' '$ENV_FILE_PATH' > '$ENV_FILE_PATH.pwtmp' && mv '$ENV_FILE_PATH.pwtmp' '$ENV_FILE_PATH' && chmod 600 '$ENV_FILE_PATH'"
	fi
}

# 平台探测(决定下载资产名,及是否提供 systemd 服务与 Docker 自动安装)。
OS="$(uname -s)"
case "$OS" in
Linux) OS="linux" ;;
Darwin) OS="darwin" ;;
*) err "不支持的系统:$OS(Windows 请从 GitHub Release 下载 zip 解压运行)。" ;;
esac
ARCH="$(uname -m)"
case "$ARCH" in
x86_64) ARCH="amd64" ;;
aarch64 | arm64) ARCH="arm64" ;;
*) err "不支持的架构:$ARCH(发布资产仅覆盖 linux/darwin × amd64/arm64)。" ;;
esac

# 获取二进制:本地产物优先;缺失时自动下载 Release(PIPEWRIGHT_NO_DOWNLOAD=1 恢复旧行为)。
# curl|sh 管道安装即走此路径 —— 当前目录通常没有构建产物。
DOWNLOAD=0
if [ ! -f "$BIN_PATH" ]; then
	if [ "${PIPEWRIGHT_NO_DOWNLOAD:-}" = "1" ]; then
		err "找不到 ${BIN_PATH}。请先在仓库根目录执行 make build,或用 BIN_PATH=<路径> 指定。"
	fi
	DOWNLOAD=1
	download_binary
else
	[ -x "$BIN_PATH" ] || err "${BIN_PATH} 不可执行。请确认是 make build 产出的二进制。"
fi

if [ "$DOWNLOAD" = "1" ]; then
	info "安装下载的 ${BIN} ${TAG}(${BIN_PATH})"
else
	info "安装本地编译的 ${BIN}(${BIN_PATH})"
fi

# 安装:目录不可写则用 sudo。
info "安装到 ${INSTALL_DIR}…"
if [ -w "$INSTALL_DIR" ]; then
	cp "$BIN_PATH" "${INSTALL_DIR}/${BIN}.tmp" && mv "${INSTALL_DIR}/${BIN}.tmp" "${INSTALL_DIR}/${BIN}"
elif command -v sudo >/dev/null 2>&1; then
	sudo cp "$BIN_PATH" "${INSTALL_DIR}/${BIN}.tmp" && sudo mv "${INSTALL_DIR}/${BIN}.tmp" "${INSTALL_DIR}/${BIN}"
else
	err "${INSTALL_DIR} 不可写且无 sudo。可设 INSTALL_DIR=\$HOME/.local/bin 重试。"
fi
chmod +x "${INSTALL_DIR}/${BIN}" 2>/dev/null || sudo chmod +x "${INSTALL_DIR}/${BIN}"

# 部署形态标记:本地编译安装记录源码仓库根,平台自升级据此走 git pull + make build
# (每次安装/升级都刷新,仓库可能被移动)。下载安装是二进制部署 —— 不写标记,且清掉
# 旧标记与 env 残留,Web 自升级转为直接下载 Release。
if [ "$DOWNLOAD" = "1" ]; then
	if [ -f "${INSTALL_DIR}/.pipewright-source" ]; then
		rm -f "${INSTALL_DIR}/.pipewright-source" 2>/dev/null ||
			sudo rm -f "${INSTALL_DIR}/.pipewright-source" 2>/dev/null || true
		info "已清除源码部署标记:本次起为二进制部署,Web 端自升级将直接下载 Release。"
	fi
else
	if [ -w "$INSTALL_DIR" ]; then
		printf '%s\n' "$SCRIPT_DIR" >"${INSTALL_DIR}/.pipewright-source" 2>/dev/null || true
	elif command -v sudo >/dev/null 2>&1; then
		printf '%s\n' "$SCRIPT_DIR" | sudo tee "${INSTALL_DIR}/.pipewright-source" >/dev/null 2>&1 || true
	fi
fi

# 已有 env 文件时同步部署形态:删 PIPEWRIGHT_SOURCE_DIR 残留;记录发布仓库,使 Web 自升级
# 与本次安装同渠道。SETUP_SERVICE 首装路径在 setup_service 里处理(env 文件彼时才创建)。
ENV_FILE_PATH=/etc/pipewright/pipewright.env
if [ "$DOWNLOAD" = "1" ] && [ -f "$ENV_FILE_PATH" ]; then
	if grep -q '^PIPEWRIGHT_SOURCE_DIR=' "$ENV_FILE_PATH" 2>/dev/null; then
		rm_env_line '/^PIPEWRIGHT_SOURCE_DIR=/d'
		info "已从 ${ENV_FILE_PATH} 移除 PIPEWRIGHT_SOURCE_DIR(源码部署残留)。"
	fi
	if ! grep -q '^PIPEWRIGHT_RELEASE_REPO=' "$ENV_FILE_PATH" 2>/dev/null; then
		if [ "$(id -u)" -eq 0 ]; then
			echo "PIPEWRIGHT_RELEASE_REPO=${RELEASE_REPO}" >>"$ENV_FILE_PATH"
		elif command -v sudo >/dev/null 2>&1; then
			echo "PIPEWRIGHT_RELEASE_REPO=${RELEASE_REPO}" | sudo tee -a "$ENV_FILE_PATH" >/dev/null
		fi
	fi
fi

# SELinux(RHEL/CentOS/Rocky/Fedora 系,强制模式):二进制带错误类型时 systemd(init_t)
# 无权执行 → 装为服务后起不来(AVC denied execute)。重置为该路径的默认上下文
# (/usr/local/bin → bin_t)。非 SELinux 系统无 restorecon,跳过。
if command -v restorecon >/dev/null 2>&1; then
	restorecon "${INSTALL_DIR}/${BIN}" 2>/dev/null ||
		sudo restorecon "${INSTALL_DIR}/${BIN}" 2>/dev/null || true
fi

info "完成 ✓  $("${INSTALL_DIR}/${BIN}" --version 2>/dev/null || echo "${BIN}(版本元数据见 go build 注入)")"

# ── 可选:装为 systemd 服务 ──────────────────────────────────────────
# 「部署平台」需要开机自启 + 崩溃重启,且自更新(二进制自替换 + syscall.Exec 自重启)要求
# 进程对 ${INSTALL_DIR} 有写权限 —— 故服务以 root 运行。本函数幂等:重装复用已有 master key /
# env 文件,不覆盖用户改动,升级不丢保险库。
setup_service() {
	SUDO=""
	if [ "$(id -u)" -ne 0 ]; then
		command -v sudo >/dev/null 2>&1 || err "安装 systemd 服务需 root 权限,且未找到 sudo。"
		SUDO="sudo"
	fi

	CONF_DIR=/etc/pipewright
	DATA_DIR=/var/lib/pipewright
	KEY_FILE="${CONF_DIR}/master.key"
	ENV_FILE="${CONF_DIR}/pipewright.env"
	UNIT=/etc/systemd/system/pipewright.service
	ADDR="${PIPEWRIGHT_ADDR:-:8080}"

	info "配置 systemd 服务…"
	$SUDO mkdir -p "$CONF_DIR" "$DATA_DIR"

	# master key:已存在则复用(升级不丢保险库),否则生成 32 字节(base64)。
	if [ -s "$KEY_FILE" ]; then
		info "复用已有 master key:${KEY_FILE}"
	else
		info "生成凭据保险库 master key:${KEY_FILE}"
		head -c 32 /dev/urandom | base64 | tr -d '\n' | $SUDO tee "$KEY_FILE" >/dev/null
		$SUDO chmod 600 "$KEY_FILE"
	fi

	# DB 后端:默认 SQLite(数据落 ${DATA_DIR});传入 PIPEWRIGHT_DB_DRIVER=mysql + PIPEWRIGHT_DB_DSN
	# 则用 MySQL(DSN 为 go-sql-driver 格式,如 user:pw@tcp(host:3306)/db?parseTime=true&charset=utf8mb4)。
	DB_DRIVER="${PIPEWRIGHT_DB_DRIVER:-sqlite}"
	if [ "$DB_DRIVER" = "mysql" ] && [ -z "${PIPEWRIGHT_DB_DSN:-}" ]; then
		err "PIPEWRIGHT_DB_DRIVER=mysql 需同时提供 PIPEWRIGHT_DB_DSN(go-sql-driver DSN,parseTime=true 必带)。"
	fi

	# env 文件:不存在才写(保留用户后续改动);改后 systemctl restart pipewright 生效。
	if [ ! -f "$ENV_FILE" ]; then
		{
			echo '# Pipewright 运行配置(systemd EnvironmentFile);改后 systemctl restart pipewright 生效。'
			echo "PIPEWRIGHT_ADDR=${ADDR}"
			echo "PIPEWRIGHT_MASTER_KEY_FILE=${KEY_FILE}"
			if [ "$DB_DRIVER" = "mysql" ]; then
				echo 'PIPEWRIGHT_DB_DRIVER=mysql'
				echo "PIPEWRIGHT_DB_DSN=${PIPEWRIGHT_DB_DSN}"
			else
				echo "PIPEWRIGHT_DB=${DATA_DIR}/pipewright.db"
			fi
		} | $SUDO tee "$ENV_FILE" >/dev/null
		$SUDO chmod 600 "$ENV_FILE"
	fi

	# 部署形态记入 env:源码部署记仓库根路径(平台据此识别源码部署并定位仓库做自升级,
	# 已存在则不重复写;仓库移动后改此项即可);下载安装改记发布仓库,使 Web 自升级与
	# 安装渠道一致(源码残留的清理已在主流程做过)。
	if [ "$DOWNLOAD" = "0" ]; then
		if ! grep -q '^PIPEWRIGHT_SOURCE_DIR=' "$ENV_FILE" 2>/dev/null; then
			echo "PIPEWRIGHT_SOURCE_DIR=${SCRIPT_DIR}" | $SUDO tee -a "$ENV_FILE" >/dev/null
		fi
	elif ! grep -q '^PIPEWRIGHT_RELEASE_REPO=' "$ENV_FILE" 2>/dev/null; then
		echo "PIPEWRIGHT_RELEASE_REPO=${RELEASE_REPO}" | $SUDO tee -a "$ENV_FILE" >/dev/null
	fi

	# systemd unit。User=root:自更新须写 ${INSTALL_DIR}、隔离构建须用 docker、SSH 部署须读密钥。
	# 自更新经 syscall.Exec 自替换映像(同 PID),systemd 无感,与 Restart=always 不冲突。
	printf '%s\n' \
		'[Unit]' \
		'Description=Pipewright CI/CD 平台' \
		'After=network-online.target docker.service' \
		'Wants=network-online.target' \
		'' \
		'[Service]' \
		'Type=simple' \
		'User=root' \
		"WorkingDirectory=${DATA_DIR}" \
		"EnvironmentFile=${ENV_FILE}" \
		"ExecStart=${INSTALL_DIR}/${BIN}" \
		'Restart=always' \
		'RestartSec=3' \
		'' \
		'[Install]' \
		'WantedBy=multi-user.target' |
		$SUDO tee "$UNIT" >/dev/null

	$SUDO systemctl daemon-reload
	$SUDO systemctl enable pipewright >/dev/null 2>&1 || true
	$SUDO systemctl restart pipewright

	SERVICE_INSTALLED=1
	info "服务已启动并设为开机自启。"
	printf '  状态:%s\n' "systemctl status pipewright"
	printf '  日志:%s\n' "journalctl -u pipewright -f"
	printf '  初始口令:%s\n' "未设 PIPEWRIGHT_ADMIN_PASSWORD 时自动生成随机口令,root 见 ${DATA_DIR}/admin_password.txt(登录后改掉并删除)"
	printf '  访问:http://<本机IP>%s\n' "$ADDR"
}

# 决定是否装服务:SETUP_SERVICE=1 装 / =0 跳过 / 交互式询问 / 非交互且未设则给提示。
maybe_setup_service() {
	[ "$OS" = "linux" ] || return 0
	if ! command -v systemctl >/dev/null 2>&1; then
		[ "${SETUP_SERVICE:-}" = "1" ] && warn "未检测到 systemd(systemctl),跳过服务安装。"
		return 0
	fi

	do_svc=0
	case "${SETUP_SERVICE:-}" in
	1) do_svc=1 ;;
	0) do_svc=0 ;;
	*)
		if [ -t 0 ]; then
			printf '  装为 systemd 服务?开机自启 + 崩溃重启 + 自更新可用(以 root 运行)。[Y/n] ' >&2
			read -r ans
			case "$ans" in n | N | no | NO) do_svc=0 ;; *) do_svc=1 ;; esac
		else
			warn "如需开机自启 + 自更新,重跑时加 SETUP_SERVICE=1(装为 systemd 服务)。"
		fi
		;;
	esac

	[ "$do_svc" = "1" ] && setup_service
}

# ── Docker 运行时检测 ────────────────────────────────────────────────
# 「隔离构建 / 容器部署」需要 Docker;控制台 / SSH 部署 / AI 诊断不需要。
# 缺失不致命(平台照常起),但隔离构建会降级到桩 runner —— 故给出明确提示 / 可选自动安装。
check_docker() {
	if command -v docker >/dev/null 2>&1; then
		if docker info >/dev/null 2>&1; then
			info "Docker 已就绪 ✓  隔离构建 / 容器部署可用"
		else
			warn "Docker 已安装但守护进程未运行。启动后隔离构建可用(Linux:sudo systemctl start docker)。"
		fi
		return
	fi

	warn "未检测到 Docker。平台可运行,但「隔离构建 / 容器部署」需要它(否则降级到桩 runner,不做真实构建)。"

	# macOS:Docker Desktop 是 GUI 应用,无法命令行装,只提示。
	if [ "$OS" != "linux" ]; then
		printf '  macOS 请安装 Docker Desktop:https://www.docker.com/products/docker-desktop/\n' >&2
		return
	fi

	# Linux:opt-in 自动装(INSTALL_DOCKER=1 或交互式确认)。官方 get.docker.com 仅支持 Linux。
	do_install=0
	if [ "${INSTALL_DOCKER:-}" = "1" ]; then
		do_install=1
	elif [ -t 0 ]; then
		printf '  是否现在用官方脚本 get.docker.com 自动安装 Docker?[y/N] ' >&2
		read -r ans
		case "$ans" in y | Y | yes | YES) do_install=1 ;; esac
	else
		printf '  Linux 自动安装:重跑时加 INSTALL_DOCKER=1,或手动 curl -fsSL https://get.docker.com | sh\n' >&2
	fi

	if [ "$do_install" = "1" ]; then
		command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 ||
			err "自动安装 Docker 需要 curl 或 wget,或手动执行 curl -fsSL https://get.docker.com | sh。"
		info "经官方脚本安装 Docker(get.docker.com,需要 sudo 提权)…"
		if curl -fsSL https://get.docker.com | sh 2>/dev/null || wget -qO- https://get.docker.com | sh; then
			info "Docker 安装完成。建议:sudo systemctl enable --now docker;将当前用户加入 docker 组(sudo usermod -aG docker \"\$USER\")后重新登录免 sudo。"
		else
			warn "Docker 自动安装失败,请参考 https://docs.docker.com/engine/install/ 手动安装。"
		fi
	fi
}
# ── 升级检测 ─────────────────────────────────────────────────────────
# 已有二进制或已装 systemd 服务 → 升级模式:仅替换二进制并重启已有服务,零交互
# (Docker 检测/自动安装与「装为服务」询问都是首装关切,升级时不再打扰)。
UPGRADE=0
HAS_UNIT=0
[ -x "${INSTALL_DIR}/${BIN}" ] && UPGRADE=1
if [ -f /etc/systemd/system/pipewright.service ]; then
	UPGRADE=1
	HAS_UNIT=1
fi

if [ "$UPGRADE" = "1" ]; then
	info "检测到已有安装 → 升级模式:仅替换二进制并重启服务(跳过 Docker 检测与安装询问)。"
	if [ "${SETUP_SERVICE:-}" = "1" ]; then
		# 显式要求:幂等重建服务配置(master key / env 文件复用)并重启。
		maybe_setup_service
		info "完成 ✓  升级完成,服务已重启,新版本已生效。"
	elif [ "$HAS_UNIT" = "1" ] && command -v systemctl >/dev/null 2>&1; then
		# 仅在服务正运行时重启(平台自升级会经本脚本重启自身);服务停着就不擅自拉起。
		if systemctl is-active --quiet pipewright 2>/dev/null; then
			if [ "$(id -u)" -eq 0 ]; then
				systemctl restart pipewright
			elif command -v sudo >/dev/null 2>&1; then
				sudo systemctl restart pipewright
			else
				warn "无 sudo,无法自动重启服务,请手动执行:systemctl restart pipewright"
			fi
			info "完成 ✓  升级完成,服务已重启,新版本已生效。"
		else
			info "完成 ✓  升级完成。服务当前未运行,未自动启动(需要时:systemctl start pipewright)。"
		fi
	else
		info "完成 ✓  升级完成。未能自动重启:若进程在跑,请手动重启以加载新版本。"
	fi
else
	check_docker
	maybe_setup_service

	if [ "$SERVICE_INSTALLED" = "1" ]; then
		info "完成 ✓  Pipewright 已作为 systemd 服务运行(开机自启 + 自更新可用)。"
	else
		printf '启动:%s\n' "${INSTALL_DIR}/${BIN}   # 默认监听 :8080,数据落当前目录 pipewright.db"
		printf '提示:%s\n' "如需开机自启 + 自更新,可重跑并加 SETUP_SERVICE=1 装为 systemd 服务。"
	fi
fi
