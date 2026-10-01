package dnsprovider

import (
	"context"
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

// dialFactory 据提供商类型 + (API ID, Secret) 造一个 DNSClient(注入便于单测;生产用 prodDialFactory)。
type dialFactory func(providerType, apiID, secret string) DNSClient

// prodDialFactory 是生产工厂:真实 net/http 客户端(Cloudflare 真连,其余桩)。
func prodDialFactory(providerType, apiID, secret string) DNSClient {
	return newDNSClient(providerType, apiID, secret, nil, "")
}

// service 是 store + vault + dialFactory 支撑的 Service 实现。
type service struct {
	store *Store
	vault vaultReader
	dial  dialFactory
}

// New 构造 Service。
//   - db:参数化 SQL 触库。
//   - v:凭据保险库(取 DNS Secret 明文);nil/未配置时 Verify/Allocate 返回 ErrVaultUnconfigured。
//
// 不在此做任何重活(无 init 副作用)。
func New(db *sql.DB, v vault.Vault) Service {
	var vr vaultReader
	if v != nil {
		vr = v
	}
	return &service{
		store: NewStore(db),
		vault: vr,
		dial:  prodDialFactory,
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

// AllocateFQDN 为一个**指定的** FQDN 建 A 记录指向 hostIP(R4 E4.1 预览环境用)。
// 子域名由调用方给定(不随机挑),用于幂等可预测的预览域名 pr-<n>-<proj>.base。
// Caddy 反代下线后本方法只管 DNS 记录,不再创建反代路由;预览流量路由由网关体系另行承接。
//   - in.Subdomain 必须非空且**严格落在**该提供商某个根区下(后缀 .base 校验,最长后缀优先;
//     杜绝越权为任意域建记录);否则 ErrAllocate。
func (s *service) AllocateFQDN(ctx context.Context, in AllocateInput) (*RouteRef, error) {
	p, err := s.store.get(ctx, in.ProviderID)
	if err != nil {
		return nil, err
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
	return &RouteRef{Domain: domain, ProviderID: p.ID}, nil
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


// validIPv4 报告 s 是否为合法 IPv4 地址(子域名 A 记录指向宿主机 IPv4)。
func validIPv4(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	return ip != nil && ip.To4() != nil
}
