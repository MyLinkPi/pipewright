// Package servicereg 是「服务注册网关」的领域层:在一台用户选定的网关主机上,经 SSH 编排一个
// 独立部署的 nginx 容器,把「服务名 + 基域」映射为子域名反向代理(abc + efg.com → abc.efg.com)。
//
// 定位(Caddy 反代下线后的唯一网关):面向「泛域名解析到网关主机 + 证书托管」的场景 ——
// *.efg.com 由用户解析到网关主机 IP,平台不接管网关域 DNS。证书的签发/续期/导入统一由
// internal/certmgmt(acme.sh)负责,经 CertSink(UploadCert)同步到基域;本包只管证书密文的
// 存取与 nginx 渲染下发。证书 PEM 经 vault SealSecret 加密入库,绝不明文回 API。
//
// 设计纪律(与 proxy 包一致):
//   - 一切 docker/nginx 命令经 target.Exec 以 array 形式执行(AC-SEC-02 不拼 shell);唯一例外是
//     容器启动命令内一段**固定常量**的 sh -c 自举脚本(无任何用户输入,不是注入面)。
//   - 声明式全量收敛:任何变更后对该主机全量重渲染 nginx.conf → 先 nginx -t 校验再热加载,
//     校验失败不动活配置;新增 TCP 监听端口时容器带「既有映射 ∪ 新端口」重建(Docker 端口
//     创建时固定)。
//   - 上游动态解析:nginx 的 proxy_pass 带变量时按 resolver(127.0.0.11,Docker 内嵌 DNS)运行期
//     解析,容器重建换 IP 自动跟随 —— 与 Caddy 的按容器名路由等价。
//   - 证书 PEM 绝不明文入库/回 API/写日志:SealSecret 密文存 BLOB,仅在 apply 时于进程内解密下发。
//   - 网关未指定主机(server_id 为空)时一切 CRUD 照常(配置态),编排静默跳过,DeployGateway 时收敛。
package servicereg

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 协议 / 上游类型枚举(DB 存小写字串)。
const (
	// ProtocolHTTP 表示 HTTP(S) 反代:按子域名 server 块路由。
	ProtocolHTTP = "http"
	// ProtocolTCP 表示 TCP 反代:网关监听独立端口转发到上游(stream 块)。
	ProtocolTCP = "tcp"

	// UpstreamKindContainer 表示上游为网关同机上的 docker 容器(Upstream=容器名):
	// 容器被接入共享网络,nginx 按 Docker 内嵌 DNS 以容器名解析。
	UpstreamKindContainer = "container"
	// UpstreamKindAddress 表示上游为任意地址(Upstream=HOST:IP/FQDN/host.docker.internal)。
	UpstreamKindAddress = "address"
)

// 领域错误(httpapi 层映射状态码;错误体绝无敏感信息)。
var (
	// ErrNotFound 表示域名或服务不存在。
	ErrNotFound = errors.New("servicereg: not found")
	// ErrDomainTaken 表示基域已被注册(base_domain 唯一)。
	ErrDomainTaken = errors.New("servicereg: base domain already registered")
	// ErrServiceTaken 表示该基域下服务名已被占用。
	ErrServiceTaken = errors.New("servicereg: service name already in use")
	// ErrFQDNTaken 表示子域名已全局被其它服务占用(FQDN 唯一,防跨基域撞名)。
	ErrFQDNTaken = errors.New("servicereg: subdomain already in use")
	// ErrInvalidBaseDomain 表示基域格式非法。
	ErrInvalidBaseDomain = errors.New("servicereg: invalid base domain")
	// ErrInvalidServiceName 表示服务名非法(子域名 label)。
	ErrInvalidServiceName = errors.New("servicereg: invalid service name")
	// ErrInvalidProtocol 表示协议枚举非法。
	ErrInvalidProtocol = errors.New("servicereg: invalid protocol")
	// ErrInvalidUpstream 表示上游值非法(容器名字符集 / 地址形态)。
	ErrInvalidUpstream = errors.New("servicereg: invalid upstream")
	// ErrInvalidPort 表示端口越界(1..65535)。
	ErrInvalidPort = errors.New("servicereg: port must be in 1..65535")
	// ErrTCPPortTaken 表示 TCP 监听端口与其它服务或 80/443 冲突。
	ErrTCPPortTaken = errors.New("servicereg: tcp listen port already in use")
	// ErrInvalidCert 表示证书 PEM 非法或证书/私钥不配对。
	ErrInvalidCert = errors.New("servicereg: invalid certificate or key")
	// ErrNoGateway 表示尚未指定网关主机,无法执行编排动作。
	ErrNoGateway = errors.New("servicereg: gateway server not configured")
	// ErrNginxStart 表示网关容器启动/重建失败。
	ErrNginxStart = errors.New("servicereg: 启动网关容器失败")
	// ErrPortConflict 表示网关主机 80/443(HTTP/HTTPS 宿主端口)被占用。
	ErrPortConflict = errors.New("servicereg: 网关主机端口被占用,无法启动网关容器")
	// ErrUpstreamConnect 表示上游容器接入共享网络失败。
	ErrUpstreamConnect = errors.New("servicereg: 上游容器接入网关网络失败")
	// ErrApply 表示渲染/校验/下发/热加载 nginx 配置失败。
	ErrApply = errors.New("servicereg: 应用网关配置失败")
	// ErrInvalidSetting 表示网关设置项非法(镜像/网络/容器名/卷名/端口)。
	ErrInvalidSetting = errors.New("servicereg: invalid gateway setting")
)

// SecretSealer 抽象保险库的对称加解密(证书 PEM 密文存储);由已装配的 vault.Vault 满足。
// 未配置 master key 时 Seal/Open 返回错误,vault 语义原样透传(httpapi 映射 vault_unconfigured)。
type SecretSealer interface {
	SealSecret(plaintext []byte) ([]byte, error)
	OpenSecret(sealed []byte) ([]byte, error)
}

// 校验白名单(与 proxy 包同风格;渲染处不再二次防注入)。
var (
	// baseDomainRe:小写 FQDN,≥2 个 label,每 label 1..63 字符 [a-z0-9-] 且不以 - 开头/结尾。
	baseDomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	// serviceNameRe:子域名 label,小写字母数字(可含 -,不以 - 开头),≤63 字符。
	serviceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// dockerNameRe:docker 容器/网络/卷名(字母数字 + . _ -,不以 - 开头)。
	dockerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	// imageRefRe:镜像引用(registry/repo:tag@digest;首字符字母数字防 flag 注入)。
	imageRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
)

// Settings 是网关设置(单例行;已剥离敏感字段——只有 token 哈希存库,哈希也不出本包)。
type Settings struct {
	ServerID       string
	HTTPPort       int // 宿主 HTTP 端口(容器内恒 80)
	HTTPSPort      int // 宿主 HTTPS 端口(容器内恒 443)
	Image          string
	Network        string
	ContainerName  string
	VolumeName     string
	LastApplyAt    time.Time
	LastApplyError string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Domain 是一个已注册基域。证书密文留在 store 层,领域对象只带展示元数据。
type Domain struct {
	ID            string
	BaseDomain    string // efg.com
	HasCert       bool   // 是否已上传证书(决定该基域走 HTTPS 还是仅 HTTP)
	CertSubject   string // 如 "CN=*.efg.com"
	CertDNSNames  string // SAN 摘要(逗号分隔)
	CertExpiresAt time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RegisteredService 是一个已注册服务(abc → abc.efg.com)。
type RegisteredService struct {
	ID            string
	DomainID      string
	Name          string // abc;FQDN = Name + "." + BaseDomain
	Protocol      string // http | tcp
	UpstreamKind  string // container | address
	Upstream      string // 容器名 或 HOST(IP/FQDN/host.docker.internal)
	UpstreamPort  int
	TCPListenPort int // tcp 协议:网关监听端口;http 协议恒 0
	Enabled       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// GatewayStatus 是网关容器探测快照(前端展示 + 部署前知情)。
type GatewayStatus struct {
	Configured     bool   // 是否已指定网关主机
	ServerID       string
	ServerName     string
	Installed      bool   // 容器是否存在
	Running        bool
	Image          string
	Ports          string // 发布端口摘要,如 "80,443,3306"
	LastApplyAt    time.Time
	LastApplyError string
}

// SettingsUpdate 是更新网关设置的入参(指针字段,nil = 保持不变)。
type SettingsUpdate struct {
	ServerID      *string // 空串 = 清空(转配置态)
	HTTPPort      *int
	HTTPSPort     *int
	Image         *string
	Network       *string
	ContainerName *string
	VolumeName    *string
}

// CreateServiceInput 是注册服务的入参。
type CreateServiceInput struct {
	DomainID      string
	Name          string
	Protocol      string
	UpstreamKind  string
	Upstream      string
	UpstreamPort  int
	TCPListenPort int
}

// UpdateServiceInput 是更新服务的入参(指针字段,nil = 保持不变)。
type UpdateServiceInput struct {
	Name          *string
	Protocol      *string
	UpstreamKind  *string
	Upstream      *string
	UpstreamPort  *int
	TCPListenPort *int
}

// Service 定义服务注册网关领域对外接口(httpapi 消费)。
type Service interface {
	// GetSettings 返回网关设置(库为空时返回默认值单例行,无副作用)。
	GetSettings(ctx context.Context) (*Settings, error)
	// UpdateSettings 校验并更新网关设置;不触网(纯落库)。改端口/容器名后需 DeployGateway 重建生效。
	UpdateSettings(ctx context.Context, in SettingsUpdate) (*Settings, error)

	// ListDomains 返回全部基域(创建时间倒序)。
	ListDomains(ctx context.Context) ([]Domain, error)
	// CreateDomain 注册基域(纯落库,不编排;DNS 由用户自行解析)。
	CreateDomain(ctx context.Context, baseDomain string) (*Domain, error)
	// DeleteDomain 删除基域及其下全部服务 → apply 收敛(配置摘除)。
	DeleteDomain(ctx context.Context, id string) error
	// UploadCert 校验证书/私钥配对 → 密文落库 → apply 下发(证书管理页经 CertSink 调用)。
	UploadCert(ctx context.Context, domainID, certPEM, keyPEM string) (*Domain, error)
	// ClearCert 清空某基域证书(回退 HTTP-only 并 apply)。删除证书时由证书管理联动调用。
	ClearCert(ctx context.Context, domainID string) error
	// ExportCert 返回某基域当前证书 PEM 明文(进程内供证书管理比对归属;绝不过 HTTP)。无证书 ok=false。
	ExportCert(ctx context.Context, domainID string) (certPEM string, ok bool, err error)

	// ListServices 返回全部注册服务(创建时间倒序,含基域名供展示)。
	ListServices(ctx context.Context) ([]ServiceWithDomain, error)
	// CreateService 校验 → 落库 → 编排(ensure 网关 + 接入上游 + apply);编排失败回滚删除。
	// http+container 服务自动创建实例 #1(单一上游,行为与单实例一致)。
	CreateService(ctx context.Context, in CreateServiceInput) (*RegisteredService, error)
	// UpdateService 更新服务定义 → apply 收敛。
	UpdateService(ctx context.Context, id string, in UpdateServiceInput) (*RegisteredService, error)
	// DeleteService 删除服务 → apply 收敛。
	DeleteService(ctx context.Context, id string) error
	// SetServiceEnabled 启停服务 → apply 收敛。
	SetServiceEnabled(ctx context.Context, id string, on bool) error

	// ListInstances 返回某服务的全部实例(创建时间正序)。
	ListInstances(ctx context.Context, serviceID string) ([]Instance, error)
	// AddInstance 给 http+container 服务加实例(默认 attached);触发 apply,失败回滚删除。
	AddInstance(ctx context.Context, serviceID, container string, port int) (*Instance, error)
	// RemoveInstance 删除实例(最后一个被删后该服务渲染为 503 维护态)。
	RemoveInstance(ctx context.Context, instanceID string) error
	// SetInstanceAttached 摘除/挂回实例(摘除 = 从 upstream 剔除,其余实例继续服务)。
	SetInstanceAttached(ctx context.Context, instanceID string, attached bool) error
	// SwapInstance 原子替换实例容器(行改名 + 单次 reload):deploy 实例轮转的切流量原语。
	SwapInstance(ctx context.Context, serviceID, oldContainer, newContainer string) error
	// ResolveDeployInstances 供 deploy 实例轮转反查(实例容器名或服务名 → attached 实例清单)。
	ResolveDeployInstances(ctx context.Context, serverID, container string) ([]DeployInstance, error)

	// GatewayStatus 探测网关容器状态(容器不存在不算错误)。
	GatewayStatus(ctx context.Context) (*GatewayStatus, error)
	// DeployGateway 显式部署/重建网关容器 + 全量 apply。
	DeployGateway(ctx context.Context) (*GatewayStatus, error)
	// RemoveGateway 停止并删除网关容器(保留卷);幂等。
	RemoveGateway(ctx context.Context) error
	// Apply 手动全量收敛(渲染 → 校验 → 下发 → 热加载)。
	Apply(ctx context.Context) error
}

// ServiceWithDomain 是服务 + 所属基域展示名(列表用)。
type ServiceWithDomain struct {
	RegisteredService
	BaseDomain string
}

// service 是 store + target + vault 支撑的 Service 实现。
type service struct {
	store *Store
	tg    target.Service
	vault SecretSealer
}

// New 构造 Service。tg 复用已装配的 target.Service(SSH + docker);vault 用于证书密文存储
// (nil = 证书功能不可用,其余照常)。不做任何重活(无 init 副作用)。
func New(db *sql.DB, tg target.Service, vault SecretSealer) Service {
	return &service{store: NewStore(db), tg: tg, vault: vault}
}

// ---------- 设置 ----------

func (s *service) GetSettings(ctx context.Context) (*Settings, error) {
	return s.store.getOrCreateSettings(ctx)
}

func (s *service) UpdateSettings(ctx context.Context, in SettingsUpdate) (*Settings, error) {
	cur, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return nil, err
	}
	next := *cur
	if in.ServerID != nil {
		next.ServerID = strings.TrimSpace(*in.ServerID)
	}
	if in.HTTPPort != nil {
		next.HTTPPort = *in.HTTPPort
	}
	if in.HTTPSPort != nil {
		next.HTTPSPort = *in.HTTPSPort
	}
	if in.Image != nil {
		next.Image = strings.TrimSpace(*in.Image)
		if next.Image == "" {
			next.Image = defaultNginxImage
		}
	}
	if in.Network != nil {
		next.Network = strings.TrimSpace(*in.Network)
		if next.Network == "" {
			next.Network = defaultNetwork
		}
	}
	if in.ContainerName != nil {
		next.ContainerName = strings.TrimSpace(*in.ContainerName)
		if next.ContainerName == "" {
			next.ContainerName = defaultContainerName
		}
	}
	if in.VolumeName != nil {
		next.VolumeName = strings.TrimSpace(*in.VolumeName)
		if next.VolumeName == "" {
			next.VolumeName = defaultVolumeName
		}
	}
	if err := validateSettings(next); err != nil {
		return nil, err
	}
	if err := s.store.updateSettings(ctx, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

// ---------- 基域 ----------

func (s *service) ListDomains(ctx context.Context) ([]Domain, error) {
	return s.store.listDomains(ctx)
}

func (s *service) CreateDomain(ctx context.Context, baseDomain string) (*Domain, error) {
	d := strings.ToLower(strings.TrimSpace(baseDomain))
	if err := validateBaseDomain(d); err != nil {
		return nil, err
	}
	dom := &Domain{
		ID:        uuid.NewString(),
		BaseDomain: d,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.store.insertDomain(ctx, dom); err != nil {
		return nil, err
	}
	return dom, nil
}

func (s *service) DeleteDomain(ctx context.Context, id string) error {
	if err := s.store.deleteDomain(ctx, id); err != nil {
		return err
	}
	return s.applyBestEffort(ctx)
}

func (s *service) UploadCert(ctx context.Context, domainID, certPEM, keyPEM string) (*Domain, error) {
	if s.vault == nil {
		return nil, target.ErrVaultUnconfigured
	}
	certPEM = strings.TrimSpace(certPEM)
	keyPEM = strings.TrimSpace(keyPEM)
	if certPEM == "" || keyPEM == "" {
		return nil, ErrInvalidCert
	}
	if len(certPEM) > maxCertPEMBytes || len(keyPEM) > maxCertPEMBytes {
		return nil, ErrInvalidCert
	}
	// tls.X509KeyPair 同时校验 PEM 形态与证书/私钥配对;再读叶子证书取主题/有效期。
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil || len(pair.Certificate) == 0 {
		return nil, ErrInvalidCert
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, ErrInvalidCert
	}
	subject := leaf.Subject.String()
	dnsNames := strings.Join(leaf.DNSNames, ",")
	expires := leaf.NotAfter.UTC()

	sealedCert, err := s.vault.SealSecret([]byte(certPEM))
	if err != nil {
		return nil, err
	}
	sealedKey, err := s.vault.SealSecret([]byte(keyPEM))
	if err != nil {
		return nil, err
	}
	dom, err := s.store.updateCert(ctx, domainID, sealedCert, sealedKey, subject, dnsNames, expires)
	if err != nil {
		return nil, err
	}
	// 证书落库即视为成功(脚本可幂等重试);下发失败单独报错但保留证书。
	if err := s.applyBestEffort(ctx); err != nil {
		return dom, err
	}
	return dom, nil
}

// ClearCert 清空某基域证书(密文/元数据置空)并 apply(基域回退 HTTP-only)。
func (s *service) ClearCert(ctx context.Context, domainID string) error {
	if err := s.store.clearCert(ctx, domainID); err != nil {
		return err
	}
	return s.applyBestEffort(ctx)
}

// ExportCert 返回某基域当前证书 PEM 明文(进程内解密;仅证书管理删除比对用,绝不过 HTTP)。
func (s *service) ExportCert(ctx context.Context, domainID string) (string, bool, error) {
	if s.vault == nil {
		return "", false, target.ErrVaultUnconfigured
	}
	sealed, _, ok, err := s.store.getCertSealed(ctx, domainID)
	if err != nil || !ok {
		return "", false, err
	}
	plain, uerr := s.vault.OpenSecret(sealed)
	if uerr != nil {
		return "", false, uerr
	}
	return string(plain), true, nil
}

// ---------- 服务 ----------

func (s *service) ListServices(ctx context.Context) ([]ServiceWithDomain, error) {
	return s.store.listServicesWithDomain(ctx)
}

func (s *service) CreateService(ctx context.Context, in CreateServiceInput) (*RegisteredService, error) {
	svc, err := s.prepareService(ctx, "", in)
	if err != nil {
		return nil, err
	}
	if err := s.store.insertService(ctx, svc); err != nil {
		return nil, err
	}
	// http+container 服务自动创建实例 #1(单一上游 = 单实例;多实例由 AddInstance 扩)。
	// 失败即回滚整个服务(无实例的 container 服务会渲染 503,不能带病落库)。
	if svc.Protocol == ProtocolHTTP && svc.UpstreamKind == UpstreamKindContainer {
		now := time.Now().UTC()
		if ierr := s.store.insertInstance(ctx, &Instance{
			ID: uuid.NewString(), ServiceID: svc.ID, Container: svc.Upstream,
			Port: svc.UpstreamPort, Attached: true, CreatedAt: now, UpdatedAt: now,
		}); ierr != nil {
			_ = s.store.deleteService(ctx, svc.ID)
			return nil, ierr
		}
	}
	// 编排失败回滚删除(声明状态与部署状态一致);网关未配置时静默跳过。
	if err := s.applyBestEffort(ctx); err != nil {
		_ = s.store.deleteService(ctx, svc.ID)
		return nil, err
	}
	return svc, nil
}

func (s *service) UpdateService(ctx context.Context, id string, in UpdateServiceInput) (*RegisteredService, error) {
	cur, err := s.store.getService(ctx, id)
	if err != nil {
		return nil, err
	}
	ci := CreateServiceInput{
		DomainID:      cur.DomainID,
		Name:          cur.Name,
		Protocol:      cur.Protocol,
		UpstreamKind:  cur.UpstreamKind,
		Upstream:      cur.Upstream,
		UpstreamPort:  cur.UpstreamPort,
		TCPListenPort: cur.TCPListenPort,
	}
	if in.Name != nil {
		ci.Name = *in.Name
	}
	if in.Protocol != nil {
		ci.Protocol = *in.Protocol
	}
	if in.UpstreamKind != nil {
		ci.UpstreamKind = *in.UpstreamKind
	}
	if in.Upstream != nil {
		ci.Upstream = *in.Upstream
	}
	if in.UpstreamPort != nil {
		ci.UpstreamPort = *in.UpstreamPort
	}
	if in.TCPListenPort != nil {
		ci.TCPListenPort = *in.TCPListenPort
	}
	svc, err := s.prepareService(ctx, id, ci)
	if err != nil {
		return nil, err
	}
	if err := s.store.updateService(ctx, svc); err != nil {
		return nil, err
	}
	if err := s.applyBestEffort(ctx); err != nil {
		return nil, err
	}
	return svc, nil
}

func (s *service) DeleteService(ctx context.Context, id string) error {
	if err := s.store.deleteService(ctx, id); err != nil {
		return err
	}
	return s.applyBestEffort(ctx)
}

func (s *service) SetServiceEnabled(ctx context.Context, id string, on bool) error {
	if err := s.store.setServiceEnabled(ctx, id, on); err != nil {
		return err
	}
	return s.applyBestEffort(ctx)
}

// prepareService 把入参归一化 + 全量校验(含跨服务 FQDN / TCP 端口唯一性),构造待落库对象。
// selfID 为更新场景自身 id(唯一性检查排除自己);创建时为空。
func (s *service) prepareService(ctx context.Context, selfID string, in CreateServiceInput) (*RegisteredService, error) {
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	in.Protocol = strings.TrimSpace(in.Protocol)
	if in.Protocol == "" {
		in.Protocol = ProtocolHTTP
	}
	in.UpstreamKind = strings.TrimSpace(in.UpstreamKind)
	if in.UpstreamKind == "" {
		in.UpstreamKind = UpstreamKindContainer
	}
	in.Upstream = strings.TrimSpace(in.Upstream)

	dom, err := s.store.getDomain(ctx, in.DomainID)
	if err != nil {
		return nil, err
	}
	if !serviceNameRe.MatchString(in.Name) {
		return nil, ErrInvalidServiceName
	}
	if in.Protocol != ProtocolHTTP && in.Protocol != ProtocolTCP {
		return nil, ErrInvalidProtocol
	}
	if in.UpstreamKind != UpstreamKindContainer && in.UpstreamKind != UpstreamKindAddress {
		return nil, ErrInvalidUpstream
	}
	if in.Upstream == "" {
		return nil, ErrInvalidUpstream
	}
	if in.UpstreamKind == UpstreamKindContainer {
		if len(in.Upstream) > 128 || !dockerNameRe.MatchString(in.Upstream) {
			return nil, ErrInvalidUpstream
		}
	} else if !validUpstreamHost(in.Upstream) {
		return nil, ErrInvalidUpstream
	}
	if in.UpstreamPort < 1 || in.UpstreamPort > 65535 {
		return nil, ErrInvalidPort
	}
	if in.Protocol == ProtocolTCP {
		if in.TCPListenPort < 1 || in.TCPListenPort > 65535 {
			return nil, ErrInvalidPort
		}
	} else {
		in.TCPListenPort = 0
	}

	// 跨服务唯一性:FQDN 全局唯一(防 efg.com 与 x.efg.com 两个基域撞出同名 FQDN)、TCP 监听端口
	// 全局唯一且避开 80/443(容器内 HTTP 监听)。
	all, err := s.store.listServices(ctx)
	if err != nil {
		return nil, err
	}
	fqdn := in.Name + "." + dom.BaseDomain
	for _, o := range all {
		if o.ID == selfID {
			continue
		}
		// TCP 监听端口全局唯一(先于同域跳过:同域服务也要查端口)。
		if in.Protocol == ProtocolTCP && o.Protocol == ProtocolTCP && o.TCPListenPort == in.TCPListenPort {
			return nil, ErrTCPPortTaken
		}
		if o.DomainID == in.DomainID {
			// 同域重名由 (domain_id,name) 唯一约束兜底(ErrServiceTaken);FQDN 检查只管跨域撞名。
			continue
		}
		od, derr := s.store.getDomain(ctx, o.DomainID)
		if derr != nil {
			continue
		}
		if o.Name+"."+od.BaseDomain == fqdn {
			return nil, ErrFQDNTaken
		}
	}
	if in.Protocol == ProtocolTCP && (in.TCPListenPort == 80 || in.TCPListenPort == 443) {
		return nil, ErrTCPPortTaken
	}

	now := time.Now().UTC()
	svc := &RegisteredService{
		ID:            uuid.NewString(),
		DomainID:      in.DomainID,
		Name:          in.Name,
		Protocol:      in.Protocol,
		UpstreamKind:  in.UpstreamKind,
		Upstream:      in.Upstream,
		UpstreamPort:  in.UpstreamPort,
		TCPListenPort: in.TCPListenPort,
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if selfID != "" {
		cur, err := s.store.getService(ctx, selfID)
		if err != nil {
			return nil, err
		}
		svc.ID = cur.ID
		svc.CreatedAt = cur.CreatedAt
		svc.Enabled = cur.Enabled
	}
	return svc, nil
}

// ---------- 网关编排 ----------

func (s *service) GatewayStatus(ctx context.Context) (*GatewayStatus, error) {
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return nil, err
	}
	g := &GatewayStatus{
		Configured:     st.ServerID != "",
		ServerID:       st.ServerID,
		LastApplyAt:    st.LastApplyAt,
		LastApplyError: st.LastApplyError,
	}
	if st.ServerID == "" {
		return g, nil
	}
	if srv, err := s.tg.Get(ctx, st.ServerID); err == nil {
		g.ServerName = srv.Name
	}
	inst, err := inspectNginx(ctx, s.tg, st)
	if err != nil {
		return nil, err
	}
	g.Installed = inst.Installed
	g.Running = inst.Running
	g.Image = inst.Image
	g.Ports = inst.Ports
	return g, nil
}

func (s *service) DeployGateway(ctx context.Context) (*GatewayStatus, error) {
	if err := s.applyFull(ctx); err != nil {
		return nil, err
	}
	return s.GatewayStatus(ctx)
}

func (s *service) RemoveGateway(ctx context.Context) error {
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return err
	}
	if st.ServerID == "" {
		return ErrNoGateway
	}
	return removeNginx(ctx, s.tg, st)
}

func (s *service) Apply(ctx context.Context) error {
	return s.applyFull(ctx)
}

// applyBestEffort:网关未配置(server_id 空)时静默跳过(配置态);否则全量收敛并把结果
// 记回 settings.last_apply_*(成功清错,失败记因)。返回 error 供调用方决定回滚策略。
func (s *service) applyBestEffort(ctx context.Context) error {
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return err
	}
	if st.ServerID == "" {
		return nil
	}
	if err := s.applyWith(ctx, st); err != nil {
		_ = s.store.setApplyResult(ctx, time.Now().UTC(), err.Error())
		return err
	}
	_ = s.store.setApplyResult(ctx, time.Now().UTC(), "")
	return nil
}

// applyFull 是显式编排入口(DeployGateway / Apply):未配置网关主机 → ErrNoGateway。
func (s *service) applyFull(ctx context.Context) error {
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return err
	}
	if st.ServerID == "" {
		return ErrNoGateway
	}
	if err := s.applyWith(ctx, st); err != nil {
		_ = s.store.setApplyResult(ctx, time.Now().UTC(), err.Error())
		return err
	}
	_ = s.store.setApplyResult(ctx, time.Now().UTC(), "")
	return nil
}

// applyWith 执行一轮完整收敛:ensure 容器(TCP 端口并集重建)→ 接入上游(container 上游按
// **attached 实例**逐个接入;死实例探活剔除)→ 下发证书 → 渲染 → nginx -t 校验 → docker cp
// → 热加载。任何一步失败即返回人话错误。
func (s *service) applyWith(ctx context.Context, st *Settings) error {
	domains, err := s.store.listDomains(ctx)
	if err != nil {
		return err
	}
	services, err := s.store.listServices(ctx)
	if err != nil {
		return err
	}
	allInstances, err := s.store.listAllInstances(ctx)
	if err != nil {
		return err
	}
	svcByID := make(map[string]*RegisteredService, len(services))
	var enabled []RegisteredService
	var tcpPorts []int
	for i := range services {
		svc := &services[i]
		svcByID[svc.ID] = svc
		if !svc.Enabled {
			continue
		}
		enabled = append(enabled, *svc)
		if svc.Protocol == ProtocolTCP {
			tcpPorts = append(tcpPorts, svc.TCPListenPort)
		}
	}

	if err := ensureNginx(ctx, s.tg, st, tcpPorts); err != nil {
		return err
	}

	// 实例探活 + 接入共享网络:attached 实例中容器已消失/已停止的**静默剔除**本次渲染
	// (否则 nginx -t 会因 "host not found in upstream" 整体失败,一个死实例卡死全部配置变更);
	// 存活的实例接入网络(幂等),接入后渲染进 upstream。
	aliveInstances := make([]Instance, 0, len(allInstances))
	for _, inst := range allInstances {
		if !inst.Attached {
			continue
		}
		svc, ok := svcByID[inst.ServiceID]
		if !ok || !svc.Enabled || svc.Protocol != ProtocolHTTP || svc.UpstreamKind != UpstreamKindContainer {
			continue
		}
		alive, _ := s.containerAlive(ctx, st, inst.Container)
		if !alive {
			continue
		}
		if err := connectUpstream(ctx, s.tg, st, inst.Container); err != nil {
			return err
		}
		aliveInstances = append(aliveInstances, inst)
	}
	// tcp 服务的容器上游仍是单一 Upstream(实例化仅覆盖 http)。
	for _, svc := range enabled {
		if svc.Protocol == ProtocolTCP && svc.UpstreamKind == UpstreamKindContainer {
			if err := connectUpstream(ctx, s.tg, st, svc.Upstream); err != nil {
				return err
			}
		}
	}
	if err := s.deployCerts(ctx, st, domains); err != nil {
		return err
	}
	conf := renderNginxConf(domains, enabled, aliveInstances)
	return applyNginxConf(ctx, s.tg, st, conf)
}

// containerAlive 探测网关主机上某容器是否存在且运行中(docker inspect State.Running)。
// 探测不确定(输出异常)时保守视为存活,交由 nginx -t 兜底报错;确证不存在/已停止 → false。
func (s *service) containerAlive(ctx context.Context, st *Settings, container string) (bool, error) {
	res, err := s.tg.Exec(ctx, st.ServerID, []string{
		"docker", "inspect", "--format", "{{.State.Running}}", container,
	})
	if err != nil {
		return true, err
	}
	if res.ExitCode != 0 {
		return false, nil // No such object → 实例容器已消失
	}
	if strings.EqualFold(strings.TrimSpace(res.Stdout), "false") {
		return false, nil // 容器存在但已停止(docker DNS 不再解析其名)
	}
	return true, nil
}

// ---------- 校验 ----------

func validateBaseDomain(d string) error {
	if d == "" || len(d) > 253 || !baseDomainRe.MatchString(d) {
		return ErrInvalidBaseDomain
	}
	return nil
}

func validateSettings(st Settings) error {
	if st.HTTPPort < 1 || st.HTTPPort > 65535 || st.HTTPSPort < 1 || st.HTTPSPort > 65535 {
		return ErrInvalidSetting
	}
	if st.HTTPPort == st.HTTPSPort {
		return ErrInvalidSetting
	}
	if len(st.Image) > 255 || !imageRefRe.MatchString(st.Image) {
		return ErrInvalidSetting
	}
	for _, v := range []string{st.Network, st.ContainerName, st.VolumeName} {
		if v == "" || len(v) > 128 || !dockerNameRe.MatchString(v) {
			return ErrInvalidSetting
		}
	}
	return nil
}

// validUpstreamHost 校验 address 上游的 HOST:IP / FQDN / host.docker.internal。
func validUpstreamHost(h string) bool {
	if h == "host.docker.internal" {
		return true
	}
	if net.ParseIP(h) != nil {
		return true
	}
	return len(h) <= 253 && baseDomainRe.MatchString(strings.ToLower(h))
}

// FQDN 返回服务的完整域名(供展示;http 服务即访问地址)。
func (rs RegisteredService) FQDN(base string) string { return rs.Name + "." + base }

// maxCertPEMBytes 是单份证书/私钥 PEM 的尺寸上限(容 RSA-4096 链)。
const maxCertPEMBytes = 64 << 10
