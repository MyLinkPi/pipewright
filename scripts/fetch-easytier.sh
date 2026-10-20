#!/usr/bin/env bash
# fetch-easytier.sh 把 EasyTier 官方二进制预先取到 third_party/easytier/,供发版
# 包直接携带(pwclient 开箱即用,免首连下载,对 GitHub 不可达环境尤其重要)。
#
# 产物布局(与 goreleaser pwclient 归档的 files 映射对应):
#   third_party/easytier/windows-<amd64|arm64>/bin/easytier-{core,cli}.exe
#   third_party/easytier/licenses/VERSION                     —— 本次携带的官方 tag
#   third_party/easytier/licenses/                   —— LGPL-3.0/GPL-3.0 全文(入库)
#
# 用法: bash scripts/fetch-easytier.sh [版本tag]
# 升级源: env PIPEWRIGHT_RELEASE_MIRROR 覆盖下载 base(默认 GitHub 官方),与平台镜像机制一致。
set -euo pipefail

version="${1:-v2.6.4}"
base="${PIPEWRIGHT_RELEASE_MIRROR:-https://github.com}"
root="$(cd "$(dirname "$0")/.." && pwd)"
dest="$root/third_party/easytier"
repo="EasyTier/EasyTier"

arch_asset() {
	case "$1" in
	amd64) echo x86_64 ;;
	arm64) echo arm64 ;;
	*) echo "不支持的架构: $1" >&2; exit 2 ;;
	esac
}

fetch() { # fetch <url> <输出>
	if ! curl -fL --retry 3 --max-time 300 -o "$2" "$1"; then
		echo "下载失败: $1" >&2
		exit 1
	fi
}

for arch in amd64 arm64; do
	asset="easytier-windows-$(arch_asset "$arch")-$version.zip"
	bindir="$dest/windows-$arch/bin"
	rm -rf "$bindir"
	mkdir -p "$bindir"
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	fetch "$base/$repo/releases/download/$version/$asset" "$tmp/$asset"
	# 解包:unzip(GitHub Linux runner 自带)→ PowerShell Expand-Archive(Windows)。
	if command -v unzip >/dev/null 2>&1; then
		unzip -q "$tmp/$asset" -d "$tmp/unz"
	elif command -v powershell >/dev/null 2>&1; then
		powershell -NoProfile -Command "Expand-Archive -LiteralPath '$(cygpath -w "$tmp/$asset" 2>/dev/null || echo "$tmp/$asset")' -DestinationPath '$(cygpath -w "$tmp/unz" 2>/dev/null || echo "$tmp/unz")' -Force"
	else
		echo "既无 unzip 也无 powershell,无法解包" >&2
		exit 1
	fi
	# 官方包平铺:easytier-core/cli + 运行库(wintun.dll 虚拟网卡 / Packet.dll、
	# WinDivert64.sys 抓包 —— 缺任一 easytier-core 无法启动或建不了网卡;web 系列不取)。
	for f in easytier-core* easytier-cli* wintun.dll Packet.dll WinDivert64.sys; do
		src="$(find "$tmp/unz" -name "$f" ! -name '*.sig' | head -1)"
		if [ -z "$src" ]; then
			echo "包内未找到 $f: $asset" >&2
			exit 1
		fi
		cp "$src" "$bindir/"
	done
	echo "windows/$arch: $(ls "$bindir" | tr '\n' ' ')← $asset"
done

echo "$version" > "$dest/licenses/VERSION"
echo "EasyTier $version 就绪于 third_party/easytier/。"
