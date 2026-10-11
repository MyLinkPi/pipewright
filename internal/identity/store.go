package identity

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// invitationTokenBytes 是邀请 token 随机字节数(32B → 64 hex chars),与会话 token 同强度。
const invitationTokenBytes = 32

// scanner 抽象 *sql.Row 与 *sql.Rows 的 Scan。
type scanner interface {
	Scan(dest ...any) error
}

// randHex 生成 n 字节随机数并返回 hex 编码字串。
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// scanUser 扫描一行用户(不含口令哈希列;绝不读取 password_hash)。
func scanUser(sc scanner) (*User, error) {
	var u User
	var createdStr, updatedStr string
	if err := sc.Scan(&u.ID, &u.Username, &u.Role, &u.Status, &createdStr, &updatedStr); err != nil {
		return nil, err
	}
	created, err := time.Parse(time.RFC3339, createdStr)
	if err != nil {
		return nil, fmt.Errorf("identity: parse user created_at: %w", err)
	}
	updated, err := time.Parse(time.RFC3339, updatedStr)
	if err != nil {
		return nil, fmt.Errorf("identity: parse user updated_at: %w", err)
	}
	u.CreatedAt = created
	u.UpdatedAt = updated
	return &u, nil
}

// scanInvitation 扫描一行邀请并据 now 计算状态。
func scanInvitation(sc scanner, now time.Time) (*Invitation, error) {
	var inv Invitation
	var expiresStr, createdStr string
	var usedAt, usedBy sql.NullString
	if err := sc.Scan(&inv.ID, &inv.Token, &inv.CreatedBy, &expiresStr, &usedAt, &usedBy, &createdStr); err != nil {
		return nil, err
	}
	expires, err := time.Parse(time.RFC3339, expiresStr)
	if err != nil {
		return nil, fmt.Errorf("identity: parse invitation expires_at: %w", err)
	}
	created, err := time.Parse(time.RFC3339, createdStr)
	if err != nil {
		return nil, fmt.Errorf("identity: parse invitation created_at: %w", err)
	}
	inv.ExpiresAt = expires
	inv.CreatedAt = created
	inv.UsedBy = usedBy.String
	if usedAt.Valid && usedAt.String != "" {
		if t, err := time.Parse(time.RFC3339, usedAt.String); err == nil {
			inv.UsedAt = &t
		} else {
			inv.UsedAt = &now
		}
	}
	inv.State = invitationState(inv.UsedAt, inv.ExpiresAt, now)
	return &inv, nil
}

// invitationState 计算邀请状态:已用 > 过期 > 待用。
func invitationState(usedAt *time.Time, expiresAt, now time.Time) string {
	if usedAt != nil {
		return InvitationUsed
	}
	if now.After(expiresAt) {
		return InvitationExpired
	}
	return InvitationPending
}
