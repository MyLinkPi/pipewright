package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/systemcfg"
	"github.com/huangchengsir/pipewright/internal/version"
)

// 系统配置写操作审计 action / target。
const (
	auditActionSystemConfigSet = "systemconfig.set"
	auditTargetSystemConfig    = "system_config"
)

// systemConfigDTO 是系统配置对外响应体。
type systemConfigDTO struct {
	PublicURL     string              `json:"publicUrl"`
	ReleaseMirror string              `json:"releaseMirror"`
	UpdatedAt     string              `json:"updatedAt"`
	Effective     *effectiveSourceDTO `json:"effectiveSource"`
}

// effectiveSourceDTO 是实际生效的升级源(镜像解析结果),让默认值透明可见:
// 未配置镜像时即 GitHub Releases 官方(检查 api.github.com / 下载 github.com);
// 也兜住「库内留空但环境变量配了镜像」的情况 —— 界面如实显示生效值与来源。
type effectiveSourceDTO struct {
	Origin  string `json:"origin"` // config(库内配置) | env(环境变量) | default(GitHub 官方)
	APIBase string `json:"apiBase"`
	DLBase  string `json:"dlBase"`
}

func toSystemConfigDTO(c systemcfg.Config) systemConfigDTO {
	updated := ""
	if !c.UpdatedAt.IsZero() {
		updated = c.UpdatedAt.UTC().Format(time.RFC3339)
	}
	src, origin := version.ResolveMirror(c.ReleaseMirror)
	return systemConfigDTO{
		PublicURL:     c.PublicURL,
		ReleaseMirror: c.ReleaseMirror,
		UpdatedAt:     updated,
		Effective:     &effectiveSourceDTO{Origin: string(origin), APIBase: src.APIBase, DLBase: src.DLBase},
	}
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

// makeSetSystemConfigHandler 返回 PUT /api/system/config {publicUrl?, releaseMirror?}。
// 字段按需更新:缺省(null)= 不动该项;空串 = 清除。修改即时生效:
//   - publicUrl:通知审批链接 / PR 回写 target_url 的前缀;
//   - releaseMirror:自升级(检查更新 + 二进制下载)的镜像源,下次检查即走新源
//     (Checker 缓存按源隔离,换源不返回旧源结果)。
func makeSetSystemConfigHandler(svc systemcfg.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		PublicURL     *string `json:"publicUrl"`
		ReleaseMirror *string `json:"releaseMirror"`
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
		c, err := svc.Get(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		if req.PublicURL != nil {
			if c, err = svc.SetPublicURL(r.Context(), *req.PublicURL); err != nil {
				if errors.Is(err, systemcfg.ErrInvalidURL) {
					writeError(w, http.StatusBadRequest, "invalid_public_url",
						"外部访问地址非法:须为 http(s)://host[:port] 形式的 origin(不带路径),留空表示清除")
					return
				}
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
				return
			}
		}
		if req.ReleaseMirror != nil {
			if c, err = svc.SetReleaseMirror(r.Context(), *req.ReleaseMirror); err != nil {
				if errors.Is(err, systemcfg.ErrInvalidMirror) {
					writeError(w, http.StatusBadRequest, "invalid_release_mirror",
						"升级源非法:须为 http(s)://host[:port][/path] 形式的地址(不带查询串),留空表示清除")
					return
				}
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
				return
			}
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionSystemConfigSet, TargetType: auditTargetSystemConfig,
			TargetID: "default", Detail: map[string]any{"ok": true, "publicUrl": c.PublicURL, "releaseMirror": c.ReleaseMirror}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toSystemConfigDTO(c))
	}
}
