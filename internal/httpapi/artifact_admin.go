// artifact_admin.go 是「产物管理面」的 HTTP 面(admin 子组挂载):
//   - GET  /api/artifacts(跨运行产物列表:筛选 projectId/type + 分页;产物管理页数据源);
//   - DELETE /api/runs/{id}/artifacts/{artifactId}(删记录 + 镜像类联动删内置 registry tag
//     + 已无引用的制品库 blob best-effort 回收);
//   - GET  /api/runs/{id}/artifacts/{artifactId}/image-archive(镜像类产物 tar 导出下载:
//     EnsureLocal(pull 若不在本地)→ docker save 流式输出)。
//
// 纪律:产物契约(artifactDTO)保持冻结,管理面用独立的 artifactRowDTO(additive 字段);
// 删除非终态运行 → 409(进行中产物行是按节点恢复/跨阶段恢复的基线);镜像 tag 删除失败 →
// 502 且记录不动(可重试;digest 共租保留别名后继续删记录);全部删除写审计。
package httpapi

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/artifactstore"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/retention"
	"github.com/huangchengsir/pipewright/internal/run"
)

// artifactImageExporter 抽象「导出镜像 tar」的能力(*build.ImageTransfer 实现;测试注入 fake)。
type artifactImageExporter interface {
	// EnsureLocal 确保镜像在本机 daemon(不在则 pull;外置私有仓库可能失败)。
	EnsureLocal(ctx context.Context, ref string) error
	// ExportStream 把镜像归档(docker save)流式写到 w。
	ExportStream(ctx context.Context, ref string, w io.Writer) error
}

// artifactRowDTO 是 GET /api/artifacts 列表行(管理面):冻结 artifactDTO 字段 +
// 关联运行/项目信息(additive,不改 artifactDTO 本身)。
type artifactRowDTO struct {
	artifactDTO
	RunID       string `json:"runId"`
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	RunStatus   string `json:"runStatus"`
}

// artifactAdminListDTO 是 GET /api/artifacts 响应体(仿 runListDTO)。
type artifactAdminListDTO struct {
	Items []artifactRowDTO `json:"items"`
	Page  int              `json:"page"`
	Total int              `json:"total"`
}

// makeArtifactsListHandler 返回 GET /api/artifacts handler(产物管理页数据源;admin)。
// 筛选 projectId/type + 分页 page/pageSize(1 起,pageSize 钳制 [1,100]);按产生时间倒序。
func makeArtifactsListHandler(svc run.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "运行服务未初始化")
			return
		}
		q := r.URL.Query()
		page := 1
		if v := q.Get("page"); v != "" {
			p, err := strconv.Atoi(v)
			if err != nil || p < 1 {
				writeError(w, http.StatusBadRequest, "invalid_page", "page 必须为不小于 1 的整数")
				return
			}
			if p > maxPage {
				writeError(w, http.StatusBadRequest, "invalid_page", "page 超出上界")
				return
			}
			page = p
		}
		pageSize := 20
		if v := q.Get("pageSize"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				writeError(w, http.StatusBadRequest, "invalid_page_size", "pageSize 必须为 1~100 的整数")
				return
			}
			pageSize = n
		}
		res, err := svc.ListAllArtifacts(r.Context(), run.ArtifactFilter{
			ProjectID: q.Get("projectId"),
			Type:      q.Get("type"),
			Page:      page,
			PageSize:  pageSize,
		})
		if err != nil {
			if errors.Is(err, run.ErrInvalidArtifactType) {
				writeError(w, http.StatusBadRequest, "invalid_artifact_type", "产物类型筛选值非法")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		items := make([]artifactRowDTO, 0, len(res.Items))
		for i := range res.Items {
			e := &res.Items[i]
			items = append(items, artifactRowDTO{
				artifactDTO: toArtifactDTO(e.Artifact),
				RunID:       e.RunID,
				ProjectID:   e.ProjectID,
				ProjectName: e.ProjectName,
				RunStatus:   e.RunStatus,
			})
		}
		writeJSON(w, http.StatusOK, artifactAdminListDTO{Items: items, Page: res.Page, Total: res.Total})
	}
}

// builtinImageRepoTag 判定镜像 reference 是否指向内置 registry(<addr>/<repo>:<tag> 形态),
// 是则拆出 repo 与 tag(无 tag 后缀视为 :latest)。regHub 为 nil / 未启用 / 前缀不匹配 → ok=false。
func builtinImageRepoTag(ctx context.Context, regHub registryHubService, ref string) (repo, tag string, ok bool) {
	if regHub == nil {
		return "", "", false
	}
	addr, enabled := regHub.ResolveBuiltin(ctx)
	if !enabled || strings.TrimSpace(addr) == "" {
		return "", "", false
	}
	prefix := strings.TrimSuffix(addr, "/") + "/"
	if !strings.HasPrefix(ref, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(ref, prefix)
	slash := strings.LastIndex(rest, "/")
	colon := strings.LastIndex(rest, ":")
	if colon > slash {
		return rest[:colon], rest[colon+1:], true
	}
	return rest, "latest", true
}

// makeArtifactDeleteHandler 返回 DELETE /api/runs/{id}/artifacts/{artifactId}(admin)。
// 顺序:终态护栏 → 镜像类联动删内置 registry tag(失败即中止,记录不动)→ 删记录 →
// best-effort 回收已无引用的制品库 blob → 审计。响应 { deleted, registryTagDeleted, blobsRemoved }。
func makeArtifactDeleteHandler(svc run.Service, store *artifactstore.Store, regHub registryHubService, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "运行服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		artID := chi.URLParam(r, "artifactId")

		rn, err := svc.Get(r.Context(), id)
		if err != nil {
			writeRunError(w, err)
			return
		}
		if !run.IsTerminal(rn.Status) {
			writeError(w, http.StatusConflict, "run_not_finished", "运行尚未结束,产物不可删除")
			return
		}
		arts, err := svc.ListArtifacts(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		var art *run.Artifact
		for i := range arts {
			if arts[i].ID == artID {
				art = &arts[i]
				break
			}
		}
		if art == nil {
			writeError(w, http.StatusNotFound, "artifact_not_found", "产物不存在")
			return
		}

		// 镜像类 + 内置 registry:先删 tag(失败 → 502 且记录不动,语义明确可重试;
		// digest 共租 → 别名保留,registryTagDeleted=false,继续删记录)。
		registryTagDeleted := false
		if art.Type == run.ArtifactImage {
			if repo, tag, builtin := builtinImageRepoTag(r.Context(), regHub, art.Reference); builtin {
				deleted, derr := regHub.DeleteTag(r.Context(), repo, tag)
				if derr != nil {
					writeError(w, http.StatusBadGateway, "registry_delete_failed",
						"删除内置 registry 镜像 tag 失败:"+derr.Error())
					return
				}
				registryTagDeleted = deleted
			}
		}

		deletedArt, orphanKeys, derr := svc.DeleteArtifact(r.Context(), id, artID)
		if derr != nil {
			switch {
			case errors.Is(derr, run.ErrNotFound):
				writeError(w, http.StatusNotFound, "run_not_found", "运行不存在")
			case errors.Is(derr, run.ErrArtifactNotFound):
				writeError(w, http.StatusNotFound, "artifact_not_found", "产物不存在")
			case errors.Is(derr, run.ErrRunNotFinished):
				writeError(w, http.StatusConflict, "run_not_finished", "运行尚未结束,产物不可删除")
			default:
				writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			}
			return
		}

		// 已无引用的 blob 物理删除(best-effort;复用 GC 的 retention 存储面,失败留 GC 兜底)。
		blobsRemoved := 0
		if store != nil && len(orphanKeys) > 0 {
			bs := retention.NewBlobStore(store.Root())
			for _, k := range orphanKeys {
				if err := bs.Remove(k); err == nil {
					blobsRemoved++
				} else {
					log.Printf("[artifacts] 回收制品 blob %s 失败(留待 GC):%v", k[:12], err)
				}
			}
		}

		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     "runs.artifact.delete",
			TargetType: "run_artifact",
			TargetID:   artID,
			Detail: map[string]any{
				"runId":              id,
				"name":               deletedArt.Name,
				"type":               deletedArt.Type,
				"reference":          deletedArt.Reference,
				"registryTagDeleted": registryTagDeleted,
				"blobsRemoved":       blobsRemoved,
			},
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"deleted":            true,
			"registryTagDeleted": registryTagDeleted,
			"blobsRemoved":       blobsRemoved,
		})
	}
}

// makeArtifactImageArchiveHandler 返回 GET /api/runs/{id}/artifacts/{artifactId}/image-archive
// (admin;镜像类产物下载):EnsureLocal(pull 若不在本地;外置私有仓库可能失败 → 502 诚实报错)
// → docker save 流式导出 .tar(不落临时文件)。type 非 image → 400。
func makeArtifactImageArchiveHandler(svc run.Service, exporter artifactImageExporter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "运行服务未初始化")
			return
		}
		if exporter == nil {
			writeError(w, http.StatusServiceUnavailable, "image_export_unavailable", "镜像导出器未启用(需控制机容器 CLI)")
			return
		}
		id := chi.URLParam(r, "id")
		artID := chi.URLParam(r, "artifactId")
		if _, err := svc.Get(r.Context(), id); err != nil {
			writeRunError(w, err)
			return
		}
		arts, err := svc.ListArtifacts(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}
		var art *run.Artifact
		for i := range arts {
			if arts[i].ID == artID {
				art = &arts[i]
				break
			}
		}
		if art == nil {
			writeError(w, http.StatusNotFound, "artifact_not_found", "产物不存在")
			return
		}
		if art.Type != run.ArtifactImage {
			writeError(w, http.StatusBadRequest, "artifact_not_image", "仅镜像类产物可导出 tar")
			return
		}
		ref := strings.TrimSpace(art.Reference)
		if ref == "" {
			writeError(w, http.StatusNotFound, "artifact_not_downloadable", "该产物无镜像引用,无法导出")
			return
		}
		if err := exporter.EnsureLocal(r.Context(), ref); err != nil {
			writeError(w, http.StatusBadGateway, "image_export_failed",
				"镜像定位/拉取失败(外置私有仓库需凭据时不可导出)")
			return
		}
		name := sanitizeFilename(art.Name)
		if name == "" {
			name = "image"
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+name+".tar\"")
		if err := exporter.ExportStream(r.Context(), ref, w); err != nil {
			// 头已发出,只能记日志(客户端见截断流;浏览器 download 会报失败)。
			log.Printf("[artifacts] 导出镜像 tar 失败(%s):%v", ref, err)
		}
	}
}
