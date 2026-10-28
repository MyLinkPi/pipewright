// mesh_credentials_admin.go 是临时凭据与 Secure Mode 统一开关的管理面端点。
//
// 凭据三端点(issue/list/revoke)同时服务两类调用方,经 meshCredentialAuth 双通道:
//   - 外部系统:Authorization: Bearer pwmc_*(mesh_api_tokens 哈希查表,审计 actor
//     记 token:<名>);
//   - 浏览器:管理员会话 + CSRF(与 /api 组同款中间件,因为这三条挂在根路由——
//     chi 静态路由优先于 /api/* 子树挂载,若挂进子组会被根路由遮蔽)。
//
// 明文纪律:credential_secret 只出现在签发响应一次(绝不落库/落日志);API 令牌明文
// 只出现在创建响应一次(库内只有 SHA-256)。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/mesh"
)

// TTL 护栏:官方 CLI 无默认无上限,平台自定 —— 默认 24h、最长 90 天、下限 60s
// (下限给联调留空间,不是鼓励超短凭据)。
const (
	meshCredTTLDefault = int64(24 * 3600)
	meshCredTTLMax     = int64(90 * 24 * 3600)
	meshCredTTLMin     = int64(60)
	meshCredRemarkMax  = 200
)

// meshCredActorKey 是双通道认证注入的 actor("admin" | "token:<名>")的 context 键。
type meshCredActorKeytype int

const meshCredActorKey meshCredActorKeytype = 1

func meshCredActor(ctx context.Context) (actor, source string) {
	if v, ok := ctx.Value(meshCredActorKey).(string); ok && v != "" {
		if strings.HasPrefix(v, "token:") {
			return v, "token"
		}
		return v, "admin"
	}
	return "admin", "admin"
}

// meshCredentialAuth 是凭据端点的双通道认证:Bearer 令牌优先,浏览器走会话+CSRF+管理员。
func meshCredentialAuth(identity auth.Authenticator, meshSvc mesh.Service) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if h := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(h, "Bearer ") {
				token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
				name := ""
				if meshSvc != nil {
					name = meshSvc.ApiTokenLookup(r.Context(), token)
				}
				if name == "" || !strings.HasPrefix(token, "pwmc_") {
					writeError(w, http.StatusUnauthorized, "mesh_token_invalid", "凭据 API 令牌无效或已撤销")
					return
				}
				ctx := context.WithValue(r.Context(), meshCredActorKey, "token:"+name)
				next(w, r.WithContext(ctx))
				return
			}
			// 浏览器路径:与 /api 组同款链(requireAuth → requireCSRF → requireAdmin),
			// 通过后注入 admin actor。
			requireAuth(identity, requireCSRF(requireAdmin(http.HandlerFunc(func(rw http.ResponseWriter, rr *http.Request) {
				ctx := context.WithValue(rr.Context(), meshCredActorKey, "admin")
				next(rw, rr.WithContext(ctx))
			})))).ServeHTTP(w, r)
		}
	}
}

// mountMeshCredentialAPI 把凭据端点挂到根路由(公开区,webhook/mesh 入站旁)。
// 三条路径与 admin 组有意不重复注册(chi 静态优先会遮蔽子组同路径)。
func mountMeshCredentialAPI(r chi.Router, d meshAdminDeps, identity auth.Authenticator) {
	if d.mesh == nil {
		return
	}
	gate := meshCredentialAuth(identity, d.mesh)
	r.Post("/api/mesh/credentials", gate(makeMeshCredentialIssueHandler(d)))
	r.Get("/api/mesh/credentials", gate(makeMeshCredentialsListHandler(d)))
	r.Post("/api/mesh/credentials/{id}/revoke", gate(makeMeshCredentialRevokeHandler(d)))
}

// makeMeshCredentialIssueHandler 签发临时凭据:Secure Mode 前置 → 经本节点
// easytier-core 生成(自预生成 credential_id)→ 登记簿落库 → 明文 secret 仅本次返回。
func makeMeshCredentialIssueHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		var body struct {
			TTLSeconds int64  `json:"ttlSeconds"`
			Remark     string `json:"remark"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		if body.TTLSeconds == 0 {
			body.TTLSeconds = meshCredTTLDefault
		}
		if body.TTLSeconds < meshCredTTLMin || body.TTLSeconds > meshCredTTLMax {
			writeError(w, http.StatusUnprocessableEntity, "invalid_ttl",
				"有效期须在 60 秒 ~ 90 天之间")
			return
		}
		if len([]rune(body.Remark)) > meshCredRemarkMax {
			writeError(w, http.StatusUnprocessableEntity, "invalid_remark", "备注超过 200 字")
			return
		}
		st, err := d.mesh.Settings(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		if !st.Enabled || !st.SecureMode {
			writeError(w, http.StatusConflict, "mesh_secure_mode_off",
				"集群未启用 Secure Mode,临时凭据无法入网;请先在集群设置发起全集群统一开启")
			return
		}
		if d.runtime == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群运行时未装配")
			return
		}
		provider := d.runtime.CredentialProvider()
		if provider == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "凭据签发器未就绪(EasyTier 未运行或外部模式)")
			return
		}
		actor, source := meshCredActor(r.Context())
		credentialID := uuid.NewString()
		secret, err := provider.Generate(r.Context(), body.TTLSeconds, credentialID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "mesh_credential_generate", "签发失败:"+firstLineOf(err.Error()))
			return
		}
		rec, err := d.mesh.IssueCredentialRecord(r.Context(), credentialID, body.Remark, body.TTLSeconds, actor, source)
		if err != nil {
			// 补偿:easytier 侧凭据已签出,落库失败即回滚撤销(尽力而为;失败也无碍 ——
			// secret 已丢弃无人可用,残留凭据会以 external/active 幽灵行出现在列表且可撤销,
			// 有 TTL 自然过期兜底,但能撤则撤,避免误导运维)。
			if rerr := provider.Revoke(r.Context(), credentialID); rerr != nil {
				log.Printf("[mesh] 凭据 %s 登记失败且回滚撤销失败(将以 external 幽灵行残留至过期):%v", credentialID, rerr)
			}
			writeServerError(w, err)
			return
		}
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: actor, Action: "mesh_credential_issue", TargetType: "mesh_credential", TargetID: credentialID,
			Detail: map[string]any{"ttlSeconds": body.TTLSeconds, "remark": body.Remark, "source": source},
			IP:     clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"credentialId": rec.CredentialID,
			// credential_secret 仅此一次返回;调用方负责安全送达设备。
			"credentialSecret": secret,
			"expiresAt":        rec.ExpiresAt,
			"remark":           rec.Remark,
		})
	}
}

// meshCredentialItem 是凭据列表行(登记簿 × easytier 活跃态合并)。
type meshCredentialItem struct {
	mesh.CredentialRecord
	Status string `json:"status"` // active | expired | revoked | missing
}

// makeMeshCredentialsListHandler 列出凭据:登记簿行 + easytier 活跃表合并 ——
// 撤销以登记簿为准,活跃/过期以 easytier 为准(未到期但不在活跃表 = missing:
// 凭据文件丢失或管理节点侧被清理);活跃表里有而登记簿无的(外部签发)也一并列出。
func makeMeshCredentialsListHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		records, err := d.mesh.ListCredentialRecords(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		live := map[string]bool{}
		if d.runtime != nil {
			if provider := d.runtime.CredentialProvider(); provider != nil {
				if entries, lerr := provider.List(r.Context()); lerr == nil {
					for _, e := range entries {
						live[e.CredentialID] = true
					}
				}
			}
		}
		now := time.Now().UTC()
		seen := map[string]bool{}
		items := make([]meshCredentialItem, 0, len(records))
		for _, rec := range records {
			seen[rec.CredentialID] = true
			status := "expired"
			switch {
			case rec.RevokedAt != "":
				status = "revoked"
			case live[rec.CredentialID]:
				status = "active"
			case rec.ExpiresAt > now.Format(time.RFC3339):
				status = "missing"
			}
			items = append(items, meshCredentialItem{CredentialRecord: rec, Status: status})
		}
		// 外部签发(其它管理节点 CLI 直签,未经本平台登记):补列,备注空。
		for id := range live {
			if !seen[id] {
				items = append(items, meshCredentialItem{CredentialRecord: mesh.CredentialRecord{
					CredentialID: id, Source: "external",
				}, Status: "active"})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makeMeshCredentialRevokeHandler 撤销凭据:先撤 easytier(全网 1~3 秒踢下线),
// 再标记登记簿(未知 ID 容忍 —— 外部签发的也能撤)。
func makeMeshCredentialRevokeHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		if d.runtime == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群运行时未装配")
			return
		}
		provider := d.runtime.CredentialProvider()
		if provider == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "凭据签发器未就绪(EasyTier 未运行或外部模式)")
			return
		}
		id := chi.URLParam(r, "id")
		if err := provider.Revoke(r.Context(), id); err != nil {
			writeError(w, http.StatusBadGateway, "mesh_credential_revoke", "撤销失败:"+firstLineOf(err.Error()))
			return
		}
		if err := d.mesh.MarkCredentialRevoked(r.Context(), id); err != nil && !errors.Is(err, mesh.ErrNotFound) {
			writeServerError(w, err)
			return
		}
		actor, _ := meshCredActor(r.Context())
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: actor, Action: "mesh_credential_revoke", TargetType: "mesh_credential", TargetID: id,
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ---- API 令牌(仅管理员会话;管理面挂载) ----

func makeMeshApiTokenCreateHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		rec, plaintext, err := d.mesh.CreateApiToken(r.Context(), body.Name)
		if err != nil {
			if errors.Is(err, mesh.ErrInvalidInput) || errors.Is(err, mesh.ErrDuplicate) {
				writeError(w, http.StatusUnprocessableEntity, "invalid_token_name", firstLineOf(err.Error()))
				return
			}
			writeServerError(w, err)
			return
		}
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: "admin", Action: "mesh_api_token_create", TargetType: "mesh_api_token", TargetID: rec.Name,
			IP: clientIP(r),
		})
		// 明文仅此一次返回。
		writeJSON(w, http.StatusCreated, map[string]any{"name": rec.Name, "token": plaintext, "createdAt": rec.CreatedAt})
	}
}

func makeMeshApiTokenListHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		tokens, err := d.mesh.ListApiTokens(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": tokens})
	}
}

func makeMeshApiTokenDeleteHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		name := chi.URLParam(r, "name")
		if err := d.mesh.RevokeApiToken(r.Context(), name); err != nil {
			if errors.Is(err, mesh.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "令牌不存在或已撤销")
				return
			}
			writeServerError(w, err)
			return
		}
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: "admin", Action: "mesh_api_token_revoke", TargetType: "mesh_api_token", TargetID: name,
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ---- Secure Mode 统一开关(仅管理员会话;管理面挂载) ----

func makeMeshSecureModeSwitchHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群服务未初始化")
			return
		}
		if d.switcher == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "统一开关协调器未装配")
			return
		}
		var body struct {
			Action             string `json:"action"`
			EffectiveInMinutes int    `json:"effectiveInMinutes"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		res, err := d.switcher.Initiate(r.Context(), body.Action, body.EffectiveInMinutes)
		if err != nil {
			if errors.Is(err, mesh.ErrInvalidInput) {
				writeError(w, http.StatusUnprocessableEntity, "invalid_switch", firstLineOf(err.Error()))
				return
			}
			writeServerError(w, err)
			return
		}
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: "admin", Action: "mesh_secure_mode_switch", TargetType: "mesh_settings", TargetID: res.ChangeID,
			Detail: map[string]any{
				"action": res.Action, "armed": res.Armed,
				"effectiveAt": res.EffectiveAt.Format(time.RFC3339), "failedNodes": res.FailedNodes,
				"skippedNodes": res.SkippedNodes,
			},
			IP: clientIP(r),
		})
		if !res.Armed {
			writeError(w, http.StatusConflict, "mesh_switch_aborted",
				"有节点未确认,已回滚全部下发:"+strings.Join(res.FailedNodes, ", "))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"changeId": res.ChangeID, "action": res.Action,
			"effectiveAt": res.EffectiveAt.Format(time.RFC3339), "armed": true,
			// 无虚拟 IP 的对端无法纳入齐切,保持旧态(前端在状态区展示为「未纳入」)。
			"skippedNodes": res.SkippedNodes,
		})
	}
}

func makeMeshSecureModeAbortHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil || d.switcher == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "统一开关协调器未装配")
			return
		}
		var body struct {
			ChangeID string `json:"changeId"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		if err := d.switcher.Abort(r.Context(), body.ChangeID); err != nil {
			switch {
			case errors.Is(err, mesh.ErrNotFound):
				writeError(w, http.StatusNotFound, "not_found", "没有进行中的切换指令")
				return
			case errors.Is(err, mesh.ErrInvalidInput):
				writeError(w, http.StatusUnprocessableEntity, "invalid_abort", firstLineOf(err.Error()))
				return
			}
			writeServerError(w, err)
			return
		}
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: "admin", Action: "mesh_secure_mode_abort", TargetType: "mesh_settings", TargetID: body.ChangeID,
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func makeMeshSecureModeStatusHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.mesh == nil || d.switcher == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "统一开关协调器未装配")
			return
		}
		st, err := d.switcher.Status(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toMeshSwitchStatusDTO(st))
	}
}

// meshSwitchStatusDTO 是统一开关状态的对外形态:时刻字段一律转成「空串 = 无」的
// RFC3339 字符串。不能直接序列化 mesh.SwitchStatus —— time.Time 零值会输出
// "0001-01-01T00:00:00Z",前端按空串判「未应用/无指令」会永远失准。
type meshSwitchStatusDTO struct {
	SecureMode bool                 `json:"secureMode"`
	Pending    *meshPendingDTO      `json:"pending"`
	Nodes      []mesh.ModeNodeState `json:"nodes"`
	Skipped    []string             `json:"skipped"`
}

type meshPendingDTO struct {
	ChangeID    string `json:"changeId"`
	Action      string `json:"action"`
	EffectiveAt string `json:"effectiveAt"`
	InitiatedBy string `json:"initiatedBy"`
	AckedAt     string `json:"ackedAt"`
	AppliedAt   string `json:"appliedAt"`
}

func toMeshSwitchStatusDTO(st *mesh.SwitchStatus) meshSwitchStatusDTO {
	out := meshSwitchStatusDTO{
		SecureMode: st.SecureMode,
		Nodes:      st.Nodes,
		Skipped:    st.Skipped,
	}
	if out.Nodes == nil {
		out.Nodes = []mesh.ModeNodeState{}
	}
	if out.Skipped == nil {
		out.Skipped = []string{}
	}
	if st.Pending != nil {
		out.Pending = &meshPendingDTO{
			ChangeID:    st.Pending.ChangeID,
			Action:      st.Pending.Action,
			EffectiveAt: meshPendingTS(st.Pending.EffectiveAt),
			InitiatedBy: st.Pending.InitiatedBy,
			AckedAt:     meshPendingTS(st.Pending.AckedAt),
			AppliedAt:   meshPendingTS(st.Pending.AppliedAt),
		}
	}
	return out
}

// meshPendingTS 把时刻序列化为 RFC3339(零值 → 空串,前端据此判「未应用」)。
func meshPendingTS(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// firstLineOf 取错误文本首行截断(签发/撤销错误的 stderr 摘要友好化)。
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
