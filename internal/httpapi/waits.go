package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/waitnode"
)

// waits.go 实现「等待」节点(wait)的运行中查询与手动跳过端点。
//
// 等待节点执行期 sleep 固定秒数(解决部署时序);等待期间本端点列出运行详情里可跳过的节点,
// skip 端点经协调器投递「跳过」决定——执行器收到后立即结束等待,该节点 step 落 skipped 但
// 放行下游。与审批门(approvals.go)同构:进程内协调器投递、无等待者 409、审计留痕。

// runWaitDTO 是一条等待中的节点(对齐前端 RunStep 的 ordinal 定位 + 跳过用的 jobId)。
type runWaitDTO struct {
	JobID            string `json:"jobId"`
	JobName          string `json:"jobName"`
	StageName        string `json:"stageName"`
	Ordinal          int    `json:"ordinal"`
	TotalSeconds     int    `json:"totalSeconds"`
	RemainingSeconds int    `json:"remainingSeconds"`
}

// makeListRunWaitsHandler 返回 GET /api/runs/{id}/waits(某运行当前等待中的节点列表;
// 前端据此在步骤行上展示「跳过等待」按钮)。coord 为 nil → 503。
func makeListRunWaitsHandler(coord *waitnode.Coordinator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if coord == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "等待节点服务未初始化")
			return
		}
		infos := coord.PendingByRun(chi.URLParam(r, "id"))
		items := make([]runWaitDTO, 0, len(infos))
		for _, info := range infos {
			items = append(items, runWaitDTO{
				JobID:            info.JobID,
				JobName:          info.JobName,
				StageName:        info.StageName,
				Ordinal:          info.Ordinal,
				TotalSeconds:     info.TotalSeconds,
				RemainingSeconds: info.RemainingSeconds(),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makeSkipWaitHandler 返回 POST /api/runs/{id}/skip-wait:body {jobId},经协调器投递跳过决定。
// 该节点当前不在等待(未开始 / 已到时 / 已跳过)→ 409。coord 为 nil → 503。
func makeSkipWaitHandler(coord *waitnode.Coordinator, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if coord == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "等待节点服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<13)
		var req struct {
			JobID string `json:"jobId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		jobID := strings.TrimSpace(req.JobID)
		if jobID == "" {
			writeError(w, http.StatusUnprocessableEntity, "missing_job", "缺少 jobId")
			return
		}
		if !coord.Resolve(waitnode.Key(id, jobID), waitnode.Skip{Actor: auditActor}) {
			writeError(w, http.StatusConflict, "not_waiting", "该节点当前不在等待中")
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     "run.skip_wait",
			TargetType: "run",
			TargetID:   id,
			Detail:     map[string]any{"jobId": jobID},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
