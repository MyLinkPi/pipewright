// credentials.go 是临时凭据登记簿与凭据 API 令牌的存储实现(mesh.Service 新增方法的
// *service 落地)。设计约束:
//   - mesh_credentials 绝不存 credential_secret —— 私钥只在签发响应出现一次,事实
//     来源在各节点 easytier 的 --credential-file;本表只补备注/创建者/来源与撤销标记;
//   - mesh_api_tokens 只存 SHA-256 hex;明文令牌 pwmc_ 前缀 + 32 字节随机,创建响应
//     之外无处可取,撤销即失效(查询按"未撤销且哈希命中")。
package mesh

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CredentialRecord 是一条已签发凭据的登记行(过期/撤销的实时事实以 easytier
// credential list 为准,上层按 credential_id 合并)。
type CredentialRecord struct {
	CredentialID string `json:"credentialId"`
	Remark       string `json:"remark"`
	TTLSeconds   int64  `json:"ttlSeconds"`
	CreatedAt    string `json:"createdAt"`
	ExpiresAt    string `json:"expiresAt"`
	RevokedAt    string `json:"revokedAt"` // 空 = 未撤销(是否已过期看 ExpiresAt/活跃态)
	CreatedBy    string `json:"createdBy"`
	Source       string `json:"source"` // admin | token
}

// ApiToken 是一条凭据 API 令牌(哈希绝不外显)。
type ApiToken struct {
	Name       string `json:"name"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt"`
	RevokedAt  string `json:"revokedAt"` // 空 = 有效
}

// ---- 凭据登记簿 ----

func (s *service) IssueCredentialRecord(ctx context.Context, credentialID, remark string, ttlSeconds int64, actor, source string) (*CredentialRecord, error) {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" || ttlSeconds <= 0 {
		return nil, fmt.Errorf("%w: credential id/ttl 必填", ErrInvalidInput)
	}
	if len([]rune(remark)) > 200 {
		return nil, fmt.Errorf("%w: 备注超过 200 字", ErrInvalidInput)
	}
	now := time.Now().UTC()
	rec := &CredentialRecord{
		CredentialID: credentialID,
		Remark:       strings.TrimSpace(remark),
		TTLSeconds:   ttlSeconds,
		CreatedAt:    now.Format(time.RFC3339),
		ExpiresAt:    now.Add(time.Duration(ttlSeconds) * time.Second).Format(time.RFC3339),
		CreatedBy:    strings.TrimSpace(actor),
		Source:       strings.TrimSpace(source),
	}
	if rec.Source == "" {
		rec.Source = "admin"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mesh_credentials (credential_id, remark, ttl_seconds, created_at, expires_at, created_by, source)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.CredentialID, rec.Remark, rec.TTLSeconds, rec.CreatedAt, rec.ExpiresAt, rec.CreatedBy, rec.Source)
	if err != nil {
		// 同 ID 重复登记:幂等返回既有行(generate 对已存在 ID 返回既有凭据,登记同理)。
		if existing, gerr := s.credentialRecord(ctx, credentialID); gerr == nil {
			return existing, nil
		}
		return nil, fmt.Errorf("mesh: insert credential record: %w", err)
	}
	return rec, nil
}

func (s *service) ListCredentialRecords(ctx context.Context) ([]CredentialRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT credential_id, remark, ttl_seconds, created_at, expires_at, revoked_at, created_by, source
		 FROM mesh_credentials ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("mesh: list credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []CredentialRecord{}
	for rows.Next() {
		var rec CredentialRecord
		var revoked sql.NullString
		if err := rows.Scan(&rec.CredentialID, &rec.Remark, &rec.TTLSeconds,
			&rec.CreatedAt, &rec.ExpiresAt, &revoked, &rec.CreatedBy, &rec.Source); err != nil {
			return nil, err
		}
		if revoked.Valid {
			rec.RevokedAt = revoked.String
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *service) MarkCredentialRevoked(ctx context.Context, credentialID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE mesh_credentials SET revoked_at = ? WHERE credential_id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(credentialID))
	if err != nil {
		return fmt.Errorf("mesh: revoke credential: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 幂等:已撤销的再撤一次不算错;完全未知 → ErrNotFound(撤销的是已登记凭据)。
		if _, gerr := s.credentialRecord(ctx, credentialID); gerr != nil {
			return ErrNotFound
		}
	}
	return nil
}

func (s *service) credentialRecord(ctx context.Context, credentialID string) (*CredentialRecord, error) {
	var rec CredentialRecord
	var revoked sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT credential_id, remark, ttl_seconds, created_at, expires_at, revoked_at, created_by, source
		 FROM mesh_credentials WHERE credential_id = ?`, credentialID).
		Scan(&rec.CredentialID, &rec.Remark, &rec.TTLSeconds, &rec.CreatedAt, &rec.ExpiresAt, &revoked, &rec.CreatedBy, &rec.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mesh: get credential: %w", err)
	}
	if revoked.Valid {
		rec.RevokedAt = revoked.String
	}
	return &rec, nil
}

// ---- 凭据 API 令牌 ----

// apiTokenPlaintext 生成明文令牌:pwmc_ 前缀 + 32 字节随机数的 base64url(无填充,
// 43 字符)。前缀让令牌可被扫描器识别(泄露预警),随机量 256 位。
func apiTokenPlaintext() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "pwmc_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func (s *service) CreateApiToken(ctx context.Context, name string) (*ApiToken, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 {
		return nil, "", fmt.Errorf("%w: 令牌名必填且 ≤64 字", ErrInvalidInput)
	}
	plaintext, err := apiTokenPlaintext()
	if err != nil {
		return nil, "", fmt.Errorf("mesh: generate token: %w", err)
	}
	rec := &ApiToken{Name: name, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO mesh_api_tokens (name, token_hash, created_at) VALUES (?, ?, ?)`,
		name, hashToken(plaintext), rec.CreatedAt)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			return nil, "", fmt.Errorf("%w: 令牌名 %s 已存在", ErrDuplicate, name)
		}
		return nil, "", fmt.Errorf("mesh: insert api token: %w", err)
	}
	return rec, plaintext, nil
}

func (s *service) ListApiTokens(ctx context.Context) ([]ApiToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, created_at, last_used_at, revoked_at FROM mesh_api_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("mesh: list api tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []ApiToken{}
	for rows.Next() {
		var rec ApiToken
		var last, revoked sql.NullString
		if err := rows.Scan(&rec.Name, &rec.CreatedAt, &last, &revoked); err != nil {
			return nil, err
		}
		if last.Valid {
			rec.LastUsedAt = last.String
		}
		if revoked.Valid {
			rec.RevokedAt = revoked.String
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *service) RevokeApiToken(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE mesh_api_tokens SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(name))
	if err != nil {
		return fmt.Errorf("mesh: revoke api token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 已撤销/不存在:区分与否价值低,统一按未找到处理由上层展示(删除/撤销幂等)。
		return ErrNotFound
	}
	return nil
}

// ApiTokenLookup 校证明文:命中且未撤销 → 刷新 last_used_at(尽力而为)并返回令牌名。
func (s *service) ApiTokenLookup(ctx context.Context, plaintext string) string {
	plaintext = strings.TrimSpace(plaintext)
	if plaintext == "" {
		return ""
	}
	hash := hashToken(plaintext)
	var name string
	var revoked sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT name, revoked_at FROM mesh_api_tokens WHERE token_hash = ?`, hash).Scan(&name, &revoked)
	if err != nil || revoked.Valid {
		return ""
	}
	_, _ = s.db.ExecContext(ctx,
		`UPDATE mesh_api_tokens SET last_used_at = ? WHERE name = ?`,
		time.Now().UTC().Format(time.RFC3339), name)
	return name
}
