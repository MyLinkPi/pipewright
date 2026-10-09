package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/servercmd"
)

// 批量执行命令(服务器状态页 → 勾选多机 → 同步执行)+ 历史回看。
//
// 任意 shell 命令串是本功能的本质:平台为单管理员、既有 WS 终端(单机)本就能执行任意
// 命令,批量入口不提升权限面。护栏(AC-SEC-02 精神):
//   - 命令长度 ≤ 8KiB、机器数 1..100(去重)、单机超时 5..300s(0=默认 60s)、并发 6、
//     输出逐流截断 64KiB(领域层钳制);
//   - 写方法 → 过 auth + CSRF(组中间件);每次执行(无论成败)写 append-only 审计;
//   - 逐机独立容错:未知 serverId / SSH 失败 / 非零退出码 → 200 + 该机 ok:false,绝不 500。
//
// 执行经 `sh -c <命令>`(target.Upload 的 sh -c 固定脚本同一先例);历史落库保最近 200 次。

// batchCommandRequest 是 POST /api/servers/commands/batch 请求体(冻结契约)。
type batchCommandRequest struct {
	Command    string   `json:"command"`   // shell 命令串(经 sh -c 执行)
	ServerIDs  []string `json:"serverIds"` // 目标机 id(1..100,重复项去重)
	TimeoutSec int      `json:"timeoutSeconds,omitempty"`
}

// batchCommandItemDTO 是单机结果(冻结契约)。ok=false 时 error 为人读串;绝无凭据明文。
type batchCommandItemDTO struct {
	ServerID   string `json:"serverId"`
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Error      string `json:"error"`
	DurationMs int64  `json:"durationMs"`
}

// batchCommandSummaryDTO 是整批成败计数。
type batchCommandSummaryDTO struct {
	Total  int `json:"total"`
	OK     int `json:"ok"`
	Failed int `json:"failed"`
}

// batchCommandResponse 是批量执行响应体(冻结契约)。runId 供历史详情回查。
type batchCommandResponse struct {
	RunID   string                 `json:"runId"`
	Items   []batchCommandItemDTO  `json:"items"`
	Summary batchCommandSummaryDTO `json:"summary"`
}

// commandRunSummaryDTO 是历史列表条目(不含逐机输出)。
type commandRunSummaryDTO struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Total     int    `json:"total"`
	OK        int    `json:"ok"`
	Failed    int    `json:"failed"`
	CreatedAt string `json:"createdAt"` // RFC3339
}

// commandRunDetailDTO 是历史详情响应体(头部 + 逐机结果,复用 batchCommandItemDTO)。
type commandRunDetailDTO struct {
	commandRunSummaryDTO
	Items []batchCommandItemDTO `json:"items"`
}

// validateBatchCommand 校验请求(命令非空≤8KiB;serverIds 去重后 1..100)。非法返回人读错误。
func validateBatchCommand(req batchCommandRequest) error {
	cmd := strings.TrimSpace(req.Command)
	if cmd == "" {
		return errors.New("命令不能为空")
	}
	if len(cmd) > servercmd.MaxCommandLen {
		return errors.New("命令过长(上限 8KiB)")
	}
	seen := make(map[string]bool)
	n := 0
	for _, id := range req.ServerIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		n++
	}
	if n == 0 {
		return errors.New("至少选择一台服务器")
	}
	if n > servercmd.MaxServers {
		return errors.New("单次批量机器数超上限(100)")
	}
	return nil
}

// toBatchCommandItemDTO 领域 → 契约。
func toBatchCommandItemDTO(it servercmd.ResultItem) batchCommandItemDTO {
	return batchCommandItemDTO{
		ServerID:   it.ServerID,
		Name:       it.ServerName,
		OK:         it.OK,
		ExitCode:   it.ExitCode,
		Stdout:     it.Stdout,
		Stderr:     it.Stderr,
		Error:      it.Error,
		DurationMs: it.DurationMs,
	}
}

// makeBatchCommandHandler 返回 POST /api/servers/commands/batch handler(写操作:auth+CSRF+审计)。
// 校验不过 → 400 invalid_command_request;逐机失败 → 200 + 该机 ok:false(不 500)。
func makeBatchCommandHandler(svc *servercmd.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "批量命令服务未初始化")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req batchCommandRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := validateBatchCommand(req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_command_request", "批量命令请求非法:"+err.Error())
			return
		}

		res, err := svc.Run(r.Context(), servercmd.RunInput{
			Command:    req.Command,
			ServerIDs:  req.ServerIDs,
			TimeoutSec: req.TimeoutSec,
		})
		if err != nil {
			if errors.Is(err, servercmd.ErrInvalidInput) {
				writeError(w, http.StatusBadRequest, "invalid_command_request", "批量命令请求非法:命令或目标机列表不合规")
				return
			}
			if errors.Is(err, servercmd.ErrUninitialized) {
				writeError(w, http.StatusServiceUnavailable, "internal", "批量命令服务未初始化")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}

		// 审计:批量执行的**任何尝试**都留痕(NFR-8 取证),不只成功;detail 仅白名单
		// 结构化字段(命令截 256 字 + 计数 + runId),自由输出绝不入审计。
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionServerCommand,
			TargetType: audit.TargetServer,
			Detail: map[string]any{
				"command": truncateLog(strings.TrimSpace(req.Command), 256),
				"servers": res.Total,
				"ok":      res.OK,
				"failed":  res.Failed,
				"runId":   res.RunID,
			},
			IP: clientIP(r),
		})

		items := make([]batchCommandItemDTO, 0, len(res.Items))
		for _, it := range res.Items {
			items = append(items, toBatchCommandItemDTO(it))
		}
		writeJSON(w, http.StatusOK, batchCommandResponse{
			RunID:   res.RunID,
			Items:   items,
			Summary: batchCommandSummaryDTO{Total: res.Total, OK: res.OK, Failed: res.Failed},
		})
	}
}

// makeListCommandRunsHandler 返回 GET /api/servers/commands/runs handler(只读,过 auth)。
// query:limit(默认 50,上限 200)。
func makeListCommandRunsHandler(svc *servercmd.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "批量命令服务未初始化")
			return
		}
		limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
		runs, err := svc.ListRuns(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		items := make([]commandRunSummaryDTO, 0, len(runs))
		for _, run := range runs {
			items = append(items, toCommandRunSummaryDTO(run))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makeGetCommandRunHandler 返回 GET /api/servers/commands/runs/{runId} handler(只读)。
// 不存在 → 404 command_run_not_found。
func makeGetCommandRunHandler(svc *servercmd.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "批量命令服务未初始化")
			return
		}
		id := chi.URLParam(r, "runId")
		det, err := svc.GetRun(r.Context(), id)
		if err != nil {
			if errors.Is(err, servercmd.ErrNotFound) {
				writeError(w, http.StatusNotFound, "command_run_not_found", "批量命令记录不存在")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		items := make([]batchCommandItemDTO, 0, len(det.Items))
		for _, it := range det.Items {
			items = append(items, toBatchCommandItemDTO(it))
		}
		writeJSON(w, http.StatusOK, commandRunDetailDTO{
			commandRunSummaryDTO: toCommandRunSummaryDTO(det.RunSummary),
			Items:                items,
		})
	}
}

// toCommandRunSummaryDTO 领域 → 契约。
func toCommandRunSummaryDTO(s servercmd.RunSummary) commandRunSummaryDTO {
	return commandRunSummaryDTO{
		ID:        s.ID,
		Command:   s.Command,
		Total:     s.Total,
		OK:        s.OK,
		Failed:    s.Failed,
		CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339),
	}
}
