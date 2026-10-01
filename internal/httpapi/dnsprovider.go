package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/dnsprovider"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 审计 action / target(DNS 提供商 + 根区写操作)。detail 绝无 Secret / 凭据明文
// (API ID 非机密,可入 detail)。
const (
	auditActionDNSProviderCreate = "dns.provider.create"
	auditActionDNSProviderUpdate = "dns.provider.update"
	auditActionDNSProviderDelete = "dns.provider.delete"
	auditActionDNSProviderVerify = "dns.provider.verify"
	auditActionDNSZoneAdd        = "dns.provider.zone.add"
	auditActionDNSZoneRemove     = "dns.provider.zone.remove"
	auditTargetDNSProvider       = "dns_provider"
)

// dnsZoneDTO 是 DNS 提供商根区的对外响应体(camelCase)。
type dnsZoneDTO struct {
	ID         string `json:"id"`
	BaseDomain string `json:"baseDomain"`
	CreatedAt  string `json:"createdAt"`
}

// dnsProviderDTO 是 DNS 提供商对外响应体(冻结契约;camelCase)。绝不外泄 Secret:
// 仅以 credentialConfigured 布尔告知前端「是否已绑凭据」;API ID 非机密,可回显。
type dnsProviderDTO struct {
	ID                   string       `json:"id"`
	Type                 string       `json:"type"`
	Name                 string       `json:"name"`
	APIID                string       `json:"apiId"`
	Zones                []dnsZoneDTO `json:"zones"`
	CredentialConfigured bool         `json:"credentialConfigured"`
	CreatedAt            string       `json:"createdAt"`
}

// toDNSProviderDTO 把领域 Provider 转为契约 DTO(剥离 credentialId,仅暴露「是否已绑凭据」)。
func toDNSProviderDTO(p dnsprovider.Provider) dnsProviderDTO {
	zones := make([]dnsZoneDTO, 0, len(p.Zones))
	for _, z := range p.Zones {
		zones = append(zones, dnsZoneDTO{
			ID:         z.ID,
			BaseDomain: z.BaseDomain,
			CreatedAt:  z.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return dnsProviderDTO{
		ID:                   p.ID,
		Type:                 p.Type,
		Name:                 p.Name,
		APIID:                p.APIID,
		Zones:                zones,
		CredentialConfigured: strings.TrimSpace(p.CredentialID) != "",
		CreatedAt:            p.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// writeDNSProviderError 把 DNS 提供商领域错误映射为契约错误码/状态码;绝不回显明文/Secret。
func writeDNSProviderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, dnsprovider.ErrNotFound):
		writeError(w, http.StatusNotFound, "dns_provider_not_found", "DNS 提供商不存在")
	case errors.Is(err, dnsprovider.ErrInvalidType):
		writeError(w, http.StatusBadRequest, "invalid_dns_provider", "DNS 提供商类型非法(仅支持 cloudflare / dnspod / alidns)")
	case errors.Is(err, dnsprovider.ErrEmptyName):
		writeError(w, http.StatusBadRequest, "invalid_dns_provider", "请填写 DNS 提供商展示名")
	case errors.Is(err, dnsprovider.ErrEmptyCredentialID):
		writeError(w, http.StatusBadRequest, "invalid_dns_provider", "请选择 API 凭据")
	case errors.Is(err, dnsprovider.ErrNoBaseDomains):
		writeError(w, http.StatusBadRequest, "invalid_dns_provider", "请至少填写一个根域(如 example.com)")
	case errors.Is(err, dnsprovider.ErrInvalidBaseDomain):
		writeError(w, http.StatusBadRequest, "invalid_dns_provider", "根域格式非法(或已存在),请填写有效的域名(如 example.com)")
	case errors.Is(err, dnsprovider.ErrZoneNotFound):
		writeError(w, http.StatusNotFound, "dns_zone_not_found", "根区不存在或不属于该提供商")
	case errors.Is(err, dnsprovider.ErrInvalidAPIID):
		writeError(w, http.StatusBadRequest, "invalid_dns_provider", "API ID 与提供商类型不匹配:DNSPod 填 SecretId、阿里云填 AccessKeyId;Cloudflare 无需填写")
	case errors.Is(err, dnsprovider.ErrCredentialNotFound):
		writeError(w, http.StatusUnprocessableEntity, "credential_error", "引用的 API 凭据不存在")
	case errors.Is(err, dnsprovider.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法取 DNS 凭据")
	case errors.Is(err, dnsprovider.ErrInvalidCredential):
		writeError(w, http.StatusBadRequest, "invalid_dns_credential", "DNS 凭据非法:请确认 API ID 与 Secret 均已正确填写")
	case errors.Is(err, dnsprovider.ErrProviderNotImplemented):
		writeError(w, http.StatusNotImplemented, "not_implemented", "该提供商暂未实现自动建 A 记录(DNS-01 证书签发仍可用,请手动添加解析)")
	case errors.Is(err, dnsprovider.ErrVerifyFailed):
		writeError(w, http.StatusBadGateway, "dns_verify_failed", "DNS 校验失败:凭据无效、无该域权限或 API 不可达")
	case errors.Is(err, dnsprovider.ErrEnsureRecord):
		writeError(w, http.StatusBadGateway, "dns_record_failed", "建立 A 记录失败,请确认凭据有该域 DNS 编辑权限")
	case errors.Is(err, dnsprovider.ErrAllocate):
		writeError(w, http.StatusBadGateway, "subdomain_alloc_failed", "子域名分配失败,请稍后重试")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusBadGateway, "dns_unreachable", "连接 DNS API 超时")
	// target(SSH)层错误:复用语义映射。
	case errors.Is(err, target.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法取 DNS 凭据")
	case errors.Is(err, target.ErrNotFound):
		writeError(w, http.StatusUnprocessableEntity, "server_not_found", "目标主机不存在")
	case errors.Is(err, target.ErrCredentialNotFound):
		writeError(w, http.StatusUnprocessableEntity, "credential_error", "引用的 SSH 凭据不存在")
	case errors.Is(err, target.ErrAuth):
		writeError(w, http.StatusBadGateway, "ssh_auth_failed", "SSH 认证失败:密钥或口令无效,或无登录权限")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// makeListDNSProvidersHandler 返回 GET /api/dns/providers → { items: [...] }(只读,无审计)。
func makeListDNSProvidersHandler(svc dnsprovider.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "DNS 提供商服务未初始化")
			return
		}
		providers, err := svc.List(r.Context())
		if err != nil {
			writeDNSProviderError(w, err)
			return
		}
		out := make([]dnsProviderDTO, 0, len(providers))
		for _, p := range providers {
			out = append(out, toDNSProviderDTO(p))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// makeCreateDNSProviderHandler 返回 POST /api/dns/providers(认证 + CSRF)→ Provider(201)。
// body: { type, name, apiId, secret, baseDomains: [...] }。
func makeCreateDNSProviderHandler(svc dnsprovider.Service, v vault.Vault, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "DNS 提供商服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
		// secret 是「只写」字段:前端创建时提交一次明文,经 vault 加密入库换取 credentialId
		// 后即弃,绝不回库/响应/审计/日志。DB 的 dns_providers 只持 credential_id 引用;
		// apiId 非机密,明文列存储。
		var in struct {
			Type        string   `json:"type"`
			Name        string   `json:"name"`
			APIID       string   `json:"apiId"`
			Secret      string   `json:"secret"`
			BaseDomains []string `json:"baseDomains"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if v == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法存 DNS 凭据")
			return
		}
		if strings.TrimSpace(in.Secret) == "" {
			writeError(w, http.StatusBadRequest, "invalid_dns_provider", "请填写 API Secret")
			return
		}
		// 先把 Secret 存入 vault(加密),拿到 credentialId;DB 与领域层全程只见 credential_id。
		cred, err := v.Create(vault.CreateInput{
			Name:   "DNS · " + strings.TrimSpace(in.Name),
			Type:   vault.TypeDNSToken,
			Secret: in.Secret,
		})
		if err != nil {
			writeVaultError(w, err)
			return
		}
		p, err := svc.Create(r.Context(), dnsprovider.CreateInput{
			Type:         in.Type,
			Name:         in.Name,
			APIID:        in.APIID,
			CredentialID: cred.ID,
			BaseDomains:  in.BaseDomains,
		})
		if err != nil {
			// 登记失败 → 回滚刚存的凭据,避免悬挂(provider 没建成,凭据不应留存)。
			_ = v.Delete(cred.ID)
			writeDNSProviderError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionDNSProviderCreate,
			TargetType: auditTargetDNSProvider,
			TargetID:   p.ID,
			Detail:     map[string]any{"type": p.Type, "name": p.Name, "apiId": p.APIID, "zones": len(p.Zones)}, // 绝无 Secret
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toDNSProviderDTO(*p))
	}
}

// makeUpdateDNSProviderHandler 返回 PUT /api/dns/providers/{id}(认证 + CSRF)→ Provider。
// body: { name?, apiId?, secret? }(指针语义:缺省字段不修改;secret 非空即原地轮换,credentialId 不变)。
// 顺序:先做不落库的入参校验(vault 可用 / secret 非空),再落库领域字段,最后轮换 Secret——
// 避免「name/apiId 已改、轮换才失败」的半生效状态;轮换仍失败时补 partial 审计(写操作任何
// 尝试都留痕,NFR-8)再返回错误。
func makeUpdateDNSProviderHandler(svc dnsprovider.Service, v vault.Vault, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "DNS 提供商服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
		var in struct {
			Name   *string `json:"name"`
			APIID  *string `json:"apiId"`
			Secret *string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		// 含 secret 轮换时先做不落库的校验:vault 须可用、secret 须非空(否则直接拒绝,不动领域字段)。
		if in.Secret != nil {
			if v == nil {
				writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法存 DNS 凭据")
				return
			}
			if strings.TrimSpace(*in.Secret) == "" {
				writeError(w, http.StatusBadRequest, "invalid_dns_provider", "Secret 不能为空")
				return
			}
		}
		// 再编辑领域字段(名称 / API ID;API ID 与类型匹配在领域层校验)。
		p, err := svc.Update(r.Context(), id, dnsprovider.UpdateInput{Name: in.Name, APIID: in.APIID})
		if err != nil {
			writeDNSProviderError(w, err)
			return
		}
		// 可选轮换 Secret:vault 原地重封(credentialId 不变,引用零迁移)。
		rotated := false
		if in.Secret != nil {
			if _, err := v.Update(p.CredentialID, vault.UpdateInput{Secret: in.Secret}); err != nil {
				// 领域字段已改但轮换失败:补一条 partial 审计(绝不写 Secret 明文)再返回错误。
				recordAudit(r.Context(), rec, audit.Entry{
					Actor:      auditActor,
					Action:     auditActionDNSProviderUpdate,
					TargetType: auditTargetDNSProvider,
					TargetID:   p.ID,
					Detail:     map[string]any{"name": p.Name, "apiId": p.APIID, "secretRotated": false, "partial": true},
					IP:         clientIP(r),
				})
				writeVaultError(w, err)
				return
			}
			rotated = true
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionDNSProviderUpdate,
			TargetType: auditTargetDNSProvider,
			TargetID:   p.ID,
			Detail:     map[string]any{"name": p.Name, "apiId": p.APIID, "secretRotated": rotated}, // 绝无 Secret
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, toDNSProviderDTO(*p))
	}
}

// makeDeleteDNSProviderHandler 返回 DELETE /api/dns/providers/{id}(认证 + CSRF)→ { ok: true }。
func makeDeleteDNSProviderHandler(svc dnsprovider.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "DNS 提供商服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.Delete(r.Context(), id); err != nil {
			writeDNSProviderError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionDNSProviderDelete,
			TargetType: auditTargetDNSProvider,
			TargetID:   id,
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makeAddDNSZoneHandler 返回 POST /api/dns/providers/{id}/zones(认证 + CSRF)→ Zone(201)。
// body: { baseDomain }(一个提供商可托管多个根区,随时追加)。
func makeAddDNSZoneHandler(svc dnsprovider.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "DNS 提供商服务未初始化")
			return
		}
		providerID := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<12)
		var in struct {
			BaseDomain string `json:"baseDomain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		z, err := svc.AddZone(r.Context(), providerID, in.BaseDomain)
		if err != nil {
			writeDNSProviderError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionDNSZoneAdd,
			TargetType: auditTargetDNSProvider,
			TargetID:   providerID,
			Detail:     map[string]any{"zoneId": z.ID, "baseDomain": z.BaseDomain},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusCreated, dnsZoneDTO{
			ID:         z.ID,
			BaseDomain: z.BaseDomain,
			CreatedAt:  z.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
}

// makeRemoveDNSZoneHandler 返回 DELETE /api/dns/providers/{id}/zones/{zoneId}(认证 + CSRF)→ { ok: true }。
func makeRemoveDNSZoneHandler(svc dnsprovider.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "DNS 提供商服务未初始化")
			return
		}
		providerID := chi.URLParam(r, "id")
		zoneID := chi.URLParam(r, "zoneId")
		if err := svc.RemoveZone(r.Context(), providerID, zoneID); err != nil {
			writeDNSProviderError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionDNSZoneRemove,
			TargetType: auditTargetDNSProvider,
			TargetID:   providerID,
			Detail:     map[string]any{"zoneId": zoneID},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makeVerifyDNSProviderHandler 返回 POST /api/dns/providers/{id}/verify(认证 + CSRF)
// → { ok: <全部通过>, zones: [{id, baseDomain, ok, error}] }。
// 取该提供商凭据明文 → 逐根区经 DNS API 校验可管理(Secret 用完即弃,绝不外泄)。
func makeVerifyDNSProviderHandler(svc dnsprovider.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "DNS 提供商服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		results, err := svc.Verify(r.Context(), id)
		if err != nil {
			writeDNSProviderError(w, err)
			return
		}
		allOK := true
		type zoneVerifyDTO struct {
			ID         string `json:"id"`
			BaseDomain string `json:"baseDomain"`
			OK         bool   `json:"ok"`
			Error      string `json:"error,omitempty"`
		}
		out := make([]zoneVerifyDTO, 0, len(results))
		for _, res := range results {
			item := zoneVerifyDTO{ID: res.ZoneID, BaseDomain: res.BaseDomain, OK: res.Err == nil}
			if res.Err != nil {
				allOK = false
				item.Error = res.Err.Error() // 客户端错误为人话,绝不含凭据
			}
			out = append(out, item)
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionDNSProviderVerify,
			TargetType: auditTargetDNSProvider,
			TargetID:   id,
			Detail:     map[string]any{"ok": allOK, "zones": len(results)},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": allOK, "zones": out})
	}
}

// resolveHostIPv4 把 host(IP 或域名)解析为一个公网 IPv4 字符串(A 记录指向它)。
// host 本身是 IPv4 → 直接返回;是域名 → DNS 解析取首个 IPv4。无 IPv4 → 错误。
func resolveHostIPv4(ctx context.Context, host string) (string, error) {
	host = strings.TrimSpace(host)
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), nil
		}
		return "", errors.New("host is not IPv4")
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			return ip.To4().String(), nil
		}
	}
	return "", errors.New("no IPv4 for host")
}
