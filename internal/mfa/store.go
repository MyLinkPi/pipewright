package mfa

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/huangchengsir/pipewright/internal/store"
)

func encodeBase64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
func decodeBase64Std(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// store 封装 mfa 四张表的直接 SQL 访问(时间串一律 RFC3339 UTC,与全库一致)。
type mfaStore struct {
	db      *sql.DB
	dialect store.Dialect
}

// ---- TOTP -----------------------------------------------------------------

// totpCredential 是 totp_credentials 行视图。
type totpCredential struct {
	SecretEnc string
	Enabled   bool
}

// getTOTP 读取 owner 的 TOTP 配置;无行 → ErrTOTPNotSetup。
func (st *mfaStore) getTOTP(owner string) (*totpCredential, error) {
	var c totpCredential
	var enabled int
	err := st.db.QueryRow(
		`SELECT secret_enc, enabled FROM totp_credentials WHERE owner_key = ?`, owner,
	).Scan(&c.SecretEnc, &enabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTOTPNotSetup
		}
		return nil, fmt.Errorf("mfa: get totp: %w", err)
	}
	c.Enabled = enabled != 0
	return &c, nil
}

// upsertTOTP 写入 owner 的 TOTP 密钥(重置 setup:enabled 归 0、备份码全量替换)。
// upsert 方言尾句经 store.UpsertAssignSuffix 生成(SQLite ON CONFLICT / MySQL ON DUPLICATE)。
func (st *mfaStore) upsertTOTP(owner, secretEnc string, backupHashes []string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	suffix := store.UpsertAssignSuffix(st.dialect, []string{"owner_key"}, []string{
		"secret_enc = " + store.Excluded(st.dialect, "secret_enc"),
		"enabled = 0",
		"updated_at = " + store.Excluded(st.dialect, "updated_at"),
	})
	tx, err := st.db.Begin()
	if err != nil {
		return fmt.Errorf("mfa: upsert totp begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT INTO totp_credentials (owner_key, secret_enc, enabled, created_at, updated_at)
		 VALUES (?, ?, 0, ?, ?) `+suffix,
		owner, secretEnc, now, now,
	); err != nil {
		return fmt.Errorf("mfa: upsert totp: %w", err)
	}
	if err := replaceBackupCodesTx(tx, owner, backupHashes, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mfa: upsert totp commit: %w", err)
	}
	return nil
}

// setTOTPEnabled 置启用位(开启二步验证)。
func (st *mfaStore) setTOTPEnabled(owner string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := st.db.Exec(
		`UPDATE totp_credentials SET enabled = ?, updated_at = ? WHERE owner_key = ?`,
		v, time.Now().UTC().Format(time.RFC3339), owner,
	)
	if err != nil {
		return fmt.Errorf("mfa: set totp enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTOTPNotSetup
	}
	return nil
}

// deleteTOTP 删除 owner 的 TOTP 配置与全部备份码(关闭二步验证/管理员重置)。
func (st *mfaStore) deleteTOTP(owner string) error {
	if _, err := st.db.Exec(`DELETE FROM totp_credentials WHERE owner_key = ?`, owner); err != nil {
		return fmt.Errorf("mfa: delete totp: %w", err)
	}
	if _, err := st.db.Exec(`DELETE FROM totp_backup_codes WHERE owner_key = ?`, owner); err != nil {
		return fmt.Errorf("mfa: delete backup codes: %w", err)
	}
	return nil
}

// ---- 备份码 ----------------------------------------------------------------

// backupCode 是未使用备份码的行视图(仅哈希,无明文)。
type backupCode struct {
	ID   int64
	Hash string
}

// unusedBackupCodes 列出 owner 全部未使用的备份码哈希。
func (st *mfaStore) unusedBackupCodes(owner string) ([]backupCode, error) {
	rows, err := st.db.Query(
		`SELECT id, code_hash FROM totp_backup_codes WHERE owner_key = ? AND used_at IS NULL`, owner,
	)
	if err != nil {
		return nil, fmt.Errorf("mfa: list backup codes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []backupCode
	for rows.Next() {
		var c backupCode
		if err := rows.Scan(&c.ID, &c.Hash); err != nil {
			return nil, fmt.Errorf("mfa: scan backup code: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// markBackupCodeUsed 标记备份码已用(一次性)。
func (st *mfaStore) markBackupCodeUsed(id int64) error {
	_, err := st.db.Exec(
		`UPDATE totp_backup_codes SET used_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id,
	)
	if err != nil {
		return fmt.Errorf("mfa: mark backup code used: %w", err)
	}
	return nil
}

// backupCodesRemaining 统计剩余可用备份码数。
func (st *mfaStore) backupCodesRemaining(owner string) int {
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(1) FROM totp_backup_codes WHERE owner_key = ? AND used_at IS NULL`, owner,
	).Scan(&n); err != nil {
		return 0
	}
	return n
}

// replaceBackupCodes 全量替换 owner 的备份码(事务内)。
func (st *mfaStore) replaceBackupCodes(owner string, hashes []string) error {
	tx, err := st.db.Begin()
	if err != nil {
		return fmt.Errorf("mfa: replace backup codes begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := replaceBackupCodesTx(tx, owner, hashes, now); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceBackupCodesTx(tx *sql.Tx, owner string, hashes []string, now string) error {
	if _, err := tx.Exec(`DELETE FROM totp_backup_codes WHERE owner_key = ?`, owner); err != nil {
		return fmt.Errorf("mfa: clear backup codes: %w", err)
	}
	for _, h := range hashes {
		if _, err := tx.Exec(
			`INSERT INTO totp_backup_codes (owner_key, code_hash, used_at, created_at) VALUES (?, ?, NULL, ?)`,
			owner, h, now,
		); err != nil {
			return fmt.Errorf("mfa: insert backup code: %w", err)
		}
	}
	return nil
}

// ---- Passkey 凭据 ------------------------------------------------------------

// Passkey 是 webauthn_credentials 行的领域视图(不含公钥原文,公钥仅在校验时内部加载)。
type Passkey struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt"`
}

// passkeyRow 是完整行(含验证所需的二进制字段,base64 串)。
type passkeyRow struct {
	ID             int64
	CredentialID   string // base64 std
	OwnerKey       string
	Name           string
	PublicKey      string // base64 std
	AttestationType string
	AAGUID         string // base64 std
	SignCount      uint32
	Transports     string // JSON 数组串
	CloneWarning   bool
	UserPresent    bool
	UserVerified   bool
	BackupEligible bool
	BackupState    bool
	Attachment     string
	LastUsedAt     *time.Time
	CreatedAt      time.Time
}

const passkeyCols = `id, credential_id, owner_key, name, public_key, attestation_type, aaguid,
	sign_count, transports, clone_warning, user_present, user_verified, backup_eligible,
	backup_state, attachment, last_used_at, created_at`

func scanPasskey(sc interface{ Scan(...any) error }) (*passkeyRow, error) {
	var r passkeyRow
	var cw, up, uv, be, bs int
	var lastUsed sql.NullString
	var createdStr string
	if err := sc.Scan(&r.ID, &r.CredentialID, &r.OwnerKey, &r.Name, &r.PublicKey,
		&r.AttestationType, &r.AAGUID, &r.SignCount, &r.Transports,
		&cw, &up, &uv, &be, &bs, &r.Attachment, &lastUsed, &createdStr); err != nil {
		return nil, err
	}
	r.CloneWarning, r.UserPresent, r.UserVerified = cw != 0, up != 0, uv != 0
	r.BackupEligible, r.BackupState = be != 0, bs != 0
	if createdStr != "" {
		if t, err := time.Parse(time.RFC3339, createdStr); err == nil {
			r.CreatedAt = t
		}
	}
	if lastUsed.Valid && lastUsed.String != "" {
		if t, err := time.Parse(time.RFC3339, lastUsed.String); err == nil {
			r.LastUsedAt = &t
		}
	}
	return &r, nil
}

// listPasskeyRows 列出 owner 的全部凭据(注册排除列表 / FinishRegistration 用户视图)。
func (st *mfaStore) listPasskeyRows(owner string) ([]*passkeyRow, error) {
	rows, err := st.db.Query(
		`SELECT `+passkeyCols+` FROM webauthn_credentials WHERE owner_key = ? ORDER BY created_at ASC, id ASC`, owner)
	if err != nil {
		return nil, fmt.Errorf("mfa: list passkeys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*passkeyRow
	for rows.Next() {
		r, err := scanPasskey(rows)
		if err != nil {
			return nil, fmt.Errorf("mfa: scan passkey: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// getPasskeyByCredentialID 按 WebAuthn credential ID(base64 std 编码后)反查凭据;
// discoverable 登录路径。无行返回 ErrPasskeyNotFound。
func (st *mfaStore) getPasskeyByCredentialID(credentialID []byte) (*passkeyRow, error) {
	r, err := scanPasskey(st.db.QueryRow(
		`SELECT `+passkeyCols+` FROM webauthn_credentials WHERE credential_id = ?`,
		base64Std(credentialID),
	))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPasskeyNotFound
		}
		return nil, fmt.Errorf("mfa: get passkey by credential id: %w", err)
	}
	return r, nil
}

// insertPasskey 插入新凭据,返回自增 id。
func (st *mfaStore) insertPasskey(r *passkeyRow) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := st.db.Exec(
		`INSERT INTO webauthn_credentials
		 (credential_id, owner_key, name, public_key, attestation_type, aaguid, sign_count,
		  transports, clone_warning, user_present, user_verified, backup_eligible, backup_state,
		  attachment, last_used_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
		r.CredentialID, r.OwnerKey, r.Name, r.PublicKey, r.AttestationType, r.AAGUID,
		r.SignCount, r.Transports, boolInt(r.CloneWarning), boolInt(r.UserPresent),
		boolInt(r.UserVerified), boolInt(r.BackupEligible), boolInt(r.BackupState),
		r.Attachment, now,
	)
	if err != nil {
		return 0, fmt.Errorf("mfa: insert passkey: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// updatePasskeyVerification 登录成功后回写签名计数/克隆告警/最近使用时间。
func (st *mfaStore) updatePasskeyVerification(id int64, signCount uint32, cloneWarning bool) error {
	_, err := st.db.Exec(
		`UPDATE webauthn_credentials
		 SET sign_count = ?, clone_warning = ?, last_used_at = ? WHERE id = ?`,
		signCount, boolInt(cloneWarning), time.Now().UTC().Format(time.RFC3339), id,
	)
	if err != nil {
		return fmt.Errorf("mfa: update passkey verification: %w", err)
	}
	return nil
}

// deletePasskey 删除 owner 名下指定 id 的凭据;无权或不存在 → ErrPasskeyNotFound。
func (st *mfaStore) deletePasskey(owner string, id int64) error {
	res, err := st.db.Exec(
		`DELETE FROM webauthn_credentials WHERE id = ? AND owner_key = ?`, id, owner)
	if err != nil {
		return fmt.Errorf("mfa: delete passkey: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPasskeyNotFound
	}
	return nil
}

// deletePasskeysByOwner 删除 owner 全部凭据(管理员重置),返回删除数。
func (st *mfaStore) deletePasskeysByOwner(owner string) (int64, error) {
	res, err := st.db.Exec(`DELETE FROM webauthn_credentials WHERE owner_key = ?`, owner)
	if err != nil {
		return 0, fmt.Errorf("mfa: delete passkeys by owner: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// anyPasskeyExists 报告实例内是否至少注册了一把通行密钥(登录页按钮显隐)。
func (st *mfaStore) anyPasskeyExists() bool {
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(1) FROM webauthn_credentials`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// countPasskeysByOwner 统计 owner 的凭据数(用户列表展示)。
func (st *mfaStore) countPasskeysByOwner(owner string) int {
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(1) FROM webauthn_credentials WHERE owner_key = ?`, owner).Scan(&n); err != nil {
		return 0
	}
	return n
}

// ---- webauthn.Credential 转换 -------------------------------------------------

func base64Std(b []byte) string { return encodeBase64Std(b) }

// toWebAuthnCredential 把库行转回 go-webauthn 凭据(登录断言校验用)。
func (r *passkeyRow) toWebAuthnCredential() webauthn.Credential {
	credID, _ := decodeBase64Std(r.CredentialID)
	pubKey, _ := decodeBase64Std(r.PublicKey)
	aaguid, _ := decodeBase64Std(r.AAGUID)
	var transports []protocol.AuthenticatorTransport
	if strings.TrimSpace(r.Transports) != "" {
		var list []string
		if json.Unmarshal([]byte(r.Transports), &list) == nil {
			transports = make([]protocol.AuthenticatorTransport, 0, len(list))
			for _, t := range list {
				transports = append(transports, protocol.AuthenticatorTransport(t))
			}
		}
	}
	return webauthn.Credential{
		ID:              credID,
		PublicKey:       pubKey,
		AttestationType: r.AttestationType,
		Transport:       transports,
		Flags: webauthn.CredentialFlags{
			UserPresent:    r.UserPresent,
			UserVerified:   r.UserVerified,
			BackupEligible: r.BackupEligible,
			BackupState:    r.BackupState,
		},
		Authenticator: webauthn.Authenticator{
			AAGUID:       aaguid,
			SignCount:    r.SignCount,
			CloneWarning: r.CloneWarning,
			Attachment:   protocol.AuthenticatorAttachment(r.Attachment),
		},
	}
}

// fromWebAuthnCredential 把注册完成的新凭据转为库行。
func fromWebAuthnCredential(owner, name string, c *webauthn.Credential) *passkeyRow {
	var transportsJSON string
	if len(c.Transport) > 0 {
		list := make([]string, 0, len(c.Transport))
		for _, t := range c.Transport {
			list = append(list, string(t))
		}
		if b, err := json.Marshal(list); err == nil {
			transportsJSON = string(b)
		}
	}
	return &passkeyRow{
		CredentialID:    base64Std(c.ID),
		OwnerKey:        owner,
		Name:            name,
		PublicKey:       base64Std(c.PublicKey),
		AttestationType: c.AttestationType,
		AAGUID:          base64Std(c.Authenticator.AAGUID),
		SignCount:       c.Authenticator.SignCount,
		Transports:      transportsJSON,
		CloneWarning:    c.Authenticator.CloneWarning,
		UserPresent:     c.Flags.UserPresent,
		UserVerified:    c.Flags.UserVerified,
		BackupEligible:  c.Flags.BackupEligible,
		BackupState:     c.Flags.BackupState,
		Attachment:      string(c.Authenticator.Attachment),
	}
}

// ---- 挑战暂存 ------------------------------------------------------------------

// challengeKind 挑战用途。
const (
	challengeRegister = "register"
	challengeLogin    = "login"
)

// saveChallenge 存入一次性挑战(go-webauthn SessionData JSON),返回随机 id。
func (st *mfaStore) saveChallenge(kind string, data []byte, ttl time.Duration) (string, error) {
	id, err := randHex(32)
	if err != nil {
		return "", fmt.Errorf("mfa: generate challenge id: %w", err)
	}
	_, err = st.db.Exec(
		`INSERT INTO webauthn_challenges (id, kind, data, expires_at) VALUES (?, ?, ?, ?)`,
		id, kind, string(data), time.Now().UTC().Add(ttl).Format(time.RFC3339),
	)
	if err != nil {
		return "", fmt.Errorf("mfa: save challenge: %w", err)
	}
	return id, nil
}

// popChallenge 原子取出并删除挑战(一次性);不存在/过期/用途不符 → ErrChallengeNotFound。
// 用途不符时不消耗该行(注册/登录挑战互不串用)。
func (st *mfaStore) popChallenge(id, kind string) ([]byte, error) {
	var gotKind, data, expiresStr string
	err := st.db.QueryRow(
		`SELECT kind, data, expires_at FROM webauthn_challenges WHERE id = ?`, id,
	).Scan(&gotKind, &data, &expiresStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrChallengeNotFound
		}
		return nil, fmt.Errorf("mfa: get challenge: %w", err)
	}
	if gotKind != kind {
		return nil, ErrChallengeNotFound
	}
	if exp, perr := time.Parse(time.RFC3339, expiresStr); perr != nil || time.Now().UTC().After(exp) {
		_, _ = st.db.Exec(`DELETE FROM webauthn_challenges WHERE id = ?`, id)
		return nil, ErrChallengeNotFound
	}
	_, _ = st.db.Exec(`DELETE FROM webauthn_challenges WHERE id = ?`, id)
	return []byte(data), nil
}

// PurgeExpiredChallenges 清理过期挑战行(启动时调用)。
func (st *mfaStore) PurgeExpiredChallenges() (int64, error) {
	res, err := st.db.Exec(
		`DELETE FROM webauthn_challenges WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("mfa: purge challenges: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---- 杂项 ---------------------------------------------------------------------

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
