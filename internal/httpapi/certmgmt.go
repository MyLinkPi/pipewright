package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/certmgmt"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 证书管理写操作审计 action / target。detail 绝无 PEM 内容。
const (
	auditActionCertCreate    = "certmgmt.cert.create"
	auditActionCertImport    = "certmgmt.cert.import"
	auditActionCertRenew     = "certmgmt.cert.renew"
	auditActionCertDelete    = "certmgmt.cert.delete"
	auditActionCertAutoRenew = "certmgmt.cert.autorenew"
	auditActionCertEngineDep = "certmgmt.engine.deploy"
	auditTargetCertMgmt      = "certificate"
	auditTargetCertEngine    = "cert_engine"
)

// certDTO 是证书对外响应体(冻结契约;绝无 PEM/密文)。
type certDTO struct {
	ID            string   `json:"id"`
	PrimaryDomain string   `json:"primaryDomain"`
	Domains       []string `json:"domains"`
	Source        string   `json:"source"`     // acme | manual
	CA            string   `json:"ca"`         // letsencrypt | zerossl | buypass(manual 为空)
	Validation    string   `json:"validation"` // dns | manual
	DNSProviderID string   `json:"dnsProviderId"`
	KeyType       string   `json:"keyType"`
	AutoRenew     bool     `json:"autoRenew"`
	Status        string   `json:"status"` // pending | issued | failed
	StatusDetail  string   `json:"statusDetail"`
	Subject       string   `json:"subject"`
	Issuer        string   `json:"issuer"`
	NotBefore     string   `json:"notBefore"`
	NotAfter      string   `json:"notAfter"`
	LastIssuedAt  string   `json:"lastIssuedAt"`
	CreatedAt     string   `json:"createdAt"`
	UpdatedAt     string   `json:"updatedAt"`
}

func toCertDTO(c certmgmt.Certificate) certDTO {
	t := func(v time.Time) string {
		if v.IsZero() {
			return ""
		}
		return v.UTC().Format(time.RFC3339)
	}
	domains := c.Domains
	if domains == nil {
		domains = []string{}
	}
	return certDTO{
		ID: c.ID, PrimaryDomain: c.PrimaryDomain, Domains: domains,
		Source: c.Source, CA: c.CA, Validation: c.Validation, DNSProviderID: c.DNSProviderID,
		KeyType: c.KeyType, AutoRenew: c.AutoRenew, Status: c.Status, StatusDetail: c.StatusDetail,
		Subject: c.Subject, Issuer: c.Issuer,
		NotBefore: t(c.NotBefore), NotAfter: t(c.NotAfter), LastIssuedAt: t(c.LastIssuedAt),
		CreatedAt: t(c.CreatedAt), UpdatedAt: t(c.UpdatedAt),
	}
}

// certEngineDTO 是签发引擎(控制机本地的 acme.sh 脚本集)状态对外响应体。
type certEngineDTO struct {
	Installed bool   `json:"installed"`
	Ready     bool   `json:"ready"`
	Version   string `json:"version"`
}

func toCertEngineDTO(e *certmgmt.EngineStatus) certEngineDTO {
	return certEngineDTO{
		Installed: e.Installed, Ready: e.Ready, Version: e.Version,
	}
}

// writeCertMgmtError 把领域错误映射为契约错误码/状态码(照 writeServiceRegError 风格)。
func writeCertMgmtError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, certmgmt.ErrNotFound):
		writeError(w, http.StatusNotFound, "cert_not_found", "证书不存在")
	case errors.Is(err, certmgmt.ErrDomainTaken):
		writeError(w, http.StatusConflict, "cert_domain_taken", "该主域名已有证书")
	case errors.Is(err, certmgmt.ErrInvalidDomain):
		writeError(w, http.StatusBadRequest, "invalid_cert_domain", "域名格式非法:应为 FQDN(如 efg.com)或通配符 *.efg.com")
	case errors.Is(err, certmgmt.ErrTooManyDomains):
		writeError(w, http.StatusBadRequest, "invalid_cert_domain", "附加域名过多(单张证书最多 32 个 SAN)")
	case errors.Is(err, certmgmt.ErrNoDNSProvider):
		writeError(w, http.StatusBadRequest, "dns_provider_required", "ACME 签发走 DNS-01,必须选择一个已绑定的 DNS 提供商")
	case errors.Is(err, certmgmt.ErrInvalidDNSProvider):
		writeError(w, http.StatusBadRequest, "invalid_dns_provider", "DNS 提供商不存在、类型不支持或凭据缺失")
	case errors.Is(err, certmgmt.ErrZoneUncovered):
		writeError(w, http.StatusBadRequest, "zone_uncovered", "有域名不在所选 DNS 提供商托管的根区下,无法完成 DNS-01 挑战")
	case errors.Is(err, certmgmt.ErrInvalidCA):
		writeError(w, http.StatusBadRequest, "invalid_cert_input", "CA 只支持 letsencrypt / zerossl / buypass")
	case errors.Is(err, certmgmt.ErrInvalidKeyType):
		writeError(w, http.StatusBadRequest, "invalid_cert_input", "密钥类型只支持 ec-256 / ec-384 / rsa-2048")
	case errors.Is(err, certmgmt.ErrInvalidCert):
		writeError(w, http.StatusBadRequest, "invalid_cert", "证书/私钥 PEM 非法或两者不配对")
	case errors.Is(err, certmgmt.ErrBusy):
		writeError(w, http.StatusConflict, "cert_busy", "该证书已有签发/续期操作进行中,请稍候")
	case errors.Is(err, certmgmt.ErrCertInUse):
		writeError(w, http.StatusConflict, "cert_in_use", "证书正被平台 HTTPS 使用,请先在「设置 → HTTPS 访问」中更换或关闭")
	case errors.Is(err, certmgmt.ErrAcmeshStart):
		writeError(w, http.StatusBadGateway, "cert_engine_start_failed", "安装 acme.sh 签发引擎失败,请确认控制机已安装 sh / curl / openssl(引擎在控制机本地运行,须为 Linux/POSIX 环境)")
	case errors.Is(err, certmgmt.ErrIssue):
		writeError(w, http.StatusBadGateway, "acme_failed", "acme.sh 签发/续期失败(详情见错误信息;也可在证书列表查看)")
	case errors.Is(err, certmgmt.ErrReadBack):
		writeError(w, http.StatusBadGateway, "cert_readback_failed", "证书文件读取/校验失败")
	// target(SSH)层错误:复用语义映射。
	case errors.Is(err, target.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法加密证书或取 SSH 凭据")
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

// ---------- 证书 ----------

// makeListCertsHandler 返回 GET /api/certmgmt/certs → { items: [...] }。
func makeListCertsHandler(svc certmgmt.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书管理未初始化")
			return
		}
		certs, err := svc.List(r.Context())
		if err != nil {
			writeCertMgmtError(w, err)
			return
		}
		items := make([]certDTO, 0, len(certs))
		for _, c := range certs {
			items = append(items, toCertDTO(c))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makeGetCertHandler 返回 GET /api/certmgmt/certs/{id}。
func makeGetCertHandler(svc certmgmt.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书管理未初始化")
			return
		}
		c, err := svc.Get(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeCertMgmtError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toCertDTO(*c))
	}
}

// makeCreateCertHandler 返回 POST /api/certmgmt/certs(201,异步签发:立即返回 pending 行)。
func makeCreateCertHandler(svc certmgmt.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		PrimaryDomain string   `json:"primaryDomain"`
		Domains       []string `json:"domains"`
		DNSProviderID string   `json:"dnsProviderId"`
		CA            string   `json:"ca"`
		KeyType       string   `json:"keyType"`
		AutoRenew     *bool    `json:"autoRenew"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书管理未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		autoRenew := true
		if req.AutoRenew != nil {
			autoRenew = *req.AutoRenew
		}
		c, err := svc.Create(r.Context(), certmgmt.CreateInput{
			PrimaryDomain: req.PrimaryDomain, Domains: req.Domains, DNSProviderID: req.DNSProviderID,
			CA: req.CA, KeyType: req.KeyType, AutoRenew: autoRenew,
		})
		if err != nil {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionCertCreate, TargetType: auditTargetCertMgmt,
				TargetID: req.PrimaryDomain, Detail: map[string]any{"ok": false}, IP: clientIP(r),
			})
			writeCertMgmtError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionCertCreate, TargetType: auditTargetCertMgmt,
			TargetID: c.ID, Detail: map[string]any{"ok": true, "primaryDomain": c.PrimaryDomain, "ca": c.CA}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toCertDTO(*c))
	}
}

// makeImportCertHandler 返回 POST /api/certmgmt/certs/import(201,同步导入)。
func makeImportCertHandler(svc certmgmt.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		CertPEM string `json:"certPem"`
		KeyPEM  string `json:"keyPem"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书管理未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		c, err := svc.Import(r.Context(), certmgmt.ImportInput{CertPEM: req.CertPEM, KeyPEM: req.KeyPEM})
		if err != nil {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionCertImport, TargetType: auditTargetCertMgmt,
				TargetID: "-", Detail: map[string]any{"ok": false}, IP: clientIP(r),
			})
			writeCertMgmtError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionCertImport, TargetType: auditTargetCertMgmt,
			TargetID: c.ID, Detail: map[string]any{"ok": true, "primaryDomain": c.PrimaryDomain}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toCertDTO(*c))
	}
}

// makeRenewCertHandler 返回 POST /api/certmgmt/certs/{id}/renew(acme:异步重签;manual:同步重新下发)。
func makeRenewCertHandler(svc certmgmt.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书管理未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		c, err := svc.Renew(r.Context(), id)
		if err != nil {
			writeCertMgmtError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionCertRenew, TargetType: auditTargetCertMgmt,
			TargetID: id, Detail: map[string]any{"ok": true, "primaryDomain": c.PrimaryDomain}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toCertDTO(*c))
	}
}

// makeDeleteCertHandler 返回 DELETE /api/certmgmt/certs/{id}。
func makeDeleteCertHandler(svc certmgmt.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书管理未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.Delete(r.Context(), id); err != nil {
			writeCertMgmtError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionCertDelete, TargetType: auditTargetCertMgmt,
			TargetID: id, Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// makeSetCertAutoRenewHandler 返回 POST /api/certmgmt/certs/{id}/auto-renew {enabled}。
func makeSetCertAutoRenewHandler(svc certmgmt.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Enabled bool `json:"enabled"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书管理未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		c, err := svc.SetAutoRenew(r.Context(), id, req.Enabled)
		if err != nil {
			writeCertMgmtError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionCertAutoRenew, TargetType: auditTargetCertMgmt,
			TargetID: id, Detail: map[string]any{"enabled": req.Enabled}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toCertDTO(*c))
	}
}

// ---------- 签发引擎 ----------

// makeGetCertEngineHandler 返回 GET /api/certmgmt/engine(acme.sh 脚本集状态)。
func makeGetCertEngineHandler(svc certmgmt.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书管理未初始化")
			return
		}
		e, err := svc.EngineStatus(r.Context())
		if err != nil {
			writeCertMgmtError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toCertEngineDTO(e))
	}
}

// makeDeployCertEngineHandler 返回 POST /api/certmgmt/engine/deploy(显式部署/更新 acme.sh 脚本集)。
func makeDeployCertEngineHandler(svc certmgmt.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书管理未初始化")
			return
		}
		e, err := svc.DeployEngine(r.Context())
		if err != nil {
			writeCertMgmtError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionCertEngineDep, TargetType: auditTargetCertEngine,
			TargetID: "local", Detail: map[string]any{"ok": true, "version": e.Version}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toCertEngineDTO(e))
	}
}
