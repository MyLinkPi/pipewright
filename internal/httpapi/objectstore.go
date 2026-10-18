package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/objectstore"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 审计 action / target(对象存储配置写操作)。detail 绝无 AccessKeySecret / 凭据明文
// (AccessKeyId 非机密,可入 detail)。
const (
	auditActionObjectStoreCreate = "objectstore.create"
	auditActionObjectStoreUpdate = "objectstore.update"
	auditActionObjectStoreDelete = "objectstore.delete"
	auditActionObjectStoreTest   = "objectstore.test"
	auditTargetObjectStore       = "object_store"
)

// objectStoreDTO 是对象存储配置对外响应体(冻结契约;camelCase)。绝不外泄 Secret:
// 仅以 credentialConfigured 布尔告知前端「是否已绑凭据」;AccessKeyId 非机密,可回显。
type objectStoreDTO struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Name                 string `json:"name"`
	Endpoint             string `json:"endpoint"`
	Bucket               string `json:"bucket"`
	PathPrefix           string `json:"pathPrefix"`
	AccessKeyID          string `json:"accessKeyId"`
	CredentialConfigured bool   `json:"credentialConfigured"`
	CreatedAt            string `json:"createdAt"`
}

// toObjectStoreDTO 把领域 Provider 转为契约 DTO(剥离 credentialId,仅暴露「是否已绑凭据」)。
func toObjectStoreDTO(p objectstore.Provider) objectStoreDTO {
	return objectStoreDTO{
		ID:                   p.ID,
		Type:                 p.Type,
		Name:                 p.Name,
		Endpoint:             p.Endpoint,
		Bucket:               p.Bucket,
		PathPrefix:           p.PathPrefix,
		AccessKeyID:          p.AccessKeyID,
		CredentialConfigured: strings.TrimSpace(p.CredentialID) != "",
		CreatedAt:            p.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// writeObjectStoreError 把对象存储领域错误映射为契约错误码/状态码;绝不回显明文/Secret。
func writeObjectStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, objectstore.ErrNotFound):
		writeError(w, http.StatusNotFound, "object_store_not_found", "对象存储配置不存在")
	case errors.Is(err, objectstore.ErrInvalidType):
		writeError(w, http.StatusBadRequest, "invalid_object_store", "对象存储类型非法(支持 aliyun_oss / rustfs)")
	case errors.Is(err, objectstore.ErrEmptyName):
		writeError(w, http.StatusBadRequest, "invalid_object_store", "请填写对象存储展示名")
	case errors.Is(err, objectstore.ErrEmptyEndpoint):
		writeError(w, http.StatusBadRequest, "invalid_object_store", "请填写 Endpoint(如 oss-cn-hangzhou.aliyuncs.com)")
	case errors.Is(err, objectstore.ErrEmptyBucket):
		writeError(w, http.StatusBadRequest, "invalid_object_store", "请填写 Bucket")
	case errors.Is(err, objectstore.ErrEmptyAccessKeyID):
		writeError(w, http.StatusBadRequest, "invalid_object_store", "请填写 AccessKeyId")
	case errors.Is(err, objectstore.ErrEmptyCredentialID):
		writeError(w, http.StatusBadRequest, "invalid_object_store", "请填写 AccessKeySecret")
	case errors.Is(err, objectstore.ErrCredentialNotFound):
		writeError(w, http.StatusUnprocessableEntity, "credential_error", "引用的凭据不存在")
	case errors.Is(err, objectstore.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法存取 AccessKeySecret")
	case errors.Is(err, objectstore.ErrTestFailed):
		writeError(w, http.StatusBadGateway, "object_store_test_failed", "对象存储连接测试失败:凭据无效、桶不存在或 Endpoint 不可达")
	case errors.Is(err, objectstore.ErrUploadFailed):
		writeError(w, http.StatusBadGateway, "object_store_upload_failed", "上传失败:请确认凭据有该桶的写权限")
	case errors.Is(err, objectstore.ErrBrowseFailed):
		writeError(w, http.StatusBadGateway, "object_store_browse_failed", "浏览失败:请确认凭据有该桶的读权限")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusBadGateway, "object_store_unreachable", "连接对象存储超时")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// makeListObjectStoresHandler 返回 GET /api/object-stores → { items: [...] }(只读,无审计)。
func makeListObjectStoresHandler(svc objectstore.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "对象存储服务未初始化")
			return
		}
		providers, err := svc.List(r.Context())
		if err != nil {
			writeObjectStoreError(w, err)
			return
		}
		out := make([]objectStoreDTO, 0, len(providers))
		for _, p := range providers {
			out = append(out, toObjectStoreDTO(p))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// makeCreateObjectStoreHandler 返回 POST /api/object-stores(认证 + CSRF)→ ObjectStore(201)。
// body: { type, name, endpoint, bucket, pathPrefix?, accessKeyId, secret }。
func makeCreateObjectStoreHandler(svc objectstore.Service, v vault.Vault, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "对象存储服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
		// secret 是「只写」字段:前端创建时提交一次明文,经 vault 加密入库换取 credentialId
		// 后即弃,绝不回库/响应/审计/日志。DB 的 object_stores 只持 credential_id 引用;
		// accessKeyId 非机密,明文列存储。
		var in struct {
			Type        string `json:"type"`
			Name        string `json:"name"`
			Endpoint    string `json:"endpoint"`
			Bucket      string `json:"bucket"`
			PathPrefix  string `json:"pathPrefix"`
			AccessKeyID string `json:"accessKeyId"`
			Secret      string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if v == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法存 AccessKeySecret")
			return
		}
		if strings.TrimSpace(in.Secret) == "" {
			writeError(w, http.StatusBadRequest, "invalid_object_store", "请填写 AccessKeySecret")
			return
		}
		// 先把 AccessKeySecret 存入 vault(加密),拿到 credentialId;DB 与领域层全程只见 credential_id。
		cred, err := v.Create(vault.CreateInput{
			Name:   "对象存储 · " + strings.TrimSpace(in.Name),
			Type:   vault.TypeObjectStoreSecret,
			Secret: in.Secret,
		})
		if err != nil {
			writeVaultError(w, err)
			return
		}
		p, err := svc.Create(r.Context(), objectstore.CreateInput{
			Type:         in.Type,
			Name:         in.Name,
			Endpoint:     in.Endpoint,
			Bucket:       in.Bucket,
			PathPrefix:   in.PathPrefix,
			AccessKeyID:  in.AccessKeyID,
			CredentialID: cred.ID,
		})
		if err != nil {
			// 登记失败 → 回滚刚存的凭据,避免悬挂(配置没建成,凭据不应留存)。
			_ = v.Delete(cred.ID)
			writeObjectStoreError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionObjectStoreCreate,
			TargetType: auditTargetObjectStore,
			TargetID:   p.ID,
			Detail:     map[string]any{"type": p.Type, "name": p.Name, "endpoint": p.Endpoint, "bucket": p.Bucket, "pathPrefix": p.PathPrefix, "accessKeyId": p.AccessKeyID}, // 绝无 AccessKeySecret
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toObjectStoreDTO(*p))
	}
}

// makeUpdateObjectStoreHandler 返回 PUT /api/object-stores/{id}(认证 + CSRF)→ ObjectStore。
// body: { name?, endpoint?, bucket?, pathPrefix?, accessKeyId?, secret? }(指针语义:缺省字段不修改;
// pathPrefix 传空串 = 清空前缀;secret 非空即原地轮换,credentialId 不变)。
func makeUpdateObjectStoreHandler(svc objectstore.Service, v vault.Vault, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "对象存储服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
		var in struct {
			Name        *string `json:"name"`
			Endpoint    *string `json:"endpoint"`
			Bucket      *string `json:"bucket"`
			PathPrefix  *string `json:"pathPrefix"`
			AccessKeyID *string `json:"accessKeyId"`
			Secret      *string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		// 含 secret 轮换时先做不落库的校验:vault 须可用、secret 须非空(否则直接拒绝,不动领域字段)。
		if in.Secret != nil {
			if v == nil {
				writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法存 AccessKeySecret")
				return
			}
			if strings.TrimSpace(*in.Secret) == "" {
				writeError(w, http.StatusBadRequest, "invalid_object_store", "AccessKeySecret 不能为空")
				return
			}
		}
		p, err := svc.Update(r.Context(), id, objectstore.UpdateInput{
			Name:        in.Name,
			Endpoint:    in.Endpoint,
			Bucket:      in.Bucket,
			PathPrefix:  in.PathPrefix,
			AccessKeyID: in.AccessKeyID,
		})
		if err != nil {
			writeObjectStoreError(w, err)
			return
		}
		// 可选轮换 Secret:vault 原地重封(credentialId 不变,引用零迁移)。
		rotated := false
		if in.Secret != nil {
			if _, err := v.Update(p.CredentialID, vault.UpdateInput{Secret: in.Secret}); err != nil {
				recordAudit(r.Context(), rec, audit.Entry{
					Actor:      auditActor,
					Action:     auditActionObjectStoreUpdate,
					TargetType: auditTargetObjectStore,
					TargetID:   p.ID,
					Detail:     map[string]any{"name": p.Name, "secretRotated": false, "partial": true},
					IP:         clientIP(r),
				})
				writeVaultError(w, err)
				return
			}
			rotated = true
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionObjectStoreUpdate,
			TargetType: auditTargetObjectStore,
			TargetID:   p.ID,
			Detail:     map[string]any{"name": p.Name, "endpoint": p.Endpoint, "bucket": p.Bucket, "pathPrefix": p.PathPrefix, "accessKeyId": p.AccessKeyID, "secretRotated": rotated}, // 绝无 AccessKeySecret
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, toObjectStoreDTO(*p))
	}
}

// makeDeleteObjectStoreHandler 返回 DELETE /api/object-stores/{id}(认证 + CSRF)→ { ok: true }。
// 删除配置后级联删除其保险库凭据(object_stores 只持 credential_id 引用,配置没了凭据即成孤儿)。
// 凭据删除为尽力而为(失败不回滚配置删除,仅记审计)。
func makeDeleteObjectStoreHandler(svc objectstore.Service, v vault.Vault, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "对象存储服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		var credentialID string
		if p, err := svc.Get(r.Context(), id); err == nil {
			credentialID = p.CredentialID
		}
		if err := svc.Delete(r.Context(), id); err != nil {
			writeObjectStoreError(w, err)
			return
		}
		credDeleted := false
		if credentialID != "" && v != nil {
			if err := v.Delete(credentialID); err == nil {
				credDeleted = true
			}
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionObjectStoreDelete,
			TargetType: auditTargetObjectStore,
			TargetID:   id,
			Detail:     map[string]any{"credentialDeleted": credDeleted}, // 绝无 Secret / 凭据明文
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// objectStoreTestDTO 是 POST /api/object-stores/{id}/test 响应体(冻结契约:ok/latencyMs/detail/error)。
type objectStoreTestDTO struct {
	OK        bool    `json:"ok"`
	LatencyMs int64   `json:"latencyMs"`
	Detail    string  `json:"detail"`
	Error     *string `json:"error"`
}

// makeTestObjectStoreHandler 返回 POST /api/object-stores/{id}/test(认证 + CSRF)
// → { ok, latencyMs, detail, error }。取该配置凭据明文 → 校验桶可访问(Secret 用完即弃,绝不外泄)。
func makeTestObjectStoreHandler(svc objectstore.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "对象存储服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		res, err := svc.Test(r.Context(), id)
		if err != nil {
			writeObjectStoreError(w, err)
			return
		}
		dto := objectStoreTestDTO{OK: res.OK, LatencyMs: res.LatencyMs, Detail: res.Detail}
		if res.Err != nil {
			// error 为人读(describeClientError 已剥离密钥,绝不含 AccessKeySecret)。
			dto.Error = &res.Detail
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor:      auditActor,
			Action:     auditActionObjectStoreTest,
			TargetType: auditTargetObjectStore,
			TargetID:   id,
			Detail:     map[string]any{"ok": res.OK}, // 绝无 Secret
			IP:         clientIP(r),
		})
		writeJSON(w, http.StatusOK, dto)
	}
}

// objectEntryDTO 是浏览列表里的一个对象条目(key 为桶内完整键,前端按当前目录截断展示)。
type objectEntryDTO struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	LastModified string `json:"lastModified"`
}

// browseObjectStoreDTO 是 GET /api/object-stores/{id}/objects 响应体(冻结契约):
// prefixes 为子目录(以 / 结尾),nextMarker 为下一页游标(空 = 最后一页)。
type browseObjectStoreDTO struct {
	Prefixes   []string         `json:"prefixes"`
	Objects    []objectEntryDTO `json:"objects"`
	NextMarker string           `json:"nextMarker"`
	Truncated  bool             `json:"truncated"`
}

// makeBrowseObjectStoreHandler 返回 GET /api/object-stores/{id}/objects?prefix=&marker=&maxKeys=
// (只读,无审计)。桶内对象可能巨多:marker 游标单页列举,绝不聚合全量。
func makeBrowseObjectStoreHandler(svc objectstore.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "对象存储服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		q := r.URL.Query()
		// maxKeys 负载防御在领域层钳制(默认 100,上限 1000);此处仅解析。
		maxKeys := atoiDefault(q.Get("maxKeys"), objectstore.DefaultBrowsePageSize)
		res, err := svc.Browse(r.Context(), id, q.Get("prefix"), q.Get("marker"), maxKeys)
		if err != nil {
			writeObjectStoreError(w, err)
			return
		}
		out := browseObjectStoreDTO{
			Prefixes:   make([]string, 0, len(res.Prefixes)),
			Objects:    make([]objectEntryDTO, 0, len(res.Objects)),
			NextMarker: res.NextMarker,
			Truncated:  res.NextMarker != "",
		}
		for _, p := range res.Prefixes {
			out.Prefixes = append(out.Prefixes, p)
		}
		for _, obj := range res.Objects {
			out.Objects = append(out.Objects, objectEntryDTO{
				Key:          obj.Key,
				Size:         obj.Size,
				LastModified: obj.LastModified.UTC().Format(time.RFC3339),
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}
