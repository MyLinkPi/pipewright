// Package systemcfg 是「系统级运行时配置」的领域层:平台自身对外行为的可在线修改配置,
// 取代需要重启的环境变量。本期仅 public_url(平台对外访问地址):
//   - 通知里的签名审批链接(internal/httpapi notify_hook)以它为前缀;
//   - PR 状态回写的 target_url(internal/httpapi prstatus)以它为前缀;
//   - 空 = 未配置,相关外链优雅关闭(不发审批链接、回写不带 target_url)。
//
// 消费方经 Resolve 在每次触发时读取(单行主键查询,频率低),运行时修改即时生效、无需重启。
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

// maxURLLen 是 public_url 的长度上限。
const maxURLLen = 512

// Config 是系统配置单例的领域模型(目前仅 public_url;后续系统级配置在此加列)。
type Config struct {
	PublicURL string
	UpdatedAt time.Time
}

// Service 定义系统配置对外接口(httpapi 消费;钩子经 Resolve 读)。
type Service interface {
	// Get 返回配置单例(行由迁移种子保证存在;读失败不构成"未配置"语义)。
	Get(ctx context.Context) (Config, error)
	// SetPublicURL 校验并落库 public_url(空串 = 清除,外链功能关闭)。
	SetPublicURL(ctx context.Context, raw string) (Config, error)
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

type service struct{ db *sql.DB }

// New 构造 Service(单行表由迁移种子,读路径无需兜底插入)。
func New(db *sql.DB) Service { return &service{db: db} }

func (s *service) Get(ctx context.Context) (Config, error) {
	var (
		c          Config
		updatedStr string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT public_url, updated_at FROM system_config WHERE id = 1`).
		Scan(&c.PublicURL, &updatedStr)
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
