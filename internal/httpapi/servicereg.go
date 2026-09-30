package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/servicereg"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 服务注册网关(nginx)写操作审计 action / target。detail 绝无 PEM 内容/token 明文。
const (
	auditActionSRSettingsUpdate = "servicereg.settings.update"
	auditActionSRTokenGenerate  = "servicereg.token.generate"
	auditActionSRTokenRevoke    = "servicereg.token.revoke"
	auditActionSRGatewayDeploy  = "servicereg.gateway.deploy"
	auditActionSRGatewayRemove  = "servicereg.gateway.remove"
	auditActionSRDomainCreate   = "servicereg.domain.create"
	auditActionSRDomainDelete   = "servicereg.domain.delete"
	auditActionSRCertUpload     = "servicereg.cert.upload"
	auditActionSRServiceCreate  = "servicereg.service.create"
	auditActionSRServiceUpdate  = "servicereg.service.update"
	auditActionSRServiceDelete  = "servicereg.service.delete"
	auditActionSRServiceEnable  = "servicereg.service.enable"
	auditActionSRInstanceAdd    = "servicereg.instance.add"
	auditActionSRInstanceRemove = "servicereg.instance.remove"
	auditActionSRInstanceAttach = "servicereg.instance.attach"
	auditActionSRApply          = "servicereg.apply"
	auditTargetServiceReg       = "service_reg"
	// auditActorCertToken 是经证书上传 token(脚本)认证的审计 actor。
	auditActorCertToken = "cert-upload-token"
)

// serviceRegSettingsDTO 是网关设置对外响应体(冻结契约)。
type serviceRegSettingsDTO struct {
	ServerID       string `json:"serverId"`
	HTTPPort       int    `json:"httpPort"`
	HTTPSPort      int    `json:"httpsPort"`
	Image          string `json:"image"`
	Network        string `json:"network"`
	ContainerName  string `json:"containerName"`
	VolumeName     string `json:"volumeName"`
	HasUploadToken bool   `json:"hasUploadToken"`
	LastApplyAt    string `json:"lastApplyAt"`
	LastApplyError string `json:"lastApplyError"`
}

func toServiceRegSettingsDTO(st *servicereg.Settings) serviceRegSettingsDTO {
	last := ""
	if !st.LastApplyAt.IsZero() {
		last = st.LastApplyAt.UTC().Format(time.RFC3339)
	}
	return serviceRegSettingsDTO{
		ServerID: st.ServerID, HTTPPort: st.HTTPPort, HTTPSPort: st.HTTPSPort,
		Image: st.Image, Network: st.Network, ContainerName: st.ContainerName,
		VolumeName: st.VolumeName, HasUploadToken: st.HasUploadToken,
		LastApplyAt: last, LastApplyError: st.LastApplyError,
	}
}

// serviceRegDomainDTO 是基域对外响应体(绝无 PEM/密文)。
type serviceRegDomainDTO struct {
	ID            string `json:"id"`
	BaseDomain    string `json:"baseDomain"`
	HasCert       bool   `json:"hasCert"`
	CertSubject   string `json:"certSubject"`
	CertExpiresAt string `json:"certExpiresAt"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

func toServiceRegDomainDTO(d servicereg.Domain) serviceRegDomainDTO {
	exp := ""
	if !d.CertExpiresAt.IsZero() {
		exp = d.CertExpiresAt.UTC().Format(time.RFC3339)
	}
	return serviceRegDomainDTO{
		ID: d.ID, BaseDomain: d.BaseDomain, HasCert: d.HasCert,
		CertSubject: d.CertSubject, CertExpiresAt: exp,
		CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: d.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// serviceRegServiceDTO 是注册服务对外响应体(附基域名与完整 FQDN 便于前端直显)。
type serviceRegServiceDTO struct {
	ID            string `json:"id"`
	DomainID      string `json:"domainId"`
	BaseDomain    string `json:"baseDomain"`
	Name          string `json:"name"`
	FQDN          string `json:"fqdn"`
	Protocol      string `json:"protocol"`
	UpstreamKind  string `json:"upstreamKind"`
	Upstream      string `json:"upstream"`
	UpstreamPort  int    `json:"upstreamPort"`
	TCPListenPort int    `json:"tcpListenPort"`
	Enabled       bool   `json:"enabled"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

func toServiceRegServiceDTO(s servicereg.ServiceWithDomain) serviceRegServiceDTO {
	return serviceRegServiceDTO{
		ID: s.ID, DomainID: s.DomainID, BaseDomain: s.BaseDomain, Name: s.Name,
		FQDN: s.FQDN(s.BaseDomain), Protocol: s.Protocol, UpstreamKind: s.UpstreamKind,
		Upstream: s.Upstream, UpstreamPort: s.UpstreamPort, TCPListenPort: s.TCPListenPort,
		Enabled: s.Enabled,
		CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: s.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// serviceRegGatewayDTO 是网关容器状态对外响应体。
type serviceRegGatewayDTO struct {
	Configured     bool   `json:"configured"`
	ServerID       string `json:"serverId"`
	ServerName     string `json:"serverName"`
	Installed      bool   `json:"installed"`
	Running        bool   `json:"running"`
	Image          string `json:"image"`
	Ports          string `json:"ports"`
	LastApplyAt    string `json:"lastApplyAt"`
	LastApplyError string `json:"lastApplyError"`
}

func toServiceRegGatewayDTO(g *servicereg.GatewayStatus) serviceRegGatewayDTO {
	last := ""
	if !g.LastApplyAt.IsZero() {
		last = g.LastApplyAt.UTC().Format(time.RFC3339)
	}
	return serviceRegGatewayDTO{
		Configured: g.Configured, ServerID: g.ServerID, ServerName: g.ServerName,
		Installed: g.Installed, Running: g.Running, Image: g.Image, Ports: g.Ports,
		LastApplyAt: last, LastApplyError: g.LastApplyError,
	}
}

// writeServiceRegError 把领域错误映射为契约错误码/状态码(照 writeProxyError 风格)。
func writeServiceRegError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, servicereg.ErrNotFound):
		writeError(w, http.StatusNotFound, "service_reg_not_found", "基域或服务不存在")
	case errors.Is(err, servicereg.ErrDomainTaken):
		writeError(w, http.StatusConflict, "base_domain_taken", "该基域已注册")
	case errors.Is(err, servicereg.ErrServiceTaken):
		writeError(w, http.StatusConflict, "service_name_taken", "该基域下服务名已被占用")
	case errors.Is(err, servicereg.ErrFQDNTaken):
		writeError(w, http.StatusConflict, "fqdn_taken", "该子域名已全局被其它服务占用")
	case errors.Is(err, servicereg.ErrTCPPortTaken):
		writeError(w, http.StatusConflict, "tcp_port_taken", "TCP 监听端口与其它服务或 80/443 冲突")
	case errors.Is(err, servicereg.ErrInvalidBaseDomain):
		writeError(w, http.StatusBadRequest, "invalid_service_reg", "基域格式非法,请填写小写 FQDN(如 efg.com,不带 www/通配符)")
	case errors.Is(err, servicereg.ErrInvalidServiceName):
		writeError(w, http.StatusBadRequest, "invalid_service_reg", "服务名非法:小写字母/数字(可含 -,不以 - 开头),将作为子域名前缀")
	case errors.Is(err, servicereg.ErrInvalidProtocol):
		writeError(w, http.StatusBadRequest, "invalid_service_reg", "协议只支持 http 或 tcp")
	case errors.Is(err, servicereg.ErrInvalidUpstream):
		writeError(w, http.StatusBadRequest, "invalid_service_reg", "上游非法:容器名仅字母数字与 . _ -,地址须为 IP / 域名 / host.docker.internal")
	case errors.Is(err, servicereg.ErrInvalidPort):
		writeError(w, http.StatusBadRequest, "invalid_service_reg", "端口必须在 1..65535 之间")
	case errors.Is(err, servicereg.ErrInstanceTaken):
		writeError(w, http.StatusConflict, "instance_taken", "该服务下此实例容器名已被占用")
	case errors.Is(err, servicereg.ErrInvalidInstance):
		writeError(w, http.StatusBadRequest, "invalid_service_reg", "实例非法:容器名仅字母数字与 . _ -,端口 0..65535(0=继承服务端口)")
	case errors.Is(err, servicereg.ErrNotContainerService):
		writeError(w, http.StatusBadRequest, "invalid_service_reg", "实例仅适用于「HTTP + 容器上游」的服务")
	case errors.Is(err, servicereg.ErrInvalidCert):
		writeError(w, http.StatusBadRequest, "invalid_cert", "证书/私钥 PEM 非法或两者不配对")
	case errors.Is(err, servicereg.ErrNoGateway):
		writeError(w, http.StatusBadRequest, "gateway_not_configured", "请先在网关设置中选择网关主机")
	case errors.Is(err, servicereg.ErrInvalidSetting):
		writeError(w, http.StatusBadRequest, "invalid_setting", "网关设置非法(端口/镜像/网络/容器名/卷名)")
	case errors.Is(err, servicereg.ErrNginxStart):
		writeError(w, http.StatusBadGateway, "gateway_start_failed", "启动网关容器失败,请确认目标主机已安装 docker 且有权限")
	case errors.Is(err, servicereg.ErrPortConflict):
		writeError(w, http.StatusConflict, "port_conflict", "网关主机 HTTP/HTTPS 端口被占用,无法启动网关容器")
	case errors.Is(err, servicereg.ErrUpstreamConnect):
		writeError(w, http.StatusBadGateway, "upstream_connect_failed", "上游容器接入网关网络失败,请确认容器名正确且正在运行")
	case errors.Is(err, servicereg.ErrApply):
		writeError(w, http.StatusBadGateway, "apply_failed", "下发网关配置失败(详情见错误信息)")
	// target(SSH)层错误:复用语义映射。
	case errors.Is(err, target.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法加密证书或取 SSH 凭据")
	case errors.Is(err, target.ErrNotFound):
		writeError(w, http.StatusUnprocessableEntity, "server_not_found", "目标主机不存在")
	case errors.Is(err, target.ErrCredentialNotFound):
		writeError(w, http.StatusUnprocessableEntity, "credential_error", "引用的 SSH 凭据不存在")
	case errors.Is(err, target.ErrAuth):
		writeError(w, http.StatusBadGateway, "ssh_auth_failed", "SSH 认证失败:密钥或口令无效,或无登录权限")
	case errors.Is(err, target.ErrUnreachable), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusBadGateway, "ssh_unreachable", "无法连接目标主机:端口未开放、主机不可达或超时")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// ---------- 设置 ----------

// makeGetServiceRegSettingsHandler 返回 GET /api/servicereg/settings。
func makeGetServiceRegSettingsHandler(svc servicereg.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		st, err := svc.GetSettings(r.Context())
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toServiceRegSettingsDTO(st))
	}
}

// makeUpdateServiceRegSettingsHandler 返回 PUT /api/servicereg/settings。
func makeUpdateServiceRegSettingsHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		ServerID      *string `json:"serverId"`
		HTTPPort      *int    `json:"httpPort"`
		HTTPSPort     *int    `json:"httpsPort"`
		Image         *string `json:"image"`
		Network       *string `json:"network"`
		ContainerName *string `json:"containerName"`
		VolumeName    *string `json:"volumeName"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		st, err := svc.UpdateSettings(r.Context(), servicereg.SettingsUpdate{
			ServerID: req.ServerID, HTTPPort: req.HTTPPort, HTTPSPort: req.HTTPSPort,
			Image: req.Image, Network: req.Network, ContainerName: req.ContainerName, VolumeName: req.VolumeName,
		})
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRSettingsUpdate, TargetType: auditTargetServiceReg,
			TargetID: "settings", Detail: map[string]any{"ok": true, "serverId": st.ServerID}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toServiceRegSettingsDTO(st))
	}
}

// makeGenerateSRUploadTokenHandler 返回 POST /api/servicereg/settings/upload-token。
// 明文 token 仅本次响应返回一次,库中只留 sha256。
func makeGenerateSRUploadTokenHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		token, _, err := svc.GenerateUploadToken(r.Context())
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRTokenGenerate, TargetType: auditTargetServiceReg,
			TargetID: "settings", Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]string{"token": token})
	}
}

// makeRevokeSRUploadTokenHandler 返回 DELETE /api/servicereg/settings/upload-token。
func makeRevokeSRUploadTokenHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		if err := svc.RevokeUploadToken(r.Context()); err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRTokenRevoke, TargetType: auditTargetServiceReg,
			TargetID: "settings", Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ---------- 网关 ----------

// makeGetServiceRegGatewayHandler 返回 GET /api/servicereg/gateway。
func makeGetServiceRegGatewayHandler(svc servicereg.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		g, err := svc.GatewayStatus(r.Context())
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toServiceRegGatewayDTO(g))
	}
}

// makeDeployServiceRegGatewayHandler 返回 POST /api/servicereg/gateway/deploy。
func makeDeployServiceRegGatewayHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		g, err := svc.DeployGateway(r.Context())
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRGatewayDeploy, TargetType: auditTargetServiceReg,
			TargetID: g.ServerID, Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toServiceRegGatewayDTO(g))
	}
}

// makeRemoveServiceRegGatewayHandler 返回 DELETE /api/servicereg/gateway。
func makeRemoveServiceRegGatewayHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		if err := svc.RemoveGateway(r.Context()); err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRGatewayRemove, TargetType: auditTargetServiceReg,
			TargetID: "gateway", Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ---------- 基域 ----------

// makeListServiceRegDomainsHandler 返回 GET /api/servicereg/domains → { items: [...] }。
func makeListServiceRegDomainsHandler(svc servicereg.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		domains, err := svc.ListDomains(r.Context())
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		items := make([]serviceRegDomainDTO, 0, len(domains))
		for _, d := range domains {
			items = append(items, toServiceRegDomainDTO(d))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makeCreateServiceRegDomainHandler 返回 POST /api/servicereg/domains。
func makeCreateServiceRegDomainHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		BaseDomain string `json:"baseDomain"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		d, err := svc.CreateDomain(r.Context(), req.BaseDomain)
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRDomainCreate, TargetType: auditTargetServiceReg,
			TargetID: d.ID, Detail: map[string]any{"baseDomain": d.BaseDomain}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toServiceRegDomainDTO(*d))
	}
}

// makeDeleteServiceRegDomainHandler 返回 DELETE /api/servicereg/domains/{id}。
func makeDeleteServiceRegDomainHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.DeleteDomain(r.Context(), id); err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRDomainDelete, TargetType: auditTargetServiceReg,
			TargetID: id, Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ---------- 证书上传(双认证:Bearer token 或 管理员会话 + CSRF) ----------

// bearerToken 抽取 Authorization: Bearer <token>(无则空串)。
func bearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// serviceRegCertUploadRequest 是 POST /api/servicereg/cert 请求体。
type serviceRegCertUploadRequest struct {
	BaseDomain string `json:"baseDomain"`
	CertPEM    string `json:"certPem"`
	KeyPEM     string `json:"keyPem"`
}

// makeServiceRegCertUploadHandler 返回 POST /api/servicereg/cert(注册在受保护 /api 组之外):
// 支持两种认证 —— Authorization: Bearer <证书上传 token>(供自签发工具脚本调用,token 即认证,
// 非 cookie 无 CSRF 面)或管理员会话 + CSRF(页面表单)。按 baseDomain 定位基域,幂等可重试。
func makeServiceRegCertUploadHandler(svc servicereg.Service, authn auth.Authenticator, aud audit.Recorder) http.HandlerFunc {
	handle := func(w http.ResponseWriter, r *http.Request, actor string) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		var req serviceRegCertUploadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		domains, err := svc.ListDomains(r.Context())
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		var domainID string
		for _, d := range domains {
			if strings.EqualFold(d.BaseDomain, strings.TrimSpace(req.BaseDomain)) {
				domainID = d.ID
				break
			}
		}
		if domainID == "" {
			writeError(w, http.StatusNotFound, "base_domain_not_found", "基域不存在,请先注册 " + req.BaseDomain)
			return
		}
		d, err := svc.UploadCert(r.Context(), domainID, req.CertPEM, req.KeyPEM)
		if err != nil {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: actor, Action: auditActionSRCertUpload, TargetType: auditTargetServiceReg,
				TargetID: domainID, Detail: map[string]any{"ok": false, "baseDomain": req.BaseDomain}, IP: clientIP(r),
			})
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: actor, Action: auditActionSRCertUpload, TargetType: auditTargetServiceReg,
			TargetID: domainID, Detail: map[string]any{"ok": true, "baseDomain": d.BaseDomain, "subject": d.CertSubject}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toServiceRegDomainDTO(*d))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Bearer token 即认证(常量时间校验在领域层);无 token 时回退会话 + CSRF。
		if tok := bearerToken(r); tok != "" && svc.VerifyUploadToken(tok) {
			handle(w, r, auditActorCertToken)
			return
		}
		requireAuth(authn, requireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handle(w, r, auditActor)
		}))).ServeHTTP(w, r)
	}
}

// ---------- 服务 ----------

// makeListServiceRegServicesHandler 返回 GET /api/servicereg/services → { items: [...] }。
func makeListServiceRegServicesHandler(svc servicereg.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		items, err := svc.ListServices(r.Context())
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		out := make([]serviceRegServiceDTO, 0, len(items))
		for _, s := range items {
			out = append(out, toServiceRegServiceDTO(s))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// serviceRegServiceRequest 是创建/更新服务的请求体字段(创建时全量,更新时缺省保持)。
type serviceRegServiceRequest struct {
	DomainID      *string `json:"domainId"`
	Name          *string `json:"name"`
	Protocol      *string `json:"protocol"`
	UpstreamKind  *string `json:"upstreamKind"`
	Upstream      *string `json:"upstream"`
	UpstreamPort  *int    `json:"upstreamPort"`
	TCPListenPort *int    `json:"tcpListenPort"`
}

// makeCreateServiceRegServiceHandler 返回 POST /api/servicereg/services。
func makeCreateServiceRegServiceHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		var req serviceRegServiceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if req.DomainID == nil || req.Name == nil || req.Upstream == nil || req.UpstreamPort == nil {
			writeError(w, http.StatusBadRequest, "invalid_service_reg", "domainId / name / upstream / upstreamPort 必填")
			return
		}
		in := servicereg.CreateServiceInput{
			DomainID: *req.DomainID, Name: *req.Name, Upstream: *req.Upstream, UpstreamPort: *req.UpstreamPort,
		}
		if req.Protocol != nil {
			in.Protocol = *req.Protocol
		}
		if req.UpstreamKind != nil {
			in.UpstreamKind = *req.UpstreamKind
		}
		if req.TCPListenPort != nil {
			in.TCPListenPort = *req.TCPListenPort
		}
		created, err := svc.CreateService(r.Context(), in)
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRServiceCreate, TargetType: auditTargetServiceReg,
			TargetID: created.ID, Detail: map[string]any{"name": created.Name, "protocol": created.Protocol}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": created.ID, "name": created.Name, "enabled": created.Enabled,
		})
	}
}

// makeUpdateServiceRegServiceHandler 返回 PUT /api/servicereg/services/{id}。
func makeUpdateServiceRegServiceHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		var req serviceRegServiceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		in := servicereg.UpdateServiceInput{}
		if req.Name != nil {
			in.Name = req.Name
		}
		if req.Protocol != nil {
			in.Protocol = req.Protocol
		}
		if req.UpstreamKind != nil {
			in.UpstreamKind = req.UpstreamKind
		}
		if req.Upstream != nil {
			in.Upstream = req.Upstream
		}
		if req.UpstreamPort != nil {
			in.UpstreamPort = req.UpstreamPort
		}
		if req.TCPListenPort != nil {
			in.TCPListenPort = req.TCPListenPort
		}
		id := chi.URLParam(r, "id")
		updated, err := svc.UpdateService(r.Context(), id, in)
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRServiceUpdate, TargetType: auditTargetServiceReg,
			TargetID: id, Detail: map[string]any{"name": updated.Name, "ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"id": updated.ID, "name": updated.Name})
	}
}

// makeDeleteServiceRegServiceHandler 返回 DELETE /api/servicereg/services/{id}。
func makeDeleteServiceRegServiceHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.DeleteService(r.Context(), id); err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRServiceDelete, TargetType: auditTargetServiceReg,
			TargetID: id, Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// makeSetServiceRegServiceEnabledHandler 返回 POST /api/servicereg/services/{id}/enabled {enabled}。
func makeSetServiceRegServiceEnabledHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Enabled bool `json:"enabled"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.SetServiceEnabled(r.Context(), id, req.Enabled); err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRServiceEnable, TargetType: auditTargetServiceReg,
			TargetID: id, Detail: map[string]any{"enabled": req.Enabled}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// makeApplyServiceRegHandler 返回 POST /api/servicereg/apply(手动全量收敛)。
func makeApplyServiceRegHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		if err := svc.Apply(r.Context()); err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRApply, TargetType: auditTargetServiceReg,
			TargetID: "gateway", Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ---------- 实例 ----------

// serviceRegInstanceDTO 是服务实例对外响应体。
type serviceRegInstanceDTO struct {
	ID        string `json:"id"`
	ServiceID string `json:"serviceId"`
	Container string `json:"container"`
	Port      int    `json:"port"` // 0 = 继承服务端口
	Attached  bool   `json:"attached"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

func toServiceRegInstanceDTO(i servicereg.Instance) serviceRegInstanceDTO {
	return serviceRegInstanceDTO{
		ID: i.ID, ServiceID: i.ServiceID, Container: i.Container, Port: i.Port,
		Attached: i.Attached,
		CreatedAt: i.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: i.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// makeListServiceRegInstancesHandler 返回 GET /api/servicereg/services/{id}/instances。
func makeListServiceRegInstancesHandler(svc servicereg.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		items, err := svc.ListInstances(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		out := make([]serviceRegInstanceDTO, 0, len(items))
		for _, i := range items {
			out = append(out, toServiceRegInstanceDTO(i))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// makeAddServiceRegInstanceHandler 返回 POST /api/servicereg/services/{id}/instances
// {container, port}(port=0 继承服务端口;默认 attached 并立即 apply)。
func makeAddServiceRegInstanceHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Container string `json:"container"`
		Port      int    `json:"port"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		inst, err := svc.AddInstance(r.Context(), chi.URLParam(r, "id"), req.Container, req.Port)
		if err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRInstanceAdd, TargetType: auditTargetServiceReg,
			TargetID: inst.ServiceID, Detail: map[string]any{"container": inst.Container}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toServiceRegInstanceDTO(*inst))
	}
}

// makeDeleteServiceRegInstanceHandler 返回 DELETE /api/servicereg/instances/{id}。
func makeDeleteServiceRegInstanceHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.RemoveInstance(r.Context(), id); err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRInstanceRemove, TargetType: auditTargetServiceReg,
			TargetID: id, Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// makeSetServiceRegInstanceAttachedHandler 返回 POST /api/servicereg/instances/{id}/attached
// {attached}(摘除 = 从 upstream 剔除并 reload,其余实例继续服务)。
func makeSetServiceRegInstanceAttachedHandler(svc servicereg.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Attached bool `json:"attached"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.SetInstanceAttached(r.Context(), id, req.Attached); err != nil {
			writeServiceRegError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSRInstanceAttach, TargetType: auditTargetServiceReg,
			TargetID: id, Detail: map[string]any{"attached": req.Attached}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
