// mesh_dns.go 是集群 DNS 映射的管理面端点(本节点 UI 用,admin session + CSRF,挂
// /api/mesh/dns-records*;mesh_admin.go 的 mountMeshAdmin 内注册):
//   - GET/POST   /api/mesh/dns-records     :映射列表 / 新增(域名须落在所选提供商根区下);
//   - PUT/DELETE /api/mesh/dns-records/{id}:编辑(域名/启用位)/ 删除(仅本地,云端记录保留);
//   - POST       /api/mesh/dns-records/sync:手动「立即同步」——把启用条目的 A 记录写为
//     本机当前虚拟 IP(同步执行,逐条返回结果;自动跟随由运行时 Syncer 循环承担)。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/meshdns"
)

// meshDNSDeps 聚集群 DNS 映射依赖(领域服务 + 手动同步入口)。
type meshDNSDeps struct {
	svc    meshdns.Service
	syncer *meshdns.Syncer
}

// WithMeshDNS 注入集群 DNS 映射(域名 A 记录 → 本机虚拟 IP;nil svc = 端点 503 降级)。
func (d meshAdminDeps) WithMeshDNS(svc meshdns.Service, syncer *meshdns.Syncer) meshAdminDeps {
	d.meshDNS = &meshDNSDeps{svc: svc, syncer: syncer}
	return d
}

// writeMeshDNSError 把 meshdns 领域错误映射为状态码(人话错误体,绝不含凭据)。
func writeMeshDNSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, meshdns.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "映射条目不存在")
	case errors.Is(err, meshdns.ErrInvalidInput):
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", err.Error())
	case errors.Is(err, meshdns.ErrProviderNotFound):
		writeError(w, http.StatusUnprocessableEntity, "provider_not_found", "所选 DNS 提供商不存在(可能已被删除)")
	case errors.Is(err, meshdns.ErrZoneNotFound):
		writeError(w, http.StatusUnprocessableEntity, "zone_not_found", err.Error())
	case errors.Is(err, meshdns.ErrDuplicate):
		writeError(w, http.StatusConflict, "duplicate", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", "集群 DNS 映射操作失败")
	}
}

func makeMeshDnsRecordsListHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.meshDNS == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群 DNS 映射未初始化")
			return
		}
		items, err := d.meshDNS.svc.List(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		if items == nil {
			items = []meshdns.Record{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func makeMeshDnsRecordsCreateHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.meshDNS == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群 DNS 映射未初始化")
			return
		}
		var body struct {
			ProviderID string `json:"providerId"`
			FQDN       string `json:"fqdn"`
			Enabled    *bool  `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		rec, err := d.meshDNS.svc.Create(r.Context(), meshdns.CreateInput{
			ProviderID: body.ProviderID, FQDN: body.FQDN, Enabled: enabled,
		})
		if err != nil {
			writeMeshDNSError(w, err)
			return
		}
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: "admin", Action: "mesh_dns_record_save", TargetType: "mesh_dns_record", TargetID: rec.ID,
			Detail: map[string]any{"fqdn": rec.FQDN, "providerId": rec.ProviderID, "enabled": rec.Enabled},
			IP:     clientIP(r),
		})
		writeJSON(w, http.StatusOK, rec)
	}
}

func makeMeshDnsRecordsUpdateHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.meshDNS == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群 DNS 映射未初始化")
			return
		}
		var body struct {
			FQDN    *string `json:"fqdn"`
			Enabled *bool   `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		id := chi.URLParam(r, "id")
		rec, err := d.meshDNS.svc.Update(r.Context(), id, meshdns.UpdateInput{FQDN: body.FQDN, Enabled: body.Enabled})
		if err != nil {
			writeMeshDNSError(w, err)
			return
		}
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: "admin", Action: "mesh_dns_record_save", TargetType: "mesh_dns_record", TargetID: id,
			Detail: map[string]any{"fqdn": rec.FQDN, "enabled": rec.Enabled},
			IP:     clientIP(r),
		})
		writeJSON(w, http.StatusOK, rec)
	}
}

// makeMeshDnsRecordsDeleteHandler 删除映射(仅本地条目;云端 DNS 记录不动 —— UI 确认
// 框明示,管理员如需清理自行去提供商控制台)。
func makeMeshDnsRecordsDeleteHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.meshDNS == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群 DNS 映射未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := d.meshDNS.svc.Delete(r.Context(), id); err != nil {
			writeMeshDNSError(w, err)
			return
		}
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: "admin", Action: "mesh_dns_record_delete", TargetType: "mesh_dns_record", TargetID: id,
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makeMeshDnsRecordsSyncHandler 手动「立即同步」:同步执行 SyncNow(逐条 upsert A 记录为
// 本机当前虚拟 IP),返回当前虚拟 IP 与逐条结果。虚拟 IP 尚不可用(集群未启用 /
// EasyTier 未就绪)时 results 为空,前端据 ip="" 提示。
func makeMeshDnsRecordsSyncHandler(d meshAdminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.meshDNS == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "集群 DNS 映射未初始化")
			return
		}
		ip, results := d.meshDNS.syncer.SyncNow(r.Context())
		recordAudit(r.Context(), d.audit, audit.Entry{
			Actor: "admin", Action: "mesh_dns_record_sync", TargetType: "mesh_dns_record", TargetID: "all",
			Detail: map[string]any{"ip": ip, "count": len(results)},
			IP:     clientIP(r),
		})
		if results == nil {
			results = []meshdns.SyncResult{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ip": ip, "results": results})
	}
}
