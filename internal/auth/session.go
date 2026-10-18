package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	// sessionTTL 普通会话有效期(7 天)。
	sessionTTL = 7 * 24 * time.Hour
	// mfaSessionTTL 是通过二步验证(TOTP 验证码 / Passkey 登录)的会话有效期(30 天):
	// 二步验证抬高的是登录门槛,不应因频繁重新登录而反复触发验证;配合会话列表
	// 可随时强制下线,长会话风险可控。
	mfaSessionTTL = 30 * 24 * time.Hour
	// pendingMFATTL 是「密码已过、待二步验证」中间态会话的有效期(10 分钟,固定不滑动)。
	pendingMFATTL = 10 * time.Minute
	// sessionRefreshInterval 是滑动过期的最小写库间隔:Get 命中有效会话后,
	// 仅当距上次 last_seen 超过此间隔才写库刷新,避免每请求一次 UPDATE 的写放大。
	sessionRefreshInterval = 60 * time.Second
	// tokenBytes 会话 token 随机字节数(32B → 64 hex chars)。
	tokenBytes = 32
)

// 会话二步验证状态(sessions.mfa_state 列)。三档同时决定 TTL 与访问语义:
//   - MFAStateNone:    未涉及二步验证,7 天滑动;
//   - MFAStatePending: 密码已过、待验证码,10 分钟固定,受保护 API 一律 401 mfa_required;
//   - MFAStateVerified:已通过二步验证(TOTP 完成 / Passkey 登录),30 天滑动。
const (
	MFAStateNone    = 0
	MFAStatePending = 1
	MFAStateVerified = 2
)

// ErrSessionNotFound 表示会话不存在或已过期。
var ErrSessionNotFound = errors.New("auth: session not found or expired")

// Session 代表一个服务端会话。会话携带身份(角色/用户),供权限校验与回显;
// 管理员会话的 UserID 为空串("")。
type Session struct {
	Token      string
	CSRFToken  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	// Role 是会话角色:"admin" 或 "member"。
	Role string
	// UserID 是普通用户(user)的 id;管理员会话为空串。
	UserID string
	// Username 是会话所属用户名(用于回显/审计)。
	Username string
	// MFAState 是二步验证状态(MFAStateNone/Pending/Verified),决定 TTL 档位与访问语义。
	MFAState int
	// MFAPending 为 true 表示密码已过、待输 TOTP 验证码(MFAState==MFAStatePending)。
	MFAPending bool
	// MFAVerified 为 true 表示会话已通过二步验证(MFAState==MFAStateVerified)。
	MFAVerified bool
	// Refreshed 由 Get 在本次调用执行了滑动续期写库时置位(其余路径恒 false)。
	// HTTP 层据此同步重发 cookie——否则长开不刷新的 SPA 标签页 cookie Expires 固定,
	// 库内会话虽在滑动,cookie 到期即被动登出。
	Refreshed bool
}

// applyMFAState 依据 MFAState 回填派生布尔位。
func (s *Session) applyMFAState() {
	s.MFAPending = s.MFAState == MFAStatePending
	s.MFAVerified = s.MFAState == MFAStateVerified
}

// ttlForState 返回状态对应的会话有效期档位。
func ttlForState(state int) time.Duration {
	switch state {
	case MFAStateVerified:
		return mfaSessionTTL
	case MFAStatePending:
		return pendingMFATTL
	default:
		return sessionTTL
	}
}

// SessionStore 操作 sessions 表。
type SessionStore struct {
	db *sql.DB
}

// NewSessionStore 构造 SessionStore。
func NewSessionStore(db *sql.DB) *SessionStore {
	return &SessionStore{db: db}
}

// 会话角色常量。
const (
	// RoleAdmin 是管理员角色(唯一,存于 admin_user)。
	RoleAdmin = "admin"
	// RoleMember 是普通用户角色。
	RoleMember = "member"
)

// Create 创建新会话并持久化到 DB(未涉及二步验证,7 天 TTL)。返回 Session(含 Token 与 CSRFToken)。
// role 为 RoleAdmin / RoleMember;userID 仅普通用户非空;username 为会话所属用户名。
// 落库的是 token 的 sha256 hex(见 hashToken),原始 token 仅存于 cookie/内存。
func (ss *SessionStore) Create(role, userID, username string) (*Session, error) {
	return ss.CreateWithMFAState(role, userID, username, MFAStateNone)
}

// CreateWithMFAState 按指定二步验证状态创建会话:TTL 随状态分档
// (None=7 天 / Pending=10 分钟 / Verified=30 天),见 ttlForState。
func (ss *SessionStore) CreateWithMFAState(role, userID, username string, mfaState int) (*Session, error) {
	token, err := randHex(tokenBytes)
	if err != nil {
		return nil, fmt.Errorf("auth: generate session token: %w", err)
	}
	csrfToken, err := randHex(tokenBytes)
	if err != nil {
		return nil, fmt.Errorf("auth: generate csrf token: %w", err)
	}
	now := time.Now().UTC()
	ttl := ttlForState(mfaState)
	exp := now.Add(ttl)

	_, err = ss.db.Exec(
		`INSERT INTO sessions (token, csrf_token, created_at, expires_at, last_seen_at, role, user_id, username, mfa_state)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(token),
		csrfToken,
		now.Format(time.RFC3339),
		exp.Format(time.RFC3339),
		now.Format(time.RFC3339),
		role,
		userID,
		username,
		mfaState,
	)
	if err != nil {
		return nil, fmt.Errorf("auth: persist session: %w", err)
	}
	sess := &Session{
		Token:      token,
		CSRFToken:  csrfToken,
		CreatedAt:  now,
		ExpiresAt:  exp,
		LastSeenAt: now,
		Role:       role,
		UserID:     userID,
		Username:   username,
		MFAState:   mfaState,
	}
	sess.applyMFAState()
	return sess, nil
}

// Get 按 token 查找会话;不存在或已过期返回 ErrSessionNotFound。
// 若会话有效则滑动更新 last_seen_at(按会话自身 TTL 档位续期,见 ttlForState);
// 本次调用实际执行了写库续期时,返回的 Session.Refreshed 置位(HTTP 层据此重发 cookie)。
func (ss *SessionStore) Get(token string) (*Session, error) {
	var s Session
	var storedToken string
	var createdStr, expiresStr, lastSeenStr string
	err := ss.db.QueryRow(
		`SELECT token, csrf_token, created_at, expires_at, last_seen_at,
		        COALESCE(role, ?), COALESCE(user_id, ''), COALESCE(username, ''), COALESCE(mfa_state, 0)
		 FROM sessions WHERE token = ?`,
		RoleAdmin, hashToken(token),
	).Scan(&storedToken, &s.CSRFToken, &createdStr, &expiresStr, &lastSeenStr,
		&s.Role, &s.UserID, &s.Username, &s.MFAState)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("auth: get session: %w", err)
	}
	// 库内存的是哈希;对外回填原始 token(调用方语义不变)。
	s.Token = token
	s.applyMFAState()

	if s.CreatedAt, err = time.Parse(time.RFC3339, createdStr); err != nil {
		return nil, fmt.Errorf("auth: parse created_at: %w", err)
	}
	if s.ExpiresAt, err = time.Parse(time.RFC3339, expiresStr); err != nil {
		return nil, fmt.Errorf("auth: parse expires_at: %w", err)
	}
	if s.LastSeenAt, err = time.Parse(time.RFC3339, lastSeenStr); err != nil {
		return nil, fmt.Errorf("auth: parse last_seen_at: %w", err)
	}

	now := time.Now().UTC()
	if now.After(s.ExpiresAt) {
		// 惰性清理过期会话。
		_, _ = ss.db.Exec(`DELETE FROM sessions WHERE token = ?`, hashToken(token))
		return nil, ErrSessionNotFound
	}

	// 滑动空闲超时:命中有效会话时把 expires_at 前移为 now+TTL(按档位),并刷新 last_seen_at。
	// **写节流**:仅当距上次 last_seen 超过 sessionRefreshInterval 才写库。否则每个
	// 认证请求(列表/SSE 等)都触发一次 UPDATE,在单写 modernc/sqlite 上造成写放大与
	// 锁竞争(SSE 并发下易 SQLITE_BUSY)。
	// **待二步验证的中间态不滑动**:10 分钟是验证码时限,不是空闲超时。
	if s.MFAState != MFAStatePending && now.Sub(s.LastSeenAt) >= sessionRefreshInterval {
		newExp := now.Add(ttlForState(s.MFAState))
		if _, err := ss.db.Exec(
			`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE token = ?`,
			now.Format(time.RFC3339), newExp.Format(time.RFC3339), hashToken(token),
		); err == nil {
			s.LastSeenAt = now
			s.ExpiresAt = newExp
			s.Refreshed = true
		}
	}
	return &s, nil
}

// PurgeExpired 删除所有已过期会话,返回删除行数。
// 在启动时调用一次以清理 DB 中惰性删除遗漏的过期会话。
func (ss *SessionStore) PurgeExpired() (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := ss.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now)
	if err != nil {
		return 0, fmt.Errorf("auth: purge expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Delete 删除指定 token 对应的会话(登出)。
func (ss *SessionStore) Delete(token string) error {
	_, err := ss.db.Exec(`DELETE FROM sessions WHERE token = ?`, hashToken(token))
	if err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

// CompleteMFA 把「待二步验证」会话升级为「已验证」正式会话:状态置 MFAStateVerified,
// 有效期重置为 now+mfaSessionTTL(30 天滑动)。仅 MFAStatePending 的会话可升级
// (WHERE mfa_state = 1);无匹配(不存在/非 pending)返回 ErrSessionNotFound。
// 验证码本身的校验由调用方(mfa 服务)先行完成,本方法只负责会话状态迁移。
func (ss *SessionStore) CompleteMFA(token string) (*Session, error) {
	now := time.Now().UTC()
	res, err := ss.db.Exec(
		`UPDATE sessions SET mfa_state = ?, last_seen_at = ?, expires_at = ?
		 WHERE token = ? AND mfa_state = ?`,
		MFAStateVerified, now.Format(time.RFC3339),
		now.Add(mfaSessionTTL).Format(time.RFC3339),
		hashToken(token), MFAStatePending,
	)
	if err != nil {
		return nil, fmt.Errorf("auth: complete mfa: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrSessionNotFound
	}
	return ss.Get(token)
}

// sessionIDPrefixLen 是会话对外 id 的长度(库内 token 哈希 hex 的前缀字符数)。
// 16 hex = 64 bit,足够在单实例会话集合内唯一定位,且**绝不回传原 token**。
const sessionIDPrefixLen = 16

// hashToken 计算会话 token 的 sha256 hex(64 字符)。sessions.token 列只落该哈希:
// 即使 DB 文件泄漏,库内值也不能直接当作 cookie 冒充会话(原始 token 仅存在于
// cookie 与进程内存)。列宽 191 足够容纳 64 hex。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SessionID 返回 token 的 sha256 hex 前 16 位,作为对外可定位 id(绝不暴露原 token)。
func SessionID(token string) string {
	return hashToken(token)[:sessionIDPrefixLen]
}

// SessionMeta 是会话的对外只读元数据(用于列表;不含原 token)。
type SessionMeta struct {
	ID         string // token 的 sha256 hex 前缀
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	Current    bool // 是否为当前请求所属会话
}

// List 返回所有未过期正式会话的元数据(不含原 token)。currentToken 用于标识 current。
// 按 created_at DESC 排序(最新在前)。待二步验证的中间态会话(mfa_state=1)不列出:
// 它们不是可用会话,10 分钟内未完成验证即失效。
func (ss *SessionStore) List(currentToken string) ([]SessionMeta, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := ss.db.Query(
		`SELECT token, created_at, last_seen_at, expires_at
		 FROM sessions WHERE expires_at >= ? AND mfa_state <> ? ORDER BY created_at DESC`,
		now, MFAStatePending,
	)
	if err != nil {
		return nil, fmt.Errorf("auth: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	currentID := SessionID(currentToken)
	var out []SessionMeta
	for rows.Next() {
		var storedToken, createdStr, lastSeenStr, expiresStr string
		if err := rows.Scan(&storedToken, &createdStr, &lastSeenStr, &expiresStr); err != nil {
			return nil, fmt.Errorf("auth: scan session: %w", err)
		}
		// 库内即 token 哈希,取前缀即对外 id。短于前缀的异常行直接跳过(不 panic)。
		if len(storedToken) < sessionIDPrefixLen {
			continue
		}
		m := SessionMeta{ID: storedToken[:sessionIDPrefixLen]}
		if m.CreatedAt, err = time.Parse(time.RFC3339, createdStr); err != nil {
			return nil, fmt.Errorf("auth: parse created_at: %w", err)
		}
		if m.LastSeenAt, err = time.Parse(time.RFC3339, lastSeenStr); err != nil {
			return nil, fmt.Errorf("auth: parse last_seen_at: %w", err)
		}
		if m.ExpiresAt, err = time.Parse(time.RFC3339, expiresStr); err != nil {
			return nil, fmt.Errorf("auth: parse expires_at: %w", err)
		}
		m.Current = m.ID == currentID
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: iterate sessions: %w", err)
	}
	return out, nil
}

// reSessionID 限定对外会话 id 形态:小写 hex(与库内哈希前缀一致)。
// 拒绝 LIKE 通配符(%/_)等非 hex 输入,防畸形前缀误删(如 "%" 会匹配全部)。
var reSessionID = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

// DeleteByIDPrefix 删除 sha256 前缀匹配 id 的会话。返回删除行数。
// 库内 token 即 sha256 hex,直接 SQL 前缀匹配;非 hex 的 id 一律不匹配(0 行)。
func (ss *SessionStore) DeleteByIDPrefix(id string) (int64, error) {
	if !reSessionID.MatchString(id) {
		return 0, nil
	}
	res, err := ss.db.Exec(`DELETE FROM sessions WHERE token LIKE ?`, id+"%")
	if err != nil {
		return 0, fmt.Errorf("auth: delete session by id: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteOthers 删除除 keepToken 外的所有会话(改口令后使其它会话失效)。返回删除行数。
//
// keepToken 为空时**拒绝**(返回错误):空 token 会让 `token != ”` 匹配所有真实会话,
// 误删包括当前会话在内的全部,与「当前会话保留」语义相悖。这是危险原语的兜底护栏
// (调用方应保证传入有效的当前会话 token)。
func (ss *SessionStore) DeleteOthers(keepToken string) (int64, error) {
	if keepToken == "" {
		return 0, errors.New("auth: DeleteOthers 拒绝空 keepToken(避免误删全部会话)")
	}
	res, err := ss.db.Exec(`DELETE FROM sessions WHERE token != ?`, hashToken(keepToken))
	if err != nil {
		return 0, fmt.Errorf("auth: delete other sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// randHex 生成 n 字节随机数并返回 hex 编码字串。
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
