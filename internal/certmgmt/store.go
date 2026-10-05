package certmgmt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
)

// Store 持久化证书管理(参数化 SQL,sqlite/mysql 两方言一致;仿 servicereg.Store 手法)。
type Store struct {
	db *sql.DB
}

// NewStore 构造持久层。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// certCols 是证书行的统一 SELECT 列(HasPEM 由密文长度推导)。
const certCols = `SELECT id, primary_domain, domains, source, ca, validation, dns_provider_id, key_type,
       auto_renew, status, status_detail,
       cert_pem_sealed IS NOT NULL AND LENGTH(cert_pem_sealed) > 0,
       subject, issuer, not_before, not_after, last_issued_at, last_attempt_at, created_at, updated_at
FROM certificates`

// insert 落库证书行(无密文,签发中);primary_domain 唯一冲突 → ErrDomainTaken。
func (s *Store) insert(ctx context.Context, c *Certificate) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO certificates
		   (id, primary_domain, domains, source, ca, validation, dns_provider_id, key_type,
		    auto_renew, status, status_detail, subject, issuer, not_before, not_after,
		    last_issued_at, last_attempt_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.PrimaryDomain, joinDomains(c.Domains), c.Source, c.CA, c.Validation, c.DNSProviderID,
		c.KeyType, boolInt(c.AutoRenew), c.Status, c.StatusDetail, c.Subject, c.Issuer,
		fmtTime(c.NotBefore), fmtTime(c.NotAfter), fmtTime(c.LastIssuedAt), fmtTime(c.LastAttemptAt),
		fmtTime(c.CreatedAt), fmtTime(c.UpdatedAt),
	)
	if err != nil {
		if store.IsUniqueErr(err) {
			return ErrDomainTaken
		}
		return fmt.Errorf("certmgmt: insert certificate: %w", err)
	}
	return nil
}

// insertWithPEM 落库证书行 + 密文对(导入路径);唯一冲突 → ErrDomainTaken。
func (s *Store) insertWithPEM(ctx context.Context, c *Certificate, sealedCert, sealedKey []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO certificates
		   (id, primary_domain, domains, source, ca, validation, dns_provider_id, key_type,
		    auto_renew, status, status_detail, cert_pem_sealed, key_pem_sealed,
		    subject, issuer, not_before, not_after, last_issued_at, last_attempt_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.PrimaryDomain, joinDomains(c.Domains), c.Source, c.CA, c.Validation, c.DNSProviderID,
		c.KeyType, boolInt(c.AutoRenew), c.Status, c.StatusDetail, sealedCert, sealedKey,
		c.Subject, c.Issuer, fmtTime(c.NotBefore), fmtTime(c.NotAfter),
		fmtTime(c.LastIssuedAt), fmtTime(c.LastAttemptAt), fmtTime(c.CreatedAt), fmtTime(c.UpdatedAt),
	)
	if err != nil {
		if store.IsUniqueErr(err) {
			return ErrDomainTaken
		}
		return fmt.Errorf("certmgmt: insert certificate: %w", err)
	}
	return nil
}

// get 读取单张证书;不存在 → ErrNotFound。
func (s *Store) get(ctx context.Context, id string) (*Certificate, error) {
	row := s.db.QueryRowContext(ctx, certCols+` WHERE id = ?`, id)
	c, err := scanCert(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return c, nil
}

// list 返回全部证书(创建时间倒序)。
func (s *Store) list(ctx context.Context) ([]Certificate, error) {
	rows, err := s.db.QueryContext(ctx, certCols+` ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("certmgmt: list certificates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Certificate, 0)
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// getSealed 返回密文对(签发回读/下发/删除比对用);无密文 → ok=false。
func (s *Store) getSealed(ctx context.Context, id string) (cert, key []byte, ok bool, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT cert_pem_sealed, key_pem_sealed FROM certificates WHERE id = ?`, id)
	if err := row.Scan(&cert, &key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, false, ErrNotFound
		}
		return nil, nil, false, fmt.Errorf("certmgmt: read sealed: %w", err)
	}
	if len(cert) == 0 || len(key) == 0 {
		return nil, nil, false, nil
	}
	return cert, key, true, nil
}

// markAttempt 置进行中:status=pending + 阶段话术 + last_attempt_at=now。
func (s *Store) markAttempt(ctx context.Context, id, phase string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE certificates SET status = ?, status_detail = ?, last_attempt_at = ?, updated_at = ? WHERE id = ?`,
		StatusPending, phase, now, now, id)
	if err != nil {
		return fmt.Errorf("certmgmt: mark attempt: %w", err)
	}
	return nil
}

// markFailed 回写失败(status 保持密文可用性不动:issued+failed 时密文仍在,仅记录原因;
// 统一置 failed,HasPEM 列独立存在,前端「已签发过但最近失败」仍可见到期信息)。
func (s *Store) markFailed(ctx context.Context, id, detail string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE certificates SET status = ?, status_detail = ?, updated_at = ? WHERE id = ?`,
		StatusFailed, truncate(detail, 2000), now, id)
	if err != nil {
		return fmt.Errorf("certmgmt: mark failed: %w", err)
	}
	return nil
}

// markIssued 回写成功:密文对 + 叶子元数据 + last_issued_at。
func (s *Store) markIssued(ctx context.Context, id string, sealedCert, sealedKey []byte, m certMeta, detail string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE certificates
		 SET cert_pem_sealed = ?, key_pem_sealed = ?, subject = ?, issuer = ?,
		     not_before = ?, not_after = ?, last_issued_at = ?,
		     status = ?, status_detail = ?, updated_at = ?
		 WHERE id = ?`,
		sealedCert, sealedKey, m.subject, m.issuer,
		m.notBefore.Format(time.RFC3339), m.notAfter.Format(time.RFC3339), now,
		StatusIssued, detail, now, id,
	)
	if err != nil {
		return fmt.Errorf("certmgmt: mark issued: %w", err)
	}
	return nil
}

// setStatusDetail 仅覆盖详情话术(导入/手动下发的提示)。
func (s *Store) setStatusDetail(ctx context.Context, id, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE certificates SET status_detail = ?, updated_at = ? WHERE id = ?`,
		truncate(detail, 2000), time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("certmgmt: set detail: %w", err)
	}
	return nil
}

// appendStatusDetail 在现有详情后追加(签发成功 + 下发提示两段人话)。
func (s *Store) appendStatusDetail(ctx context.Context, id, extra string) error {
	cur, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	detail := truncate(cur.StatusDetail+" "+extra, 2000)
	return s.setStatusDetail(ctx, id, strings.TrimSpace(detail))
}

// setAutoRenew 开/关自动续期;行不存在 → ErrNotFound。
func (s *Store) setAutoRenew(ctx context.Context, id string, on bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE certificates SET auto_renew = ?, updated_at = ? WHERE id = ?`,
		boolInt(on), time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("certmgmt: set auto renew: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// delete 删除证书行;不存在 → ErrNotFound。
func (s *Store) delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM certificates WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("certmgmt: delete certificate: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanCert 行扫描(scanner 抽象 *sql.Row 与 *sql.Rows)。
func scanCert(sc scanner) (*Certificate, error) {
	var (
		c          Certificate
		domains    string
		autoRenew  int
		notBefore  sql.NullString
		notAfter   sql.NullString
		lastIssued sql.NullString
		lastTry    sql.NullString
		createdS   string
		updatedS   string
	)
	if err := sc.Scan(
		&c.ID, &c.PrimaryDomain, &domains, &c.Source, &c.CA, &c.Validation, &c.DNSProviderID,
		&c.KeyType, &autoRenew, &c.Status, &c.StatusDetail, &c.HasPEM,
		&c.Subject, &c.Issuer, &notBefore, &notAfter, &lastIssued, &lastTry, &createdS, &updatedS,
	); err != nil {
		return nil, err
	}
	c.Domains = splitDomains(domains)
	c.AutoRenew = autoRenew != 0
	c.NotBefore = parseTime(notBefore.String)
	c.NotAfter = parseTime(notAfter.String)
	c.LastIssuedAt = parseTime(lastIssued.String)
	c.LastAttemptAt = parseTime(lastTry.String)
	c.CreatedAt = parseTime(createdS)
	c.UpdatedAt = parseTime(updatedS)
	return &c, nil
}

// scanner 抽象 *sql.Row 与 *sql.Rows 的 Scan(与 servicereg 同手法)。
type scanner interface {
	Scan(dest ...any) error
}

// 回填行 status_detail 标记(copies 识别用):old 为历史版本遗留文案,new 为当前版本。
const (
	backfillDetail    = "迁移自服务注册基域的历史证书"
	backfillDetailOld = "由服务注册页历史导入"
)

// BackfillFromServiceReg 把 service_reg_domains 里的存量证书一次性回填进 certificates
// (source=manual,密文原样搬移,domains 只记基域;幂等:按 primary_domain 跳过已存在)。
// 返回回填条数。
//
// 覆盖跳过是关键:重构后该表的证书列只由 CertSink(证书管理侧签发/导入后同步)写入,
// 凡基域已被证书库任一证书覆盖(SAN 精确等于基域,或 *.基域 泛域名覆盖,与下发判定
// domainCoversAny 同语义),该行必是库内证书的同步副本 —— 回填只会得到一张永不续期的
// 冻结副本。只有库内无任何覆盖的孤儿证书(证书管理上线前的历史手动遗留)才回填兜底可见。
// best-effort:单行失败跳过,不让回填阻断启动。
func (s *Store) BackfillFromServiceReg(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT base_domain, cert_pem_sealed, key_pem_sealed, cert_subject, cert_expires_at, created_at
		 FROM service_reg_domains
		 WHERE cert_pem_sealed IS NOT NULL AND LENGTH(cert_pem_sealed) > 0`)
	if err != nil {
		return 0, fmt.Errorf("certmgmt: backfill source: %w", err)
	}
	type src struct {
		base, subject, expires, created string
		certSealed, keySealed           []byte
	}
	var srcs []src
	for rows.Next() {
		var v src
		if err := rows.Scan(&v.base, &v.certSealed, &v.keySealed, &v.subject, &v.expires, &v.created); err != nil {
			_ = rows.Close()
			return 0, err
		}
		srcs = append(srcs, v)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	lib, err := s.list(ctx)
	if err != nil {
		return 0, fmt.Errorf("certmgmt: backfill lib list: %w", err)
	}
	coveredByLib := func(base string) bool {
		for i := range lib {
			if domainCoversAny(append(lib[i].Domains, lib[i].PrimaryDomain), base) {
				return true
			}
		}
		return false
	}
	n := 0
	for _, v := range srcs {
		base := strings.ToLower(strings.TrimSpace(v.base))
		if base == "" || coveredByLib(base) {
			continue
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if _, ierr := s.db.ExecContext(ctx,
			`INSERT INTO certificates
			   (id, primary_domain, domains, source, validation, auto_renew, status, status_detail,
			    cert_pem_sealed, key_pem_sealed, subject, not_after, last_issued_at, created_at, updated_at)
			 VALUES (?, ?, ?, 'manual', 'manual', 0, 'issued', ?,
			         ?, ?, ?, ?, ?, ?, ?)`,
			newID(), base, base, backfillDetail, v.certSealed, v.keySealed, truncate(v.subject, 1000), v.expires,
			v.created, coalesceTime(v.created), now,
		); ierr != nil {
			continue // 唯一冲突(已回填过)或单行失败:跳过,不阻断
		}
		n++
	}
	return n, nil
}

// joinDomains / splitDomains:domains 列的逗号编解码。
func joinDomains(ds []string) string { return strings.Join(ds, ",") }
func splitDomains(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// boolInt 是 bool → INTEGER 的存储映射。
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fmtTime 时间 → RFC3339 存储(零值给空串)。
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseTime RFC3339 存储 → 时间(失败给零值,不炸列表;与 servicereg 同手法)。
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// coalesceTime 空串回退当前时间(回填行 created_at 兜底)。
func coalesceTime(s string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return time.Now().UTC().Format(time.RFC3339)
}
