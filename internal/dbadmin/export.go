package dbadmin

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// 导出限量(防一次导出拖垮对端/网关;超限截断并留注释标记)。
const (
	maxExportRows  = 1_000_000
	maxExportBytes = 200 << 20
	batchRows      = 100 // 每条多值 INSERT 聚合的行数
)

// ExportSpec 是导出参数(Table 空 = 整库;Format: sql | csv——CSV 仅支持单表)。
type ExportSpec struct {
	Database string
	Table    string
	Format   string
}

// flusher 可 Flush 的流(http.Flusher;非流式场景无此能力也照常工作)。
type flusher interface{ Flush() }

// flushWriter 包装响应流:每批数据后尽力 Flush(流式下载体感)。
type flushWriter struct {
	w       io.Writer
	f       flusher
	written int64
}

func newFlushWriter(w io.Writer) *flushWriter {
	fw := &flushWriter{w: w}
	if f, ok := w.(flusher); ok {
		fw.f = f
	}
	return fw
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.written += int64(n)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

func (fw *flushWriter) writeString(s string) error {
	_, err := io.WriteString(fw, s)
	return err
}

// Export 流式导出:SQL(MySQL 附 SHOW CREATE TABLE 的 DDL + 多值 INSERT 数据;
// PG 为数据-only INSERT 并注明)或 CSV(单表)。读语义,只读模式同样可用。
func (s *service) Export(ctx context.Context, id string, spec ExportSpec, w io.Writer) error {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	switch spec.Format {
	case "sql", "csv":
	default:
		return fmt.Errorf("%w: export format", ErrInvalidInput)
	}
	if spec.Format == "csv" && spec.Table == "" {
		return fmt.Errorf("%w: csv requires table", ErrInvalidInput)
	}
	qctx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()
	conn, err := s.connect(qctx, inst, spec.Database, connOpts{readTimeout: exportTimeout})
	if err != nil {
		return err
	}
	defer conn.Close()
	fw := newFlushWriter(w)

	tables := []string{spec.Table}
	if spec.Table == "" {
		list, err := s.ListTables(qctx, id, spec.Database)
		if err != nil {
			return err
		}
		tables = make([]string, 0, len(list))
		for _, t := range list {
			tables = append(tables, t.Ref)
		}
	}
	totalRows := int64(0)
	for _, t := range tables {
		n, err := exportTable(qctx, conn, inst.Engine, spec.Database, t, spec.Format, fw, totalRows)
		totalRows += n
		if err != nil {
			// 流已提交(状态码不可再改):错误以尾注形式留在导出文件里,同时上抛给调用方审计。
			_ = fw.writeString("\n-- 导出中断: " + strings.ReplaceAll(err.Error(), "\n", " ") + "\n")
			return err
		}
		if totalRows >= maxExportRows || fw.written >= maxExportBytes {
			_ = fw.writeString("\n-- 已达导出上限(行数或字节数),结果被截断\n")
			return nil
		}
	}
	return nil
}

// exportTable 导出一张表,返回导出行数。
func exportTable(ctx context.Context, db *sql.DB, engine, database, table, format string, fw *flushWriter, doneRows int64) (int64, error) {
	st, err := tableColumnsForExport(ctx, db, engine, database, table)
	if err != nil {
		return 0, err
	}
	ref, err := quoteTableRef(engine, database, table)
	if err != nil {
		return 0, err
	}
	cols := columnNames(st.Columns)
	if format == "csv" {
		return exportCSVRows(ctx, db, engine, ref, cols, fw)
	}
	// SQL 头注释 + 结构(MySQL 用 SHOW CREATE TABLE 原文;PG 不重建 DDL,注明)。
	if err := fw.writeString(fmt.Sprintf("\n--\n-- %s\n--\n\n", table)); err != nil {
		return 0, err
	}
	if engine == EngineMySQL {
		var tblName, ddl string
		if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE "+ref).Scan(&tblName, &ddl); err != nil {
			return 0, humanQueryError(err)
		}
		if err := fw.writeString("DROP TABLE IF EXISTS " + mustQuote(engine, table) + ";\n" + ddl + ";\n\n"); err != nil {
			return 0, err
		}
	} else if err := fw.writeString("-- 说明:PostgreSQL 导出仅含数据(INSERT),表结构不在其中\n\n"); err != nil {
		return 0, err
	}
	return exportInsertRows(ctx, db, engine, ref, cols, fw, doneRows)
}

// tableColumnsForExport 取表列(导出直连场景不经过实例级 TableStructure,复用其查询)。
func tableColumnsForExport(ctx context.Context, db *sql.DB, engine, database, table string) (*TableStructure, error) {
	if engine == EngineMySQL {
		return tableStructureMySQL(ctx, db, database, table)
	}
	return tableStructurePostgres(ctx, db, table)
}

// exportInsertRows 流式生成多值 INSERT(每 batchRows 行一条;超限截断标记)。
func exportInsertRows(ctx context.Context, db *sql.DB, engine, ref string, cols []string, fw *flushWriter, doneRows int64) (int64, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+joinQuoted(engine, cols)+" FROM "+ref)
	if err != nil {
		return 0, humanQueryError(err)
	}
	defer rows.Close()
	count := int64(0)
	prefix := "INSERT INTO " + ref + " (" + joinQuoted(engine, cols) + ") VALUES "
	batch := make([]string, 0, batchRows)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := fw.writeString(prefix + strings.Join(batch, ",") + ";\n"); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		if doneRows+count >= maxExportRows || fw.written >= maxExportBytes {
			_ = flush()
			return count, nil
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return count, humanQueryError(err)
		}
		lits := make([]string, 0, len(vals))
		for _, v := range vals {
			lits = append(lits, sqlLiteral(engine, v))
		}
		batch = append(batch, "("+strings.Join(lits, ", ")+")")
		count++
		if len(batch) >= batchRows {
			if err := flush(); err != nil {
				return count, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return count, humanQueryError(err)
	}
	if err := flush(); err != nil {
		return count, err
	}
	if count > 0 {
		_ = fw.writeString("\n")
	}
	return count, nil
}

// exportCSVRows 流式导出 CSV(含表头)。
func exportCSVRows(ctx context.Context, db *sql.DB, engine, ref string, cols []string, fw *flushWriter) (int64, error) {
	cw := csv.NewWriter(fw)
	if err := cw.Write(cols); err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx, "SELECT "+joinQuoted(engine, cols)+" FROM "+ref)
	if err != nil {
		return 0, humanQueryError(err)
	}
	defer rows.Close()
	count := int64(0)
	for rows.Next() {
		if count >= maxExportRows || fw.written >= maxExportBytes {
			cw.Flush()
			return count, nil
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return count, humanQueryError(err)
		}
		record := make([]string, 0, len(cols))
		for _, v := range vals {
			record = append(record, csvCell(v))
		}
		if err := cw.Write(record); err != nil {
			return count, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, humanQueryError(err)
	}
	cw.Flush()
	return count, cw.Error()
}

// csvCell 单元格字符串化(nil→空;二进制可打印直出,否则 hex)。
func csvCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		if isMostlyPrintable(t) {
			return string(t)
		}
		return "0x" + fmt.Sprintf("%x", t)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprint(v)
	}
}

// sqlLiteral 把扫描值转 SQL 字面量(按引擎转义;二进制走 hex/E'' 形态)。
func sqlLiteral(engine string, v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if t {
			return "TRUE"
		}
		return "FALSE"
	case int64:
		return strconv.FormatInt(t, 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int16:
		return strconv.FormatInt(int64(t), 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'g', -1, 32)
	case time.Time:
		return sqlStringLiteral(engine, t.UTC().Format("2006-01-02 15:04:05.999999"))
	case string:
		return sqlStringLiteral(engine, t)
	case []byte:
		if isMostlyPrintable(t) {
			return sqlStringLiteral(engine, string(t))
		}
		hex := fmt.Sprintf("%x", t)
		if engine == EngineMySQL {
			return "0x" + hex
		}
		return "E'\\\\x" + hex + "'"
	default:
		return sqlStringLiteral(engine, fmt.Sprint(v))
	}
}

// sqlStringLiteral 组字符串字面量:MySQL 反斜杠转义;PG 常规 '' 双写、含反斜杠/控制符时 E'' 前缀。
func sqlStringLiteral(engine, s string) string {
	if engine == EngineMySQL {
		var b strings.Builder
		b.Grow(len(s) + 2)
		b.WriteByte('\'')
		for _, r := range s {
			switch r {
			case '\\':
				b.WriteString(`\\`)
			case '\'':
				b.WriteString(`\'`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case 0:
				b.WriteString(`\0`)
			case 0x1a:
				b.WriteString(`\Z`)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteByte('\'')
		return b.String()
	}
	needE := strings.ContainsAny(s, "\\\n\r\t\x00")
	var b strings.Builder
	if needE {
		b.WriteString("E'")
	} else {
		b.WriteByte('\'')
	}
	for _, r := range s {
		switch r {
		case '\\':
			if needE {
				b.WriteString(`\\`)
			} else {
				b.WriteByte('\\')
			}
		case '\'':
			b.WriteString(`''`)
		case '\n':
			if needE {
				b.WriteString(`\n`)
			} else {
				b.WriteByte('\n')
			}
		case '\r':
			if needE {
				b.WriteString(`\r`)
			} else {
				b.WriteByte('\r')
			}
		case '\t':
			if needE {
				b.WriteString(`\t`)
			} else {
				b.WriteByte('\t')
			}
		case 0:
			b.WriteString(``)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}
