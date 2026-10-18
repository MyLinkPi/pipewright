// Package systemcfg 是「系统级运行时配置」的领域层:平台自身对外行为的可在线修改配置,
// 取代需要重启的环境变量。目前有:
//   - public_url(平台对外访问地址):
//     通知里的签名审批链接(internal/httpapi notify_hook)以它为前缀;
//     PR 状态回写的 target_url(internal/httpapi prstatus)以它为前缀;
//     Passkey WebAuthn RPID/Origin 自动推导的来源之一;
//     空 = 未配置,相关外链优雅关闭(不发审批链接、回写不带 target_url)。
//   - release_mirror(自升级镜像源 base URL):
//     检查更新(internal/version)与二进制下载所用 GitHub 路径兼容镜像的 base;
//     空 = 未配置,回退 GitHub 官方源(env PIPEWRIGHT_RELEASE_MIRROR 为部署级兜底)。
//   - Passkey/WebAuthn RP 配置(webauthn_rp_id / webauthn_origins / passkey_login_enabled):
//     两步验证「通行密钥」端的依赖方标识;留空时按 public_url → 请求 Host 自动推导。
//
// 消费方经 Resolve / ResolveReleaseMirror 在每次触发时读取(单行主键查询,频率低),
// 运行时修改即时生效、无需重启。
package systemcfg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// ErrInvalidURL 表示 public_url 形态非法(须为 http/https 绝对地址,或空 = 清除)。
var ErrInvalidURL = errors.New("systemcfg: invalid public url")

// ErrInvalidMirror 表示 release_mirror 形态非法(须为 http/https 绝对地址,可带子路径,或空 = 清除)。
var ErrInvalidMirror = errors.New("systemcfg: invalid release mirror")

// ErrInvalidRPID 表示 webauthn_rp_id 形态非法(须为合法域名,可带端口,或空 = 自动推导)。
var ErrInvalidRPID = errors.New("systemcfg: invalid webauthn rp id")

// ErrInvalidOrigins 表示 webauthn_origins 形态非法(逗号分隔的 http/https origin,或空 = 自动推导)。
var ErrInvalidOrigins = errors.New("systemcfg: invalid webauthn origins")

// maxURLLen 是 public_url / release_mirror / webauthn 配置的长度上限。
const maxURLLen = 512

// Config 是系统配置单例的领域模型(行由迁移种子保证存在;后续系统级配置在此加列)。
type Config struct {
	PublicURL     string
	ReleaseMirror string
	// WebAuthnRPID / WebAuthnOrigins 是 Passkey(WebAuthn)依赖方显式配置;空 = 自动推导
	// (public_url → 请求 Host)。Origins 为逗号分隔列表。
	WebAuthnRPID        string
	WebAuthnOrigins     string
	PasskeyLoginEnabled bool
	// MFASecretKey 是 TOTP 密钥加密的兜底密钥(vault master key 未配置时自动生成)。
	// 仅 mfa 包内部读写,不经 HTTP API 暴露。
	MFASecretKey string
	UpdatedAt    time.Time
}

// Service 定义系统配置对外接口(httpapi 消费;钩子经 Resolve 读)。
type Service interface {
	// Get 返回配置单例(行由迁移种子保证存在;读失败不构成"未配置"语义)。
	Get(ctx context.Context) (Config, error)
	// SetPublicURL 校验并落库 public_url(空串 = 清除,外链功能关闭)。
	SetPublicURL(ctx context.Context, raw string) (Config, error)
	// SetReleaseMirror 校验并落库 release_mirror(空串 = 清除,升级源回退 GitHub 官方)。
	SetReleaseMirror(ctx context.Context, raw string) (Config, error)
	// SetWebAuthnRPID 校验并落库 webauthn_rp_id(空串 = 清除,改回自动推导)。
	SetWebAuthnRPID(ctx context.Context, raw string) (Config, error)
	// SetWebAuthnOrigins 校验并落库 webauthn_origins(逗号分隔;空串 = 清除,改回自动推导)。
	SetWebAuthnOrigins(ctx context.Context, raw string) (Config, error)
	// SetPasskeyLoginEnabled 落库 passkey_login_enabled(通行密钥登录总开关)。
	SetPasskeyLoginEnabled(ctx context.Context, enabled bool) (Config, error)
}

// Resolve 返回当前 public_url;任何读取失败一律回退空串(等价"未配置",优雅降级),
// 供钩子每次触发时取值。
func Resolve(ctx context.Context, svc Service) string {
	if svc == nil {
		return ""
	}
	c, err := svc.Get(ctx)
	if err != nil {
		return ""
	}
	return c.PublicURL
}

// ResolveReleaseMirror 返回当前升级镜像 base;任何读取失败一律回退空串(= 回退 GitHub
// 官方源,优雅降级),供升级检查在每次触发时取值(读失败不阻断检查)。
func ResolveReleaseMirror(ctx context.Context, svc Service) string {
	if svc == nil {
		return ""
	}
	c, err := svc.Get(ctx)
	if err != nil {
		return ""
	}
	return c.ReleaseMirror
}

type service struct{ db *sql.DB }

// New 构造 Service(单行表由迁移种子,读路径无需兜底插入)。
func New(db *sql.DB) Service { return &service{db: db} }

func (s *service) Get(ctx context.Context) (Config, error) {
	var (
		c          Config
		updatedStr string
		passkeyOn  int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT public_url, release_mirror, updated_at,
		        COALESCE(webauthn_rp_id, ''), COALESCE(webauthn_origins, ''),
		        COALESCE(passkey_login_enabled, 1), COALESCE(mfa_secret_key, '')
		 FROM system_config WHERE id = 1`).
		Scan(&c.PublicURL, &c.ReleaseMirror, &updatedStr,
			&c.WebAuthnRPID, &c.WebAuthnOrigins, &passkeyOn, &c.MFASecretKey)
	if err != nil {
		return Config{}, fmt.Errorf("systemcfg: get: %w", err)
	}
	c.PasskeyLoginEnabled = passkeyOn != 0
	if updatedStr != "" {
		if t, perr := time.Parse(time.RFC3339, updatedStr); perr == nil {
			c.UpdatedAt = t
		}
	}
	return c, nil
}

func (s *service) SetPublicURL(ctx context.Context, raw string) (Config, error) {
	u, err := NormalizeURL(raw)
	if err != nil {
		return Config{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE system_config SET public_url = ?, updated_at = ? WHERE id = 1`, u, now); err != nil {
		return Config{}, fmt.Errorf("systemcfg: set public url: %w", err)
	}
	return s.Get(ctx)
}

func (s *service) SetReleaseMirror(ctx context.Context, raw string) (Config, error) {
	u, err := NormalizeMirrorURL(raw)
	if err != nil {
		return Config{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE system_config SET release_mirror = ?, updated_at = ? WHERE id = 1`, u, now); err != nil {
		return Config{}, fmt.Errorf("systemcfg: set release mirror: %w", err)
	}
	return s.Get(ctx)
}

func (s *service) SetWebAuthnRPID(ctx context.Context, raw string) (Config, error) {
	id, err := NormalizeRPID(raw)
	if err != nil {
		return Config{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE system_config SET webauthn_rp_id = ?, updated_at = ? WHERE id = 1`, id, now); err != nil {
		return Config{}, fmt.Errorf("systemcfg: set webauthn rp id: %w", err)
	}
	return s.Get(ctx)
}

func (s *service) SetWebAuthnOrigins(ctx context.Context, raw string) (Config, error) {
	list, err := NormalizeOrigins(raw)
	if err != nil {
		return Config{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE system_config SET webauthn_origins = ?, updated_at = ? WHERE id = 1`, list, now); err != nil {
		return Config{}, fmt.Errorf("systemcfg: set webauthn origins: %w", err)
	}
	return s.Get(ctx)
}

func (s *service) SetPasskeyLoginEnabled(ctx context.Context, enabled bool) (Config, error) {
	v := 0
	if enabled {
		v = 1
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE system_config SET passkey_login_enabled = ?, updated_at = ? WHERE id = 1`, v, now); err != nil {
		return Config{}, fmt.Errorf("systemcfg: set passkey login enabled: %w", err)
	}
	return s.Get(ctx)
}

// NormalizeURL 归一化并校验 public_url:去空白、去尾部 "/"(消费方拼接子路径);
// 空串原样通过(= 清除);非空须为 http/https 绝对地址且带 host。
func NormalizeURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	u = strings.TrimRight(u, "/")
	if u == "" {
		return "", nil
	}
	if len(u) > maxURLLen {
		return "", ErrInvalidURL
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", ErrInvalidURL
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", ErrInvalidURL
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalidURL // 只接受 origin(https://host[:port]),不带路径/查询/片段
	}
	return u, nil
}

// NormalizeMirrorURL 归一化并校验 release_mirror:去空白、去尾部 "/";
// 空串原样通过(= 清除,升级源回退 GitHub 官方);非空须为 http/https 绝对地址且带 host,
// 允许子路径前缀(如 https://mirror.example.com/gh),拒绝查询串与片段。
// 与 NormalizeURL(public_url)的差别仅在于放行路径:镜像 base 会被升级检查直接拼
// /repos/... 等子路径,子路径前缀属合法部署形态(同域反代挂子路径)。
func NormalizeMirrorURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	u = strings.TrimRight(u, "/")
	if u == "" {
		return "", nil
	}
	if len(u) > maxURLLen {
		return "", ErrInvalidMirror
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", ErrInvalidMirror
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", ErrInvalidMirror
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalidMirror // 不接受查询串/片段(拼接子路径语义不明)
	}
	return u, nil
}

// NormalizeRPID 归一化并校验 webauthn_rp_id:去空白、去端口、转小写。
// 空串原样通过(= 清除,改回自动推导);非空须为合法域名形态(字母/数字/点/连字符,
// 不含路径/协议)。只做形态校验,不做 DNS 解析。
func NormalizeRPID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", nil
	}
	// 允许用户带端口录入,统一剥掉(RPID 规范即不含端口)。
	if h, _, err := net.SplitHostPort(id); err == nil {
		id = h
	}
	id = strings.ToLower(id)
	if len(id) > 255 || id == "" {
		return "", ErrInvalidRPID
	}
	for _, r := range id {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-'
		if !valid {
			return "", ErrInvalidRPID
		}
	}
	if strings.HasPrefix(id, ".") || strings.HasSuffix(id, ".") ||
		strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		return "", ErrInvalidRPID
	}
	return id, nil
}

// NormalizeOrigins 归一化并校验 webauthn_origins:逗号分隔的 http/https origin 列表
// (scheme://host[:port],无路径/查询/片段;协议与主机转小写)。空串原样通过(= 清除)。
// 返回规范化后的逗号拼接串。允许 http(开发环境 localhost 部署);生产要求 HTTPS 由
// 浏览器 WebAuthn 安全文强制,服务端不强拦。
func NormalizeOrigins(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parts := strings.Split(raw, ",")
	normalized := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > maxURLLen {
			return "", ErrInvalidOrigins
		}
		u, err := url.Parse(p)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "", ErrInvalidOrigins
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", ErrInvalidOrigins
		}
		if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return "", ErrInvalidOrigins // 仅接受 origin 形态
		}
		normalized = append(normalized, strings.ToLower(u.Scheme)+"://"+u.Host)
	}
	if len(normalized) == 0 {
		return "", ErrInvalidOrigins
	}
	return strings.Join(normalized, ","), nil
}
