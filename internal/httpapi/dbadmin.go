// dbadmin.go 是「数据库管理」的 HTTP 面(phpMyAdmin 式在线管理,admin session 鉴权,
// 挂受保护 /api 组的 requireAdmin 子组):
//   - 实例注册(设置 → 数据库):GET/POST /api/db-instances、PUT/DELETE /{id}、
//     POST /test(未存凭据直连测试)、POST /{id}/test、PUT /{id}/readonly(解锁写模式);
//   - 管理操作:info / databases(建删)/ tables / structure / rows(分页浏览+行增删改)/
//     query(SQL 控制台)/ users(账号与授权)/ export(流式 SQL/CSV)/ import(.sql)。
//
// 纪律:密码绝不入响应/审计/日志;SQL 文本绝不入审计(可能含 CREATE USER 凭据字面量),
// 审计只记目标(实例/库/表/账号)与规模(字节数);库/表名经路径段传输,一律 PathUnescape。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/dbadmin"
	"github.com/huangchengsir/pipewright/internal/dbbackup"
)

// 审计 action(前缀 dbadmin.*);detail 绝无密码与 SQL 文本。
const (
	auditActionDBInstanceCreate   = "dbadmin.instance.create"
	auditActionDBInstanceUpdate   = "dbadmin.instance.update"
	auditActionDBInstanceDelete   = "dbadmin.instance.delete"
	auditActionDBInstanceTest     = "dbadmin.instance.test"
	auditActionDBInstanceReadonly = "dbadmin.instance.readonly"
	// auditActionDBInstanceRegister 供应用市场部署联动登记链路(apps.go)复用。
	auditActionDBInstanceRegister = "dbadmin.instance.register"
	auditActionDBQuery            = "dbadmin.query"
	auditActionDBRowWrite         = "dbadmin.row.write"
	auditActionDBDatabaseCreate   = "dbadmin.database.create"
	auditActionDBDatabaseDrop     = "dbadmin.database.drop"
	auditActionDBTableAction      = "dbadmin.table.action"
	auditActionDBUserAction       = "dbadmin.user.action"
	auditActionDBImport           = "dbadmin.import"
	auditActionDBExport           = "dbadmin.export"
	auditTargetDBInstance         = "db_instance"
)

// dbImportLimit 是 .sql 导入请求体上限(与领域层 maxImportBytes 一致)。
const dbImportLimit = 32 << 20

// mountDBAdmin 把数据库管理端点挂进 admin 子组(路径前缀 /db-instances)。
func mountDBAdmin(ar chi.Router, svc dbadmin.Service, backups *dbbackup.Service, aud audit.Recorder) {
	// 实例注册(设置 → 数据库)。
	ar.Get("/db-instances", makeDBInstanceListHandler(svc))
	ar.Post("/db-instances", makeDBInstanceCreateHandler(svc, aud))
	ar.Post("/db-instances/test", makeDBDirectTestHandler(svc))
	ar.Put("/db-instances/{id}", makeDBInstanceUpdateHandler(svc, aud))
	ar.Delete("/db-instances/{id}", makeDBInstanceDeleteHandler(svc, backups, aud))
	ar.Post("/db-instances/{id}/test", makeDBInstanceTestHandler(svc, aud))
	ar.Put("/db-instances/{id}/readonly", makeDBInstanceReadonlyHandler(svc, aud))

	// 服务端信息与库管理。
	ar.Get("/db-instances/{id}/info", makeDBServerInfoHandler(svc))
	ar.Get("/db-instances/{id}/databases", makeDBListDatabasesHandler(svc))
	ar.Post("/db-instances/{id}/databases", makeDBCreateDatabaseHandler(svc, aud))
	ar.Delete("/db-instances/{id}/databases/{db}", makeDBDropDatabaseHandler(svc, aud))

	// 表与数据。
	ar.Get("/db-instances/{id}/databases/{db}/tables", makeDBListTablesHandler(svc))
	ar.Get("/db-instances/{id}/databases/{db}/tables/{table}/structure", makeDBTableStructureHandler(svc))
	ar.Get("/db-instances/{id}/databases/{db}/tables/{table}/rows", makeDBBrowseHandler(svc))
	ar.Post("/db-instances/{id}/databases/{db}/tables/{table}/rows", makeDBInsertRowHandler(svc, aud))
	ar.Put("/db-instances/{id}/databases/{db}/tables/{table}/rows", makeDBUpdateRowHandler(svc, aud))
	// 删除/单行回显走 POST 子动作(前端 http 封装 DELETE 不带 body)。
	ar.Post("/db-instances/{id}/databases/{db}/tables/{table}/rows/get", makeDBGetRowHandler(svc))
	ar.Post("/db-instances/{id}/databases/{db}/tables/{table}/rows/delete", makeDBDeleteRowHandler(svc, aud))
	ar.Post("/db-instances/{id}/databases/{db}/tables/{table}/drop", makeDBDropTableHandler(svc, aud))
	ar.Post("/db-instances/{id}/databases/{db}/tables/{table}/truncate", makeDBTruncateTableHandler(svc, aud))

	// SQL 控制台 / 导入 / 导出。
	ar.Post("/db-instances/{id}/query", makeDBQueryHandler(svc, aud))
	ar.Post("/db-instances/{id}/import", makeDBImportHandler(svc, aud))
	ar.Get("/db-instances/{id}/export", makeDBExportHandler(svc, aud))

	// 用户与授权。
	ar.Get("/db-instances/{id}/users", makeDBListUsersHandler(svc))
	ar.Post("/db-instances/{id}/users", makeDBCreateUserHandler(svc, aud))
	ar.Post("/db-instances/{id}/users/password", makeDBSetUserPasswordHandler(svc, aud))
	ar.Post("/db-instances/{id}/users/delete", makeDBDropUserHandler(svc, aud))
	ar.Get("/db-instances/{id}/users/grants", makeDBUserGrantsHandler(svc))
	ar.Post("/db-instances/{id}/users/grant", makeDBGrantHandler(svc, aud))
	ar.Post("/db-instances/{id}/users/revoke", makeDBRevokeHandler(svc, aud))
}

// writeDBAdminError 把领域错误映射为契约错误码/状态码。
func writeDBAdminError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, dbadmin.ErrNotFound):
		writeError(w, http.StatusNotFound, "db_instance_not_found", "数据库实例不存在")
	case errors.Is(err, dbadmin.ErrReadonly):
		writeError(w, http.StatusForbidden, "db_readonly", "实例处于只读模式,请先解锁写模式")
	case errors.Is(err, dbadmin.ErrInvalidEngine):
		writeError(w, http.StatusBadRequest, "invalid_db_instance", "引擎仅支持 mysql / postgres")
	case errors.Is(err, dbadmin.ErrEmptyName):
		writeError(w, http.StatusBadRequest, "invalid_db_instance", "实例名称不能为空")
	case errors.Is(err, dbadmin.ErrEmptyHost):
		writeError(w, http.StatusBadRequest, "invalid_db_instance", "主机地址不能为空")
	case errors.Is(err, dbadmin.ErrEmptyUsername):
		writeError(w, http.StatusBadRequest, "invalid_db_instance", "用户名不能为空")
	case errors.Is(err, dbadmin.ErrEmptyPassword):
		writeError(w, http.StatusBadRequest, "invalid_db_instance", "密码不能为空")
	case errors.Is(err, dbadmin.ErrInvalidPort):
		writeError(w, http.StatusBadRequest, "invalid_db_instance", "端口须在 1-65535 之间")
	case errors.Is(err, dbadmin.ErrEndpointTaken):
		writeError(w, http.StatusConflict, "db_endpoint_taken", "同引擎同地址的实例已存在")
	case errors.Is(err, dbadmin.ErrVaultUnconfigured):
		writeError(w, http.StatusServiceUnavailable, "vault_unconfigured", "保险库未配置 master key,无法保存数据库密码")
	case errors.Is(err, dbadmin.ErrConnectFailed):
		writeError(w, http.StatusBadGateway, "db_connect_failed", "连接失败:"+strings.TrimPrefix(err.Error(), dbadmin.ErrConnectFailed.Error()+": "))
	case errors.Is(err, dbadmin.ErrInvalidIdentifier):
		writeError(w, http.StatusBadRequest, "invalid_db_identifier", "库/表/列标识符非法")
	case errors.Is(err, dbadmin.ErrNoPrimaryKey):
		writeError(w, http.StatusBadRequest, "db_no_primary_key", "表没有主键,无法定位行")
	case errors.Is(err, dbadmin.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_db_input", "入参非法:"+strings.TrimPrefix(err.Error(), dbadmin.ErrInvalidInput.Error()+": "))
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// dbInstanceDTO 是实例对外视图(绝无密码/密文)。
type dbInstanceDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Engine    string `json:"engine"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Username  string `json:"username"`
	TLS       string `json:"tls"`
	Readonly  bool   `json:"readonly"`
	Source    string `json:"source"`
	ServerID  string `json:"serverId,omitempty"`
	StackName string `json:"stackName,omitempty"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

func toDBInstanceDTO(i dbadmin.Instance) dbInstanceDTO {
	return dbInstanceDTO{
		ID: i.ID, Name: i.Name, Engine: i.Engine, Host: i.Host, Port: i.Port,
		Username: i.Username, TLS: i.TLS, Readonly: i.ReadOnly, Source: i.Source,
		ServerID: i.ServerID, StackName: i.StackName,
		CreatedAt: i.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: i.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// pathParam 取路径段并做 Percent 解码(chi 保留编码形态;标识符可含中文/点)。
func pathParam(r *http.Request, key string) (string, error) {
	raw := chi.URLParam(r, key)
	if raw == "" {
		return "", nil
	}
	dec, err := url.PathUnescape(raw)
	if err != nil {
		return "", dbadmin.ErrInvalidIdentifier
	}
	return dec, nil
}

// ---- 实例注册 ----

func makeDBInstanceListHandler(svc dbadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := svc.List(r.Context())
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		items := make([]dbInstanceDTO, 0, len(list))
		for _, i := range list {
			items = append(items, toDBInstanceDTO(i))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func makeDBInstanceCreateHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Name     string `json:"name"`
		Engine   string `json:"engine"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		TLS      string `json:"tls"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		inst, err := svc.Create(r.Context(), dbadmin.CreateInput{
			Name: req.Name, Engine: req.Engine, Host: req.Host, Port: req.Port,
			Username: req.Username, Password: req.Password, TLS: req.TLS,
		})
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBInstanceCreate, TargetType: auditTargetDBInstance,
			TargetID: inst.ID, Detail: map[string]any{"engine": inst.Engine, "host": inst.Host, "port": inst.Port}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusCreated, toDBInstanceDTO(*inst))
	}
}

func makeDBInstanceUpdateHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Name     *string `json:"name"`
		Host     *string `json:"host"`
		Port     *int    `json:"port"`
		Username *string `json:"username"`
		Password *string `json:"password"`
		TLS      *string `json:"tls"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		inst, err := svc.Update(r.Context(), chi.URLParam(r, "id"), dbadmin.UpdateInput{
			Name: req.Name, Host: req.Host, Port: req.Port, Username: req.Username,
			Password: req.Password, TLS: req.TLS,
		})
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBInstanceUpdate, TargetType: auditTargetDBInstance,
			TargetID: inst.ID,
			Detail:   map[string]any{"engine": inst.Engine, "host": inst.Host, "port": inst.Port, "passwordRotated": req.Password != nil},
			IP:       clientIP(r),
		})
		writeJSON(w, http.StatusOK, toDBInstanceDTO(*inst))
	}
}

func makeDBInstanceDeleteHandler(svc dbadmin.Service, backups *dbbackup.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		inst, err := svc.Get(r.Context(), id)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		if err := svc.Delete(r.Context(), id); err != nil {
			writeDBAdminError(w, err)
			return
		}
		// 级联清理实例备份(记录 + 磁盘文件;best-effort,失败不阻断删除)。
		if backups != nil {
			if err := backups.DeleteForInstance(r.Context(), inst.Engine, id); err != nil {
				log.Printf("[dbbackup] 实例删除级联清理备份失败(%s):%v", id, err)
			}
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBInstanceDelete, TargetType: auditTargetDBInstance,
			TargetID: id, Detail: map[string]any{"engine": inst.Engine, "host": inst.Host, "port": inst.Port}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// makeDBDirectTestHandler 测未保存凭据(表单「测试连接」按钮;密码用完即弃)。
func makeDBDirectTestHandler(svc dbadmin.Service) http.HandlerFunc {
	type request struct {
		Engine   string `json:"engine"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		TLS      string `json:"tls"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		res, err := svc.TestDirect(r.Context(), dbadmin.ConnectInput{
			Engine: req.Engine, Host: req.Host, Port: req.Port,
			Username: req.Username, Password: req.Password, TLS: req.TLS,
		})
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dbTestResultDTO(res))
	}
}

func makeDBInstanceTestHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := svc.Test(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBInstanceTest, TargetType: auditTargetDBInstance,
			TargetID: chi.URLParam(r, "id"), Detail: map[string]any{"ok": res.OK}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, dbTestResultDTO(res))
	}
}

type dbTestResult struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latencyMs"`
	Version   string `json:"version"`
	Error     string `json:"error,omitempty"`
}

func dbTestResultDTO(res *dbadmin.TestResult) dbTestResult {
	out := dbTestResult{OK: res.OK, LatencyMs: res.LatencyMs, Version: res.Version}
	if res.Err != nil {
		out.Error = res.Err.Error()
	}
	return out
}

func makeDBInstanceReadonlyHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Readonly *bool `json:"readonly"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Readonly == nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		inst, err := svc.SetReadOnly(r.Context(), chi.URLParam(r, "id"), *req.Readonly)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBInstanceReadonly, TargetType: auditTargetDBInstance,
			TargetID: inst.ID, Detail: map[string]any{"readonly": inst.ReadOnly}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, toDBInstanceDTO(*inst))
	}
}

// ---- 管理操作 ----

func makeDBServerInfoHandler(svc dbadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := svc.ServerInfo(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, info)
	}
}

func makeDBListDatabasesHandler(svc dbadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := svc.ListDatabases(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": list})
	}
}

func makeDBCreateDatabaseHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Name string `json:"name"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := svc.CreateDatabase(r.Context(), chi.URLParam(r, "id"), req.Name); err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBDatabaseCreate, TargetType: auditTargetDBInstance,
			TargetID: chi.URLParam(r, "id"), Detail: map[string]any{"database": req.Name}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	}
}

func makeDBDropDatabaseHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := pathParam(r, "db")
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		if err := svc.DropDatabase(r.Context(), chi.URLParam(r, "id"), db); err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBDatabaseDrop, TargetType: auditTargetDBInstance,
			TargetID: chi.URLParam(r, "id"), Detail: map[string]any{"database": db}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeDBListTablesHandler(svc dbadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := pathParam(r, "db")
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		list, err := svc.ListTables(r.Context(), chi.URLParam(r, "id"), db)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": list})
	}
}

func makeDBTableStructureHandler(svc dbadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, table, err := dbTableParams(r)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		st, err := svc.TableStructure(r.Context(), chi.URLParam(r, "id"), db, table)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

func dbTableParams(r *http.Request) (db, table string, err error) {
	if db, err = pathParam(r, "db"); err != nil {
		return
	}
	table, err = pathParam(r, "table")
	return
}

func makeDBBrowseHandler(svc dbadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, table, err := dbTableParams(r)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		res, err := svc.Browse(r.Context(), chi.URLParam(r, "id"), db, table, dbadmin.BrowseQuery{
			Limit: limit, Offset: offset, Sort: q.Get("sort"), Dir: q.Get("dir"), Where: q.Get("where"),
		})
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

func makeDBInsertRowHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Values map[string]any `json:"values"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		db, table, err := dbTableParams(r)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		var req request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := svc.InsertRow(r.Context(), chi.URLParam(r, "id"), db, table, req.Values); err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordDBRowAudit(r, aud, chi.URLParam(r, "id"), db, table, "insert")
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	}
}

func makeDBUpdateRowHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Key    dbadmin.RowKey `json:"key"`
		Values map[string]any `json:"values"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		db, table, err := dbTableParams(r)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		var req request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := svc.UpdateRow(r.Context(), chi.URLParam(r, "id"), db, table, req.Key, req.Values); err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordDBRowAudit(r, aud, chi.URLParam(r, "id"), db, table, "update")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeDBGetRowHandler(svc dbadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, table, err := dbTableParams(r)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		var req struct {
			Key dbadmin.RowKey `json:"key"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		row, err := svc.GetRow(r.Context(), chi.URLParam(r, "id"), db, table, req.Key)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"values": row})
	}
}

func makeDBDeleteRowHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, table, err := dbTableParams(r)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		var req struct {
			Key dbadmin.RowKey `json:"key"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := svc.DeleteRow(r.Context(), chi.URLParam(r, "id"), db, table, req.Key); err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordDBRowAudit(r, aud, chi.URLParam(r, "id"), db, table, "delete")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func recordDBRowAudit(r *http.Request, aud audit.Recorder, instID, db, table, action string) {
	recordAudit(r.Context(), aud, audit.Entry{
		Actor: auditActor, Action: auditActionDBRowWrite, TargetType: auditTargetDBInstance,
		TargetID: instID, Detail: map[string]any{"database": db, "table": table, "op": action}, IP: clientIP(r),
	})
}

func makeDBDropTableHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return makeDBTableActionHandler(svc, aud, "drop", func(s dbadmin.Service, ctx context.Context, id, db, table string) error {
		return s.DropTable(ctx, id, db, table)
	})
}

func makeDBTruncateTableHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return makeDBTableActionHandler(svc, aud, "truncate", func(s dbadmin.Service, ctx context.Context, id, db, table string) error {
		return s.TruncateTable(ctx, id, db, table)
	})
}

func makeDBTableActionHandler(svc dbadmin.Service, aud audit.Recorder, action string, run func(dbadmin.Service, context.Context, string, string, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, table, err := dbTableParams(r)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		if err := run(svc, r.Context(), chi.URLParam(r, "id"), db, table); err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBTableAction, TargetType: auditTargetDBInstance,
			TargetID: chi.URLParam(r, "id"), Detail: map[string]any{"database": db, "table": table, "op": action}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ---- SQL 控制台 / 导入 / 导出 ----

func makeDBQueryHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Database  string `json:"database"`
		Statement string `json:"sql"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		res, err := svc.RunSQL(r.Context(), chi.URLParam(r, "id"), req.Database, req.Statement)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		// SQL 文本绝不入审计(可能含凭据字面量);只记目标库与语句规模。
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBQuery, TargetType: auditTargetDBInstance,
			TargetID: chi.URLParam(r, "id"),
			Detail:   map[string]any{"database": req.Database, "bytes": len(req.Statement), "isSelect": res.IsSelect},
			IP:       clientIP(r),
		})
		writeJSON(w, http.StatusOK, res)
	}
}

func makeDBImportHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Database string `json:"database"`
		SQL      string `json:"sql"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, dbImportLimit+(64<<10))
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		res, err := svc.Import(r.Context(), chi.URLParam(r, "id"), req.Database, req.SQL)
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBImport, TargetType: auditTargetDBInstance,
			TargetID: chi.URLParam(r, "id"), Detail: map[string]any{"database": req.Database, "bytes": len(req.SQL)}, IP: clientIP(r),
		})
		writeJSON(w, http.StatusOK, res)
	}
}

// makeDBExportHandler 流式导出:Content-Disposition 附件下载;流一旦开写状态码不可
// 回退,故用计数 writer——未写出任何字节时错误仍可写 JSON 错误体。
func makeDBExportHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		spec := dbadmin.ExportSpec{Database: q.Get("database"), Table: q.Get("table"), Format: q.Get("format")}
		base := sanitizeFilename(spec.Database)
		if spec.Table != "" {
			base += "_" + sanitizeFilename(spec.Table)
		}
		ct := "application/sql"
		if spec.Format == "csv" {
			ct = "text/csv"
		} else {
			spec.Format = "sql"
		}
		w.Header().Set("Content-Type", ct+"; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+base+"."+spec.Format+`"`)

		cw := &countingResponseWriter{ResponseWriter: w}
		err := svc.Export(r.Context(), chi.URLParam(r, "id"), spec, cw)
		recordAudit(r.Context(), aud, audit.Entry{
			Actor: auditActor, Action: auditActionDBExport, TargetType: auditTargetDBInstance,
			TargetID: chi.URLParam(r, "id"),
			Detail:   map[string]any{"database": spec.Database, "table": spec.Table, "format": spec.Format, "bytes": cw.n, "ok": err == nil},
			IP:       clientIP(r),
		})
		if err != nil && cw.n == 0 {
			writeDBAdminError(w, err)
			return
		}
		// 部分成功:错误已以尾注写进导出流,状态码保持 200。
	}
}

// countingResponseWriter 计已写字节(导出错误时判断能否回写 JSON 错误体)。
type countingResponseWriter struct {
	http.ResponseWriter
	n int
}

func (c *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += n
	return n, err
}

// ---- 用户与授权 ----

func makeDBListUsersHandler(svc dbadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := svc.ListUsers(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": list})
	}
}

func makeDBCreateUserHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		Password string `json:"password"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := svc.CreateUser(r.Context(), chi.URLParam(r, "id"), dbadmin.UserInput{Name: req.Name, Host: req.Host, Password: req.Password}); err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordDBUserAudit(r, aud, chi.URLParam(r, "id"), "create", req.Name, req.Host, "")
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	}
}

func makeDBSetUserPasswordHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		Password string `json:"password"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := svc.SetUserPassword(r.Context(), chi.URLParam(r, "id"), dbadmin.UserInput{Name: req.Name, Host: req.Host, Password: req.Password}); err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordDBUserAudit(r, aud, chi.URLParam(r, "id"), "password", req.Name, req.Host, "")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeDBDropUserHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
			Host string `json:"host"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := svc.DropUser(r.Context(), chi.URLParam(r, "id"), dbadmin.UserRef{Name: req.Name, Host: req.Host}); err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordDBUserAudit(r, aud, chi.URLParam(r, "id"), "delete", req.Name, req.Host, "")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeDBUserGrantsHandler(svc dbadmin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		view, err := svc.UserGrants(r.Context(), chi.URLParam(r, "id"), dbadmin.UserRef{
			Name: r.URL.Query().Get("name"), Host: r.URL.Query().Get("host"),
		})
		if err != nil {
			writeDBAdminError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}

func makeDBGrantHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		Database string `json:"database"`
		Template string `json:"template"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := svc.GrantDatabase(r.Context(), chi.URLParam(r, "id"), dbadmin.GrantInput{
			Name: req.Name, Host: req.Host, Database: req.Database, Template: req.Template,
		}); err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordDBUserAudit(r, aud, chi.URLParam(r, "id"), "grant", req.Name, req.Host, req.Database)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func makeDBRevokeHandler(svc dbadmin.Service, aud audit.Recorder) http.HandlerFunc {
	type request struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		Database string `json:"database"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if err := svc.RevokeDatabase(r.Context(), chi.URLParam(r, "id"), dbadmin.GrantInput{
			Name: req.Name, Host: req.Host, Database: req.Database,
		}); err != nil {
			writeDBAdminError(w, err)
			return
		}
		recordDBUserAudit(r, aud, chi.URLParam(r, "id"), "revoke", req.Name, req.Host, req.Database)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func recordDBUserAudit(r *http.Request, aud audit.Recorder, instID, op, name, host, database string) {
	detail := map[string]any{"user": name, "op": op}
	if host != "" {
		detail["host"] = host
	}
	if database != "" {
		detail["database"] = database
	}
	recordAudit(r.Context(), aud, audit.Entry{
		Actor: auditActor, Action: auditActionDBUserAction, TargetType: auditTargetDBInstance,
		TargetID: instID, Detail: detail, IP: clientIP(r),
	})
}
