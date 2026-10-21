// Package meshdns 是「集群 DNS 映射」的领域层:把指定域名的 A 记录自动指向本机虚拟 IP。
//
// 背景:集群(EasyTier)下本机虚拟 IP 可为管理员手填或 DHCP 自动分配,IP 会变;把
// 好记的域名(如 home.example.com)钉到虚拟 IP 上,节点间互访就不必记 IP。本包管理
// 映射条目(域名 × DNS 提供商)并驱动同步:
//   - 条目 CRUD(表 mesh_dns_records;fqdn 全局唯一);
//   - Syncer 循环:虚拟 IP 变化 → 自动 upsert A 记录(DDNS 式跟随),周期兜底重同步
//     防云端记录被手工改动漂移,失败退避重试;手动「立即同步」同步执行并返回逐条结果。
//
// 与 DNS 提供商集成层(internal/dnsprovider)的关系:本包只依赖其 Service 的两个能力
// (Get 取提供商+根区、EnsureSubdomainRecord 幂等建/改 A 记录),经 ProviderSource 接口
// 注入(生产实现即 dnsprovider.Service);凭据/根区校验均由该层把守,本包不触凭据。
//
// AC-SEC:fqdn 须严格落在所选提供商某个根区下(服务端校验,防越权为任意域建记录);
// 写入 A 记录的 IP 只来自注入的 selfIP 提供器(运行态虚拟 IP),非用户自由文本。
package meshdns

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/huangchengsir/pipewright/internal/dnsprovider"
	"github.com/huangchengsir/pipewright/internal/store"
)

// 领域错误(人话由 httpapi 层映射状态码)。
var (
	// ErrNotFound 表示映射条目不存在。
	ErrNotFound = errors.New("meshdns: record not found")
	// ErrInvalidInput 表示入参非法(域名格式 / providerId 缺失等)。
	ErrInvalidInput = errors.New("meshdns: invalid input")
	// ErrDuplicate 表示域名已被其他条目占用(fqdn 全局唯一)。
	ErrDuplicate = errors.New("meshdns: duplicate fqdn")
	// ErrProviderNotFound 表示引用的 DNS 提供商不存在(可能已被删除)。
	ErrProviderNotFound = errors.New("meshdns: provider not found")
	// ErrZoneNotFound 表示域名未落在所选提供商的任何根区下。
	ErrZoneNotFound = errors.New("meshdns: fqdn not under any zone of provider")
)

// Record 是一条「域名 → 本机虚拟 IP」映射条目(含最近同步状态,由 Syncer 回写)。
type Record struct {
	ID           string `json:"id"`
	ProviderID   string `json:"providerId"`
	FQDN         string `json:"fqdn"`
	Enabled      bool   `json:"enabled"`
	LastIP       string `json:"lastIp"`       // 最近一次成功写入 A 记录的虚拟 IP
	LastSyncedAt string `json:"lastSyncedAt"` // 最近一次成功同步时间(RFC3339;空 = 从未成功)
	LastError    string `json:"lastError"`    // 最近一次同步失败的人话错误(空 = 无)
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
	// 以下为 List 回显字段(服务端关联提供商信息;ProviderGone=true 表示提供商已被删,
	// 该条目不会再被同步,应删除或换绑)。
	ProviderName string `json:"providerName"`
	ProviderType string `json:"providerType"`
	ProviderGone bool   `json:"providerGone"`
}

// CreateInput 是新增映射的入参。
type CreateInput struct {
	ProviderID string
	FQDN       string
	Enabled    bool
}

// UpdateInput 是编辑映射的入参;指针为 nil 表示不修改。
type UpdateInput struct {
	FQDN    *string
	Enabled *bool
}

// ProviderSource 抽象 DNS 提供商集成层的最小能力(生产实现 dnsprovider.Service;
// 注入便于单测,不触真网络)。凭据只经 EnsureSubdomainRecord 间接使用,绝不进入本包。
type ProviderSource interface {
	// Get 返回提供商(含根区;无 Secret);不存在 → dnsprovider.ErrNotFound。
	Get(ctx context.Context, providerID string) (*dnsprovider.Provider, error)
	// EnsureSubdomainRecord 为根区下 fqdn 建/改 A 记录指向 ip(幂等 upsert;
	// 严格子域校验由集成层把守)。
	EnsureSubdomainRecord(ctx context.Context, providerID, fqdn, ip string) error
}

// Service 定义集群 DNS 映射对外接口(httpapi 消费;Syncer 复用 List/Mark*)。
type Service interface {
	// List 返回全部映射(创建时间正序;关联提供商名称/类型回显)。
	List(ctx context.Context) ([]Record, error)
	// Create 新增映射(校验提供商存在 + 域名严格落在其某根区下 + fqdn 全局唯一)。
	Create(ctx context.Context, in CreateInput) (*Record, error)
	// Update 编辑域名/启用位(域名变更重新过根区覆盖校验)。
	Update(ctx context.Context, id string, in UpdateInput) (*Record, error)
	// Delete 删除映射(仅本地条目;云端 DNS 记录保留,由管理员自行处理)。
	Delete(ctx context.Context, id string) error

	// MarkSynced / MarkSyncFailed 由 Syncer 回写单条同步结果(其余字段不动)。
	MarkSynced(ctx context.Context, id, ip string) error
	MarkSyncFailed(ctx context.Context, id, errMsg string) error
}

// service 是 store + ProviderSource 支撑的 Service 实现。
type service struct {
	store *Store
	src   ProviderSource
}

// New 构造 Service。src 为 DNS 提供商集成层(dnsprovider.Service)。
func New(db *sql.DB, src ProviderSource) Service {
	return &service{store: NewStore(db), src: src}
}

func (s *service) List(ctx context.Context) ([]Record, error) {
	records, err := s.store.list(ctx)
	if err != nil {
		return nil, err
	}
	// 关联回显提供商名称/类型(条目量小;同提供商只查一次)。提供商已删 → ProviderGone。
	cache := map[string]*providerView{}
	for i := range records {
		v, ok := cache[records[i].ProviderID]
		if !ok {
			v = s.providerView(ctx, records[i].ProviderID)
			cache[records[i].ProviderID] = v
		}
		records[i].ProviderName, records[i].ProviderType, records[i].ProviderGone = v.name, v.typ, v.gone
	}
	return records, nil
}

// providerView 是回显用的提供商快照(gone=true 表示已删)。
type providerView struct {
	name string
	typ  string
	gone bool
}

func (s *service) providerView(ctx context.Context, providerID string) *providerView {
	p, err := s.src.Get(ctx, providerID)
	if err != nil {
		return &providerView{gone: true}
	}
	return &providerView{name: p.Name, typ: p.Type}
}

func (s *service) Create(ctx context.Context, in CreateInput) (*Record, error) {
	fqdn := normalizeFQDN(in.FQDN)
	if strings.TrimSpace(in.ProviderID) == "" || !validFQDN(fqdn) {
		return nil, fmt.Errorf("%w:providerId 与合法域名必填", ErrInvalidInput)
	}
	if err := s.validateCoverage(ctx, in.ProviderID, fqdn); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	r := Record{
		ID: uuid.NewString(), ProviderID: in.ProviderID, FQDN: fqdn,
		Enabled: in.Enabled, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.insert(ctx, r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *service) Update(ctx context.Context, id string, in UpdateInput) (*Record, error) {
	cur, err := s.store.get(ctx, id)
	if err != nil {
		return nil, err
	}
	fqdn := cur.FQDN
	if in.FQDN != nil {
		fqdn = normalizeFQDN(*in.FQDN)
		if !validFQDN(fqdn) {
			return nil, fmt.Errorf("%w:域名格式非法", ErrInvalidInput)
		}
	}
	if in.FQDN != nil && fqdn != cur.FQDN {
		if err := s.validateCoverage(ctx, cur.ProviderID, fqdn); err != nil {
			return nil, err
		}
	}
	enabled := cur.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	updated, err := s.store.update(ctx, id, fqdn, enabled)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *service) Delete(ctx context.Context, id string) error { return s.store.del(ctx, id) }

func (s *service) MarkSynced(ctx context.Context, id, ip string) error {
	return s.store.markSynced(ctx, id, ip)
}

func (s *service) MarkSyncFailed(ctx context.Context, id, errMsg string) error {
	return s.store.markSyncFailed(ctx, id, errMsg)
}

// validateCoverage 校验提供商存在且 fqdn 严格落在其某个根区下(等于根区不算;
// 最长后缀优先,与 dnsprovider.AllocateFQDN 同规)。
func (s *service) validateCoverage(ctx context.Context, providerID, fqdn string) error {
	p, err := s.src.Get(ctx, providerID)
	if err != nil {
		if errors.Is(err, dnsprovider.ErrNotFound) {
			return ErrProviderNotFound
		}
		return err
	}
	if coveringZone(p.Zones, fqdn) == nil {
		return fmt.Errorf("%w:%s 须是 %s 某个根区下的子域名", ErrZoneNotFound, fqdn, p.Name)
	}
	return nil
}

// coveringZone 返回覆盖 fqdn 的根区(严格子域:fqdn == base 不算;最长后缀优先);无 → nil。
func coveringZone(zones []dnsprovider.Zone, fqdn string) *dnsprovider.Zone {
	var best *dnsprovider.Zone
	for i := range zones {
		z := zones[i]
		if fqdn != z.BaseDomain && strings.HasSuffix(fqdn, "."+z.BaseDomain) {
			if best == nil || len(z.BaseDomain) > len(best.BaseDomain) {
				best = &zones[i]
			}
		}
	}
	return best
}

// fqdnRe 校验普通域名 FQDN(每段字母数字/连字符,≥2 段;不接受裸 IP/端口/通配符)。
var fqdnRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func normalizeFQDN(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func validFQDN(s string) bool {
	return s != "" && len(s) <= 253 && fqdnRe.MatchString(s)
}

// ---- store(SQL CRUD;方言无关,唯一约束冲突经 store.IsUniqueErr 归一)--------

// Store 是 mesh_dns_records 表的 SQL 持久层。
type Store struct {
	DB *sql.DB
}

// NewStore 构造 Store。
func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

func (st *Store) list(ctx context.Context) ([]Record, error) {
	rows, err := st.DB.QueryContext(ctx, `SELECT id, provider_id, fqdn, enabled, last_ip,
		last_synced_at, last_error, created_at, updated_at FROM mesh_dns_records ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("meshdns: list: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var enabled int
		if err := rows.Scan(&r.ID, &r.ProviderID, &r.FQDN, &enabled, &r.LastIP,
			&r.LastSyncedAt, &r.LastError, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("meshdns: scan: %w", err)
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func (st *Store) get(ctx context.Context, id string) (*Record, error) {
	row := st.DB.QueryRowContext(ctx, `SELECT id, provider_id, fqdn, enabled, last_ip,
		last_synced_at, last_error, created_at, updated_at FROM mesh_dns_records WHERE id = ?`, id)
	var r Record
	var enabled int
	err := row.Scan(&r.ID, &r.ProviderID, &r.FQDN, &enabled, &r.LastIP,
		&r.LastSyncedAt, &r.LastError, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("meshdns: get: %w", err)
	}
	r.Enabled = enabled != 0
	return &r, nil
}

func (st *Store) insert(ctx context.Context, r Record) error {
	_, err := st.DB.ExecContext(ctx, `INSERT INTO mesh_dns_records
		(id, provider_id, fqdn, enabled, last_ip, last_synced_at, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, '', '', '', ?, ?)`,
		r.ID, r.ProviderID, r.FQDN, boolToInt(r.Enabled), r.CreatedAt, r.UpdatedAt)
	if err != nil {
		if store.IsUniqueErr(err) {
			return fmt.Errorf("%w:%s", ErrDuplicate, r.FQDN)
		}
		return fmt.Errorf("meshdns: insert: %w", err)
	}
	return nil
}

func (st *Store) update(ctx context.Context, id, fqdn string, enabled bool) (*Record, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := st.DB.ExecContext(ctx,
		`UPDATE mesh_dns_records SET fqdn = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		fqdn, boolToInt(enabled), now, id)
	if err != nil {
		if store.IsUniqueErr(err) {
			return nil, fmt.Errorf("%w:%s", ErrDuplicate, fqdn)
		}
		return nil, fmt.Errorf("meshdns: update: %w", err)
	}
	return st.get(ctx, id)
}

func (st *Store) del(ctx context.Context, id string) error {
	res, err := st.DB.ExecContext(ctx, `DELETE FROM mesh_dns_records WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("meshdns: delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// markSynced 回写一次成功同步:last_ip / last_synced_at 置新值,清 last_error。
func (st *Store) markSynced(ctx context.Context, id, ip string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := st.DB.ExecContext(ctx,
		`UPDATE mesh_dns_records SET last_ip = ?, last_synced_at = ?, last_error = '' WHERE id = ?`,
		ip, now, id)
	if err != nil {
		return fmt.Errorf("meshdns: mark synced: %w", err)
	}
	return nil
}

// markSyncFailed 回写一次失败同步:只置 last_error(保留 last_ip/last_synced_at 的
// 最近成功值,UI 语义才是「上次成功」)。截断到列宽,防超长 API 错误撑爆存储。
func (st *Store) markSyncFailed(ctx context.Context, id, errMsg string) error {
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	_, err := st.DB.ExecContext(ctx,
		`UPDATE mesh_dns_records SET last_error = ? WHERE id = ?`, errMsg, id)
	if err != nil {
		return fmt.Errorf("meshdns: mark failed: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
