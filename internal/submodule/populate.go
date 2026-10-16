// Package submodule 递归拉取构建工作区的 git 子模块。
//
// 背景:build.Cloner / repocache.Cache 克隆的是超仓库,PlainClone 不会拉取 .gitmodules
// 声明的子模块——工作区里子模块目录为空,Docker 构建上下文缺文件。本包在克隆/检出
// 完成后,把每个子模块按超仓库 index 里 pinned 的 gitlink commit 克隆到对应路径,
// 并递归处理嵌套子模块。
//
// 安全:.gitmodules 内容随仓库可控,每个子模块 URL/路径都按不可信输入处理:
//   - URL 过 gitauth.ValidateRepoURL 同款 SSRF 收口(http/https/ssh;拒回环/链路本地/
//     云元数据),另拒本地路径形态——防把宿主目录拉进构建上下文外泄;
//   - 凭据只在子模块 host 与父仓库一致时转发(防恶意仓库借 .gitmodules 把主仓凭据
//     送到第三方服务器);跨 host 一律匿名,匿名失败即失败;
//   - 子模块 path 拒绝绝对路径与 ".." 逃逸(不得越出工作区)。
//
// 不采用 go-git 自带的 Submodules.UpdateContext:其鉴权是单一 Auth 应用于全部子模块
// URL(无法按 host 收口),相对 URL 按父仓远端解析(repocache 下会错误地对本地镜像
// 路径解析)。仅复用 Worktree.Submodules() 做 .gitmodules 解析(自带路径/名字加固,
// 无文件时返回空表)。
package submodule

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/huangchengsir/pipewright/internal/gitauth"
	"github.com/huangchengsir/pipewright/internal/vault"
)

const (
	// cloneTimeout 是单个子模块克隆的硬超时(与 build.Cloner 的单仓克隆同宽,防黑洞地址)。
	cloneTimeout = 5 * time.Minute
	// maxDepth 是子模块递归深度上限(gitlink 的 commit DAG 正常无环,上限纯防病态构造)。
	maxDepth = 16
)

// 领域错误(不携带 URL/凭据细节;由调用方映射干净哨兵)。
var (
	// ErrBlocked 表示子模块 URL/路径未通过收口(SSRF、本地路径、路径逃逸)。
	ErrBlocked = errors.New("submodule: url or path blocked by guard")
	// ErrTooDeep 表示子模块嵌套超过 maxDepth。
	ErrTooDeep = errors.New("submodule: submodule nesting too deep")
)

// Populate 递归拉取 destDir 工作区 .gitmodules 声明的子模块,检出到超仓库 pinned 的
// commit,并递归处理嵌套子模块。parentURL 是父仓库的**真实远端地址**(相对子模块 URL
// 按它解析;repocache 场景须传原始仓库地址而非本地镜像路径)。凭据按 host 收口转发。
// allowInsecure 仅供测试放行 file:// 本地夹具,生产调用方必须传 false。
func Populate(ctx context.Context, destDir, parentURL string, cred vault.GitAuth, allowInsecure bool) error {
	return populate(ctx, destDir, parentURL, cred, allowInsecure, 0)
}

func populate(ctx context.Context, dir, parentURL string, cred vault.GitAuth, insecure bool, depth int) error {
	if depth > maxDepth {
		return ErrTooDeep
	}
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("submodule: open worktree: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("submodule: worktree: %w", err)
	}
	subs, err := wt.Submodules()
	if err != nil {
		return fmt.Errorf("submodule: parse .gitmodules: %w", err)
	}

	for _, sub := range subs {
		cfg := sub.Config()
		name := strings.TrimSpace(cfg.Name)
		rawPath := strings.TrimSpace(cfg.Path)
		rawURL := strings.TrimSpace(cfg.URL)
		if rawPath == "" || rawURL == "" {
			return fmt.Errorf("submodule %q: empty path or url", name)
		}
		subDir, ok := safeSubPath(dir, rawPath)
		if !ok {
			return fmt.Errorf("%w: submodule %q escapes worktree", ErrBlocked, name)
		}

		// gitlink:超仓库 index 里 pinned 的 commit。零值 = .gitmodules 声明了但
		// index 未注册(git 同语义:未 add 的子模块),跳过。
		st, serr := sub.Status()
		if serr != nil {
			return fmt.Errorf("submodule %q: status: %w", name, serr)
		}
		if st.Expected.IsZero() {
			continue
		}

		resolved := resolveSubmoduleURL(parentURL, rawURL)
		if !insecure && !allowedSubmoduleURL(resolved) {
			return fmt.Errorf("%w: submodule %q url", ErrBlocked, name)
		}

		if perr := pullSubmodule(ctx, name, subDir, resolved, parentURL, st.Expected, cred); perr != nil {
			return perr
		}
		if rerr := populate(ctx, subDir, resolved, cred, insecure, depth+1); rerr != nil {
			return rerr
		}
	}
	return nil
}

// pullSubmodule 把单个子模块仓库全量克隆到 subDir 并检出 pinned commit。全量克隆是
// 必要的:pinned commit 不保证在任何分支尖上,无法用浅克隆+ref 定位。
func pullSubmodule(ctx context.Context, name, subDir, subURL, parentURL string, pinned plumbing.Hash, cred vault.GitAuth) error {
	auth := submoduleAuth(subURL, parentURL, cred)

	cctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()

	repo, err := gogit.PlainCloneContext(cctx, subDir, false, &gogit.CloneOptions{
		URL:  subURL,
		Auth: auth,
		Tags: gogit.NoTags,
	})
	if err != nil {
		return fmt.Errorf("submodule %q: clone: %w", name, err)
	}
	wt, werr := repo.Worktree()
	if werr != nil {
		return fmt.Errorf("submodule %q: worktree: %w", name, werr)
	}
	if cerr := wt.Checkout(&gogit.CheckoutOptions{Hash: pinned, Force: true}); cerr != nil {
		// pinned commit 不在任何分支尖(如未合入的 bump 提交)→ sha 定向 fetch 后重试
		// (go-git Submodule.fetchAndCheckout 同款回退;需服务端支持
		// uploadpack.allowReachableSHA1InWant)。
		ferr := repo.FetchContext(cctx, &gogit.FetchOptions{
			Auth:     auth,
			RefSpecs: []config.RefSpec{config.RefSpec("+" + pinned.String() + ":" + pinned.String())},
		})
		if ferr != nil && !errors.Is(ferr, gogit.NoErrAlreadyUpToDate) {
			return fmt.Errorf("submodule %q: fetch pinned commit: %w", name, ferr)
		}
		if cerr2 := wt.Checkout(&gogit.CheckoutOptions{Hash: pinned, Force: true}); cerr2 != nil {
			return fmt.Errorf("submodule %q: checkout pinned commit: %w", name, cerr2)
		}
	}
	return nil
}

// submoduleAuth 装配子模块克隆凭据:secret 为空 → 匿名;子模块 host 与父仓库一致才
// 转发凭据(host 收口,防 .gitmodules 声明第三方地址窃取主仓凭据),装配失败(如
// 同 host 但协议错配)也降级匿名——匿名失败会在克隆处以干净错误暴露。
func submoduleAuth(subURL, parentURL string, cred vault.GitAuth) transport.AuthMethod {
	if strings.TrimSpace(cred.Secret) == "" {
		return nil
	}
	_, subHost, _, sok := gitauth.ParseRepoURL(subURL)
	_, parentHost, _, pok := gitauth.ParseRepoURL(parentURL)
	if !sok || !pok || !strings.EqualFold(subHost, parentHost) {
		return nil
	}
	auth, err := gitauth.TransportAuth(subURL, cred)
	if err != nil {
		return nil
	}
	return auth
}

// schemeURLRe 匹配带 scheme 的绝对 URL(http://、ssh://、file:// …)。
var schemeURLRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// scpURLRe 匹配 SCP 风格绝对地址 [user@]host:path(冒号在首个斜杠之前,与
// gitauth.ParseRepoURL / go-git transport.NewEndpoint 同语义);捕获组:[user@]host: 前缀、路径。
var scpURLRe = regexp.MustCompile(`^((?:[^@/]+@)?[^/:]+):(.+)$`)

// windowsDriveRe 匹配 Windows 盘符形态本地路径("C:\x"、"c:/x")。gitauth 的 SCP
// 归一会把单字母 host 认成 ssh 地址(host="C"),而本地路径克隆是宿主目录外泄通道,
// 必须先拒。
var windowsDriveRe = regexp.MustCompile(`^[A-Za-z]:`)

// resolveSubmoduleURL 解析子模块 URL:绝对形态(scheme://、SCP 或绝对本地路径)原样
// 返回;相对形态(./x、../x,git 语义仅这两种前缀算相对)按父仓库 URL 形态拼接——
// http(s)/ssh/file 用 net/url;SCP 用手工前缀;本地路径(仅测试夹具/镜像)按目录拼接。
// 解析失败原样返回,由下游 SSRF 收口兜底拒绝。
func resolveSubmoduleURL(parent, sub string) string {
	sub = strings.TrimSpace(sub)
	parent = strings.TrimSpace(parent)
	switch {
	case sub == "":
		return sub
	case schemeURLRe.MatchString(sub), scpURLRe.MatchString(sub),
		path.IsAbs(sub), filepath.IsAbs(filepath.FromSlash(sub)):
		return sub
	}

	// 相对 URL。
	if schemeURLRe.MatchString(parent) {
		if u, uerr := url.Parse(parent); uerr == nil && u.Path != "" {
			u.Path = path.Join(path.Dir(u.Path), sub)
			u.RawPath = ""
			return u.String()
		}
	}
	if !windowsDriveRe.MatchString(parent) {
		if m := scpURLRe.FindStringSubmatch(parent); m != nil {
			joined := path.Join(path.Dir(m[2]), sub)
			return m[1] + ":" + joined
		}
	}
	return filepath.Join(filepath.Dir(filepath.FromSlash(parent)), filepath.FromSlash(sub))
}

// allowedSubmoduleURL 是子模块 URL 的生产收口:gitauth.ValidateRepoURL 同款 SSRF
// (http/https/ssh,拒回环/链路本地/云元数据;file:// 等其它协议被拒)+ Windows
// 盘符本地路径显式拒绝。
func allowedSubmoduleURL(u string) bool {
	if windowsDriveRe.MatchString(u) {
		return false
	}
	return gitauth.ValidateRepoURL(u) == nil
}

// safeSubPath 把 .gitmodules 声明的子模块 path 收口到工作区内:拒绝绝对路径与
// ".." 逃逸(防子仓库被写出到构建工作区之外)。返回目标绝对目录。
func safeSubPath(base, p string) (string, bool) {
	clean := filepath.Clean(filepath.FromSlash(p))
	if clean == "" || clean == "." || filepath.IsAbs(clean) ||
		clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Join(base, clean), true
}
