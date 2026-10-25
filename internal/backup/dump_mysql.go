// dump_mysql.go:MySQL 纯 Go SQL dump(备份侧)与分块回放(恢复侧)。
// 不依赖 mysqldump 二进制(容器/瘦控制机不可假设其存在):
// information_schema 枚举 → SHOW CREATE / catalog 重建 DDL → 分批 INSERT。
// 回放侧开着 FOREIGN_KEY_CHECKS=0 吃任意建表顺序;连接须 multiStatements=true。
//
// 导出面(供 internal/dbbackup 做实例级备份;平台自身备份走 Service.dumpMySQL):
//   - DumpMySQLAll:服务器级连接上 dump 全部可 dump 库(含 CREATE DATABASE/USE 段)
//   - ReplayChunkedSQL:按 PWSTMT-ATOM 标记感知的分块回放(引擎无关,PG 子集 dump 同用)
package backup

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"
)

// dumpMySQL 把平台库全量 dump 为 SQL 文本(DDL + 数据 + 触发器)。
func (s *Service) dumpMySQL(ctx context.Context, w io.Writer) error {
	d := &mysqlDumper{db: s.db, w: w}
	schema, err := d.currentSchema(ctx)
	if err != nil {
		return err
	}
	return d.dumpWhole(ctx, schema)
}

// DumpMySQLAll 在服务器级连接(未选库)上 dump 全部可 dump 的库:每库一段
// `-- PWDB:` 标记 + CREATE DATABASE IF NOT EXISTS + USE,库内容与平台库同构。
// 跳过 information_schema/performance_schema/sys(不可 dump 的系统库);包含 mysql。
// 供 internal/dbbackup 做 MySQL 实例备份(连接由 dbadmin.ConnectBackup 提供)。
func DumpMySQLAll(ctx context.Context, db *sql.DB, w io.Writer) error {
	d := &mysqlDumper{db: db, w: w}
	if err := d.printf("SET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS=0;\nSET UNIQUE_CHECKS=0;\n\n"); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('information_schema','performance_schema','sys') ORDER BY schema_name`)
	if err != nil {
		return fmt.Errorf("backup: list schemata: %w", err)
	}
	var schemas []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			_ = rows.Close()
			return err
		}
		schemas = append(schemas, s)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, s := range schemas {
		var name, ddl string
		if err := db.QueryRowContext(ctx, "SHOW CREATE DATABASE "+quoteMySQLIdent(s)).Scan(&name, &ddl); err != nil {
			return fmt.Errorf("backup: show create database %s: %w", s, err)
		}
		// SHOW CREATE 产物是 "CREATE DATABASE `x` ...";插入 IF NOT EXISTS 保回放幂等。
		ddl = strings.Replace(ddl, "CREATE DATABASE ", "CREATE DATABASE IF NOT EXISTS ", 1)
		if err := d.printf(atomStart+"\n%s;\nUSE %s;\n"+atomEnd+"\n\n", ddl, quoteMySQLIdent(s)); err != nil {
			return err
		}
		if err := d.dumpSchema(ctx, s); err != nil {
			return err
		}
	}
	return d.printf("\nSET FOREIGN_KEY_CHECKS=1;\nSET UNIQUE_CHECKS=1;\n")
}

type mysqlDumper struct {
	db *sql.DB
	w  io.Writer
}

func (d *mysqlDumper) printf(format string, args ...any) error {
	_, err := fmt.Fprintf(d.w, format, args...)
	return err
}

// currentSchema 取连接当前库名(平台备份的连接带 DBName)。
func (d *mysqlDumper) currentSchema(ctx context.Context) (string, error) {
	var schema sql.NullString
	if err := d.db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil || !schema.Valid {
		return "", fmt.Errorf("backup: connection has no current database: %v", err)
	}
	return schema.String, nil
}

// dumpWhole 平台自身备份:单库全量(表/视图/触发器/例程)。
func (d *mysqlDumper) dumpWhole(ctx context.Context, schema string) error {
	if err := d.printf("SET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS=0;\nSET UNIQUE_CHECKS=0;\n\n"); err != nil {
		return err
	}
	if err := d.dumpSchema(ctx, schema); err != nil {
		return err
	}
	return d.printf("\nSET FOREIGN_KEY_CHECKS=1;\nSET UNIQUE_CHECKS=1;\n")
}

// dumpSchema 输出单个库:基表(名字序)→ 视图 → 触发器 → 存储过程/函数。
func (d *mysqlDumper) dumpSchema(ctx context.Context, schema string) error {
	tables, views, err := d.listTables(ctx, schema)
	if err != nil {
		return err
	}
	// 基表按名字序建(外键约束关闭,顺序无碍);视图依赖基表,统一放最后。
	sorted := append([]string(nil), tables...)
	sort.Strings(sorted)
	for _, t := range sorted {
		if err := d.dumpTable(ctx, schema, t); err != nil {
			return err
		}
	}
	viewSorted := append([]string(nil), views...)
	sort.Strings(viewSorted)
	for _, v := range viewSorted {
		if err := d.dumpView(ctx, schema, v); err != nil {
			return err
		}
	}
	if err := d.dumpTriggers(ctx, schema); err != nil {
		return err
	}
	return d.dumpRoutines(ctx, schema)
}

// listTables 返回指定库的基表与视图。
func (d *mysqlDumper) listTables(ctx context.Context, schema string) (tables, views []string, err error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT table_name, table_type FROM information_schema.tables WHERE table_schema = ?`, schema)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, nil, err
		}
		if typ == "VIEW" {
			views = append(views, name)
		} else {
			tables = append(tables, name)
		}
	}
	return tables, views, rows.Err()
}

// quoteMySQLIdent 反引号引用标识符(内部反引号双写)。
func quoteMySQLIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// atomStart/atomEnd 是回放器的语句原子边界标记:两条标记之间的内容属于同一条
// 服务端生成 DDL(触发器/过程体内含分号,不能按行尾分号切分语句)。
// ReplayChunkedSQL 按此标记切分;PG 子集 dump(internal/dbbackup)沿用同一约定。
const (
	atomStart = "-- PWSTMT-ATOM-START"
	atomEnd   = "-- PWSTMT-ATOM-END"
)

// qualifiedRef 输出 `schema`.`obj` 限定名(SHOW CREATE / SELECT 均可跨库使用)。
func qualifiedRef(schema, obj string) string {
	return quoteMySQLIdent(schema) + "." + quoteMySQLIdent(obj)
}

// dumpTable 输出单表:DROP + CREATE + 数据 INSERT。DDL 用 PWSTMT-ATOM 标记包裹
// (回放器据此切分语句块:服务端生成的 DDL 内部可能含分号,不能按行尾分号切)。
func (d *mysqlDumper) dumpTable(ctx context.Context, schema, table string) error {
	ref := qualifiedRef(schema, table)
	var name, ddl string
	if err := d.db.QueryRowContext(ctx, "SHOW CREATE TABLE "+ref).Scan(&name, &ddl); err != nil {
		return fmt.Errorf("backup: show create %s: %w", table, err)
	}
	if err := d.printf(atomStart+"\nDROP TABLE IF EXISTS %s;\n%s;\n"+atomEnd+"\n\n", ref, ddl); err != nil {
		return err
	}

	cols, binFlags, err := d.tableColumns(ctx, schema, table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	colRefs := make([]string, len(cols))
	for i, c := range cols {
		colRefs[i] = quoteMySQLIdent(c)
	}
	colList := strings.Join(colRefs, ",")

	rows, err := d.db.QueryContext(ctx, "SELECT "+colList+" FROM "+ref)
	if err != nil {
		return fmt.Errorf("backup: select %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	var batch []string
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := d.printf("INSERT INTO %s (%s) VALUES\n%s;\n", ref, colList, strings.Join(batch, ",\n")); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("backup: scan %s: %w", table, err)
		}
		parts := make([]string, len(cols))
		for i := range values {
			parts[i] = formatMySQLValue(values[i], binFlags[i])
		}
		batch = append(batch, "("+strings.Join(parts, ",")+")")
		if len(batch) >= 200 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	return d.printf("\n")
}

// tableColumns 返回列名与「是否二进制列」标记(二进制值须用 X'..' 十六进制字面量)。
func (d *mysqlDumper) tableColumns(ctx context.Context, schema, table string) ([]string, []bool, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: columns %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	var bins []bool
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			return nil, nil, err
		}
		cols = append(cols, name)
		bins = append(bins, dataType == "binary" || dataType == "varbinary" ||
			dataType == "blob" || dataType == "tinyblob" || dataType == "mediumblob" || dataType == "longblob")
	}
	return cols, bins, rows.Err()
}

// dumpView 输出单视图:DROP VIEW + CREATE(SHOW CREATE TABLE 对视图同样适用)。
func (d *mysqlDumper) dumpView(ctx context.Context, schema, view string) error {
	ref := qualifiedRef(schema, view)
	var name, ddl string
	if err := d.db.QueryRowContext(ctx, "SHOW CREATE TABLE "+ref).Scan(&name, &ddl); err != nil {
		return fmt.Errorf("backup: show create view %s: %w", view, err)
	}
	if err := d.printf(atomStart+"\nDROP VIEW IF EXISTS %s;\n%s;\n"+atomEnd+"\n\n", ref, ddl); err != nil {
		return err
	}
	return nil
}

// mysqlTriggerRow 是触发器 catalog 行(用于完整重建 CREATE TRIGGER)。
type mysqlTriggerRow struct {
	Name, Event, Table, Timing, Statement string
}

// dumpTriggers 输出指定库全部触发器。用 information_schema 重建完整 CREATE 语句
// (SHOW CREATE TRIGGER 的 DDL 列位随版本漂移且带 DEFINER——重建产物无 DEFINER,
// 回放到任何账号都成立)。
func (d *mysqlDumper) dumpTriggers(ctx context.Context, schema string) error {
	rows, err := d.db.QueryContext(ctx,
		`SELECT trigger_name, event_manipulation, event_object_table, action_timing, action_statement
		 FROM information_schema.triggers WHERE trigger_schema = ? ORDER BY trigger_name`, schema)
	if err != nil {
		return fmt.Errorf("backup: list triggers: %w", err)
	}
	var trigs []mysqlTriggerRow
	for rows.Next() {
		var t mysqlTriggerRow
		if err := rows.Scan(&t.Name, &t.Event, &t.Table, &t.Timing, &t.Statement); err != nil {
			_ = rows.Close()
			return err
		}
		trigs = append(trigs, t)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, t := range trigs {
		stmt := fmt.Sprintf("CREATE TRIGGER %s %s %s ON %s FOR EACH ROW %s",
			quoteMySQLIdent(t.Name), t.Timing, t.Event, qualifiedRef(schema, t.Table), t.Statement)
		if err := d.printf(atomStart+"\nDROP TRIGGER IF EXISTS %s;\n%s;\n"+atomEnd+"\n\n", qualifiedRef(schema, t.Name), stmt); err != nil {
			return err
		}
	}
	return nil
}

// dumpRoutines 输出指定库全部存储过程/函数(SHOW CREATE 的 DDL 列按列名定位,
// 不依赖列位;产物不含 DEFINER)。
func (d *mysqlDumper) dumpRoutines(ctx context.Context, schema string) error {
	rows, err := d.db.QueryContext(ctx,
		`SELECT routine_name, routine_type FROM information_schema.routines WHERE routine_schema = ? ORDER BY routine_name`, schema)
	if err != nil {
		return fmt.Errorf("backup: list routines: %w", err)
	}
	type routine struct{ name, typ string }
	var list []routine
	for rows.Next() {
		var r routine
		if err := rows.Scan(&r.name, &r.typ); err != nil {
			_ = rows.Close()
			return err
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, r := range list {
		showCmd := "SHOW CREATE PROCEDURE "
		ddlCol := "Create Procedure"
		dropCmd := "PROCEDURE"
		if strings.EqualFold(r.typ, "FUNCTION") {
			showCmd = "SHOW CREATE FUNCTION "
			ddlCol = "Create Function"
			dropCmd = "FUNCTION"
		}
		res, err := d.db.QueryContext(ctx, showCmd+qualifiedRef(schema, r.name))
		if err != nil {
			return fmt.Errorf("backup: show create routine %s: %w", r.name, err)
		}
		cols, cerr := res.Columns()
		if cerr != nil {
			_ = res.Close()
			return cerr
		}
		idx := -1
		for i, c := range cols {
			if c == ddlCol {
				idx = i
				break
			}
		}
		if idx < 0 {
			_ = res.Close()
			return fmt.Errorf("backup: SHOW CREATE %s lacks %q column", r.typ, ddlCol)
		}
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		var ddl string
		if res.Next() {
			if err := res.Scan(ptrs...); err == nil && vals[idx] != nil {
				ddl = string(vals[idx])
			}
		}
		_ = res.Close()
		if ddl == "" {
			continue // 无法取得 DDL(权限不足等)→ 跳过该例程,dump 继续
		}
		if err := d.printf(atomStart+"\nDROP %s IF EXISTS %s;\n%s;\n"+atomEnd+"\n\n", dropCmd, qualifiedRef(schema, r.name), ddl); err != nil {
			return err
		}
	}
	return nil
}

// formatMySQLValue 把扫描出的值渲染为 SQL 字面量;二进制列走十六进制。
func formatMySQLValue(v any, binary bool) string {
	switch val := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		if binary {
			return fmt.Sprintf("X'%x'", val)
		}
		return quoteMySQLString(string(val))
	case string:
		return quoteMySQLString(val)
	case bool:
		if val {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", val)
	}
}

// quoteMySQLString 渲染带引号的字符串字面量(反斜杠/引号/控制字符转义)。
func quoteMySQLString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case 0:
			b.WriteString(`\0`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '"':
			b.WriteString(`\"`)
		case 0x1a:
			b.WriteString(`\Z`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}
