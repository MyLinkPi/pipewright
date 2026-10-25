// dump_mysql.go:MySQL 模式下的纯 Go SQL dump(备份侧)与回放(恢复侧)。
// 不依赖 mysqldump 二进制(容器/瘦控制机不可假设其存在),与 dbadmin.Export 同路线:
// information_schema 枚举表 → SHOW CREATE TABLE/SHOW TRIGGERS 取 DDL → 分批 INSERT。
// 回放侧开着 FOREIGN_KEY_CHECKS=0 吃任意建表顺序;连接须 multiStatements=true。
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
	return d.dump(ctx)
}

type mysqlDumper struct {
	db *sql.DB
	w  io.Writer
}

func (d *mysqlDumper) printf(format string, args ...any) error {
	_, err := fmt.Fprintf(d.w, format, args...)
	return err
}

// dump 全量导出当前 DATABASE() 下的基表/视图/触发器。
func (d *mysqlDumper) dump(ctx context.Context) error {
	if err := d.printf("SET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS=0;\nSET UNIQUE_CHECKS=0;\n\n"); err != nil {
		return err
	}
	tables, views, err := d.listTables(ctx)
	if err != nil {
		return err
	}
	// 基表按名字序建(外键约束关闭,顺序无碍);视图依赖基表,统一放最后。
	sorted := append([]string(nil), tables...)
	sort.Strings(sorted)
	for _, t := range sorted {
		if err := d.dumpTable(ctx, t); err != nil {
			return err
		}
	}
	viewSorted := append([]string(nil), views...)
	sort.Strings(viewSorted)
	for _, v := range viewSorted {
		if err := d.dumpView(ctx, v); err != nil {
			return err
		}
	}
	if err := d.dumpTriggers(ctx, append(append([]string(nil), sorted...), viewSorted...)); err != nil {
		return err
	}
	return d.printf("\nSET FOREIGN_KEY_CHECKS=1;\nSET UNIQUE_CHECKS=1;\n")
}

// listTables 返回当前库的基表与视图(系统表/schema_migrations 不排除——恢复需要它)。
func (d *mysqlDumper) listTables(ctx context.Context) (tables, views []string, err error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT table_name, table_type FROM information_schema.tables WHERE table_schema = DATABASE()`)
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
// 服务端生成 DDL(触发器体内含分号,不能按行尾分号切分语句)。
const (
	atomStart = "-- PWSTMT-ATOM-START"
	atomEnd   = "-- PWSTMT-ATOM-END"
)

// dumpTable 输出单表:DROP + CREATE + 数据 INSERT。DDL 用 PWSTMT-ATOM 标记包裹
// (回放器据此切分语句块:服务端生成的 DDL 内部可能含分号,不能按行尾分号切)。
func (d *mysqlDumper) dumpTable(ctx context.Context, table string) error {
	ref := quoteMySQLIdent(table)
	var name, ddl string
	if err := d.db.QueryRowContext(ctx, "SHOW CREATE TABLE "+ref).Scan(&name, &ddl); err != nil {
		return fmt.Errorf("backup: show create %s: %w", table, err)
	}
	if err := d.printf(atomStart+"\nDROP TABLE IF EXISTS %s;\n%s;\n"+atomEnd+"\n\n", ref, ddl); err != nil {
		return err
	}

	cols, binFlags, err := d.tableColumns(ctx, table)
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
func (d *mysqlDumper) tableColumns(ctx context.Context, table string) ([]string, []bool, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? ORDER BY ordinal_position`, table)
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

// dumpView 输出单视图:DROP VIEW + CREATE(来自 SHOW CREATE TABLE 对视图同样适用)。
func (d *mysqlDumper) dumpView(ctx context.Context, view string) error {
	ref := quoteMySQLIdent(view)
	var name, ddl string
	if err := d.db.QueryRowContext(ctx, "SHOW CREATE TABLE "+ref).Scan(&name, &ddl); err != nil {
		return fmt.Errorf("backup: show create view %s: %w", view, err)
	}
	if err := d.printf(atomStart+"\nDROP VIEW IF EXISTS %s;\n%s;\n"+atomEnd+"\n\n", ref, ddl); err != nil {
		return err
	}
	return nil
}

// dumpTriggers 输出全部触发器(平台审计的 append-only 硬拦就在触发器里,必须随包)。
func (d *mysqlDumper) dumpTriggers(ctx context.Context, _ []string) error {
	rows, err := d.db.QueryContext(ctx, "SHOW TRIGGERS")
	if err != nil {
		return fmt.Errorf("backup: show triggers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	// SHOW TRIGGERS 固定列:Trigger/Event/Table/Statement/Timing/Created/...;取 Trigger 名即可。
	idx := -1
	for i, c := range cols {
		if c == "Trigger" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("backup: SHOW TRIGGERS lacks Trigger column")
	}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var names []string
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if vals[idx] != nil {
			names = append(names, string(vals[idx]))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Strings(names)
	for _, n := range names {
		var tName, ddl string
		// SHOW CREATE TRIGGER 返回多列,首列即触发器名,第二列为原始 CREATE 语句。
		if err := d.db.QueryRowContext(ctx, "SHOW CREATE TRIGGER "+quoteMySQLIdent(n)).Scan(&tName, &ddl, &sql.RawBytes{}, &sql.RawBytes{}, &sql.RawBytes{}, &sql.RawBytes{}, &sql.RawBytes{}); err != nil {
			return fmt.Errorf("backup: show create trigger %s: %w", n, err)
		}
		// 用 DROP TRIGGER 保证回放幂等(创建语句本身带 IF NOT EXISTS 语义缺失时也安全)。
		if err := d.printf(atomStart+"\nDROP TRIGGER IF EXISTS %s;\n%s;\n"+atomEnd+"\n\n", quoteMySQLIdent(n), ddl); err != nil {
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
