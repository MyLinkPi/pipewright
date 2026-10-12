package objectstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Store 持久化对象存储配置(参数化 SQL,两方言一致)。绝不读/写任何 Secret 明文
// (只存 credential_id;AccessKeyId 非机密,明文列)。
type Store struct {
	db *sql.DB
}

// NewStore 构造对象存储持久层。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// create 落库一条对象存储配置。
func (s *Store) create(ctx context.Context, p *Provider) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO object_stores (id, type, name, endpoint, bucket, path_prefix, access_key_id, credential_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Type, p.Name, p.Endpoint, p.Bucket, p.PathPrefix, p.AccessKeyID, p.CredentialID,
		p.CreatedAt.UTC().Format(time.RFC3339), p.UpdatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("objectstore: insert: %w", err)
	}
	return nil
}

// get 读取单条配置;不存在 → ErrNotFound。
func (s *Store) get(ctx context.Context, id string) (*Provider, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, type, name, endpoint, bucket, path_prefix, access_key_id, credential_id, created_at, updated_at
		 FROM object_stores WHERE id = ?`, id)
	p, err := scanProvider(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return p, nil
}

// list 返回全部配置(created_at 倒序)。
func (s *Store) list(ctx context.Context) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, type, name, endpoint, bucket, path_prefix, access_key_id, credential_id, created_at, updated_at
		 FROM object_stores ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("objectstore: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Provider, 0)
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("objectstore: iterate: %w", err)
	}
	return out, nil
}

// update 编辑配置字段(nil 表示不修改);不存在 → ErrNotFound。
func (s *Store) update(ctx context.Context, id string, in UpdateInput) (*Provider, error) {
	p, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			return nil, ErrEmptyName
		}
		p.Name = name
	}
	if in.Endpoint != nil {
		endpoint := normalizeEndpoint(*in.Endpoint)
		if endpoint == "" {
			return nil, ErrEmptyEndpoint
		}
		p.Endpoint = endpoint
	}
	if in.Bucket != nil {
		bucket := strings.TrimSpace(*in.Bucket)
		if bucket == "" {
			return nil, ErrEmptyBucket
		}
		p.Bucket = bucket
	}
	if in.PathPrefix != nil {
		// 非 nil 即改;空串 = 清空前缀(可选字段,允许为空)。
		p.PathPrefix = NormalizePathPrefix(*in.PathPrefix)
	}
	if in.AccessKeyID != nil {
		ak := strings.TrimSpace(*in.AccessKeyID)
		if ak == "" {
			return nil, ErrEmptyAccessKeyID
		}
		p.AccessKeyID = ak
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE object_stores SET name = ?, endpoint = ?, bucket = ?, path_prefix = ?, access_key_id = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.Endpoint, p.Bucket, p.PathPrefix, p.AccessKeyID, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return nil, fmt.Errorf("objectstore: update: %w", err)
	}
	return s.get(ctx, id)
}

// del 删除配置;不存在 → ErrNotFound。
func (s *Store) del(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM object_stores WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("objectstore: delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanner 抽象 *sql.Row 与 *sql.Rows 的 Scan。
type scanner interface {
	Scan(dest ...any) error
}

// scanProvider 把一行扫描为 Provider(永不读任何密文/Secret 列)。
func scanProvider(sc scanner) (*Provider, error) {
	var (
		p          Provider
		createdStr string
		updatedStr string
	)
	if err := sc.Scan(&p.ID, &p.Type, &p.Name, &p.Endpoint, &p.Bucket, &p.PathPrefix, &p.AccessKeyID, &p.CredentialID, &createdStr, &updatedStr); err != nil {
		return nil, err
	}
	created, err := time.Parse(time.RFC3339, createdStr)
	if err != nil {
		return nil, fmt.Errorf("objectstore: parse created_at: %w", err)
	}
	updated, err := time.Parse(time.RFC3339, updatedStr)
	if err != nil {
		return nil, fmt.Errorf("objectstore: parse updated_at: %w", err)
	}
	p.CreatedAt = created
	p.UpdatedAt = updated
	return &p, nil
}

// newProvider 据归一化后的入参构造一条待插入配置(id/时间戳在此填好)。
func newProvider(in CreateInput) *Provider {
	now := time.Now().UTC()
	return &Provider{
		ID:           uuid.NewString(),
		Type:         in.Type,
		Name:         in.Name,
		Endpoint:     in.Endpoint,
		Bucket:       in.Bucket,
		PathPrefix:   in.PathPrefix,
		AccessKeyID:  in.AccessKeyID,
		CredentialID: in.CredentialID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}
