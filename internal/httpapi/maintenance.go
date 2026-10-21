package httpapi

// maintenance.go 是「磁盘清理手动触发」入口(磁盘治理,0094):POST /api/maintenance/cleanup
// 按白名单目标同步执行一次维护服务的清理(与后台定时任务同一套实现),供设置页「立即清理」
// 按钮调用。纪律与 makeRegistryPruneHandler / makeSystemPruneHandler 一致:
//
//   - target 走**枚举白名单**(绝不接受任意字符串),非法 400;
//   - 认证 + CSRF + 审计(ActionMaintenanceCleanup;detail 只记 target/ok/计数,无自由输出);
//   - 同步执行、200 返回结果 JSON;单项内部失败以 ok:false + 人读 error 表达,不 500;
//   - 单请求 120s 上限(远程清扫逐机 60s × 并发 8,有界完成)。

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/maintenance"
)

// maintenanceCleanupTimeout 是手动清理的单请求上限(防极慢 SSH 拖住连接)。
const maintenanceCleanupTimeout = 120 * time.Second

// maintenanceTargets 是清理目标白名单 → 维护服务方法的映射(枚举即安全边界,非法 400)。
var maintenanceTargets = map[string]func(*maintenance.Service, context.Context) maintenance.Result{
	"buildcache":        (*maintenance.Service).CleanBuildcache,
	"repocache":         (*maintenance.Service).CleanRepocache,
	"remote-workspaces": (*maintenance.Service).SweepRemoteWorkspaces,
	"local-temp":        (*maintenance.Service).SweepLocalTemp,
}

// maintenanceCleanupRequest 是 POST /api/maintenance/cleanup 请求体。
type maintenanceCleanupRequest struct {
	Target string `json:"target"`
}

// makeMaintenanceCleanupHandler 返回 POST /api/maintenance/cleanup(认证 + CSRF + 审计)。
func makeMaintenanceCleanupHandler(svc *maintenance.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "维护服务未初始化")
			return
		}
		var req maintenanceCleanupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		fn, ok := maintenanceTargets[req.Target]
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_request", "未知的清理目标")
			return
		}
		cctx, cancel := context.WithTimeout(r.Context(), maintenanceCleanupTimeout)
		defer cancel()
		res := fn(svc, cctx)
		// 审计 detail:结论性字段 + 远程清扫的机器成败与命令数(逐条明细在 HTTP 响应里,
		// 审计不重复搬运路径清单——append-only 日志防撑爆,数量足够对账)。
		detail := map[string]any{"ok": res.OK, "items": res.Items, "bytes": res.Bytes, "error": res.Detail}
		if len(res.Machines) > 0 {
			ok := 0
			for _, m := range res.Machines {
				if m.OK {
					ok++
				}
			}
			detail["machinesOk"] = ok
			detail["machinesTotal"] = len(res.Machines)
		}
		if len(res.Steps) > 0 {
			detail["steps"] = len(res.Steps)
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor:      auditActor,
			Action:     audit.ActionMaintenanceCleanup,
			TargetType: "maintenance",
			TargetID:   res.Target,
			Detail:     detail,
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, res)
	}
}
