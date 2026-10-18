// mesh_inbound.go 是集群对等节点的入站 API(/api/mesh/*)。
//
// 与 webhook 同款挂载位置:在受保护 /api 组之外注册(豁免 session/CSRF),鉴权改走
// meshAuth——节点 ed25519 签名头(X-PW-Mesh-*)。验签通过 ≠ 放行:数据类端点还须过
// 接收方 ACL(visible_to / push_allowed / trigger_allowed,默认全拒绝)。
//
// /api/mesh/hello 是唯一免签端点(发现握手的 TOFU 锚点:公钥由此首次交换;网络层
// EasyTier network-secret 已先行把关,应用层随后续请求验签)。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/pushsource"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/version"
)

// meshCallerKey 是入站调用方节点 id 的 context 键。
type meshCallerKeytype int

const meshCallerKey meshCallerKeytype = 1

// meshCaller 从 context 取已验签的调用方节点 id(空 = 未过 meshAuth)。
func meshCaller(ctx context.Context) string {
	v, _ := ctx.Value(meshCallerKey).(string)
	return v
}

// meshDeps 汇集 mesh 入站/管理面所需依赖(装配层一次性注入)。
type meshDeps struct {
	mesh       mesh.Service
	receiver   *mesh.Receiver
	client     *mesh.Client
	projects   project.Service
	runs       run.Service
	runSub     runSubscriber
	parameters run.ParameterService
	servers    target.Service
	pipelines  pipelineLister
	audit      audit.Recorder
	pushedRevs pushsource.Store
	// catalogSrc 提供本端服务目录(跨环境转发;/api/mesh/servicereg/catalog 的数据源;
	// servicereg.Service 满足;nil = 端点 503 降级)。装配层经 WithServiceCatalog 注入。
	catalogSrc meshCatalogSource
	// advertisePort 是广播给对端的 HTTP 端口(0 = 跟随服务监听口,装配层已解析)。
	advertisePort int
}

// meshCatalogSource 抽象「本端服务目录」读取(servicereg.Service 原样满足;
// 出站 catalog 拉取经由 mesh.Client,与该接口无关)。
type meshCatalogSource interface {
	MeshCatalog(ctx context.Context) (*mesh.ServiceCatalog, error)
}

// pipelineLister 抽象「判断项目是否已保存流水线」(pipeline.Service.HasConfig 原样满足)。
type pipelineLister interface {
	HasConfig(ctx context.Context, projectID string) bool
}

// meshAuth 校验节点签名并把调用方 node_id 注入 context。对未知节点一律 401
// (公钥交换只发生在 hello 的 TOUF 路径);验签失败不区分细节(防探测)。
func meshAuth(svc mesh.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if svc == nil {
				writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
				return
			}
			st, err := svc.Settings(r.Context())
			if err != nil || !st.Enabled {
				writeError(w, http.StatusNotFound, "not_found", "集群未启用")
				return
			}
			nodeID := strings.TrimSpace(r.Header.Get(mesh.HeaderNode))
			if nodeID == "" {
				writeError(w, http.StatusUnauthorized, "unauthorized", "缺少节点签名头")
				return
			}
			peer, err := svc.GetPeer(r.Context(), nodeID)
			if err != nil || peer.PublicKey == "" {
				writeError(w, http.StatusUnauthorized, "unauthorized", "未知节点")
				return
			}
			if err := mesh.VerifyRequest(r, peer.PublicKey); err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "节点签名校验失败")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), meshCallerKey, peer.NodeID)))
		})
	}
}

// meshRequire 在 meshAuth 之后叠加 ACL 判定(perm 指定三张授权单之一;默认拒绝)。
func meshRequire(svc mesh.Service, perm func(context.Context, mesh.Service, string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller := meshCaller(r.Context())
			if caller == "" {
				writeError(w, http.StatusUnauthorized, "unauthorized", "缺少节点身份")
				return
			}
			if !perm(r.Context(), svc, caller) {
				writeError(w, http.StatusForbidden, "forbidden", "节点未获授权")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// meshGate 组合「签名鉴权 + ACL 判定」为一层中间件(perm 指定三张授权单之一;默认拒绝)。
func meshGate(svc mesh.Service, perm func(context.Context, mesh.Service, string) bool) func(http.HandlerFunc) http.HandlerFunc {
	auth := meshAuth(svc)
	acl := meshRequire(svc, perm)
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			auth(acl(next)).ServeHTTP(w, r)
		}
	}
}

// permVisible/permPush/permTrigger/permForward 是四张授权单的判定适配。
func permVisible(ctx context.Context, svc mesh.Service, nodeID string) bool {
	return svc.AllowsVisible(ctx, nodeID)
}

func permPush(ctx context.Context, svc mesh.Service, nodeID string) bool {
	return svc.AllowsPush(ctx, nodeID)
}

func permTrigger(ctx context.Context, svc mesh.Service, nodeID string) bool {
	return svc.AllowsTrigger(ctx, nodeID)
}

func permForward(ctx context.Context, svc mesh.Service, nodeID string) bool {
	return svc.AllowsForward(ctx, nodeID)
}

// mountMeshInbound 把对等入站端点挂到根路由(与 webhook 同级;豁免 session/CSRF)。
func mountMeshInbound(r chi.Router, d meshDeps) {
	r.Get("/api/mesh/hello", makeMeshHelloHandler(d))
	visible := meshGate(d.mesh, permVisible)
	push := meshGate(d.mesh, permPush)
	trigger := meshGate(d.mesh, permTrigger)
	forward := meshGate(d.mesh, permForward)
	r.Get("/api/mesh/servers/metrics", visible(makeAllServerMetricsHandler(d.servers)))
	r.Get("/api/mesh/projects", visible(makeMeshProjectsHandler(d)))
	r.Get("/api/mesh/projects/{id}/runs", visible(makeMeshProjectRunsHandler(d)))
	r.Post("/api/mesh/projects/{id}/runs", trigger(makeMeshTriggerRunHandler(d)))
	// by-origin 寻址:按 (来源节点, 来源项目) 定位对方推送建立的对应项目(mesh_trigger 的
	// origin 模式用;比 {id} 多一段,不会被吞)。
	r.Post("/api/mesh/projects/by-origin/{node}/{ref}/runs", trigger(makeMeshTriggerRunByOriginHandler(d)))
	r.Get("/api/mesh/runs/{id}", trigger(makeMeshRunStatusHandler(d)))
	r.Get("/api/mesh/runs/{id}/events", trigger(makeRunEventsHandler(d.runs, d.runSub)))
	r.Post("/api/mesh/push/preflight", push(makeMeshPushPreflightHandler(d)))
	r.Post("/api/mesh/push", push(makeMeshPushHandler(d)))
	// 跨环境转发:本端服务目录(对端渲染 forward 服务 + 规划 /32 路由的数据源)。
	r.Get("/api/mesh/servicereg/catalog", forward(makeMeshServiceCatalogHandler(d)))
}

// makeMeshServiceCatalogHandler 返回本端服务目录(网关主机 + 可转发服务;授权对端拉取)。
// 数据源未装配 → 503(对端按目录不可达降级渲染 503 维护态)。
func makeMeshServiceCatalogHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.catalogSrc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "服务注册未初始化")
			return
		}
		cat, err := d.catalogSrc.MeshCatalog(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, cat)
	}
}

// makeMeshHelloHandler 是发现握手:返回本节点身份(公钥随此 TOFU 交换)。
func makeMeshHelloHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		st, err := d.mesh.Settings(r.Context())
		if err != nil || !st.Enabled {
			writeError(w, http.StatusNotFound, "not_found", "集群未启用")
			return
		}
		if st.PublicKey == "" {
			writeError(w, http.StatusServiceUnavailable, "internal", "节点密钥未就绪(需配置 master key)")
			return
		}
		writeJSON(w, http.StatusOK, mesh.HelloInfo{
			NodeID:    st.NodeID,
			Name:      st.Name,
			Version:   version.Get().Version,
			PublicKey: st.PublicKey,
			APIPort:   d.advertisePort,
		})
	}
}

// meshPeerProject 是 projects 列表条目(供对端 mesh_trigger 选择器与集群页展示)。
type meshPeerProject struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	SourceType  string          `json:"sourceType"`
	HasPipeline bool            `json:"hasPipeline"`
	Params      []meshPeerParam `json:"params"`
}

type meshPeerParam struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Required     bool   `json:"required"`
	DefaultValue string `json:"default"`
}

// makeMeshProjectsHandler(visible):项目清单 + 流水线就绪标志 + 参数 schema。
func makeMeshProjectsHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.projects == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "项目服务未初始化")
			return
		}
		list, err := d.projects.List(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		out := make([]meshPeerProject, 0, len(list))
		for _, p := range list {
			item := meshPeerProject{ID: p.ID, Name: p.Name, SourceType: p.SourceType, Params: []meshPeerParam{}}
			if d.pipelines != nil {
				item.HasPipeline = d.pipelines.HasConfig(r.Context(), p.ID)
			}
			if d.parameters != nil {
				if cfg, perr := d.parameters.Get(r.Context(), p.ID); perr == nil && cfg != nil {
					for _, def := range cfg.Defs {
						item.Params = append(item.Params, meshPeerParam{
							Name: def.Key, Type: def.Type, Required: def.Required, DefaultValue: def.Default,
						})
					}
				}
			}
			out = append(out, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// makeMeshProjectRunsHandler(visible):项目近期运行摘要(集群页流水线状态)。
func makeMeshProjectRunsHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.runs == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "运行服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		res, err := d.runs.List(r.Context(), run.ListFilter{ProjectID: id, Page: 1, PageSize: 20})
		if err != nil {
			writeServerError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(res.Items))
		for _, rn := range res.Items {
			item := map[string]any{
				"id": rn.ID, "status": rn.Status, "trigger": rn.Trigger.Type,
				"actor": rn.Trigger.Actor, "createdAt": rn.CreatedAt,
			}
			if rn.FinishedAt != nil {
				item["finishedAt"] = rn.FinishedAt
			}
			items = append(items, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makeMeshTriggerRunHandler(trigger_allowed):对等远程触发本节点项目工作流。
// 来源记入 actor(mesh:<对端节点名>);pushed 项目可带 revision(经保留参数键传递)。
func makeMeshTriggerRunHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		triggerMeshRun(w, r, d, chi.URLParam(r, "id"))
	}
}

// makeMeshTriggerRunByOriginHandler(trigger_allowed):按对应键寻址触发(mesh_trigger 的
// origin 模式)。项目不存在 → 404(提示先推送)。
func makeMeshTriggerRunByOriginHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.projects == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "项目服务未初始化")
			return
		}
		p, err := d.projects.GetByOrigin(r.Context(), chi.URLParam(r, "node"), chi.URLParam(r, "ref"))
		if err != nil {
			writeRunError(w, err)
			return
		}
		triggerMeshRun(w, r, d, p.ID)
	}
}

// triggerMeshRun 是远程触发的公共实现(显式 {id} 与 by-origin 寻址共用)。
func triggerMeshRun(w http.ResponseWriter, r *http.Request, d meshDeps, projectID string) {
	if d.runs == nil || d.mesh == nil {
		writeError(w, http.StatusServiceUnavailable, "internal", "运行服务未初始化")
		return
	}
	var body struct {
		Params   map[string]string `json:"params"`
		Revision string            `json:"revision"`
		Actor    string            `json:"actor"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}
	params := body.Params
	if params == nil {
		params = map[string]string{}
	}
	if strings.TrimSpace(body.Revision) != "" {
		params[pushsource.ParamRevision] = strings.TrimSpace(body.Revision)
	}
	caller := meshCaller(r.Context())
	peer, _ := d.mesh.GetPeer(r.Context(), caller)
	actor := strings.TrimSpace(body.Actor)
	if actor == "" && peer != nil {
		actor = peer.Name
		if actor == "" {
			actor = caller
		}
	}
	rn, err := d.runs.Create(r.Context(), projectID, run.Trigger{
		Type:   run.TriggerMesh,
		Actor:  "mesh:" + actor,
		Params: params,
	})
	if err != nil {
		writeRunError(w, err)
		return
	}
	recordAudit(r.Context(), d.audit, audit.Entry{
		Actor:      "mesh:" + actor,
		Action:     "run_trigger_mesh",
		TargetType: "run",
		TargetID:   rn.ID,
		Detail:     map[string]any{"project": projectID, "peerNode": caller},
		IP:         clientIP(r),
	})
	writeJSON(w, http.StatusOK, mesh.RunTriggerResult{
		RunID:   rn.ID,
		RunPath: "/runs/" + rn.ID,
		Status:  rn.Status,
	})
}

// makeMeshRunStatusHandler(trigger_allowed):单运行状态摘要(mesh_trigger 等待期的轮询兜底)。
func makeMeshRunStatusHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.runs == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "运行服务未初始化")
			return
		}
		rn, err := d.runs.Get(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeRunError(w, err)
			return
		}
		out := map[string]any{"id": rn.ID, "status": rn.Status, "trigger": rn.Trigger.Type}
		if rn.FinishedAt != nil {
			out["finishedAt"] = rn.FinishedAt
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// makeMeshPushPreflightHandler(push_allowed):报告接收方缺失的 blob(发送方据此只传缺段)。
func makeMeshPushPreflightHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.receiver == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "推送接收未初始化")
			return
		}
		var manifest mesh.PushManifest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&manifest); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"missing": d.receiver.MissingBlobs(manifest)})
	}
}

// meshPushManifestMax 限制清单段大小(blob 段不受限,流式直入库)。
const meshPushManifestMax = 1 << 20

// makeMeshPushHandler(push_allowed):multipart 流式接收——manifest 段 + 逐 blob 段。
// 任一 blob 哈希与清单不符即 422(整次失败;已入库 blob 内容寻址无害,重推只补缺)。
func makeMeshPushHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.receiver == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "推送接收未初始化")
			return
		}
		mr, err := r.MultipartReader()
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "需要 multipart 请求体")
			return
		}
		var ing *mesh.PushIngest
		for {
			part, perr := mr.NextPart()
			if perr == io.EOF {
				break
			}
			if perr != nil {
				writeError(w, http.StatusBadRequest, "invalid_body", "multipart 解析失败")
				return
			}
			name := part.FormName()
			switch {
			case name == "manifest" && ing == nil:
				var manifest mesh.PushManifest
				if err := json.NewDecoder(io.LimitReader(part, meshPushManifestMax)).Decode(&manifest); err != nil {
					writeError(w, http.StatusBadRequest, "invalid_body", "manifest 段不是合法 JSON")
					return
				}
				ing, err = d.receiver.Begin(manifest)
				if err != nil {
					writeMeshError(w, err)
					return
				}
			case name == "blob":
				if ing == nil {
					writeError(w, http.StatusBadRequest, "invalid_body", "manifest 段必须先于 blob 段")
					return
				}
				// 段文件名约定为 blob sha256(发送方 client.Push 生成)。
				key := strings.TrimSpace(part.FileName())
				if err := ing.Blob(d.receiver, key, part); err != nil {
					writeMeshError(w, err)
					return
				}
			default:
				_, _ = io.Copy(io.Discard, part) // 未知段丢弃
			}
		}
		if ing == nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "缺少 manifest 段")
			return
		}
		result, err := d.receiver.Commit(r.Context(), ing)
		if err != nil {
			writeMeshError(w, err)
			return
		}
		caller := meshCaller(r.Context())
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor:      "mesh:peer",
			Action:     "mesh_push_received",
			TargetType: "project",
			TargetID:   result.ProjectID,
			Detail: map[string]any{
				"peerNode": caller, "revision": result.RevisionID, "created": result.Created,
			},
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, result)
	}
}

// writeMeshError 把 mesh 领域错误映射为 HTTP 状态码(内部错误服务端留日志,便于排障)。
func writeMeshError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mesh.ErrInvalidInput):
		writeError(w, http.StatusUnprocessableEntity, "invalid_push", err.Error())
	case errors.Is(err, mesh.ErrPeerUnreachable):
		writeError(w, http.StatusBadGateway, "peer_unreachable", err.Error())
	default:
		log.Printf("[mesh] push handling error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "推送处理失败")
	}
}
