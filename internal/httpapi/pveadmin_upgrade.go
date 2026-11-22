// pveadmin_upgrade.go — PVE 升级 HTTP 面(admin 组):更新矩阵/索引刷新/日常升级任务、
// 节点重启、apt 仓库管理(单节点 + 批量)、大版本 8→9 滚动升级(预检/启动/快照/中止/重试/续跑)。
//
// 纪律:写操作 readonly 拒绝与受理均留审计;审计 detail 只含 node/preset/action/confirm
// 结果等非密字段,绝不含 Token/凭据;请求体 ≤64KiB;危险动作(重启/大版本)输入确认在领域层。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/pveadmin"
)

// 升级面审计 action。
const (
	auditActionPVEUpgradeRefresh = "pveadmin.upgrade.refresh"
	auditActionPVEUpgradeStart   = "pveadmin.upgrade.start"
	auditActionPVENodeReboot     = "pveadmin.node.reboot"
	auditActionPVEAptToggle      = "pveadmin.apt.toggle"
	auditActionPVEAptMirror      = "pveadmin.apt.mirror"
	auditActionPVEAptPreset      = "pveadmin.apt.preset"
	auditActionPVEAptRestore     = "pveadmin.apt.restore"
	auditActionPVEAptApply       = "pveadmin.apt.apply"
	auditActionPVEMajorStart     = "pveadmin.upgrade.major.start"
	auditActionPVEMajorAbort     = "pveadmin.upgrade.major.abort"
	auditActionPVEMajorRetry     = "pveadmin.upgrade.major.retry"
	auditActionPVEMajorResume    = "pveadmin.upgrade.major.resume"
)

// mountPVEUpgrade 把升级面端点挂进 admin 子组(由 mountPVEAdmin 调用)。
func mountPVEUpgrade(ar chi.Router, svc pveadmin.Service, aud audit.Recorder) {
	ar.Get("/pve/clusters/{id}/updates", makePVEUpdatesHandler(svc))
	ar.Post("/pve/clusters/{id}/updates/refresh", makePVEUpdatesRefreshHandler(svc, aud))
	ar.Post("/pve/clusters/{id}/upgrade", makePVEUpgradeStartHandler(svc, aud))
	ar.Get("/pve/clusters/{id}/upgrade-status", makePVEUpgradeStatusHandler(svc))
	ar.Post("/pve/clusters/{id}/nodes/{node}/reboot", makePVENodeRebootHandler(svc, aud))

	ar.Get("/pve/clusters/{id}/nodes/{node}/apt-repos", makePVEAptReposHandler(svc))
	ar.Post("/pve/clusters/{id}/nodes/{node}/apt-repos/toggle", makePVEAptToggleHandler(svc, aud))
	ar.Post("/pve/clusters/{id}/nodes/{node}/apt-repos/mirror", makePVEAptMirrorHandler(svc, aud))
	ar.Post("/pve/clusters/{id}/nodes/{node}/apt-repos/preset", makePVEAptPresetHandler(svc, aud))
	ar.Post("/pve/clusters/{id}/nodes/{node}/apt-repos/restore", makePVEAptRestoreHandler(svc, aud))
	ar.Post("/pve/clusters/{id}/apt-repos/apply", makePVEAptApplyHandler(svc, aud))
	ar.Get("/pve/clusters/{id}/apt-repos/overview", makePVEAptRepoOverviewHandler(svc))

	ar.Get("/pve/clusters/{id}/major-upgrade/preflight", makePVEMajorPreflightHandler(svc))
	ar.Post("/pve/clusters/{id}/major-upgrade", makePVEMajorStartHandler(svc, aud))
	ar.Get("/pve/clusters/{id}/major-upgrade-status", makePVEMajorStatusHandler(svc))
	ar.Post("/pve/clusters/{id}/major-upgrade/abort", makePVEMajorAbortHandler(svc, aud))
	ar.Post("/pve/clusters/{id}/major-upgrade/retry", makePVEMajorRetryHandler(svc, aud))
	ar.Post("/pve/clusters/{id}/major-upgrade/resume", makePVEMajorResumeHandler(svc, aud))
}

// pveUpgradeReadonlyAudit 只读拒绝留痕(拒绝也是「有人试图操作」的事实)。
func pveUpgradeReadonlyAudit(r *http.Request, aud audit.Recorder, action, id string, detail map[string]any) {
	detail["result"] = "拒绝(只读模式)"
	recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: action,
		TargetType: auditTargetPVECluster, TargetID: id, Detail: detail, IP: clientIP(r)})
}

// ---- 更新矩阵 / 索引刷新 / 日常升级 ------------------------------------------------------

// makePVEUpdatesHandler 返回 GET .../updates(逐节点版本/待更新/订阅/需重启;只读)。
func makePVEUpdatesHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		ov, err := svc.ListUpdates(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ov)
	}
}

// makePVEUpdatesRefreshHandler 返回 POST .../updates/refresh {nodes?} → 202 {items:[{node,upid}]}。
func makePVEUpdatesRefreshHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Nodes []string `json:"nodes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		items, err := svc.RefreshUpdates(r.Context(), id, req.Nodes)
		if err != nil {
			if errors.Is(err, pveadmin.ErrReadonly) {
				pveUpgradeReadonlyAudit(r, aud, auditActionPVEUpgradeRefresh, id, map[string]any{"nodes": req.Nodes})
			}
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEUpgradeRefresh,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "nodes": req.Nodes}, IP: clientIP(r)})
		writeJSON(w, http.StatusAccepted, map[string]any{"items": items})
	}
}

// makePVEUpgradeStartHandler 返回 POST .../upgrade {nodes?} → 202 {ok:true}(异步串行任务)。
func makePVEUpgradeStartHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var in pveadmin.UpgradeInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.StartUpgrade(r.Context(), id, in); err != nil {
			if errors.Is(err, pveadmin.ErrReadonly) {
				pveUpgradeReadonlyAudit(r, aud, auditActionPVEUpgradeStart, id, map[string]any{"nodes": in.Nodes})
			}
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEUpgradeStart,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "nodes": in.Nodes}, IP: clientIP(r)})
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
	}
}

// makePVEUpgradeStatusHandler 返回 GET .../upgrade-status(前端轮询至 done)。
func makePVEUpgradeStatusHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		st, err := svc.UpgradeStatus(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// makePVENodeRebootHandler 返回 POST .../nodes/{node}/reboot {confirm} → 202 {node,upid}。
// confirm 须原样等于节点名(领域层校验);拒绝与受理均留审计。
func makePVENodeRebootHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Confirm string `json:"confirm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		node := chi.URLParam(r, "node")
		ref, err := svc.RebootNode(r.Context(), id, node, req.Confirm)
		if err != nil {
			if errors.Is(err, pveadmin.ErrReadonly) {
				pveUpgradeReadonlyAudit(r, aud, auditActionPVENodeReboot, id, map[string]any{"node": node})
			}
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVENodeReboot,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "node": node, "upid": ref.UPID}, IP: clientIP(r)})
		writeJSON(w, http.StatusAccepted, ref)
	}
}

// ---- apt 仓库管理 -----------------------------------------------------------------------

// makePVEAptReposHandler 返回 GET .../nodes/{node}/apt-repos(清单 + 警告;只读)。
func makePVEAptReposHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		inv, err := svc.ListAptRepos(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "node"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, inv)
	}
}

// makePVEAptRepoOverviewHandler 返回 GET .../apt-repos/overview(集群仓库总览;只读)。
func makePVEAptRepoOverviewHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		ov, err := svc.ListAptRepoOverview(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ov)
	}
}

// makePVEAptToggleHandler 返回 POST .../apt-repos/toggle {path,key,enabled} → 写入结果。
func makePVEAptToggleHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var in pveadmin.AptRepoToggleInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		node := chi.URLParam(r, "node")
		res, err := svc.ToggleAptRepo(r.Context(), id, node, in)
		if err != nil {
			if errors.Is(err, pveadmin.ErrReadonly) {
				pveUpgradeReadonlyAudit(r, aud, auditActionPVEAptToggle, id, map[string]any{"node": node, "path": in.Path, "enabled": in.Enabled})
			}
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEAptToggle,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "node": node, "path": in.Path, "key": in.Key, "enabled": in.Enabled, "updateOk": res.UpdateOK}, IP: clientIP(r)})
		writeJSON(w, http.StatusOK, res)
	}
}

// makePVEAptMirrorHandler 返回 POST .../apt-repos/mirror {preset} → 写入结果。
func makePVEAptMirrorHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Preset string `json:"preset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		node := chi.URLParam(r, "node")
		res, err := svc.SetAptRepoMirror(r.Context(), id, node, req.Preset)
		if err != nil {
			if errors.Is(err, pveadmin.ErrReadonly) {
				pveUpgradeReadonlyAudit(r, aud, auditActionPVEAptMirror, id, map[string]any{"node": node, "preset": req.Preset})
			}
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEAptMirror,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "node": node, "preset": req.Preset, "updateOk": res.UpdateOK}, IP: clientIP(r)})
		writeJSON(w, http.StatusOK, res)
	}
}

// makePVEAptPresetHandler 返回 POST .../apt-repos/preset {preset} → 写入结果。
func makePVEAptPresetHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Preset string `json:"preset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		node := chi.URLParam(r, "node")
		res, err := svc.SetAptRepoPreset(r.Context(), id, node, req.Preset)
		if err != nil {
			if errors.Is(err, pveadmin.ErrReadonly) {
				pveUpgradeReadonlyAudit(r, aud, auditActionPVEAptPreset, id, map[string]any{"node": node, "preset": req.Preset})
			}
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEAptPreset,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "node": node, "preset": req.Preset, "updateOk": res.UpdateOK}, IP: clientIP(r)})
		writeJSON(w, http.StatusOK, res)
	}
}

// makePVEAptRestoreHandler 返回 POST .../apt-repos/restore {path,backupPath} → 写入结果。
func makePVEAptRestoreHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Path       string `json:"path"`
			BackupPath string `json:"backupPath"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		node := chi.URLParam(r, "node")
		res, err := svc.RestoreAptRepo(r.Context(), id, node, req.Path, req.BackupPath)
		if err != nil {
			if errors.Is(err, pveadmin.ErrReadonly) {
				pveUpgradeReadonlyAudit(r, aud, auditActionPVEAptRestore, id, map[string]any{"node": node, "path": req.Path})
			}
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEAptRestore,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "node": node, "path": req.Path, "backupPath": req.BackupPath, "updateOk": res.UpdateOK}, IP: clientIP(r)})
		writeJSON(w, http.StatusOK, res)
	}
}

// makePVEAptApplyHandler 返回 POST .../apt-repos/apply {action,preset,nodes?} → {items}(批量)。
func makePVEAptApplyHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var in pveadmin.AptRepoApplyInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		items, err := svc.ApplyAptRepos(r.Context(), id, in)
		if err != nil {
			if errors.Is(err, pveadmin.ErrReadonly) {
				pveUpgradeReadonlyAudit(r, aud, auditActionPVEAptApply, id, map[string]any{"action": in.Action, "preset": in.Preset})
			}
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEAptApply,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "action": in.Action, "preset": in.Preset, "nodes": in.Nodes}, IP: clientIP(r)})
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// ---- 大版本升级 -------------------------------------------------------------------------

// makePVEMajorPreflightHandler 返回 GET .../major-upgrade/preflight(同步预检报告;只读)。
func makePVEMajorPreflightHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		rep, err := svc.MajorPreflight(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
	}
}

// makePVEMajorStartHandler 返回 POST .../major-upgrade {confirm,acks,guestPolicy,nodes?} → 202。
func makePVEMajorStartHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var in pveadmin.MajorUpgradeInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.StartMajorUpgrade(r.Context(), id, in); err != nil {
			if errors.Is(err, pveadmin.ErrReadonly) {
				pveUpgradeReadonlyAudit(r, aud, auditActionPVEMajorStart, id, map[string]any{"guestPolicy": in.GuestPolicy})
			}
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEMajorStart,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "guestPolicy": in.GuestPolicy, "nodes": in.Nodes, "acks": in.Acks}, IP: clientIP(r)})
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
	}
}

// makePVEMajorStatusHandler 返回 GET .../major-upgrade-status(DB-backed 快照)。
func makePVEMajorStatusHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		st, err := svc.MajorUpgradeStatus(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// makePVEMajorAbortHandler 返回 POST .../major-upgrade/abort(步骤边界生效)。
func makePVEMajorAbortHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.AbortMajorUpgrade(r.Context(), id); err != nil {
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEMajorAbort,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理(步骤边界生效)"}, IP: clientIP(r)})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makePVEMajorRetryHandler 返回 POST .../major-upgrade/retry {node,confirm} → 202。
func makePVEMajorRetryHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Node    string `json:"node"`
			Confirm string `json:"confirm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.RetryMajorNode(r.Context(), id, req.Node, req.Confirm); err != nil {
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEMajorRetry,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "node": req.Node}, IP: clientIP(r)})
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
	}
}

// makePVEMajorResumeHandler 返回 POST .../major-upgrade/resume {confirm} → 202。
func makePVEMajorResumeHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Confirm string `json:"confirm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.ResumeMajorUpgrade(r.Context(), id, req.Confirm); err != nil {
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEMajorResume,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理"}, IP: clientIP(r)})
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
	}
}
