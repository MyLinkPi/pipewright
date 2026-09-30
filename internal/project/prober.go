package project

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/huangchengsir/pipewright/internal/gitauth"
	"github.com/huangchengsir/pipewright/internal/vault"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
)

// probeTimeout 是单次 ls-remote 探测的硬超时(防黑洞 IP/慢 DNS 把请求 goroutine 挂到 OS TCP 超时)。
const probeTimeout = 15 * time.Second

// goGitProber 用 go-git 的 ListRemote(= git ls-remote 语义)做仓库连通校验。
// 纯 Go,不要求宿主装 git;认证经 gitauth.TransportAuth 按协议装配(https token 或
// ssh 私钥/密码,参数化 API,绝不拼命令字符串)。全程在内存进行(memory.NewStorage),
// 不落任何工作区/对象到磁盘。
//
// SSRF 收口:生产路径允许 http/https/ssh scheme(含 git@host:path);拒云元数据/链路
// 本地/回环地址;私网 IP(自托管内网 Git)放行。allowInsecureSchemes 仅供测试注入
// (放行 file:// 等本地夹具),生产构造(project.New 默认)永远为 false。
type goGitProber struct {
	// allowInsecureSchemes 仅供测试:为 true 时跳过 scheme/host SSRF 校验(放行 file:// 夹具)。
	// 生产路径绝不设置此字段。
	allowInsecureSchemes bool
}

// Probe 用凭据对 repoURL 做 ListRemote,成功返回远端默认分支(由 HEAD 符号引用解析)。
//
// 安全:凭据明文仅进参数化 API,绝不进 URL/日志/错误。
// 失败统一映射为干净领域错误:鉴权类(含凭据类型与协议错配、私钥不可解析)→
// ErrCredentialError;其余(DNS/连接/不存在)→ ErrRepoUnreachable。返回的错误
// 不 %w 原始错误,避免把含敏感细节的底层错误外泄。
func (p goGitProber) Probe(ctx context.Context, repoURL string, cred vault.GitAuth) (string, error) {
	if strings.TrimSpace(repoURL) == "" {
		return "", ErrRepoUnreachable
	}

	if !p.allowInsecureSchemes {
		if err := validateRepoURL(repoURL); err != nil {
			return "", err
		}
	}

	rem := gogit.NewRemote(memory.NewStorage(), &gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})

	// 认证按协议装配:https token/密码 → BasicAuth(Gitee 须真实账号名);ssh → 私钥/密码。
	// 见 gitauth 包。
	auth, aerr := gitauth.TransportAuth(repoURL, cred)
	if aerr != nil {
		return "", classifyAuthErr(aerr)
	}

	// 硬超时:防黑洞 IP/慢 DNS 把请求 goroutine 挂死。
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	refs, err := rem.ListContext(cctx, &gogit.ListOptions{Auth: auth})
	if err != nil {
		return "", classifyProbeErr(err)
	}

	return defaultBranchFromRefs(refs), nil
}

// validateRepoURL 对仓库地址做 SSRF 收口(生产路径),委托全平台共享的
// gitauth.ValidateRepoURL:允许 http/https/ssh(含 git@host:path);拒云元数据/
// 链路本地/回环;私网放行。返回 ErrRepoUnreachable(不泄漏内部细节)。
func validateRepoURL(repoURL string) error {
	if gitauth.ValidateRepoURL(repoURL) != nil {
		return ErrRepoUnreachable
	}
	return nil
}

// classifyAuthErr 把认证装配错误(类型错配/私钥不可解析)映射为干净领域错误。
func classifyAuthErr(err error) error {
	switch {
	case errors.Is(err, gitauth.ErrSchemeMismatch),
		errors.Is(err, gitauth.ErrInvalidSSHKey):
		return ErrCredentialError
	default:
		return ErrRepoUnreachable
	}
}

// classifyProbeErr 把 go-git/transport 错误映射为干净领域错误(不携带底层文本)。
//
// 仅依赖 go-git/transport 的哨兵错误判定凭据问题,绝不做 "401"/"403" 文本子串嗅探
// (会把 "403ms"、含数字的主机名等无关文本误判为鉴权失败)。无法明确归为凭据错误的
// 一律归不可达(DNS/连接/仓库不存在/协议错误等)。SSH 鉴权失败同样落在 transport
// 哨兵错误上(ErrAuthorizationFailed 等),无需文本嗅探。
func classifyProbeErr(err error) error {
	switch {
	case errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed),
		errors.Is(err, transport.ErrInvalidAuthMethod):
		return ErrCredentialError
	default:
		return ErrRepoUnreachable
	}
}

// defaultBranchFromRefs 从 ls-remote 引用列表解析远端默认分支(HEAD 指向的分支短名)。
// 解析不出时返回空字符串(调用方按缺省处理,不视为错误)。
func defaultBranchFromRefs(refs []*plumbing.Reference) string {
	// HEAD 通常是指向 refs/heads/<branch> 的符号引用。
	for _, r := range refs {
		if r.Name() == plumbing.HEAD && r.Type() == plumbing.SymbolicReference {
			return r.Target().Short()
		}
	}
	// 退化:若 HEAD 是 hash 引用,匹配同 hash 的某个分支。
	var headHash plumbing.Hash
	for _, r := range refs {
		if r.Name() == plumbing.HEAD {
			headHash = r.Hash()
			break
		}
	}
	if !headHash.IsZero() {
		for _, r := range refs {
			if r.Name().IsBranch() && r.Hash() == headHash {
				return r.Name().Short()
			}
		}
	}
	return ""
}
