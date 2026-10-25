// replay_mysql.go:MySQL 模式的恢复回放——先清空现有对象(dump 自带 DROP,这一步
// 兜住「备份之后新建的表」),再按语句块分批 Exec(dump 内的 PWSTMT-ATOM 标记
// 保证触发器等含内部分号的服务端 DDL 不被行尾分号误切)。
package backup

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// replayChunkMax 单块上限:multiStatements 单次 Exec 是一个网络包,
// 留足 max_allowed_packet 余量(默认 64MB,常见托管 256MB+)。
const replayChunkMax = 16 << 20

// replayMySQLDump 按 DSN 打开多语句连接,清空现库后回放 dump 文件。
func replayMySQLDump(dsn, dumpPath string) error {
	if strings.TrimSpace(dsn) == "" {
		return fmt.Errorf("backup: mysql 恢复需要 DSN")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("backup: parse dsn: %w", err)
	}
	cfg.ClientFoundRows = true
	cfg.MultiStatements = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		return fmt.Errorf("backup: ping mysql: %w", err)
	}
	if err := dropAllObjects(db); err != nil {
		return err
	}
	f, err := os.Open(dumpPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return execChunked(db, f)
}

// dropAllObjects 删除当前库的全部触发器/视图/基表(外键约束关闭下任意顺序)。
func dropAllObjects(db *sql.DB) error {
	if _, err := db.Exec("SET FOREIGN_KEY_CHECKS=0"); err != nil {
		return fmt.Errorf("backup: disable fk checks: %w", err)
	}
	defer func() { _, _ = db.Exec("SET FOREIGN_KEY_CHECKS=1") }()

	var triggers []string
	rows, err := db.Query("SHOW TRIGGERS")
	if err != nil {
		return fmt.Errorf("backup: show triggers: %w", err)
	}
	cols, _ := rows.Columns()
	idx := -1
	for i, c := range cols {
		if c == "Trigger" {
			idx = i
			break
		}
	}
	if idx >= 0 {
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err == nil && vals[idx] != nil {
				triggers = append(triggers, string(vals[idx]))
			}
		}
	}
	_ = rows.Close()
	for _, t := range triggers {
		if _, err := db.Exec("DROP TRIGGER IF EXISTS " + quoteMySQLIdent(t)); err != nil {
			return fmt.Errorf("backup: drop trigger %s: %w", t, err)
		}
	}

	rows, err = db.Query(
		`SELECT table_name, table_type FROM information_schema.tables WHERE table_schema = DATABASE()`)
	if err != nil {
		return fmt.Errorf("backup: list tables: %w", err)
	}
	var views, tables []string
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			_ = rows.Close()
			return err
		}
		if typ == "VIEW" {
			views = append(views, name)
		} else {
			tables = append(tables, name)
		}
	}
	_ = rows.Close()
	// 先视图后表:视图依赖基表,先删表会因依赖报错(FK 关闭不影响视图依赖)。
	for _, v := range views {
		if _, err := db.Exec("DROP VIEW IF EXISTS " + quoteMySQLIdent(v)); err != nil {
			return fmt.Errorf("backup: drop view %s: %w", v, err)
		}
	}
	for _, t := range tables {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + quoteMySQLIdent(t)); err != nil {
			return fmt.Errorf("backup: drop table %s: %w", t, err)
		}
	}
	return nil
}

// execChunked 逐行读 dump,在语句边界(非 ATOM 区内行尾分号)切块 Exec。
func execChunked(db *sql.DB, r *os.File) error {
	scanner := bufio.NewScanner(r)
	// 单行上限:一条 INSERT 批次可能上百 MB?批 200 行 × 单行值上限 64KB ≈ 12.5MB,
	// 取 64MB 缓冲绰绰有余;超出则属异常数据,宁可报错不可静默截断。
	scanner.Buffer(make([]byte, 0, 64*1024), 64<<20)

	var chunk strings.Builder
	inAtom := false
	lineNo := 0
	execChunk := func() error {
		stmt := strings.TrimRight(chunk.String(), "\n")
		chunk.Reset()
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			return nil
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("backup: replay near line %d: %w", lineNo, err)
		}
		return nil
	}
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		switch line {
		case atomStart:
			inAtom = true
		case atomEnd:
			inAtom = false
		}
		chunk.WriteString(line)
		chunk.WriteByte('\n')
		if !inAtom && (strings.HasSuffix(line, ";") || chunk.Len() > replayChunkMax) {
			if err := execChunk(); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("backup: read dump: %w", err)
	}
	if inAtom {
		return fmt.Errorf("backup: dump 以未闭合的 %s 结束(文件损坏?)", atomStart)
	}
	return execChunk() // 尾块(无行尾分号的收尾语句)
}
