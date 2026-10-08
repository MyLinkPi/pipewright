package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/approval"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/run"
)

// RunApprovalLister 列出某运行的审批记录(供恢复守卫判定「存在被拒门 → 不可按节点恢复」)。
// *approval.Store 满足此接口;nil 表示审批门未装配(守卫跳过)。
type RunApprovalLister interface {
	ListForRun(ctx context.Context, runID string) ([]approval.Record, error)
}

// resume.go 实现「失败运行按节点恢复(派生重跑)」的端点:
//
//	POST /api/runs/{id}/resume — body {actions: {"<ordinal>": "retry"|"skip"}}
//
// 流程:取父运行(须 failed/partial_failed 终态)→ 用与执行器**同一** SpecLoader 加载当前
// 流水线配置 → dagrun.BuildResumePlan 做节点身份校验(逐 ordinal 比对 stage/name;配置已变
// → 409 spec_changed)并生成全量动作数组 → run.Service.CreateResume 创建派生运行(复制触发
// 上下文 / 产物 / 部署目标行,部署节点在子运行中天然增量:只重试失败机器)→ 201 + 子运行 DTO,
// 前端跳转子运行详情。
//
// 与审批门(approve/reject)互补:审批是**运行中**阻塞等人工决定;恢复是**终态失败后**派生
// 新运行,父运行历史保持不动。
//
// actions 键 = run_steps.ordinal(stepDTO.ordinal),值仅支持 "retry"(真实重跑该节点)与
// "skip"(人工跳过该节点但放行下游)。未指定的失败节点 → 422(不猜测用户意图)。

// makeResumeRunHandler 返回 POST /api/runs/{id}/resume。
// runs / specLoader 为 nil → 503(legacy runner 模式不装配 loader,恢复端点不可用,与其
// 不支持按节点恢复一致)。approvals 为审批记录列表器(即 *approval.Store):存在被拒审批门
// (人工拒绝/超时/门控处取消)的运行不可按节点恢复——即使终态是 failed(混合情况)——409。
func makeResumeRunHandler(runs run.Service, specLoader dagrun.SpecLoader, approvals RunApprovalLister, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if runs == nil || specLoader == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "运行恢复服务未初始化")
			return
		}
		parentID := chi.URLParam(r, "id")

		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Actions map[string]string `json:"actions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if len(req.Actions) == 0 {
			writeError(w, http.StatusUnprocessableEntity, "missing_actions", "缺少 actions(须为失败节点的处置映射)")
			return
		}
		actions := make(map[int]string, len(req.Actions))
		for k, v := range req.Actions {
			ord, perr := strconv.Atoi(strings.TrimSpace(k))
			if perr != nil || ord < 0 {
				writeError(w, http.StatusUnprocessableEntity, "bad_action", "actions 键须为非负整数 ordinal(见 stepDTO.ordinal)")
				return
			}
			actions[ord] = strings.TrimSpace(v)
		}

		parent, err := runs.Get(r.Context(), parentID)
		if err != nil {
			writeRunError(w, err)
			return
		}
		// 可恢复性先于计划校验:非失败/部分失败终态 → 409(即使计划本身也不合法,语义上「不可恢复」优先)。
		if parent.Status != run.StatusFailed && parent.Status != run.StatusPartialFailed {
			writeResumeError(w, run.ErrNotResumable)
			return
		}
		// 审批门被人工关闭过(拒绝/超时/门控处取消)的运行不可按节点恢复——即使是「真失败 +
		// 拒绝」的混合情况(终态 failed):重走审批必须重新触发新流水线,而非绕过门的节点恢复。
		if approvals != nil {
			recs, lerr := approvals.ListForRun(r.Context(), parentID)
			if lerr == nil {
				for _, rec := range recs {
					if rec.Status == approval.StatusRejected {
						writeError(w, http.StatusConflict, "run_not_resumable",
							"运行包含被拒绝的审批门,不可按节点恢复,请重新触发新流水线")
						return
					}
				}
			}
		}
		cfg, _, err := specLoader.Get(r.Context(), parent.ProjectID, parent.Trigger.Branch)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "spec_unavailable", "无法加载项目流水线配置,无法按节点恢复")
			return
		}
		plan, err := dagrun.BuildResumePlan(cfg, parent, actions)
		if err != nil {
			writeResumeError(w, err)
			return
		}

		child, err := runs.CreateResume(r.Context(), parentID, plan, auditActor)
		if err != nil {
			writeResumeError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     "run.resume",
			TargetType: "run",
			TargetID:   parentID,
			Detail:     map[string]any{"childRunId": child.ID, "actions": req.Actions},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toRunDetailDTO(child, nil, nil))
	}
}

// writeResumeError 把恢复链路错误映射为契约错误码/状态码(人读中文,无敏感信息)。
func writeResumeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, run.ErrNotFound):
		writeError(w, http.StatusNotFound, "run_not_found", "运行不存在")
	case errors.Is(err, run.ErrNotResumable):
		writeError(w, http.StatusConflict, "run_not_resumable", "仅失败/部分失败的运行可按节点恢复")
	case errors.Is(err, run.ErrInvalidResumePlan):
		writeError(w, http.StatusUnprocessableEntity, "invalid_resume_plan", err.Error())
	case errors.Is(err, dagrun.ErrSpecChanged):
		writeError(w, http.StatusConflict, "spec_changed", "流水线配置已变化,无法按节点恢复")
	case errors.Is(err, dagrun.ErrNoFailedNodes):
		writeError(w, http.StatusUnprocessableEntity, "no_failed_nodes", "该运行没有失败的节点,无可恢复项")
	case errors.Is(err, run.ErrQueueFull), errors.Is(err, run.ErrPoolStopped):
		writeError(w, http.StatusServiceUnavailable, "run_queue_full", "调度队列已满,请稍后重试")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}
