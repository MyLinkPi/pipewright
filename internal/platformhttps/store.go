package platformhttps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
)

// settingsID 是 platform_https_settings 的单行主键(全库仅此一行)。
const settingsID = "default"

// ErrStoreNotFound 表示设置行不存在(store 层语义;对外经 getOrCreate 兜底,不外泄)。
var ErrStoreNotFound = errors.New("platformhttps: settings row not found")

// Store 持久化平台 HTTPS 单行设置(参数化 SQL,sqlite/mysql 两方言一致)。
type Store struct {
	db *sql.DB
}

// NewStore 构造持久层。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// getOrCreate 读取设置单例;不存在则插入默认行(未启用)后回读(幂等)。
func (s *Store) getOrCreate(ctx context.Context) (*Settings, error) {
	st, err := s.get(ctx)
	if err == nil {
		return st, nil
	}
	if !errors.Is(err, ErrStoreNotFound) {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, ierr := s.db.ExecContext(ctx,
		`INSERT INTO platform_https_settings
		   (id, enabled, server_id, domain, cert_id, upstream_host, upstream_port, http_redirect,
		    status, status_detail, last_applied_at, created_at, updated_at)
		 VALUES (?, 0, '', '', '', '127.0.0.1', 0, 1, '', '', '', ?, ?)`,
		settingsID, now, now)
	if ierr != nil {
		// 并发初始化撞唯一键 → 回读即可。
		if !store.IsUniqueErr(ierr) {
			return nil, fmt.Errorf("platformhttps: insert settings: %w", ierr)
		}
	}
	return s.get(ctx)
}

// get 读取设置单例;不存在 → ErrStoreNotFound。
func (s *Store) get(ctx context.Context) (*Settings, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT enabled, server_id, domain, cert_id, upstream_host, upstream_port, http_redirect,
		        status, status_detail, last_applied_at, created_at, updated_at
		 FROM platform_https_settings WHERE id = ?`, settingsID)
	var (
		st         Settings
		enabled    int
		redirect   int
		lastApply  sql.NullString
		createdStr string
		updatedStr string
	)
	if err := row.Scan(
		&enabled, &st.ServerID, &st.Domain, &st.CertID, &st.UpstreamHost, &st.UpstreamPort, &redirect,
		&st.Status, &st.StatusDetail, &lastApply, &createdStr, &updatedStr,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrStoreNotFound
		}
		return nil, fmt.Errorf("platformhttps: scan settings: %w", err)
	}
	st.Enabled = enabled != 0
	st.HTTPRedirect = redirect != 0
	if lastApply.Valid && lastApply.String != "" {
		if t, err := time.Parse(time.RFC3339, lastApply.String); err == nil {
			st.LastAppliedAt = t
		}
	}
	st.CreatedAt = parseTime(createdStr)
	st.UpdatedAt = parseTime(updatedStr)
	return &st, nil
}

// save 落库设置(全部业务列;调用方已校验)。status 等应用结果列不动(由 setResult/setDisabled 管)。
func (s *Store) save(ctx context.Context, st *Settings) error {
	now := time.Now().UTC().Format(time.RFC3339)
	enabled, redirect := 0, 0
	if st.Enabled {
		enabled = 1
	}
	if st.HTTPRedirect {
		redirect = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE platform_https_settings
		 SET enabled = ?, server_id = ?, domain = ?, cert_id = ?, upstream_host = ?, upstream_port = ?,
		     http_redirect = ?, updated_at = ?
		 WHERE id = ?`,
		enabled, st.ServerID, st.Domain, st.CertID, st.UpstreamHost, st.UpstreamPort,
		redirect, now, settingsID)
	if err != nil {
		return fmt.Errorf("platformhttps: save settings: %w", err)
	}
	st.UpdatedAt = time.Now().UTC()
	return nil
}

// setResult 回写最近一次应用结果(成功 detail 为空)。
func (s *Store) setResult(ctx context.Context, status, detail string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE platform_https_settings
		 SET status = ?, status_detail = ?, last_applied_at = ?, updated_at = ?
		 WHERE id = ?`,
		status, detail, now, now, settingsID)
	if err != nil {
		return fmt.Errorf("platformhttps: set result: %w", err)
	}
	return nil
}

// clearStatus 仅清空应用状态(status/status_detail,不动 last_applied_at):换证书后旧
// status 描述的是旧配置,重置为「待应用」引导重新收敛。
func (s *Store) clearStatus(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE platform_https_settings
		 SET status = '', status_detail = '', updated_at = ?
		 WHERE id = ?`, now, settingsID)
	if err != nil {
		return fmt.Errorf("platformhttps: clear status: %w", err)
	}
	return nil
}

// setDisabled 复位为未启用(保留 server/domain/cert 等配置,清空应用状态,便于再次启用)。
func (s *Store) setDisabled(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE platform_https_settings
		 SET enabled = 0, status = '', status_detail = '', updated_at = ?
		 WHERE id = ?`, now, settingsID)
	if err != nil {
		return fmt.Errorf("platformhttps: set disabled: %w", err)
	}
	return nil
}

// parseTime 宽松解析 RFC3339 时间串(空/坏值 → 零值)。
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
