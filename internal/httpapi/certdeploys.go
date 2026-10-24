package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/certdeploy"
	"github.com/huangchengsir/pipewright/internal/target"
)

// certDeployDTO 是部署目标对外响应体(camelCase;无 PEM/凭据;status 为最近一次部署结果)。
type certDeployDTO struct {
	ID             string `json:"id"`
	CertID         string `json:"certId"`
	ServerID       string `json:"serverId"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`           // ssh | safeline
	WafInstanceID  string `json:"wafInstanceId"`  // safeline:安全中心 WAF 实例
	CertName       string `json:"certName"`       // safeline:雷池侧证书名
	CertPath       string `json:"certPath"`       // ssh
	KeyPath        string `json:"keyPath"`        // ssh
	PostCommand    string `json:"postCommand"`    // ssh
	Enabled        bool   `json:"enabled"`
	Status         string `json:"status"` // ''|running|success|failed
	StatusDetail   string `json:"statusDetail"`
	LastDeployedAt string `json:"lastDeployedAt"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
	// 冗余展示(join 填充)。
	ServerName        string `json:"serverName"`
	CertPrimaryDomain string `json:"certPrimaryDomain"`
}

func toCertDeployDTO(d *certdeploy.Target) certDeployDTO {
	last := ""
	if !d.LastDeployedAt.IsZero() {
		last = d.LastDeployedAt.UTC().Format(time.RFC3339)
	}
	return certDeployDTO{
		ID: d.ID, CertID: d.CertID, ServerID: d.ServerID, Name: d.Name,
		Kind: d.Kind, WafInstanceID: d.WafInstanceID, CertName: d.CertName,
		CertPath: d.CertPath, KeyPath: d.KeyPath, PostCommand: d.PostCommand,
		Enabled: d.Enabled, Status: d.Status, StatusDetail: d.StatusDetail,
		LastDeployedAt: last,
		CreatedAt:      d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      d.UpdatedAt.UTC().Format(time.RFC3339),
		ServerName:     d.ServerName, CertPrimaryDomain: d.CertPrimaryDomain,
	}
}

// certDeployRunDTO 是一次部署的历史记录。
type certDeployRunDTO struct {
	ID         string `json:"id"`
	Source     string `json:"source"` // manual | renew
	Status     string `json:"status"` // success | failed
	Detail     string `json:"detail"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	DurationMs int64  `json:"durationMs"`
}

func toCertDeployRunDTO(r *certdeploy.Run) certDeployRunDTO {
	var dur int64
	if !r.FinishedAt.IsZero() && !r.StartedAt.IsZero() {
		dur = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
	}
	return certDeployRunDTO{
		ID: r.ID, Source: r.Source, Status: r.Status, Detail: r.Detail,
		StartedAt:  r.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt: r.FinishedAt.UTC().Format(time.RFC3339),
		DurationMs: dur,
	}
}

// certDeployCreateDTO 创建请求体(certId/serverId/路径必填;无凭据明文)。
type certDeployCreateDTO struct {
	Kind          string `json:"kind"`
	CertID        string `json:"certId"`
	ServerID      string `json:"serverId"`
	Name          string `json:"name"`
	CertPath      string `json:"certPath"`
	KeyPath       string `json:"keyPath"`
	PostCommand   string `json:"postCommand"`
	WafInstanceID string `json:"wafInstanceId"`
	CertName      string `json:"certName"`
	Enabled       *bool  `json:"enabled"`
}

// certDeployUpdateDTO 更新请求体(指针字段 nil = 不改;certId/serverId 不可改)。
type certDeployUpdateDTO struct {
	Name        *string `json:"name"`
	CertPath    *string `json:"certPath"`
	KeyPath     *string `json:"keyPath"`
	PostCommand *string `json:"postCommand"`
	CertName    *string `json:"certName"` // safeline:雷池侧证书名
	Enabled     *bool   `json:"enabled"`
}

// writeCertDeployError 把领域错误映射为契约错误码/状态码(校验类人话提示安全可回显;
// 绝不回显 PEM/凭据/栈)。
func writeCertDeployError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, certdeploy.ErrNotFound):
		writeError(w, http.StatusNotFound, "cert_deploy_not_found", "部署目标不存在")
	case errors.Is(err, certdeploy.ErrCertNotFound):
		writeError(w, http.StatusUnprocessableEntity, "cert_not_found", "证书不存在或已删除")
	case errors.Is(err, certdeploy.ErrInvalidInput):
		writeError(w, http.StatusUnprocessableEntity, "invalid_cert_deploy", "部署目标配置无效:"+err.Error())
	case errors.Is(err, target.ErrNotFound):
		writeError(w, http.StatusUnprocessableEntity, "invalid_server", "绑定的服务器不存在或已删除")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// auditCertDeployOp 写部署目标操作审计(detail 只记动作与目标标识,不记路径/命令细节)。
func auditCertDeployOp(r *http.Request, aud audit.Recorder, action, id string, ok bool) {
	recordAudit(r.Context(), aud, audit.Entry{
		Actor:      auditActor,
		Action:     action,
		TargetType: audit.TargetCertDeploy,
		TargetID:   id,
		Detail:     map[string]any{"action": action, "ok": ok},
		IP:         clientIP(r),
	})
}

// makeListCertDeploysHandler 返回 GET /api/cert-deploys?certId= handler → { items }。
func makeListCertDeploysHandler(svc *certdeploy.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书部署服务未初始化")
			return
		}
		items, err := svc.List(r.Context(), r.URL.Query().Get("certId"))
		if err != nil {
			writeCertDeployError(w, err)
			return
		}
		out := make([]certDeployDTO, 0, len(items))
		for _, d := range items {
			out = append(out, toCertDeployDTO(d))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// makeCreateCertDeployHandler 返回 POST /api/cert-deploys handler(写:auth + CSRF + 审计)。
func makeCreateCertDeployHandler(svc *certdeploy.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书部署服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req certDeployCreateDTO
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		d, err := svc.Create(r.Context(), certdeploy.CreateInput{
			Kind: req.Kind, CertID: req.CertID, ServerID: req.ServerID, Name: req.Name,
			CertPath: req.CertPath, KeyPath: req.KeyPath,
			PostCommand: req.PostCommand, Enabled: req.Enabled,
			WafInstanceID: req.WafInstanceID, CertName: req.CertName,
		})
		if err != nil {
			writeCertDeployError(w, err)
			return
		}
		auditCertDeployOp(r, aud, audit.ActionCertDeployCreate, d.ID, true)
		writeJSON(w, http.StatusCreated, toCertDeployDTO(d))
	}
}

// makeUpdateCertDeployHandler 返回 PUT /api/cert-deploys/{id} handler(写:auth + CSRF + 审计)。
func makeUpdateCertDeployHandler(svc *certdeploy.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书部署服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req certDeployUpdateDTO
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		d, err := svc.Update(r.Context(), chi.URLParam(r, "id"), certdeploy.UpdateInput{
			Name: req.Name, CertPath: req.CertPath, KeyPath: req.KeyPath,
			PostCommand: req.PostCommand, CertName: req.CertName, Enabled: req.Enabled,
		})
		if err != nil {
			writeCertDeployError(w, err)
			return
		}
		auditCertDeployOp(r, aud, audit.ActionCertDeployUpdate, d.ID, true)
		writeJSON(w, http.StatusOK, toCertDeployDTO(d))
	}
}

// makeDeleteCertDeployHandler 返回 DELETE /api/cert-deploys/{id} handler(写:auth + CSRF + 审计;
// 一并删除该目标的部署历史)。
func makeDeleteCertDeployHandler(svc *certdeploy.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书部署服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.Delete(r.Context(), id); err != nil {
			writeCertDeployError(w, err)
			return
		}
		auditCertDeployOp(r, aud, audit.ActionCertDeployDelete, id, true)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makeDeployCertDeployHandler 返回 POST /api/cert-deploys/{id}/deploy handler:同步执行一次
// 部署(上传证书/私钥到目标路径 + 部署后命令),总预算 2 分钟;执行失败也返回 200 + 最新行
// (status=failed + statusDetail 人话原因,对齐 servers test 端点「200 + 结果体」先例)。
func makeDeployCertDeployHandler(svc *certdeploy.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书部署服务未初始化")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		d, err := svc.DeployOne(ctx, chi.URLParam(r, "id"))
		if d == nil {
			writeCertDeployError(w, err)
			return
		}
		auditCertDeployOp(r, aud, audit.ActionCertDeployRun, d.ID, err == nil)
		writeJSON(w, http.StatusOK, toCertDeployDTO(d))
	}
}

// makeListCertDeployRunsHandler 返回 GET /api/cert-deploys/{id}/runs handler(只读:过 auth;
// 部署历史,新→旧,默认/上限 50)。
func makeListCertDeployRunsHandler(svc *certdeploy.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "证书部署服务未初始化")
			return
		}
		runs, err := svc.ListRuns(r.Context(), chi.URLParam(r, "id"), 0)
		if err != nil {
			writeCertDeployError(w, err)
			return
		}
		out := make([]certDeployRunDTO, 0, len(runs))
		for _, run := range runs {
			out = append(out, toCertDeployRunDTO(run))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}
