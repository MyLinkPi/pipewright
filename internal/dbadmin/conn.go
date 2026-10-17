package dbadmin

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // 注册 "pgx" 驱动(database/sql 适配)
)

// 连接/查询时限与限量(管理工具语义:短连接 + 硬上限,防失控查询拖垮对端)。
const (
	connectTimeout = 5 * time.Second
	queryTimeout   = 60 * time.Second
	importTimeout  = 10 * time.Minute
	exportTimeout  = 5 * time.Minute
	maxCellBytes   = 64 << 10  // 单格 64KB 截断(SQL 控制台/浏览结果)
	maxResultRows  = 1000      // SQL 控制台结果行上限
	maxBrowseLimit = 200       // 浏览分页单页上限
)

// connOptions 控制本次连接的形态(导入需多语句;导出/导入需更长读写超时)。
type connOpts struct {
	multiStatements bool        // MySQL multiStatements / PG simple_protocol(导入)
	readTimeout     time.Duration
	connectTimeout  time.Duration
}

// pgx 驱动名(stdlib 适配)。
const pgxDriver = "pgx"

// openEngine 按引擎开一个短连接 *sql.DB:密码经 password() 惰性解密(仅进程内),
// DSN 不落日志。database 为空 = MySQL 服务器级连接;PG 必须指定库(空则回落维护库)。
func (s *service) openEngine(ctx context.Context, engine, host string, port int, username, tlsMode, database string, password func() (string, error), opts connOpts) (*sql.DB, error) {
	pass, err := password()
	if err != nil {
		return nil, err
	}
	ct := opts.connectTimeout
	if ct == 0 {
		ct = connectTimeout
	}
	switch engine {
	case EngineMySQL:
		readTimeout := opts.readTimeout
		if readTimeout == 0 {
			readTimeout = queryTimeout
		}
		if opts.multiStatements && opts.readTimeout == 0 {
			readTimeout = importTimeout
		}
		cfg := mysql.Config{
			User: username, Passwd: pass, Net: "tcp", Addr: hostPort(host, port),
			DBName: database, ParseTime: true, Loc: time.UTC,
			Timeout: ct, ReadTimeout: readTimeout, WriteTimeout: readTimeout,
			AllowNativePasswords: true, MultiStatements: opts.multiStatements,
		}
		if tlsMode == TLSSkipVerify {
			cfg.TLSConfig = "skip-verify"
		}
		db, err := sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			return nil, humanConnectError(err)
		}
		// Ping 强制真实建连(带 ctx 超时),让凭据/网络错误在此处暴露成人话。
		pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		defer cancel()
		if err := db.PingContext(pingCtx); err != nil {
			db.Close()
			return nil, humanConnectError(err)
		}
		return db, nil
	case EnginePostgres:
		dsn := pgDSN(host, port, username, pass, database, tlsMode, opts)
		db, err := sql.Open(pgxDriver, dsn)
		if err != nil {
			return nil, humanConnectError(err)
		}
		pingCtx, cancel := context.WithTimeout(ctx, ct)
		defer cancel()
		if err := db.PingContext(pingCtx); err != nil {
			db.Close()
			return nil, humanConnectError(err)
		}
		return db, nil
	default:
		return nil, ErrInvalidEngine
	}
}

// pgDSN 组 PostgreSQL 连接串(url.UserPassword 处理密码转义;simple_protocol 允许
// 多语句导入)。database 为空回落 postgres 维护库(凭据需可访问)。
func pgDSN(host string, port int, username, password, database, tlsMode string, opts connOpts) string {
	if database == "" {
		database = "postgres"
	}
	q := url.Values{}
	q.Set("sslmode", "disable")
	if tlsMode == TLSSkipVerify {
		q.Set("sslmode", "require") // require = 加密不校验证书,对应 skip-verify 语义
	}
	q.Set("connect_timeout", "5")
	if opts.multiStatements {
		// 简单协议允许一次 Exec 携带分号分隔的多条语句(导入语义)。
		q.Set("default_query_exec_mode", "simple_protocol")
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(username, password),
		Host:     hostPort(host, port),
		Path:     "/" + strings.ReplaceAll(database, "?", "%3F"),
		RawQuery: q.Encode(),
	}
	return u.String()
}

// connect 是管理操作的统一入口:按实例解密密码 → 开短连接(database 指定目标库;
// PG 空库回落维护库)。调用方 defer db.Close()。
func (s *service) connect(ctx context.Context, inst *Instance, database string, opts connOpts) (*sql.DB, error) {
	return s.openEngine(ctx, inst.Engine, inst.Host, inst.Port, inst.Username, inst.TLS, database, func() (string, error) {
		pass, err := s.vault.OpenSecret(s.passwordSealed(ctx, inst.ID))
		if err != nil {
			return "", fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
		}
		return string(pass), nil
	}, opts)
}

// connectAnyDB 连 PG 时目标库可能不存在/不可访问:先试 postgres 维护库,再试与
// 用户名同名的库(应用市场部署的 postgres 账号同库场景),均失败则报错。
func (s *service) connectAnyDB(ctx context.Context, inst *Instance, opts connOpts) (*sql.DB, error) {
	if inst.Engine != EnginePostgres {
		return s.connect(ctx, inst, "", opts)
	}
	db, err := s.connect(ctx, inst, "postgres", opts)
	if err == nil {
		return db, nil
	}
	db2, err2 := s.connect(ctx, inst, inst.Username, opts)
	if err2 == nil {
		return db2, nil
	}
	return nil, err
}

// humanConnectError 把驱动连接错误转人话(剥 DSN/凭据;双引擎常见文案透传)。
func humanConnectError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(err.Error())
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return fmt.Errorf("%w: %s", ErrConnectFailed, msg)
}

// ---- 标识符转义与表引用 ----

// quoteIdent 转义一个标识符(MySQL 反引号 / PG 双引号;内嵌定界符双写)。拒绝空与 NUL。
func quoteIdent(engine, ident string) (string, error) {
	if ident == "" || strings.ContainsRune(ident, '\x00') {
		return "", ErrInvalidIdentifier
	}
	if engine == EngineMySQL {
		return "`" + strings.ReplaceAll(ident, "`", "``") + "`", nil
	}
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`, nil
}

// quoteTableRef 组限定表引用:MySQL 为 `db`.`table`;PG 的 table 形如 schema.name
//(首个 . 分隔,允许 schema 缺省时按裸表名)。
func quoteTableRef(engine, db, table string) (string, error) {
	if engine == EngineMySQL {
		d, err := quoteIdent(engine, db)
		if err != nil {
			return "", err
		}
		t, err := quoteIdent(engine, table)
		if err != nil {
			return "", err
		}
		return d + "." + t, nil
	}
	schema, name := "public", table
	if i := strings.IndexByte(table, '.'); i >= 0 {
		schema, name = table[:i], table[i+1:]
	}
	s, err := quoteIdent(engine, schema)
	if err != nil {
		return "", err
	}
	t, err := quoteIdent(engine, name)
	if err != nil {
		return "", err
	}
	return s + "." + t, nil
}

// validateDatabaseName 建库场景的严格标识符白名单(防奇怪字符进 DDL)。
func validateDatabaseName(name string) error {
	if name == "" || len(name) > 63 {
		return ErrInvalidIdentifier
	}
	for _, r := range name {
		ok := r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !ok {
			return ErrInvalidIdentifier
		}
	}
	return nil
}

// normalizeCell 把驱动扫出的值规整为 JSON 友好形态:[]byte→字符串(超 64KB 截断标记)、
// time→RFC3339、其余原样。[]byte 二进制内容以 hex 前缀呈现。
func normalizeCell(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		if len(t) > maxCellBytes {
			return string(t[:maxCellBytes]) + "…(截断)"
		}
		if isMostlyPrintable(t) {
			return string(t)
		}
		return "0x" + fmt.Sprintf("%x", t)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	default:
		return v
	}
}

// isMostlyPrintable 报告字节串是否大多可打印(决定字符串/hex 呈现)。
func isMostlyPrintable(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	nonPrintable := 0
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			nonPrintable++
		}
	}
	return nonPrintable*10 < len(b) // <10% 不可打印视为文本
}
