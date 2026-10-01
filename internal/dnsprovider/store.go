package dnsprovider

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Store 持久化 DNS 提供商与其根区(参数化 SQL,两方言一致)。绝不读/写任何 Secret 明文
// (提供商只存 credential_id;API ID 非机密,明文列)。
type Store struct {
	db *sql.DB
}

// NewStore 构造 DNS 提供商持久层。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// createWithZones 在一个事务里落库提供商 + 初始根区(保证不留「无根区提供商」的半写)。
func (s *Store) createWithZones(ctx context.Context, p *Provider, zones []Zone) error {
	created := p.CreatedAt.UTC().Format(time.RFC3339)
	updated := p.UpdatedAt.UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dnsprovider: begin create: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // 已提交时为 no-op

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO dns_providers (id, type, name, api_id, credential_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Type, p.Name, p.APIID, p.CredentialID, created, updated,
	); err != nil {
		return fmt.Errorf("dnsprovider: insert: %w", err)
	}
	for i := range zones {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO dns_provider_zones (id, provider_id, base_domain, created_at) VALUES (?, ?, ?, ?)`,
			zones[i].ID, zones[i].ProviderID, zones[i].BaseDomain, zones[i].CreatedAt.UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("dnsprovider: insert zone: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dnsprovider: commit create: %w", err)
	}
	return nil
}

// get 读取单条提供商(**不含根区**,由上层装配);不存在 → ErrNotFound。
func (s *Store) get(ctx context.Context, id string) (*Provider, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, type, name, api_id, credential_id, created_at, updated_at
		 FROM dns_providers WHERE id = ?`, id)
	p, err := scanProvider(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return p, nil
}

// list 返回全部提供商(**不含根区**;created_at 倒序)。
func (s *Store) list(ctx context.Context) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, type, name, api_id, credential_id, created_at, updated_at
		 FROM dns_providers ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("dnsprovider: list: %w", err)
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
		return nil, fmt.Errorf("dnsprovider: iterate: %w", err)
	}
	return out, nil
}

// update 编辑展示名 / API ID(nil 表示不修改);不存在 → ErrNotFound。
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
	if in.APIID != nil {
		p.APIID = strings.TrimSpace(*in.APIID)
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE dns_providers SET name = ?, api_id = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.APIID, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return nil, fmt.Errorf("dnsprovider: update: %w", err)
	}
	return s.get(ctx, id)
}

// del 删除提供商(连同其根区;单事务);不存在 → ErrNotFound。
func (s *Store) del(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dnsprovider: begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // 已提交时为 no-op

	res, err := tx.ExecContext(ctx, `DELETE FROM dns_providers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("dnsprovider: delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM dns_provider_zones WHERE provider_id = ?`, id); err != nil {
		return fmt.Errorf("dnsprovider: delete zones: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dnsprovider: commit delete: %w", err)
	}
	return nil
}

// ─── 根区(zone)CRUD ─────────────────────────────────────────────────────────────

// insertZone 给提供商追加一个根区;base_domain 在该提供商下重复 → ErrInvalidBaseDomain。
func (s *Store) insertZone(ctx context.Context, z *Zone) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dns_provider_zones (id, provider_id, base_domain, created_at) VALUES (?, ?, ?, ?)`,
		z.ID, z.ProviderID, z.BaseDomain, z.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		if isConstraintErr(err) {
			return fmt.Errorf("%w:根域已存在", ErrInvalidBaseDomain)
		}
		return fmt.Errorf("dnsprovider: insert zone: %w", err)
	}
	return nil
}

// getZone 读取单个根区;不存在 → ErrZoneNotFound。
func (s *Store) getZone(ctx context.Context, zoneID string) (*Zone, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, provider_id, base_domain, created_at FROM dns_provider_zones WHERE id = ?`, zoneID)
	z, err := scanZone(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrZoneNotFound
		}
		return nil, err
	}
	return z, nil
}

// listZones 返回全部根区,按 provider_id 分组(供 List/Get 装配)。
func (s *Store) listZones(ctx context.Context) (map[string][]Zone, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, provider_id, base_domain, created_at FROM dns_provider_zones ORDER BY base_domain`)
	if err != nil {
		return nil, fmt.Errorf("dnsprovider: list zones: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string][]Zone)
	for rows.Next() {
		z, err := scanZone(rows)
		if err != nil {
			return nil, err
		}
		out[z.ProviderID] = append(out[z.ProviderID], *z)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dnsprovider: iterate zones: %w", err)
	}
	return out, nil
}

// listZonesByProvider 返回某提供商的全部根区(按 base_domain 排序)。
func (s *Store) listZonesByProvider(ctx context.Context, providerID string) ([]Zone, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, provider_id, base_domain, created_at FROM dns_provider_zones
		 WHERE provider_id = ? ORDER BY base_domain`, providerID)
	if err != nil {
		return nil, fmt.Errorf("dnsprovider: list provider zones: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Zone, 0)
	for rows.Next() {
		z, err := scanZone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *z)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dnsprovider: iterate provider zones: %w", err)
	}
	return out, nil
}

// deleteZone 删除某提供商的某个根区(不属于该提供商 → ErrZoneNotFound)。
func (s *Store) deleteZone(ctx context.Context, providerID, zoneID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM dns_provider_zones WHERE id = ? AND provider_id = ?`, zoneID, providerID)
	if err != nil {
		return fmt.Errorf("dnsprovider: delete zone: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrZoneNotFound
	}
	return nil
}

// newProvider 据入参构造一条待插入提供商 + 其初始根区(id/时间戳在此填好;根区去重归一)。
func newProvider(in CreateInput) (*Provider, []Zone) {
	now := time.Now().UTC()
	p := &Provider{
		ID:           uuid.NewString(),
		Type:         in.Type,
		Name:         in.Name,
		APIID:        strings.TrimSpace(in.APIID),
		CredentialID: in.CredentialID,
		Zones:        nil,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	seen := make(map[string]struct{}, len(in.BaseDomains))
	zones := make([]Zone, 0, len(in.BaseDomains))
	for _, d := range in.BaseDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		zones = append(zones, Zone{ID: uuid.NewString(), ProviderID: p.ID, BaseDomain: d, CreatedAt: now})
	}
	p.Zones = zones
	return p, zones
}

// newZone 构造一条待插入根区。
func newZone(providerID, baseDomain string) *Zone {
	return &Zone{
		ID:         uuid.NewString(),
		ProviderID: providerID,
		BaseDomain: strings.ToLower(strings.TrimSpace(baseDomain)),
		CreatedAt:  time.Now().UTC(),
	}
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
	if err := sc.Scan(&p.ID, &p.Type, &p.Name, &p.APIID, &p.CredentialID, &createdStr, &updatedStr); err != nil {
		return nil, err
	}
	created, err := time.Parse(time.RFC3339, createdStr)
	if err != nil {
		return nil, fmt.Errorf("dnsprovider: parse created_at: %w", err)
	}
	updated, err := time.Parse(time.RFC3339, updatedStr)
	if err != nil {
		return nil, fmt.Errorf("dnsprovider: parse updated_at: %w", err)
	}
	p.CreatedAt = created
	p.UpdatedAt = updated
	return &p, nil
}

// scanZone 把一行扫描为 Zone。
func scanZone(sc scanner) (*Zone, error) {
	var (
		z          Zone
		createdStr string
	)
	if err := sc.Scan(&z.ID, &z.ProviderID, &z.BaseDomain, &createdStr); err != nil {
		return nil, err
	}
	created, err := time.Parse(time.RFC3339, createdStr)
	if err != nil {
		return nil, fmt.Errorf("dnsprovider: parse zone created_at: %w", err)
	}
	z.CreatedAt = created
	return &z, nil
}

// isConstraintErr 判定是否为唯一约束冲突(sqlite "constraint" / mysql 1062 duplicate key)。
func isConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "constraint") || strings.Contains(msg, "duplicate")
}
