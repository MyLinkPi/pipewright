package dbadmin

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// BrowseQuery 是分页浏览参数(Sort/Dir/Where 由前端过滤栏提供)。
type BrowseQuery struct {
	Limit  int
	Offset int
	Sort   string // 列名;空 = 主键序;仍需白名单校验
	Dir    string // asc | desc
	Where  string // 自由条件(phpMyAdmin 语义;驱动单语句模式兜底防叠句)
}

// BrowseResult 是浏览结果(列名 + 规整后的行值)。
type BrowseResult struct {
	Total   int64    `json:"total"`
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// RowKey 是主键定位(列名 + 对应值;值为字符串,由驱动按列类型协同比较)。
type RowKey struct {
	Columns []string `json:"columns"`
	Values  []string `json:"values"`
}

// ensureWritable 只读模式守卫:所有写操作入口先过此关。
func ensureWritable(inst *Instance) error {
	if inst.ReadOnly {
		return ErrReadonly
	}
	return nil
}

// pholder 是占位符序列生成器:MySQL 产出 ?,PG 产出 $1..$n(pgx 不认 ?)。
type pholder struct {
	engine string
	n      int
}

func (p *pholder) next() string {
	p.n++
	if p.engine == EngineMySQL {
		return "?"
	}
	return "$" + strconv.Itoa(p.n)
}

// Browse 分页浏览一张表:先取结构(列白名单 + 主键排序兜底)→ COUNT → SELECT 分页。
func (s *service) Browse(ctx context.Context, id, db, table string, q BrowseQuery) (*BrowseResult, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	st, err := s.TableStructure(ctx, id, db, table)
	if err != nil {
		return nil, err
	}
	if len(st.Columns) == 0 {
		return nil, fmt.Errorf("%w: table %s", ErrInvalidIdentifier, table)
	}
	if q.Limit < 1 || q.Limit > maxBrowseLimit {
		q.Limit = 50
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	// 排序白名单:列必须真实存在;方向仅 asc/desc;缺省回落主键(无主键按第一列)。
	columnSet := map[string]bool{}
	for _, c := range st.Columns {
		columnSet[c.Name] = true
	}
	orderCol := ""
	if q.Sort != "" {
		if !columnSet[q.Sort] {
			return nil, fmt.Errorf("%w: sort column", ErrInvalidInput)
		}
		orderCol = q.Sort
	} else if len(st.PrimaryKeys) > 0 {
		orderCol = st.PrimaryKeys[0]
	} else {
		orderCol = st.Columns[0].Name
	}
	dir := "ASC"
	if strings.EqualFold(q.Dir, "desc") {
		dir = "DESC"
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	conn, err := s.connect(qctx, inst, db, connOpts{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ref, err := quoteTableRef(inst.Engine, db, table)
	if err != nil {
		return nil, err
	}
	cond := whereClause(q.Where)
	// LIMIT/OFFSET 为受控整数,直接内联(两方言一致,免占位符差异)。
	var total int64
	if err := conn.QueryRowContext(qctx, "SELECT COUNT(*) FROM "+ref+cond).Scan(&total); err != nil {
		return nil, humanQueryError(err)
	}
	cols := columnNames(st.Columns)
	sel := "SELECT " + joinQuoted(inst.Engine, cols) + " FROM " + ref + cond +
		" ORDER BY " + mustQuote(inst.Engine, orderCol) + " " + dir +
		" LIMIT " + strconv.Itoa(q.Limit) + " OFFSET " + strconv.Itoa(q.Offset)
	rows, err := conn.QueryContext(qctx, sel)
	if err != nil {
		return nil, humanQueryError(err)
	}
	defer rows.Close()
	out := &BrowseResult{Total: total, Columns: cols, Rows: [][]any{}}
	for rows.Next() {
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

// whereClause 组 WHERE 子句(自由条件;拒绝 NUL)。含分号的条件原样透传——多语句
// 由驱动(未开 multiStatements)拒绝并透出错误,字符串字面量中的分号不受影响。
func whereClause(cond string) string {
	cond = strings.TrimSpace(cond)
	if cond == "" || strings.ContainsRune(cond, '\x00') {
		return ""
	}
	return " WHERE (" + cond + ")"
}

// mustQuote 引用列名(已白名单校验,错误不可能发生)。
func mustQuote(engine, ident string) string {
	q, _ := quoteIdent(engine, ident)
	return q
}

func columnNames(cols []ColumnInfo) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Name)
	}
	return out
}

// primaryKeyWhere 组主键定位子句(列名白名单 = 结构主键;参数按序绑定)。
func primaryKeyWhere(engine string, st *TableStructure, key RowKey) (string, []any, error) {
	if len(st.PrimaryKeys) == 0 {
		return "", nil, ErrNoPrimaryKey
	}
	if len(key.Columns) != len(st.PrimaryKeys) || len(key.Values) != len(key.Columns) {
		return "", nil, fmt.Errorf("%w: primary key arity", ErrInvalidInput)
	}
	ph := &pholder{engine: engine}
	parts := make([]string, 0, len(key.Columns))
	args := make([]any, 0, len(key.Values))
	for i, c := range key.Columns {
		if !slices.Contains(st.PrimaryKeys, c) {
			return "", nil, fmt.Errorf("%w: primary key column", ErrInvalidInput)
		}
		parts = append(parts, mustQuote(engine, c)+" = "+ph.next())
		args = append(args, key.Values[i])
	}
	return " WHERE " + strings.Join(parts, " AND "), args, nil
}

// GetRow 按主键取单行(行编辑回显)。
func (s *service) GetRow(ctx context.Context, id, db, table string, key RowKey) (map[string]any, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	st, err := s.TableStructure(ctx, id, db, table)
	if err != nil {
		return nil, err
	}
	ref, err := quoteTableRef(inst.Engine, db, table)
	if err != nil {
		return nil, err
	}
	cond, args, err := primaryKeyWhere(inst.Engine, st, key)
	if err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	conn, err := s.connect(qctx, inst, db, connOpts{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	cols := columnNames(st.Columns)
	sel := "SELECT " + joinQuoted(inst.Engine, cols) + " FROM " + ref + cond
	rowVals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range rowVals {
		ptrs[i] = &rowVals[i]
	}
	if err := conn.QueryRowContext(qctx, sel, args...).Scan(ptrs...); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: row not found", ErrInvalidInput)
		}
		return nil, humanQueryError(err)
	}
	out := map[string]any{}
	for i, c := range cols {
		out[c] = normalizeCell(rowVals[i])
	}
	return out, nil
}

// InsertRow 插入一行(values 键须为真实列;值 nil = NULL)。
func (s *service) InsertRow(ctx context.Context, id, db, table string, values map[string]any) error {
	return s.mutateRow(ctx, id, db, table, values, nil)
}

// UpdateRow 按主键更新一行(values 同上;key 为主键旧值)。
func (s *service) UpdateRow(ctx context.Context, id, db, table string, key RowKey, values map[string]any) error {
	return s.mutateRow(ctx, id, db, table, values, &key)
}

// mutateRow 统一实现 INSERT/UPDATE:列名白名单校验后参数化执行。
func (s *service) mutateRow(ctx context.Context, id, db, table string, values map[string]any, key *RowKey) error {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := ensureWritable(inst); err != nil {
		return err
	}
	st, err := s.TableStructure(ctx, id, db, table)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return fmt.Errorf("%w: empty values", ErrInvalidInput)
	}
	columnSet := map[string]bool{}
	for _, c := range st.Columns {
		columnSet[c.Name] = true
	}
	cols := make([]string, 0, len(values))
	vals := make([]any, 0, len(values))
	for k, v := range values {
		if !columnSet[k] {
			return fmt.Errorf("%w: unknown column", ErrInvalidInput)
		}
		cols = append(cols, k)
		vals = append(vals, v)
	}
	ref, err := quoteTableRef(inst.Engine, db, table)
	if err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	conn, err := s.connect(qctx, inst, db, connOpts{})
	if err != nil {
		return err
	}
	defer conn.Close()

	ph := &pholder{engine: inst.Engine}
	var stmt string
	var args []any
	if key == nil {
		placehos := make([]string, 0, len(cols))
		for range cols {
			placehos = append(placehos, ph.next())
		}
		stmt = "INSERT INTO " + ref + " (" + joinQuoted(inst.Engine, cols) + ") VALUES (" + strings.Join(placehos, ", ") + ")"
		args = vals
	} else {
		cond, keyArgs, err := primaryKeyWhere(inst.Engine, st, *key)
		if err != nil {
			return err
		}
		sets := make([]string, 0, len(cols))
		for _, c := range cols {
			sets = append(sets, mustQuote(inst.Engine, c)+" = "+ph.next())
		}
		stmt = "UPDATE " + ref + " SET " + strings.Join(sets, ", ") + cond
		args = append(vals, keyArgs...)
	}
	if _, err := conn.ExecContext(qctx, stmt, args...); err != nil {
		return humanQueryError(err)
	}
	return nil
}

// DeleteRow 按主键删除一行。
func (s *service) DeleteRow(ctx context.Context, id, db, table string, key RowKey) error {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := ensureWritable(inst); err != nil {
		return err
	}
	st, err := s.TableStructure(ctx, id, db, table)
	if err != nil {
		return err
	}
	ref, err := quoteTableRef(inst.Engine, db, table)
	if err != nil {
		return err
	}
	cond, args, err := primaryKeyWhere(inst.Engine, st, key)
	if err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	conn, err := s.connect(qctx, inst, db, connOpts{})
	if err != nil {
		return err
	}
	defer conn.Close()
	res, err := conn.ExecContext(qctx, "DELETE FROM "+ref+cond, args...)
	if err != nil {
		return humanQueryError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: row not found", ErrInvalidInput)
	}
	return nil
}

// joinQuoted 组逗号分隔的引用列清单。
func joinQuoted(engine string, cols []string) string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, mustQuote(engine, c))
	}
	return strings.Join(out, ", ")
}

// DropTable 删除表(须解锁;审计在 httpapi 层)。
func (s *service) DropTable(ctx context.Context, id, db, table string) error {
	return s.execTableDDL(ctx, id, db, "DROP TABLE ", table)
}

// TruncateTable 清空表(保留结构)。
func (s *service) TruncateTable(ctx context.Context, id, db, table string) error {
	return s.execTableDDL(ctx, id, db, "TRUNCATE TABLE ", table)
}

// execTableDDL 统一表级 DDL 执行(解锁校验 → 引用转义 → 执行)。
func (s *service) execTableDDL(ctx context.Context, id, db, verb, table string) error {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := ensureWritable(inst); err != nil {
		return err
	}
	ref, err := quoteTableRef(inst.Engine, db, table)
	if err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	conn, err := s.connect(qctx, inst, db, connOpts{})
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(qctx, verb+ref); err != nil {
		return humanQueryError(err)
	}
	return nil
}

// CreateDatabase 建库(严格标识符白名单;MySQL 服务器级执行,PG 在维护库执行)。
func (s *service) CreateDatabase(ctx context.Context, id, name string) error {
	return s.execDatabaseDDL(ctx, id, name, false)
}

// DropDatabase 删库(不可逆;审计与确认在 httpapi/前端层)。
func (s *service) DropDatabase(ctx context.Context, id, name string) error {
	return s.execDatabaseDDL(ctx, id, name, true)
}

func (s *service) execDatabaseDDL(ctx context.Context, id, name string, drop bool) error {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := ensureWritable(inst); err != nil {
		return err
	}
	if err := validateDatabaseName(name); err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var conn *sql.DB
	if inst.Engine == EngineMySQL {
		conn, err = s.connectAnyDB(qctx, inst, connOpts{})
	} else {
		conn, err = s.connect(qctx, inst, "postgres", connOpts{})
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	var stmt string
	if drop {
		stmt = "DROP DATABASE " + mustQuote(inst.Engine, name)
	} else if inst.Engine == EngineMySQL {
		stmt = "CREATE DATABASE " + mustQuote(inst.Engine, name) + " CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
	} else {
		stmt = "CREATE DATABASE " + mustQuote(inst.Engine, name)
	}
	if _, err := conn.ExecContext(qctx, stmt); err != nil {
		return humanQueryError(err)
	}
	return nil
}
