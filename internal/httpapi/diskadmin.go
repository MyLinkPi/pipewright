// diskadmin.go 是「服务器磁盘管理」的 HTTP 面(admin session 鉴权,挂受保护 /api 组的
// requireAdmin 子组,路径前缀 /api/diskadmin/servers/{id}):
//   - 拓扑与解锁:GET topology、POST unlock / lock(解锁 30 分钟 TTL,进程内状态);
//   - 分区:create / delete / grow(末分区扩满) / shrink(ext4 离线、btrfs 在线);
//   - 文件系统:format / mount(fstab 持久化) / umount;
//   - LVM:pv / vg / lv 三个动作端点(action 判别)。
//
// 纪律:写操作先过解锁闸门 + 实时拓扑守卫(领域层);全部变更留审计;危险操作要求
// confirm 字段逐字等于目标路径(领域层校验);错误体可带远端 stderr 尾部,绝无凭据。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/diskadmin"
)

// 审计 action(前缀 diskadmin.*)。
const (
	auditActionDiskUnlock          = "diskadmin.server.unlock"
	auditActionDiskLock            = "diskadmin.server.lock"
	auditActionDiskPartitionCreate = "diskadmin.partition.create"
	auditActionDiskPartitionDelete = "diskadmin.partition.delete"
	auditActionDiskPartitionGrow   = "diskadmin.partition.grow"
	auditActionDiskShrink          = "diskadmin.partition.shrink"
	auditActionDiskFormat          = "diskadmin.format"
	auditActionDiskMount           = "diskadmin.mount"
	auditActionDiskUmount          = "diskadmin.umount"
	auditActionDiskPV              = "diskadmin.pv.op"
	auditActionDiskVG              = "diskadmin.vg.op"
	auditActionDiskLV              = "diskadmin.lv.op"
	auditTargetDiskServer          = "disk_server"
)

// mountDiskAdmin 把磁盘管理端点挂进 admin 子组。
func mountDiskAdmin(ar chi.Router, svc diskadmin.Service, rec audit.Recorder) {
	ar.Route("/diskadmin/servers/{id}", func(r chi.Router) {
		r.Get("/topology", makeDiskTopologyHandler(svc))
		r.Post("/unlock", makeDiskUnlockHandler(svc, rec))
		r.Post("/lock", makeDiskLockHandler(svc, rec))
		r.Post("/partitions", makeDiskPartitionCreateHandler(svc, rec))
		r.Post("/partitions/delete", makeDiskPartitionDeleteHandler(svc, rec))
		r.Post("/partitions/grow", makeDiskPartitionGrowHandler(svc, rec))
		r.Post("/partitions/shrink", makeDiskShrinkHandler(svc, rec))
		r.Post("/format", makeDiskFormatHandler(svc, rec))
		r.Post("/mount", makeDiskMountHandler(svc, rec))
		r.Post("/umount", makeDiskUmountHandler(svc, rec))
		r.Post("/lvm/pv", makeDiskPVHandler(svc, rec))
		r.Post("/lvm/vg", makeDiskVGHandler(svc, rec))
		r.Post("/lvm/lv", makeDiskLVHandler(svc, rec))
	})
}

// writeDiskAdminError 把领域错误映射为契约错误码/状态码(错误体可带 stderr 尾部)。
func writeDiskAdminError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, diskadmin.ErrReadonly):
		writeError(w, http.StatusForbidden, "disk_readonly", "该服务器处于只读模式,请先解锁写模式")
	case errors.Is(err, diskadmin.ErrServerNotFound):
		writeError(w, http.StatusNotFound, "disk_server_not_found", err.Error())
	case errors.Is(err, diskadmin.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_disk_input", err.Error())
	case errors.Is(err, diskadmin.ErrConfirmMismatch):
		writeError(w, http.StatusBadRequest, "disk_confirm_mismatch", err.Error())
	case errors.Is(err, diskadmin.ErrDeviceNotFound):
		writeError(w, http.StatusNotFound, "disk_device_not_found", err.Error())
	case errors.Is(err, diskadmin.ErrDeviceBusy), errors.Is(err, diskadmin.ErrAlreadyMounted),
		errors.Is(err, diskadmin.ErrMountpointTaken), errors.Is(err, diskadmin.ErrNotLastPartition):
		writeError(w, http.StatusConflict, "disk_device_busy", err.Error())
	case errors.Is(err, diskadmin.ErrMountRequired):
		writeError(w, http.StatusUnprocessableEntity, "disk_mount_required", err.Error())
	case errors.Is(err, diskadmin.ErrProtectedDevice):
		writeError(w, http.StatusConflict, "disk_protected", err.Error())
	case errors.Is(err, diskadmin.ErrFSTypeUnsupported):
		writeError(w, http.StatusUnprocessableEntity, "disk_fstype_unsupported", err.Error())
	case errors.Is(err, diskadmin.ErrLvmMissing):
		writeError(w, http.StatusUnprocessableEntity, "disk_lvm_missing", err.Error())
	case errors.Is(err, diskadmin.ErrNoPrivilege):
		writeError(w, http.StatusUnprocessableEntity, "disk_no_privilege", err.Error())
	case errors.Is(err, diskadmin.ErrFstabVerify):
		writeError(w, http.StatusBadGateway, "disk_fstab_verify", err.Error())
	case errors.Is(err, diskadmin.ErrExec):
		writeError(w, http.StatusBadGateway, "disk_exec_failed", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// writeDiskOpResult 写操作成功响应:{ ok:true, topology? }(topology 尽力带回,失败
// 不影响 ok —— 前端拿到 null 时自行重拉)。
func writeDiskOpResult(w http.ResponseWriter, r *http.Request, svc diskadmin.Service, serverID string) {
	var topo any
	if t, err := svc.Topology(r.Context(), serverID); err == nil {
		topo = t
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "topology": topo})
}

// decodeDiskBody 解析有限体积 JSON 请求体(失败 → 400,写错误信封)。
func decodeDiskBody(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return false
	}
	return true
}

// diskOpDeps 是写操作 handler 的公共依赖。
type diskOpDeps struct {
	rec      audit.Recorder
	serverID string
}

// ── 拓扑与解锁 ───────────────────────────────────────────────────────────────

// makeDiskTopologyHandler 返回 GET .../topology → Topology(只读,无审计)。
func makeDiskTopologyHandler(svc diskadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		topo, err := svc.Topology(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeDiskAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, topo)
	}
}

// makeDiskUnlockHandler 返回 POST .../unlock(认证 + CSRF;审计)。续期式,30 分钟。
func makeDiskUnlockHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		st := svc.Unlock(id)
		recordAudit(r.Context(), rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskUnlock, TargetType: auditTargetDiskServer,
			TargetID: id,
			Detail:   map[string]any{"expiresAt": st.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")},
			IP:       clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"unlocked": true, "expiresAt": st.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
}

// makeDiskLockHandler 返回 POST .../lock(认证 + CSRF;审计)。立即回锁,幂等。
func makeDiskLockHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		_ = svc.Lock(id)
		recordAudit(r.Context(), rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskLock, TargetType: auditTargetDiskServer,
			TargetID: id, Detail: map[string]any{},
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"unlocked": false})
	}
}

// ── 分区 ─────────────────────────────────────────────────────────────────────

// makeDiskPartitionCreateHandler 返回 POST .../partitions。
// body: { device, sizeSpec, makeLabel }。
func makeDiskPartitionCreateHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		d := diskOpDeps{rec: rec, serverID: chi.URLParam(r, "id")}
		var in struct {
			Device    string `json:"device"`
			SizeSpec  string `json:"sizeSpec"`
			MakeLabel bool   `json:"makeLabel"`
		}
		if !decodeDiskBody(w, r, &in) {
			return
		}
		if err := svc.CreatePartition(r.Context(), d.serverID, diskadmin.PartitionCreateInput{
			Device: in.Device, SizeSpec: in.SizeSpec, MakeLabel: in.MakeLabel,
		}); err != nil {
			writeDiskAdminError(w, err)
			return
		}
		recordAudit(r.Context(), d.rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskPartitionCreate, TargetType: auditTargetDiskServer,
			TargetID: d.serverID,
			Detail:   map[string]any{"device": in.Device, "sizeSpec": in.SizeSpec, "makeLabel": in.MakeLabel},
			IP:       clientIP(r),
		})
		writeDiskOpResult(w, r, svc, d.serverID)
	}
}

// makeDiskPartitionDeleteHandler 返回 POST .../partitions/delete。
// body: { partition, confirm }。
func makeDiskPartitionDeleteHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		d := diskOpDeps{rec: rec, serverID: chi.URLParam(r, "id")}
		var in struct {
			Partition string `json:"partition"`
			Confirm   string `json:"confirm"`
		}
		if !decodeDiskBody(w, r, &in) {
			return
		}
		if err := svc.DeletePartition(r.Context(), d.serverID, diskadmin.PartitionDeleteInput{
			Partition: in.Partition, Confirm: in.Confirm,
		}); err != nil {
			writeDiskAdminError(w, err)
			return
		}
		recordAudit(r.Context(), d.rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskPartitionDelete, TargetType: auditTargetDiskServer,
			TargetID: d.serverID,
			Detail:   map[string]any{"partition": in.Partition},
			IP:       clientIP(r),
		})
		writeDiskOpResult(w, r, svc, d.serverID)
	}
}

// makeDiskPartitionGrowHandler 返回 POST .../partitions/grow。
// body: { target }(末分区扩满 + 在线扩 fs)。
func makeDiskPartitionGrowHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		d := diskOpDeps{rec: rec, serverID: chi.URLParam(r, "id")}
		var in struct {
			Target string `json:"target"`
		}
		if !decodeDiskBody(w, r, &in) {
			return
		}
		if err := svc.GrowPartition(r.Context(), d.serverID, in.Target); err != nil {
			writeDiskAdminError(w, err)
			return
		}
		recordAudit(r.Context(), d.rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskPartitionGrow, TargetType: auditTargetDiskServer,
			TargetID: d.serverID,
			Detail:   map[string]any{"partition": in.Target},
			IP:       clientIP(r),
		})
		writeDiskOpResult(w, r, svc, d.serverID)
	}
}

// makeDiskShrinkHandler 返回 POST .../partitions/shrink。
// body: { target, targetGiB, confirm }。
func makeDiskShrinkHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		d := diskOpDeps{rec: rec, serverID: chi.URLParam(r, "id")}
		var in struct {
			Target    string `json:"target"`
			TargetGiB int    `json:"targetGiB"`
			Confirm   string `json:"confirm"`
		}
		if !decodeDiskBody(w, r, &in) {
			return
		}
		if err := svc.ShrinkPartition(r.Context(), d.serverID, diskadmin.ShrinkInput{
			Target: in.Target, TargetGiB: in.TargetGiB, Confirm: in.Confirm,
		}); err != nil {
			writeDiskAdminError(w, err)
			return
		}
		recordAudit(r.Context(), d.rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskShrink, TargetType: auditTargetDiskServer,
			TargetID: d.serverID,
			Detail:   map[string]any{"partition": in.Target, "targetGiB": in.TargetGiB},
			IP:       clientIP(r),
		})
		writeDiskOpResult(w, r, svc, d.serverID)
	}
}

// ── 文件系统 ─────────────────────────────────────────────────────────────────

// makeDiskFormatHandler 返回 POST .../format。body: { target, fstype, label?, confirm }。
func makeDiskFormatHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		d := diskOpDeps{rec: rec, serverID: chi.URLParam(r, "id")}
		var in struct {
			Target  string `json:"target"`
			FSType  string `json:"fstype"`
			Label   string `json:"label"`
			Confirm string `json:"confirm"`
		}
		if !decodeDiskBody(w, r, &in) {
			return
		}
		if err := svc.Format(r.Context(), d.serverID, diskadmin.FormatInput{
			Target: in.Target, FSType: in.FSType, Label: in.Label, Confirm: in.Confirm,
		}); err != nil {
			writeDiskAdminError(w, err)
			return
		}
		recordAudit(r.Context(), d.rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskFormat, TargetType: auditTargetDiskServer,
			TargetID: d.serverID,
			Detail:   map[string]any{"target": in.Target, "fstype": in.FSType, "label": in.Label},
			IP:       clientIP(r),
		})
		writeDiskOpResult(w, r, svc, d.serverID)
	}
}

// makeDiskMountHandler 返回 POST .../mount。body: { target, mountpoint, options? }。
func makeDiskMountHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		d := diskOpDeps{rec: rec, serverID: chi.URLParam(r, "id")}
		var in struct {
			Target     string `json:"target"`
			Mountpoint string `json:"mountpoint"`
			Options    string `json:"options"`
		}
		if !decodeDiskBody(w, r, &in) {
			return
		}
		if err := svc.Mount(r.Context(), d.serverID, diskadmin.MountInput{
			Target: in.Target, Mountpoint: in.Mountpoint, Options: in.Options,
		}); err != nil {
			writeDiskAdminError(w, err)
			return
		}
		recordAudit(r.Context(), d.rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskMount, TargetType: auditTargetDiskServer,
			TargetID: d.serverID,
			Detail:   map[string]any{"target": in.Target, "mountpoint": in.Mountpoint, "options": in.Options},
			IP:       clientIP(r),
		})
		writeDiskOpResult(w, r, svc, d.serverID)
	}
}

// makeDiskUmountHandler 返回 POST .../umount。body: { mountpoint, removeDir? }。
func makeDiskUmountHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		d := diskOpDeps{rec: rec, serverID: chi.URLParam(r, "id")}
		var in struct {
			Mountpoint string `json:"mountpoint"`
			RemoveDir  bool   `json:"removeDir"`
		}
		if !decodeDiskBody(w, r, &in) {
			return
		}
		if err := svc.Unmount(r.Context(), d.serverID, diskadmin.UnmountInput{
			Mountpoint: in.Mountpoint, RemoveDir: in.RemoveDir,
		}); err != nil {
			writeDiskAdminError(w, err)
			return
		}
		recordAudit(r.Context(), d.rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskUmount, TargetType: auditTargetDiskServer,
			TargetID: d.serverID,
			Detail:   map[string]any{"mountpoint": in.Mountpoint, "removeDir": in.RemoveDir},
			IP:       clientIP(r),
		})
		writeDiskOpResult(w, r, svc, d.serverID)
	}
}

// ── LVM ──────────────────────────────────────────────────────────────────────

// makeDiskPVHandler 返回 POST .../lvm/pv。body: { action: create|remove, device, confirm? }。
func makeDiskPVHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		d := diskOpDeps{rec: rec, serverID: chi.URLParam(r, "id")}
		var in struct {
			Action  string `json:"action"`
			Device  string `json:"device"`
			Confirm string `json:"confirm"`
		}
		if !decodeDiskBody(w, r, &in) {
			return
		}
		var err error
		switch in.Action {
		case "create":
			err = svc.PVCreate(r.Context(), d.serverID, in.Device)
		case "remove":
			err = svc.PVRemove(r.Context(), d.serverID, diskadmin.RemoveTargetInput{Target: in.Device, Confirm: in.Confirm})
		default:
			writeError(w, http.StatusBadRequest, "invalid_disk_input", "action 须为 create|remove")
			return
		}
		if err != nil {
			writeDiskAdminError(w, err)
			return
		}
		recordAudit(r.Context(), d.rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskPV, TargetType: auditTargetDiskServer,
			TargetID: d.serverID,
			Detail:   map[string]any{"action": in.Action, "device": in.Device},
			IP:       clientIP(r),
		})
		writeDiskOpResult(w, r, svc, d.serverID)
	}
}

// makeDiskVGHandler 返回 POST .../lvm/vg。
// body: { action: create|extend|reduce|remove, name, devices?, device?, confirm? }。
func makeDiskVGHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		d := diskOpDeps{rec: rec, serverID: chi.URLParam(r, "id")}
		var in struct {
			Action  string   `json:"action"`
			Name    string   `json:"name"`
			Devices []string `json:"devices"`
			Device  string   `json:"device"`
			Confirm string   `json:"confirm"`
		}
		if !decodeDiskBody(w, r, &in) {
			return
		}
		var err error
		switch in.Action {
		case "create":
			err = svc.VGCreate(r.Context(), d.serverID, diskadmin.VGCreateInput{Name: in.Name, Devices: in.Devices})
		case "extend":
			err = svc.VGExtend(r.Context(), d.serverID, diskadmin.VGExtendInput{Name: in.Name, Device: in.Device})
		case "reduce":
			err = svc.VGReduce(r.Context(), d.serverID, diskadmin.VGReduceInput{Name: in.Name, Device: in.Device, Confirm: in.Confirm})
		case "remove":
			err = svc.VGRemove(r.Context(), d.serverID, diskadmin.RemoveConfirmInput{Name: in.Name, Confirm: in.Confirm})
		default:
			writeError(w, http.StatusBadRequest, "invalid_disk_input", "action 须为 create|extend|reduce|remove")
			return
		}
		if err != nil {
			writeDiskAdminError(w, err)
			return
		}
		recordAudit(r.Context(), d.rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskVG, TargetType: auditTargetDiskServer,
			TargetID: d.serverID,
			Detail:   map[string]any{"action": in.Action, "vg": in.Name, "device": in.Device, "devices": in.Devices},
			IP:       clientIP(r),
		})
		writeDiskOpResult(w, r, svc, d.serverID)
	}
}

// makeDiskLVHandler 返回 POST .../lvm/lv。body 按 action 判别:
//
//	create: { action, vg, name, sizeGiB?, useAllFree?, fstype?, mountpoint? }
//	extend: { action, path, sizeGiB?, useAllFree? }
//	reduce: { action, path, targetGiB, confirm }
//	remove: { action, path, confirm }
func makeDiskLVHandler(svc diskadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "磁盘管理服务未初始化")
			return
		}
		d := diskOpDeps{rec: rec, serverID: chi.URLParam(r, "id")}
		var in struct {
			Action     string `json:"action"`
			VG         string `json:"vg"`
			Name       string `json:"name"`
			Path       string `json:"path"`
			SizeGiB    int    `json:"sizeGiB"`
			TargetGiB  int    `json:"targetGiB"`
			UseAllFree bool   `json:"useAllFree"`
			FSType     string `json:"fstype"`
			Mountpoint string `json:"mountpoint"`
			Confirm    string `json:"confirm"`
		}
		if !decodeDiskBody(w, r, &in) {
			return
		}
		var err error
		switch in.Action {
		case "create":
			err = svc.LVCreate(r.Context(), d.serverID, diskadmin.LVCreateInput{
				VG: in.VG, Name: in.Name, SizeGiB: in.SizeGiB, UseAllFree: in.UseAllFree,
				FSType: in.FSType, Mountpoint: in.Mountpoint,
			})
		case "extend":
			err = svc.LVGrow(r.Context(), d.serverID, diskadmin.LVGrowInput{
				Path: in.Path, SizeGiB: in.SizeGiB, UseAllFree: in.UseAllFree,
			})
		case "reduce":
			err = svc.LVShrink(r.Context(), d.serverID, diskadmin.ShrinkInput{
				Target: in.Path, TargetGiB: in.TargetGiB, Confirm: in.Confirm,
			})
		case "remove":
			err = svc.LVRemove(r.Context(), d.serverID, diskadmin.RemoveTargetInput{Target: in.Path, Confirm: in.Confirm})
		default:
			writeError(w, http.StatusBadRequest, "invalid_disk_input", "action 须为 create|extend|reduce|remove")
			return
		}
		if err != nil {
			writeDiskAdminError(w, err)
			return
		}
		recordAudit(r.Context(), d.rec, audit.Entry{
			Actor: auditActor, Action: auditActionDiskLV, TargetType: auditTargetDiskServer,
			TargetID: d.serverID,
			Detail: map[string]any{
				"action": in.Action, "vg": in.VG, "name": in.Name, "path": in.Path,
				"sizeGiB": in.SizeGiB, "targetGiB": in.TargetGiB, "useAllFree": in.UseAllFree,
			},
			IP: clientIP(r),
		})
		writeDiskOpResult(w, r, svc, d.serverID)
	}
}
