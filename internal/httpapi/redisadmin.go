// redisadmin.go 是「Redis 管理」的 HTTP 面(admin session 鉴权,挂受保护 /api 组的
// requireAdmin 子组):
//   - 实例注册(设置 → Redis):GET/POST /api/redis-instances、PUT/DELETE /{id}、
//     POST /test(未存凭据直连测试)、POST /{id}/test、PUT /{id}/readonly(解锁写模式);
//   - 管理操作:info(服务端概览)、keys(SCAN 游标分页浏览)、键详情、删键、改 TTL。
//
// 纪律:密码绝不入响应/审计/日志(列表只以 passwordConfigured 布尔告知是否已配置);
// 键名经 query 参数传输(键可含任意字符);写操作(删键/改 TTL/解锁)留审计。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/redisadmin"
)

// 审计 action(前缀 redisadmin.*);detail 绝无密码。
const (
	auditActionRedisInstanceCreate   = "redisadmin.instance.create"
	auditActionRedisInstanceUpdate   = "redisadmin.instance.update"
	auditActionRedisInstanceDelete   = "redisadmin.instance.delete"
	auditActionRedisInstanceTest     = "redisadmin.instance.test"
	auditActionRedisInstanceReadonly = "redisadmin.instance.readonly"
	// auditActionRedisInstanceRegister 供应用市场部署联动登记链路(apps.go)复用。
	auditActionRedisInstanceRegister = "redisadmin.instance.register"
	auditActionRedisKeyDelete        = "redisadmin.key.delete"
	auditActionRedisKeyTTL           = "redisadmin.key.ttl"
	auditTargetRedisInstance         = "redis_instance"
)

// mountRedisAdmin 把 Redis 管理端点挂进 admin 子组(路径前缀 /redis-instances)。
func mountRedisAdmin(ar chi.Router, svc redisadmin.Service, aud audit.Recorder) {
	// 实例注册(设置 → Redis)。
	ar.Get("/redis-instances", makeRedisInstanceListHandler(svc))
	ar.Post("/redis-instances", makeRedisInstanceCreateHandler(svc, aud))
	ar.Post("/redis-instances/test", makeRedisDirectTestHandler(svc))
	ar.Put("/redis-instances/{id}", makeRedisInstanceUpdateHandler(svc, aud))
	ar.Delete("/redis-instances/{id}", makeRedisInstanceDeleteHandler(svc, aud))
	ar.Post("/redis-instances/{id}/test", makeRedisInstanceTestHandler(svc, aud))
	ar.Put("/redis-instances/{id}/readonly", makeRedisInstanceReadonlyHandler(svc, aud))

	// 管理操作(只读 + 删键/改 TTL)。
	ar.Get("/redis-instances/{id}/info", makeRedisInfoHandler(svc))
	ar.Get("/redis-instances/{id}/keys", makeRedisScanHandler(svc))
	ar.Get("/redis-instances/{id}/keys/detail", makeRedisKeyDetailHandler(svc))
	ar.Delete("/redis-instances/{id}/keys", makeRedisDeleteKeyHandler(svc, aud))
	ar.Post("/redis-instances/{id}/keys/ttl", makeRedisSetTTLHandler(svc, aud))
}

// redisInstanceDTO 是实例对外响应体(冻结契约;camelCase)。绝无密码,仅以
// passwordConfigured 布尔告知「是否配置了密码」(无密码实例合法)。
type redisInstanceDTO struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Host               string `json:"host"`
	Port               int    `json:"port"`
	Endpoint           string `json:"endpoint"` // host:port(展示用)
	DBNum              int    `json:"dbNum"`
	ReadOnly           bool   `json:"readonly"`
	Source             string `json:"source"`
	ServerID           string `json:"serverId"`
	StackName          string `json:"stackName"`
	PasswordConfigured bool   `json:"passwordConfigured"`
	CreatedAt          string `json:"createdAt"`
}

// toRedisInstanceDTO 把领域 Instance 转为契约 DTO。
func toRedisInstanceDTO(inst redisadmin.Instance) redisInstanceDTO {
	return redisInstanceDTO{
		ID:                 inst.ID,
		Name:               inst.Name,
		Host:               inst.Host,
		Port:               inst.Port,
		Endpoint:           inst.Host + ":" + strconv.Itoa(inst.Port),
		DBNum:              inst.DBNum,
		ReadOnly:           inst.ReadOnly,
		Source:             inst.Source,
		ServerID:           inst.ServerID,
		StackName:          inst.StackName,
		PasswordConfigured: inst.PasswordConfigured,
		CreatedAt:          inst.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// writeRedisAdminError 把领域错误映射为契约错误码/状态码。
func writeRedisAdminError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, redisadmin.ErrNotFound):
		writeError(w, http.StatusNotFound, "redis_instance_not_found", "Redis 实例不存在")
	case errors.Is(err, redisadmin.ErrReadonly):
		writeError(w, http.StatusForbidden, "redis_readonly", "实例处于只读模式,请先解锁写模式")
	case errors.Is(err, redisadmin.ErrEmptyName):
		writeError(w, http.StatusBadRequest, "invalid_redis_instance", "实例名称不能为空")
	case errors.Is(err, redisadmin.ErrEmptyHost):
		writeError(w, http.StatusBadRequest, "invalid_redis_instance", "主机地址不能为空")
	case errors.Is(err, redisadmin.ErrEmptyPassword):
		writeError(w, http.StatusBadRequest, "invalid_redis_instance", "密码不能为空")
	case errors.Is(err, redisadmin.ErrInvalidPort):
		writeError(w, http.StatusBadRequest, "invalid_redis_instance", "端口须在 1-65535 之间")
	case errors.Is(err, redisadmin.ErrInvalidDB):
		writeError(w, http.StatusBadRequest, "invalid_redis_instance", "逻辑库号须在 0-15 之间")
	case errors.Is(err, redisadmin.ErrEndpointTaken):
		writeError(w, http.StatusConflict, "redis_endpoint_taken", "同地址的实例已存在")
	case errors.Is(err, redisadmin.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法存取连接密码")
	case errors.Is(err, redisadmin.ErrConnectFailed):
		writeError(w, http.StatusBadGateway, "redis_connect_failed", "连接 Redis 失败:地址不可达或密码错误")
	case errors.Is(err, redisadmin.ErrKeyNotFound):
		writeError(w, http.StatusNotFound, "redis_key_not_found", "键不存在(可能已被移除)")
	case errors.Is(err, redisadmin.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_redis_input", "入参非法")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// makeRedisInstanceListHandler 返回 GET /api/redis-instances → { items: [...] }(只读,无审计)。
func makeRedisInstanceListHandler(svc redisadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		list, err := svc.List(r.Context())
		if err != nil {
			writeRedisAdminError(w, err)
			return
		}
		out := make([]redisInstanceDTO, 0, len(list))
		for _, inst := range list {
			out = append(out, toRedisInstanceDTO(inst))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

// makeRedisInstanceCreateHandler 返回 POST /api/redis-instances(认证 + CSRF)→ Instance(201)。
// body: { name, host, port, dbNum?, password? }(密码可空 = 无密码实例)。
func makeRedisInstanceCreateHandler(svc redisadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var in struct {
			Name     string `json:"name"`
			Host     string `json:"host"`
			Port     int    `json:"port"`
			DBNum    int    `json:"dbNum"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		inst, err := svc.Create(r.Context(), redisadmin.CreateInput{
			Name: in.Name, Host: in.Host, Port: in.Port, DBNum: in.DBNum, Password: in.Password,
		})
		if err != nil {
			writeRedisAdminError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor: auditActor, Action: auditActionRedisInstanceCreate, TargetType: auditTargetRedisInstance,
			TargetID: inst.ID,
			Detail:   map[string]any{"name": inst.Name, "host": inst.Host, "port": inst.Port, "dbNum": inst.DBNum, "passwordConfigured": in.Password != ""}, // 绝无密码
			IP:       clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toRedisInstanceDTO(*inst))
	}
}

// makeRedisDirectTestHandler 返回 POST /api/redis-instances/test(未存凭据直连测试;
// 密码用完即弃,不入审计 detail)。
func makeRedisDirectTestHandler(svc redisadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var in struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			DBNum    int    `json:"dbNum"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		res, err := svc.TestDirect(r.Context(), redisadmin.ConnectInput{
			Host: in.Host, Port: in.Port, DBNum: in.DBNum, Password: in.Password,
		})
		writeRedisTestResult(w, err, res)
	}
}

// makeRedisInstanceUpdateHandler 返回 PUT /api/redis-instances/{id}(认证 + CSRF)→ Instance。
// body: { name?, host?, port?, dbNum?, password? }(指针语义;password 非 nil 即轮换,空串 = 清除)。
func makeRedisInstanceUpdateHandler(svc redisadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var in struct {
			Name     *string `json:"name"`
			Host     *string `json:"host"`
			Port     *int    `json:"port"`
			DBNum    *int    `json:"dbNum"`
			Password *string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		inst, err := svc.Update(r.Context(), id, redisadmin.UpdateInput{
			Name: in.Name, Host: in.Host, Port: in.Port, DBNum: in.DBNum, Password: in.Password,
		})
		if err != nil {
			writeRedisAdminError(w, err)
			return
		}
		rotated := in.Password != nil
		recordAudit(r.Context(), rec, audit.Entry{
			Actor: auditActor, Action: auditActionRedisInstanceUpdate, TargetType: auditTargetRedisInstance,
			TargetID: inst.ID,
			Detail:   map[string]any{"name": inst.Name, "host": inst.Host, "port": inst.Port, "dbNum": inst.DBNum, "passwordRotated": rotated}, // 绝无密码
			IP:       clientIP(r),
		})
		writeJSON(w, http.StatusOK, toRedisInstanceDTO(*inst))
	}
}

// makeRedisInstanceDeleteHandler 返回 DELETE /api/redis-instances/{id}(认证 + CSRF)→ { ok: true }。
func makeRedisInstanceDeleteHandler(svc redisadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		if err := svc.Delete(r.Context(), id); err != nil {
			writeRedisAdminError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor: auditActor, Action: auditActionRedisInstanceDelete, TargetType: auditTargetRedisInstance,
			TargetID: id, Detail: map[string]any{},
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makeRedisInstanceTestHandler 返回 POST /api/redis-instances/{id}/test → { ok, latencyMs, version, error }。
func makeRedisInstanceTestHandler(svc redisadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		res, err := svc.Test(r.Context(), id)
		if err != nil {
			writeRedisAdminError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor: auditActor, Action: auditActionRedisInstanceTest, TargetType: auditTargetRedisInstance,
			TargetID: id, Detail: map[string]any{"ok": res.OK},
			IP: clientIP(r),
		})
		writeRedisTestResult(w, nil, res)
	}
}

// writeRedisTestResult 渲染连接测试结果(Err 人话,绝不含密码)。
func writeRedisTestResult(w http.ResponseWriter, err error, res *redisadmin.TestResult) {
	if err != nil {
		writeRedisAdminError(w, err)
		return
	}
	out := struct {
		OK        bool    `json:"ok"`
		LatencyMs int64   `json:"latencyMs"`
		Version   string  `json:"version"`
		Error     *string `json:"error"`
	}{OK: res.OK, LatencyMs: res.LatencyMs, Version: res.Version}
	if res.Err != nil {
		msg := res.Err.Error()
		out.Error = &msg
	}
	writeJSON(w, http.StatusOK, out)
}

// makeRedisInstanceReadonlyHandler 返回 PUT /api/redis-instances/{id}/readonly(解锁/上锁)。
// body: { readonly: bool }(false = 解锁写模式,留审计)。
func makeRedisInstanceReadonlyHandler(svc redisadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<12)
		var in struct {
			Readonly bool `json:"readonly"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		inst, err := svc.SetReadOnly(r.Context(), id, in.Readonly)
		if err != nil {
			writeRedisAdminError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor: auditActor, Action: auditActionRedisInstanceReadonly, TargetType: auditTargetRedisInstance,
			TargetID: inst.ID, Detail: map[string]any{"readonly": inst.ReadOnly},
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toRedisInstanceDTO(*inst))
	}
}

// makeRedisInfoHandler 返回 GET /api/redis-instances/{id}/info?db=N → 服务端概览。
func makeRedisInfoHandler(svc redisadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		info, err := svc.Info(r.Context(), id, atoiDefault(r.URL.Query().Get("db"), 0))
		if err != nil {
			writeRedisAdminError(w, err)
			return
		}
		keyspace := make(map[string]int64, len(info.Keyspace))
		for db, keys := range info.Keyspace {
			keyspace[strconv.Itoa(db)] = keys
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"version":          info.Version,
			"uptimeSeconds":    info.UptimeSeconds,
			"usedMemory":       info.UsedMemory,
			"connectedClients": info.ConnectedClients,
			"dbSize":           info.DBSize,
			"keyspace":         keyspace,
		})
	}
}

// makeRedisScanHandler 返回 GET /api/redis-instances/{id}/keys?db=&cursor=&match=&count=
// → { keys, nextCursor }(SCAN 游标分页;nextCursor=0 表示遍历完成)。
func makeRedisScanHandler(svc redisadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		q := r.URL.Query()
		cursor, err := strconv.ParseUint(q.Get("cursor"), 10, 64)
		if err != nil {
			cursor = 0
		}
		res, err := svc.ScanKeys(r.Context(), id, atoiDefault(q.Get("db"), 0), redisadmin.ScanInput{
			Cursor: cursor, Match: q.Get("match"), Count: atoiDefault(q.Get("count"), 100),
		})
		if err != nil {
			writeRedisAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"keys":       res.Keys,
			"nextCursor": strconv.FormatUint(res.NextCursor, 10),
		})
	}
}

// makeRedisKeyDetailHandler 返回 GET /api/redis-instances/{id}/keys/detail?key=&db= → 键详情。
func makeRedisKeyDetailHandler(svc redisadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		detail, err := svc.KeyDetail(r.Context(), id, atoiDefault(r.URL.Query().Get("db"), 0), r.URL.Query().Get("key"))
		if err != nil {
			writeRedisAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"key":         detail.Key,
			"type":        detail.Type,
			"ttlSeconds":  detail.TTLSeconds,
			"memoryBytes": detail.MemoryBytes,
			"strValue":    detail.StrValue,
			"strLen":      detail.StrLen,
			"truncated":   detail.Truncated,
			"len":         detail.Len,
			"hashMembers": detail.HashMembers,
			"listItems":   detail.ListItems,
			"setItems":    detail.SetItems,
			"zsetItems":   detail.ZSetItems,
		})
	}
}

// makeRedisDeleteKeyHandler 返回 DELETE /api/redis-instances/{id}/keys?key=&db=(写,留审计)。
func makeRedisDeleteKeyHandler(svc redisadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		q := r.URL.Query()
		key := q.Get("key")
		dbNum := atoiDefault(q.Get("db"), 0)
		if err := svc.DeleteKey(r.Context(), id, dbNum, key); err != nil {
			writeRedisAdminError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor: auditActor, Action: auditActionRedisKeyDelete, TargetType: auditTargetRedisInstance,
			TargetID: id, Detail: map[string]any{"key": key, "db": dbNum},
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// makeRedisSetTTLHandler 返回 POST /api/redis-instances/{id}/keys/ttl(写,留审计)。
// body: { db, key, ttl }(ttl >= 0 = EXPIRE 秒;-1 = PERSIST 持久化)。
func makeRedisSetTTLHandler(svc redisadmin.Service, rec audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "Redis 管理服务未初始化")
			return
		}
		id := chi.URLParam(r, "id")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var in struct {
			DBNum int    `json:"db"`
			Key   string `json:"key"`
			TTL   int64  `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := svc.SetTTL(r.Context(), id, in.DBNum, in.Key, in.TTL); err != nil {
			writeRedisAdminError(w, err)
			return
		}
		recordAudit(r.Context(), rec, audit.Entry{
			Actor: auditActor, Action: auditActionRedisKeyTTL, TargetType: auditTargetRedisInstance,
			TargetID: id, Detail: map[string]any{"key": in.Key, "db": in.DBNum, "ttl": in.TTL},
			IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
