// dump_sql.go:协议路径 dump。MySQL 走 internal/backup 的全库 dumper(导出面);
// PostgreSQL 外部实例走本文件的子集 dump(pg_catalog 重建 DDL + 批量 INSERT)。
//
// PG 子集 dump 的保真边界(头部注释同样注明):表(列/NOT NULL/默认值)/PK/UNIQUE 索引/
// 其余索引/外键/视图/数据;不含函数/触发器/扩展/分区细节。格式约定:每库一段
// `-- PWDB: <name>`,DDL 用 PWSTMT-ATOM 标记包裹(与 backup 包一致),回放经
// backup.ReplayChunkedSQL(逐库连接回放,见 restore.go)。
package dbbackup

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/huangchengsir/pipewright/internal/backup"
)

// dumpMySQLProtocol 全库协议 dump(连接:服务器级,由 dbadmin.ConnectBackup 提供)。
func (s *Service) dumpMySQLProtocol(ctx context.Context, ref *instanceRef, w io.Writer) error {
	db, err := s.dba.ConnectBackup(ctx, ref.ID, "")
	if err != nil {
		return fmt.Errorf("连接实例失败: %w", err)
	}
	defer func() { _ = db.Close() }()
	return backup.DumpMySQLAll(ctx, db, w)
}

// ── PostgreSQL 协议子集 dump ──────────────────────────────────────────────────

// pgRows 抽象 sql.Rows 最小面(测试可注入桩)。
type pgRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// pgQueryer 抽象「按查询取行」(生产包 *sql.DB;测试可注入桩)。
type pgQueryer interface {
	Query(ctx context.Context, query string, args ...any) (pgRows, error)
}

// dbQueryer 适配 *sql.DB → pgQueryer。
type dbQueryer struct{ db *sql.DB }

func (d dbQueryer) Query(ctx context.Context, query string, args ...any) (pgRows, error) {
	return d.db.QueryContext(ctx, query, args...)
}

// pgColumn 是列元数据(渲染 DDL 的纯输入,便于单测)。
type pgColumn struct {
	Name    string
	Type    string // format_type 产物(如 integer, varchar(64))
	NotNull bool
	HasDef  bool
	Default string // 列默认值表达式(pg_get_expr 求值产物)
}

// pgFK 是一条外键声明(表全部建完后统一输出)。
type pgFK struct {
	Table, Name, Def string
}

// dumpPostgresProtocol 外部 PG 实例的子集 dump:维护库连接枚举数据库 → 逐库连接 dump。
func (s *Service) dumpPostgresProtocol(ctx context.Context, ref *instanceRef, w io.Writer) error {
	maint, err := s.dba.ConnectBackup(ctx, ref.ID, "")
	if err != nil {
		return fmt.Errorf("连接实例失败: %w", err)
	}
	defer func() { _ = maint.Close() }()
	q := dbQueryer{db: maint}

	var databases []string
	rows, err := q.Query(ctx,
		`SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname`)
	if err != nil {
		return fmt.Errorf("枚举数据库失败: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		databases = append(databases, name)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := fmt.Fprint(w, "-- Pipewright PostgreSQL protocol backup (subset)\n"+
		"-- 包含:表(列/NOT NULL/默认值)/主键/唯一/外键/索引/视图/数据\n"+
		"-- 不含:函数/触发器/扩展/分区细节;需要全保真请将实例部署于纳管主机\n\n"); err != nil {
		return err
	}
	for _, dbname := range databases {
		db, err := s.dba.ConnectBackup(ctx, ref.ID, dbname)
		if err != nil {
			return fmt.Errorf("连接数据库 %s 失败: %w", dbname, err)
		}
		err = dumpPostgresDatabase(ctx, dbQueryer{db: db}, w)
		_ = db.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// dumpPostgresDatabase dump 单个 PG 库(连接已绑定该库)。
func dumpPostgresDatabase(ctx context.Context, q pgQueryer, w io.Writer) error {
	d := &pgDumper{q: q, w: w}
	if err := d.dumpTables(ctx); err != nil {
		return err
	}
	// 外键统一段(全部表已建,任意建表顺序可回放)。
	for _, fk := range d.fks {
		if err := d.printf("-- PWSTMT-ATOM-START\nALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;\nALTER TABLE %s ADD CONSTRAINT %s %s;\n-- PWSTMT-ATOM-END\n\n",
			fk.Table, pgQuoteIdent(fk.Name), fk.Table, pgQuoteIdent(fk.Name), fk.Def); err != nil {
			return err
		}
	}
	return d.dumpViews(ctx)
}

type pgDumper struct {
	q   pgQueryer
	w   io.Writer
	fks []pgFK
}

func (d *pgDumper) printf(format string, args ...any) error {
	_, err := fmt.Fprintf(d.w, format, args...)
	return err
}

// dumpTables 基表(普通+分区母表)按 schema.table 字典序。
func (d *pgDumper) dumpTables(ctx context.Context) error {
	rows, err := d.q.Query(ctx,
		`SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.relkind IN ('r','p') AND n.nspname NOT IN ('pg_catalog','information_schema') AND c.relname NOT LIKE 'pg\_%'
		 ORDER BY n.nspname, c.relname`)
	if err != nil {
		return fmt.Errorf("枚举表失败: %w", err)
	}
	type tbl struct{ schema, name string }
	var tables []tbl
	for rows.Next() {
		var t tbl
		if err := rows.Scan(&t.schema, &t.name); err != nil {
			_ = rows.Close()
			return err
		}
		tables = append(tables, t)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range tables {
		if err := d.dumpOneTable(ctx, t.schema, t.name); err != nil {
			return err
		}
	}
	return nil
}

// dumpOneTable 单表:CREATE TABLE → 索引(含 PK/UNIQUE,pg_get_indexdef)→ 外键登记 → 数据。
func (d *pgDumper) dumpOneTable(ctx context.Context, schema, table string) error {
	qn := pgQuoteIdent(schema) + "." + pgQuoteIdent(table)

	cols, err := d.tableColumns(ctx, schema, table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	colDefs := make([]string, len(cols))
	for i, c := range cols {
		colDefs[i] = renderPGColumn(c)
	}
	if err := d.printf("-- PWSTMT-ATOM-START\nDROP TABLE IF EXISTS %s;\nCREATE TABLE %s (\n  %s\n);\n-- PWSTMT-ATOM-END\n\n",
		qn, qn, strings.Join(colDefs, ",\n  ")); err != nil {
		return err
	}
	if err := d.dumpIndexes(ctx, schema, table); err != nil {
		return err
	}
	if err := d.collectForeignKeys(ctx, schema, table, qn); err != nil {
		return err
	}
	return d.dumpTableData(ctx, qn, cols)
}

// tableColumns 取列元数据(默认值经 pg_get_expr 求值)。
func (d *pgDumper) tableColumns(ctx context.Context, schema, table string) ([]pgColumn, error) {
	rows, err := d.q.Query(ctx,
		`SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
			pg_get_expr(def.adbin, def.adrelid)
		 FROM pg_attribute a
		 JOIN pg_class c ON c.oid = a.attrelid
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 LEFT JOIN pg_attrdef def ON def.adrelid = a.attrelid AND def.adnum = a.attnum
		 WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		 ORDER BY a.attnum`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("枚举列失败(%s.%s): %w", schema, table, err)
	}
	var cols []pgColumn
	for rows.Next() {
		var c pgColumn
		var def sql.NullString
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &def); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.HasDef, c.Default = def.Valid, def.String
		cols = append(cols, c)
	}
	_ = rows.Close()
	return cols, rows.Err()
}

// dumpIndexes 输出全部索引(PG 的 PK/UNIQUE 均为索引,pg_get_indexdef 完整可信)。
func (d *pgDumper) dumpIndexes(ctx context.Context, schema, table string) error {
	rows, err := d.q.Query(ctx,
		`SELECT i.relname, pg_get_indexdef(i.oid) FROM pg_index x
		 JOIN pg_class c ON c.oid = x.indrelid
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 JOIN pg_class i ON i.oid = x.indexrelid
		 WHERE n.nspname = $1 AND c.relname = $2
		 ORDER BY i.relname`, schema, table)
	if err != nil {
		return fmt.Errorf("枚举索引失败(%s): %w", table, err)
	}
	type idx struct{ name, ddl string }
	var list []idx
	for rows.Next() {
		var e idx
		if err := rows.Scan(&e.name, &e.ddl); err != nil {
			_ = rows.Close()
			return err
		}
		list = append(list, e)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range list {
		// DROP IF EXISTS 保回放幂等(CREATE INDEX 的 IF NOT EXISTS 在老版本缺位)。
		if err := d.printf("-- PWSTMT-ATOM-START\nDROP INDEX IF EXISTS %s;\n%s;\n-- PWSTMT-ATOM-END\n\n",
			pgQuoteIdent(e.name), e.ddl); err != nil {
			return err
		}
	}
	return nil
}

// collectForeignKeys 登记外键(表全部建完后统一输出,保证任意建表顺序可回放)。
func (d *pgDumper) collectForeignKeys(ctx context.Context, schema, table, qn string) error {
	rows, err := d.q.Query(ctx,
		`SELECT con.conname, pg_get_constraintdef(con.oid) FROM pg_constraint con
		 JOIN pg_class c ON c.oid = con.conrelid
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE con.contype = 'f' AND n.nspname = $1 AND c.relname = $2 ORDER BY con.conname`, schema, table)
	if err != nil {
		return fmt.Errorf("枚举外键失败(%s): %w", table, err)
	}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			_ = rows.Close()
			return err
		}
		d.fks = append(d.fks, pgFK{Table: qn, Name: name, Def: def})
	}
	_ = rows.Close()
	return rows.Err()
}

// dumpViews 视图(基表与外键之后):CREATE OR REPLACE VIEW(pg_get_viewdef)。
func (d *pgDumper) dumpViews(ctx context.Context) error {
	rows, err := d.q.Query(ctx,
		`SELECT n.nspname, c.relname, pg_get_viewdef(c.oid) FROM pg_class c
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.relkind = 'v' AND n.nspname NOT IN ('pg_catalog','information_schema')
		 ORDER BY n.nspname, c.relname`)
	if err != nil {
		return fmt.Errorf("枚举视图失败: %w", err)
	}
	type view struct{ schema, name, def string }
	var views []view
	for rows.Next() {
		var v view
		if err := rows.Scan(&v.schema, &v.name, &v.def); err != nil {
			_ = rows.Close()
			return err
		}
		views = append(views, v)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, v := range views {
		if err := d.printf("-- PWSTMT-ATOM-START\nCREATE OR REPLACE VIEW %s.%s AS %s;\n-- PWSTMT-ATOM-END\n\n",
			pgQuoteIdent(v.schema), pgQuoteIdent(v.name), strings.TrimSpace(v.def)); err != nil {
			return err
		}
	}
	return nil
}

// dumpTableData 分批 INSERT(列序 = attnum)。
func (d *pgDumper) dumpTableData(ctx context.Context, qn string, cols []pgColumn) error {
	colList := make([]string, len(cols))
	for i, c := range cols {
		colList[i] = pgQuoteIdent(c.Name)
	}
	rows, err := d.q.Query(ctx, "SELECT "+strings.Join(colList, ",")+" FROM "+qn)
	if err != nil {
		return fmt.Errorf("读数据失败(%s): %w", qn, err)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var batch []string
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := d.printf("INSERT INTO %s (%s) VALUES\n%s;\n", qn, strings.Join(colList, ","), strings.Join(batch, ",\n")); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			_ = rows.Close()
			return err
		}
		parts := make([]string, len(vals))
		for i := range vals {
			parts[i] = renderPGValue(vals[i])
		}
		batch = append(batch, "("+strings.Join(parts, ",")+")")
		if len(batch) >= 200 {
			if err := flush(); err != nil {
				_ = rows.Close()
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	return flush()
}

// renderPGColumn 渲染单列定义(纯函数,单测覆盖)。
func renderPGColumn(c pgColumn) string {
	def := pgQuoteIdent(c.Name) + " " + c.Type
	if c.HasDef && c.Default != "" {
		def += " DEFAULT " + c.Default
	}
	if c.NotNull {
		def += " NOT NULL"
	}
	return def
}

// renderPGValue 把扫描值渲染为 PG 字面量。
func renderPGValue(v any) string {
	switch val := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		// PG 无 X'' 十六进制字面量;字节串按 E'\x..' 十六进制转义(不依赖编码)。
		var b strings.Builder
		b.WriteString("E'\\x")
		for _, by := range val {
			b.WriteString(fmt.Sprintf("%02x", by))
		}
		b.WriteByte('\'')
		return b.String()
	case bool:
		if val {
			return "true"
		}
		return "false"
	case string:
		return pgQuoteString(val)
	case float64:
		return strconv.FormatFloat(val, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// pgQuoteString 渲染带引号的 PG 字符串字面量(标准 SQL 转义:单引号双写——
// simple_protocol 回放下不吃反斜杠转义,双写最稳)。
func pgQuoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// pgQuoteIdent 双引号引用 PG 标识符(内部双引号双写)。
func pgQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
