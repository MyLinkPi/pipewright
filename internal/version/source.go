package version

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

const (
	// defaultRepo 是检查更新所查询的默认仓库(owner/name)。可经 PIPEWRIGHT_RELEASE_REPO
	// 覆盖(便于 fork 指向自己的发布渠道)。
	defaultRepo = "huangchengsir/pipewright"

	githubAPIBase = "https://api.github.com"
	githubWebBase = "https://github.com"
)

// Source 是一次「检查更新 / 下载升级包」所用的升级源。各 base 均为 GitHub 路径兼容的
// 前缀(可含子路径,不带尾斜杠):
//   - 检查: {APIBase}/repos/{Repo}/releases/latest
//   - 兜底: {WebBase}/{Repo}/releases/latest(302 重定向解析 tag)与 {WebBase}/{Repo}/releases.atom
//   - 下载: {DLBase}/{Repo}/releases/download/{tag}/{资产} 与 checksums.txt
//
// 留空字段回落 GitHub 官方源。自建镜像(如 nginx 反代 api.github.com 与 github.com)或
// 内网发布站只需按同样路径布局转发,即可替代 GitHub;二进制完整性仍由 sha256 校验和保障
// (镜像属管理员显式配置的信任源,与信任 GitHub 本身同级)。
type Source struct {
	Repo    string
	APIBase string
	WebBase string
	DLBase  string
}

// SourceProvider 返回当前生效的升级源;每次检查/下载前调用,使升级源修改即时生效、无需重启。
// 实现应自带优雅降级(读配置失败回落 env / 官方源),不得 panic。
type SourceProvider func(ctx context.Context) Source

// ResolveSource 把镜像 base 组装成升级源,优先级:传入的库内配置 > env
// PIPEWRIGHT_RELEASE_MIRROR > GitHub 官方源。非空镜像 = 三个 base 都指向它
// (镜像须按 GitHub 路径布局转发,见 Source 注释;允许子路径前缀)。repo 与镜像
// 无关,恒取 PIPEWRIGHT_RELEASE_REPO(fork 覆盖)后回落 defaultRepo。
func ResolveSource(mirror string) Source {
	repo := strings.TrimSpace(os.Getenv("PIPEWRIGHT_RELEASE_REPO"))
	if repo == "" {
		repo = defaultRepo
	}
	mirror = strings.TrimRight(strings.TrimSpace(mirror), "/")
	if mirror == "" {
		mirror = envMirror()
	}
	mirror = strings.TrimRight(mirror, "/")
	if mirror == "" {
		return Source{Repo: repo, APIBase: githubAPIBase, WebBase: githubWebBase, DLBase: githubWebBase}
	}
	return Source{Repo: repo, APIBase: mirror, WebBase: mirror, DLBase: mirror}
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
