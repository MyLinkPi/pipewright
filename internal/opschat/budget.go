package opschat

import (
	"context"
	"database/sql"
	"errors"
	"log"
)

const runStorageReserve int64 = 4 << 20

// LENGTH on BLOB/MEDIUMBLOB counts bytes in both dialects, including cipher overhead.
func (s *Service) storageBytes(ctx context.Context, q querier, id string) (int64, error) {
	var size int64
	query := `SELECT COALESCE((SELECT LENGTH(body) FROM ops_chat_sessions WHERE id=?),0)`
	args := []any{id}
	for _, table := range []string{"ops_chat_entries", "ops_chat_runs", "ops_chat_calls", "ops_chat_approvals"} {
		query += " + COALESCE((SELECT SUM(LENGTH(body)) FROM " + table + " WHERE session_id=?),0)"
		args = append(args, id)
	}
	if err := q.QueryRowContext(ctx, query, args...).Scan(&size); err != nil {
		return 0, ErrStorage
	}
	return size, nil
}
func (s *Service) runStorageBytes(ctx context.Context, q querier, id, run string) (int64, error) {
	var size int64
	query := `SELECT COALESCE((SELECT LENGTH(body) FROM ops_chat_runs WHERE session_id=? AND id=?),0)`
	args := []any{id, run}
	for _, table := range []string{"ops_chat_entries", "ops_chat_calls", "ops_chat_approvals"} {
		query += " + COALESCE((SELECT SUM(LENGTH(body)) FROM " + table + " WHERE session_id=? AND run_id=?),0)"
		args = append(args, id, run)
	}
	if err := q.QueryRowContext(ctx, query, args...).Scan(&size); err != nil {
		return 0, ErrStorage
	}
	return size, nil
}

// enforceStorage 在写事务提交前校验 session 的存储配额(会话总量上限 +
// 条目数上限 + 活动 run 的 4MB 保留额)。只校验本事务写入的会话:任一会话
// 的体量只会在它自己的写事务里增长,校验别的会话是纯浪费的全库扫描。
func (s *Service) enforceStorage(ctx context.Context, tx *sql.Tx, session string) error {
	if session == "" || !validID(session) {
		return nil
	}
	var active string
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT active_run,visible_count FROM ops_chat_sessions WHERE id=?", session).Scan(&active, &count); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // 会话已在同一事务中删除(Delete),无配额可校验
		}
		return ErrStorage
	}
	size, e := s.storageBytes(ctx, tx, session)
	if e != nil {
		return e
	}
	if size > int64(resourceLimits.SessionBytes) || count > resourceLimits.Entries {
		log.Printf("[opschat] storage quota hit session=%s bytes=%d entries=%d", session, size, count)
		return ErrQuota
	}
	if active != "" {
		used, e := s.runStorageBytes(ctx, tx, session, active)
		if e != nil {
			return e
		}
		// Only this run can spend its reserve. Draft/title edits count as historical storage.
		if size+max(int64(0), runStorageReserve-used) > int64(resourceLimits.SessionBytes) {
			log.Printf("[opschat] storage quota hit (run reserve) session=%s run=%s bytes=%d", session, active, size)
			return ErrQuota
		}
	}
	return nil
}

// StorageBytes is a pure, consistent read of actual encrypted serialized body sizes.
func (s *Service) StorageBytes(ctx context.Context, id string) (int64, error) {
	var size int64
	err := s.transaction(ctx, id, false, func(tx *sql.Tx) error {
		if _, e := s.session(ctx, tx, id); e != nil {
			return e
		}
		var e error
		size, e = s.storageBytes(ctx, tx, id)
		return e
	})
	return size, err
}
