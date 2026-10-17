// ceph.go 是 Ceph 管理面(admin session 鉴权,挂受保护 /api 组内 requireAdmin 子组):
//   - GET/POST /api/ceph/clusters, GET/DELETE /api/ceph/clusters/{id}
//     :集群登记(向导消费)与解除纳管;
//   - POST /api/ceph/clusters/{id}/deploy, GET .../deploy/status
//     :cephadm 部署管线(202 异步触发 + 轮询快照);
//   - GET .../status :集群健康/容量(TTL 缓存);
//   - GET/POST .../hosts, DELETE .../hosts/{host}   :主机纳管/移除(drain);
//   - GET/POST .../osds, DELETE .../osds/{osdId}    :OSD 增删(安全移除);
//   - pools / pools/{name}/images / images/{image}/snapshots / filesystems/.../subvolumes
//     :存储池、RBD 块设备(含快照/回滚)、CephFS 子卷管理。
//
// 写方法过 CSRF + 审计;移除/回滚类动作在前端有强确认,后端不再二次拦截。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/ceph"
)

// mountCeph 把 Ceph 管理端点挂进 admin 组(路径前缀 /ceph)。
func mountCeph(ar chi.Router, svc ceph.Service, aud audit.Recorder) {
	ar.Get("/ceph/clusters", makeCephListClustersHandler(svc))
	ar.Post("/ceph/clusters", makeCephCreateClusterHandler(svc, aud))
	ar.Get("/ceph/clusters/{id}", makeCephGetClusterHandler(svc))
	ar.Delete("/ceph/clusters/{id}", makeCephDeleteClusterHandler(svc, aud))
	ar.Post("/ceph/clusters/{id}/deploy", makeCephDeployHandler(svc, aud))
	ar.Get("/ceph/clusters/{id}/deploy/status", makeCephDeployStatusHandler(svc))
	ar.Get("/ceph/clusters/{id}/status", makeCephStatusHandler(svc))

	ar.Get("/ceph/clusters/{id}/hosts", makeCephListHostsHandler(svc))
	ar.Post("/ceph/clusters/{id}/hosts", makeCephAddHostHandler(svc, aud))
	ar.Delete("/ceph/clusters/{id}/hosts/{host}", makeCephRemoveHostHandler(svc, aud))
	ar.Get("/ceph/clusters/{id}/osds", makeCephListOSDsHandler(svc))
	ar.Post("/ceph/clusters/{id}/osds", makeCephAddOSDHandler(svc, aud))
	ar.Delete("/ceph/clusters/{id}/osds/{osdId}", makeCephRemoveOSDHandler(svc, aud))

	ar.Get("/ceph/clusters/{id}/pools", makeCephListPoolsHandler(svc))
	ar.Post("/ceph/clusters/{id}/pools", makeCephCreatePoolHandler(svc, aud))
	ar.Patch("/ceph/clusters/{id}/pools/{name}", makeCephUpdatePoolHandler(svc, aud))
	ar.Delete("/ceph/clusters/{id}/pools/{name}", makeCephDeletePoolHandler(svc, aud))

	ar.Get("/ceph/clusters/{id}/pools/{name}/images", makeCephListImagesHandler(svc))
	ar.Post("/ceph/clusters/{id}/pools/{name}/images", makeCephCreateImageHandler(svc, aud))
	ar.Patch("/ceph/clusters/{id}/pools/{name}/images/{image}", makeCephResizeImageHandler(svc, aud))
	ar.Delete("/ceph/clusters/{id}/pools/{name}/images/{image}", makeCephDeleteImageHandler(svc, aud))
	ar.Get("/ceph/clusters/{id}/pools/{name}/images/{image}/snapshots", makeCephListSnapshotsHandler(svc))
	ar.Post("/ceph/clusters/{id}/pools/{name}/images/{image}/snapshots", makeCephCreateSnapshotHandler(svc, aud))
	ar.Delete("/ceph/clusters/{id}/pools/{name}/images/{image}/snapshots/{snap}", makeCephDeleteSnapshotHandler(svc, aud))
	ar.Post("/ceph/clusters/{id}/pools/{name}/images/{image}/snapshots/{snap}/rollback", makeCephRollbackSnapshotHandler(svc, aud))

	ar.Get("/ceph/clusters/{id}/filesystems", makeCephListFilesystemsHandler(svc))
	ar.Get("/ceph/clusters/{id}/filesystems/{fs}/subvolumes", makeCephListSubvolumesHandler(svc))
	ar.Post("/ceph/clusters/{id}/filesystems/{fs}/subvolumes", makeCephCreateSubvolumeHandler(svc, aud))
	ar.Delete("/ceph/clusters/{id}/filesystems/{fs}/subvolumes/{name}", makeCephDeleteSubvolumeHandler(svc, aud))
}

// writeCephError 把域错误映射为 HTTP 状态(错误码稳定,前端按码提示)。
func writeCephError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ceph.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ceph.ErrNameTaken):
		writeError(w, http.StatusConflict, "name_taken", err.Error())
	case errors.Is(err, ceph.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, ceph.ErrBusy):
		writeError(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, ceph.ErrClusterNotReady):
		writeError(w, http.StatusConflict, "cluster_not_ready", err.Error())
	case errors.Is(err, ceph.ErrNoPrivilege):
		writeError(w, http.StatusBadRequest, "no_privilege", err.Error())
	case errors.Is(err, ceph.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", err.Error())
	case errors.Is(err, ceph.ErrExec):
		writeError(w, http.StatusBadGateway, "exec_failed", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// cephClusterDTO 是集群视图(camelCase;含 bootstrap 主机名)。
type cephClusterDTO struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	FSID                  string `json:"fsid"`
	BootstrapServerID     string `json:"bootstrapServerId"`
	BootstrapServerName   string `json:"bootstrapServerName"`
	Image                 string `json:"image"`
	CephadmSource         string `json:"cephadmSource"`
	PublicNetwork         string `json:"publicNetwork"`
	ClusterNetwork        string `json:"clusterNetwork"`
	State                 string `json:"state"`
	DashboardURL          string `json:"dashboardUrl"`
	DashboardUser         string `json:"dashboardUser"`
	DashboardCredentialID string `json:"dashboardCredentialId"`
	CreatedAt             string `json:"createdAt"`
	UpdatedAt             string `json:"updatedAt"`
}

func toCephClusterDTO(c *ceph.Cluster) cephClusterDTO {
	return cephClusterDTO{
		ID: c.ID, Name: c.Name, FSID: c.FSID,
		BootstrapServerID: c.BootstrapServerID, BootstrapServerName: c.BootstrapServerName,
		Image: c.Image, CephadmSource: c.CephadmSource,
		PublicNetwork: c.PublicNetwork, ClusterNetwork: c.ClusterNetwork,
		State: c.State, DashboardURL: c.DashboardURL,
		DashboardUser: c.DashboardUser, DashboardCredentialID: c.DashboardCredentialID,
		CreatedAt: c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt: c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// decodeJSONBody 解析请求体(空体/非法 JSON → 400)。
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return false
	}
	return true
}

// auditCeph 记一条 Ceph 域审计(失败不阻断,见 recordAudit)。
func auditCeph(r *http.Request, aud audit.Recorder, action, targetID string) {
	recordAudit(r.Context(), aud, audit.Entry{
		Actor: "admin", Action: action, TargetType: "ceph_cluster", TargetID: targetID,
		IP: clientIP(r),
	})
}

// ── 集群 ─────────────────────────────────────────────────────────────────────

func makeCephListClustersHandler(svc ceph.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		cls, err := svc.ListClusters(r.Context())
		if err != nil {
			writeCephError(w, err)
			return
		}
		out := make([]cephClusterDTO, 0, len(cls))
		for _, c := range cls {
			out = append(out, toCephClusterDTO(c))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// cephHostInputDTO 是创建/追加主机的入参形态。
type cephHostInputDTO struct {
	ServerID string   `json:"serverId"`
	Roles    []string `json:"roles"`
	Labels   []string `json:"labels"`
	Devices  []string `json:"devices"`
}

type cephCreateClusterBody struct {
	Name              string             `json:"name"`
	BootstrapServerID string             `json:"bootstrapServerId"`
	Image             string             `json:"image"`
	CephadmSource     string             `json:"cephadmSource"`
	PublicNetwork     string             `json:"publicNetwork"`
	ClusterNetwork    string             `json:"clusterNetwork"`
	Hosts             []cephHostInputDTO `json:"hosts"`
}

func makeCephCreateClusterHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		var body cephCreateClusterBody
		if !decodeJSONBody(w, r, &body) {
			return
		}
		in := ceph.CreateInput{
			Name:              body.Name,
			BootstrapServerID: body.BootstrapServerID,
			Image:             body.Image,
			CephadmSource:     body.CephadmSource,
			PublicNetwork:     body.PublicNetwork,
			ClusterNetwork:    body.ClusterNetwork,
		}
		for _, h := range body.Hosts {
			in.Hosts = append(in.Hosts, ceph.HostSpec{
				ServerID: h.ServerID, Roles: h.Roles, Labels: h.Labels, Devices: h.Devices,
			})
		}
		cl, err := svc.CreateCluster(r.Context(), in)
		if err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_cluster_create", cl.ID)
		writeJSON(w, http.StatusCreated, toCephClusterDTO(cl))
	}
}

func makeCephGetClusterHandler(svc ceph.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		cl, err := svc.GetCluster(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeCephError(w, err)
			return
		}
		st, _ := svc.DeployStatus(r.Context(), cl.ID)
		writeJSON(w, http.StatusOK, map[string]any{"cluster": toCephClusterDTO(cl), "deploy": st})
	}
}

func makeCephDeleteClusterHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.DeleteCluster(r.Context(), id); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_cluster_delete", id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeCephDeployHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.StartDeploy(r.Context(), id); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_cluster_deploy", id)
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	}
}

func makeCephDeployStatusHandler(svc ceph.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		st, err := svc.DeployStatus(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeCephError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

func makeCephStatusHandler(svc ceph.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		st, err := svc.ClusterStatus(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeCephError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// ── 主机 / OSD ───────────────────────────────────────────────────────────────

func makeCephListHostsHandler(svc ceph.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		hosts, err := svc.ListHosts(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeCephError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": hosts})
	}
}

type cephAddHostBody struct {
	ServerID string   `json:"serverId"`
	Labels   []string `json:"labels"`
	Devices  []string `json:"devices"`
}

func makeCephAddHostHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		var body cephAddHostBody
		if !decodeJSONBody(w, r, &body) {
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.AddHost(r.Context(), id, ceph.AddHostInput{
			ServerID: body.ServerID, Labels: body.Labels, Devices: body.Devices,
		}); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_host_add", id+"/"+body.ServerID)
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	}
}

func makeCephRemoveHostHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		id, host := chi.URLParam(r, "id"), chi.URLParam(r, "host")
		if err := svc.RemoveHost(r.Context(), id, host); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_host_remove", id+"/"+host)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeCephListOSDsHandler(svc ceph.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		osds, err := svc.ListOSDs(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeCephError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": osds})
	}
}

type cephAddOSDBody struct {
	Host   string `json:"host"`
	Device string `json:"device"`
}

func makeCephAddOSDHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		var body cephAddOSDBody
		if !decodeJSONBody(w, r, &body) {
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.AddOSD(r.Context(), id, ceph.AddOSDInput{Host: body.Host, Device: body.Device}); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_osd_add", id+"/"+body.Host+":"+body.Device)
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	}
}

func makeCephRemoveOSDHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		osdID, err := strconv.ParseInt(chi.URLParam(r, "osdId"), 10, 64)
		if err != nil || osdID < 0 {
			writeError(w, http.StatusBadRequest, "invalid_input", "非法 OSD 编号")
			return
		}
		if err := svc.RemoveOSD(r.Context(), id, osdID); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_osd_remove", id+"/osd."+strconv.FormatInt(osdID, 10))
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ── 存储池 ───────────────────────────────────────────────────────────────────

func makeCephListPoolsHandler(svc ceph.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		pools, err := svc.ListPools(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeCephError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": pools})
	}
}

func makeCephCreatePoolHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		var body ceph.PoolCreateInput
		if !decodeJSONBody(w, r, &body) {
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.CreatePool(r.Context(), id, body); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_pool_create", id+"/"+body.Name)
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	}
}

func makeCephUpdatePoolHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		var body ceph.PoolUpdateInput
		if !decodeJSONBody(w, r, &body) {
			return
		}
		id, name := chi.URLParam(r, "id"), chi.URLParam(r, "name")
		if err := svc.UpdatePool(r.Context(), id, name, body); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_pool_update", id+"/"+name)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeCephDeletePoolHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		id, name := chi.URLParam(r, "id"), chi.URLParam(r, "name")
		if err := svc.DeletePool(r.Context(), id, name); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_pool_delete", id+"/"+name)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ── RBD ──────────────────────────────────────────────────────────────────────

func makeCephListImagesHandler(svc ceph.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		images, err := svc.ListImages(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "name"))
		if err != nil {
			writeCephError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": images})
	}
}

func makeCephCreateImageHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		var body ceph.ImageCreateInput
		if !decodeJSONBody(w, r, &body) {
			return
		}
		id, pool := chi.URLParam(r, "id"), chi.URLParam(r, "name")
		if err := svc.CreateImage(r.Context(), id, pool, body); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_rbd_create", id+"/"+pool+"/"+body.Name)
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	}
}

func makeCephResizeImageHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		var body ceph.ImageResizeInput
		if !decodeJSONBody(w, r, &body) {
			return
		}
		id, pool, image := chi.URLParam(r, "id"), chi.URLParam(r, "name"), chi.URLParam(r, "image")
		if err := svc.ResizeImage(r.Context(), id, pool, image, body); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_rbd_resize", id+"/"+pool+"/"+image)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeCephDeleteImageHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		id, pool, image := chi.URLParam(r, "id"), chi.URLParam(r, "name"), chi.URLParam(r, "image")
		if err := svc.DeleteImage(r.Context(), id, pool, image); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_rbd_delete", id+"/"+pool+"/"+image)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeCephListSnapshotsHandler(svc ceph.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		snaps, err := svc.ListSnapshots(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "name"), chi.URLParam(r, "image"))
		if err != nil {
			writeCephError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": snaps})
	}
}

type cephSnapBody struct {
	Name string `json:"name"`
}

func makeCephCreateSnapshotHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		var body cephSnapBody
		if !decodeJSONBody(w, r, &body) {
			return
		}
		id, pool, image := chi.URLParam(r, "id"), chi.URLParam(r, "name"), chi.URLParam(r, "image")
		if err := svc.CreateSnapshot(r.Context(), id, pool, image, body.Name); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_snap_create", id+"/"+pool+"/"+image+"@"+body.Name)
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	}
}

func makeCephDeleteSnapshotHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		id, pool, image, snap := chi.URLParam(r, "id"), chi.URLParam(r, "name"), chi.URLParam(r, "image"), chi.URLParam(r, "snap")
		if err := svc.DeleteSnapshot(r.Context(), id, pool, image, snap); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_snap_delete", id+"/"+pool+"/"+image+"@"+snap)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeCephRollbackSnapshotHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		id, pool, image, snap := chi.URLParam(r, "id"), chi.URLParam(r, "name"), chi.URLParam(r, "image"), chi.URLParam(r, "snap")
		if err := svc.RollbackSnapshot(r.Context(), id, pool, image, snap); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_snap_rollback", id+"/"+pool+"/"+image+"@"+snap)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ── CephFS ───────────────────────────────────────────────────────────────────

func makeCephListFilesystemsHandler(svc ceph.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		fss, err := svc.ListFilesystems(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeCephError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": fss})
	}
}

func makeCephListSubvolumesHandler(svc ceph.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		svs, err := svc.ListSubvolumes(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "fs"))
		if err != nil {
			writeCephError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": svs})
	}
}

func makeCephCreateSubvolumeHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		var body ceph.SubvolumeCreateInput
		if !decodeJSONBody(w, r, &body) {
			return
		}
		id, fs := chi.URLParam(r, "id"), chi.URLParam(r, "fs")
		if err := svc.CreateSubvolume(r.Context(), id, fs, body); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_subvolume_create", id+"/"+fs+"/"+body.Name)
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	}
}

func makeCephDeleteSubvolumeHandler(svc ceph.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Ceph 服务未初始化")
			return
		}
		id, fs, name := chi.URLParam(r, "id"), chi.URLParam(r, "fs"), chi.URLParam(r, "name")
		if err := svc.DeleteSubvolume(r.Context(), id, fs, name); err != nil {
			writeCephError(w, err)
			return
		}
		auditCeph(r, aud, "ceph_subvolume_delete", id+"/"+fs+"/"+name)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
