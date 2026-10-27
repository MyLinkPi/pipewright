// Package servicereg 是「服务注册网关」的领域层:在用户选定的(一台或多台)网关主机上,经 SSH
// 各编排一个**配置完全一致**的独立 nginx 容器,把「服务名 + 基域」映射为子域名反向代理
// (abc + efg.com → abc.efg.com)。多台网关的流量分配由用户在 DNS 层自行解析,平台不做负载均衡。
//
// 集群模型(0068):实例 = (服务器, 宿主端口),不再限网关本机容器 —— upsteram 成员是
// 「服务器地址:端口」,各网关主机渲染同一份全量配置,本机没有部署的服务同样代理远端。
// 容器实例由部署自动注册(宿主端口范围探测自动分配);非容器实例由部署注册或人工添加。
// 升级语义:容器走「扩容起新 → 预热 → 原子切换 → 排空 → 缩容停旧」零停机轮转;非容器走
// 「摘除 → 原地升级 → 健康 → 挂回」,集群可用性由其余机器实例保证。
//
// 定位(Caddy 反代下线后的唯一网关):面向「泛域名解析到网关主机 + 证书托管」的场景 ——
// *.efg.com 由用户解析到网关主机 IP,平台不接管网关域 DNS。证书的签发/续期/导入统一由
// internal/certmgmt(acme.sh)负责,经 CertSink(UploadCert)同步到基域;本包只管证书密文的
// 存取与 nginx 渲染下发。证书 PEM 经 vault SealSecret 加密入库,绝不明文回 API。
//
// 设计纪律(与 proxy 包一致):
//   - 一切 docker/nginx 命令经 target.Exec 以 array 形式执行(AC-SEC-02 不拼 shell);唯一例外是
//     容器启动命令内一段**固定常量**的 sh -c 自举脚本(无任何用户输入,不是注入面)。
//   - 声明式全量收敛:任何变更后全量重渲染(主配置 nginx.conf + conf.d/ 每服务一个文件,
//     主配置显式 include)→ 先在沙箱 nginx -t 校验再热加载,校验失败不动活配置;新增 TCP
//     监听端口时容器带「既有映射 ∪ 新端口」重建(Docker 端口创建时固定)。
//   - 实例地址静态可解析:渲染前由控制端把 serverID 解析为 IP 写入 upstream(reload 时重解析),
//     解析失败的实例跳过渲染(注释标注),故障实例交给 max_fails 熔断;人工 address 单上游
//     仍走 resolver(127.0.0.11,Docker 内嵌 DNS)运行期动态解析。
//   - 证书 PEM 绝不明文入库/回 API/写日志:SealSecret 密文存 BLOB,仅在 apply 时于进程内解密下发。
//   - 网关未配置主机(主机列表为空)时一切 CRUD 照常(配置态),编排静默跳过,DeployGateway 时收敛;
//     多机 apply 逐台执行,单台失败不阻断其余台,聚合报错。
package servicereg

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/mesh"
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
	// UpstreamKindStatic 表示网关 nginx 直接托管静态文件(纯前端 dist):Upstream 恒空、
	// UpstreamPort 恒 0、无实例;内容由「网关静态发布」部署节点落到各网关主机的
	// <StaticRoot>/<服务ID>/ 目录(bind mount 只读挂进网关容器)。
	UpstreamKindStatic = "static"
	// UpstreamKindForward 表示转发 mesh 对端环境注册的服务(跨环境服务转发):Upstream/
	// UpstreamPort 恒空、无本地实例;PeerNodeID/PeerServiceID 指向对端服务。渲染成员 =
	// 对端网关 IP:端口(经 /api/mesh/servicereg/catalog 拉取,授权由对端 forward ACL
	// 控制);数据面 = 网关机 /32 路由 + 本机 ip_forward + EasyTier 子网代理。
	UpstreamKindForward = "forward"
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
	// ErrPathTaken 表示同一 FQDN 下相同 path 已被其它服务占用(同域多服务靠不同 path 共存)。
	ErrPathTaken = errors.New("servicereg: path already in use")
	// ErrInvalidPath 表示子路径格式非法(须 / 开头、合法字符集、≤128 字符;tcp 服务不允许 path)。
	ErrInvalidPath = errors.New("servicereg: invalid service path")
	// ErrStaticService 表示该操作不适用于静态托管服务(静态服务无实例/上游端口语义)。
	ErrStaticService = errors.New("servicereg: not supported for static service")
	// ErrForwardService 表示该操作不适用于转发服务(转发服务无本地实例语义,
	// 上游来自对端目录)。
	ErrForwardService = errors.New("servicereg: not supported for forward service")
	// ErrInvalidBaseDomain 表示基域格式非法。
	ErrInvalidBaseDomain = errors.New("servicereg: invalid base domain")
	// ErrInvalidServiceName 表示服务名非法(子域名 label)。
	ErrInvalidServiceName = errors.New("servicereg: invalid service name")
	// ErrInvalidProtocol 表示协议枚举非法。
	ErrInvalidProtocol = errors.New("servicereg: invalid protocol")
	// ErrInvalidUpstream 表示上游值非法(容器名字符集 / 地址形态)。
	ErrInvalidUpstream = errors.New("servicereg: invalid upstream")
	// ErrInvalidForwardPeer 表示转发入参非法(peer 未登记/目录不可达(未授权)/协议不匹配/服务不存在)。
	ErrInvalidForwardPeer = errors.New("servicereg: invalid forward peer or service")
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
	// ErrConfRead 表示读取网关容器内 nginx 配置文件失败(容器未部署/文件缺失/传输失败)。
	ErrConfRead = errors.New("servicereg: 读取网关配置失败")
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
	// servicePathRe:URL 子路径(/ 开头,字母数字与 . _ ~ - /;归一化后无尾斜杠、无 // ;
	// "." / ".." 段由 normalizeServicePath 逐段拒绝)。
	servicePathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/-]+$`)
	// staticRootRe:静态发布根目录(绝对路径,路径安全字符集,无尾斜杠)。
	staticRootRe = regexp.MustCompile(`^/[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
)

// Settings 是网关设置(单例行;已剥离敏感字段——只有 token 哈希存库,哈希也不出本包)。
// 多机模型:全部网关主机列于 ServerIDs,每台部署完全一致的网关(DNS 轮询由用户自行解析)。
type Settings struct {
	// ServerIDs 是全部网关主机引用 id(升序无关,按用户选择顺序)。空 = 未配置(配置态)。
	ServerIDs []string
	// ServerID 是 ServerIDs[0] 的派生镜像(读侧兜底同步;仅兼容展示,勿用于编排判断)。
	ServerID      string
	HTTPPort      int // 宿主 HTTP 端口(容器内恒 80)
	HTTPSPort     int // 宿主 HTTPS 端口(容器内恒 443)
	Image         string
	Network       string
	ContainerName string
	VolumeName    string
	// StaticRoot 是各网关主机上的静态发布根目录(bind mount 只读挂进网关容器的
	// /etc/pipewright/static;全部网关主机使用同一路径约定)。
	StaticRoot     string
	LastApplyAt    time.Time
	LastApplyError string // 最近一轮聚合错误文案(逐台明细见 LastApplyErrors)
	// LastApplyErrors 是最近一轮逐台错误(serverID → 文案,仅失败项;nil = 全部成功)。
	LastApplyErrors map[string]string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// PrimaryServerID 返回第一台网关主机(兼容单机视图);未配置 → ""。
func (st Settings) PrimaryServerID() string {
	if len(st.ServerIDs) == 0 {
		return ""
	}
	return st.ServerIDs[0]
}

// HasServer 报告某主机是否网关主机。
func (st Settings) HasServer(id string) bool {
	for _, v := range st.ServerIDs {
		if v == id {
			return true
		}
	}
	return false
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
	ID       string
	DomainID string
	Name     string // abc;FQDN = Name + "." + BaseDomain
	// Aliases 是同基域下的额外子域名 label(如 api-v2 → api-v2.efg.com):与主 FQDN
	// 同一 server 块 server_name,任一域名都路由到本服务;全局(主名 + 别名)互斥。
	// tcp 协议允许存但渲染忽略(stream 不按域名路由)。
	Aliases       []string
	Path          string // 子路径('' = 根路径 /);同 FQDN 下多服务靠不同 path 共存
	Protocol      string // http | tcp
	UpstreamKind  string // container | address | static | forward
	Upstream      string // 容器名 或 HOST(IP/FQDN/host.docker.internal);static/forward 恒空
	UpstreamPort  int    // static/forward 恒 0
	TCPListenPort int    // tcp 协议:网关监听端口;http 协议恒 0
	// PeerNodeID / PeerServiceID 是 forward 专用的对端引用(mesh 节点 id + 对端服务 id);
	// 其余类型恒空。
	PeerNodeID    string
	PeerServiceID string
	// StripPathPrefix 反代是否剥离 location 前缀再转发(默认 false = 原样透传完整路径);
	// 仅 http 反代形态有效,static/tcp 恒 false。
	StripPathPrefix bool
	Enabled         bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// GatewayStatus 是网关容器探测快照(前端展示 + 部署前知情)。多机:每台一项 Servers;
// 旧平铺字段(ServerID/ServerName/Installed/Running/Image/Ports/Version)填第一台,兼容旧前端。
type GatewayStatus struct {
	Configured      bool // 是否已配置网关主机
	ServerID        string
	ServerName      string
	Installed       bool   // 容器是否存在
	Running         bool   //
	Image           string //
	Ports           string // 发布端口摘要,如 "80,443,3306"
	Version         string // 容器内 nginx 二进制版本,如 "1.27.4"(探测失败为空)
	Servers         []GatewayServerStatus
	LastApplyAt     time.Time
	LastApplyError  string
	LastApplyErrors map[string]string // 逐台收敛错误(serverID → 文案,仅失败项)
	// Forward 是跨环境转发数据面状态(/32 路由对账 + ip_forward + 冲突告警)。
	Forward *ForwardStatus
}

// GatewayServerStatus 是单台网关主机的容器探测快照。
type GatewayServerStatus struct {
	ServerID   string
	ServerName string
	Installed  bool
	Running    bool
	Image      string
	Ports      string
	Version    string
}

// SettingsUpdate 是更新网关设置的入参(指针字段,nil = 保持不变)。
type SettingsUpdate struct {
	// ServerIDs 设置全部网关主机(空列表 = 清空转配置态);优先于 ServerID。
	ServerIDs *[]string
	// ServerID 是旧版单机入口(等价于单元素 ServerIDs;空串 = 清空)。仅兼容保留。
	ServerID      *string
	HTTPPort      *int
	HTTPSPort     *int
	Image         *string
	Network       *string
	ContainerName *string
	VolumeName    *string
	StaticRoot    *string
}

// CreateServiceInput 是注册服务的入参。
type CreateServiceInput struct {
	DomainID      string
	Name          string
	Aliases       []string // 同基域额外子域名(normalizeServiceAliases 归一化)
	Path          string
	Protocol      string
	UpstreamKind  string
	Upstream      string
	UpstreamPort  int
	TCPListenPort int
	// PeerNodeID / PeerServiceID 是 forward 专用(对端 mesh 节点 id + 对端服务 id)。
	PeerNodeID    string
	PeerServiceID string
	// StripPathPrefix 反代剥离 location 前缀(默认保留前缀原样透传)。
	StripPathPrefix bool
}

// UpdateServiceInput 是更新服务的入参(指针字段,nil = 保持不变)。
type UpdateServiceInput struct {
	Name            *string
	Aliases         *[]string
	Path            *string
	Protocol        *string
	UpstreamKind    *string
	Upstream        *string
	UpstreamPort    *int
	TCPListenPort   *int
	PeerNodeID      *string
	PeerServiceID   *string
	StripPathPrefix *bool
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
	// AddInstance 给 http 服务人工加实例(默认 attached):server 必填;container 空 = 非容器
	// 实例(hostPort 即进程端口)。触发 apply,失败回滚删除。
	AddInstance(ctx context.Context, serviceID, serverID, container string, port, hostPort int) (*Instance, error)
	// EnsureService 幂等注册/刷新服务(部署节点「一键生成」):按(基域, 服务名, path)定位,
	// 存在即刷新上游默认,不存在即创建;不自动建实例(实例由部署按目标机自动注册;static 无实例)。
	EnsureService(ctx context.Context, in CreateServiceInput) (*RegisteredService, error)
	// EnsureInstance 部署期幂等 upsert 实例(认领遗留行/更新端点/插入),行保留、apply 尽力。
	EnsureInstance(ctx context.Context, serviceID, serverID, container string, port, hostPort int) (*Instance, error)
	// EnsureInstanceDetached 部署期「预注册」(摘挂流程第一步):新行以 detached 落库(不进
	// upstream),存量行只更新端口/归属、attached 保持;不触发 apply。
	EnsureInstanceDetached(ctx context.Context, serviceID, serverID, container string, port, hostPort int) (*Instance, error)
	// PruneInstancesNotIn 同步清理:删除服务下 server 不在列表内的实例(部署全部成功后调用)。
	PruneInstancesNotIn(ctx context.Context, serviceID string, serverIDs []string) (int, error)
	// RemoveInstance 删除实例(最后一个被删后该服务渲染为 503 维护态)。
	RemoveInstance(ctx context.Context, instanceID string) error
	// SetInstanceAttached 摘除/挂回实例(摘除 = 从 upstream 剔除,其余实例继续服务)。
	SetInstanceAttached(ctx context.Context, instanceID string, attached bool) error
	// SwapInstance 原子替换实例容器(行改名 + host_port 同步 + 单次 reload):容器「扩缩容
	// 轮转」的切流量原语。
	SwapInstance(ctx context.Context, serviceID, serverID, oldContainer, newContainer string, hostPort int) error
	// ResolveDeployInstances 供 deploy 反查(serviceRef=服务 ID 或 container=容器名/服务名 →
	// **该 serverID 上**的 attached 实例清单;滚动升级只动本机)。
	ResolveDeployInstances(ctx context.Context, serverID, serviceRef, container string) ([]DeployInstance, error)
	// ResolveStaticDeploy 供 deploy 反查静态发布目标(serviceRef 须定位到 static 形态服务):
	// 返回全部网关主机与发布根目录(deploy_static 节点消费)。
	ResolveStaticDeploy(ctx context.Context, serviceRef string) (*StaticDeploySpec, error)

	// GatewayStatus 探测网关容器状态(容器不存在不算错误)。
	GatewayStatus(ctx context.Context) (*GatewayStatus, error)
	// GetGatewayConfig 读取某台网关主机容器内实际生效的 nginx.conf 及其引用文件(证书等)。
	// serverID 必须在网关主机列表内;主配置读取失败返回错误(容器未部署等)。
	GetGatewayConfig(ctx context.Context, serverID string) ([]NginxConfFile, error)
	// DeployGateway 显式部署/重建网关容器 + 全量 apply。
	DeployGateway(ctx context.Context) (*GatewayStatus, error)
	// RemoveGateway 停止并删除网关容器(保留卷);幂等。
	RemoveGateway(ctx context.Context) error
	// UpgradeGatewayImage 升级全部网关主机的 nginx 镜像:逐台 pull 期望引用并按镜像 ID 比对,
	// 有变化的台重建容器 + 全量收敛(配置/证书在具名卷,重建不丢);未变化的台不动容器。
	UpgradeGatewayImage(ctx context.Context) (*GatewayImageUpgradeResult, error)
	// Apply 手动全量收敛(渲染 → 校验 → 下发 → 热加载)。
	Apply(ctx context.Context) error

	// MeshCatalog 返回本端服务目录(授权 mesh 对端拉取;/api/mesh/servicereg/catalog 载荷)。
	MeshCatalog(ctx context.Context) (*mesh.ServiceCatalog, error)
	// ForwardPeers 返回可用作转发来源的 mesh 对端 + 各自目录可达性(服务表单级联下拉)。
	ForwardPeers(ctx context.Context) ([]ForwardPeerInfo, error)
	// ReconcileForwardRoutes 对账网关机上的跨环境 /32 路由(发现循环/apply 触发;幂等)。
	ReconcileForwardRoutes(ctx context.Context)
}

// ServiceWithDomain 是服务 + 所属基域展示名(列表用);InstanceCount 为该服务实例总数
// (列表徽标直显,免前端逐服务加载实例)。
type ServiceWithDomain struct {
	RegisteredService
	BaseDomain string
	// InstanceCount 含 detached 实例(与 ListInstances 口径一致)。
	InstanceCount int
}

// service 是 store + target + vault 支撑的 Service 实现。
type service struct {
	store *Store
	tg    target.Service
	vault SecretSealer
	// certSyncHook 由 main.go 注入(拉取证书管理侧既有证书并同步到本网关,best-effort):
	// 新建基域时证书下发链路(certmgmt → 本包)只会推给当时已存在的基域,后建的基域拿不到
	// 已有证书,靠此钩子在创建后反向拉一次,免手动刷新。
	certSyncHook func(ctx context.Context) error
	// serverHost 把 serverID 解析为注册时的原始地址(域名原样返回,IP 原样返回):实例池
	// upstream 成员直接写该地址,域名成员由网关容器自身的 DNS 在 reload 时解析,IP 变化后
	// 重新 apply 即生效。main.go 注入;nil(单测)→ 回落 serverAddr。
	serverHost func(ctx context.Context, serverID string) (string, error)
	// serverAddr 把 serverID 解析为网关可达 IP(控制端 DNS 解析主机名):mesh /32 路由与
	// 对端目录转发规划用 —— 路由配置只接受 IP,与实例池渲染(用 serverHost)语义不同。
	// main.go 注入;nil(单测)→ 实例全部跳过渲染、mesh 路由降级。
	serverAddr func(ctx context.Context, serverID string) (string, error)
	// fwd 聚跨环境转发(mesh)运行态:对端目录缓存 + 注入的 peer 清单/目录拉取器 + 路由
	// 对账快照(见 forward.go)。
	fwd forwardRuntime
}

// New 构造 Service。tg 复用已装配的 target.Service(SSH + docker);vault 用于证书密文存储
// (nil = 证书功能不可用,其余照常)。不做任何重活(无 init 副作用)。
func New(db *sql.DB, tg target.Service, vault SecretSealer) *service {
	return &service{store: NewStore(db), tg: tg, vault: vault}
}

// SetServerAddrResolver 注入 serverID → 可达 IP 解析器(main.go 晚绑:target 查 Host,
// 主机名再经 net.LookupHost 解析为 IP)。mesh /32 路由与对端目录的地基:路由与转发规划
// 只接受 IP。
func (s *service) SetServerAddrResolver(f func(ctx context.Context, serverID string) (string, error)) {
	s.serverAddr = f
}

// SetServerHostResolver 注入 serverID → 注册原始地址 解析器(main.go 晚绑:target 查 Host
// 原样返回,不做 DNS 解析)。实例池 upstream 成员的地基:域名注册的机器写域名,由网关
// 容器 DNS 在 reload 时解析(IP 变化后重新 apply 即生效);控制端不代解析,避免把会漂移
// 的 IP 静态烧进配置。未注入时回落 serverAddr(兼容旧装配/单测)。
func (s *service) SetServerHostResolver(f func(ctx context.Context, serverID string) (string, error)) {
	s.serverHost = f
}

// SetMeshForward 注入跨环境转发(mesh)依赖(main.go 晚绑,防包间依赖):
// listPeers 返回已知 mesh 对端(服务表单下拉);fetchCatalog 拉取某对端服务目录
// (mesh 签名客户端,403 = 未授权)。两者任一为 nil → 转发功能优雅关闭(校验拒绝创建)。
func (s *service) SetMeshForward(
	listPeers func(ctx context.Context) ([]ForwardPeer, error),
	fetchCatalog func(ctx context.Context, nodeID string) (*mesh.ServiceCatalog, error),
) {
	s.fwd.mu.Lock()
	s.fwd.listPeers = listPeers
	s.fwd.fetchCatalog = fetchCatalog
	s.fwd.cache = nil
	s.fwd.mu.Unlock()
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
	if in.ServerIDs != nil {
		next.ServerIDs = normalizeServerIDs(*in.ServerIDs)
	} else if in.ServerID != nil {
		// 旧版单机入口:等价于单元素列表(空串 = 清空)。
		next.ServerIDs = normalizeServerIDs([]string{*in.ServerID})
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
	if in.StaticRoot != nil {
		next.StaticRoot = strings.TrimSpace(*in.StaticRoot)
		if next.StaticRoot == "" {
			next.StaticRoot = defaultStaticRoot
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

// SetCertSyncHook 注入「拉取证书管理侧既有证书」的钩子(main.go 晚绑接线,避免包间依赖)。
func (s *service) SetCertSyncHook(hook func(ctx context.Context) error) { s.certSyncHook = hook }

// pullExistingCerts best-effort 触发一次证书拉取(未注入/失败均不影响调用方主流程)。
func (s *service) pullExistingCerts(ctx context.Context) {
	if s.certSyncHook == nil {
		return
	}
	_ = s.certSyncHook(ctx)
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
		ID:         uuid.NewString(),
		BaseDomain: d,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
	if err := s.store.insertDomain(ctx, dom); err != nil {
		return nil, err
	}
	// 已有覆盖该基域的证书(如 *.efg.com)→ 立即拉取下发,免去手动刷新证书;
	// 无覆盖证书时钩子为 no-op,失败亦不回滚建域。
	s.pullExistingCerts(ctx)
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
	// 集群模型:不再自动创建实例 #1 —— 实例 = (服务器, 宿主端口) 由部署自动注册或人工添加;
	// http+container 服务在首个实例就位前渲染 503 维护态(声明了实例池但无成员)。
	if err := s.applyBestEffort(ctx); err != nil {
		_ = s.store.deleteService(ctx, svc.ID)
		return nil, err
	}
	return svc, nil
}

// EnsureService 幂等注册/刷新服务(部署节点「一键生成」):按(基域, 服务名, path)定位 ——
// 存在 → 校验一致性后刷新上游默认与别名(kind/Upstream/UpstreamPort/StripPathPrefix/Aliases;
// 节点侧改了别名再生成须生效);不存在 → 创建。不自动建实例(部署按目标机 EnsureInstance;
// static 服务无实例)。重复点击/多流水线共用同一服务均安全。
func (s *service) EnsureService(ctx context.Context, in CreateServiceInput) (*RegisteredService, error) {
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	if in.Protocol == "" {
		in.Protocol = ProtocolHTTP
	}
	if in.UpstreamKind == "" {
		in.UpstreamKind = UpstreamKindContainer
	}
	path, perr := normalizeServicePath(in.Path)
	if perr != nil {
		return nil, perr
	}
	in.Path = path
	aliases, aerr := normalizeServiceAliases(in.Name, in.Aliases)
	if aerr != nil {
		return nil, aerr
	}
	in.Aliases = aliases
	all, err := s.store.listServices(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range all {
		if o.DomainID != in.DomainID || o.Name != in.Name || o.Path != in.Path {
			continue
		}
		if o.Protocol != in.Protocol {
			return nil, ErrServiceTaken // 同名同 path 服务协议不同:人工决断,不静默改写
		}
		upd := UpdateServiceInput{
			UpstreamKind:    &in.UpstreamKind,
			Upstream:        &in.Upstream,
			UpstreamPort:    &in.UpstreamPort,
			StripPathPrefix: &in.StripPathPrefix,
			Aliases:         &in.Aliases,
		}
		return s.UpdateService(ctx, o.ID, upd)
	}
	return s.CreateService(ctx, in)
}

func (s *service) UpdateService(ctx context.Context, id string, in UpdateServiceInput) (*RegisteredService, error) {
	cur, err := s.store.getService(ctx, id)
	if err != nil {
		return nil, err
	}
	ci := CreateServiceInput{
		DomainID:        cur.DomainID,
		Name:            cur.Name,
		Aliases:         cur.Aliases,
		Path:            cur.Path,
		Protocol:        cur.Protocol,
		UpstreamKind:    cur.UpstreamKind,
		Upstream:        cur.Upstream,
		UpstreamPort:    cur.UpstreamPort,
		TCPListenPort:   cur.TCPListenPort,
		PeerNodeID:      cur.PeerNodeID,
		PeerServiceID:   cur.PeerServiceID,
		StripPathPrefix: cur.StripPathPrefix,
	}
	if in.Name != nil {
		ci.Name = *in.Name
	}
	if in.Aliases != nil {
		ci.Aliases = *in.Aliases
	}
	if in.Path != nil {
		ci.Path = *in.Path
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
	if in.PeerNodeID != nil {
		ci.PeerNodeID = *in.PeerNodeID
	}
	if in.PeerServiceID != nil {
		ci.PeerServiceID = *in.PeerServiceID
	}
	if in.StripPathPrefix != nil {
		ci.StripPathPrefix = *in.StripPathPrefix
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
	// 级联清理实例行(否则孤儿实例在 svcByID 反查不中,虽不渲染但会积攒脏数据)。
	if err := s.store.deleteInstancesByService(ctx, id); err != nil {
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

// prepareService 把入参归一化 + 全量校验(含跨服务 FQDN+path / TCP 端口唯一性),构造待落库对象。
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
	path, perr := normalizeServicePath(in.Path)
	if perr != nil {
		return nil, perr
	}
	in.Path = path

	dom, err := s.store.getDomain(ctx, in.DomainID)
	if err != nil {
		return nil, err
	}
	if !serviceNameRe.MatchString(in.Name) {
		return nil, ErrInvalidServiceName
	}
	aliases, aerr := normalizeServiceAliases(in.Name, in.Aliases)
	if aerr != nil {
		return nil, aerr
	}
	in.Aliases = aliases
	if in.Protocol != ProtocolHTTP && in.Protocol != ProtocolTCP {
		return nil, ErrInvalidProtocol
	}
	if in.UpstreamKind != UpstreamKindContainer && in.UpstreamKind != UpstreamKindAddress &&
		in.UpstreamKind != UpstreamKindStatic && in.UpstreamKind != UpstreamKindForward {
		return nil, ErrInvalidUpstream
	}
	in.PeerNodeID = strings.TrimSpace(in.PeerNodeID)
	in.PeerServiceID = strings.TrimSpace(in.PeerServiceID)
	if in.UpstreamKind == UpstreamKindForward {
		// 转发远端:无本地上游/实例语义(Upstream 归零);peer 必须已登记且目录可拉取
		// (= 对端 forward ACL 已授权),对端服务须存在且协议与本端声明一致。
		in.Upstream = ""
		in.UpstreamPort = 0
		if in.PeerNodeID == "" || in.PeerServiceID == "" {
			return nil, ErrInvalidForwardPeer
		}
		var peerSvc *mesh.CatalogService
		cat, cerr := s.peerCatalog(ctx, in.PeerNodeID)
		if cerr == nil {
			for i := range cat.Services {
				if cat.Services[i].ServiceID == in.PeerServiceID {
					peerSvc = &cat.Services[i]
					break
				}
			}
		}
		if peerSvc == nil || peerSvc.Protocol != in.Protocol {
			return nil, ErrInvalidForwardPeer
		}
		if in.Protocol == ProtocolTCP {
			if in.TCPListenPort < 1 || in.TCPListenPort > 65535 {
				return nil, ErrInvalidPort
			}
		} else {
			in.TCPListenPort = 0
		}
	} else if in.UpstreamKind == UpstreamKindStatic {
		// 静态托管:仅 http;无上游/端口/前缀语义,统一归零(存库形态唯一)。
		if in.Protocol != ProtocolHTTP {
			return nil, ErrInvalidProtocol
		}
		in.Upstream = ""
		in.UpstreamPort = 0
		in.TCPListenPort = 0
		in.StripPathPrefix = false
	} else {
		// 上游校验按形态分叉:仅 tcp 强制显式上游;http 的 Upstream 均可空(部署托管:
		// container 由实例行携带容器名,address 由实例行携带主机地址,空时渲染维护态)。
		// 非空时按形态校验:container=容器名,address=主机地址。
		if in.Upstream == "" {
			if in.Protocol == ProtocolTCP {
				return nil, ErrInvalidUpstream
			}
		} else if in.UpstreamKind == UpstreamKindContainer {
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
	}
	if in.Protocol == ProtocolTCP && in.Path != "" {
		return nil, ErrInvalidPath // tcp 透传无 URL 语义,不允许子路径
	}

	// 跨服务唯一性:同一 FQDN 下相同 path 互斥(ErrPathTaken;同 FQDN 不同 path 共存于同一
	// server 块)、别名 label(主名 + 别名)同域互斥(别名代表整个 server 块,任何 path 都
	// 归属原服务,不能只按 path 区分)、TCP 监听端口全局唯一且避开 80/443(容器内 HTTP 监听)。
	all, err := s.store.listServices(ctx)
	if err != nil {
		return nil, err
	}
	fqdn := in.Name + "." + dom.BaseDomain
	for _, o := range all {
		if o.ID == selfID {
			continue
		}
		// TCP 监听端口全局唯一(先于其它检查:同域服务也要查端口)。
		if in.Protocol == ProtocolTCP && o.Protocol == ProtocolTCP && o.TCPListenPort == in.TCPListenPort {
			return nil, ErrTCPPortTaken
		}
		if o.DomainID == in.DomainID {
			// 同域同服务名同 path:领域层直接报 ErrPathTaken(比 DB 唯一约束的 ErrServiceTaken
			// 语义更准确);同服务名不同 path 合法共存。
			if o.Name == in.Name && o.Path == in.Path {
				return nil, ErrPathTaken
			}
			// 别名与既有服务的域名(主名或别名)互斥:别名 FQDN 会写进同一 server 块的
			// server_name,与他服务域名重复 = nginx 冲突 server name(先匹配者生效)。
			for _, a := range in.Aliases {
				if a == o.Name {
					return nil, ErrFQDNTaken
				}
				for _, oa := range o.Aliases {
					if a == oa {
						return nil, ErrFQDNTaken
					}
				}
			}
			for _, oa := range o.Aliases {
				if in.Name == oa {
					return nil, ErrFQDNTaken
				}
			}
			continue
		}
		od, derr := s.store.getDomain(ctx, o.DomainID)
		if derr != nil {
			continue
		}
		// 跨基域撞出同名 FQDN(防御:服务名字符集不含点,理论不可达):主名对主名同 path
		// 互斥;涉及别名的 FQDN 对称参与防御检查(整域互斥)。
		oFqdns := make(map[string]bool, 1+len(o.Aliases))
		oFqdns[o.Name+"."+od.BaseDomain] = true
		for _, oa := range o.Aliases {
			oFqdns[oa+"."+od.BaseDomain] = true
		}
		fqdnHit := oFqdns[fqdn]
		aliasHit := false
		for _, a := range in.Aliases {
			if oFqdns[a+"."+dom.BaseDomain] {
				aliasHit = true
				break
			}
		}
		if aliasHit || (fqdnHit && o.Path == in.Path) {
			return nil, ErrFQDNTaken
		}
	}
	if in.Protocol == ProtocolTCP && (in.TCPListenPort == 80 || in.TCPListenPort == 443) {
		return nil, ErrTCPPortTaken
	}

	now := time.Now().UTC()
	svc := &RegisteredService{
		ID:              uuid.NewString(),
		DomainID:        in.DomainID,
		Name:            in.Name,
		Aliases:         in.Aliases,
		Path:            in.Path,
		Protocol:        in.Protocol,
		UpstreamKind:    in.UpstreamKind,
		Upstream:        in.Upstream,
		UpstreamPort:    in.UpstreamPort,
		TCPListenPort:   in.TCPListenPort,
		PeerNodeID:      in.PeerNodeID,
		PeerServiceID:   in.PeerServiceID,
		StripPathPrefix: in.StripPathPrefix,
		Enabled:         true,
		CreatedAt:       now,
		UpdatedAt:       now,
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
		Configured:      len(st.ServerIDs) > 0,
		LastApplyAt:     st.LastApplyAt,
		LastApplyError:  st.LastApplyError,
		LastApplyErrors: st.LastApplyErrors,
	}
	for _, sid := range st.ServerIDs {
		gs := GatewayServerStatus{ServerID: sid}
		if srv, err := s.tg.Get(ctx, sid); err == nil && srv != nil {
			gs.ServerName = srv.Name
		}
		// 探测传输失败不拖垮整体状态:该台按未知处理(installed=false),交由 apply 报错。
		if inst, ierr := inspectNginx(ctx, s.tg, st, sid); ierr == nil {
			gs.Installed = inst.Installed
			gs.Running = inst.Running
			gs.Image = inst.Image
			gs.Ports = inst.Ports
			gs.Version = inst.Version
		}
		g.Servers = append(g.Servers, gs)
	}
	// 旧平铺字段填第一台(兼容旧前端/旧消费方)。
	if len(g.Servers) > 0 {
		g.ServerID = g.Servers[0].ServerID
		g.ServerName = g.Servers[0].ServerName
		g.Installed = g.Servers[0].Installed
		g.Running = g.Servers[0].Running
		g.Image = g.Servers[0].Image
		g.Ports = g.Servers[0].Ports
		g.Version = g.Servers[0].Version
	}
	fwd := s.forwardStatusSnapshot()
	g.Forward = &fwd
	return g, nil
}

// GetGatewayConfig 读取某台网关主机容器内实际生效的 nginx.conf 及其引用文件。
// serverID 必须在网关主机列表内(防止借查看接口对任意服务器执行命令)。
func (s *service) GetGatewayConfig(ctx context.Context, serverID string) ([]NginxConfFile, error) {
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, sid := range st.ServerIDs {
		if sid == serverID {
			ok = true
			break
		}
	}
	if !ok {
		if len(st.ServerIDs) == 0 {
			return nil, ErrNoGateway
		}
		return nil, fmt.Errorf("%w: server %s 不在网关主机列表内", ErrNotFound, serverID)
	}
	return readNginxConfFiles(ctx, s.tg, st, serverID)
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
	if len(st.ServerIDs) == 0 {
		return ErrNoGateway
	}
	// 逐台移除:单台失败不阻断其余台,聚合报错。
	var errs []string
	for _, sid := range st.ServerIDs {
		if err := removeNginx(ctx, s.tg, st, sid); err != nil {
			errs = append(errs, sid+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w:%s", ErrApply, strings.Join(errs, "; "))
	}
	return nil
}

// GatewayImageUpgrade 是单台网关主机的镜像升级结果。
type GatewayImageUpgrade struct {
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	OK         bool   `json:"ok"`
	Changed    bool   `json:"changed"` // 是否拉到新镜像/换了引用并重建了容器(false = 已是最新,未动容器)
	Error      string `json:"error,omitempty"`
}

// GatewayImageUpgradeResult 是 UpgradeGatewayImage 的聚合结果(逐台独立成败)。
type GatewayImageUpgradeResult struct {
	Image   string                `json:"image"` // 当前期望镜像引用
	Results []GatewayImageUpgrade `json:"results"`
}

// UpgradeGatewayImage 升级全部网关主机的 nginx 容器镜像。逐台:容器未部署 → ok:false(请改用
// 「部署网关」),不阻断其余台;docker pull 期望引用 → 与容器实际镜像 ID 比对(另:容器创建
// 引用与期望不符也算 changed,覆盖「改了镜像设置没重建」的场景)—— 无变化标 changed=false 收工;
// 有变化的台先 rm 旧容器,再统一走 applyWith 声明式收敛(ensureNginx 按新镜像重建 + 全量下发
// 配置/证书;配置/证书在具名卷,重建不丢)。未配置网关主机 → ErrNoGateway。
func (s *service) UpgradeGatewayImage(ctx context.Context) (*GatewayImageUpgradeResult, error) {
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return nil, err
	}
	if len(st.ServerIDs) == 0 {
		return nil, ErrNoGateway
	}
	image := nginxImageRef(st)
	res := &GatewayImageUpgradeResult{Image: image}
	// 阶段一:逐台 pull + 比对(不动容器)。
	for _, sid := range st.ServerIDs {
		item := GatewayImageUpgrade{ServerID: sid, OK: true}
		if srv, gerr := s.tg.Get(ctx, sid); gerr == nil && srv != nil {
			item.ServerName = srv.Name
		}
		chg, uerr := s.pullGatewayImageCheck(ctx, st, sid, image)
		if uerr != nil {
			item.OK = false
			item.Error = uerr.Error()
		} else {
			item.Changed = chg
		}
		res.Results = append(res.Results, item)
	}
	// 阶段二:有变化的台 rm 旧容器(配置/证书在具名卷,不丢)。
	needApply := false
	for i := range res.Results {
		item := &res.Results[i]
		if !item.OK || !item.Changed {
			continue
		}
		if rerr := removeNginx(ctx, s.tg, st, item.ServerID); rerr != nil {
			item.OK = false
			item.Changed = false
			item.Error = "移除旧容器失败:" + rerr.Error()
			continue
		}
		needApply = true
	}
	// 阶段三:统一全量收敛(被 rm 的台重建新容器,其余台幂等不动),逐台错误回填。
	if needApply {
		serverErrs := s.applyWith(ctx, st)
		s.ReconcileForwardRoutes(ctx)
		if len(serverErrs) > 0 {
			_ = s.store.setApplyResult(ctx, time.Now().UTC(), aggregateApplyErrors(st.ServerIDs, serverErrs), serverErrs)
		} else {
			_ = s.store.setApplyResult(ctx, time.Now().UTC(), "", nil)
		}
		for i := range res.Results {
			item := &res.Results[i]
			if item.OK && item.Changed && serverErrs[item.ServerID] != "" {
				item.OK = false
				item.Error = "重建/收敛失败:" + serverErrs[item.ServerID]
			}
		}
	}
	return res, nil
}

// pullGatewayImageCheck 在单台网关主机上 pull 期望镜像并判定是否需要重建容器:
// 容器未部署/pull 失败 → error;容器实际镜像 ID 与 pull 后本地镜像 ID 不同,或容器创建引用
// 与期望引用不符 → changed=true。
func (s *service) pullGatewayImageCheck(ctx context.Context, st *Settings, serverID, image string) (bool, error) {
	// 容器当前实际镜像 ID(容器不存在 → inspect 非零退出)。
	oldRes, err := s.tg.Exec(ctx, serverID, []string{"docker", "inspect", "-f", "{{.Image}}", st.ContainerName})
	if err != nil {
		return false, err
	}
	if oldRes.ExitCode != 0 {
		return false, errors.New("该主机尚未部署网关容器,请先用「部署网关」")
	}
	oldID := strings.TrimSpace(oldRes.Stdout)
	curRef, err := inspectContainerImageRef(ctx, s.tg, st, serverID)
	if err != nil {
		return false, err
	}
	pull, err := s.tg.Exec(ctx, serverID, []string{"docker", "pull", image})
	if err != nil {
		return false, err
	}
	if pull.ExitCode != 0 {
		return false, fmt.Errorf("拉取镜像失败:%s", strings.TrimSpace(firstNonEmpty(pull.Stderr, pull.Stdout)))
	}
	newRes, err := s.tg.Exec(ctx, serverID, []string{"docker", "image", "inspect", "-f", "{{.Id}}", image})
	if err != nil {
		return false, err
	}
	if newRes.ExitCode != 0 {
		return false, fmt.Errorf("读取本地镜像 ID 失败:%s", strings.TrimSpace(firstNonEmpty(newRes.Stderr, newRes.Stdout)))
	}
	return strings.TrimSpace(newRes.Stdout) != oldID || curRef != image, nil
}

func (s *service) Apply(ctx context.Context) error {
	return s.applyFull(ctx)
}

// applyBestEffort:网关未配置(主机列表空)时静默跳过(配置态);否则逐台全量收敛并把结果
// 记回 settings.last_apply_*(成功清错,失败记因)。返回 error 供调用方决定回滚策略。
func (s *service) applyBestEffort(ctx context.Context) error {
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return err
	}
	if len(st.ServerIDs) == 0 {
		return nil
	}
	serverErrs := s.applyWith(ctx, st)
	// 转发路由随 apply 对账一次(发现循环节拍兜底;无 forward 服务时零开销)。
	s.ReconcileForwardRoutes(ctx)
	if len(serverErrs) > 0 {
		msg := aggregateApplyErrors(st.ServerIDs, serverErrs)
		_ = s.store.setApplyResult(ctx, time.Now().UTC(), msg, serverErrs)
		return fmt.Errorf("%w:%s", ErrApply, msg)
	}
	_ = s.store.setApplyResult(ctx, time.Now().UTC(), "", nil)
	return nil
}

// applyFull 是显式编排入口(DeployGateway / Apply):未配置网关主机 → ErrNoGateway。
func (s *service) applyFull(ctx context.Context) error {
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return err
	}
	if len(st.ServerIDs) == 0 {
		return ErrNoGateway
	}
	serverErrs := s.applyWith(ctx, st)
	// 转发路由随 apply 对账一次(发现循环节拍兜底;无 forward 服务时零开销)。
	s.ReconcileForwardRoutes(ctx)
	if len(serverErrs) > 0 {
		msg := aggregateApplyErrors(st.ServerIDs, serverErrs)
		_ = s.store.setApplyResult(ctx, time.Now().UTC(), msg, serverErrs)
		return fmt.Errorf("%w:%s", ErrApply, msg)
	}
	_ = s.store.setApplyResult(ctx, time.Now().UTC(), "", nil)
	return nil
}

// applyWith 集群级收敛:静态配置读一次 → 解析全部实例归属服务器地址 → 渲染**一套**全量
// 配置(主配置 + conf.d/ 每服务文件;全部网关主机一致,本机无实例同样代理远端)→ 逐台
// (ensure 容器 + TCP 上游接网 + 下发证书 + 校验下发热加载)。单台失败记录并继续,返回
// 逐台错误(serverID → 文案;nil = 全部成功)。
func (s *service) applyWith(ctx context.Context, st *Settings) map[string]string {
	domains, err := s.store.listDomains(ctx)
	if err != nil {
		return mapErrAll(st.ServerIDs, err)
	}
	services, err := s.store.listServices(ctx)
	if err != nil {
		return mapErrAll(st.ServerIDs, err)
	}
	allInstances, err := s.store.listAllInstances(ctx)
	if err != nil {
		return mapErrAll(st.ServerIDs, err)
	}
	var enabled []RegisteredService
	var tcpPorts []int
	for i := range services {
		svc := &services[i]
		if !svc.Enabled {
			continue
		}
		enabled = append(enabled, *svc)
		if svc.Protocol == ProtocolTCP {
			tcpPorts = append(tcpPorts, svc.TCPListenPort)
		}
	}

	// 实例归属服务器的可达地址(逐台网关反代集群实例的地基)。解析失败/遗留行(server_id='')
	// 的实例跳过渲染(渲染注释标注),故障实例交给 upstream max_fails 熔断,不阻断 apply。
	serverAddr, skipped := s.resolveInstanceAddrs(ctx, allInstances)
	// forward 服务:拉取引用对端的服务目录 → 解析渲染成员(目录不可达/IP 冲突 → 503 维护态)。
	plans, conflicts := s.buildForwardPlans(ctx, enabled)
	// 同一产物渲染两份主配置:live 的 include 指向正式 conf.d,test 指向下发中转目录
	// (nginx -t 先测后换的沙箱;conf.d 文件内容两者完全一致)。
	confLive := renderNginxConf(nginxConfDir, domains, enabled, allInstances, serverAddr, skipped, plans, conflicts)
	confTest := renderNginxConf(nginxStageDir+"/conf.d", domains, enabled, allInstances, serverAddr, skipped, plans, conflicts)

	serverErrs := make(map[string]string)
	for _, sid := range st.ServerIDs {
		if err := s.applyWithServer(ctx, st, sid, tcpPorts, enabled, domains, confLive, confTest); err != nil {
			serverErrs[sid] = err.Error()
		}
	}
	if len(serverErrs) == 0 {
		return nil
	}
	return serverErrs
}

// applyWithServer 是单台网关主机的收敛流程(applyWith 逐台调用;返回人话错误)。
func (s *service) applyWithServer(ctx context.Context, st *Settings, serverID string, tcpPorts []int, enabled []RegisteredService, domains []Domain, confLive, confTest *nginxConfRender) error {
	if err := ensureNginx(ctx, s.tg, st, serverID, tcpPorts); err != nil {
		return err
	}
	// tcp 服务的容器上游仍是单一 Upstream(经共享网络按容器名解析,实例化仅覆盖 http)。
	for _, svc := range enabled {
		if svc.Protocol == ProtocolTCP && svc.UpstreamKind == UpstreamKindContainer && svc.Upstream != "" {
			if err := connectUpstream(ctx, s.tg, st, serverID, svc.Upstream); err != nil {
				return err
			}
		}
	}
	if err := s.deployCerts(ctx, st, serverID, domains); err != nil {
		return err
	}
	return applyNginxConf(ctx, s.tg, st, serverID, confLive, confTest)
}

// resolveInstanceAddrs 解析实例引用到的全部服务器地址(serverID → 注册原始地址:域名/
// IP 原样,域名成员由网关容器 DNS 在 reload 时解析)。返回可解析映射 + 被跳过的 attached
// 实例数(遗留行 server_id=” / 地址解析失败 / 容器实例未发布宿主端口;detached 实例本就
// 不参与渲染,不计入)。未注入 serverHost 时回落 serverAddr(兼容旧装配/单测)。
func (s *service) resolveInstanceAddrs(ctx context.Context, instances []Instance) (map[string]string, int) {
	ids := make(map[string]struct{})
	for _, inst := range instances {
		if !inst.Attached {
			continue
		}
		if inst.ServerID != "" {
			ids[inst.ServerID] = struct{}{}
		}
	}
	resolve := s.serverHost
	if resolve == nil {
		resolve = s.serverAddr
	}
	resolved := make(map[string]string, len(ids))
	if resolve != nil {
		for id := range ids {
			if addr, err := resolve(ctx, id); err == nil && addr != "" {
				resolved[id] = addr
			}
		}
	}
	skipped := 0
	for _, inst := range instances {
		if !inst.Attached {
			continue
		}
		if _, ok := resolved[inst.ServerID]; !ok {
			skipped++
			continue
		}
		if inst.Container != "" && inst.HostPort <= 0 {
			skipped++ // 容器实例未发布宿主端口:渲染侧同步跳过
		}
	}
	return resolved, skipped
}

// aggregateApplyErrors 按 ServerIDs 顺序聚合成 "serverID: err" 分号串(不改变可读性)。
func aggregateApplyErrors(ids []string, errs map[string]string) string {
	var parts []string
	for _, sid := range ids {
		if e, ok := errs[sid]; ok {
			parts = append(parts, sid+": "+e)
		}
	}
	// 防御:错误里出现列表外 id(理论不可达)也带出。
	extra := make([]string, 0)
	for sid := range errs {
		found := false
		for _, id := range ids {
			if id == sid {
				found = true
				break
			}
		}
		if !found {
			extra = append(extra, sid+": "+errs[sid])
		}
	}
	parts = append(parts, extra...)
	return strings.Join(parts, "; ")
}

// mapErrAll 把单点错误展开为逐台错误(全局配置读取失败时所有台同因)。
func mapErrAll(ids []string, err error) map[string]string {
	out := make(map[string]string, len(ids))
	for _, sid := range ids {
		out[sid] = err.Error()
	}
	return out
}

// normalizeServerIDs 归一化主机列表:去空白、去空项、去重(保序)。
func normalizeServerIDs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------- 校验 ----------

func validateBaseDomain(d string) error {
	if d == "" || len(d) > 253 || !baseDomainRe.MatchString(d) {
		return ErrInvalidBaseDomain
	}
	return nil
}

func validateSettings(st Settings) error {
	// 主机列表:非空项 ≤64 字符(server_id 列宽),且不重复。
	seen := make(map[string]struct{}, len(st.ServerIDs))
	for _, sid := range st.ServerIDs {
		if sid == "" || len(sid) > 64 {
			return ErrInvalidSetting
		}
		if _, dup := seen[sid]; dup {
			return ErrInvalidSetting
		}
		seen[sid] = struct{}{}
	}
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
	// 静态根目录:绝对路径,无尾斜杠,路径安全字符集(bind mount 目标;渲染不回显进配置,
	// 仅部署侧拼 <root>/<serviceID>,serviceID 为 uuid,无注入面)。
	if st.StaticRoot == "" || len(st.StaticRoot) > 255 || !staticRootRe.MatchString(st.StaticRoot) {
		return ErrInvalidSetting
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

// normalizeServicePath 归一化子路径:去空白;无前导 / 自动补;去尾 /;根路径("/")与空串 → ""
// (存储与冲突比较的规范形态)。非法字符集/过长/"//"/"."/".." 段 → ErrInvalidPath。
func normalizeServicePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "", nil // 全斜杠输入归一根路径
	}
	if len(p) > 128 || !servicePathRe.MatchString(p) || strings.Contains(p, "//") {
		return "", ErrInvalidPath
	}
	for i, seg := range strings.Split(p, "/") {
		if i == 0 {
			continue // 前导 / 产生的空段
		}
		if seg == "." || seg == ".." {
			return "", ErrInvalidPath
		}
	}
	return p, nil
}

// DisplayPath 返回路由/展示用的 path(” → "/")。
func (rs RegisteredService) DisplayPath() string {
	if rs.Path == "" {
		return "/"
	}
	return rs.Path
}

// FQDN 返回服务的完整域名(供展示;http 服务即访问地址)。
func (rs RegisteredService) FQDN(base string) string { return rs.Name + "." + base }

// FQDNs 返回服务的全部域名(主 FQDN 在前,别名 FQDN 随后;展示/DTO 直显)。
func (rs RegisteredService) FQDNs(base string) []string {
	out := make([]string, 0, 1+len(rs.Aliases))
	out = append(out, rs.FQDN(base))
	for _, a := range rs.Aliases {
		out = append(out, a+"."+base)
	}
	return out
}

// maxServiceAliases 是单个服务的别名数量上限(防滥用;渲染为单行 server_name)。
const maxServiceAliases = 10

// normalizeServiceAliases 归一化别名列表:逐个小写去空白、复用 serviceNameRe 校验
// (与主名同字符集)、去重、禁止与主名相同;空串项静默丢弃(前端逗号分隔输入的尾逗号)。
func normalizeServiceAliases(name string, raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw)+1)
	for _, a := range raw {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if a == name || seen[a] {
			return nil, ErrInvalidServiceName
		}
		if !serviceNameRe.MatchString(a) {
			return nil, ErrInvalidServiceName
		}
		seen[a] = true
		out = append(out, a)
	}
	if len(out) > maxServiceAliases {
		return nil, ErrInvalidServiceName
	}
	return out, nil
}

// maxCertPEMBytes 是单份证书/私钥 PEM 的尺寸上限(容 RSA-4096 链)。
const maxCertPEMBytes = 64 << 10
