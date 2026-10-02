// Package platformhttps 是「平台 HTTPS 访问」的领域层:当平台自身所在主机装有宿主 nginx 时,
// 自动把平台自己的 Web 页面发布为 HTTPS —— 下发证书 → 写 /etc/nginx/conf.d/pipewright-platform.conf
// (443 ssl 反代平台 Web 端口 + 80→443 跳转)→ nginx -t → reload。全部命令在本机执行
// (Host 抽象,生产基于 os/exec;不走 SSH、不选服务器)。
//
// 与既有体系的关系:
//   - 与 internal/servicereg(平台自管的 nginx **容器**网关)互不影响:本功能面向宿主机自装
//     nginx,只写自己的两个路径(conf.d 单文件 + /etc/nginx/pipewright/certs/<域名>/),
//     与用户已有配置共存,绝不接管整个 nginx.conf。
//   - 证书来自 internal/certmgmt(经 CertSource 软引用,本表不存 PEM):续期/重下发后由
//     certmgmt 经 RedeployCert 联动重新下发并 reload;删除证书被 UsesCert 拦截。
//
// 设计纪律(与 servicereg/certmgmt 一致):
//   - 一切本机命令经 Host.Run 以 array 形式执行(AC-SEC-02 不拼 shell);提权方式:root
//     直接执行 > 固定前缀 sudo -n(免密),皆不可用报人话指引(不涉密码,绝不挂起等 TTY)。
//   - 证书 PEM 仅在进程内解密传递(vault/certmgmt → 本机落盘),绝不日志/回库/回 API。
//   - 先测后换:nginx -t 失败不动活配置(自动回滚我们写入的 conf.d 文件);reload 失败
//     给人话指引,不自动 start nginx。
package platformhttps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 状态枚举(DB 存小写字串;” = 从未应用)。
const (
	// StatusActive 表示最近一次应用成功(远端配置在位且已 reload)。
	StatusActive = "active"
	// StatusFailed 表示最近一次应用失败(status_detail 有人话原因)。
	StatusFailed = "failed"
)

// 领域错误(httpapi 层映射状态码;错误体绝无敏感信息)。
var (
	// ErrNotConfigured 表示设置不完整(启用需要:域名 + 证书)。
	ErrNotConfigured = errors.New("platformhttps: 设置不完整")
	// ErrInvalidDomain 表示访问域名格式非法(单一 FQDN,不支持通配符)。
	ErrInvalidDomain = errors.New("platformhttps: invalid domain")
	// ErrInvalidUpstream 表示反代上游 host/port 非法。
	ErrInvalidUpstream = errors.New("platformhttps: invalid upstream host or port")
	// ErrCertSourceMissing 表示证书模块未接入(无法校验/取证书)。
	ErrCertSourceMissing = errors.New("platformhttps: 证书模块未接入")
	// ErrCertNotReady 表示引用的证书不存在或尚未签发完成(无可用 PEM)。
	ErrCertNotReady = errors.New("platformhttps: 证书不存在或未签发完成")
	// ErrCertNotCover 表示证书 SAN 不覆盖所配置的访问域名。
	ErrCertNotCover = errors.New("platformhttps: 证书不覆盖该域名")
	// ErrNoNginx 表示本机未安装 nginx(或不在 PATH)。
	ErrNoNginx = errors.New("platformhttps: 本机未安装 nginx")
	// ErrNoPrivilege 表示平台运行用户非 root 且无免密 sudo(无法写 /etc/nginx)。
	ErrNoPrivilege = errors.New("platformhttps: 无 root 权限且免密 sudo 不可用")
	// ErrApply 表示下发/校验/热加载失败(附人话摘要)。
	ErrApply = errors.New("platformhttps: 应用 HTTPS 配置失败")
	// ErrAppliedChange 表示配置已应用(status 非空)时试图变更域名:直接改会让旧域名证书
	// 目录(含私钥)成孤儿(Disable 只按当前值清理),须先「禁用并清理」。
	ErrAppliedChange = errors.New("platformhttps: 配置已应用")
)

// domainRe 校验访问域名(与 certmgmt 同规则:单一 FQDN,每段字母数字/连字符,顶级域 ≥2 字母)。
var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// Settings 是平台 HTTPS 的单行设置(证书经 cert_id 软引用,本表无 PEM)。
type Settings struct {
	Enabled       bool
	Domain        string    // 平台访问域名(如 pip.efg.com)
	CertID        string    // certificates.id 引用
	UpstreamHost  string    // 反代上游 host(默认 127.0.0.1)
	UpstreamPort  int       // 反代上游端口(0 = 平台自身 Web 端口)
	HTTPRedirect  bool      // 80 → 443 跳转(默认开)
	Status        string    // '' | active | failed(最近一次应用结果)
	StatusDetail  string    // 人话:失败原因
	LastAppliedAt time.Time // 最近一次应用时间
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// SettingsInput 是更新设置的入参(指针字段,nil = 保持不变;空串 = 清空对应引用)。
type SettingsInput struct {
	Enabled      *bool
	Domain       *string
	CertID       *string
	UpstreamHost *string
	UpstreamPort *int
	HTTPRedirect *bool
}

// CertInfo 是证书管理侧的证书摘要(覆盖校验用;无 PEM)。
type CertInfo struct {
	ID            string
	PrimaryDomain string
	Domains       []string
	Status        string
	NotAfter      time.Time
}

// CertSource 抽象「证书元数据 + 进程内解密 PEM」的能力(由 main.go 适配 certmgmt.Service 注入,
// 避免包环)。PEM 仅进程内传递,绝不过 HTTP。
type CertSource interface {
	// GetCert 返回证书摘要;不存在 → ErrNotFound 语义(certmgmt.ErrNotFound)。
	GetCert(ctx context.Context, id string) (*CertInfo, error)
	// OpenCertPEM 解密返回证书/私钥 PEM 明文(仅进程内使用)。
	OpenCertPEM(ctx context.Context, id string) (certPEM, keyPEM string, err error)
}

// Host 抽象「平台自身所在主机」的命令执行与文件写入(生产实现基于 os/exec + os.WriteFile;
// 测试注入 fake,不碰真机)。命令以 array 传入,绝不经 shell 解释(AC-SEC-02)。
type Host interface {
	// Run 同步执行 name+args,返回 stdout/stderr/退出码;err 非 nil 表示无法启动
	// (命令不存在等,此时 exitCode<0);跑完但非零退出不算 err(退出码在 exitCode)。
	Run(ctx context.Context, name string, args []string) (stdout, stderr string, exitCode int, err error)
	// WriteFile 写本机文件(父目录由调用方确保存在;perm 如 0o600)。
	WriteFile(path string, data []byte, perm os.FileMode) error
}

// localHost 是基于 os/exec + os.WriteFile 的默认 Host 实现(子进程经 CommandContext 绑 ctx,
// ctx 取消即 kill;命令绝不经 shell 解释)。
type localHost struct{}

func (localHost) Run(ctx context.Context, name string, args []string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return outBuf.String(), errBuf.String(), ee.ExitCode(), nil // 跑完非零:退出码说话
		}
		return outBuf.String(), errBuf.String(), -1, err // 启动失败(命令不存在等)
	}
	return outBuf.String(), errBuf.String(), 0, nil
}

func (localHost) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

// NginxDetect 是本机宿主 nginx 的探测快照(配置前知情)。
type NginxDetect struct {
	Installed     bool   // nginx -v 可执行
	Version       string // 如 "1.24.0"
	IsRoot        bool   // 平台运行用户是否 root
	SudoOk        bool   // root 恒 true;非 root 为 sudo -n 是否可用
	ConfDIncluded bool   // nginx -T 是否 include conf.d(best-effort,防静默失效)
	ManagedConf   bool   // /etc/nginx/conf.d/pipewright-platform.conf 是否已在(平台曾应用过)
}

// Service 定义平台 HTTPS 访问领域对外接口(httpapi 消费;UsesCert/RedeployCert 供 certmgmt 联动)。
type Service interface {
	// GetSettings 返回单行设置(库为空时返回默认行,无副作用)。
	GetSettings(ctx context.Context) (*Settings, error)
	// EffectiveUpstream 返回设置当前生效的反代目标(如 "127.0.0.1:8080";空值回退默认,供展示)。
	EffectiveUpstream(st *Settings) string
	// SaveSettings 校验并更新设置;纯落库不触网。启用(enabled=true)要求配置完整且证书覆盖域名。
	SaveSettings(ctx context.Context, in SettingsInput) (*Settings, error)
	// Detect 探测本机宿主 nginx / 提权能力 / conf.d include 情况。
	Detect(ctx context.Context) (*NginxDetect, error)
	// Apply 全量收敛:下发证书 + 写 conf.d vhost + nginx -t(失败回滚)+ reload + include 验证;
	// 结果(含失败人话)记回设置行。未启用/配置不完整 → ErrNotConfigured。
	Apply(ctx context.Context) (*Settings, error)
	// Disable 移除本机 conf.d 文件与证书目录并 reload(本机无平台配置时仅复位本地行);
	// 本地行保留(enabled=0)便于再次启用。
	Disable(ctx context.Context) (*Settings, error)

	// UsesCert 报告平台 HTTPS 是否正在使用该证书(certmgmt 删除证书时拦截)。
	UsesCert(ctx context.Context, certID string) (bool, error)
	// RedeployCert 证书续期/重下发后联动:引用该证书且远端有配置(status 非空)时重新
	// Apply(否则 no-op;status 为空 = 未应用/已清理,下次应用自会取新 PEM)。
	RedeployCert(ctx context.Context, certID string) error
}

// service 是 store + Host (+ CertSource) 支撑的 Service 实现。
type service struct {
	store       *Store
	host        Host
	certs       CertSource
	defaultPort int // UpstreamPort 为 0 时的默认值(平台自身 Web 端口)

	// mu 互斥 Apply/Disable:HTTP 手动应用与 certmgmt 续期联动(RedeployCert,异步
	// goroutine)可能并发,共用固定 /tmp 路径与备份的命令序列交错会互相破坏。
	mu sync.Mutex
}

// New 构造 Service。host 为 nil 时用本机默认实现(os/exec + os.WriteFile);certs 为 nil 时
// 证书校验/下发不可用(其余照常);defaultPort 为平台自身 Web 监听端口(main.go 由 cfg.Addr
// 推导),作为反代上游端口的默认值。
func New(db *sql.DB, host Host, certs CertSource, defaultPort int) Service {
	if host == nil {
		host = localHost{}
	}
	if defaultPort < 1 || defaultPort > 65535 {
		defaultPort = 8080
	}
	return &service{store: NewStore(db), host: host, certs: certs, defaultPort: defaultPort}
}

// ---------- 设置 ----------

func (s *service) GetSettings(ctx context.Context) (*Settings, error) {
	return s.store.getOrCreate(ctx)
}

// EffectiveUpstream 返回生效的反代目标 "host:port"(空 host/0 端口回退默认;供 DTO 展示)。
func (s *service) EffectiveUpstream(st *Settings) string {
	host, port := s.effectiveUpstream(*st)
	return host + ":" + strconv.Itoa(port)
}

func (s *service) SaveSettings(ctx context.Context, in SettingsInput) (*Settings, error) {
	cur, err := s.store.getOrCreate(ctx)
	if err != nil {
		return nil, err
	}
	next := *cur
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	if in.Domain != nil {
		next.Domain = strings.ToLower(strings.TrimSpace(*in.Domain))
	}
	if in.CertID != nil {
		next.CertID = strings.TrimSpace(*in.CertID)
	}
	if in.UpstreamHost != nil {
		next.UpstreamHost = strings.ToLower(strings.TrimSpace(*in.UpstreamHost))
	}
	if in.UpstreamPort != nil {
		next.UpstreamPort = *in.UpstreamPort
	}
	if in.HTTPRedirect != nil {
		next.HTTPRedirect = *in.HTTPRedirect
	}
	// 已应用(status 非空 = 本机可能有配置)时变更域名 → 拒绝:Disable 只按当前值清理,
	// 直接改会让旧域名证书目录(含私钥)成孤儿,且无任何清理入口。
	if cur.Status != "" && next.Domain != cur.Domain {
		return nil, fmt.Errorf("%w:配置已应用,变更域名前请先「禁用并清理」", ErrAppliedChange)
	}
	if err := s.validate(ctx, next); err != nil {
		return nil, err
	}
	if err := s.store.save(ctx, &next); err != nil {
		return nil, err
	}
	// 换证书允许(同机同路径,重新应用即收敛),但旧 status 描述的是旧配置 → 重置为待应用,
	// 引导重新应用;UsesCert 按新 certId + 空 status 判定,删除拦截随收敛恢复。
	if cur.Status != "" && next.CertID != cur.CertID {
		if err := s.store.clearStatus(ctx); err != nil {
			return nil, err
		}
	}
	return s.store.getOrCreate(ctx)
}

// validate 全量校验落库前状态:未启用只查宽松项(host/port 形态);启用要求域名合法、
// 证书已签发且 SAN 覆盖域名。
func (s *service) validate(ctx context.Context, st Settings) error {
	if st.Domain != "" && !domainRe.MatchString(st.Domain) {
		return ErrInvalidDomain
	}
	host := st.UpstreamHost
	if host == "" {
		host = "127.0.0.1"
	}
	if !validUpstreamHost(host) {
		return ErrInvalidUpstream
	}
	if st.UpstreamPort < 0 || st.UpstreamPort > 65535 {
		return ErrInvalidUpstream
	}
	if !st.Enabled {
		return nil
	}
	if st.Domain == "" || st.CertID == "" {
		return ErrNotConfigured
	}
	if s.certs == nil {
		return ErrCertSourceMissing
	}
	c, err := s.certs.GetCert(ctx, st.CertID)
	if err != nil {
		return fmt.Errorf("%w:%s", ErrCertNotReady, st.CertID)
	}
	if c.Status != "issued" {
		return fmt.Errorf("%w:证书尚未签发完成(%s)", ErrCertNotReady, c.Status)
	}
	if !domainCoveredBy(st.Domain, c.Domains) {
		return fmt.Errorf("%w:%s", ErrCertNotCover, st.Domain)
	}
	return nil
}

// domainCoveredBy 报告域名是否被证书 SAN 集覆盖:精确等于某 SAN,或被某通配符 SAN 覆盖
// (子域须带 "." 分隔,防 evilefg.com 误匹配 *.efg.com;基域自身须显式列入 SAN,宁缺毋滥)。
func domainCoveredBy(domain string, sans []string) bool {
	for _, san := range sans {
		if san == domain {
			return true
		}
		if strings.HasPrefix(san, "*.") {
			if base := strings.TrimPrefix(san, "*."); strings.HasSuffix(domain, "."+base) {
				return true
			}
		}
	}
	return false
}

// validUpstreamHost 校验反代上游 host:IP / FQDN / localhost。
func validUpstreamHost(h string) bool {
	if h == "localhost" || h == "127.0.0.1" {
		return true
	}
	if net.ParseIP(h) != nil {
		return true
	}
	return len(h) <= 253 && domainRe.MatchString(h)
}

// effectiveUpstream 返回生效的上游 host:port(空 host 回退 127.0.0.1,0 端口回退平台默认端口)。
func (s *service) effectiveUpstream(st Settings) (string, int) {
	host := st.UpstreamHost
	if host == "" {
		host = "127.0.0.1"
	}
	port := st.UpstreamPort
	if port <= 0 {
		port = s.defaultPort
	}
	return host, port
}

// ---------- 探测 ----------

func (s *service) Detect(ctx context.Context) (*NginxDetect, error) {
	return detectNginx(ctx, s.host)
}

// ---------- 应用 / 禁用 ----------

func (s *service) Apply(ctx context.Context) (*Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.store.getOrCreate(ctx)
	if err != nil {
		return nil, err
	}
	if !st.Enabled || st.Domain == "" || st.CertID == "" {
		return nil, ErrNotConfigured
	}
	if err := s.validate(ctx, *st); err != nil {
		return nil, err
	}
	if s.certs == nil {
		return nil, ErrCertSourceMissing
	}
	certPEM, keyPEM, err := s.certs.OpenCertPEM(ctx, st.CertID)
	if err != nil {
		return nil, fmt.Errorf("%w:证书密文不可用", ErrCertNotReady)
	}
	host, port := s.effectiveUpstream(*st)
	if err := applyPlatformConf(ctx, s.host, st.Domain, host, port, st.HTTPRedirect, certPEM, keyPEM); err != nil {
		_ = s.store.setResult(ctx, StatusFailed, truncate(err.Error(), 2000))
		s2, gerr := s.store.getOrCreate(ctx)
		if gerr == nil {
			return s2, err
		}
		return nil, err
	}
	_ = s.store.setResult(ctx, StatusActive, "")
	return s.store.getOrCreate(ctx)
}

func (s *service) Disable(ctx context.Context) (*Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.store.getOrCreate(ctx)
	if err != nil {
		return nil, err
	}
	// 本机从未应用过(状态为空)→ 仅复位本地行。
	if st.Status != "" {
		if err := removePlatformConf(ctx, s.host, st.Domain); err != nil {
			return nil, err
		}
	}
	if err := s.store.setDisabled(ctx); err != nil {
		return nil, err
	}
	return s.store.getOrCreate(ctx)
}

// ---------- certmgmt 联动 ----------

func (s *service) UsesCert(ctx context.Context, certID string) (bool, error) {
	st, err := s.store.getOrCreate(ctx)
	if err != nil {
		return false, err
	}
	// 以 status 非空(本机可能有配置)为准,不看 enabled:用户仅关掉启用开关而未「禁用并
	// 清理」时本机仍在跑该证书,删除拦截必须继续生效。
	return st.CertID == certID && st.Status != "", nil
}

func (s *service) RedeployCert(ctx context.Context, certID string) error {
	st, err := s.store.getOrCreate(ctx)
	if err != nil {
		return err
	}
	if st.CertID != certID || st.Status == "" {
		return nil // 未引用或本机无配置:无事可做
	}
	_, aerr := s.Apply(ctx)
	return aerr
}

// truncate 截断长文本(入库/回显限制)。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
