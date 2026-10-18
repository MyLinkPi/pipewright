// fileadmin.go 是「服务器文件查看」的 HTTP 面(admin session 鉴权,挂受保护 /api 组的
// requireAdmin 子组,路径前缀 /api/fileadmin/servers/{id}):
//   - GET  files           — 目录列表(?path=);
//   - GET  file            — 小文本内容与元数据(?path=;>64KiB/二进制仅元数据);
//   - GET  file/download   — 流式下载(?path=;octet-stream + Content-Disposition);
//   - PUT  file            — 覆盖保存(body {path, content};CSRF + 审计;服务端先
//     cp -a 同目录备份再写入)。
//
// 纪律:命令构造与路径校验在领域层(AC-SEC-02 argv 化);写操作留审计;错误体可带
// 远端 stderr 尾部,绝无凭据。仅管理员可访问(文件系统全量视图)。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/fileadmin"
)

// 审计 action(前缀 fileadmin.*)。
const (
	auditActionFileWrite  = "fileadmin.file.write"
	auditTargetFileServer = "file_server"
)

// fileWriteBodyMax 是保存请求体上限(64KiB 内容 + JSON 转义开销余量)。
const fileWriteBodyMax = 72 << 10

// mountFileAdmin 把文件查看端点挂进 admin 子组。
func mountFileAdmin(ar chi.Router, svc fileadmin.Service, rec audit.Recorder) {
	ar.Route("/fileadmin/servers/{id}", func(r chi.Router) {
		r.Get("/files", makeFileListHandler(svc))
		r.Get("/file", makeFileReadHandler(svc))
		r.Get("/file/download", makeFileDownloadHandler(svc))
		r.Put("/file", makeFileWriteHandler(svc, rec))
	})
}

// writeFileAdminError 把领域错误映射为契约错误码/状态码(错误体可带 stderr 尾部)。
func writeFileAdminError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fileadmin.ErrServerNotFound):
		writeError(w, http.StatusNotFound, "file_server_not_found", err.Error())
	case errors.Is(err, fileadmin.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_file_input", err.Error())
	case errors.Is(err, fileadmin.ErrNotFound):
		writeError(w, http.StatusNotFound, "file_not_found", err.Error())
	case errors.Is(err, fileadmin.ErrNotDir):
		writeError(w, http.StatusUnprocessableEntity, "file_not_dir", err.Error())
	case errors.Is(err, fileadmin.ErrNotFile):
		writeError(w, http.StatusUnprocessableEntity, "file_not_regular", err.Error())
	case errors.Is(err, fileadmin.ErrExec):
		writeError(w, http.StatusBadGateway, "file_exec_failed", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// filePathParam 取 ?path= 查询参数(空 → 400,不透传到领域层)。
func filePathParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := r.URL.Query().Get("path")
	if p == "" {
		writeError(w, http.StatusBadRequest, "invalid_file_input", "缺少 path 参数")
		return "", false
	}
	return p, true
}

// makeFileListHandler 返回 GET .../files handler(admin,只读;?path= 目录)。
func makeFileListHandler(svc fileadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "文件查看服务未初始化")
			return
		}
		p, ok := filePathParam(w, r)
		if !ok {
			return
		}
		listing, err := svc.List(r.Context(), chi.URLParam(r, "id"), p)
		if err != nil {
			writeFileAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, listing)
	}
}

// makeFileReadHandler 返回 GET .../file handler(admin,只读;?path= 文件)。
// >64KiB 或二进制不报错,以 TooLarge/Binary 标记引导前端走下载。
func makeFileReadHandler(svc fileadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "文件查看服务未初始化")
			return
		}
		p, ok := filePathParam(w, r)
		if !ok {
			return
		}
		tf, err := svc.Read(r.Context(), chi.URLParam(r, "id"), p)
		if err != nil {
			writeFileAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, tf)
	}
}

// makeFileDownloadHandler 返回 GET .../file/download handler(admin,流式下载)。
// 头三件套(octet-stream + nosniff + attachment)与制品下载先例一致;Content-Length
// 取自下载前的 stat。流开始后中途失败只能记日志(头已发出)。
func makeFileDownloadHandler(svc fileadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "文件查看服务未初始化")
			return
		}
		p, ok := filePathParam(w, r)
		if !ok {
			return
		}
		rc, size, err := svc.Download(r.Context(), chi.URLParam(r, "id"), p)
		if err != nil {
			writeFileAdminError(w, err)
			return
		}
		defer func() { _ = rc.Close() }()

		name := sanitizeFilename(path.Base(p))
		if name == "" || name == "." || name == "/" {
			name = "file"
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, rc); err != nil {
			// 头已发出,无法改状态码;客户端断开/网络中断属常态,记日志即可。
			log.Printf("[fileadmin] 下载流中断(%s):%v", p, err)
		}
	}
}

// makeFileWriteHandler 返回 PUT .../file handler(admin + CSRF + 审计)。
// body: { path, content };服务端先同目录备份再覆盖写入,响应带回备份文件路径。
func makeFileWriteHandler(svc fileadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "文件查看服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, fileWriteBodyMax)
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误或超过大小上限")
			return
		}
		backup, err := svc.Write(r.Context(), id, in.Path, in.Content)
		if err != nil {
			writeFileAdminError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor: auditActor, Action: auditActionFileWrite, TargetType: auditTargetFileServer,
			TargetID: id,
			Detail:   map[string]any{"path": in.Path, "bytes": len(in.Content), "backup": backup},
			IP:       clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "backup": backup})
	}
}
