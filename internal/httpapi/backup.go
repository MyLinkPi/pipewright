// Package httpapi:平台备份恢复端点(管理员)。
//
//	GET    /api/backup/config                   读取策略 + 待恢复标记状态
//	PUT    /api/backup/config                   写策略
//	POST   /api/backup/run                      立即备份(异步执行,返回运行记录)
//	GET    /api/backup/list                     备份记录列表
//	GET    /api/backup/{id}/download            下载归档
//	DELETE /api/backup/{id}                     删除记录与归档
//	POST   /api/backup/{id}/upload-objectstore  上传归档到对象存储
//	POST   /api/backup/restore                  上传归档并安排在线恢复(空闲后自重启)
//	GET    /api/backup/restore/pending          待恢复状态(前端横幅)
//	DELETE /api/backup/restore/pending          取消待恢复
//
// 恢复口令绝不入审计/日志;归档下载走附件流。
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/backup"
	"github.com/huangchengsir/pipewright/internal/config"
	"github.com/huangchengsir/pipewright/internal/objectstore"
	"github.com/huangchengsir/pipewright/internal/version"
)

// 审计动作(平台级,TargetType 用 platform_backup)。
const (
	auditActionBackupRun     = "backup.run"
	auditActionBackupDelete  = "backup.delete"
	auditActionBackupUpload  = "backup.upload"
	auditActionBackupRestore = "backup.restore"
	auditTargetBackup        = "platform_backup"
)

// backupConfigDTO 是策略的对外形状(camelCase)。
type backupConfigDTO struct {
	Enabled               bool   `json:"enabled"`
	IntervalHours         int    `json:"intervalHours"`
	RetentionCount        int    `json:"retentionCount"`
	IncludeArtifacts      bool   `json:"includeArtifacts"`
	IncludeMasterKey      bool   `json:"includeMasterKey"`
	ObjectstoreProviderID string `json:"objectstoreProviderId"`
	LastRunAt             string `json:"lastRunAt"`
	// MasterKeyConfigured 表示平台当前是否配置了 master key(前端决定能否带口令备份)。
	MasterKeyConfigured bool `json:"masterKeyConfigured"`
}

func makeGetBackupConfigHandler(svc *backup.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "备份服务未装配")
			return
		}
		cfg, err := svc.GetConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "backup_config_read", err.Error())
			return
		}
		masterConfigured := false
		if _, kerr := config.LoadMasterKey(); kerr == nil {
			masterConfigured = true
		}
		pending, _ := backup.ReadPendingRestore(svc.DataDir())
		writeJSON(w, http.StatusOK, map[string]any{
			"config": backupConfigDTO{
				Enabled: cfg.Enabled, IntervalHours: cfg.IntervalHours, RetentionCount: cfg.RetentionCount,
				IncludeArtifacts: cfg.IncludeArtifacts, IncludeMasterKey: cfg.IncludeMasterKey,
				ObjectstoreProviderID: cfg.ObjectstoreProviderID, LastRunAt: cfg.LastRunAt,
				MasterKeyConfigured: masterConfigured,
			},
			"restorePending": pending != nil,
		})
	}
}

func makeSetBackupConfigHandler(svc *backup.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "备份服务未装配")
			return
		}
		var in backupConfigDTO
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		if in.IntervalHours < 0 || in.IntervalHours > 24*30 {
			writeError(w, http.StatusBadRequest, "backup_config_invalid", "intervalHours 须在 0..720 小时")
			return
		}
		if in.RetentionCount < 0 || in.RetentionCount > 365 {
			writeError(w, http.StatusBadRequest, "backup_config_invalid", "retentionCount 须在 0..365")
			return
		}
		if in.IncludeMasterKey && !in.MasterKeyConfigured {
			writeError(w, http.StatusBadRequest, "backup_config_invalid", "携带 master key 需要平台先配置 PIPEWRIGHT_MASTER_KEY")
			return
		}
		if err := svc.SetConfig(r.Context(), backup.Config{
			Enabled: in.Enabled, IntervalHours: in.IntervalHours, RetentionCount: in.RetentionCount,
			IncludeArtifacts: in.IncludeArtifacts, IncludeMasterKey: in.IncludeMasterKey,
			ObjectstoreProviderID: in.ObjectstoreProviderID,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "backup_config_write", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// backupRecordDTO 是备份运行记录的对外形状。
type backupRecordDTO struct {
	ID         string `json:"id"`
	FileName   string `json:"fileName"`
	SizeBytes  int64  `json:"sizeBytes"`
	Sha256     string `json:"sha256"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Error      string `json:"error"`
	ObjectKey  string `json:"objectKey"`
	DBDriver   string `json:"dbDriver"`
	CreatedAt  string `json:"createdAt"`
	FinishedAt string `json:"finishedAt"`
}

func backupRecordToDTO(r backup.Record) backupRecordDTO {
	return backupRecordDTO{
		ID: r.ID, FileName: r.FileName, SizeBytes: r.SizeBytes, Sha256: r.Sha256,
		Kind: r.Kind, Status: r.Status, Error: r.Error, ObjectKey: r.ObjectKey,
		DBDriver: r.DBDriver, CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt,
	}
}

// makeRunBackupHandler 立即备份。passphrase 仅当开启 include_master_key 时有意义,
// 绝不写审计/日志。
func makeRunBackupHandler(svc *backup.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "备份服务未装配")
			return
		}
		var in struct {
			IncludeArtifacts *bool  `json:"includeArtifacts"`
			Passphrase       string `json:"passphrase"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		rec, err := svc.CreateBackup(r.Context(), backup.CreateInput{
			Kind: "manual", IncludeArtifacts: in.IncludeArtifacts, Passphrase: in.Passphrase,
		})
		if err != nil {
			if errors.Is(err, backup.ErrBackupRunning) {
				writeError(w, http.StatusConflict, "backup_busy", "已有备份在进行,请稍候")
				return
			}
			writeError(w, http.StatusInternalServerError, "backup_run_failed", err.Error())
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: auditActionBackupRun, TargetType: auditTargetBackup, TargetID: rec.ID,
			IP: clientIP(r), Detail: map[string]any{"fileName": rec.FileName, "kind": rec.Kind},
		})
		writeJSON(w, http.StatusOK, backupRecordToDTO(*rec))
	}
}

func makeListBackupsHandler(svc *backup.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "备份服务未装配")
			return
		}
		list, err := svc.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "backup_list_failed", err.Error())
			return
		}
		out := make([]backupRecordDTO, 0, len(list))
		for _, e := range list {
			out = append(out, backupRecordToDTO(e))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

func makeBackupDownloadHandler(svc *backup.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "备份服务未装配")
			return
		}
		rec, err := svc.Get(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeError(w, http.StatusNotFound, "backup_not_found", "备份记录不存在")
			return
		}
		f, err := os.Open(svc.ArchivePath(rec))
		if err != nil {
			writeError(w, http.StatusNotFound, "backup_file_missing", "归档文件已不在磁盘")
			return
		}
		defer func() { _ = f.Close() }()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, rec.FileName))
		_, _ = io.Copy(w, f)
	}
}

func makeDeleteBackupHandler(svc *backup.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "备份服务未装配")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.Delete(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "backup_delete_failed", err.Error())
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: auditActionBackupDelete, TargetType: auditTargetBackup,
			TargetID: id, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makeBackupUploadObjectStoreHandler 把归档上传到指定对象存储配置。
func makeBackupUploadObjectStoreHandler(svc *backup.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "备份服务未装配")
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
			writeError(w, http.StatusBadRequest, "backup_upload_invalid", "providerId 必填")
			return
		}
		if err := svc.UploadToObjectStore(r.Context(), chi.URLParam(r, "id"), in.ProviderID); err != nil {
			writeError(w, http.StatusInternalServerError, "backup_upload_failed", err.Error())
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: auditActionBackupUpload, TargetType: auditTargetBackup,
			TargetID: chi.URLParam(r, "id"), IP: clientIP(r),
			Detail: map[string]any{"providerId": in.ProviderID},
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// restoreMaxUploadBytes 在线恢复上传的大小上限(含 artifacts 的全量包可能很大;
// SQLite 快照 + acme 通常在几百 MB 内)。
const restoreMaxUploadBytes = 8 << 30

// makeBackupRestoreHandler 安排一次在线恢复。两种来源:
//   - multipart(file 字段):上传本地归档(原有行为);
//   - JSON {providerId, objectKey}:从对象存储配置直接拉取归档(objectstore.Download,
//     对象须为 .pwb;平台备份自动上传的对象键前缀为 platform-backups/)。
//
// 两路共用同一尾部:校验 → 暂存 + pending 标记 → 空闲后自重启。
// passphrase/force 字段两路语义相同;口令绝不写审计/日志。
func makeBackupRestoreHandler(svc *backup.Service, stores objectstore.Service, guard ReexecGuard, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "备份服务未装配")
			return
		}
		if pending, _ := backup.ReadPendingRestore(svc.DataDir()); pending != nil {
			writeError(w, http.StatusConflict, "backup_restore_pending", "已有待应用的恢复任务")
			return
		}

		staging := backup.StagingDir(svc.DataDir())
		if err := os.MkdirAll(staging, 0o700); err != nil {
			writeError(w, http.StatusInternalServerError, "backup_restore_stage", err.Error())
			return
		}
		staged := filepath.Join(staging, "restore-"+uuid.NewString()+".pwb")
		var passphrase, auditSource string
		var force bool
		var auditDetail map[string]any

		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			// 来源 1:本地上传。
			r.Body = http.MaxBytesReader(w, r.Body, restoreMaxUploadBytes)
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				writeError(w, http.StatusBadRequest, "backup_restore_upload", "上传解析失败:"+err.Error())
				return
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				writeError(w, http.StatusBadRequest, "backup_restore_upload", "缺少 file 字段")
				return
			}
			passphrase = strings.TrimSpace(r.FormValue("passphrase"))
			force = r.FormValue("force") == "1"
			out, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				_ = file.Close()
				writeError(w, http.StatusInternalServerError, "backup_restore_stage", err.Error())
				return
			}
			_, werr := io.Copy(out, file)
			_ = out.Close()
			_ = file.Close()
			if werr != nil {
				_ = os.Remove(staged)
				writeError(w, http.StatusBadRequest, "backup_restore_upload", "上传写入失败:"+werr.Error())
				return
			}
			auditSource = "upload"
		} else {
			// 来源 2:对象存储拉取。
			if stores == nil {
				writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "对象存储服务未装配")
				return
			}
			var in struct {
				ProviderID string `json:"providerId"`
				ObjectKey  string `json:"objectKey"`
				Passphrase string `json:"passphrase"`
				Force      bool   `json:"force"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
				return
			}
			in.ObjectKey = strings.TrimSpace(strings.Trim(in.ObjectKey, "/"))
			if in.ProviderID == "" || in.ObjectKey == "" {
				writeError(w, http.StatusBadRequest, "backup_restore_source", "providerId 与 objectKey 必填")
				return
			}
			if !strings.HasSuffix(in.ObjectKey, ".pwb") {
				writeError(w, http.StatusBadRequest, "backup_restore_source", "objectKey 须指向 .pwb 备份归档")
				return
			}
			passphrase, force = strings.TrimSpace(in.Passphrase), in.Force
			out, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "backup_restore_stage", err.Error())
				return
			}
			derr := stores.Download(r.Context(), in.ProviderID, in.ObjectKey, out)
			_ = out.Close()
			if derr != nil {
				_ = os.Remove(staged)
				if errors.Is(derr, objectstore.ErrObjectNotFound) {
					writeError(w, http.StatusNotFound, "backup_restore_object", "对象不存在:"+in.ObjectKey)
					return
				}
				writeError(w, http.StatusBadGateway, "backup_restore_download", "从对象存储拉取失败:"+derr.Error())
				return
			}
			auditSource = "objectstore"
			auditDetail = map[string]any{"providerId": in.ProviderID, "objectKey": in.ObjectKey}
		}

		m, verr := backup.ValidateArchive(staged)
		if verr != nil {
			_ = os.Remove(staged)
			writeError(w, http.StatusBadRequest, "backup_restore_invalid", verr.Error())
			return
		}
		if m.Contains.MasterKeySealed && passphrase == "" {
			_ = os.Remove(staged)
			writeError(w, http.StatusBadRequest, "backup_restore_passphrase", "归档携带口令密封的 master key,需要提供备份时的口令")
			return
		}
		rel, _ := filepath.Rel(svc.DataDir(), staged)
		if err := backup.WritePendingRestore(svc.DataDir(), rel, passphrase); err != nil {
			_ = os.Remove(staged)
			writeError(w, http.StatusBadRequest, "backup_restore_passphrase", err.Error())
			return
		}
		entry := audit.Entry{
			Actor: "admin", Action: auditActionBackupRestore, TargetType: auditTargetBackup,
			IP: clientIP(r),
			Detail: map[string]any{
				"source":            auditSource,
				"containsDb":        m.Contains.DB,
				"containsAcme":      m.Contains.Acme,
				"containsArtifacts": m.Contains.Artifacts,
				"masterKeySealed":   m.Contains.MasterKeySealed,
				"backupCreatedAt":   m.CreatedAt,
				"dbDriver":          m.DBDriver,
			},
		}
		for k, v := range auditDetail {
			entry.Detail[k] = v
		}
		recordAudit(r.Context(), aud, entry)

		// 空闲门槛:有活跃任务且未强制 → 等空闲自重启(语义与自更新一致);
		// 否则立即安排重启。
		if guard != nil {
			if runs, shared := guard.ActiveTasks(); runs+shared > 0 && !force {
				writeJSON(w, http.StatusOK, map[string]any{
					"status": "deferred",
					"message": fmt.Sprintf("恢复已就绪:当前有 %d 个运行、%d 个共享构建任务进行中,全部结束后自动重启应用", runs, shared),
				})
				scheduleDeferredReexec(guard)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "restarting",
			"message": "恢复已就绪,平台即将重启应用(重启完成后登录验证数据)",
		})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		cancelDeferredReexec()
		go func() {
			if guard != nil {
				guard.GracefulShutdown()
			}
			if err := version.Reexec(); err != nil {
				// 自重启失败:保留 pending 标记,提示手动重启。
				log.Printf("[backup] 自重启失败(恢复标记保留,请手动重启):%v", err)
			}
		}()
	}
}

// makeBackupRestorePendingHandler 返回待恢复状态(前端横幅)。
func makeBackupRestorePendingHandler(svc *backup.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "备份服务未装配")
			return
		}
		pending, err := backup.ReadPendingRestore(svc.DataDir())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "backup_restore_pending", err.Error())
			return
		}
		if pending == nil {
			writeJSON(w, http.StatusOK, map[string]any{"pending": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"pending": true, "createdAt": pending.CreatedAt})
	}
}

// makeBackupRestoreCancelHandler 取消待恢复(清标记与暂存;已在等待的重启一并取消)。
func makeBackupRestoreCancelHandler(svc *backup.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "备份服务未装配")
			return
		}
		if err := backup.ClearPendingRestore(svc.DataDir()); err != nil {
			writeError(w, http.StatusInternalServerError, "backup_restore_cancel", err.Error())
			return
		}
		cancelDeferredReexec()
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: auditActionBackupRestore, TargetType: auditTargetBackup,
			IP: clientIP(r), Detail: map[string]any{"canceled": true},
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
