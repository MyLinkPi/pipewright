package auth

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Authenticator 定义认证领域对外接口。(-er 命名约定)
type Authenticator interface {
	// Login 以用户名/口令认证;clientIP 用于按 IP 分桶的失败锁定(可空串=只按账户分桶);
	// 成功返回新 Session,失败或锁定返回相应错误。
	Login(username, password, clientIP string) (*Session, error)
	// Verify 校验会话 token 是否有效;有效返回 Session。
	Verify(token string) (*Session, error)
	// Logout 使指定 token 的会话失效。
	Logout(token string) error
	// AdminUsername 返回当前存储的管理员用户名(用于回显)。
	AdminUsername() (string, error)
}

// ErrInvalidCredentials 用户名或口令错误。
var ErrInvalidCredentials = errors.New("auth: invalid credentials")

// ErrInvalidCurrentPassword 改口令时当前口令校验失败。
var ErrInvalidCurrentPassword = errors.New("auth: invalid current password")

// ErrWeakPassword 新口令不满足强度规则(至少 8 位、非空)。
var ErrWeakPassword = errors.New("auth: weak password")

// minPasswordLen 新口令最小长度。
const minPasswordLen = 8

// ErrLockedOut 登录失败次数过多被锁定。
type ErrLockedOut struct {
	RetryAfter time.Duration
}

func (e *ErrLockedOut) Error() string {
	mins := int(e.RetryAfter.Minutes()) + 1
	return fmt.Sprintf("auth: locked out, retry after %d minutes", mins)
}

// dummyHash 是一个**预计算的静态** argon2id PHC 串(参数与真实哈希一致:
// m=64MB,t=1,p=4),用于在用户名不匹配 / admin 不存在时仍执行一次口令校验,
// 使「用户名错」与「口令错」两条失败路径耗时一致,消除时序枚举侧信道。
// 其明文不可知且永不匹配真实输入。
//
// 必须用静态常量、而非在 init() 里跑 argon2 生成:argon2id 单次需 ~64MB,
// init 期生成会让进程**空载常驻内存**永久抬高 ~64MB(实测 18MB→82MB),
// 逼近 NFR-4 的 ≤100MB 红线。静态常量下 64MB 仅在实际登录校验时**瞬时**分配。
const dummyHash = "$argon2id$v=19$m=65536,t=1,p=4$WyWRpzFnab6vlgFLECjL9g$ZJhWU5lKWEo1T7NhxBxQf70Gk+We+sUQ8SO52b8nnxQ"

// Service 实现 Authenticator,依赖 *sql.DB(经构造函数注入)。
type Service struct {
	db       *sql.DB
	sessions *SessionStore
	lockout  *LockoutManager
}

// NewService 构造 Service;clock 为 nil 时使用 RealClock。
func NewService(db *sql.DB, clock Clock) *Service {
	return &Service{
		db:       db,
		sessions: NewSessionStore(db),
		lockout:  NewLockoutManager(clock),
	}
}

// Bootstrap 管理员引导:admin_user 为空时创建管理员。每次启动都会执行,以库为准(幂等):
//   - 已有管理员:直接返回,不触碰任何 env/文件(重启绝不重置口令)。
//   - 未设口令:生成随机强口令,明文写入 passwordFile(0600,先写文件后入库:中途崩溃下
//     一次启动会以库为准重新生成覆盖,不会产生"库里有 admin 却拿不到口令"的锁死态),
//     日志只提示文件路径,不打口令本身。
//   - 设了口令(env):用 env 口令建管理员,不写文件(口令已在用户手里,不额外落盘)。
func (s *Service) Bootstrap(username, password, passwordFile string) error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM admin_user`).Scan(&count); err != nil {
		return fmt.Errorf("auth: bootstrap check: %w", err)
	}
	if count > 0 {
		// 已有管理员,忽略 env(口令变更在 Story 1.7)。
		return nil
	}
	if password == "" {
		var err error
		if password, err = GeneratePassword(); err != nil {
			return fmt.Errorf("auth: bootstrap generate password: %w", err)
		}
		if err := writePasswordFile(passwordFile, password); err != nil {
			return err
		}
	}
	if username == "" {
		username = "admin"
	}
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("auth: bootstrap hash: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.Exec(
		`INSERT INTO admin_user (id, username, password_hash, created_at, updated_at)
		 VALUES (1, ?, ?, ?, ?)`,
		username, hash, now, now,
	)
	if err != nil {
		return fmt.Errorf("auth: bootstrap insert: %w", err)
	}
	if fileExists(passwordFile) {
		log.Printf("[auth] 管理员账号已初始化:username=%s(初始口令见 %s,登录后请修改口令并删除该文件)", username, passwordFile)
	} else {
		log.Printf("[auth] 管理员账号已初始化:username=%s", username)
	}
	return nil
}

// writePasswordFile 把随机生成的初始口令明文写入 path(0600,仅属主可读),目录不存在则建。
// path 为空视为不支持落盘,直接报错(随机口令不落盘即无处获取,宁可启动失败也不锁死用户)。
func writePasswordFile(path, password string) error {
	if path == "" {
		return fmt.Errorf("auth: bootstrap: 随机初始口令需要落盘路径(PIPEWRIGHT_ADMIN_PASSWORD_FILE)")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("auth: bootstrap password file mkdir: %w", err)
		}
	}
	// 覆盖写:库为唯一事实源,残留旧文件(手删过库/上次崩溃)一律以本次为准。
	if err := os.WriteFile(path, []byte(password+"\n"), 0o600); err != nil {
		return fmt.Errorf("auth: bootstrap password file write: %w", err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Login 认证用户名/口令;通过则创建新会话,失败则记录失败计数(按账户与来源 IP 分桶)。
// 身份来源双表:管理员(admin_user,id=1)或普通用户(users,status=active)。
// 无论命中哪一张表(或都不命中),都**只执行一次**口令校验(命中用真实 hash,否则 dummy),
// 使各失败路径耗时一致,消除用户名存在性/角色枚举侧信道。
func (s *Service) Login(username, password, clientIP string) (*Session, error) {
	// 管理员记录(单行)。
	var adminUsername, adminHash string
	adminErr := s.db.QueryRow(
		`SELECT username, password_hash FROM admin_user WHERE id = 1`,
	).Scan(&adminUsername, &adminHash)
	hasAdmin := adminErr == nil
	if adminErr != nil && !errors.Is(adminErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("auth: query admin: %w", adminErr)
	}

	// 普通用户记录(仅 active)。
	var memberID, memberUsername, memberHash string
	memberErr := s.db.QueryRow(
		`SELECT id, username, password_hash FROM users WHERE username = ? AND status = 'active'`,
		username,
	).Scan(&memberID, &memberUsername, &memberHash)
	hasMember := memberErr == nil
	if memberErr != nil && !errors.Is(memberErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("auth: query user: %w", memberErr)
	}

	// 选定候选:管理员优先(用户名恒定时间比较);否则普通用户;都未命中 → dummy。
	role := ""
	userID := ""
	echoedUsername := username
	verifyHash := dummyHash
	adminMatch := hasAdmin &&
		subtle.ConstantTimeCompare([]byte(adminUsername), []byte(username)) == 1
	switch {
	case adminMatch:
		role = RoleAdmin
		echoedUsername = adminUsername
		verifyHash = adminHash
	case hasMember:
		role = RoleMember
		userID = memberID
		echoedUsername = memberUsername
		verifyHash = memberHash
	}

	// 在单次持锁内完成「检查锁定 → 口令校验 → 记失败/重置」,消除 TOCTOU。
	// 桶:按提交的用户名(是否命中真实账户无差别,无存在性侧信道)+ 按来源 IP
	// (空则跳过)。账户桶隔离各账户;IP 桶拦「单 IP 轮换用户名」的绕过。
	keys := make([]lockoutKey, 0, 2)
	keys = append(keys, userLockoutKey(username))
	if ip := strings.TrimSpace(clientIP); ip != "" {
		keys = append(keys, ipLockoutKey(ip))
	}
	ok, locked, remaining, gErr := s.lockout.Guard(keys, func() (bool, error) {
		pwOK, vErr := VerifyPassword(password, verifyHash)
		if vErr != nil {
			return false, vErr
		}
		return pwOK, nil
	})
	if locked {
		return nil, &ErrLockedOut{RetryAfter: remaining}
	}
	if gErr != nil {
		return nil, fmt.Errorf("auth: verify password: %w", gErr)
	}
	if !ok {
		return nil, ErrInvalidCredentials
	}

	// 认证成功:创建会话(Guard 已重置计数)。dummy 永不匹配,故此处 role 必非空。
	sess, err := s.sessions.Create(role, userID, echoedUsername)
	if err != nil {
		return nil, fmt.Errorf("auth: create session: %w", err)
	}
	return sess, nil
}

// PurgeExpiredSessions 删除 DB 中所有已过期会话,返回删除行数。
// 由入口在启动后调用一次,清理惰性删除遗漏的过期会话。
func (s *Service) PurgeExpiredSessions() (int64, error) {
	return s.sessions.PurgeExpired()
}

// AdminUsername 返回当前存储的管理员用户名;无 admin 时返回 ErrSessionNotFound 语义之外的空。
func (s *Service) AdminUsername() (string, error) {
	var u string
	err := s.db.QueryRow(`SELECT username FROM admin_user WHERE id = 1`).Scan(&u)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrSessionNotFound
		}
		return "", fmt.Errorf("auth: query admin username: %w", err)
	}
	return u, nil
}

// Verify 校验会话 token;有效返回 Session。
func (s *Service) Verify(token string) (*Session, error) {
	sess, err := s.sessions.Get(token)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("auth: verify session: %w", err)
	}
	return sess, nil
}

// Logout 删除会话。
func (s *Service) Logout(token string) error {
	return s.sessions.Delete(token)
}

// ChangePassword 校验当前口令 → 更新为新 argon2id 哈希 → 删除除当前会话外的所有会话。
//   - role=member 时更新 users WHERE id=userID;否则更新 admin_user(id=1)。
//   - current 错 → ErrInvalidCurrentPassword(401)。
//   - new 不满足强度(< minPasswordLen 或空)→ ErrWeakPassword(422)。
//   - 成功:更新对应表 password_hash;DELETE WHERE token != currentToken(当前不掉线)。
//
// 与当前口令相同的新口令也允许(不强制不同)。
func (s *Service) ChangePassword(role, userID, current, newPassword, currentToken string) error {
	// 当前会话 token 必须存在(requireAuth 已保证 cookie 在;空 token 属异常状态)。
	// 守住此前提,避免后续 DeleteOthers("") 误删全部会话(含当前)。
	if currentToken == "" {
		return ErrInvalidCurrentPassword
	}
	// 强度校验先行(无需触 argon2,避免无谓内存分配)。
	if len(newPassword) < minPasswordLen {
		return ErrWeakPassword
	}

	// 按身份定位目标行与其当前哈希。
	isMember := role == RoleMember
	var storedHash string
	if isMember {
		err := s.db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&storedHash)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInvalidCurrentPassword
			}
			return fmt.Errorf("auth: query user hash: %w", err)
		}
	} else {
		err := s.db.QueryRow(`SELECT password_hash FROM admin_user WHERE id = 1`).Scan(&storedHash)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInvalidCurrentPassword
			}
			return fmt.Errorf("auth: query admin hash: %w", err)
		}
	}

	// 当前口令校验同样受锁定节流:已持会话者不可无限试当前口令(改密是持久化入口)。
	changeKey := pwChangeLockoutKey(role, userID)
	ok, locked, remaining, err := s.lockout.Guard([]lockoutKey{changeKey}, func() (bool, error) {
		return VerifyPassword(current, storedHash)
	})
	if locked {
		return &ErrLockedOut{RetryAfter: remaining}
	}
	if err != nil {
		return fmt.Errorf("auth: verify current password: %w", err)
	}
	if !ok {
		return ErrInvalidCurrentPassword
	}

	newHash, err := HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("auth: hash new password: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if isMember {
		if _, err := s.db.Exec(
			`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
			newHash, now, userID,
		); err != nil {
			return fmt.Errorf("auth: update user password hash: %w", err)
		}
	} else {
		if _, err := s.db.Exec(
			`UPDATE admin_user SET password_hash = ?, updated_at = ? WHERE id = 1`,
			newHash, now,
		); err != nil {
			return fmt.Errorf("auth: update password hash: %w", err)
		}
	}

	// 改口令后使其它会话失效(当前 token 保留,用户不掉线)。
	if _, err := s.sessions.DeleteOthers(currentToken); err != nil {
		return fmt.Errorf("auth: revoke other sessions: %w", err)
	}
	return nil
}

// ListSessions 返回所有未过期会话元数据(不含原 token);currentToken 标识 current。
func (s *Service) ListSessions(currentToken string) ([]SessionMeta, error) {
	return s.sessions.List(currentToken)
}

// RevokeSession 按 id(sha256 前缀)删除会话;无匹配 → ErrSessionNotFound。
func (s *Service) RevokeSession(id string) error {
	n, err := s.sessions.DeleteByIDPrefix(id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSessionNotFound
	}
	return nil
}
