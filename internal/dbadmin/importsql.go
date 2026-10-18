package dbadmin

import (
	"context"
	"fmt"
	"strings"
)

// 导入限量(与 httpapi 层 MaxBytesReader 同值,领域层再兜一道)。
const maxImportBytes = 32 << 20

// ImportResult 是导入执行结果(人话消息直出前端)。
type ImportResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Import 执行多语句 .sql 文本(典型 mysqldump/pg_dump 产物):开启多语句通道
// (MySQL multiStatements / PG simple_protocol)一次性 Exec;失败时驱动错误原样透出。
// 需要已解锁(只读模式拒绝);语句文本绝不进审计(可能含 CREATE USER 凭据字面量)。
func (s *service) Import(ctx context.Context, id, database, dump string) (*ImportResult, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := ensureWritable(inst); err != nil {
		return nil, err
	}
	dump = strings.TrimSpace(dump)
	if dump == "" {
		return nil, fmt.Errorf("%w: empty dump", ErrInvalidInput)
	}
	if len(dump) > maxImportBytes {
		return nil, fmt.Errorf("%w: dump too large", ErrInvalidInput)
	}
	ictx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	conn, err := s.connect(ictx, inst, database, connOpts{
		multiStatements: true,
		readTimeout:     importTimeout,
		connectTimeout:  connectTimeout,
	})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	res, err := conn.ExecContext(ictx, dump)
	if err != nil {
		return nil, humanQueryError(err)
	}
	affected, _ := res.RowsAffected()
	msg := fmt.Sprintf("导入完成(共 %d 字节;驱动影响行数 %d,多语句下以末条语句计)", len(dump), affected)
	return &ImportResult{OK: true, Message: msg}, nil
}
