package version

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

const (
	// fallbackRepo 是默认发布仓库(owner/name)的硬编码兜底。DefaultRepo 为构建期可注入
	// 变量,未注入(本地 make build)或注入空串时回落到它。
	fallbackRepo = "huangchengsir/pipewright"

	githubAPIBase = "https://api.github.com"
	githubWebBase = "https://github.com"
)

// DefaultRepo 是检查更新所查询的默认仓库(owner/name)。goreleaser 发版时经 -X 注入为
// 构建仓库 —— tag 推到哪个仓库,产物就默认跟随哪个仓库查更新(fork 发版自洽);本地
// make build 未注入则保持 fallbackRepo。运行期可经 PIPEWRIGHT_RELEASE_REPO 覆盖。
var DefaultRepo = fallbackRepo

// Source 是一次「检查更新 / 下载升级包」所用的升级源。各 base 均为 GitHub 路径兼容的
// 前缀(可含子路径,不带尾斜杠):
//   - 检查: {APIBase}/repos/{Repo}/releases/latest
//   - 兜底: {WebBase}/{Repo}/releases/latest(302 重定向解析 tag)与 {WebBase}/{Repo}/releases.atom
//   - 下载: {DLBase}/{Repo}/releases/download/{tag}/{资产} 与 checksums.txt
//
// 留空字段回落 GitHub 官方源。镜像支持两种形态(见 buildSource):
//   - 路径兼容反代:自建 nginx 按同样路径布局转发 api.github.com 与 github.com;
//   - 前缀式代理:形如 https://gh-proxy.com/https://github.com —— base 内嵌完整上游
//     origin(gh-proxy / ghproxy 系公共代理的通用用法),自动派生 API 与下载两种上游。
//
// 二进制完整性仍由 sha256 校验和保障(镜像属管理员显式配置的信任源,与信任 GitHub
// 本身同级)。
type Source struct {
	Repo    string
	APIBase string
	WebBase string
	DLBase  string
}

// SourceProvider 返回当前生效的升级源;每次检查/下载前调用,使升级源修改即时生效、无需重启。
// 实现应自带优雅降级(读配置失败回落 env / 官方源),不得 panic。
type SourceProvider func(ctx context.Context) Source

// MirrorOrigin 标识生效升级镜像的来源,优先级 config > env > default。
type MirrorOrigin string

const (
	MirrorOriginConfig  MirrorOrigin = "config"  // 设置界面库内配置(release_mirror)
	MirrorOriginEnv     MirrorOrigin = "env"     // 部署环境变量 PIPEWRIGHT_RELEASE_MIRROR
	MirrorOriginDefault MirrorOrigin = "default" // 未配置任何镜像 → GitHub Releases 官方
)

// resolveMirrorBase 解析生效的镜像 base(空 = 未配置,用 GitHub 官方)与来源。
func resolveMirrorBase(configured string) (string, MirrorOrigin) {
	if m := strings.TrimSpace(configured); m != "" {
		return strings.TrimRight(m, "/"), MirrorOriginConfig
	}
	if m := envMirror(); m != "" {
		return strings.TrimRight(m, "/"), MirrorOriginEnv
	}
	return "", MirrorOriginDefault
}

// releaseRepo 取升级检查所查仓库,优先级:env PIPEWRIGHT_RELEASE_REPO(fork 覆盖)>
// DefaultRepo(构建期注入的构建仓库)> fallbackRepo(兜底,防空串)。
func releaseRepo() string {
	if repo := strings.TrimSpace(os.Getenv("PIPEWRIGHT_RELEASE_REPO")); repo != "" {
		return repo
	}
	if repo := strings.TrimSpace(DefaultRepo); repo != "" {
		return repo
	}
	return fallbackRepo
}

// buildSource 以镜像 base 组装升级源。base 为空 = GitHub 官方(检查走 api.github.com,
// 网页兜底/下载走 github.com);内嵌上游 origin 的前缀式代理按 splitPrefixProxy 拆分并
// 分别派生 API(api.github.com)与网页/下载(github.com)上游;其余 = 路径兼容反代,
// 三个 base 都指向镜像(须按 GitHub 路径布局转发)。
func buildSource(repo, base string) Source {
	if base == "" {
		return Source{Repo: repo, APIBase: githubAPIBase, WebBase: githubWebBase, DLBase: githubWebBase}
	}
	// 前缀式代理:不管内嵌的是 github.com 还是 api.github.com 形态,一律经代理根重组 ——
	// 检查走 {根}/https://api.github.com、网页/下载走 {根}/https://github.com,两类请求
	// 都不绕过代理(拆分重组同时归一了手滑的双斜杠)。
	if root, ok := splitPrefixProxy(base); ok {
		return Source{Repo: repo, APIBase: root + "/" + githubAPIBase, WebBase: root + "/" + githubWebBase, DLBase: root + "/" + githubWebBase}
	}
	return Source{Repo: repo, APIBase: base, WebBase: base, DLBase: base}
}

// splitPrefixProxy 识别前缀式代理 base(形如 https://gh-proxy.com/https://github.com:
// 代理根后跟完整上游 origin),返回归一化的代理根(去全部尾斜杠,容忍 //https:// 这类
// 手滑)。非该形态(路径兼容反代)返回 ok=false。上游 origin 只用于识别形态,两类
// base 恒经代理根重组为 GitHub 官方域,不做它用。
func splitPrefixProxy(base string) (root string, ok bool) {
	i := strings.Index(base, "://")
	if i < 0 {
		return "", false
	}
	rest := base[i+3:]
	best := -1
	for _, scheme := range []string{"https://", "http://"} {
		if j := strings.Index(rest, scheme); j > 0 && (best < 0 || j < best) {
			best = j
		}
	}
	if best < 0 {
		return "", false
	}
	return strings.TrimRight(base[:i+3+best], "/"), true
}

// ResolveSource 把镜像 base 组装成升级源,优先级:传入的库内配置 > env
// PIPEWRIGHT_RELEASE_MIRROR > GitHub 官方源。repo 与镜像无关,恒取
// PIPEWRIGHT_RELEASE_REPO(fork 覆盖)后回落 defaultRepo。
func ResolveSource(mirror string) Source {
	base, _ := resolveMirrorBase(mirror)
	return buildSource(releaseRepo(), base)
}

// ResolveMirror 返回实际生效的升级源与其镜像来源(供设置界面展示「当前生效」,
// 让默认值透明可见:未配置镜像时即 GitHub Releases 官方 —— 检查 api.github.com、
// 下载 github.com)。
func ResolveMirror(configured string) (Source, MirrorOrigin) {
	_, origin := resolveMirrorBase(configured)
	return ResolveSource(configured), origin
}

// envMirror 读部署级兜底镜像 PIPEWRIGHT_RELEASE_MIRROR(优先级低于库内配置:设置界面
// 显式配置了镜像则优先生效;env 供首次启动/无库场景)。空 = 未配置。
func envMirror() string {
	return strings.TrimSpace(os.Getenv("PIPEWRIGHT_RELEASE_MIRROR"))
}

// sourceMarkerName 是 install.sh 落在安装目录里、记录源码仓库路径的标记文件名。
const sourceMarkerName = ".pipewright-source"

// SourceDir 返回源码部署的仓库根目录(空 = 非源码部署)。识别顺序:
//  1. env PIPEWRIGHT_SOURCE_DIR(systemd 部署由 install.sh 写入 env 文件);
//  2. 安装目录里的标记文件 .pipewright-source(install.sh 每次安装/升级都写,记录仓库路径);
//  3. 当前可执行文件所在目录本身是 git 工作树(含 go.mod 与 .git)—— make run / 直接跑构建产物。
//
// 结果须通过仓库校验(含 .git),否则视为非源码部署返回空串。每次调用即取(env/文件可被
// 测试或运维修改),不做缓存。
func SourceDir() string {
	if d := strings.TrimSpace(os.Getenv("PIPEWRIGHT_SOURCE_DIR")); d != "" {
		if isRepoDir(d) {
			return d
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if b, err := os.ReadFile(filepath.Join(dir, sourceMarkerName)); err == nil {
			if d := strings.TrimSpace(string(b)); d != "" && isRepoDir(d) {
				return d
			}
		}
		if isRepoDir(dir) {
			return dir
		}
	}
	return ""
}

// isRepoDir 报告 dir 是否像本项目源码仓库根:go.mod 与 .git 须同时存在 —— 只有 go.mod
// 可能误判成别的 Go 项目目录;只有 .git 可能是裸仓库。
func isRepoDir(dir string) bool {
	if dir == "" {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil || fi.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	return true
}
