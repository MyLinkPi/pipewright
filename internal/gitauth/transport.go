// transport.go:按「仓库 URL 协议 × 凭据类型」装配 go-git 认证方式 + 共享 SSRF 收口。
//
// 背景:凭据类型有 HTTPS 系(git_token / git_http,BasicAuth)与 SSH 系
// (ssh_key / ssh_password,私钥或密码),仓库协议有 http(s) 与 ssh
// (ssh:// 或 git@host:path SCP 风格)。两者错配(如 https 地址绑 ssh_key)
// 必须显式报错而非静默降级,否则用户面对的是一句难懂的克隆失败。
//
// 安全:私钥/密码只进 x/crypto/ssh 参数化 API,绝不进 URL / 日志 / 错误文本。
// host key 校验沿 internal/target/ssh.go 的既有决策:InsecureIgnoreHostKey +
// DEFERRED(生产须 known_hosts 固定),不在本次引入。
package gitauth

import (
	"errors"
	"net"
	neturl "net/url"
	"regexp"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/huangchengsir/pipewright/internal/vault"
)

// 认证错配/私钥不可解析的哨兵错误(不携带 URL/私钥细节)。
var (
	// ErrSchemeMismatch 表示凭据类型与仓库协议不匹配(如 https 地址绑了 ssh_key,
	// 或 git@ 地址只绑了 https token)。
	ErrSchemeMismatch = errors.New("gitauth: credential type does not match repo url scheme")
	// ErrInvalidSSHKey 表示 ssh_key 凭据的明文不是可解析的 PEM/OpenSSH 私钥
	// (含带 passphrase 的私钥:当前不支持)。
	ErrInvalidSSHKey = errors.New("gitauth: ssh private key is not parseable")
	// ErrRepoBlocked 表示仓库地址未通过 SSRF 收口(协议不允许或命中禁止 IP 段)。
	ErrRepoBlocked = errors.New("gitauth: repo url not allowed by ssrf guard")
)

// sshSecretTypes 是经 SSH 传输层认证的凭据类型。
var sshSecretTypes = map[string]bool{
	vault.TypeSSHKey:      true,
	vault.TypeSSHPassword: true,
}

// TransportAuth 依据 repoURL 协议与凭据类型选择认证方式:
//
//	http(s) + git_token/git_http → BasicAuth(密码=secret,历史行为)
//	ssh      + ssh_key           → x/crypto PublicKeys(解析 PEM)
//	ssh      + ssh_password      → ssh.Password
//	协议与类型错配                → ErrSchemeMismatch
//	secret 为空                   → nil(匿名,公开仓)
//
// SSH 用户名取凭据 username(留空则由 go-git 用 URL user 段,缺省 "git")。
func TransportAuth(repoURL string, cred vault.GitAuth) (transport.AuthMethod, error) {
	if strings.TrimSpace(cred.Secret) == "" {
		return nil, nil
	}
	scheme, _, _, ok := ParseRepoURL(repoURL)
	if !ok {
		// 地址不合法:让上层按地址被拒处理(各入口在克隆前另有 SSRF 收口,
		// 此处兜底返回 ErrRepoBlocked 而非裸 BasicAuth)。
		return nil, ErrRepoBlocked
	}
	isSSH := scheme == "ssh"
	_, sshType := sshSecretTypes[cred.Type]

	if isSSH {
		if !sshType {
			return nil, ErrSchemeMismatch
		}
		// go-git 的 ssh auth 会把空 User 原样发给服务端(整体覆盖 ClientConfig,
		// 不回退 URL user 段)→ 认证必被拒。必须在此解析出非空用户名。
		user := sshUser(repoURL, cred.Username)
		if cred.Type == vault.TypeSSHKey {
			pk, err := gogitssh.NewPublicKeys(user, []byte(cred.Secret), "")
			if err != nil {
				return nil, ErrInvalidSSHKey
			}
			// ⚠️ DEFERRED(与 internal/target/ssh.go 同款):生产必须固定 known_hosts
			// (ssh.FixedHostKey / knownhosts.New),否则无法防 MITM。见 target 包注释。
			pk.HostKeyCallback = ssh.InsecureIgnoreHostKey() //nolint:gosec // 同 target 包 DEFERRED 决策
			return pk, nil
		}
		pw := &gogitssh.Password{User: user}
		pw.HostKeyCallback = ssh.InsecureIgnoreHostKey() //nolint:gosec // 同上,避免 connect 时读不到 known_hosts 直接失败
		return pw, nil
	}

	// http(s):SSH 系凭据没法当 BasicAuth 用,显式报错。
	if sshType {
		return nil, ErrSchemeMismatch
	}
	return BasicAuth(repoURL, cred.Username, cred.Secret), nil
}

// sshUser 解析 SSH 认证用户名:凭据显式 username → URL user 段(git@host:...)→
// 缺省 "git"(GitHub/GitLab/Gitee/Codeup 等 git over SSH 的通用约定用户)。
func sshUser(repoURL, credUsername string) string {
	if u := strings.TrimSpace(credUsername); u != "" {
		return u
	}
	s := strings.TrimSpace(repoURL)
	if i := strings.Index(s, "://"); i >= 0 {
		if ru, err := neturl.Parse(s); err == nil && ru.User != nil {
			if u := ru.User.Username(); u != "" {
				return u
			}
		}
		return "git"
	}
	if m := scpLikeRe.FindStringSubmatch(s); m != nil && m[1] != "" {
		return m[1]
	}
	return "git"
}

// scpLikeRe 匹配 SCP 风格仓库地址(无 scheme):[user@]host:path。
// 冒号必须在首个斜杠之前(host:path 的 path 可含 /),与 git transport.NewEndpoint 同语义。
var scpLikeRe = regexp.MustCompile(`^(?:([^@/]+)@)?([^/:]+):(.+)$`)

// ParseRepoURL 把仓库地址归一化为 (scheme, host, user)。规则:
//   - 显式 "://" URL:仅 http/https/ssh 被接受,其余(file://、git:// 等)拒绝;
//   - 无 "://" 的 [user@]host:path:按 SCP 风格归一化为 ssh(host:path 里 host
//     会被 url.Parse 误吃成 scheme,如 "localhost:8080/x",故必须先看 "://");
//   - 两者都不满足 → ok=false。
//
// host 为空串(如 "ssh://")视为不合法;user 是 URL user 段 / SCP user,可空。
func ParseRepoURL(repoURL string) (scheme, host, user string, ok bool) {
	s := strings.TrimSpace(repoURL)
	if i := strings.Index(s, "://"); i >= 0 {
		sch := strings.ToLower(s[:i])
		switch sch {
		case "http", "https", "ssh":
		default:
			return "", "", "", false
		}
		host, user := "", ""
		if u, err := neturl.Parse(s); err == nil {
			host = u.Hostname()
			if u.User != nil {
				user = u.User.Username()
			}
		}
		if host == "" {
			return "", "", "", false
		}
		return sch, host, user, true
	}
	if m := scpLikeRe.FindStringSubmatch(s); m != nil && m[2] != "" {
		return "ssh", m[2], m[1], true
	}
	return "", "", "", false
}

// IsSSHURL 判断 repoURL 是否 SSH 协议:ssh:// scheme 或 SCP 风格(git@host:path)。
func IsSSHURL(repoURL string) bool {
	scheme, _, _, ok := ParseRepoURL(repoURL)
	return ok && scheme == "ssh"
}

// ValidateRepoURL 是全平台共享的仓库地址 SSRF 收口:
//   - 协议仅允许 http/https/ssh(含 git@host:path SCP 风格,按 ssh 对待);
//   - 解析 host:拒回环 / 链路本地(含云元数据 169.254.169.254)/ 未指定地址;
//   - 私网(RFC1918 / fc00::/7)放行(自托管内网 Git 友好);
//   - 非字面 IP 的 host DNS 解析失败时放行(留给克隆路径失败,不误拒临时抖动)。
//
// 通过返回 nil。该实现此前在 build/project/ai/httpapi 各有一份拷贝,现收敛于此。
func ValidateRepoURL(repoURL string) error {
	_, host, _, ok := ParseRepoURL(repoURL)
	if !ok || host == "" {
		return ErrRepoBlocked
	}
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return ErrRepoBlocked
		}
		return nil
	}
	addrs, err := net.LookupIP(host)
	if err != nil || len(addrs) == 0 {
		return nil
	}
	for _, ip := range addrs {
		if blockedIP(ip) {
			return ErrRepoBlocked
		}
	}
	return nil
}

// blockedIP 判定 IP 是否落在禁止区:回环、链路本地(含云元数据)、未指定。
// 私网(RFC1918 / fc00::/7)不在此列(放行)。
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}
