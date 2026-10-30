#!/usr/bin/env bash
# fetch-easytier.sh 把 EasyTier 官方二进制预先取到 third_party/easytier/,供发版
# 包直接携带(pwclient 开箱即用,免首连下载,对 GitHub 不可达环境尤其重要)。
#
# 产物布局(与 goreleaser pwclient 归档的 files 映射 `third_party/easytier/{{ .Os }}-{{ .Arch }}/bin/*` 对应):
#   third_party/easytier/windows-<amd64|arm64>/bin/easytier-{core,cli}.exe + wintun.dll/Packet.dll/WinDivert64.sys
#   third_party/easytier/linux-<amd64|arm64>/bin/easytier-{core,cli}
#   third_party/easytier/darwin-<amd64|arm64>/bin/easytier-{core,cli}
#   third_party/easytier/licenses/VERSION                     —— 本次携带的官方 tag
#   third_party/easytier/licenses/                            —— LGPL-3.0/GPL-3.0 全文(入库)
#
# 用法: bash scripts/fetch-easytier.sh [版本tag]
# 升级源: env PIPEWRIGHT_RELEASE_MIRROR 覆盖下载 base(默认 GitHub 官方),与平台镜像机制一致。
set -euo pipefail

version="${1:-v2.6.4}"
base="${PIPEWRIGHT_RELEASE_MIRROR:-https://github.com}"
root="$(cd "$(dirname "$0")/.." && pwd)"
dest="$root/third_party/easytier"
repo="EasyTier/EasyTier"

# arch_asset <os> <goarch>:拼 EasyTier 官方资产里的 <平台>-<架构>段。注:资产里 macOS 叫
# macos(GOOS 为 darwin);架构 windows 用 arm64、unix 用 aarch64。目录名仍用 GOOS(与 goreleaser {{ .Os }} 对齐)。
arch_asset() {
	local os="$1" a="$2"
	local uname assetos
	case "$a" in
	amd64) uname=x86_64 ;;
	arm64)
		if [ "$os" = "windows" ]; then uname=arm64; else uname=aarch64; fi
		;;
	*) echo "不支持的架构: $a" >&2; exit 2 ;;
	esac
	if [ "$os" = "darwin" ]; then assetos=macos; else assetos=$os; fi
	echo "$assetos-$uname"
}

fetch() { # fetch <url> <输出>
	if ! curl -fL --retry 3 --max-time 300 -o "$2" "$1"; then
		echo "下载失败: $1" >&2
		exit 1
	fi
}

unzip_to() { # unzip_to <zip> <dest>
	if command -v unzip >/dev/null 2>&1; then
		unzip -q "$1" -d "$2"
	elif command -v powershell >/dev/null 2>&1; then
		powershell -NoProfile -Command "Expand-Archive -LiteralPath '$(cygpath -w "$1" 2>/dev/null || echo "$1")' -DestinationPath '$(cygpath -w "$2" 2>/dev/null || echo "$2")' -Force"
	else
		echo "既无 unzip 也无 powershell,无法解包" >&2
		exit 1
	fi
}

# 遍历 平台×架构。windows 额外携带运行库(虚拟网卡/抓包),unix 仅 core/cli 并置可执行位。
for os in windows linux darwin; do
	for arch in amd64 arm64; do
		asset="easytier-$(arch_asset "$os" "$arch")-$version.zip"
		bindir="$dest/$os-$arch/bin"
		rm -rf "$bindir"
		mkdir -p "$bindir"
		tmp="$(mktemp -d)"
		# trap 只兜失败(迭代中途 exit);成功路径迭代末尾显式清理,避免 trap 被后续
		# 迭代覆盖而漏清前几轮的临时目录。
		trap 'rm -rf "$tmp"' EXIT
		fetch "$base/$repo/releases/download/$version/$asset" "$tmp/$asset"
		unzip_to "$tmp/$asset" "$tmp/unz"
		if [ "$os" = "windows" ]; then
			# 官方包平铺:easytier-core/cli + 运行库(wintun.dll 虚拟网卡 / Packet.dll、
			# WinDivert64.sys 抓包 —— 缺任一 easytier-core 无法启动或建不了网卡;web 系列不取)。
			patterns="easytier-core* easytier-cli* wintun.dll Packet.dll WinDivert64.sys"
		else
			# Linux/macOS:仅需 core/cli(ELF/Mach-O),无额外运行库。
			patterns="easytier-core easytier-cli"
		fi
		for f in $patterns; do
			src="$(find "$tmp/unz" -name "$f" ! -name '*.sig' | head -1)"
			if [ -z "$src" ]; then
				echo "包内未找到 $f: $asset" >&2
				exit 1
			fi
			cp "$src" "$bindir/"
		done
		# unix 二进制补可执行位(zip 不保留权限)。
		if [ "$os" != "windows" ]; then
			chmod 0755 "$bindir"/easytier-core "$bindir"/easytier-cli 2>/dev/null || true
		fi
		echo "$os/$arch: $(ls "$bindir" | tr '\n' ' ')← $asset"
		rm -rf "$tmp"
	done
done

echo "$version" > "$dest/licenses/VERSION"
echo "EasyTier $version 就绪于 third_party/easytier/(windows/linux/darwin × amd64/arm64)。"
