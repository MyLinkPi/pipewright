// mesh_admin.go 是集群管理面(本节点 UI 用,admin session 鉴权,挂受保护 /api 组内):
//   - GET/PUT /api/mesh/settings :本节点身份与 EasyTier 组网配置;
//   - GET/PUT /api/mesh/acl      :三张默认全拒绝的授权单;
//   - GET/POST/DELETE /api/mesh/peers :已知节点注册表(发现登记 + 手工补录);
//   - GET /api/mesh/overview     :集群页聚合(本节点视角:EasyTier 链路 + 节点表 + 监督器运行态);
//   - GET /api/mesh/peers/{id}/{servers,projects,runs} :经本节点代理查询对端数据
//     (对端 ACL 决定可见性;未授权时返回 403 语义的 degraded 结构,前端显示占位徽标)。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/mesh/easytier"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/pushsource"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// meshAdminDeps 扩充 meshDeps:管理面额外需要集群运行时视图(EasyTier 状态/监督器)。
type meshAdminDeps struct {
	meshDeps
	runtime meshRuntimeView
}

// meshRuntimeView 是集群页运行时视图(由 cmd 装配的运行时循环实现;nil → 空态降级)。
type meshRuntimeView interface {
	// EasyTierStatus 返回组网状态快照(含链路表:每对端 p2p/中继、延迟、丢包)。
	EasyTierStatus(ctx context.Context) *easytier.Status
	// SupervisorSnapshot 返回托管 easytier-core 进程的运行态。
	SupervisorSnapshot() easytier.Runtime
	// ExternalMode 报告是否外部模式(运维自跑 EasyTier,只读状态)。
	ExternalMode() bool
	// Binaries 返回 easytier-core/cli 就位状态与自动下载进度。
	Binaries() easytier.BinStatus
	// TriggerDownload 手动触发一次二进制自动下载(幂等;未装配下载器时返回错误)。
	TriggerDownload() error
}

// NewMeshDeps 是装配层构造 meshDeps 的导出入口(字段未导出,经此一次性注入;
// runSub 为 SSE 订阅源,与 WithRuns 注入的相同;pushedRevs 为修订存储;advertisePort
// 为广播给对端的 HTTP 端口)。
func NewMeshDeps(
	svc mesh.Service,
	receiver *mesh.Receiver,
	client *mesh.Client,
	projects project.Service,
	runs run.Service,
	runSub runSubscriber,
	parameters run.ParameterService,
	servers target.Service,
	pipelines pipelineLister,
	aud audit.Recorder,
	pushedRevs pushsource.Store,
	advertisePort int,
) meshAdminDeps {
	return meshAdminDeps{meshDeps: meshDeps{
		mesh: svc, receiver: receiver, client: client, projects: projects, runs: runs,
		runSub: runSub, parameters: parameters, servers: servers, pipelines: pipelines,
		audit: aud, pushedRevs: pushedRevs, advertisePort: advertisePort,
	}}
}

// WithRuntime 注入集群运行时视图(集群页链路状态/监督器运行态展示;nil → 空态降级)。
func (d meshAdminDeps) WithRuntime(view meshRuntimeView) meshAdminDeps {
	d.runtime = view
	return d
}

// mountMeshAdmin 把管理面端点挂进受保护 /api 组(路径前缀 /mesh;admin 组内)。
func mountMeshAdmin(ar chi.Router, d meshAdminDeps) {
	ar.Get("/mesh/settings", makeMeshSettingsGetHandler(d.mesh))
	ar.Put("/mesh/settings", makeMeshSettingsPutHandler(d.mesh, d.audit))
	ar.Get("/mesh/acl", makeMeshACLGetHandler(d.mesh))
	ar.Put("/mesh/acl", makeMeshACLPutHandler(d.mesh, d.audit))
	ar.Get("/mesh/peers", makeMeshPeersListHandler(d.mesh))
	ar.Post("/mesh/peers", makeMeshPeersSaveHandler(d.mesh, d.audit))
	ar.Delete("/mesh/peers/{id}", makeMeshPeersDeleteHandler(d.mesh, d.audit))
	ar.Get("/mesh/overview", makeMeshOverviewHandler(d))
	ar.Get("/mesh/peers/{id}/servers", makeMeshPeerServersHandler(d))
	ar.Get("/mesh/peers/{id}/projects", makeMeshPeerProjectsHandler(d))
	ar.Get("/mesh/peers/{id}/runs", makeMeshPeerRunsHandler(d))
	// 手动触发 EasyTier 二进制自动下载(异步;进度经 overview 的 binaries 字段轮询)。
	ar.Post("/mesh/easytier/download", makeMeshEasyTierDownloadHandler(d))
	// 查看已保存的组网密钥(把新节点加入集群时需要对端输入同一密钥——密钥是管理员
	// 自创的共享密钥,非外发凭据,须可回显;每次查看留审计,同凭据 reveal 先例)。
	ar.Post("/mesh/settings/reveal-secret", makeMeshRevealSecretHandler(d))
}

// makeMeshRevealSecretHandler 返回已保存组网密钥的明文(POST + admin + CSRF + 审计)。
// 未设置时返回空串(非错误,前端据此提示先设置)。
func makeMeshRevealSecretHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		secret, err := d.mesh.NetworkSecret(r.Context())
		if err != nil {
			writeMeshError(w, err)
			return
		}
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: "admin", Action: "mesh_network_secret_reveal", TargetType: "mesh_settings", TargetID: "network_secret",
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]string{"secret": secret})
	}
}

// makeMeshEasyTierDownloadHandler 触发一次 easytier-core/cli 自动下载(镜像感知 +
// digest 校验;幂等)。写方法,过 auth + CSRF。
func makeMeshEasyTierDownloadHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.runtime == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群运行时未装配")
			return
		}
		if err := d.runtime.TriggerDownload(); err != nil {
			writeError(w, http.StatusServiceUnavailable, "internal", err.Error())
			return
		}
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: "admin", Action: "mesh_easytier_download", TargetType: "mesh_settings", TargetID: "easytier",
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
	}
}

// mountPushedSource 把「被推送源」项目的修订查询挂进主保护组(普通用户可访问,
// 与手动触发同级):GET /api/projects/{id}/pushed-revisions 供运行对话框的修订/条目选择器。
func mountPushedSource(ar chi.Router, d meshDeps) {
	ar.Get("/projects/{id}/pushed-revisions", makePushedRevisionsHandler(d))
}

// makePushedRevisionsHandler 列出 pushed 项目的修订(标签/条目清单/时间;条目名供多选)。
func makePushedRevisionsHandler(d meshDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.pushedRevs == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "推送源未初始化")
			return
		}
		revs, err := d.pushedRevs.List(r.Context(), chi.URLParam(r, "id"), 50)
		if err != nil {
			writeServerError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(revs))
		for _, rev := range revs {
			names := make([]string, 0, len(rev.Items))
			for _, it := range rev.Items {
				names = append(names, it.Name)
			}
			items = append(items, map[string]any{
				"id": rev.ID, "label": rev.Label, "items": names,
				"originNode": rev.OriginNodeID, "createdAt": rev.CreatedAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// meshSettingsDTO 是设置回显(密钥只报存在性;EasyTier 密钥绝不回显)。
type meshSettingsDTO struct {
	NodeID       string   `json:"nodeId"`
	Name         string   `json:"name"`
	Enabled      bool     `json:"enabled"`
	PublicKey    string   `json:"publicKey"`
	APIPort      int      `json:"apiPort"`
	EasyTierMode string   `json:"easytierMode"`
	EasyTierRPC  string   `json:"easytierRpc"`
	NetworkName  string   `json:"networkName"`
	VirtualIP    string   `json:"virtualIp"`
	Peers        []string `json:"peers"`
	HasSecret    bool     `json:"hasSecret"`
}

func toMeshSettingsDTO(st *mesh.Settings) meshSettingsDTO {
	return meshSettingsDTO{
		NodeID: st.NodeID, Name: st.Name, Enabled: st.Enabled, PublicKey: st.PublicKey,
		APIPort: st.APIPort, EasyTierMode: st.EasyTierMode, EasyTierRPC: st.EasyTierRPC,
		NetworkName: st.NetworkName, VirtualIP: st.VirtualIP, Peers: st.Peers,
		HasSecret: st.HasSecret,
	}
}

func makeMeshSettingsGetHandler(svc mesh.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		st, err := svc.Settings(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toMeshSettingsDTO(st))
	}
}

func makeMeshSettingsPutHandler(svc mesh.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		var body struct {
			Name          *string   `json:"name"`
			Enabled       *bool     `json:"enabled"`
			APIPort       *int      `json:"apiPort"`
			EasyTierMode  *string   `json:"easytierMode"`
			EasyTierRPC   *string   `json:"easytierRpc"`
			NetworkName   *string   `json:"networkName"`
			NetworkSecret *string   `json:"networkSecret"`
			VirtualIP     *string   `json:"virtualIp"`
			Peers         *[]string `json:"peers"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		st, err := svc.SaveSettings(r.Context(), mesh.SaveSettingsInput{
			Name: body.Name, Enabled: body.Enabled, APIPort: body.APIPort,
			EasyTierMode: body.EasyTierMode, EasyTierRPC: body.EasyTierRPC,
			NetworkName: body.NetworkName, NetworkSecret: body.NetworkSecret,
			VirtualIP: body.VirtualIP, Peers: body.Peers,
		})
		if err != nil {
			writeMeshError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: "mesh_settings_save", TargetType: "mesh_settings", TargetID: st.NodeID,
			Detail: map[string]any{"enabled": st.Enabled, "easytierMode": st.EasyTierMode},
			IP:     clientIP(r),
		})
		writeJSON(w, http.StatusOK, toMeshSettingsDTO(st))
	}
}

func makeMeshACLGetHandler(svc mesh.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		acl, err := svc.ACL(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, acl)
	}
}

func makeMeshACLPutHandler(svc mesh.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		var acl mesh.ACL
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&acl); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		if err := svc.SaveACL(r.Context(), acl); err != nil {
			writeMeshError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: "mesh_acl_save", TargetType: "mesh_acl", TargetID: "acl",
			Detail: map[string]any{
				"visibleTo": len(acl.VisibleTo), "pushAllowed": len(acl.PushAllowed),
				"triggerAllowed": len(acl.TriggerAllowed),
			},
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, acl)
	}
}

func makeMeshPeersListHandler(svc mesh.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		peers, err := svc.ListPeers(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": peers})
	}
}

func makeMeshPeersSaveHandler(svc mesh.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		var p mesh.Peer
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&p); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		if err := svc.SaveManualPeer(r.Context(), p); err != nil {
			writeMeshError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: "mesh_peer_save", TargetType: "mesh_peer", TargetID: p.NodeID,
			Detail: map[string]any{"virtualIp": p.VirtualIP}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func makeMeshPeersDeleteHandler(svc mesh.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.DeletePeer(r.Context(), id); err != nil {
			writeMeshError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: "admin", Action: "mesh_peer_delete", TargetType: "mesh_peer", TargetID: id, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// meshOverviewPeer 是集群页节点行:注册表信息 + EasyTier 链路(p2p/中继)按虚拟 IP 关联。
type meshOverviewPeer struct {
	mesh.Peer
	Online    bool    `json:"online"`   // EasyTier 链路存在且 hello 最近可达
	LinkType  string  `json:"linkType"` // p2p | relay | ""
	RelayVia  string  `json:"relayVia"` // 中继节点标识(relay 时)
	LatencyMS float64 `json:"latencyMs"`
}

// makeMeshOverviewHandler 聚合本节点视角的集群状态(集群页首屏;对端明细按需另查 peers/{id}/*)。
func makeMeshOverviewHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		st, err := d.mesh.Settings(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		peers, _ := d.mesh.ListPeers(r.Context())
		out := map[string]any{
			"settings": toMeshSettingsDTO(st),
		}
		links := map[string]easytier.Link{}
		var et easytier.Status
		if d.runtime != nil {
			out["externalMode"] = d.runtime.ExternalMode()
			out["supervisor"] = d.runtime.SupervisorSnapshot()
			out["binaries"] = d.runtime.Binaries()
			if es := d.runtime.EasyTierStatus(r.Context()); es != nil {
				et = *es
				for _, l := range es.Links {
					links[l.VirtualIP] = l
				}
			}
		} else {
			out["externalMode"] = false
		}
		et.Links = nil // 单独摘出后不再重复内嵌全表
		out["easytier"] = et
		rows := make([]meshOverviewPeer, 0, len(peers))
		for _, p := range peers {
			row := meshOverviewPeer{Peer: p}
			if l, ok := links[p.VirtualIP]; ok {
				row.Online = true
				row.LatencyMS = l.LatencyMS
				if l.Relay {
					row.LinkType = "relay"
					row.RelayVia = l.RelayVia
				} else {
					row.LinkType = "p2p"
				}
			}
			rows = append(rows, row)
		}
		out["peers"] = rows
		writeJSON(w, http.StatusOK, out)
	}
}

// proxyDegraded 是对端查询不可用/未授权时的统一降级结构(前端据此显示占位)。
type proxyDegraded struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

// writeMeshProxyResult 把对端代理查询结果写回:成功原样透传 JSON;
// ErrForbidden → 200 + degraded{forbidden}(占位而非错误,集群页继续渲染其余节点);
// ErrPeerUnreachable → 200 + degraded{unreachable}。
func writeMeshProxyResult(w http.ResponseWriter, body []byte, err error) {
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	msg := err.Error()
	if strings.Contains(msg, mesh.ErrForbidden.Error()) {
		writeJSON(w, http.StatusOK, proxyDegraded{Available: false, Reason: "forbidden"})
		return
	}
	writeJSON(w, http.StatusOK, proxyDegraded{Available: false, Reason: "unreachable"})
}

// peerFromRequest 解析 {id} 并定位对端(未知 → 404)。
func peerFromRequest(w http.ResponseWriter, r *http.Request, svc mesh.Service) *mesh.Peer {
	id := chi.URLParam(r, "id")
	p, err := svc.GetPeer(r.Context(), id)
	if err != nil {
		writeMeshError(w, err)
		return nil
	}
	return p
}

// makeMeshPeerServersHandler 代理查询对端服务器指标(集群页机器状态)。
func makeMeshPeerServersHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := peerFromRequest(w, r, d.mesh)
		if p == nil {
			return
		}
		body, err := d.client.DoRaw(r.Context(), p, http.MethodGet, "/api/mesh/servers/metrics", nil)
		writeMeshProxyResult(w, body, err)
	}
}

// makeMeshPeerProjectsHandler 代理查询对端项目清单(mesh_trigger 选择器/集群页)。
func makeMeshPeerProjectsHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := peerFromRequest(w, r, d.mesh)
		if p == nil {
			return
		}
		body, err := d.client.DoRaw(r.Context(), p, http.MethodGet, "/api/mesh/projects", nil)
		writeMeshProxyResult(w, body, err)
	}
}

// makeMeshPeerRunsHandler 代理查询对端项目运行(集群页流水线状态)。
func makeMeshPeerRunsHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := peerFromRequest(w, r, d.mesh)
		if p == nil {
			return
		}
		path := "/api/mesh/projects/" + r.URL.Query().Get("projectId") + "/runs"
		body, err := d.client.DoRaw(r.Context(), p, http.MethodGet, path, nil)
		writeMeshProxyResult(w, body, err)
	}
}
