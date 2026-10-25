// Package httpapi:已登记实例的备份恢复端点(功能长在 /databases 与 /redis 页)。
//
// 挂在现有实例资源树下(不建新顶层资源):
//
//	GET  /api/{db|redis}-instances/{id}/backup-config     计划 + 能力判定
//	PUT  /api/{db|redis}-instances/{id}/backup-config     写计划(间隔/保留/自动上云)
//	POST /api/{db|redis}-instances/{id}/backups           立即备份(异步,返回 running 记录)
//	GET  /api/{db|redis}-instances/{id}/backups           备份列表
//	POST /api/{db|redis}-instances/{id}/restore           恢复(backupId | 云端直拉 | 上传文件)
//	GET  /api/db-backups/{bid}/download                   下载备份
//	DELETE /api/db-backups/{bid}                          删除备份
//	POST /api/db-backups/{bid}/upload-objectstore         上传对象存储
//
// db-instances 组的 kind = 实例 engine;redis 组恒为 redis。实例删除时级联清理备份
// (挂在两个既有 DELETE handler 上,见 mount 装配参数)。
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
	"github.com/huangchengsir/pipewright/internal/dbadmin"
	"github.com/huangchengsir/pipewright/internal/dbbackup"
	"github.com/huangchengsir/pipewright/internal/objectstore"
	"github.com/huangchengsir/pipewright/internal/redisadmin"
)

// 审计动作。
const (
	auditActionDBBackupRun     = "dbbackup.run"
	auditActionDBBackupDelete  = "dbbackup.delete"
	auditActionDBBackupUpload  = "dbbackup.upload"
	auditActionDBBackupRestore = "dbbackup.restore"
	auditTargetDBBackup        = "db_backup"
)

// dbbConfigDTO 是计划+能力的对外形状。
type dbbConfigDTO struct {
	IntervalHours        int    `json:"intervalHours"`
	Retention            int    `json:"retention"`
	LastBackupAt         string `json:"lastBackupAt"`
	AutoUploadProviderID string `json:"autoUploadProviderId"`
	Mode                 string `json:"mode"`
	Backupable           bool   `json:"backupable"`
	Restoreable          bool   `json:"restoreable"`
	Note                 string `json:"note"`
}

// dbbRecordDTO 是备份记录对外形状。
type dbbRecordDTO struct {
	ID           string `json:"id"`
	InstanceKind string `json:"instanceKind"`
	InstanceID   string `json:"instanceId"`
	FileName     string `json:"fileName"`
	SizeBytes    int64  `json:"sizeBytes"`
	Sha256       string `json:"sha256"`
	Kind         string `json:"kind"`
	Status       string `json:"status"`
	Error        string `json:"error"`
	ObjectKey    string `json:"objectKey"`
	CreatedAt    string `json:"createdAt"`
	FinishedAt   string `json:"finishedAt"`
}

func dbbRecordToDTO(r dbbackup.Record) dbbRecordDTO {
	return dbbRecordDTO{
		ID: r.ID, InstanceKind: r.InstanceKind, InstanceID: r.InstanceID,
		FileName: r.FileName, SizeBytes: r.SizeBytes, Sha256: r.Sha256, Kind: r.Kind,
		Status: r.Status, Error: r.Error, ObjectKey: r.ObjectKey,
		CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt,
	}
}

func writeDBBackupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, dbbackup.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "dbbackup_invalid_input", err.Error())
	case errors.Is(err, dbbackup.ErrUnsupported):
		writeError(w, http.StatusBadRequest, "dbbackup_unsupported", err.Error())
	case errors.Is(err, dbbackup.ErrNotFound):
		writeError(w, http.StatusNotFound, "dbbackup_not_found", "实例或备份不存在")
	case errors.Is(err, dbbackup.ErrBusy):
		writeError(w, http.StatusConflict, "dbbackup_busy", "该实例已有备份/恢复进行中")
	case errors.Is(err, dbbackup.ErrReadonly):
		writeError(w, http.StatusForbidden, "dbbackup_readonly", "实例处于只读模式,请先解锁再恢复")
	default:
		writeError(w, http.StatusInternalServerError, "dbbackup_internal", err.Error())
	}
}

// ── 核心逻辑(两组路由共享;kind 由外层解析) ──────────────────────────────────

func dbbGetConfig(w http.ResponseWriter, r *http.Request, svc *dbbackup.Service, kind, id string) {
	cfg, err := svc.GetConfig(r.Context(), kind, id)
	if err != nil {
		writeDBBackupError(w, err)
		return
	}
	capability, err := svc.Capability(r.Context(), kind, id)
	if err != nil {
		writeDBBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dbbConfigDTO{
		IntervalHours: cfg.IntervalHours, Retention: cfg.Retention,
		LastBackupAt: cfg.LastBackupAt, AutoUploadProviderID: cfg.AutoUploadProviderID,
		Mode: capability.Mode, Backupable: capability.Backupable,
		Restoreable: capability.Restoreable, Note: capability.Note,
	})
}

func dbbSetConfig(w http.ResponseWriter, r *http.Request, svc *dbbackup.Service, aud audit.Recorder, kind, id string) {
	var in dbbConfigDTO
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}
	if err := svc.SetConfig(r.Context(), kind, id, dbbackup.Config{
		IntervalHours: in.IntervalHours, Retention: in.Retention,
		AutoUploadProviderID: in.AutoUploadProviderID,
	}); err != nil {
		writeDBBackupError(w, err)
		return
	}
	recordAudit(r.Context(), aud, audit.Entry{
		Actor: auditActor, Action: auditActionDBBackupRun, TargetType: auditTargetDBBackup,
		TargetID: id, IP: clientIP(r),
		Detail: map[string]any{"op": "config", "kind": kind, "hours": in.IntervalHours, "retention": in.Retention},
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func dbbRunBackup(w http.ResponseWriter, r *http.Request, svc *dbbackup.Service, aud audit.Recorder, kind, id string) {
	rec, err := svc.RunBackup(r.Context(), kind, id, "manual")
	if err != nil {
		writeDBBackupError(w, err)
		return
	}
	recordAudit(r.Context(), aud, audit.Entry{
		Actor: auditActor, Action: auditActionDBBackupRun, TargetType: auditTargetDBBackup,
		TargetID: id, IP: clientIP(r),
		Detail: map[string]any{"backupId": rec.ID, "kind": kind, "runKind": "manual"},
	})
	writeJSON(w, http.StatusOK, dbbRecordToDTO(*rec))
}

func dbbListBackups(w http.ResponseWriter, r *http.Request, svc *dbbackup.Service, kind, id string) {
	list, err := svc.List(r.Context(), kind, id)
	if err != nil {
		writeDBBackupError(w, err)
		return
	}
	out := make([]dbbRecordDTO, 0, len(list))
	for _, e := range list {
		out = append(out, dbbRecordToDTO(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// dbbRestore 恢复:JSON {backupId} / {providerId,objectKey}(云端直拉)或 multipart
// (file)。暂存文件用后即删;恢复前服务端强制安全备份;只读拒绝在领域层把关。
func dbbRestore(w http.ResponseWriter, r *http.Request, svc *dbbackup.Service, stores objectstore.Service, aud audit.Recorder, kind, id string) {
	in := dbbackup.RestoreInput{}
	cloud := map[string]any{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<30)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "dbbackup_restore_upload", "上传解析失败:"+err.Error())
			return
		}
		file, _, ferr := r.FormFile("file")
		if ferr != nil {
			writeError(w, http.StatusBadRequest, "dbbackup_restore_upload", "缺少 file 字段")
			return
		}
		staged, serr := dbbStageLocal(kind, file)
		_ = file.Close()
		if serr != nil {
			writeError(w, http.StatusBadRequest, "dbbackup_restore_upload", serr.Error())
			return
		}
		defer func() { _ = os.Remove(staged) }()
		in.LocalPath = staged
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
		switch {
		case body.BackupID != "":
			in.BackupID = body.BackupID
		case body.ProviderID != "" && strings.TrimSpace(body.ObjectKey) != "":
			if stores == nil {
				writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "对象存储服务未装配")
				return
			}
			key := strings.TrimSpace(strings.Trim(body.ObjectKey, "/"))
			if !dbbValidDumpSuffix(kind, key) {
				writeError(w, http.StatusBadRequest, "dbbackup_restore_source", "objectKey 后缀与实例引擎不匹配")
				return
			}
			staged, derr := dbbDownloadCloud(r, stores, body.ProviderID, key, kind)
			if derr != nil {
				if errors.Is(derr, objectstore.ErrObjectNotFound) {
					writeError(w, http.StatusNotFound, "dbbackup_restore_object", "对象不存在:"+key)
				} else {
					writeError(w, http.StatusBadGateway, "dbbackup_restore_download", "从对象存储拉取失败:"+derr.Error())
				}
				return
			}
			defer func() { _ = os.Remove(staged) }()
			in.LocalPath = staged
			cloud = map[string]any{"providerId": body.ProviderID, "objectKey": key}
		default:
			writeError(w, http.StatusBadRequest, "dbbackup_restore_source", "缺少恢复来源(backupId / providerId+objectKey / file)")
			return
		}
	}

	safety, rerr := svc.Restore(r.Context(), kind, id, in)
	resp := map[string]any{"ok": true}
	if safety != nil {
		resp["safetyBackup"] = dbbRecordToDTO(*safety)
	}
	if rerr != nil {
		resp["ok"] = false
		resp["error"] = rerr.Error()
	}
	entry := audit.Entry{
		Actor: auditActor, Action: auditActionDBBackupRestore, TargetType: auditTargetDBBackup,
		TargetID: id, IP: clientIP(r),
		Detail: map[string]any{
			"ok": resp["ok"] == true, "fromBackupId": in.BackupID,
			"uploaded": in.LocalPath != "" && len(cloud) == 0,
		},
	}
	for k, v := range cloud {
		entry.Detail[k] = v
	}
	recordAudit(r.Context(), aud, entry)
	if rerr != nil {
		writeJSON(w, http.StatusInternalServerError, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// dbbStageLocal 把上传流落到控制机临时文件(后缀按引擎)。
func dbbStageLocal(kind string, src io.Reader) (string, error) {
	staged := filepath.Join(os.TempDir(), "pw-dbrestore-"+uuid.NewString()+dbbDumpExt(kind))
	out, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		_ = os.Remove(staged)
		return "", err
	}
	_ = out.Close()
	return staged, nil
}

// dbbDownloadCloud 云端直拉到暂存文件。
func dbbDownloadCloud(r *http.Request, stores objectstore.Service, providerID, key, kind string) (string, error) {
	staged := filepath.Join(os.TempDir(), "pw-dbrestore-"+uuid.NewString()+dbbDumpExt(kind))
	out, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if derr := stores.Download(r.Context(), providerID, key, out); derr != nil {
		_ = out.Close()
		_ = os.Remove(staged)
		return "", derr
	}
	_ = out.Close()
	return staged, nil
}

func dbbDumpExt(kind string) string {
	if kind == dbbackup.KindRedis {
		return ".rdb"
	}
	return ".sql"
}

// dbbValidDumpSuffix 校验对象键后缀与引擎匹配。
func dbbValidDumpSuffix(kind, key string) bool {
	return strings.HasSuffix(key, dbbDumpExt(kind))
}

// ── 薄壳:db-instances 组(kind = 实例 engine) ────────────────────────────────

// dbbRequireDBInstance 取实例并返回其 engine(kind)。
func dbbRequireDBInstance(w http.ResponseWriter, r *http.Request, dba dbadmin.Service) (string, string, bool) {
	if dba == nil {
		writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "数据库服务未装配")
		return "", "", false
	}
	id := chi.URLParam(r, "id")
	inst, err := dba.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "dbbackup_not_found", "实例不存在")
		return "", "", false
	}
	return inst.Engine, id, true
}

// dbbRequireRedisInstance 校验 redis 实例存在,返回 (kind, id)。
func dbbRequireRedisInstance(w http.ResponseWriter, r *http.Request, redisa redisadmin.Service) (string, string, bool) {
	if redisa == nil {
		writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "Redis 服务未装配")
		return "", "", false
	}
	id := chi.URLParam(r, "id")
	if _, err := redisa.Get(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "dbbackup_not_found", "实例不存在")
		return "", "", false
	}
	return dbbackup.KindRedis, id, true
}

func makeDBBackupConfigGetHandler(svc *dbbackup.Service, dba dbadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		if kind, id, ok := dbbRequireDBInstance(w, r, dba); ok {
			dbbGetConfig(w, r, svc, kind, id)
		}
	}
}

func makeDBBackupConfigSetHandler(svc *dbbackup.Service, dba dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		if kind, id, ok := dbbRequireDBInstance(w, r, dba); ok {
			dbbSetConfig(w, r, svc, aud, kind, id)
		}
	}
}

func makeDBBackupRunHandler(svc *dbbackup.Service, dba dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		if kind, id, ok := dbbRequireDBInstance(w, r, dba); ok {
			dbbRunBackup(w, r, svc, aud, kind, id)
		}
	}
}

func makeDBBackupListHandler(svc *dbbackup.Service, dba dbadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		if kind, id, ok := dbbRequireDBInstance(w, r, dba); ok {
			dbbListBackups(w, r, svc, kind, id)
		}
	}
}

func makeDBBackupRestoreHandler(svc *dbbackup.Service, dba dbadmin.Service, stores objectstore.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		if kind, id, ok := dbbRequireDBInstance(w, r, dba); ok {
			dbbRestore(w, r, svc, stores, aud, kind, id)
		}
	}
}

// ── 薄壳:redis-instances 组(kind 恒为 redis) ────────────────────────────────

func makeRedisBackupConfigGetHandler(svc *dbbackup.Service, redisa redisadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		if kind, id, ok := dbbRequireRedisInstance(w, r, redisa); ok {
			dbbGetConfig(w, r, svc, kind, id)
		}
	}
}

func makeRedisBackupConfigSetHandler(svc *dbbackup.Service, redisa redisadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		if kind, id, ok := dbbRequireRedisInstance(w, r, redisa); ok {
			dbbSetConfig(w, r, svc, aud, kind, id)
		}
	}
}

func makeRedisBackupRunHandler(svc *dbbackup.Service, redisa redisadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		if kind, id, ok := dbbRequireRedisInstance(w, r, redisa); ok {
			dbbRunBackup(w, r, svc, aud, kind, id)
		}
	}
}

func makeRedisBackupListHandler(svc *dbbackup.Service, redisa redisadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		if kind, id, ok := dbbRequireRedisInstance(w, r, redisa); ok {
			dbbListBackups(w, r, svc, kind, id)
		}
	}
}

func makeRedisBackupRestoreHandler(svc *dbbackup.Service, redisa redisadmin.Service, stores objectstore.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		if kind, id, ok := dbbRequireRedisInstance(w, r, redisa); ok {
			dbbRestore(w, r, svc, stores, aud, kind, id)
		}
	}
}

// ── 按备份记录寻址的共用端点 ─────────────────────────────────────────────────

func makeDBBackupDownloadHandler(svc *dbbackup.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		rec, err := svc.Get(r.Context(), chi.URLParam(r, "bid"))
		if err != nil {
			writeDBBackupError(w, err)
			return
		}
		f, err := os.Open(svc.BackupPath(rec))
		if err != nil {
			writeError(w, http.StatusNotFound, "dbbackup_file_missing", "备份文件已不在磁盘")
			return
		}
		defer func() { _ = f.Close() }()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, rec.FileName))
		_, _ = io.Copy(w, f)
	}
}

func makeDBBackupDeleteHandler(svc *dbbackup.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
			return
		}
		bid := chi.URLParam(r, "bid")
		if err := svc.Delete(r.Context(), bid); err != nil {
			writeDBBackupError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBBackupDelete, TargetType: auditTargetDBBackup,
			TargetID: bid, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeDBBackupUploadObjectStoreHandler(svc *dbbackup.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "dbbackup_unavailable", "备份服务未装配")
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
			writeError(w, http.StatusBadRequest, "dbbackup_upload_invalid", "providerId 必填")
			return
		}
		bid := chi.URLParam(r, "bid")
		if err := svc.UploadToObjectStore(r.Context(), bid, in.ProviderID); err != nil {
			writeDBBackupError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBBackupUpload, TargetType: auditTargetDBBackup,
			TargetID: bid, IP: clientIP(r), Detail: map[string]any{"providerId": in.ProviderID},
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// mountDBBackup 挂全部备份路由(db-instances 与 redis-instances 两组 + 共用记录组)。
func mountDBBackup(ar chi.Router, svc *dbbackup.Service, dba dbadmin.Service, redisa redisadmin.Service, stores objectstore.Service, aud audit.Recorder) {
	ar.Get("/db-instances/{id}/backup-config", makeDBBackupConfigGetHandler(svc, dba))
	ar.Put("/db-instances/{id}/backup-config", makeDBBackupConfigSetHandler(svc, dba, aud))
	ar.Post("/db-instances/{id}/backups", makeDBBackupRunHandler(svc, dba, aud))
	ar.Get("/db-instances/{id}/backups", makeDBBackupListHandler(svc, dba))
	ar.Post("/db-instances/{id}/restore", makeDBBackupRestoreHandler(svc, dba, stores, aud))

	ar.Get("/redis-instances/{id}/backup-config", makeRedisBackupConfigGetHandler(svc, redisa))
	ar.Put("/redis-instances/{id}/backup-config", makeRedisBackupConfigSetHandler(svc, redisa, aud))
	ar.Post("/redis-instances/{id}/backups", makeRedisBackupRunHandler(svc, redisa, aud))
	ar.Get("/redis-instances/{id}/backups", makeRedisBackupListHandler(svc, redisa))
	ar.Post("/redis-instances/{id}/restore", makeRedisBackupRestoreHandler(svc, redisa, stores, aud))

	ar.Get("/db-backups/{bid}/download", makeDBBackupDownloadHandler(svc))
	ar.Delete("/db-backups/{bid}", makeDBBackupDeleteHandler(svc, aud))
	ar.Post("/db-backups/{bid}/upload-objectstore", makeDBBackupUploadObjectStoreHandler(svc, aud))
}
