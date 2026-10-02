// Package systemcfg 是「系统级运行时配置」的领域层:平台自身对外行为的可在线修改配置,
// 取代需要重启的环境变量。目前有:
//   - public_url(平台对外访问地址):
//     通知里的签名审批链接(internal/httpapi notify_hook)以它为前缀;
//     PR 状态回写的 target_url(internal/httpapi prstatus)以它为前缀;
//     空 = 未配置,相关外链优雅关闭(不发审批链接、回写不带 target_url)。
//   - release_mirror(自升级镜像源 base URL):
//     检查更新(internal/version)与二进制下载所用 GitHub 路径兼容镜像的 base;
//     空 = 未配置,回退 GitHub 官方源(env PIPEWRIGHT_RELEASE_MIRROR 为部署级兜底)。
//
// 消费方经 Resolve / ResolveReleaseMirror 在每次触发时读取(单行主键查询,频率低),
// 运行时修改即时生效、无需重启。
package systemcfg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ErrInvalidURL 表示 public_url 形态非法(须为 http/https 绝对地址,或空 = 清除)。
var ErrInvalidURL = errors.New("systemcfg: invalid public url")

// ErrInvalidMirror 表示 release_mirror 形态非法(须为 http/https 绝对地址,可带子路径,或空 = 清除)。
var ErrInvalidMirror = errors.New("systemcfg: invalid release mirror")

// maxURLLen 是 public_url / release_mirror 的长度上限。
const maxURLLen = 512

// Config 是系统配置单例的领域模型(行由迁移种子保证存在;后续系统级配置在此加列)。
type Config struct {
	PublicURL     string
	ReleaseMirror string
	UpdatedAt     time.Time
}

// Service 定义系统配置对外接口(httpapi 消费;钩子经 Resolve 读)。
type Service interface {
	// Get 返回配置单例(行由迁移种子保证存在;读失败不构成"未配置"语义)。
	Get(ctx context.Context) (Config, error)
	// SetPublicURL 校验并落库 public_url(空串 = 清除,外链功能关闭)。
	SetPublicURL(ctx context.Context, raw string) (Config, error)
	// SetReleaseMirror 校验并落库 release_mirror(空串 = 清除,升级源回退 GitHub 官方)。
	SetReleaseMirror(ctx context.Context, raw string) (Config, error)
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
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT public_url, release_mirror, updated_at FROM system_config WHERE id = 1`).
		Scan(&c.PublicURL, &c.ReleaseMirror, &updatedStr)
	if err != nil {
		return Config{}, fmt.Errorf("systemcfg: get: %w", err)
	}
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
