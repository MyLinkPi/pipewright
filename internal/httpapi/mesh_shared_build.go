package httpapi

// mesh_shared_build.go 挂载集群级共享构建机(0086)的 HTTP 端点:
//   - mesh 入站(签名 + build ACL,豁免 session/CSRF):目录 / 任务受理 / SSE 日志 /
//     状态轮询 / 产物回拉 / 取消 —— 供集群对端(发起方)调用;
//   - 本端管理面(requireAuth + CSRF + requireAdmin):任务列表 / 强制取消 —— 供所属环境
//     管理员查看"谁在用我的共享机"并处置。
//
// 运行时实现是 build.SharedBuildServer(经 sharedBuildRuntime 接口注入,本包不直依赖
// build,与 meshCatalogSource 同模式);排队/执行的领域语义见该文件头。
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/build"
	"github.com/huangchengsir/pipewright/internal/mesh"
)

// sharedBuildRuntime 抽象共享构建提供方运行时(build.SharedBuildServer 原样满足;
// 出站提交/跟踪经由 mesh.Client,与该接口无关)。
type sharedBuildRuntime interface {
	Accept(ctx context.Context, peerNodeID, peerName string, spec *mesh.SharedJobSpec, ws io.Reader) (string, error)
	Subscribe(jobID string) (<-chan mesh.SharedJobEvent, func(), error)
	Status(jobID string) (mesh.SharedJobStatus, error)
	Result(jobID string) (path, sha string, size int64, err error)
	Cancel(jobID string) error
	List() []mesh.SharedJobStatus
	CatalogMachines(ctx context.Context) ([]mesh.CatalogBuildMachine, error)
}

// 编译期断言:装配层注入的具体类型满足接口。
var _ sharedBuildRuntime = (*build.SharedBuildServer)(nil)

// permBuild 是第五张授权单(build_allowed)的判定适配。
func permBuild(ctx context.Context, svc mesh.Service, nodeID string) bool {
	return svc.AllowsBuild(ctx, nodeID)
}

// mountMeshSharedBuildInbound 把共享构建的 mesh 入站端点挂到根路由(mountMeshInbound 内调用)。
func mountMeshSharedBuildInbound(r chi.Router, d meshDeps) {
	if d.shared == nil {
		return // 运行时未装配(瘦控制机等):端点整体缺席,对端按目录不可达降级
	}
	gate := meshGate(d.mesh, permBuild)
	r.Get("/api/mesh/build/catalog", gate(makeSharedBuildCatalogHandler(d)))
	r.Post("/api/mesh/build/jobs", gate(makeSharedJobSubmitHandler(d)))
	r.Get("/api/mesh/build/jobs/{id}", gate(makeSharedJobStatusHandler(d)))
	r.Get("/api/mesh/build/jobs/{id}/events", gate(makeSharedJobEventsHandler(d)))
	r.Get("/api/mesh/build/jobs/{id}/result", gate(makeSharedJobResultHandler(d)))
	r.Delete("/api/mesh/build/jobs/{id}", gate(makeSharedJobCancelHandler(d)))
}

// sharedSpecMax 限制 spec 段大小(workspace 段流式落盘不受限)。
const sharedSpecMax = 1 << 20

// makeSharedBuildCatalogHandler 返回本端共享构建机目录(授权对端拉取;选择器预览/钉死校验用)。
func makeSharedBuildCatalogHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machines, err := d.shared.CatalogMachines(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "读取共享构建机目录失败")
			return
		}
		name := ""
		if st, serr := d.mesh.Settings(r.Context()); serr == nil {
			name = st.Name
		}
		writeJSON(w, http.StatusOK, mesh.BuildCatalog{NodeName: name, Machines: machines})
	}
}

// makeSharedJobSubmitHandler 受理共享构建任务:multipart(spec 段 + workspace 段)流式接收,
// workspace 直灌运行时暂存(无全量内存驻留)。sha256 不符 → 422;队列满 → 429。
func makeSharedJobSubmitHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "需要 multipart 请求体")
			return
		}
		var spec *mesh.SharedJobSpec
		var ws io.Reader
		for {
			part, perr := mr.NextPart()
			if perr == io.EOF {
				break
			}
			if perr != nil {
				writeError(w, http.StatusBadRequest, "invalid_body", "multipart 解析失败")
				return
			}
			switch part.FormName() {
			case "spec":
				if spec != nil {
					writeError(w, http.StatusBadRequest, "invalid_body", "重复的 spec 段")
					return
				}
				spec = &mesh.SharedJobSpec{}
				if err := json.NewDecoder(io.LimitReader(part, sharedSpecMax)).Decode(spec); err != nil {
					writeError(w, http.StatusBadRequest, "invalid_body", "spec 段不是合法 JSON")
					return
				}
			case "workspace":
				if ws != nil {
					writeError(w, http.StatusBadRequest, "invalid_body", "重复的 workspace 段")
					return
				}
				ws = part
			default:
				_, _ = io.Copy(io.Discard, part) // 未知段丢弃
			}
		}
		if spec == nil || ws == nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "缺少 spec/workspace 段")
			return
		}
		caller := meshCaller(r.Context())
		peerName := caller
		if peer, perr := d.mesh.GetPeer(r.Context(), caller); perr == nil && peer.Name != "" {
			peerName = peer.Name
		}
		jobID, aerr := d.shared.Accept(r.Context(), caller, peerName, spec, ws)
		if aerr != nil {
			switch {
			case errors.Is(aerr, build.ErrSharedQueueFull):
				writeError(w, http.StatusTooManyRequests, "queue_full", "对方共享构建队列已满,请稍后重试")
			case errors.Is(aerr, build.ErrSharedChecksum):
				writeError(w, http.StatusUnprocessableEntity, "checksum_mismatch", "工作区校验不符(传输损坏),请重试")
			case errors.Is(aerr, build.ErrSharedSpecInvalid):
				writeError(w, http.StatusUnprocessableEntity, "invalid_spec", aerr.Error())
			default:
				writeError(w, http.StatusInternalServerError, "internal", "受理共享构建任务失败")
			}
			return
		}
		writeJSON(w, http.StatusOK, mesh.SharedJobAccepted{JobID: jobID})
	}
}

// makeSharedJobStatusHandler 返回任务状态(轮询兜底;404 = 过期/对端重启)。
func makeSharedJobStatusHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := d.shared.Status(chi.URLParam(r, "id"))
		if err != nil {
			writeSharedJobError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// makeSharedJobEventsHandler 订阅任务 SSE:回放 ring 历史日志 → 实时日志/状态 → 终态即收流。
// 心跳 15s 保活(对端经 mesh.Client.readSSE 消费,注释行被忽略)。
func makeSharedJobEventsHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		events, unsub, err := d.shared.Subscribe(chi.URLParam(r, "id"))
		if err != nil {
			writeSharedJobError(w, err)
			return
		}
		defer unsub()

		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "internal", "服务器不支持流式响应")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		marshal := func(ev mesh.SharedJobEvent) string {
			b, _ := json.Marshal(ev)
			return string(b)
		}
		ctx := r.Context()
		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
					return
				}
				flusher.Flush()
			case ev, more := <-events:
				if !more {
					return
				}
				writeSSE(w, flusher, ev.Event, marshal(ev))
			}
		}
	}
}

// makeSharedJobResultHandler 流式下发产物归档(响应头带 sha256/大小,供发起方落盘校验)。
func makeSharedJobResultHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path, sha, size, err := d.shared.Result(chi.URLParam(r, "id"))
		if err != nil {
			writeSharedJobError(w, err)
			return
		}
		f, ferr := os.Open(path)
		if ferr != nil {
			writeError(w, http.StatusGone, "result_gone", "产物归档已过期或被清理")
			return
		}
		defer func() { _ = f.Close() }()
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("X-Pw-Sha256", sha)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
	}
}

// makeSharedJobCancelHandler 取消任务(排队撤销/运行中断;幂等)。
func makeSharedJobCancelHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := d.shared.Cancel(chi.URLParam(r, "id")); err != nil {
			writeSharedJobError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// writeSharedJobError 映射共享任务错误到状态码。
func writeSharedJobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, build.ErrSharedJobNotFound):
		writeError(w, http.StatusNotFound, "job_not_found", "共享构建任务不存在或已过期")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "共享构建服务内部错误")
	}
}

// ---- 本端管理面(owner 视角;requireAdmin 组内挂载) ----

// mountSharedBuildAdmin 挂载共享构建管理端点(admin 路由组内调用)。
func mountSharedBuildAdmin(r chi.Router, d meshDeps) {
	if d.shared == nil {
		return
	}
	r.Get("/api/build/shared/jobs", makeSharedBuildAdminListHandler(d))
	r.Delete("/api/build/shared/jobs/{id}", makeSharedBuildAdminCancelHandler(d))
}

// makeSharedBuildAdminListHandler 返回在册共享构建任务(来源环境/机器/状态/时长)。
func makeSharedBuildAdminListHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"jobs": d.shared.List()})
	}
}

// makeSharedBuildAdminCancelHandler 管理员强制取消一个跨环境共享构建任务(留审计)。
func makeSharedBuildAdminCancelHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := d.shared.Cancel(id); err != nil {
			writeSharedJobError(w, err)
			return
		}
		recordAudit(r.Context(), d.audit, audit.Entry{
			Action:     "shared_build_canceled",
			TargetType: "server",
			TargetID:   id,
			Detail:     map[string]any{"jobId": id, "by": "admin"},
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
