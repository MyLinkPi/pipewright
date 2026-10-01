// Package dnsprovider 是「DNS 提供商集成层」(R3 E3.1–E3.4)的领域层。
//
// 一个 Provider 代表一个厂商账户(Cloudflare / DNSPod / 阿里云),可托管多个根区(Zone):
// 现实中一个账户同时管理多个域名是常态,故根区独立成表(一对多),而非账户上单个 base_domain 列。
//
// 凭据按机密性拆开存储:
//   - API ID(DNSPod SecretId / 阿里云 AccessKeyId)非机密 → dns_providers.api_id 明文列,可回显可编辑;
//   - Secret(DNSPod Token / 阿里云 AccessKeySecret / Cloudflare API Token)→ vault 凭据加密存储
//     (credential_id 引用),绝不入库明文/日志/响应/错误体。
//
// 能力:
//   - DNSClient 抽象「为某域建/改 A 记录」「校验某 zone 可管理」(注入便于单测,不触真网络)。
//     Cloudflare / DNSPod / 阿里云 DNS 三家均经 net/http 直连各自 API 真实实现(无 SDK):
//     Cloudflare 用 v4 REST(Bearer token);DNSPod 用经典 dnsapi.cn form-POST(login_token=id,token);
//     阿里云用 RPC v1(HMAC-SHA1 签名,凭据 accessKeyId,accessKeySecret)。
//     三家的 DNS-01 通配符证书签发也都能用(走 Caddy 镜像内的 DNS 插件,与自动建 A 记录正交)。
//   - AllocateSubdomain:瞬时挑一个可读子域名 app-<6char>.<zone 根域> → 自动建 A 记录指向宿主机
//     公网 IP → 创建一条 DNS-01 的反代路由(证书即刻可签)。
//
// AC-SEC:写入解析的 IP 是宿主机的(由调用方传入,非用户自由文本);zone/name 服务端严格校验;
// 命令 / API 入参严格归一,杜绝注入。
package dnsprovider

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

// 提供商类型枚举(DB 存小写字串;JSON 同名)。
const (
	// TypeCloudflare 是 Cloudflare DNS(自动 A 记录已真实实现;单 API Token,无 API ID)。
	TypeCloudflare = "cloudflare"
	// TypeDNSPod 是腾讯云 DNSPod(自动 A 记录已真实实现;API ID = SecretId)。
	TypeDNSPod = "dnspod"
	// TypeAliDNS 是阿里云 DNS(自动 A 记录已真实实现;API ID = AccessKeyId)。
	TypeAliDNS = "alidns"
)

// 领域错误(人话由 httpapi 层映射状态码;错误体绝不含敏感信息)。
var (
	// ErrNotFound 表示 DNS 提供商不存在。
	ErrNotFound = errors.New("dnsprovider: provider not found")
	// ErrInvalidType 表示提供商类型枚举非法。
	ErrInvalidType = errors.New("dnsprovider: invalid provider type")
	// ErrEmptyName 表示展示名为空。
	ErrEmptyName = errors.New("dnsprovider: name must not be empty")
	// ErrEmptyCredentialID 表示未选择凭据。
	ErrEmptyCredentialID = errors.New("dnsprovider: credential id must not be empty")
	// ErrInvalidBaseDomain 表示根域格式非法。
	ErrInvalidBaseDomain = errors.New("dnsprovider: invalid base domain")
	// ErrNoBaseDomains 表示登记时未提供任何根区。
	ErrNoBaseDomains = errors.New("dnsprovider: at least one base domain is required")
	// ErrZoneNotFound 表示根区不存在(或不属于该提供商)。
	ErrZoneNotFound = errors.New("dnsprovider: zone not found")
	// ErrCredentialNotFound 表示引用的凭据不存在。
	ErrCredentialNotFound = errors.New("dnsprovider: referenced credential not found")
	// ErrVaultUnconfigured 表示保险库未配置 master key,无法取 DNS secret。
	ErrVaultUnconfigured = errors.New("dnsprovider: vault unconfigured")

	// ErrProviderNotImplemented 表示该提供商的某能力(如自动建 A 记录)尚未实现。
	// 注意:DNS-01 通配符证书签发对三种提供商都可用(走 Caddy 镜像内 DNS 插件),不受此影响。
	ErrProviderNotImplemented = errors.New("dnsprovider: provider capability not implemented")
	// ErrInvalidAPIID 表示 API ID 与提供商类型不匹配(dnspod/alidns 必填、cloudflare 须为空)。
	// API ID 非机密,但格式约束仍需服务端把关。
	ErrInvalidAPIID = errors.New("dnsprovider: invalid api id for provider type")
	// ErrInvalidCredential 表示 Secret 为空或与提供商类型不匹配。错误体绝不含凭据任何片段。
	ErrInvalidCredential = errors.New("dnsprovider: invalid provider credential")
	// ErrVerifyFailed 表示 zone 校验失败(secret 无效 / 无该 zone 权限 / API 不可达)。
	ErrVerifyFailed = errors.New("dnsprovider: zone verification failed")
	// ErrEnsureRecord 表示建/改 A 记录失败。
	ErrEnsureRecord = errors.New("dnsprovider: ensure A record failed")
	// ErrDeleteRecord 表示删除 A 记录失败(回收预览/子域名时清 DNS)。
	ErrDeleteRecord = errors.New("dnsprovider: delete A record failed")
	// ErrAllocate 表示子域名分配失败(多次随机仍冲突 / 建记录失败 / 建路由失败)。
	ErrAllocate = errors.New("dnsprovider: allocate subdomain failed")
)

// Provider 是一个 DNS 提供商账户的领域模型。绝不含 Secret 明文,只持 CredentialID 引用;
// APIID 非机密(DNSPod SecretId / 阿里云 AccessKeyId;Cloudflare 恒为空)。
type Provider struct {
	ID           string
	Type         string // cloudflare | dnspod | alidns
	Name         string
	APIID        string // 非机密:ID 类凭据半段,明文存储可回显
	CredentialID string // 指向 vault 凭据(只存 Secret 半段);明文绝不入此结构体
	Zones        []Zone // 该账户托管的根区(一对多)
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Zone 是一个 DNS 根区:某提供商账户托管的一个根域(如 example.com)。
type Zone struct {
	ID         string
	ProviderID string
	BaseDomain string
	CreatedAt  time.Time
}

// CreateInput 是登记 DNS 提供商的入参(至少一个合法根域)。
type CreateInput struct {
	Type         string
	Name         string
	APIID        string
	CredentialID string
	BaseDomains  []string
}

// UpdateInput 是编辑 DNS 提供商的入参;指针字段为 nil 表示不修改(Secret 轮换由 httpapi
// 直接经 vault.Update 完成,credentialId 不变,不经此入参)。
type UpdateInput struct {
	Name  *string
	APIID *string
}

// DNSClient 抽象一个具体提供商的 DNS 写/校验能力(注入便于单测,不触真网络)。
// EnsureARecord 建或就地 upsert 一条 A 记录(name → ip);VerifyZone 校验凭据可管理该 zone。
type DNSClient interface {
	// EnsureARecord 为 zone(根域)下的 name(FQDN)建/改 A 记录指向 ip(幂等 upsert)。
	EnsureARecord(ctx context.Context, zone, name, ip string) error
	// DeleteARecord 删除 zone(根域)下 name(FQDN)的 A 记录(回收子域名时清 DNS;幂等:无记录视为成功)。
	DeleteARecord(ctx context.Context, zone, name string) error
	// VerifyZone 校验当前凭据可管理 zone(根域),失败返回人话错误(绝不含凭据)。
	VerifyZone(ctx context.Context, zone string) error
}

// ZoneVerifyResult 是 Verify 逐根区的校验结果(Err 为 nil 表示通过;人话错误绝不含凭据)。
type ZoneVerifyResult struct {
	ZoneID     string
	BaseDomain string
	Err        error
}

// Service 定义 DNS 提供商领域对外接口(httpapi 消费)。
type Service interface {
	// List 返回全部 DNS 提供商(创建时间倒序,含各自根区;无 Secret)。
	List(ctx context.Context) ([]Provider, error)
	// Get 返回单个提供商(含根区;无 Secret);不存在 → ErrNotFound。
	Get(ctx context.Context, id string) (*Provider, error)
	// Create 校验入参(含凭据存在性)→ 落库(提供商 + 初始根区);返回提供商视图(无 Secret)。
	Create(ctx context.Context, in CreateInput) (*Provider, error)
	// Update 编辑展示名 / API ID(Secret 轮换走 vault,不经此);返回更新后视图。
	Update(ctx context.Context, id string, in UpdateInput) (*Provider, error)
	// Delete 删除提供商(连同其根区);不存在 → ErrNotFound。
	Delete(ctx context.Context, id string) error

	// AddZone 给提供商追加一个根区;baseDomain 非法或已存在 → 相应错误。
	AddZone(ctx context.Context, providerID, baseDomain string) (*Zone, error)
	// RemoveZone 删除提供商的某个根区(zoneID 不属于该提供商 → ErrZoneNotFound)。
	RemoveZone(ctx context.Context, providerID, zoneID string) error

	// Verify 取该提供商凭据 → 逐根区经 DNSClient 校验可管理,返回每个根区的结果。
	// Secret 用完即弃,绝不外泄。提供商无根区 → ErrZoneNotFound。
	Verify(ctx context.Context, id string) ([]ZoneVerifyResult, error)

	// DeleteSubdomainRecord 删除该提供商**某个根区**下 fqdn 的 A 记录(回收预览/子域名时清 DNS;
	// 按最长后缀匹配选根区;幂等)。找不到覆盖根区 → ErrZoneNotFound。
	DeleteSubdomainRecord(ctx context.Context, providerID, fqdn string) error
	// AllocateSubdomain 瞬时分配子域名(E3.3 + E3.4):在 in.ZoneID 根区下挑 app-<6char>.<根域> →
	// 建 A 记录指向 hostIP → 创建一条 DNS-01 反代路由(证书即刻可签)。返回新建的反代路由。
	AllocateSubdomain(ctx context.Context, in AllocateInput) (*RouteRef, error)

	// AllocateFQDN 为一个**指定的** FQDN(in.Subdomain,须落在该提供商某个根区下)建 A 记录指向
	// hostIP → 创建一条 DNS-01 反代路由(R4 E4.1 预览环境用:子域名确定而非随机)。返回新建路由引用。
	// in.Subdomain 缺省/不在任何根区下 → ErrAllocate。
	AllocateFQDN(ctx context.Context, in AllocateInput) (*RouteRef, error)

	// ProviderType 返回某提供商的类型(供 proxy 校验 DNS-01 引用,不取 Secret)。
	// 不存在 → ok=false。供 proxy.DNSResolver 适配。
	ProviderType(ctx context.Context, providerID string) (providerType string, ok bool, err error)
	// Zones 返回某提供商的全部根区(供 proxy 通配符覆盖校验 / previewenv 配置校验,不取 Secret)。
	// 不存在 → ok=false。供 proxy.DNSResolver 适配。
	Zones(ctx context.Context, providerID string) (zones []Zone, ok bool, err error)
	// ZoneCovers 报告 fqdn 是否落在某提供商托管的某个根区下(fqdn == 根区或为其后缀,最长后缀
	// 优先;不取 Secret)。提供商不存在 → ok=false。供 previewenv 配置校验等消费。
	ZoneCovers(ctx context.Context, providerID, fqdn string) (covered, ok bool, err error)

	// ResolveCredential 取某提供商的 (类型, API ID, Secret 明文)(供 proxy 在 apply 时渲染 DNS-01)。
	// Secret 仅供调用方即时注入 0600 临时 Caddyfile,绝不日志/回库/回 API。
	// 不存在 → ok=false;vault 未配/凭据缺失 → err。供 proxy.DNSResolver 适配。
	ResolveCredential(ctx context.Context, providerID string) (providerType, apiID, secret string, ok bool, err error)
}

// AllocateInput 是瞬时子域名分配的入参。AllocateSubdomain 用 ZoneID(在该根区下随机挑);
// AllocateFQDN 用 ProviderID + Subdomain(指定 FQDN 按最长后缀匹配根区)。
type AllocateInput struct {
	// ZoneID 是 AllocateSubdomain 的目标根区(随机子域名建在它下面)。
	ZoneID string
	// ProviderID 是 AllocateFQDN 的目标提供商(Subdomain 须落在其某个根区下)。
	ProviderID string
	ServerID          string
	UpstreamContainer string
	UpstreamPort      int
	// HostIP 是宿主机公网 IP(由调用方/上层据 server 解析得出,非用户自由文本)。写入 A 记录指向它。
	HostIP string
	// Subdomain 是 AllocateFQDN 用的**指定** FQDN(如 pr-12-abcd.preview.example.com);
	// AllocateSubdomain 不用此字段(它随机挑后缀)。须落在提供商某个根区下(后缀匹配校验)。
	Subdomain string
}

// RouteRef 是 AllocateSubdomain 创建出的反代路由引用(避免本包 import proxy 形成环;
// 由上层用其 RouteID 取完整 proxy.Route DTO)。
type RouteRef struct {
	RouteID    string
	Domain     string
	ProviderID string
}

// baseDomainRe 校验根域 FQDN(每段字母数字/连字符,段间点分,顶级域 ≥2 字母)。不接受裸 IP / 端口 / 通配符。
var baseDomainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// ValidType 报告提供商类型是否为受支持枚举(供 proxy 校验 DNS-01 引用时复用)。
func ValidType(t string) bool {
	switch t {
	case TypeCloudflare, TypeDNSPod, TypeAliDNS:
		return true
	default:
		return false
	}
}

// validAPIID 报告 API ID 是否与提供商类型匹配:dnspod/alidns 必填,cloudflare 须为空(单 Token 无 ID 半段)。
func validAPIID(providerType, apiID string) bool {
	apiID = strings.TrimSpace(apiID)
	switch providerType {
	case TypeDNSPod, TypeAliDNS:
		return apiID != ""
	case TypeCloudflare:
		return apiID == ""
	default:
		return false
	}
}

// validateCreate 校验登记入参(类型 / 展示名 / 凭据引用 / API ID / ≥1 个合法根区,根区去重)。
func validateCreate(in CreateInput) error {
	if !ValidType(in.Type) {
		return ErrInvalidType
	}
	if strings.TrimSpace(in.Name) == "" {
		return ErrEmptyName
	}
	if strings.TrimSpace(in.CredentialID) == "" {
		return ErrEmptyCredentialID
	}
	if !validAPIID(in.Type, in.APIID) {
		return ErrInvalidAPIID
	}
	if len(in.BaseDomains) == 0 {
		return ErrNoBaseDomains
	}
	for _, d := range in.BaseDomains {
		if !ValidBaseDomain(d) {
			return ErrInvalidBaseDomain
		}
	}
	return nil
}

// ValidBaseDomain 报告 s 是否为合法根域 FQDN。
func ValidBaseDomain(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s != "" && len(s) <= 253 && baseDomainRe.MatchString(s)
}
