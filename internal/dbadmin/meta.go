package dbadmin

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ServerInfo 是实例服务端概要(管理页头部)。
type ServerInfo struct {
	Engine        string `json:"engine"`
	Version       string `json:"version"`       // 短版本(如 8.4.3 / 16.4)
	VersionFull   string `json:"versionFull"`   // 完整版本串(含发行说明)
	UptimeSeconds int64  `json:"uptimeSeconds"` // 服务端运行秒数
}

// DatabaseInfo 是一个库的概要(MySQL 附表数;PG 跨库统计需逐库连接,表数留 0)。
type DatabaseInfo struct {
	Name        string `json:"name"`
	TablesCount int64  `json:"tablesCount"`
	SizeBytes   int64  `json:"sizeBytes"`
}

// TableInfo 是一张表的概要。PG 的 Name 形如 schema.name(引用时整体作为 table 参数)。
type TableInfo struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	Ref       string `json:"ref"`                 // MySQL = name;PG = schema.name
	Engine    string `json:"engine,omitempty"`    // MySQL 存储引擎
	Collation string `json:"collation,omitempty"` // MySQL 排序规则
	Rows      int64  `json:"rows"`                // 统计行数(InnoDB/reltuples 为近似值)
	SizeBytes int64  `json:"sizeBytes"`
	Comment   string `json:"comment,omitempty"`
}

// ColumnInfo 是表结构的一列。
type ColumnInfo struct {
	Name     string  `json:"name"`
	DataType string  `json:"dataType"` // MySQL column_type(含长度)/ PG format_type
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default"`
	Extra    string  `json:"extra,omitempty"` // MySQL auto_increment 等
	Key      string  `json:"key,omitempty"`   // MySQL PRI/MUL/UNI
	Comment  string  `json:"comment,omitempty"`
}

// IndexInfo 是表的一个索引。
type IndexInfo struct {
	Name       string   `json:"name"`
	Unique     bool     `json:"unique"`
	Primary    bool     `json:"primary"` // MySQL PRIMARY
	Columns    []string `json:"columns"`
	Definition string   `json:"definition,omitempty"` // PG indexdef 原文
}

// TableStructure 是表结构聚合(列 + 索引 + 主键列;主键用于行定位)。
type TableStructure struct {
	Columns     []ColumnInfo `json:"columns"`
	Indexes     []IndexInfo  `json:"indexes"`
	PrimaryKeys []string     `json:"primaryKeys"`
}

// serverVersion 取服务端版本短串(连接测试复用)。
func serverVersion(ctx context.Context, engine string, db *sql.DB) (string, error) {
	var full string
	if err := db.QueryRowContext(ctx, versionQuery(engine)).Scan(&full); err != nil {
		return "", err
	}
	return shortVersion(engine, full), nil
}

func versionQuery(engine string) string {
	if engine == EngineMySQL {
		return `SELECT VERSION()`
	}
	return `SELECT version()`
}

// shortVersion 剥出主版本号段(展示用;完整串另存 VersionFull)。
func shortVersion(engine, full string) string {
	if engine == EnginePostgres {
		// "PostgreSQL 16.4 (Debian ...)" → 16.4
		if i := strings.Index(full, " "); i >= 0 {
			rest := strings.TrimSpace(full[i+1:])
			for j := 0; j < len(rest); j++ {
				if rest[j] != '.' && (rest[j] < '0' || rest[j] > '9') {
					return rest[:j]
				}
			}
			return rest
		}
		return full
	}
	return full
}

// ServerInfo 返回实例服务端概要(版本 + 运行时长)。
func (s *service) ServerInfo(ctx context.Context, id string) (*ServerInfo, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	db, err := s.connectAnyDB(qctx, inst, connOpts{})
	if err != nil {
		return nil, err
	}
	defer db.Close()
	info := &ServerInfo{Engine: inst.Engine}
	if err := db.QueryRowContext(qctx, versionQuery(inst.Engine)).Scan(&info.VersionFull); err != nil {
		return nil, humanQueryError(err)
	}
	info.Version = shortVersion(inst.Engine, info.VersionFull)
	// 运行时长:尽力而为,失败不阻断(MySQL 需 PROCESS 权限读全局状态)。
	switch inst.Engine {
	case EngineMySQL:
		var name, val string
		if err := db.QueryRowContext(qctx, `SHOW GLOBAL STATUS LIKE 'Uptime'`).Scan(&name, &val); err == nil {
			fmt.Sscanf(val, "%d", &info.UptimeSeconds)
		}
	default:
		var secs float64
		if err := db.QueryRowContext(qctx,
			`SELECT EXTRACT(EPOCH FROM (now() - pg_postmaster_start_time()))`).Scan(&secs); err == nil {
			info.UptimeSeconds = int64(secs)
		}
	}
	return info, nil
}

// ListDatabases 返回库概要列表。
func (s *service) ListDatabases(ctx context.Context, id string) ([]DatabaseInfo, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	db, err := s.connectAnyDB(qctx, inst, connOpts{})
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if inst.Engine == EngineMySQL {
		return listDatabasesMySQL(qctx, db)
	}
	return listDatabasesPostgres(qctx, db)
}

func listDatabasesMySQL(ctx context.Context, db *sql.DB) ([]DatabaseInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.schema_name,
		       COALESCE(t.cnt, 0),
		       COALESCE(t.sz, 0)
		FROM information_schema.schemata s
		LEFT JOIN (
			SELECT table_schema, COUNT(*) AS cnt, SUM(data_length + index_length) AS sz
			FROM information_schema.tables GROUP BY table_schema
		) t ON t.table_schema = s.schema_name
		ORDER BY s.schema_name`)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]DatabaseInfo, 0)
	for rows.Next() {
		var d DatabaseInfo
		if err := rows.Scan(&d.Name, &d.TablesCount, &d.SizeBytes); err != nil {
			return nil, humanQueryError(err)
		}
		out = append(out, d)
	}
	return out, humanQueryError(rows.Err())
}

func listDatabasesPostgres(ctx context.Context, db *sql.DB) ([]DatabaseInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT d.datname, pg_database_size(d.datname)
		FROM pg_database d
		WHERE d.datallowconn AND NOT d.datistemplate
		ORDER BY d.datname`)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]DatabaseInfo, 0)
	for rows.Next() {
		var d DatabaseInfo
		if err := rows.Scan(&d.Name, &d.SizeBytes); err != nil {
			return nil, humanQueryError(err)
		}
		out = append(out, d)
	}
	return out, humanQueryError(rows.Err())
}

// ListTables 返回库内表概要。
func (s *service) ListTables(ctx context.Context, id, db string) ([]TableInfo, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := quoteIdent(inst.Engine, db); err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	conn, err := s.connect(qctx, inst, db, connOpts{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if inst.Engine == EngineMySQL {
		return listTablesMySQL(qctx, conn, db)
	}
	return listTablesPostgres(qctx, conn)
}

func listTablesMySQL(ctx context.Context, db *sql.DB, schema string) ([]TableInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT table_name, COALESCE(engine, ''), COALESCE(table_rows, 0),
		       COALESCE(data_length + index_length, 0),
		       COALESCE(table_collation, ''), COALESCE(table_comment, '')
		FROM information_schema.tables
		WHERE table_schema = ? AND table_type = 'BASE TABLE'
		ORDER BY table_name`, schema)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]TableInfo, 0)
	for rows.Next() {
		var t TableInfo
		if err := rows.Scan(&t.Name, &t.Engine, &t.Rows, &t.SizeBytes, &t.Collation, &t.Comment); err != nil {
			return nil, humanQueryError(err)
		}
		t.Ref = t.Name
		out = append(out, t)
	}
	return out, humanQueryError(rows.Err())
}

func listTablesPostgres(ctx context.Context, db *sql.DB) ([]TableInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT n.nspname, c.relname,
		       pg_total_relation_size(c.oid),
		       GREATEST(c.reltuples::bigint, 0),
		       COALESCE(obj_description(c.oid, 'pg_class'), '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg\_toast%' ESCAPE '\'
		  AND n.nspname NOT LIKE 'pg\_temp%' ESCAPE '\'
		ORDER BY n.nspname, c.relname`)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]TableInfo, 0)
	for rows.Next() {
		var t TableInfo
		if err := rows.Scan(&t.Schema, &t.Name, &t.SizeBytes, &t.Rows, &t.Comment); err != nil {
			return nil, humanQueryError(err)
		}
		t.Ref = t.Schema + "." + t.Name
		out = append(out, t)
	}
	return out, humanQueryError(rows.Err())
}

// TableStructure 返回表结构聚合(列 + 索引 + 主键列)。
func (s *service) TableStructure(ctx context.Context, id, db, table string) (*TableStructure, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := quoteTableRef(inst.Engine, db, table); err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	conn, err := s.connect(qctx, inst, db, connOpts{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if inst.Engine == EngineMySQL {
		return tableStructureMySQL(qctx, conn, db, table)
	}
	return tableStructurePostgres(qctx, conn, table)
}

func tableStructureMySQL(ctx context.Context, db *sql.DB, schema, table string) (*TableStructure, error) {
	cols, err := db.QueryContext(ctx, `
		SELECT column_name, column_type, is_nullable = 'YES', column_default, extra, column_key, column_comment
		FROM information_schema.columns
		WHERE table_schema = ? AND table_name = ?
		ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil, humanQueryError(err)
	}
	st := &TableStructure{Columns: []ColumnInfo{}, Indexes: []IndexInfo{}, PrimaryKeys: []string{}}
	defer cols.Close()
	for cols.Next() {
		var c ColumnInfo
		var def sql.NullString
		if err := cols.Scan(&c.Name, &c.DataType, &c.Nullable, &def, &c.Extra, &c.Key, &c.Comment); err != nil {
			return nil, humanQueryError(err)
		}
		if def.Valid {
			v := def.String
			c.Default = &v
		}
		st.Columns = append(st.Columns, c)
	}
	if err := cols.Err(); err != nil {
		return nil, humanQueryError(err)
	}
	cols.Close()

	// 索引:GROUP_CONCAT 按 seq_in_index 聚列;PRIMARY 名即主键。
	idxRows, err := db.QueryContext(ctx, `
		SELECT index_name, MIN(non_unique) = 0, GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',')
		FROM information_schema.statistics
		WHERE table_schema = ? AND table_name = ?
		GROUP BY index_name
		ORDER BY MIN(seq_in_index)`, schema, table)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer idxRows.Close()
	for idxRows.Next() {
		var name, colList string
		var unique bool
		if err := idxRows.Scan(&name, &unique, &colList); err != nil {
			return nil, humanQueryError(err)
		}
		names := strings.Split(colList, ",")
		if name == "PRIMARY" {
			st.PrimaryKeys = names
		}
		st.Indexes = append(st.Indexes, IndexInfo{Name: name, Unique: unique, Primary: name == "PRIMARY", Columns: names})
	}
	if err := idxRows.Err(); err != nil {
		return nil, humanQueryError(err)
	}
	return st, nil
}

func tableStructurePostgres(ctx context.Context, db *sql.DB, table string) (*TableStructure, error) {
	schema, name := "public", table
	if i := strings.IndexByte(table, '.'); i >= 0 {
		schema, name = table[:i], table[i+1:]
	}
	ref, err := quoteTableRef(EnginePostgres, "", table)
	if err != nil {
		return nil, err
	}
	st := &TableStructure{Columns: []ColumnInfo{}, Indexes: []IndexInfo{}, PrimaryKeys: []string{}}

	cols, err := db.QueryContext(ctx, `
		SELECT a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull,
		       pg_get_expr(ad.adbin, ad.adrelid),
		       '',
		       '',
		       COALESCE(col_description(a.attrelid, a.attnum), '')
		FROM pg_attribute a
		LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, ref)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer cols.Close()
	for cols.Next() {
		var c ColumnInfo
		var def sql.NullString
		if err := cols.Scan(&c.Name, &c.DataType, &c.Nullable, &def, &c.Extra, &c.Key, &c.Comment); err != nil {
			return nil, humanQueryError(err)
		}
		if def.Valid {
			v := def.String
			c.Default = &v
		}
		st.Columns = append(st.Columns, c)
	}
	if err := cols.Err(); err != nil {
		return nil, humanQueryError(err)
	}
	cols.Close()

	idx, err := db.QueryContext(ctx, `
		SELECT i.relname, ix.indisunique, ix.indisprimary,
		       COALESCE(pg_get_indexdef(ix.indexrelid), '')
		FROM pg_class c
		JOIN pg_index ix ON ix.indrelid = c.oid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2
		ORDER BY ix.indisprimary DESC, i.relname`, schema, name)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer idx.Close()
	for idx.Next() {
		var info IndexInfo
		var def sql.NullString
		if err := idx.Scan(&info.Name, &info.Unique, &info.Primary, &def); err != nil {
			return nil, humanQueryError(err)
		}
		if def.Valid {
			info.Definition = def.String
		}
		st.Indexes = append(st.Indexes, info)
	}
	if err := idx.Err(); err != nil {
		return nil, humanQueryError(err)
	}
	idx.Close()

	pk, err := db.QueryContext(ctx, `
		SELECT a.attname
		FROM pg_index ix
		JOIN LATERAL unnest(ix.indkey) WITH ORDINALITY k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = ix.indrelid AND a.attnum = k.attnum
		WHERE ix.indrelid = $1::regclass AND ix.indisprimary
		ORDER BY k.ord`, ref)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer pk.Close()
	for pk.Next() {
		var n string
		if err := pk.Scan(&n); err != nil {
			return nil, humanQueryError(err)
		}
		st.PrimaryKeys = append(st.PrimaryKeys, n)
	}
	if err := pk.Err(); err != nil {
		return nil, humanQueryError(err)
	}
	return st, nil
}

// humanQueryError 统一查询错误出口:透传驱动人话(库/表不存在、语法错误等),
// 截断超长文本;绝不含 DSN/密码(连接错误已在 openEngine 处理)。
func humanQueryError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	return fmt.Errorf("dbadmin: %s", msg)
}
