// Package registryhub 是「平台内置本地 Docker registry」的领域层。
//
// 控制机本机 docker 上自管一个双服务 registry 栈(registry:2 × 2,compose 声明式):
//   - 制品 registry(默认 :5000):构建产物 push 目标,部署机从这里 pull —— 补齐「环境
//     未绑定 ImageRegistry 时产物只有本地 tag、远程部署 pull 必败」的缺口。
//   - 缓存 registry(默认 :5001):pull-through 代理上游(默认 Docker Hub,可切镜像加速商)。
//     切换上游只改配置并重建该容器,生产机 daemon.json 零改动(pull-through 模式只读,
//     不能收 push,故必须双服务)。
//
// 各机 daemon.json 只需配一次(registry-mirrors/insecure-registries 指向本 registry 地址),
// 由设置页手动勾选机器显式下发(唯一触发方式;保存配置不触碰任何机器),覆盖前备份原文件、
// 自动重启 docker 并验证生效。
//
// 设计原则:
//   - 命令一律 array []string,绝不拼 shell(AC-SEC-02);远端经 target.Service(SSH),
//     本机经注入的 LocalRunner(os/exec)——两者共用同一 Apply 流程。
//   - 未 enabled → 一切优雅关闭:构建保持旧行为(本地 tag 不推送)、不部署栈、不下发 daemon.json。
//   - 无 init 副作用、无包级重对象;底层执行器全部可注入(fake 单测不触网/不碰真 docker)。
package registryhub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
)

// 默认值(未配置时的兜底)。
const (
	// DefaultUpstreamURL 是缓存 registry 的默认上游(Docker Hub 官方 registry API)。
	DefaultUpstreamURL = "https://registry-1.docker.io"
	// DefaultArtifactPort 是制品 registry 默认端口。
	DefaultArtifactPort = 5000
	// DefaultCachePort 是缓存 registry 默认端口。
	DefaultCachePort = 5001
)

// 领域错误(人读语义;httpapi 映射 422/503)。
var (
	// ErrInvalidAddr 表示 external_addr 非法(非 host/IP 形态)或 enabled 时为空。
	ErrInvalidAddr = errors.New("registryhub: invalid external addr")
	// ErrInvalidUpstream 表示上游 URL 非 http(s) 或无主机名。
	ErrInvalidUpstream = errors.New("registryhub: invalid upstream url")
	// ErrInvalidPort 表示端口越界(1-65535)或制品/缓存端口相同。
	ErrInvalidPort = errors.New("registryhub: invalid port")
	// ErrDisabled 表示内置 registry 未启用,操作被拒绝(部署/下发/清理)。
	ErrDisabled = errors.New("registryhub: builtin registry disabled")
	// ErrNoLocalDocker 表示控制机本机无可用容器 CLI / docker compose。
	ErrNoLocalDocker = errors.New("registryhub: no local docker/compose available")
)

// reHostAddr 校验 external_addr:纯 host/IPv4(hostname 字符集),不含 scheme/路径/端口
// (端口是独立配置项)。杜绝把 "http://x/" 之类整 URL 塞进来污染生成的 daemon.json / 镜像 tag。
var reHostAddr = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*)$`)

// Config 是内置 registry 单例配置(领域模型)。
type Config struct {
	Enabled        bool
	ExternalAddr   string // 其他机器可达的控制机地址(host/IP;enabled 时必填)
	UpstreamURL    string // 缓存 registry 上游(registry API 地址)
	ArtifactPort   int
	CachePort      int
	KeepPerProject int // 每仓库保留最近 N tag(0=不限)
	MaxAgeDays     int // 删除早于 N 天的 tag(0=不限)
	UpdatedAt      *time.Time
}

// ArtifactAddr 返回制品 registry 的 host:port(供构建 remoteTag 前缀与 daemon.json)。
func (c *Config) ArtifactAddr() string { return addrWithPort(c.ExternalAddr, c.ArtifactPort) }

// CacheAddr 返回缓存 registry 的 host:port。
func (c *Config) CacheAddr() string { addr := c.ExternalAddr; return addrWithPort(addr, c.CachePort) }

func addrWithPort(host string, port int) string {
	if port <= 0 {
		return host
	}
	if strings.Contains(host, ":") && !strings.Contains(host, "]") { // 裸 IPv6 兜底加方括号
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// Service 定义内置 registry 领域对外接口(config.go/stack.go/retention.go/daemon.go 组合实现)。
type Service interface {
	// Get 返回单例配置(无行 → 惰性默认:关闭 + 默认上游/端口)。
	Get(ctx context.Context) (*Config, error)
	// Save 校验并持久化配置(只写库,**不部署、不下发**;返回更新后配置)。
	Save(ctx context.Context, in SaveInput) (*Config, error)
}

// SaveInput 是保存配置的入参(全量字段;0 值端口表示沿用默认)。
type SaveInput struct {
	Enabled        bool
	ExternalAddr   string
	UpstreamURL    string
	ArtifactPort   int
	CachePort      int
	KeepPerProject int
	MaxAgeDays     int
}

// service 是 store 支撑的 Service 配置实现(栈/清理/下发能力在 Hub 组合,见 hub.go)。
type configService struct {
	db *sql.DB
}

// Get 读取单例配置行;无行返回惰性默认(不写库,与 ai.Config 惰性语义一致)。
func (s *configService) Get(ctx context.Context) (*Config, error) {
	var (
		enabled, artifactPort, cachePort, keep, age    int
		externalAddr, upstream, createdStr, updatedStr string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT enabled, external_addr, upstream_url, artifact_port, cache_port, keep_per_project, max_age_days, created_at, updated_at
		 FROM registry_hub_config WHERE id = 1`,
	).Scan(&enabled, &externalAddr, &upstream, &artifactPort, &cachePort, &keep, &age, &createdStr, &updatedStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &Config{
				Enabled:      false,
				UpstreamURL:  DefaultUpstreamURL,
				ArtifactPort: DefaultArtifactPort,
				CachePort:    DefaultCachePort,
			}, nil
		}
		return nil, fmt.Errorf("registryhub: get config: %w", err)
	}
	cfg := &Config{
		Enabled:        enabled != 0,
		ExternalAddr:   strings.TrimSpace(externalAddr),
		UpstreamURL:    strings.TrimSpace(upstream),
		ArtifactPort:   artifactPort,
		CachePort:      cachePort,
		KeepPerProject: keep,
		MaxAgeDays:     age,
	}
	if cfg.UpstreamURL == "" {
		cfg.UpstreamURL = DefaultUpstreamURL
	}
	if cfg.ArtifactPort <= 0 {
		cfg.ArtifactPort = DefaultArtifactPort
	}
	if cfg.CachePort <= 0 {
		cfg.CachePort = DefaultCachePort
	}
	if updatedStr != "" {
		if ts, perr := time.Parse(time.RFC3339, updatedStr); perr == nil {
			cfg.UpdatedAt = &ts
		}
	}
	return cfg, nil
}

// Save 校验并 upsert 单例配置。**只写库**:不部署栈、不下发 daemon.json、不触碰任何机器。
func (s *configService) Save(ctx context.Context, in SaveInput) (*Config, error) {
	cfg, err := NormalizeConfig(in)
	if err != nil {
		return nil, err
	}
	nowStr := time.Now().UTC().Format(time.RFC3339)
	enabled := 0
	if cfg.Enabled {
		enabled = 1
	}
	// 单例 upsert:首存 INSERT(id=1),已存 ON CONFLICT 更新(created_at 保留)。
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO registry_hub_config
		   (id, enabled, external_addr, upstream_url, artifact_port, cache_port, keep_per_project, max_age_days, created_at, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?) `+
			store.UpsertSuffix(store.DialectOf(s.db), []string{"id"},
				[]string{"enabled", "external_addr", "upstream_url", "artifact_port", "cache_port", "keep_per_project", "max_age_days", "updated_at"}),
		enabled, cfg.ExternalAddr, cfg.UpstreamURL, cfg.ArtifactPort, cfg.CachePort, cfg.KeepPerProject, cfg.MaxAgeDays, nowStr, nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("registryhub: upsert config: %w", err)
	}
	return s.Get(ctx)
}

// NormalizeConfig 校验并归一 SaveInput:去空白、端口 0 值兜底默认、上游空兜底默认。
// external_addr 允许为空(=未启用态);enabled 且为空 → ErrInvalidAddr。
func NormalizeConfig(in SaveInput) (*Config, error) {
	cfg := &Config{
		Enabled:        in.Enabled,
		ExternalAddr:   strings.TrimSpace(in.ExternalAddr),
		UpstreamURL:    strings.TrimSpace(in.UpstreamURL),
		ArtifactPort:   in.ArtifactPort,
		CachePort:      in.CachePort,
		KeepPerProject: in.KeepPerProject,
		MaxAgeDays:     in.MaxAgeDays,
	}
	if cfg.UpstreamURL == "" {
		cfg.UpstreamURL = DefaultUpstreamURL
	}
	if cfg.ArtifactPort == 0 {
		cfg.ArtifactPort = DefaultArtifactPort
	}
	if cfg.CachePort == 0 {
		cfg.CachePort = DefaultCachePort
	}
	if cfg.KeepPerProject < 0 {
		cfg.KeepPerProject = 0
	}
	if cfg.MaxAgeDays < 0 {
		cfg.MaxAgeDays = 0
	}

	if cfg.ExternalAddr != "" {
		// host/IP 形态:禁 scheme/路径/端口(端口独立配置);长度上限防滥用。
		if len(cfg.ExternalAddr) > 255 || !reHostAddr.MatchString(cfg.ExternalAddr) {
			return nil, ErrInvalidAddr
		}
	}
	if cfg.Enabled && cfg.ExternalAddr == "" {
		return nil, ErrInvalidAddr
	}
	if u, err := url.Parse(cfg.UpstreamURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, ErrInvalidUpstream
	}
	if cfg.ArtifactPort < 1 || cfg.ArtifactPort > 65535 || cfg.CachePort < 1 || cfg.CachePort > 65535 {
		return nil, ErrInvalidPort
	}
	if cfg.ArtifactPort == cfg.CachePort {
		return nil, ErrInvalidPort
	}
	return cfg, nil
}

// DetectOutboundAddr 返回控制机的出网网卡 IP(供 UI 预填 external_addr 的建议值;
// UDP dial 不发包,仅取本地路由选出的源地址)。探测失败返回空串(UI 自行留空)。
func DetectOutboundAddr() string {
	conn, err := net.DialTimeout("udp", "8.8.8.8:53", 2*time.Second)
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return addr.IP.String()
}
