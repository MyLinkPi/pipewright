// Package identity 实现普通用户体系:用户账号、管理员邀请、项目成员关系。
//
// 管理员(admin_user,id=1)不属于本包;本包只管理普通用户(member)。
// 口令哈希复用 auth 包的 argon2id 实现(同一套 PHC 格式与强度规则),不另起哈希体系。
// 数据访问仅经参数化 SQL;方言差异(upsert)经 store 的 helper 生成。
package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/store"
)

// InvitationTTL 是邀请默认有效期(未显式指定 ttl 时)。
const InvitationTTL = 7 * 24 * time.Hour

// 普通用户角色/状态常量。
const (
	// RoleMember 是普通用户角色(与 auth.RoleMember 对齐)。
	RoleMember = "member"
	// StatusActive 是可用状态;非 active 的用户不可登录。
	StatusActive = "active"
)

// 邀请状态(对外展示)。
const (
	InvitationPending = "pending"
	InvitationUsed    = "used"
	InvitationExpired = "expired"
)

// 用户名规则:3–32 位,仅字母/数字/下划线/点/连字符。
const (
	minUsernameLen = 3
	maxUsernameLen = 32
	// minPasswordLen 与 auth 保持一致(至少 8 位)。
	minPasswordLen = 8
)

// 领域错误。错误体绝不含明文口令/哈希。
var (
	// ErrInvitationNotFound 邀请不存在或 token 非法。
	ErrInvitationNotFound = errors.New("identity: invitation not found")
	// ErrInvitationUsed 邀请已被使用。
	ErrInvitationUsed = errors.New("identity: invitation already used")
	// ErrInvitationExpired 邀请已过期。
	ErrInvitationExpired = errors.New("identity: invitation expired")
	// ErrUsernameTaken 用户名已被占用(含管理员用户名)。
	ErrUsernameTaken = errors.New("identity: username already taken")
	// ErrInvalidUsername 用户名不满足规则。
	ErrInvalidUsername = errors.New("identity: invalid username")
	// ErrWeakPassword 口令不满足强度。
	ErrWeakPassword = errors.New("identity: weak password")
	// ErrUserNotFound 用户不存在。
	ErrUserNotFound = errors.New("identity: user not found")
	// ErrMemberNotFound 成员关系不存在。
	ErrMemberNotFound = errors.New("identity: project member not found")
	// ErrProjectNotFound 项目不存在。
	ErrProjectNotFound = errors.New("identity: project not found")
)

// User 是普通用户领域模型(绝不含口令哈希)。
type User struct {
	ID        string
	Username  string
	Role      string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Invitation 是邀请领域模型。Token 仅在创建时返回给调用方(用于拼链接)。
type Invitation struct {
	ID        string
	Token     string
	CreatedBy string
	ExpiresAt time.Time
	UsedAt    *time.Time
	UsedBy    string
	CreatedAt time.Time
	// State 是计算得出的状态:pending|used|expired。
	State string
}

// Service 定义用户体系对外接口。
type Service interface {
	// CreateInvitation 生成一次性邀请(ttl<=0 用 InvitationTTL)。
	CreateInvitation(ctx context.Context, createdBy string, ttl time.Duration) (*Invitation, error)
	// ListInvitations 列出全部邀请(按创建时间倒序)。
	ListInvitations(ctx context.Context) ([]Invitation, error)
	// RevokeInvitation 按 id 删除邀请;不存在 → ErrInvitationNotFound。
	RevokeInvitation(ctx context.Context, id string) error
	// InvitationInfo 按 token 查询邀请(公开校验用);不存在 → ErrInvitationNotFound。
	InvitationInfo(ctx context.Context, token string) (*Invitation, error)
	// Register 用邀请 token 注册普通用户(校验邀请有效 + 用户名可用 + 口令强度)。
	Register(ctx context.Context, token, username, password string) (*User, error)
	// ListUsers 列出全部普通用户。
	ListUsers(ctx context.Context) ([]User, error)
	// GetUser 按 id 取用户;不存在 → ErrUserNotFound。
	GetUser(ctx context.Context, id string) (*User, error)
	// GetUserByUsername 按用户名取用户;不存在 → ErrUserNotFound。
	GetUserByUsername(ctx context.Context, username string) (*User, error)
	// AddMember 把用户加入项目(幂等)。
	AddMember(ctx context.Context, projectID, userID string) error
	// RemoveMember 把用户移出项目;关系不存在 → ErrMemberNotFound。
	RemoveMember(ctx context.Context, projectID, userID string) error
	// ListProjectMembers 列出项目成员。
	ListProjectMembers(ctx context.Context, projectID string) ([]User, error)
	// ListUserProjectIDs 列出用户加入的项目 id(用于列表过滤)。
	ListUserProjectIDs(ctx context.Context, userID string) ([]string, error)
	// IsMember 判断用户是否为项目成员。
	IsMember(ctx context.Context, userID, projectID string) (bool, error)
}

// service 是 *sql.DB 支撑的 Service 实现。
type service struct {
	db      *sql.DB
	dialect store.Dialect
}

// New 构造 Service。方言经驱动判定,用于生成跨库 upsert 语句。
func New(db *sql.DB) Service {
	return &service{db: db, dialect: store.DialectOf(db)}
}

// validUsername 校验用户名规则:长度 3–32,仅 [A-Za-z0-9_.-]。
func validUsername(username string) bool {
	if len(username) < minUsernameLen || len(username) > maxUsernameLen {
		return false
	}
	for _, r := range username {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

// CreateInvitation 生成一次性邀请并入库。
func (s *service) CreateInvitation(ctx context.Context, createdBy string, ttl time.Duration) (*Invitation, error) {
	if ttl <= 0 {
		ttl = InvitationTTL
	}
	token, err := randHex(invitationTokenBytes)
	if err != nil {
		return nil, fmt.Errorf("identity: generate invitation token: %w", err)
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	expires := now.Add(ttl)

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO invitations (id, token, created_by, expires_at, used_at, used_by, created_at)
		 VALUES (?, ?, ?, ?, NULL, '', ?)`,
		id, token, createdBy, expires.Format(time.RFC3339), now.Format(time.RFC3339),
	); err != nil {
		return nil, fmt.Errorf("identity: insert invitation: %w", err)
	}
	return &Invitation{
		ID:        id,
		Token:     token,
		CreatedBy: createdBy,
		ExpiresAt: expires,
		CreatedAt: now,
		State:     InvitationPending,
	}, nil
}

// ListInvitations 列出全部邀请。
func (s *service) ListInvitations(ctx context.Context) ([]Invitation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, token, created_by, expires_at, used_at, used_by, created_at
		 FROM invitations ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("identity: list invitations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	now := time.Now().UTC()
	out := make([]Invitation, 0)
	for rows.Next() {
		inv, err := scanInvitation(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, *inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: iterate invitations: %w", err)
	}
	return out, nil
}

// RevokeInvitation 按 id 删除邀请。
func (s *service) RevokeInvitation(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM invitations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("identity: delete invitation: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrInvitationNotFound
	}
	return nil
}

// InvitationInfo 按 token 查询邀请。
func (s *service) InvitationInfo(ctx context.Context, token string) (*Invitation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, token, created_by, expires_at, used_at, used_by, created_at
		 FROM invitations WHERE token = ?`, token)
	inv, err := scanInvitation(row, time.Now().UTC())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInvitationNotFound
		}
		return nil, err
	}
	return inv, nil
}

// Register 用邀请 token 注册普通用户。
func (s *service) Register(ctx context.Context, token, username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if !validUsername(username) {
		return nil, ErrInvalidUsername
	}
	if len(password) < minPasswordLen {
		return nil, ErrWeakPassword
	}

	inv, err := s.InvitationInfo(ctx, token)
	if err != nil {
		return nil, err
	}
	switch inv.State {
	case InvitationUsed:
		return nil, ErrInvitationUsed
	case InvitationExpired:
		return nil, ErrInvitationExpired
	}

	taken, err := s.usernameTaken(ctx, username)
	if err != nil {
		return nil, err
	}
	if taken {
		return nil, ErrUsernameTaken
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("identity: hash password: %w", err)
	}

	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("identity: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, username, hash, RoleMember, StatusActive, now, now,
	); err != nil {
		if store.IsUniqueErr(err) {
			return nil, ErrUsernameTaken
		}
		return nil, fmt.Errorf("identity: insert user: %w", err)
	}

	// 标记邀请已用:仅当仍未使用才成功(防并发重复注册)。
	res, err := tx.ExecContext(ctx,
		`UPDATE invitations SET used_at = ?, used_by = ? WHERE id = ? AND used_at IS NULL`,
		now, id, inv.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("identity: mark invitation used: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrInvitationUsed
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("identity: commit register: %w", err)
	}
	return s.GetUser(ctx, id)
}

// usernameTaken 判断用户名是否被普通用户或管理员占用。
func (s *service) usernameTaken(ctx context.Context, username string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM users WHERE username = ?`, username,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("identity: check user username: %w", err)
	}
	if n > 0 {
		return true, nil
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM admin_user WHERE username = ?`, username,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("identity: check admin username: %w", err)
	}
	return n > 0, nil
}

// ListUsers 列出全部普通用户。
func (s *service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, role, status, created_at, updated_at
		 FROM users ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("identity: list users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]User, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: iterate users: %w", err)
	}
	return out, nil
}

// GetUser 按 id 取用户。
func (s *service) GetUser(ctx context.Context, id string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, role, status, created_at, updated_at FROM users WHERE id = ?`, id)
	return getUser(row)
}

// GetUserByUsername 按用户名取用户。
func (s *service) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, role, status, created_at, updated_at FROM users WHERE username = ?`, username)
	return getUser(row)
}

// getUser 扫描单行用户;ErrNoRows → ErrUserNotFound。
func getUser(sc scanner) (*User, error) {
	u, err := scanUser(sc)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return u, nil
}

// AddMember 把用户加入项目(幂等)。
func (s *service) AddMember(ctx context.Context, projectID, userID string) error {
	if projectID == "" {
		return ErrProjectNotFound
	}
	if _, err := s.GetUser(ctx, userID); err != nil {
		return err
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM projects WHERE id = ?`, projectID,
	).Scan(&n); err != nil {
		return fmt.Errorf("identity: check project: %w", err)
	}
	if n == 0 {
		return ErrProjectNotFound
	}
	now := time.Now().UTC().Format(time.RFC3339)
	sqlText := `INSERT INTO project_members (project_id, user_id, created_at) VALUES (?, ?, ?)` +
		store.DoNothingSuffix(s.dialect, []string{"project_id", "user_id"})
	if _, err := s.db.ExecContext(ctx, sqlText, projectID, userID, now); err != nil {
		return fmt.Errorf("identity: add project member: %w", err)
	}
	return nil
}

// RemoveMember 把用户移出项目。
func (s *service) RemoveMember(ctx context.Context, projectID, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM project_members WHERE project_id = ? AND user_id = ?`, projectID, userID)
	if err != nil {
		return fmt.Errorf("identity: remove project member: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrMemberNotFound
	}
	return nil
}

// ListProjectMembers 列出项目成员。
func (s *service) ListProjectMembers(ctx context.Context, projectID string) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT u.id, u.username, u.role, u.status, u.created_at, u.updated_at
		 FROM users u
		 JOIN project_members m ON m.user_id = u.id
		 WHERE m.project_id = ?
		 ORDER BY u.username`, projectID)
	if err != nil {
		return nil, fmt.Errorf("identity: list project members: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]User, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: iterate project members: %w", err)
	}
	return out, nil
}

// ListUserProjectIDs 列出用户加入的项目 id。
func (s *service) ListUserProjectIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id FROM project_members WHERE user_id = ? ORDER BY project_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("identity: list user projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0)
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, fmt.Errorf("identity: scan user project: %w", err)
		}
		out = append(out, pid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: iterate user projects: %w", err)
	}
	return out, nil
}

// IsMember 判断用户是否为项目成员。
func (s *service) IsMember(ctx context.Context, userID, projectID string) (bool, error) {
	if userID == "" || projectID == "" {
		return false, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM project_members WHERE user_id = ? AND project_id = ?`,
		userID, projectID,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("identity: check membership: %w", err)
	}
	return n > 0, nil
}
