// pveadmin.go 是「PVE 集群纳管与管理」的 HTTP 面。
//   - 注册与管理(admin 组,/api/pve/*):集群列表/详情(PVE 物理机登记时探测自动生成)、
//     Token 配置(vault 密封,响应绝无密文,仅 hasToken 布尔)、测连、只读开关、删除;
//   - 管理面(admin 组):集群总览、VM/LXC 列表、电源操作(默认只读,解锁显式留审计,
//     受理与拒绝均审计)、任务中心与单任务状态轮询(UPID);
//   - 服务器池(受保护组,GET /api/server-pools):只读池列表 + 每池机器数,供成员端
//     服务器列表分组展示;池由系统管理(PVE 探测自动建),无 CRUD 端点。
//
// 纪律:Token 绝不入响应/审计/日志;电源操作 readonly 未解锁 → 409 且留审计;
// UPID 经 query 参数传输(UPID 含冒号等字符);请求体 ≤64KiB。
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
	"github.com/huangchengsir/pipewright/internal/pveadmin"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 审计 action(前缀 pveadmin.*);detail 绝无 Token 密文。
const (
	auditActionPVEClusterToken    = "pveadmin.cluster.token"
	auditActionPVEClusterTest     = "pveadmin.cluster.test"
	auditActionPVEClusterReadonly = "pveadmin.cluster.readonly"
	auditActionPVEClusterDelete   = "pveadmin.cluster.delete"
	auditActionPVEVMAction        = "pveadmin.vm.action"
	auditTargetPVECluster         = "pve_cluster"
	auditTargetPVEVM              = "pve_vm"
)

// mountPVEAdmin 把 PVE 管理端点挂进 admin 子组(路径前缀 /pve)。
func mountPVEAdmin(ar chi.Router, svc pveadmin.Service, aud audit.Recorder) {
	ar.Get("/pve/clusters", makePVEClusterListHandler(svc))
	ar.Get("/pve/clusters/{id}", makePVEClusterGetHandler(svc))
	ar.Put("/pve/clusters/{id}/token", makePVEClusterTokenHandler(svc, aud))
	ar.Post("/pve/clusters/{id}/test", makePVEClusterTestHandler(svc, aud))
	ar.Put("/pve/clusters/{id}/readonly", makePVEClusterReadonlyHandler(svc, aud))
	ar.Delete("/pve/clusters/{id}", makePVEClusterDeleteHandler(svc, aud))

	ar.Get("/pve/clusters/{id}/overview", makePVEOverviewHandler(svc))
	ar.Get("/pve/clusters/{id}/vms", makePVEVMListHandler(svc))
	ar.Post("/pve/clusters/{id}/vms/{node}/{vmid}/status", makePVEVMActionHandler(svc, aud))
	ar.Get("/pve/clusters/{id}/tasks", makePVETaskListHandler(svc))
	ar.Get("/pve/clusters/{id}/task", makePVETaskStatusHandler(svc))
}

// writePVEAdminError 把领域错误映射为契约错误码/状态码。
func writePVEAdminError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pveadmin.ErrNotFound):
		writeError(w, http.StatusNotFound, "pve_cluster_not_found", "PVE 集群不存在")
	case errors.Is(err, pveadmin.ErrNameTaken):
		writeError(w, http.StatusConflict, "pve_cluster_name_taken", "同名 PVE 集群已存在")
	case errors.Is(err, pveadmin.ErrInvalidInput):
		writeError(w, http.StatusUnprocessableEntity, "invalid_pve_input", "入参非法:地址/Token 格式或白名单外取值")
	case errors.Is(err, pveadmin.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法存取 API Token")
	case errors.Is(err, pveadmin.ErrClusterNotReady):
		writeError(w, http.StatusConflict, "pve_cluster_not_ready", "集群尚未配置可用的 API Token,请先在设置页完成配置")
	case errors.Is(err, pveadmin.ErrReadonly):
		writeError(w, http.StatusConflict, "pve_readonly", "集群处于只读模式,请先在设置页解锁后再执行电源操作")
	case errors.Is(err, pveadmin.ErrConnectFailed):
		writeError(w, http.StatusBadGateway, "pve_connect_failed", "PVE API 请求失败:"+pveErrTail(err))
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// pveErrTail 取错误人话(剥领域前缀、限长;领域层已保证不含 Token/密文)。
func pveErrTail(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "pveadmin: ")
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

// makePVEClusterListHandler 返回 GET /api/pve/clusters → { items: [...] }(只读,无审计)。
func makePVEClusterListHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		list, err := svc.ListClusters(r.Context())
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": list})
	}
}

// makePVEClusterGetHandler 返回 GET /api/pve/clusters/{id} → 集群视图。
func makePVEClusterGetHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		v, err := svc.GetCluster(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

// makePVEClusterTokenHandler 返回 PUT /api/pve/clusters/{id}/token
// (保存 Token(密封)并立即测连;成功 → ready,失败 → 409/502 但 Token 已保存)。
func makePVEClusterTokenHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			APIURL         string `json:"apiUrl"`
			TokenName      string `json:"tokenName"`
			TokenSecret    string `json:"tokenSecret"`
			TLSMode        string `json:"tlsMode"`
			TLSFingerprint string `json:"tlsFingerprint"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.SetToken(r.Context(), id, req.APIURL, req.TokenName, req.TokenSecret, req.TLSMode, req.TLSFingerprint); err != nil {
			writePVEAdminError(w, err) // Token 已保存的测连失败也走这里(错误体不含密文)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEClusterToken,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "配置 API Token 并测连通过"}, IP: clientIP(r)}) // 绝无 Token 值
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makePVEClusterTestHandler 返回 POST /api/pve/clusters/{id}/test(实连 /version)。
func makePVEClusterTestHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.TestCluster(r.Context(), id); err != nil {
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEClusterTest,
			TargetType: auditTargetPVECluster, TargetID: id, Detail: map[string]any{"result": "连通测试通过"}, IP: clientIP(r)})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makePVEClusterReadonlyHandler 返回 PUT /api/pve/clusters/{id}/readonly {readonly:bool}
// (电源操作闸门;解锁是显式、留审计的独立动作)。
func makePVEClusterReadonlyHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Readonly *bool `json:"readonly"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Readonly == nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体须含 readonly 布尔")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.SetReadonly(r.Context(), id, *req.Readonly); err != nil {
			writePVEAdminError(w, err)
			return
		}
		detail := "切换为只读"
		if !*req.Readonly {
			detail = "解锁写模式(允许电源操作)"
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEClusterReadonly,
			TargetType: auditTargetPVECluster, TargetID: id, Detail: map[string]any{"result": detail}, IP: clientIP(r)})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makePVEClusterDeleteHandler 返回 DELETE /api/pve/clusters/{id}
// (池内机器归位 default/standard,删池与集群行;Token 密文随行删除)。
func makePVEClusterDeleteHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.DeleteCluster(r.Context(), id); err != nil {
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEClusterDelete,
			TargetType: auditTargetPVECluster, TargetID: id, Detail: map[string]any{"result": "删除集群与池,机器归位标准池"}, IP: clientIP(r)})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makePVEOverviewHandler 返回 GET /api/pve/clusters/{id}/overview(版本/quorum/节点/资源)。
func makePVEOverviewHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		ov, err := svc.Overview(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ov)
	}
}

// makePVEVMListHandler 返回 GET /api/pve/clusters/{id}/vms → { items: [...] }。
func makePVEVMListHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		vms, err := svc.ListVMs(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": vms})
	}
}

// makePVEVMActionHandler 返回 POST /api/pve/clusters/{id}/vms/{node}/{vmid}/status
// {action,kind} → {node,upid}。readonly 未解锁 → 409 且留审计(拒绝也留痕)。
func makePVEVMActionHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Action string `json:"action"`
			Kind   string `json:"kind"` // qemu | lxc(前端列表自带)
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		node := chi.URLParam(r, "node")
		vmid, err := strconv.ParseInt(chi.URLParam(r, "vmid"), 10, 64)
		if err != nil || vmid < 100 || vmid > 999999999 {
			writeError(w, http.StatusBadRequest, "invalid_pve_input", "vmid 非法")
			return
		}
		ref, aerr := svc.VMAction(r.Context(), id, node, req.Kind, vmid, req.Action)
		if aerr != nil {
			if errors.Is(aerr, pveadmin.ErrReadonly) {
				// 只读拒绝也是一次「有人试图操作」的事实,留审计(不含凭据)。
				recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEVMAction,
					TargetType: auditTargetPVEVM, TargetID: node + "/" + strconv.FormatInt(vmid, 10),
					Detail: map[string]any{"result": "拒绝(只读模式)", "action": req.Action}, IP: clientIP(r)})
			}
			writePVEAdminError(w, aerr)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEVMAction,
			TargetType: auditTargetPVEVM, TargetID: node + "/" + strconv.FormatInt(vmid, 10),
			Detail: map[string]any{"result": "受理", "action": req.Action, "kind": req.Kind, "upid": ref.UPID}, IP: clientIP(r)})
		writeJSON(w, http.StatusAccepted, ref)
	}
}

// makePVETaskListHandler 返回 GET /api/pve/clusters/{id}/tasks?limit= → { items: [...] }。
func makePVETaskListHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		tasks, err := svc.ListTasks(r.Context(), chi.URLParam(r, "id"), limit)
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": tasks})
	}
}

// makePVETaskStatusHandler 返回 GET /api/pve/clusters/{id}/task?node=&upid=(前端轮询至 done)。
// UPID 经 query 传输(UPID 含冒号,不便入路径)。
func makePVETaskStatusHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		node := r.URL.Query().Get("node")
		upid := r.URL.Query().Get("upid")
		if node == "" || upid == "" {
			writeError(w, http.StatusBadRequest, "invalid_pve_input", "缺少 node/upid 查询参数")
			return
		}
		ts, err := svc.TaskStatus(r.Context(), chi.URLParam(r, "id"), node, upid)
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ts)
	}
}

// makeServerPoolListHandler 返回 GET /api/server-pools → { items: [...] }(受保护组,
// 成员可见只读;供服务器列表分组展示)。svc 为 nil(target 未注入)→ 503。
func makeServerPoolListHandler(svc target.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务器服务未初始化")
			return
		}
		pools, err := svc.ListPools(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		out := make([]serverPoolDTO, 0, len(pools))
		for _, p := range pools {
			out = append(out, serverPoolDTO{
				ID: p.ID, Name: p.Name, Kind: p.Kind, ClusterID: p.ClusterID,
				ServerCount: p.ServerCount, CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// serverPoolDTO 是服务器池对外契约(default 标准池 + PVE 集群池;只读)。
type serverPoolDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"` // standard | pve
	ClusterID   string `json:"clusterId"`
	ServerCount int    `json:"serverCount"`
	CreatedAt   string `json:"createdAt"`
}
