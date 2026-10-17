package dbadmin

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// QueryResult 是 SQL 控制台执行结果:查询语句返回行集,其余返回影响行数。
type QueryResult struct {
	IsSelect     bool   `json:"isSelect"`
	Columns      []string `json:"columns,omitempty"`
	Rows         [][]any  `json:"rows,omitempty"`
	RowsAffected int64  `json:"rowsAffected,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"` // 结果行超过上限被截断
}

// selectLeading 报告语句首词是否「查询类」(决定 Query vs Exec 路径;不完美但足够,
// 只读模式的正确性由只读事务兜底,与分类无关)。
func selectLeading(stmt string) bool {
	word := firstWord(stmt)
	switch word {
	case "SELECT", "SHOW", "EXPLAIN", "DESC", "DESCRIBE", "VALUES", "TABLE", "WITH", "ANALYZE":
		return true
	}
	return false
}

func firstWord(s string) string {
	s = strings.TrimLeft(strings.TrimSpace(s), "( \t\r\n")
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return strings.ToUpper(s[:i])
	}
	return strings.ToUpper(s)
}

// RunSQL 执行单条语句(SQL 控制台)。空语句/超长语句拒绝;控制台为单语句语义,
// 分号拼接的多条语句由驱动(未开 multiStatements)直接拒绝,导入走 Import 专用通道。
// 只读模式:语句在只读事务内执行并回滚,任何写语句由服务端拒绝(双保险:分类白名单
// 只影响错误提示的友好度,不承担安全语义)。
func (s *service) RunSQL(ctx context.Context, id, database, statement string) (*QueryResult, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	statement = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(statement), ";"))
	if statement == "" {
		return nil, fmt.Errorf("%w: empty statement", ErrInvalidInput)
	}
	if len(statement) > 1<<20 { // 1MB:控制台单语句上限
		return nil, fmt.Errorf("%w: statement too large", ErrInvalidInput)
	}
	if strings.ContainsRune(statement, '\x00') {
		return nil, fmt.Errorf("%w: statement", ErrInvalidInput)
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	conn, err := s.connect(qctx, inst, database, connOpts{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if inst.ReadOnly {
		return runReadOnly(qctx, conn, inst.Engine, statement)
	}
	return runStatementOn(qctx, conn, inst.Engine, statement)
}

// runReadOnly 在只读事务中执行并回滚(写语句会被服务端以「read-only transaction」
// 拒绝,结果原样透出;查询结果正常返回)。
func runReadOnly(ctx context.Context, db *sql.DB, engine, statement string) (*QueryResult, error) {
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "START TRANSACTION READ ONLY"); err != nil {
		// 老版本 MySQL(5.6-)不支持 READ ONLY:退回分类白名单防线——
		// 非查询类一律拒绝,查询类照常执行(只读无害)。
		if !selectLeading(statement) {
			return nil, ErrReadonly
		}
		return runStatementOn(ctx, db, engine, statement)
	}
	defer func() { _, _ = c.ExecContext(context.Background(), "ROLLBACK") }()
	return runStatementOn(ctx, c, engine, statement)
}

// runStatementOn 在 db 或连接上执行单语句:查询类走 Query(收集 ≤maxResultRows 行,
// 列名 + 规整值);执行失败且疑似「无结果集」错误时回退 Exec。
func runStatementOn(ctx context.Context, q queryer, engine, statement string) (*QueryResult, error) {
	if selectLeading(statement) {
		rows, err := q.QueryContext(ctx, statement)
		if err == nil {
			return collectRows(rows)
		}
		if !looksLikeExecOnly(err) {
			return nil, humanQueryError(err)
		}
		// 回退 Exec(如 pgx 对返回 CommandComplete 的语句报「非结果集」)。
	}
	res, err := q.ExecContext(ctx, statement)
	if err != nil {
		return nil, humanQueryError(err)
	}
	n, _ := res.RowsAffected()
	return &QueryResult{IsSelect: false, RowsAffected: n}, nil
}

// queryer 抽象 *sql.DB 与 *sql.Conn(只读事务需要在同一连接上执行)。
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// looksLikeExecOnly 判断 Query 失败是否因「语句不返回结果集」(驱动文案各异,保守匹配)。
func looksLikeExecOnly(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "exec") && strings.Contains(msg, "result") ||
		strings.Contains(msg, "no resultset") ||
		strings.Contains(msg, "command") && strings.Contains(msg, "tag")
}

// collectRows 收集结果集(限行限格;超限标记截断)。
func collectRows(rows *sql.Rows) (*QueryResult, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, humanQueryError(err)
	}
	out := &QueryResult{IsSelect: true, Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		if len(out.Rows) >= maxResultRows {
			out.Truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, humanQueryError(err)
		}
		for i, v := range vals {
			vals[i] = normalizeCell(v)
		}
		out.Rows = append(out.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, humanQueryError(err)
	}
	return out, nil
}
