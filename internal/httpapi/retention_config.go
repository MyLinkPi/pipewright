package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/huangchengsir/pipewright/internal/retention"
)

// retentionConfigDTO 是运行数据保留策略(全局)。
//   - enabled:总开关(关时不清理 run 数据)。
//   - keepPerProject:每项目保留最近 N 条终态运行(0=不限)。
//   - maxAgeDays:删除创建早于 N 天的终态运行(0=不限)。
//   - cacheMaxGB:构建依赖缓存总量上限 GB(0=不限;独立于 enabled,LRU 淘汰)。
//   - imageKeepTags:每项目保留的本地镜像 tag 数(0=不轮转;独立于 enabled)。
type retentionConfigDTO struct {
	Enabled        bool `json:"enabled"`
	KeepPerProject int  `json:"keepPerProject"`
	MaxAgeDays     int  `json:"maxAgeDays"`
	CacheMaxGB     int  `json:"cacheMaxGb"`
	ImageKeepTags  int  `json:"imageKeepTags"`
}

// GET /api/retention/config — 读取保留策略。
func makeGetRetentionConfigHandler(svc *retention.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "保留服务未初始化")
			return
		}
		cfg, err := svc.GetConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		writeJSON(w, http.StatusOK, retentionConfigDTO{
			Enabled:        cfg.Enabled,
			KeepPerProject: cfg.KeepPerProject,
			MaxAgeDays:     cfg.MaxAgeDays,
			CacheMaxGB:     cfg.CacheMaxGB,
			ImageKeepTags:  cfg.ImageKeepTags,
		})
	}
}

// PUT /api/retention/config — 设置保留策略(负值归一为 0)。
func makeSetRetentionConfigHandler(svc *retention.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "保留服务未初始化")
			return
		}
		var in retentionConfigDTO
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		cfg := retention.Config{
			Enabled:        in.Enabled,
			KeepPerProject: in.KeepPerProject,
			MaxAgeDays:     in.MaxAgeDays,
			CacheMaxGB:     in.CacheMaxGB,
			ImageKeepTags:  in.ImageKeepTags,
		}
		if err := svc.SetConfig(r.Context(), cfg); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		// 回读归一后的值返回(负值已被归零)。
		saved, err := svc.GetConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		writeJSON(w, http.StatusOK, retentionConfigDTO{
			Enabled:        saved.Enabled,
			KeepPerProject: saved.KeepPerProject,
			MaxAgeDays:     saved.MaxAgeDays,
			CacheMaxGB:     saved.CacheMaxGB,
			ImageKeepTags:  saved.ImageKeepTags,
		})
	}
}
