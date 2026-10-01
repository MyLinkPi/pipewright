package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/systemcfg"
)

// 系统配置写操作审计 action / target。
const (
	auditActionSystemConfigSet = "systemconfig.set"
	auditTargetSystemConfig    = "system_config"
)

// systemConfigDTO 是系统配置对外响应体(目前仅 publicUrl)。
type systemConfigDTO struct {
	PublicURL string `json:"publicUrl"`
	UpdatedAt string `json:"updatedAt"`
}

func toSystemConfigDTO(c systemcfg.Config) systemConfigDTO {
	updated := ""
	if !c.UpdatedAt.IsZero() {
		updated = c.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return systemConfigDTO{PublicURL: c.PublicURL, UpdatedAt: updated}
}

// makeGetSystemConfigHandler 返回 GET /api/system/config。
func makeGetSystemConfigHandler(svc systemcfg.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "系统配置未初始化")
			return
		}
		c, err := svc.Get(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		writeJSON(w, http.StatusOK, toSystemConfigDTO(c))
	}
}

// makeSetSystemConfigHandler 返回 PUT /api/system/config {publicUrl}。
// publicUrl 须为 http(s) origin(空 = 清除);修改即时生效(通知审批链接/PR 回写 target_url)。
func makeSetSystemConfigHandler(svc systemcfg.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		PublicURL string `json:"publicUrl"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "系统配置未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		c, err := svc.SetPublicURL(r.Context(), req.PublicURL)
		if err != nil {
			if errors.Is(err, systemcfg.ErrInvalidURL) {
				writeError(w, http.StatusBadRequest, "invalid_public_url",
					"外部访问地址非法:须为 http(s)://host[:port] 形式的 origin(不带路径),留空表示清除")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSystemConfigSet, TargetType: auditTargetSystemConfig,
			TargetID: "default", Detail: map[string]any{"ok": true, "publicUrl": c.PublicURL}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toSystemConfigDTO(c))
	}
}
