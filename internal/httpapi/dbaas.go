// Package httpapi:数据库即服务端点(管理员)。
//
//	POST   /api/dbaas/instances                       创建并部署实例
//	GET    /api/dbaas/instances?refresh=1             列表(可选容器实时状态)
//	GET    /api/dbaas/instances/{id}                  详情
//	POST   /api/dbaas/instances/{id}/start|stop|restart
//	DELETE /api/dbaas/instances/{id}?keepVolumes=1    拆除
//	POST   /api/dbaas/instances/{id}/credentials      揭示口令(审计)
//	PUT    /api/dbaas/instances/{id}/schedule         备份计划
//	POST   /api/dbaas/instances/{id}/backups          立即备份(同步执行)
//	GET    /api/dbaas/instances/{id}/backups          备份列表
//	POST   /api/dbaas/instances/{id}/restore          恢复(backupId 或 multipart file)
//	GET    /api/dbaas/backups/{bid}/download          下载备份
//	DELETE /api/dbaas/backups/{bid}                   删除备份
//	POST   /api/dbaas/backups/{bid}/upload-objectstore 上传备份到对象存储
//
// 口令仅在创建响应与显式揭示响应中一次性出现,绝不入审计/日志。
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/dbaas"
	"github.com/huangchengsir/pipewright/internal/objectstore"
)

// 审计动作。
const (
	auditActionDBaaSCreate    = "dbaas.instance.create"
	auditActionDBaaSDelete    = "dbaas.instance.delete"
	auditActionDBaaSAction    = "dbaas.instance.action"
	auditActionDBaaSReveal    = "dbaas.instance.reveal"
	auditActionDBaaSBackup    = "dbaas.instance.backup"
	auditActionDBaaSRestore   = "dbaas.instance.restore"
	auditActionDBaaSBackupDel = "dbaas.backup.delete"
	auditTargetDBaaS          = "dbaas_instance"
)

// dbaasInstanceDTO 是实例对外形状(绝无口令;createPassword 仅创建响应携带一次)。
type dbaasInstanceDTO struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Engine              string `json:"engine"`
	Version             string `json:"version"`
	ServerID            string `json:"serverId"`
	ServerName          string `json:"serverName"`
	Host                string `json:"host"`
	HostPort            int    `json:"hostPort"`
	StackName           string `json:"stackName"`
	CPUs                string `json:"cpus"`
	Memory              string `json:"memory"`
	Status              string `json:"status"`
	StatusDetail        string `json:"statusDetail"`
	BackupIntervalHours int    `json:"backupIntervalHours"`
	BackupRetention     int    `json:"backupRetention"`
	LastBackupAt        string `json:"lastBackupAt"`
	LiveStatus          string `json:"liveStatus"`
	CreatedAt           string `json:"createdAt"`
	UpdatedAt           string `json:"updatedAt"`
	CreatePassword      string `json:"createPassword,omitempty"`
}

func dbaasToDTO(i dbaas.Instance) dbaasInstanceDTO {
	return dbaasInstanceDTO{
		ID: i.ID, Name: i.Name, Engine: i.Engine, Version: i.Version,
		ServerID: i.ServerID, ServerName: i.ServerName, Host: i.Host, HostPort: i.HostPort,
		StackName: i.StackName, CPUs: i.CPUs, Memory: i.Memory, Status: i.Status,
		StatusDetail: i.StatusDetail, BackupIntervalHours: i.BackupIntervalHours,
		BackupRetention: i.BackupRetention, LastBackupAt: i.LastBackupAt,
		LiveStatus: i.LiveStatus, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt,
		CreatePassword: i.GeneratedPassword,
	}
}

// writeDBaaSError 把领域错误映射为契约错误码。
func writeDBaaSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, dbaas.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "dbaas_invalid_input", err.Error())
	case errors.Is(err, dbaas.ErrUnsupported):
		writeError(w, http.StatusBadRequest, "dbaas_unsupported", err.Error())
	case errors.Is(err, dbaas.ErrNameTaken):
		writeError(w, http.StatusConflict, "dbaas_name_taken", "实例名已被占用")
	case errors.Is(err, dbaas.ErrNotFound):
		writeError(w, http.StatusNotFound, "dbaas_not_found", "实例不存在")
	case errors.Is(err, dbaas.ErrPortAlloc):
		writeError(w, http.StatusConflict, "dbaas_port_alloc", "端口段 33000-40000 已无空闲端口")
	case errors.Is(err, dbaas.ErrBusy):
		writeError(w, http.StatusConflict, "dbaas_busy", "该实例已有备份/恢复进行中")
	default:
		writeError(w, http.StatusInternalServerError, "dbaas_internal", err.Error())
	}
}

func makeDBaaSCreateHandler(svc *dbaas.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		var in dbaas.CreateInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		inst, err := svc.Create(r.Context(), in)
		if err != nil {
			writeDBaaSError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dbaasToDTO(*inst))
	}
}

func makeDBaaSListHandler(svc *dbaas.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		list, err := svc.List(r.Context(), r.URL.Query().Get("refresh") == "1")
		if err != nil {
			writeDBaaSError(w, err)
			return
		}
		out := make([]dbaasInstanceDTO, 0, len(list))
		for _, e := range list {
			out = append(out, dbaasToDTO(e))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

func makeDBaaSGetHandler(svc *dbaas.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		inst, err := svc.Get(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeDBaaSError(w, err)
			return
		}
		inst.LiveStatus, _ = svc.ContainerStatus(r.Context(), inst.ServerID, inst.StackName)
		writeJSON(w, http.StatusOK, dbaasToDTO(*inst))
	}
}

func makeDBaaSActionHandler(svc *dbaas.Service, aud audit.Recorder, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		id := chi.URLParam(r, "id")
		var inst *dbaas.Instance
		var err error
		switch action {
		case "start":
			inst, err = svc.Start(r.Context(), id)
		case "stop":
			inst, err = svc.Stop(r.Context(), id)
		case "restart":
			inst, err = svc.Restart(r.Context(), id)
		}
		if err != nil {
			writeDBaaSError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: auditActionDBaaSAction, TargetType: auditTargetDBaaS,
			TargetID: id, IP: clientIP(r),
			Detail: map[string]any{"action": action, "name": inst.Name, "engine": inst.Engine},
		})
		writeJSON(w, http.StatusOK, dbaasToDTO(*inst))
	}
}

func makeDBaaSDeleteHandler(svc *dbaas.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		id := chi.URLParam(r, "id")
		keep := r.URL.Query().Get("keepVolumes") == "1"
		inst, err := svc.Get(r.Context(), id)
		if err != nil {
			writeDBaaSError(w, err)
			return
		}
		if err := svc.Delete(r.Context(), id, keep); err != nil {
			writeDBaaSError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: auditActionDBaaSDelete, TargetType: auditTargetDBaaS,
			TargetID: id, IP: clientIP(r),
			Detail: map[string]any{"name": inst.Name, "engine": inst.Engine, "keepVolumes": keep},
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func makeDBaaSCredentialsHandler(svc *dbaas.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		id := chi.URLParam(r, "id")
		pass, err := svc.RevealPassword(r.Context(), id)
		if err != nil {
			writeDBaaSError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: auditActionDBaaSReveal, TargetType: auditTargetDBaaS,
			TargetID: id, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"password": pass})
	}
}

func makeDBaaSScheduleHandler(svc *dbaas.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		var in struct {
			BackupIntervalHours int `json:"backupIntervalHours"`
			BackupRetention     int `json:"backupRetention"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		if err := svc.SetSchedule(r.Context(), chi.URLParam(r, "id"), in.BackupIntervalHours, in.BackupRetention); err != nil {
			writeDBaaSError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: auditActionDBaaSAction, TargetType: auditTargetDBaaS,
			TargetID: chi.URLParam(r, "id"), IP: clientIP(r),
			Detail: map[string]any{"action": "schedule", "hours": in.BackupIntervalHours, "retention": in.BackupRetention},
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// dbaasBackupDTO 是备份记录对外形状。
type dbaasBackupDTO struct {
	ID         string `json:"id"`
	InstanceID string `json:"instanceId"`
	FileName   string `json:"fileName"`
	SizeBytes  int64  `json:"sizeBytes"`
	Sha256     string `json:"sha256"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Error      string `json:"error"`
	ObjectKey  string `json:"objectKey"`
	CreatedAt  string `json:"createdAt"`
	FinishedAt string `json:"finishedAt"`
}

func dbaasBackupToDTO(b dbaas.Backup) dbaasBackupDTO {
	return dbaasBackupDTO{
		ID: b.ID, InstanceID: b.InstanceID, FileName: b.FileName, SizeBytes: b.SizeBytes,
		Sha256: b.Sha256, Kind: b.Kind, Status: b.Status, Error: b.Error, ObjectKey: b.ObjectKey,
		CreatedAt: b.CreatedAt, FinishedAt: b.FinishedAt,
	}
}

// makeDBaaSBackupRunHandler 立即备份(同步执行:dump 时长取决于数据量)。
func makeDBaaSBackupRunHandler(svc *dbaas.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		id := chi.URLParam(r, "id")
		rec, err := svc.RunBackup(r.Context(), id, "manual")
		if err != nil {
			writeDBaaSError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: auditActionDBaaSBackup, TargetType: auditTargetDBaaS,
			TargetID: id, IP: clientIP(r),
			Detail: map[string]any{"backupId": rec.ID, "status": rec.Status, "bytes": rec.SizeBytes},
		})
		writeJSON(w, http.StatusOK, dbaasBackupToDTO(*rec))
	}
}

func makeDBaaSBackupListHandler(svc *dbaas.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		list, err := svc.ListBackups(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeDBaaSError(w, err)
			return
		}
		out := make([]dbaasBackupDTO, 0, len(list))
		for _, b := range list {
			out = append(out, dbaasBackupToDTO(b))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// dbaasRestoreMaxUploadBytes 恢复上传上限(SQL dump 可能很大)。
const dbaasRestoreMaxUploadBytes = 16 << 30

// makeDBaaSRestoreHandler 恢复,三种来源:
//   - multipart(file 字段):上传本地 .sql/.rdb;
//   - JSON {backupId}:恢复该实例已有的本地备份;
//   - JSON {providerId, objectKey}:直接从对象存储拉取(DBaaS 上传的对象键前缀为
//     dbaas/<实例名>/;须为 .sql/.rdb 后缀)。
//
// 恢复前服务端强制做 pre_restore 安全备份;暂存文件用后即删。
func makeDBaaSRestoreHandler(svc *dbaas.Service, stores objectstore.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		id := chi.URLParam(r, "id")
		in := dbaas.RestoreInput{}
		auditCloud := map[string]any{}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			r.Body = http.MaxBytesReader(w, r.Body, dbaasRestoreMaxUploadBytes)
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				writeError(w, http.StatusBadRequest, "dbaas_restore_upload", "上传解析失败:"+err.Error())
				return
			}
			file, _, ferr := r.FormFile("file")
			if ferr != nil {
				writeError(w, http.StatusBadRequest, "dbaas_restore_upload", "缺少 file 字段")
				return
			}
			defer func() { _ = file.Close() }()
			staged := filepath.Join(os.TempDir(), "pw-dbaas-restore-"+uuid.NewString())
			out, oerr := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if oerr != nil {
				writeError(w, http.StatusInternalServerError, "dbaas_restore_stage", oerr.Error())
				return
			}
			_, cerr := io.Copy(out, file)
			_ = out.Close()
			if cerr != nil {
				_ = os.Remove(staged)
				writeError(w, http.StatusBadRequest, "dbaas_restore_upload", "上传写入失败:"+cerr.Error())
				return
			}
			defer func() { _ = os.Remove(staged) }()
			in.UploadPath = staged
		} else {
			var body struct {
				BackupID   string `json:"backupId"`
				ProviderID string `json:"providerId"`
				ObjectKey  string `json:"objectKey"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
				return
			}
			if body.BackupID != "" {
				in.BackupID = body.BackupID
			} else if body.ProviderID != "" && strings.TrimSpace(body.ObjectKey) != "" {
				if stores == nil {
					writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "对象存储服务未装配")
					return
				}
				key := strings.TrimSpace(strings.Trim(body.ObjectKey, "/"))
				if !strings.HasSuffix(key, ".sql") && !strings.HasSuffix(key, ".rdb") {
					writeError(w, http.StatusBadRequest, "dbaas_restore_source", "objectKey 须指向 .sql 或 .rdb 备份文件")
					return
				}
				staged := filepath.Join(os.TempDir(), "pw-dbaas-restore-"+uuid.NewString())
				out, oerr := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if oerr != nil {
					writeError(w, http.StatusInternalServerError, "dbaas_restore_stage", oerr.Error())
					return
				}
				derr := stores.Download(r.Context(), body.ProviderID, key, out)
				_ = out.Close()
				if derr != nil {
					_ = os.Remove(staged)
					if errors.Is(derr, objectstore.ErrObjectNotFound) {
						writeError(w, http.StatusNotFound, "dbaas_restore_object", "对象不存在:"+key)
						return
					}
					writeError(w, http.StatusBadGateway, "dbaas_restore_download", "从对象存储拉取失败:"+derr.Error())
					return
				}
				defer func() { _ = os.Remove(staged) }()
				in.UploadPath = staged
				auditCloud = map[string]any{"providerId": body.ProviderID, "objectKey": key}
			}
		}
		safety, err := svc.Restore(r.Context(), id, in)
		resp := map[string]any{"ok": true}
		if safety != nil {
			resp["safetyBackup"] = dbaasBackupToDTO(*safety)
		}
		if err != nil {
			resp["ok"] = false
			resp["error"] = err.Error()
		}
		entry := audit.Entry{
			Actor: "admin", Action: auditActionDBaaSRestore, TargetType: auditTargetDBaaS,
			TargetID: id, IP: clientIP(r),
			Detail: map[string]any{
				"ok": resp["ok"] == true, "fromBackupId": in.BackupID, "uploaded": in.UploadPath != "",
			},
		}
		for k, v := range auditCloud {
			entry.Detail[k] = v
		}
		recordAudit(r.Context(), aud, entry)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, resp)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func makeDBaaSBackupDownloadHandler(svc *dbaas.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		b, err := svc.GetBackup(r.Context(), chi.URLParam(r, "bid"))
		if err != nil {
			writeDBaaSError(w, err)
			return
		}
		f, err := os.Open(svc.BackupPath(b))
		if err != nil {
			writeError(w, http.StatusNotFound, "dbaas_backup_missing", "备份文件已不在磁盘")
			return
		}
		defer func() { _ = f.Close() }()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, b.FileName))
		_, _ = io.Copy(w, f)
	}
}

func makeDBaaSBackupDeleteHandler(svc *dbaas.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		bid := chi.URLParam(r, "bid")
		if err := svc.DeleteBackup(r.Context(), bid); err != nil {
			writeDBaaSError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: auditActionDBaaSBackupDel, TargetType: auditTargetDBaaS,
			TargetID: bid, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func makeDBaaSBackupUploadHandler(svc *dbaas.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbaas_unavailable", "DBaaS 服务未装配")
			return
		}
		var in struct {
			ProviderID string `json:"providerId"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		if strings.TrimSpace(in.ProviderID) == "" {
			writeError(w, http.StatusBadRequest, "dbaas_upload_invalid", "providerId 必填")
			return
		}
		if err := svc.UploadBackupToObjectStore(r.Context(), chi.URLParam(r, "bid"), in.ProviderID); err != nil {
			writeError(w, http.StatusInternalServerError, "dbaas_upload_failed", err.Error())
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: auditActionDBaaSBackup, TargetType: auditTargetDBaaS,
			TargetID: chi.URLParam(r, "bid"), IP: clientIP(r),
			Detail: map[string]any{"upload": true, "providerId": in.ProviderID},
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
