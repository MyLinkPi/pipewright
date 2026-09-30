package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/runner"
)

// runner.go 暴露项目「构建机选择器」配置端点(FR-8-14 续 / FR-8-19 池化):
//
//	GET /api/projects/{id}/runner  → { runnerServerId, selector }
//	PUT /api/projects/{id}/runner  → 同上(需 CSRF;空 = 清,回本地构建)。
//
// selector 为标签选择器(`linux,arch=arm64`)或钉死形式 `server:<id>`(等价旧 runnerServerId 入参,
// 旧客户端只传 runnerServerId 仍被接受并转换)。校验在 runner.Service(语法/项目/钉死机器存在性)。

type runnerDTO struct {
	RunnerServerID string `json:"runnerServerId"`
	Selector       string `json:"selector"`
}

func writeRunnerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runner.ErrProjectNotFound):
		writeError(w, http.StatusNotFound, "project_not_found", "项目不存在")
	case errors.Is(err, runner.ErrServerNotFound):
		writeError(w, http.StatusUnprocessableEntity, "runner_server_not_found", "指定的 runner 服务器不存在")
	case errors.Is(err, runner.ErrInvalidSelector):
		writeError(w, http.StatusUnprocessableEntity, "invalid_runner_selector", "选择器语法非法(应为逗号分隔标签如 linux,arch=arm64,或 server:<id>)")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

func makeGetRunnerHandler(svc runner.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "runner 配置服务未初始化")
			return
		}
		cfg, err := svc.Get(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeRunnerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, runnerDTO{RunnerServerID: cfg.RunnerServerID, Selector: cfg.Selector})
	}
}

func makeSaveRunnerHandler(svc runner.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "runner 配置服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
		var req runnerDTO
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		// selector 优先;未传 selector 但传了旧 runnerServerId(钉死单机)→ 转规范形式。
		selector := strings.TrimSpace(req.Selector)
		if selector == "" && strings.TrimSpace(req.RunnerServerID) != "" {
			selector = "server:" + strings.TrimSpace(req.RunnerServerID)
		}
		cfg, err := svc.SaveSelector(r.Context(), id, selector)
		if err != nil {
			writeRunnerError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionProjectUpdate,
			TargetType: audit.TargetProject,
			TargetID:   id,
			Detail:     map[string]any{"selector": cfg.Selector, "runnerServerId": cfg.RunnerServerID},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, runnerDTO{RunnerServerID: cfg.RunnerServerID, Selector: cfg.Selector})
	}
}
