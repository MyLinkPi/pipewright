#!/usr/bin/env bash
# build-installer.sh —— 用 NSIS(makensis)把 pwclient 发布 zip 打成 Windows 安装包。
#
# 产物:pwclient_<version>_windows_<arch>_installer.exe
# 一个安装包只装对应架构的客户端负载(pwclient.exe + bin/easytier-*.exe);安装器本身
# 是标准 32 位 NSIS stub,能同时跑在 amd64 与 arm64 Windows 上,故 amd64/arm64 各出一个。
#
# 依赖:makensis(NSIS 3)。定位顺序:--makensis > $MKENSIS > PATH 上的 makensis。
#   Debian/Ubuntu:  sudo apt-get install -y nsis        # 提供原生 makensis
#   macOS:          brew install nsis
#   Windows:        装 NSIS 并把 Bin 加入 PATH
#
# 用法:
#   bash scripts/build-installer.sh --zip dist/pwclient_1.6.7_windows_amd64.zip --version 1.6.7 --arch amd64 --out dist/
#   bash scripts/build-installer.sh --src /path/to/staging --version 1.6.7 --arch amd64 --out dist/
# 说明:--src 目录须含 pwclient.exe / bin/ / licenses/ / LICENSE(即解包后的发布 zip 内容)。
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
nsi="$root/build/windows/pwclient.nsi"

zip=""
src=""
version=""
arch="amd64"
out="$root/dist-installer"
makensis="${MKENSIS:-}"

die() { echo "build-installer: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--zip) zip="${2:-}"; shift 2 ;;
	--src) src="${2:-}"; shift 2 ;;
	--version) version="${2:-}"; shift 2 ;;
	--arch) arch="${2:-}"; shift 2 ;;
	--out) out="${2:-}"; shift 2 ;;
	--makensis) makensis="${2:-}"; shift 2 ;;
	-h | --help)
		sed -n '1,20p' "$0"
		exit 0 ;;
	*) die "未知参数:$1(见 --help)" ;;
	esac
done

# ---- 校验入参 ----
[ -n "$version" ] || die "缺少 --version"
case "$arch" in
amd64 | arm64) ;;
*) die "不支持的架构:$arch(仅 amd64/arm64)" ;;
esac

# makensis 定位。
if [ -z "$makensis" ]; then
	makensis="$(command -v makensis || true)"
fi
[ -n "$makensis" ] || die "未找到 makensis:请安装 NSIS 3(Debian/Ubuntu: apt-get install -y nsis),或用 --makensis / MKENSIS 指定路径"

# 组装暂存目录(优先 --zip;否则用 --src 原样)。
staging=""
cleanup_staging=""
if [ -n "$zip" ]; then
	[ -f "$zip" ] || die "找不到 zip:$zip"
	staging="$(mktemp -d)"
	cleanup_staging="$staging"
	unzip -q -o "$zip" -d "$staging"
else
	[ -n "$src" ] || die "需 --zip 或 --src 之一"
	[ -d "$src" ] || die "暂存目录不存在:$src"
	staging="$src"
fi
trap '[ -n "$cleanup_staging" ] && rm -rf "$cleanup_staging" || true' EXIT

# 结构校验。
[ -f "$staging/pwclient.exe" ] || die "暂存目录缺 pwclient.exe:$staging"
[ -d "$staging/bin" ] || die "暂存目录缺 bin/(EasyTier 随包二进制):$staging"
license="$staging/LICENSE"
[ -f "$license" ] || license="$root/LICENSE" # 兜底用仓库 LICENSE
[ -f "$license" ] || die "找不到 LICENSE(暂存目录与仓库均无)"

# PE 版本资源用的 4 段数字版本:剥前导 v 与 -rc/-beta 后缀,补齐 x.y.z.0。
core="${version#v}"
numeric="$(printf '%s' "$core" | grep -oE '^[0-9]+\.[0-9]+\.[0-9]+' || true)"
vi=""
if [ -n "$numeric" ]; then vi="${numeric}.0"; fi

mkdir -p "$out"
# 归一为绝对路径:不同 makensis 版本对相对 OutFile 的解析基准不一致(Ubuntu 26.04
# 的 nsis 3.10 下 "./installer/..." 直接 Can't open output file,CI beta.3 即因此失败)。
out="$(cd "$out" && pwd)"
outfile="$out/pwclient_${version}_windows_${arch}_installer.exe"

echo "==> makensis: $makensis"
echo "    src=$staging version=$version arch=$arch out=$outfile"

# makensis 定义:OutFile 用绝对路径写到 out;可选 PE 版本资源经数组安全传入。
defines=(
	"-DPW_SRC=$staging"
	"-DPW_VERSION=$version"
	"-DPW_OUTFILE=$outfile"
	"-DPW_ICON=$root/cmd/pwclient/winres/icon.ico"
	"-DPW_LICENSE=$license"
)
if [ -n "$vi" ]; then defines+=("-DPW_VI_VER=$vi"); fi

"$makensis" "${defines[@]}" "$nsi"

[ -f "$outfile" ] || die "makensis 未产出安装包:$outfile"
echo "✅ 安装包已生成:$outfile"
