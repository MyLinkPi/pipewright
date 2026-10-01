package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/platformhttps"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 平台 HTTPS 写操作审计 action / target。detail 绝无 PEM 内容。
const (
	auditActionPlatformHTTPSSave    = "platformhttps.settings.save"
	auditActionPlatformHTTPSApply   = "platformhttps.apply"
	auditActionPlatformHTTPSDisable = "platformhttps.disable"
	auditTargetPlatformHTTPS        = "platform_https"
)

// platformHTTPSdto 是平台 HTTPS 设置对外响应体(冻结契约;无 PEM/密文)。
type platformHTTPSdto struct {
	Enabled           bool   `json:"enabled"`
	ServerID          string `json:"serverId"`
	Domain            string `json:"domain"`
	CertID            string `json:"certId"`
	UpstreamHost      string `json:"upstreamHost"`      // 空 = 127.0.0.1(展示层已归一)
	UpstreamPort      int    `json:"upstreamPort"`      // 0 = 平台默认端口(展示层已归一)
	EffectiveUpstream string `json:"effectiveUpstream"` // 生效反代目标,如 "127.0.0.1:8080"
	HTTPRedirect      bool   `json:"httpRedirect"`
	Status            string `json:"status"`       // '' | active | failed
	StatusDetail      string `json:"statusDetail"` // 人话失败原因
	LastAppliedAt     string `json:"lastAppliedAt"`
	UpdatedAt         string `json:"updatedAt"`
}

func toPlatformHTTPSdto(s *platformhttps.Settings, effective string) platformHTTPSdto {
	t := func(v time.Time) string {
		if v.IsZero() {
			return ""
		}
		return v.UTC().Format(time.RFC3339)
	}
	host, port := s.UpstreamHost, s.UpstreamPort
	if host == "" {
		host = "127.0.0.1"
	}
	return platformHTTPSdto{
		Enabled: s.Enabled, ServerID: s.ServerID, Domain: s.Domain, CertID: s.CertID,
		UpstreamHost: host, UpstreamPort: port, EffectiveUpstream: effective,
		HTTPRedirect: s.HTTPRedirect, Status: s.Status, StatusDetail: s.StatusDetail,
		LastAppliedAt: t(s.LastAppliedAt), UpdatedAt: t(s.UpdatedAt),
	}
}

// platformHTTPSDetectdto 是宿主 nginx 探测结果对外响应体。
type platformHTTPSDetectdto struct {
	ServerID      string `json:"serverId"`
	Installed     bool   `json:"installed"`
	Version       string `json:"version"`
	IsRoot        bool   `json:"isRoot"`
	SudoOk        bool   `json:"sudoOk"`
	ConfDIncluded bool   `json:"confDIncluded"`
	ManagedConf   bool   `json:"managedConf"`
}

// writePlatformHTTPSError 把领域错误映射为契约错误码/状态码(照 writeCertMgmtError 风格)。
func writePlatformHTTPSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, platformhttps.ErrNotConfigured):
		writeError(w, http.StatusBadRequest, "https_settings_incomplete", "设置不完整:启用需要选择服务器、填写域名并选择证书")
	case errors.Is(err, platformhttps.ErrInvalidDomain):
		writeError(w, http.StatusBadRequest, "invalid_https_domain", "访问域名格式非法:应为单一 FQDN(如 pip.efg.com),不支持通配符")
	case errors.Is(err, platformhttps.ErrInvalidUpstream):
		writeError(w, http.StatusBadRequest, "invalid_https_upstream", "反代上游非法:host 须为 IP/FQDN/localhost,端口 1..65535")
	case errors.Is(err, platformhttps.ErrCertSourceMissing):
		writeError(w, http.StatusServiceUnavailable, "cert_source_missing", "证书管理模块未接入,无法校验或下发证书")
	case errors.Is(err, platformhttps.ErrCertNotReady):
		writeError(w, http.StatusBadRequest, "https_cert_not_ready", "所选证书不存在或尚未签发完成")
	case errors.Is(err, platformhttps.ErrCertNotCover):
		writeError(w, http.StatusBadRequest, "https_cert_not_cover", "所选证书的 SAN 不覆盖访问域名(通配符证书须覆盖该子域)")
	case errors.Is(err, platformhttps.ErrNoNginx):
		writeError(w, http.StatusBadRequest, "nginx_not_installed", "目标服务器未安装 nginx(或不在 PATH),请先安装宿主 nginx")
	case errors.Is(err, platformhttps.ErrNoPrivilege):
		writeError(w, http.StatusBadRequest, "no_privilege", "SSH 用户非 root 且免密 sudo 不可用:请用 root 登录或为该用户配置免密 sudo")
	case errors.Is(err, platformhttps.ErrApply):
		writeError(w, http.StatusBadGateway, "https_apply_failed", "应用 HTTPS 配置失败(详情见错误信息/设置页状态)")
	case errors.Is(err, platformhttps.ErrAppliedChange):
		writeError(w, http.StatusConflict, "https_applied_change", "配置已应用:变更服务器/域名前请先「禁用并清理」,避免旧机配置与证书残留")
	// target(SSH)层错误:复用语义映射。
	case errors.Is(err, target.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法取 SSH 凭据或证书密文")
	case errors.Is(err, target.ErrNotFound):
		writeError(w, http.StatusUnprocessableEntity, "server_not_found", "目标主机不存在")
	case errors.Is(err, target.ErrCredentialNotFound):
		writeError(w, http.StatusUnprocessableEntity, "credential_error", "引用的 SSH 凭据不存在")
	case errors.Is(err, target.ErrAuth):
		writeError(w, http.StatusBadGateway, "ssh_auth_failed", "SSH 认证失败:密钥或口令无效,或无登录权限")
	case errors.Is(err, target.ErrUnreachable):
		writeError(w, http.StatusBadGateway, "server_unreachable", "无法连接目标服务器")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// ---------- 设置 ----------

// makeGetPlatformHTTPSSettingsHandler 返回 GET /api/platform-https/settings。
func makeGetPlatformHTTPSSettingsHandler(svc platformhttps.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "平台 HTTPS 未初始化")
			return
		}
		st, err := svc.GetSettings(r.Context())
		if err != nil {
			writePlatformHTTPSError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toPlatformHTTPSdto(st, svc.EffectiveUpstream(st)))
	}
}

// makeUpdatePlatformHTTPSSettingsHandler 返回 PUT /api/platform-https/settings
// (保存设置;apply=true 时保存成功后立即应用,应用失败仍返回 200 + settings + applyError 人话)。
func makeUpdatePlatformHTTPSSettingsHandler(svc platformhttps.Service, aud audit.Recorder,
) http.HandlerFunc {
	type request struct {
		Enabled      *bool   `json:"enabled"`
		ServerID     *string `json:"serverId"`
		Domain       *string `json:"domain"`
		CertID       *string `json:"certId"`
		UpstreamHost *string `json:"upstreamHost"`
		UpstreamPort *int    `json:"upstreamPort"`
		HTTPRedirect *bool   `json:"httpRedirect"`
		Apply        bool    `json:"apply"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "平台 HTTPS 未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		st, err := svc.SaveSettings(r.Context(), platformhttps.SettingsInput{
			Enabled: req.Enabled, ServerID: req.ServerID, Domain: req.Domain, CertID: req.CertID,
			UpstreamHost: req.UpstreamHost, UpstreamPort: req.UpstreamPort, HTTPRedirect: req.HTTPRedirect,
		})
		if err != nil {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionPlatformHTTPSSave, TargetType: auditTargetPlatformHTTPS,
				TargetID: "-", Detail: map[string]any{"ok": false}, IP: clientIP(r),
			})
			writePlatformHTTPSError(w, err)
			return
		}
		applyErr := ""
		if req.Apply && st.Enabled {
			if st, err = svc.Apply(r.Context()); err != nil {
				applyErr = err.Error()
			}
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionPlatformHTTPSSave, TargetType: auditTargetPlatformHTTPS,
			TargetID: st.Domain, Detail: map[string]any{
				"ok": true, "enabled": st.Enabled, "serverId": st.ServerID, "applyAttempted": req.Apply && st.Enabled,
			}, IP: clientIP(r),
		})
		resp := map[string]any{"settings": toPlatformHTTPSdto(st, svc.EffectiveUpstream(st))}
		if applyErr != "" {
			resp["applyError"] = applyErr
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// ---------- 探测 ----------

// makeDetectPlatformHTTPSHandler 返回 GET /api/platform-https/detect?serverId=。
func makeDetectPlatformHTTPSHandler(svc platformhttps.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "平台 HTTPS 未初始化")
			return
		}
		d, err := svc.Detect(r.Context(), r.URL.Query().Get("serverId"))
		if err != nil {
			writePlatformHTTPSError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, platformHTTPSDetectdto{
			ServerID: d.ServerID, Installed: d.Installed, Version: d.Version,
			IsRoot: d.IsRoot, SudoOk: d.SudoOk, ConfDIncluded: d.ConfDIncluded, ManagedConf: d.ManagedConf,
		})
	}
}

// ---------- 应用 / 禁用 ----------

// makeApplyPlatformHTTPSHandler 返回 POST /api/platform-https/apply(重新应用/收敛)。
func makeApplyPlatformHTTPSHandler(svc platformhttps.Service, aud audit.Recorder,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "平台 HTTPS 未初始化")
			return
		}
		st, err := svc.Apply(r.Context())
		if err != nil {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionPlatformHTTPSApply, TargetType: auditTargetPlatformHTTPS,
				TargetID: "-", Detail: map[string]any{"ok": false}, IP: clientIP(r),
			})
			writePlatformHTTPSError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionPlatformHTTPSApply, TargetType: auditTargetPlatformHTTPS,
			TargetID: st.Domain, Detail: map[string]any{"ok": true, "serverId": st.ServerID}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toPlatformHTTPSdto(st, svc.EffectiveUpstream(st)))
	}
}

// makeDisablePlatformHTTPSHandler 返回 POST /api/platform-https/disable
// (移除远端 conf.d 文件与证书目录并 reload;本地设置保留,便于再次启用)。
func makeDisablePlatformHTTPSHandler(svc platformhttps.Service, aud audit.Recorder,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "平台 HTTPS 未初始化")
			return
		}
		st, err := svc.Disable(r.Context())
		if err != nil {
			recordAudit(r.Context(), aud, audit.Entry{
				Actor: auditActor, Action: auditActionPlatformHTTPSDisable, TargetType: auditTargetPlatformHTTPS,
				TargetID: "-", Detail: map[string]any{"ok": false}, IP: clientIP(r),
			})
			writePlatformHTTPSError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionPlatformHTTPSDisable, TargetType: auditTargetPlatformHTTPS,
			TargetID: st.Domain, Detail: map[string]any{"ok": true}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toPlatformHTTPSdto(st, svc.EffectiveUpstream(st)))
	}
}
