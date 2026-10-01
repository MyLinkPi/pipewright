package dnsprovider

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/huangchengsir/pipewright/internal/vault"
)

// vaultReader 抽象「按 id 取凭据明文(不刷新 last_used_at)+ 校验存在」的最小能力(注入便于单测)。
// 复用 vault.Vault 的 Reveal/Exists;本包只读取 DNS Secret 明文,绝不入库/日志/响应。
type vaultReader interface {
	Reveal(id string) (string, error)
	Exists(id string) (bool, error)
}

// RouteCreator 抽象「创建一条 DNS-01 反代路由」的能力(由上层用 proxy.Service 适配注入,避免本包
// import proxy 形成环)。实现须落库一条带 DNS 提供商引用的路由并完成 Caddy 编排,返回 routeID。
type RouteCreator interface {
	// CreateDNS01Route 创建一条 DNS-01 反代路由(域名 + DNS 提供商引用 + 上游)。返回新路由 id。
	CreateDNS01Route(ctx context.Context, in CreateDNS01RouteInput) (routeID string, err error)
}

// CreateDNS01RouteInput 是创建 DNS-01 反代路由的入参(冻结契约,proxy 适配器消费)。
type CreateDNS01RouteInput struct {
	ServerID          string
	Domain            string
	UpstreamContainer string
	UpstreamPort      int
	DNSProviderID     string
}

// dialFactory 据提供商类型 + (API ID, Secret) 造一个 DNSClient(注入便于单测;生产用 prodDialFactory)。
type dialFactory func(providerType, apiID, secret string) DNSClient

// prodDialFactory 是生产工厂:真实 net/http 客户端(Cloudflare 真连,其余桩)。
func prodDialFactory(providerType, apiID, secret string) DNSClient {
	return newDNSClient(providerType, apiID, secret, nil, "")
}

// service 是 store + vault + routeCreator + dialFactory 支撑的 Service 实现。
type service struct {
	store   *Store
	vault   vaultReader
	routes  RouteCreator
	dial    dialFactory
	randGen func(n int) (string, error) // 随机子域名后缀生成器(注入便于单测;默认 crypto/rand)
}

// New 构造 Service。
//   - db:参数化 SQL 触库。
//   - v:凭据保险库(取 DNS Secret 明文);nil/未配置时 Verify/Allocate 返回 ErrVaultUnconfigured。
//   - routes:DNS-01 反代路由创建器(proxy 适配器);nil 时 AllocateSubdomain 返回 ErrAllocate。
//
// 不在此做任何重活(无 init 副作用)。
func New(db *sql.DB, v vault.Vault, routes RouteCreator) Service {
	var vr vaultReader
	if v != nil {
		vr = v
	}
	return &service{
		store:   NewStore(db),
		vault:   vr,
		routes:  routes,
		dial:    prodDialFactory,
		randGen: randSuffix,
	}
}

func (s *service) List(ctx context.Context) ([]Provider, error) {
	providers, err := s.store.list(ctx)
	if err != nil {
		return nil, err
	}
	byProvider, err := s.store.listZones(ctx)
	if err != nil {
		return nil, err
	}
	for i := range providers {
		providers[i].Zones = byProvider[providers[i].ID]
		if providers[i].Zones == nil {
			providers[i].Zones = []Zone{}
		}
	}
	return providers, nil
}

func (s *service) Get(ctx context.Context, id string) (*Provider, error) {
	p, err := s.store.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Zones, err = s.store.listZonesByProvider(ctx, id); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *service) Create(ctx context.Context, in CreateInput) (*Provider, error) {
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	in.Name = strings.TrimSpace(in.Name)
	in.APIID = strings.TrimSpace(in.APIID)
	in.CredentialID = strings.TrimSpace(in.CredentialID)
	for i := range in.BaseDomains {
		in.BaseDomains[i] = strings.ToLower(strings.TrimSpace(in.BaseDomains[i]))
	}
	if err := validateCreate(in); err != nil {
		return nil, err
	}
	// 校验凭据存在(避免悬挂引用);vault 未配置时仍允许登记(凭据存在性在 Verify/Allocate 时再校)。
	if s.vault != nil {
		ok, err := s.vault.Exists(in.CredentialID)
		if err != nil {
			if !errors.Is(err, vault.ErrVaultUnconfigured) {
				return nil, fmt.Errorf("dnsprovider: check credential: %w", err)
			}
		} else if !ok {
			return nil, ErrCredentialNotFound
		}
	}
	p, zones := newProvider(in)
	if err := s.store.createWithZones(ctx, p, zones); err != nil {
		return nil, err
	}
	return s.Get(ctx, p.ID)
}

func (s *service) Update(ctx context.Context, id string, in UpdateInput) (*Provider, error) {
	p, err := s.store.get(ctx, id)
	if err != nil {
		return nil, err
	}
	// API ID 与类型须匹配(dnspod/alidns 必填、cloudflare 须空);先取生效值再校验,后落库。
	apiID := p.APIID
	if in.APIID != nil {
		apiID = strings.TrimSpace(*in.APIID)
	}
	if !validAPIID(p.Type, apiID) {
		return nil, ErrInvalidAPIID
	}
	p, err = s.store.update(ctx, id, in)
	if err != nil {
		return nil, err
	}
	if p.Zones, err = s.store.listZonesByProvider(ctx, id); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *service) Delete(ctx context.Context, id string) error { return s.store.del(ctx, id) }

// AddZone 给提供商追加一个根区(格式校验 + 同提供商下去重)。
func (s *service) AddZone(ctx context.Context, providerID, baseDomain string) (*Zone, error) {
	if _, err := s.store.get(ctx, providerID); err != nil {
		return nil, err
	}
	baseDomain = strings.ToLower(strings.TrimSpace(baseDomain))
	if !ValidBaseDomain(baseDomain) {
		return nil, ErrInvalidBaseDomain
	}
	existing, err := s.store.listZonesByProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	for _, z := range existing {
		if z.BaseDomain == baseDomain {
			return nil, fmt.Errorf("%w:根域已存在", ErrInvalidBaseDomain)
		}
	}
	z := newZone(providerID, baseDomain)
	if err := s.store.insertZone(ctx, z); err != nil {
		return nil, err
	}
	return z, nil
}

// RemoveZone 删除提供商的某个根区(zoneID 不属于该提供商 → ErrZoneNotFound)。
func (s *service) RemoveZone(ctx context.Context, providerID, zoneID string) error {
	if _, err := s.store.get(ctx, providerID); err != nil {
		return err
	}
	return s.store.deleteZone(ctx, providerID, zoneID)
}

// Verify 逐根区校验凭据可管理性;提供商无根区 → ErrZoneNotFound。Secret 用完即弃,绝不外泄。
func (s *service) Verify(ctx context.Context, id string) ([]ZoneVerifyResult, error) {
	p, err := s.store.get(ctx, id)
	if err != nil {
		return nil, err
	}
	zones, err := s.store.listZonesByProvider(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(zones) == 0 {
		return nil, fmt.Errorf("%w:该提供商未配置任何根区", ErrZoneNotFound)
	}
	client, err := s.clientFor(p)
	if err != nil {
		return nil, err
	}
	results := make([]ZoneVerifyResult, 0, len(zones))
	for _, z := range zones {
		results = append(results, ZoneVerifyResult{
			ZoneID:     z.ID,
			BaseDomain: z.BaseDomain,
			Err:        client.VerifyZone(ctx, z.BaseDomain),
		})
	}
	return results, nil
}

// DeleteSubdomainRecord 删除某提供商根区下 fqdn 的 A 记录(回收预览/子域名时清 DNS)。
// 根区按最长后缀匹配选取。幂等:无记录视为成功。providerID 不存在 → ErrNotFound;
// 无覆盖根区 → ErrZoneNotFound;vault 未配/凭据缺失 → 相应错误。
func (s *service) DeleteSubdomainRecord(ctx context.Context, providerID, fqdn string) error {
	p, err := s.store.get(ctx, providerID)
	if err != nil {
		return err
	}
	zones, err := s.store.listZonesByProvider(ctx, providerID)
	if err != nil {
		return err
	}
	zone := zoneCovering(zones, fqdn)
	if zone == nil {
		return fmt.Errorf("%w:%s 不在该提供商任何根区下", ErrZoneNotFound, fqdn)
	}
	client, err := s.clientFor(p)
	if err != nil {
		return err
	}
	return client.DeleteARecord(ctx, zone.BaseDomain, fqdn)
}

// ProviderType 返回某提供商类型(不取 Secret);不存在 → ok=false。
func (s *service) ProviderType(ctx context.Context, providerID string) (string, bool, error) {
	p, err := s.store.get(ctx, providerID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	return p.Type, true, nil
}

// Zones 返回某提供商的全部根区(不取 Secret);不存在 → ok=false。
func (s *service) Zones(ctx context.Context, providerID string) ([]Zone, bool, error) {
	if _, err := s.store.get(ctx, providerID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	zones, err := s.store.listZonesByProvider(ctx, providerID)
	if err != nil {
		return nil, false, err
	}
	return zones, true, nil
}

// ZoneCovers 报告 fqdn 是否落在某提供商托管的某个根区下(最长后缀优先);不存在 → ok=false。
func (s *service) ZoneCovers(ctx context.Context, providerID, fqdn string) (bool, bool, error) {
	if _, err := s.store.get(ctx, providerID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, false, nil
		}
		return false, false, err
	}
	zones, err := s.store.listZonesByProvider(ctx, providerID)
	if err != nil {
		return false, false, err
	}
	return zoneCovering(zones, fqdn) != nil, true, nil
}

// ResolveCredential 取某提供商 (类型, API ID, Secret 明文)(供 proxy apply 渲染 DNS-01)。
// Secret 用完即弃。
func (s *service) ResolveCredential(ctx context.Context, providerID string) (string, string, string, bool, error) {
	p, err := s.store.get(ctx, providerID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", "", "", false, nil
		}
		return "", "", "", false, err
	}
	if s.vault == nil {
		return "", "", "", false, ErrVaultUnconfigured
	}
	secret, err := s.vault.Reveal(p.CredentialID)
	if err != nil {
		switch {
		case errors.Is(err, vault.ErrVaultUnconfigured):
			return "", "", "", false, ErrVaultUnconfigured
		case errors.Is(err, vault.ErrNotFound):
			return "", "", "", false, ErrCredentialNotFound
		default:
			return "", "", "", false, ErrVerifyFailed
		}
	}
	return p.Type, p.APIID, secret, true, nil
}

// clientFor 取该提供商凭据明文 → 造对应 DNSClient。Secret 明文仅进程内,调用方用完即弃。
func (s *service) clientFor(p *Provider) (DNSClient, error) {
	if s.vault == nil {
		return nil, ErrVaultUnconfigured
	}
	secret, err := s.vault.Reveal(p.CredentialID)
	if err != nil {
		switch {
		case errors.Is(err, vault.ErrVaultUnconfigured):
			return nil, ErrVaultUnconfigured
		case errors.Is(err, vault.ErrNotFound):
			return nil, ErrCredentialNotFound
		default:
			// 解密等内部错误:不泄漏细节。
			return nil, ErrVerifyFailed
		}
	}
	client := s.dial(p.Type, p.APIID, secret)
	// 显式清本地 secret 引用(client 已持有副本用于建连)。
	secret = "" //nolint:ineffassign // 安全归零意图
	_ = secret
	return client, nil
}

// AllocateSubdomain 瞬时分配子域名(E3.3 + E3.4):
//  1. 取目标根区 + 其提供商,校验入参(上游容器/端口、宿主机 IP)。
//  2. crypto/rand 挑可读后缀,组成 app-<6char>.<根域>;若与已建路由域名冲突则重试(有限次)。
//  3. 经 DNSClient 建 A 记录指向 hostIP(Cloudflare 真实;其余桩返回未实现)。
//  4. 经 RouteCreator 创建一条 DNS-01 反代路由(证书即刻可签;绑定提供商级 DNS-01 引用)。
func (s *service) AllocateSubdomain(ctx context.Context, in AllocateInput) (*RouteRef, error) {
	if s.routes == nil {
		return nil, fmt.Errorf("%w:路由创建器未装配", ErrAllocate)
	}
	zone, err := s.store.getZone(ctx, in.ZoneID)
	if err != nil {
		return nil, err
	}
	p, err := s.store.get(ctx, zone.ProviderID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.ServerID) == "" {
		return nil, ErrAllocate
	}
	if strings.TrimSpace(in.UpstreamContainer) == "" {
		return nil, ErrAllocate
	}
	if in.UpstreamPort < 1 || in.UpstreamPort > 65535 {
		return nil, ErrAllocate
	}
	if !validIPv4(in.HostIP) {
		// 宿主机 IP 必须是合法 IPv4(由上层据 server.Host 解析得出,非用户自由文本)。
		return nil, fmt.Errorf("%w:宿主机 IP 非法", ErrAllocate)
	}

	client, err := s.clientFor(p)
	if err != nil {
		return nil, err
	}

	// 有限次随机挑子域名,避开与已建路由域名冲突(由 RouteCreator 的 domain 唯一约束兜底)。
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		suffix, gerr := s.randGen(6)
		if gerr != nil {
			return nil, fmt.Errorf("%w:随机子域名生成失败", ErrAllocate)
		}
		domain := "app-" + suffix + "." + zone.BaseDomain

		// 建 A 记录指向宿主机 IP(幂等)。确定性错误(未实现 / 凭据缺失)直接上抛,不重试;
		// 其余(瞬时网络 / API)记为 lastErr 后换下一个后缀重试。
		if err := client.EnsureARecord(ctx, zone.BaseDomain, domain, in.HostIP); err != nil {
			if errors.Is(err, ErrProviderNotImplemented) || errors.Is(err, ErrInvalidCredential) {
				return nil, err
			}
			lastErr = err
			continue
		}

		// 创建 DNS-01 反代路由。domain 唯一冲突(极小概率随机撞车)→ 重试下一个后缀。
		routeID, rerr := s.routes.CreateDNS01Route(ctx, CreateDNS01RouteInput{
			ServerID:          in.ServerID,
			Domain:            domain,
			UpstreamContainer: in.UpstreamContainer,
			UpstreamPort:      in.UpstreamPort,
			DNSProviderID:     p.ID,
		})
		if rerr != nil {
			if isDomainCollision(rerr) {
				lastErr = rerr
				continue
			}
			return nil, rerr
		}
		return &RouteRef{RouteID: routeID, Domain: domain, ProviderID: p.ID}, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("%w:多次尝试仍失败", ErrAllocate)
	}
	return nil, ErrAllocate
}

// AllocateFQDN 为一个**指定的** FQDN 建 A 记录指向 hostIP → 创建一条 DNS-01 反代路由(R4 E4.1)。
// 与 AllocateSubdomain 共享校验/建记录/建路由逻辑,但子域名由调用方给定(不随机挑),用于
// 幂等可预测的预览域名 pr-<n>-<proj>.base。
//   - in.Subdomain 必须非空且**严格落在**该提供商某个根区下(后缀 .base 校验,最长后缀优先;
//     杜绝越权为任意域签证书 / 建记录);否则 ErrAllocate。
//   - 域名已占用(同 PR 重复分配且未先回收旧路由)→ 透传 proxy 的 domain-taken 错误,供上层处置。
func (s *service) AllocateFQDN(ctx context.Context, in AllocateInput) (*RouteRef, error) {
	if s.routes == nil {
		return nil, fmt.Errorf("%w:路由创建器未装配", ErrAllocate)
	}
	p, err := s.store.get(ctx, in.ProviderID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.ServerID) == "" || strings.TrimSpace(in.UpstreamContainer) == "" {
		return nil, ErrAllocate
	}
	if in.UpstreamPort < 1 || in.UpstreamPort > 65535 {
		return nil, ErrAllocate
	}
	if !validIPv4(in.HostIP) {
		return nil, fmt.Errorf("%w:宿主机 IP 非法", ErrAllocate)
	}
	domain := strings.ToLower(strings.TrimSpace(in.Subdomain))
	zones, err := s.store.listZonesByProvider(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	// 须严格落在某个根区下(domain == 根区本身不算子域名):最长后缀优先。
	var zone *Zone
	for i := range zones {
		z := zones[i]
		if domain != z.BaseDomain && strings.HasSuffix(domain, "."+z.BaseDomain) {
			if zone == nil || len(z.BaseDomain) > len(zone.BaseDomain) {
				zone = &zones[i]
			}
		}
	}
	if domain == "" || zone == nil {
		return nil, fmt.Errorf("%w:子域名须落在提供商某个根区下", ErrAllocate)
	}

	client, cerr := s.clientFor(p)
	if cerr != nil {
		return nil, cerr
	}
	// 建 A 记录指向宿主机 IP(幂等)。DNSPod/alidns 桩返回未实现 → 直接上抛。
	if err := client.EnsureARecord(ctx, zone.BaseDomain, domain, in.HostIP); err != nil {
		return nil, err
	}
	routeID, rerr := s.routes.CreateDNS01Route(ctx, CreateDNS01RouteInput{
		ServerID:          in.ServerID,
		Domain:            domain,
		UpstreamContainer: in.UpstreamContainer,
		UpstreamPort:      in.UpstreamPort,
		DNSProviderID:     p.ID,
	})
	if rerr != nil {
		return nil, rerr
	}
	return &RouteRef{RouteID: routeID, Domain: domain, ProviderID: p.ID}, nil
}

// zoneCovering 返回覆盖 fqdn 的根区(fqdn == base 或以 "."+base 结尾;最长后缀优先);无 → nil。
func zoneCovering(zones []Zone, fqdn string) *Zone {
	fqdn = strings.ToLower(strings.TrimSpace(fqdn))
	var best *Zone
	for i := range zones {
		z := zones[i]
		covers := strings.EqualFold(fqdn, z.BaseDomain) ||
			strings.HasSuffix(fqdn, "."+z.BaseDomain)
		if covers && (best == nil || len(z.BaseDomain) > len(best.BaseDomain)) {
			best = &zones[i]
		}
	}
	return best
}

// isDomainCollision 判定路由创建错误是否为「域名已占用」(供子域名重试)。
// 用文本匹配(避免 import proxy 形成环;proxy.ErrDomainTaken 的文本含 "domain already in use")。
func isDomainCollision(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "domain already in use")
}

// randAlphabet 是可读子域名后缀字符集(去掉易混淆的 0/o/1/l/i,纯小写+数字)。
const randAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// randSuffix 经 crypto/rand 生成长度为 n 的可读后缀(无 math/rand、无 time 依赖,测试稳定)。
func randSuffix(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, x := range b {
		out[i] = randAlphabet[int(x)%len(randAlphabet)]
	}
	return string(out), nil
}

// validIPv4 报告 s 是否为合法 IPv4 地址(子域名 A 记录指向宿主机 IPv4)。
func validIPv4(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	return ip != nil && ip.To4() != nil
}
