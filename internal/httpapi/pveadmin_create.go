// pveadmin_create.go — PVE 建机 HTTP 面(admin 组):表单配套只读下拉、建机任务
// (受理/快照)、LXC 模板在线下载、.vma 备份流式上传。
//
// 纪律:建机/下载/上传是写操作,readonly 拒绝与受理均留审计;审计 detail 只含
// mode/node/name/count/vmid/filename 等非密字段,绝不含密码/密钥/Token;
// 上传 multipart 流式透传(不缓冲、不限 64KiB),其余请求体 ≤64KiB。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/pveadmin"
)

// 建机面审计 action。
const (
	auditActionPVEVMCreate     = "pveadmin.vm.create"
	auditActionPVEAplDownload  = "pveadmin.apl.download"
	auditActionPVEBackupUpload = "pveadmin.backup.upload"
	// 2 TiB 是单次备份上传的硬上限;限制整个 multipart 流而非仅 Content-Length。
	maxPVEBackupUploadBytes int64 = 2 << 40
)

// pveUploadBody 记录 MaxBytesError,即使下游客户端包装了读取错误也能映射为 413。
type pveUploadBody struct {
	io.ReadCloser
	tooLarge atomic.Bool
}

func (b *pveUploadBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		b.tooLarge.Store(true)
	}
	return n, err
}

// makePVETemplateListHandler 返回 GET /api/pve/clusters/{id}/templates → { items }。
func makePVETemplateListHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		items, err := svc.ListTemplates(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makePVEStorageListHandler 返回 GET /api/pve/clusters/{id}/storages?node= → { items }。
func makePVEStorageListHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		node := r.URL.Query().Get("node")
		if node == "" {
			writeError(w, http.StatusBadRequest, "invalid_pve_input", "缺少 node 查询参数")
			return
		}
		items, err := svc.ListStorages(r.Context(), chi.URLParam(r, "id"), node)
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makePVEBridgeListHandler 返回 GET /api/pve/clusters/{id}/bridges?node= → { items }。
func makePVEBridgeListHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		node := r.URL.Query().Get("node")
		if node == "" {
			writeError(w, http.StatusBadRequest, "invalid_pve_input", "缺少 node 查询参数")
			return
		}
		items, err := svc.ListBridges(r.Context(), chi.URLParam(r, "id"), node)
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makePVENextIDHandler 返回 GET /api/pve/clusters/{id}/nextid → { vmid }。
func makePVENextIDHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		vmid, err := svc.NextVMID(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"vmid": vmid})
	}
}

// makePVEStorageContentHandler 返回 GET .../storage-content?node=&storage=&content= → { items }。
func makePVEStorageContentHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		q := r.URL.Query()
		node, storage, content := q.Get("node"), q.Get("storage"), q.Get("content")
		if node == "" || storage == "" || content == "" {
			writeError(w, http.StatusBadRequest, "invalid_pve_input", "缺少 node/storage/content 查询参数")
			return
		}
		items, err := svc.ListStorageContent(r.Context(), chi.URLParam(r, "id"), node, storage, content)
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makePVEAplInfoHandler 返回 GET /api/pve/clusters/{id}/aplinfo?node= → { items }。
func makePVEAplInfoHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		node := r.URL.Query().Get("node")
		if node == "" {
			writeError(w, http.StatusBadRequest, "invalid_pve_input", "缺少 node 查询参数")
			return
		}
		items, err := svc.ListAplInfo(r.Context(), chi.URLParam(r, "id"), node)
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makePVEAplDownloadHandler 返回 POST .../aplinfo/download {node,storage,template}
// → 202 {node,upid}。readonly 拒绝与受理均留审计。
func makePVEAplDownloadHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Node     string `json:"node"`
			Storage  string `json:"storage"`
			Template string `json:"template"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		ref, err := svc.DownloadAplTemplate(r.Context(), id, req.Node, req.Storage, req.Template)
		if err != nil {
			if errors.Is(err, pveadmin.ErrReadonly) {
				recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEAplDownload,
					TargetType: auditTargetPVECluster, TargetID: id,
					Detail: map[string]any{"result": "拒绝(只读模式)", "template": req.Template}, IP: clientIP(r)})
			}
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEAplDownload,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "node": req.Node, "storage": req.Storage, "template": req.Template, "upid": ref.UPID}, IP: clientIP(r)})
		writeJSON(w, http.StatusAccepted, ref)
	}
}

// makePVECreateHandler 返回 POST /api/pve/clusters/{id}/create(CreateInput)→ 202 {ok:true}。
// 建机是异步任务(快照经 create-status 轮询);readonly 拒绝与受理均留审计,detail 绝无凭据。
func makePVECreateHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var in pveadmin.CreateInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		id := chi.URLParam(r, "id")
		err := svc.StartCreate(r.Context(), id, in)
		if err != nil {
			if errors.Is(err, pveadmin.ErrReadonly) {
				recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEVMCreate,
					TargetType: auditTargetPVECluster, TargetID: id,
					Detail: map[string]any{"result": "拒绝(只读模式)", "mode": in.Mode, "name": in.Name, "count": in.Count}, IP: clientIP(r)})
			}
			writePVEAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEVMCreate,
			TargetType: auditTargetPVECluster, TargetID: id,
			Detail: map[string]any{"result": "受理", "mode": in.Mode, "node": in.Node, "name": in.Name, "count": in.Count, "vmid": in.VMID}, IP: clientIP(r)})
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
	}
}

// makePVECreateStatusHandler 返回 GET .../create-status → 任务快照(前端轮询至 done)。
func makePVECreateStatusHandler(svc pveadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		st, err := svc.CreateStatus(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writePVEAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// makePVEUploadVMAHandler 返回 POST .../upload-vma?node=&storage=(multipart 文件字段
// filename)→ { volid }(恢复源:卷 ID 或绝对路径)。流式透传不缓冲;readonly 拒绝留审计。
func makePVEUploadVMAHandler(svc pveadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "PVE 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		q := r.URL.Query()
		node, storage := q.Get("node"), q.Get("storage")
		if node == "" || storage == "" {
			writeError(w, http.StatusBadRequest, "invalid_pve_input", "缺少 node/storage 查询参数")
			return
		}
		// 仅上传路由放宽全局 30s ReadTimeout,并保留有限期限和流式字节上限。
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(pveadmin.UploadBackupTimeout))
		limited := &pveUploadBody{ReadCloser: http.MaxBytesReader(w, r.Body, maxPVEBackupUploadBytes)}
		r.Body = limited
		mr, err := r.MultipartReader()
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求须为 multipart/form-data")
			return
		}
		for {
			part, perr := mr.NextPart()
			if perr == io.EOF {
				writeError(w, http.StatusBadRequest, "bad_request", "缺少文件字段 filename")
				return
			}
			if perr != nil {
				if limited.tooLarge.Load() {
					writeError(w, http.StatusRequestEntityTooLarge, "pve_upload_too_large", "备份上传超过 2 TiB 上限")
				} else {
					writeError(w, http.StatusBadRequest, "bad_request", "multipart 解析失败")
				}
				return
			}
			if part.FormName() != "filename" {
				_ = part.Close()
				continue
			}
			fileName := part.FileName()
			volid, uerr := svc.UploadBackup(r.Context(), id, node, storage, fileName, part)
			_ = part.Close()
			if uerr != nil {
				if limited.tooLarge.Load() {
					writeError(w, http.StatusRequestEntityTooLarge, "pve_upload_too_large", "备份上传超过 2 TiB 上限")
					return
				}
				if errors.Is(uerr, pveadmin.ErrReadonly) {
					recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEBackupUpload,
						TargetType: auditTargetPVECluster, TargetID: id,
						Detail: map[string]any{"result": "拒绝(只读模式)", "filename": fileName}, IP: clientIP(r)})
				}
				writePVEAdminError(w, uerr)
				return
			}
			if limited.tooLarge.Load() {
				writeError(w, http.StatusRequestEntityTooLarge, "pve_upload_too_large", "备份上传超过 2 TiB 上限")
				return
			}
			recordAudit(r.Context(), aud, audit.Entry{Actor: auditActor, Action: auditActionPVEBackupUpload,
				TargetType: auditTargetPVECluster, TargetID: id,
				Detail: map[string]any{"result": "受理", "node": node, "storage": storage, "filename": fileName}, IP: clientIP(r)})
			writeJSON(w, http.StatusOK, map[string]any{"volid": volid})
			return
		}
	}
}
