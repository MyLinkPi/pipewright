// Package aptcacherng 是「平台内置本地 apt 包缓存(apt-cacher-ng)」的领域层。
//
// 控制机本机 docker 上自管一个 apt-cacher-ng 栈(compose 声明式,默认 :3142):
//   - 明文态:单服务 apt-cacher-ng 直接对宿主发布端口。
//   - TLS 态:apt-cacher-ng 只在栈内网络可达,前置 nginx stream 做 TLS 终止
//     (apt-cacher-ng 本体无 TLS;HTTPS 代理形态由前端实现)。
//
// 各 Debian 系机器的 apt 代理配置只需配一次(95pipewright-proxy 片段指向本缓存地址),
// 由设置页手动勾选机器显式下发(唯一触发方式;保存配置不触碰任何机器),覆盖前备份原
// 文件、原子替换并用 apt-config dump 验证生效(apt 无需重启任何服务)。
//
// TLS 可选:配置引用「证书管理」的公共可信证书(certID 软引用,PEM 仅进程内解密落盘挂载)。
// apt 客户端**严格校验**代理 TLS 证书,证书 SAN 必须覆盖 external_addr;各机须信任该证书
// (ACME 公共签发即满足,零配置直连)。
//
// 设计原则(与 registryhub 同构):
//   - 命令一律 array []string,绝不拼 shell(AC-SEC-02);远端经 target.Service(SSH),
//     本机经注入的 LocalRunner(os/exec)——两者共用同一 Apply 流程。
//   - 未 enabled → 一切优雅关闭:不部署栈、不下发代理配置。
//   - 无 init 副作用、无包级重对象;底层执行器全部可注入(fake 单测不触网/不碰真 docker)。
package aptcacherng

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
)

// 默认值(未配置时的兜底)。
const (
	// DefaultPort 是 apt-cacher-ng 代理默认端口。
	DefaultPort = 3142
)

// 领域错误(人读语义;httpapi 映射 422/503)。
var (
	// ErrInvalidAddr 表示 external_addr 非法(非 host/IP 形态)或 enabled 时为空。
	ErrInvalidAddr = errors.New("aptcacherng: invalid external addr")
	// ErrInvalidPort 表示端口越界(1-65535)。
	ErrInvalidPort = errors.New("aptcacherng: invalid port")
	// ErrDisabled 表示内置 apt 缓存未启用,操作被拒绝(部署/下发/清理)。
	ErrDisabled = errors.New("aptcacherng: builtin apt cache disabled")
	// ErrNoLocalDocker 表示控制机本机无可用容器 CLI / docker compose。
	ErrNoLocalDocker = errors.New("aptcacherng: no local docker/compose available")
	// ErrInvalidDataDir 表示缓存目录非法:非绝对路径/根目录/含控制字符,或罩住栈基目录
	// (清除缓存目录会动到 compose 文件自身)。
	ErrInvalidDataDir = errors.New("aptcacherng: invalid data dir")
	// ErrInvalidTLSCert 表示 tls_cert_id 非法,或所指证书不存在/域名未覆盖外部地址。
	ErrInvalidTLSCert = errors.New("aptcacherng: invalid tls cert")
)

// reHostAddr 校验 external_addr:纯 host/IPv4(hostname 字符集),不含 scheme/路径/端口
// (端口是独立配置项)。杜绝把 "http://x/" 之类整 URL 塞进来污染生成的代理片段。
var reHostAddr = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*)$`)

// Config 是内置 apt 缓存单例配置(领域模型)。
type Config struct {
	Enabled      bool
	ExternalAddr string // 其他机器可达的控制机地址(host/IP;enabled 时必填)
	Port         int
	DataDir      string // 缓存存储目录(空 = <BaseDir>/cache 默认)
	TLSCertID    string // 「证书管理」证书 ID(空 = 明文 HTTP;非空 = nginx 前端 TLS 终止走 HTTPS)
	UpdatedAt    *time.Time
}

// Addr 返回 apt 缓存的 host:port(供代理片段与展示)。
func (c *Config) Addr() string { return addrWithPort(c.ExternalAddr, c.Port) }

// TLSEnabled 报告栈是否以 TLS 服务:选了证书即开启(nginx stream 前端终止)。
func (c *Config) TLSEnabled() bool { return strings.TrimSpace(c.TLSCertID) != "" }

func addrWithPort(host string, port int) string {
	if port <= 0 {
		return host
	}
	if strings.Contains(host, ":") && !strings.Contains(host, "]") { // 裸 IPv6 兜底加方括号
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// Service 定义内置 apt 缓存领域配置接口(config.go 实现;栈/清理/下发在 Hub 组合)。
type Service interface {
	// Get 返回单例配置(无行 → 惰性默认:关闭 + 默认端口)。
	Get(ctx context.Context) (*Config, error)
	// Save 校验并持久化配置(只写库,**不部署、不下发**;返回更新后配置)。
	Save(ctx context.Context, in SaveInput) (*Config, error)
}

// SaveInput 是保存配置的入参(全量字段;0 值端口表示沿用默认,空目录表示沿用默认路径)。
type SaveInput struct {
	Enabled      bool
	ExternalAddr string
	Port         int
	DataDir      string
	TLSCertID    string
}

// configService 是 store 支撑的 Service 配置实现(栈/清理/下发能力在 Hub 组合,见 hub.go)。
type configService struct {
	db      *sql.DB
	baseDir string     // 栈基目录(目录冲突校验需要;空跳过该项校验)
	certs   CertSource // TLS 证书来源(nil 时选了证书的保存会被拒绝,诚实不假装校验过)
}

// Get 读取单例配置行;无行返回惰性默认(不写库,与 registryhub 惰性语义一致)。
func (s *configService) Get(ctx context.Context) (*Config, error) {
	var (
		enabled, port int
		externalAddr  string
		dataDir       string
		tlsCertID     string
		updatedStr    string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT enabled, external_addr, port, data_dir, tls_cert_id, updated_at
		 FROM apt_cacher_config WHERE id = 1`,
	).Scan(&enabled, &externalAddr, &port, &dataDir, &tlsCertID, &updatedStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &Config{Enabled: false, Port: DefaultPort}, nil
		}
		return nil, fmt.Errorf("aptcacherng: get config: %w", err)
	}
	cfg := &Config{
		Enabled:      enabled != 0,
		ExternalAddr: strings.TrimSpace(externalAddr),
		Port:         port,
		DataDir:      strings.TrimSpace(dataDir),
		TLSCertID:    strings.TrimSpace(tlsCertID),
	}
	if cfg.Port <= 0 {
		cfg.Port = DefaultPort
	}
	if updatedStr != "" {
		if ts, perr := time.Parse(time.RFC3339, updatedStr); perr == nil {
			cfg.UpdatedAt = &ts
		}
	}
	return cfg, nil
}

// Save 校验并 upsert 单例配置。**只写库**:不部署栈、不下发代理片段、不触碰任何机器。
// 选了 TLS 证书时即时校验证书存在且 SAN 覆盖外部地址(失败拒绝落库,不在部署时才炸)。
func (s *configService) Save(ctx context.Context, in SaveInput) (*Config, error) {
	cfg, err := NormalizeConfig(in, s.baseDir)
	if err != nil {
		return nil, err
	}
	if cfg.TLSCertID != "" {
		if verr := s.validateTLSCert(ctx, cfg); verr != nil {
			return nil, verr
		}
	}
	nowStr := time.Now().UTC().Format(time.RFC3339)
	enabled := 0
	if cfg.Enabled {
		enabled = 1
	}
	// 单例 upsert:首存 INSERT(id=1),已存 ON CONFLICT 更新(created_at 保留)。
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO apt_cacher_config
		   (id, enabled, external_addr, port, data_dir, tls_cert_id, created_at, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?) `+
			store.UpsertSuffix(store.DialectOf(s.db), []string{"id"},
				[]string{"enabled", "external_addr", "port", "data_dir", "tls_cert_id", "updated_at"}),
		enabled, cfg.ExternalAddr, cfg.Port, cfg.DataDir, cfg.TLSCertID, nowStr, nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("aptcacherng: upsert config: %w", err)
	}
	return s.Get(ctx)
}

// validateTLSCert 校验 TLS 证书引用:证书须存在且 SAN 覆盖 host(公共可信由证书管理
// 侧保证;本包只把「引用合法」把在保存关口)。失败一律包 ErrInvalidTLSCert(httpapi → 422)。
func (s *configService) validateTLSCert(ctx context.Context, cfg *Config) error {
	if s.certs == nil {
		return fmt.Errorf("%w: 证书能力未装配", ErrInvalidTLSCert)
	}
	c, err := s.certs.GetCert(ctx, cfg.TLSCertID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTLSCert, err)
	}
	if c == nil || !domainCovers(c.Domains, cfg.ExternalAddr) {
		return fmt.Errorf("%w: 证书不存在或域名未覆盖外部地址 %s", ErrInvalidTLSCert, cfg.ExternalAddr)
	}
	return nil
}

// NormalizeConfig 校验并归一 SaveInput:去空白、端口 0 值兜底默认。
// external_addr 允许为空(=未启用态);enabled 且为空 → ErrInvalidAddr。
// baseDir 为栈基目录(用于目录冲突校验;测试或未知时可为空)。
// 缓存目录:空 = 沿用默认;非空须为控制机绝对路径,且不得罩住栈基目录。
func NormalizeConfig(in SaveInput, baseDir string) (*Config, error) {
	cfg := &Config{
		Enabled:      in.Enabled,
		ExternalAddr: strings.TrimSpace(in.ExternalAddr),
		Port:         in.Port,
		DataDir:      strings.TrimSpace(in.DataDir),
		TLSCertID:    strings.TrimSpace(in.TLSCertID),
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, ErrInvalidPort
	}
	if len(cfg.TLSCertID) > 128 {
		return nil, ErrInvalidTLSCert
	}
	for _, r := range cfg.TLSCertID {
		if r <= ' ' || r == 0x7f { // 证书 ID 是无空白标识形态;空白/控制字符一律拒绝(防注入面)
			return nil, ErrInvalidTLSCert
		}
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
	dataDir, err := normalizeDataDir(cfg.DataDir, defaultCacheDataDir(baseDir))
	if err != nil {
		return nil, err
	}
	cfg.DataDir = dataDir
	// 目录不得罩住栈基目录(compose/nginx conf 所在),否则清缓存会把栈自身卷进去。
	if baseDir != "" && dirContains(dataDir, baseDir) {
		return nil, ErrInvalidDataDir
	}
	return cfg, nil
}

// normalizeDataDir 校验缓存目录:空 → 默认值;非空须为绝对路径、无控制字符、非根目录
// (目录会被整目录清除,相对路径与根目录都极易酿成事故)。返回 Clean 后的路径。
func normalizeDataDir(raw, def string) (string, error) {
	if raw == "" {
		return def, nil
	}
	if len(raw) > 512 {
		return "", ErrInvalidDataDir
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f { // 控制字符会污染 compose 文件(注入面)
			return "", ErrInvalidDataDir
		}
	}
	if !filepath.IsAbs(raw) {
		return "", ErrInvalidDataDir
	}
	cleaned := filepath.Clean(raw)
	if cleaned == filepath.VolumeName(cleaned)+string(filepath.Separator) || cleaned == string(filepath.Separator) {
		return "", ErrInvalidDataDir // 根目录:清除逻辑会把整个盘抹掉
	}
	return cleaned, nil
}

// dirContains 报告 child 是否等于或位于 parent 之内(按路径组件比较,纯字符串前缀会
// 误判 /a/b2 ⊂ /a/b,故用 Rel)。
func dirContains(parent, child string) bool {
	if parent == "" || child == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// domainCovers 报告证书 SAN 列表是否覆盖 host:精确相等,或通配符 "*.suffix" 覆盖 host
// 的最左一个标签(遵循 TLS 通配符语义:*.a.com 不覆盖 x.y.a.com,也不覆盖裸 a.com)。
func domainCovers(domains []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == host {
			return true
		}
		if strings.HasPrefix(d, "*.") {
			suffix := d[1:] // ".a.com"(带点)
			if strings.HasSuffix(host, suffix) && !strings.Contains(strings.TrimSuffix(host, suffix), ".") {
				return true
			}
		}
	}
	return false
}

// DetectOutboundAddr 返回控制机的出网网卡 IP(供 UI 预填 external_addr 的建议值;实现
// 在 stack.go)。UDP dial 不发包,仅取本地路由选出的源地址;探测失败返回空串(UI 自行留空)。
func DetectOutboundAddr() string { return detectOutboundAddr() }
