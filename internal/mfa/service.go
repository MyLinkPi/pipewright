package mfa

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	webauthn "github.com/go-webauthn/webauthn/webauthn"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/systemcfg"
)

// ErrOwnerNotFound 管理员重置的目标用户不存在。
var ErrOwnerNotFound = errors.New("mfa: owner not found")

// Service 是两步验证领域服务:TOTP 生命周期、验证码校验(登录/改配)、Passkey 注册与
// discoverable 登录。依赖:db(mfa 四表 + users/admin_user 身份回查)、vault Sealer
// (可 nil,退 local 密钥)、systemcfg(Passkey RP 配置)、auth.LockoutManager(验证码节流)。
type Service struct {
	st      *mfaStore
	db      *sql.DB
	secrets *secretKeeper
	syscfg  systemcfg.Service
	lockout *auth.LockoutManager

	waMu    sync.Mutex
	waCache map[string]*webauthn.WebAuthn
}

// NewService 构造 Service。clock 为 nil 用真实时钟(vaultSealer/syscfg 允许 nil:
// 前者退本地密钥加密,后者 Passkey RP 配置全走请求推导、开关默认开)。
func NewService(db *sql.DB, clock auth.Clock, vaultSealer Sealer, sys systemcfg.Service) *Service {
	return &Service{
		st:      &mfaStore{db: db, dialect: store.DialectOf(db)},
		db:      db,
		secrets: &secretKeeper{db: db, vault: vaultSealer},
		syscfg:  sys,
		lockout: auth.NewLockoutManager(clock),
	}
}

// TOTPEnabled 实现 auth.MFAChecker:登录口令通过后据此决定是否进入两步流程。
// 查询失败一律 false(fail-open:mfa 子系统故障不应把用户锁在密码登录之外,
// 降级等价于该账户未启用二步验证)。
func (s *Service) TOTPEnabled(role, userID string) bool {
	c, err := s.st.getTOTP(OwnerKey(role, userID))
	if err != nil {
		return false
	}
	return c.Enabled
}

// ---- TOTP 生命周期 ------------------------------------------------------------

// TOTPSetup 是 setup 响应:密钥与备份码明文仅此一次返回。
type TOTPSetup struct {
	Secret      string
	OTPAuthURL  string
	BackupCodes []string
}

// SetupTOTP 发起/重发起配置:生成新密钥(enabled=0)并全量替换备份码。
// 已启用的账户不允许重设(须先 disable,防半途换钥造成不可登录)。
func (s *Service) SetupTOTP(role, userID, username string) (*TOTPSetup, error) {
	owner := OwnerKey(role, userID)
	if existing, err := s.st.getTOTP(owner); err == nil && existing.Enabled {
		return nil, ErrTOTPAlreadyOn
	}
	secret, otpauthURL, err := GenerateTOTPSecret(username)
	if err != nil {
		return nil, err
	}
	sealed, err := s.secrets.seal([]byte(secret))
	if err != nil {
		return nil, err
	}
	codes, err := GenerateBackupCodes()
	if err != nil {
		return nil, err
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		h, herr := auth.HashPassword(c)
		if herr != nil {
			return nil, fmt.Errorf("mfa: hash backup code: %w", herr)
		}
		hashes[i] = h
	}
	if err := s.st.upsertTOTP(owner, sealed, hashes); err != nil {
		return nil, err
	}
	return &TOTPSetup{Secret: secret, OTPAuthURL: otpauthURL, BackupCodes: codes}, nil
}

// EnableTOTP 验证首个动态码,把 setup 置为启用。
func (s *Service) EnableTOTP(role, userID, code string) error {
	owner := OwnerKey(role, userID)
	c, err := s.st.getTOTP(owner)
	if err != nil {
		return err
	}
	if c.Enabled {
		return ErrTOTPAlreadyOn
	}
	if err := s.verifyCodeGuarded(owner, c.SecretEnc, code); err != nil {
		return err
	}
	return s.st.setTOTPEnabled(owner, true)
}

// DisableTOTP 关闭二步验证:须提供有效动态码或未用备份码;成功即删密钥与备份码。
func (s *Service) DisableTOTP(role, userID, code string) error {
	owner := OwnerKey(role, userID)
	c, err := s.st.getTOTP(owner)
	if err != nil {
		return err
	}
	if !c.Enabled {
		return ErrTOTPNotEnabled
	}
	if err := s.verifyCodeGuarded(owner, c.SecretEnc, code); err != nil {
		return err
	}
	return s.st.deleteTOTP(owner)
}

// RegenerateBackupCodes 重新生成备份码(旧码全量作废);须验证当前动态码/备份码。
func (s *Service) RegenerateBackupCodes(role, userID, code string) ([]string, error) {
	owner := OwnerKey(role, userID)
	c, err := s.st.getTOTP(owner)
	if err != nil {
		return nil, err
	}
	if !c.Enabled {
		return nil, ErrTOTPNotEnabled
	}
	if err := s.verifyCodeGuarded(owner, c.SecretEnc, code); err != nil {
		return nil, err
	}
	codes, err := GenerateBackupCodes()
	if err != nil {
		return nil, err
	}
	hashes := make([]string, len(codes))
	for i, code := range codes {
		h, herr := auth.HashPassword(code)
		if herr != nil {
			return nil, fmt.Errorf("mfa: hash backup code: %w", herr)
		}
		hashes[i] = h
	}
	if err := s.st.replaceBackupCodes(owner, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// VerifyLoginCode 登录第二步:校验 pending 会话持有者的动态码(备份码兜底),
// 失败按 owner 节流(5 次失败锁 15 分钟,与登录口令同参数)。不区分两种因素的
// 失败信息(统一 ErrInvalidCode),避免探测指引。
func (s *Service) VerifyLoginCode(role, userID, code string) error {
	owner := OwnerKey(role, userID)
	c, err := s.st.getTOTP(owner)
	if err != nil {
		return err
	}
	if !c.Enabled {
		return ErrTOTPNotEnabled
	}
	return s.verifyCodeGuarded(owner, c.SecretEnc, code)
}

// verifyCodeGuarded 解封密钥并校验动态码,失败再试备份码;全程在锁定 Guard 内
// (单次持锁消除 TOCTOU)。系统错误(解封失败等)不计为失败次数。
func (s *Service) verifyCodeGuarded(owner, secretEnc, code string) error {
	ok, locked, remaining, err := s.lockout.GuardMFA(owner, func() (bool, error) {
		secret, serr := s.secrets.open(secretEnc)
		if serr != nil {
			return false, serr
		}
		if ValidateTOTPCode(string(secret), code) {
			return true, nil
		}
		if looksLikeBackupCode(code) {
			return s.consumeBackupCode(owner, code)
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	if locked {
		return &ErrLockedOut{RetryAfter: remaining}
	}
	if !ok {
		return ErrInvalidCode
	}
	return nil
}

// consumeBackupCode 匹配并消耗一枚未用备份码(argon2id 校验,命中即标记 used)。
func (s *Service) consumeBackupCode(owner, code string) (bool, error) {
	normalized, ok := NormalizeBackupCode(code)
	if !ok {
		return false, nil
	}
	codes, err := s.st.unusedBackupCodes(owner)
	if err != nil {
		return false, err
	}
	for _, c := range codes {
		if match, verr := auth.VerifyPassword(normalized, c.Hash); verr == nil && match {
			if uerr := s.st.markBackupCodeUsed(c.ID); uerr != nil {
				return false, uerr
			}
			return true, nil
		}
	}
	return false, nil
}

// ---- 状态查询 ------------------------------------------------------------------

// PasskeyInfo 对外的通行密钥视图(不含任何密钥材料)。
type PasskeyInfo struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"createdAt"`
	LastUsedAt *string `json:"lastUsedAt"`
}

// StatusInfo 是 GET /api/account/mfa 响应体。
type StatusInfo struct {
	TOTPEnabled          bool          `json:"totpEnabled"`
	TOTPSetupPending     bool          `json:"totpSetupPending"`
	BackupCodesRemaining int           `json:"backupCodesRemaining"`
	Passkeys             []PasskeyInfo `json:"passkeys"`
}

// Status 汇总 owner 的两步验证状态。
func (s *Service) Status(role, userID string) (*StatusInfo, error) {
	owner := OwnerKey(role, userID)
	info := &StatusInfo{Passkeys: []PasskeyInfo{}}
	if c, err := s.st.getTOTP(owner); err == nil {
		info.TOTPEnabled = c.Enabled
		info.TOTPSetupPending = !c.Enabled
		info.BackupCodesRemaining = s.st.backupCodesRemaining(owner)
	} else if !errors.Is(err, ErrTOTPNotSetup) {
		return nil, err
	}
	rows, err := s.st.listPasskeyRows(owner)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		p := PasskeyInfo{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt.Format("2006-01-02 15:04")}
		if r.LastUsedAt != nil {
			t := r.LastUsedAt.Format("2006-01-02 15:04")
			p.LastUsedAt = &t
		}
		info.Passkeys = append(info.Passkeys, p)
	}
	return info, nil
}

// UserMFASummary 供管理员用户列表展示某成员的 2FA 概要。
func (s *Service) UserMFASummary(userID string) (totpEnabled bool, passkeyCount int) {
	owner := OwnerKey(auth.RoleMember, userID)
	if c, err := s.st.getTOTP(owner); err == nil {
		totpEnabled = c.Enabled
	}
	passkeyCount = s.st.countPasskeysByOwner(owner)
	return
}

// ---- Passkey 注册 ---------------------------------------------------------------

// BeginResult 是 begin 响应:挑战 id(HTTP 层写 cookie)+ 浏览器原生选项。
type BeginResult struct {
	ChallengeID string
	Options     any // *protocol.CredentialCreation / *protocol.CredentialAssertion
}

// registerChallenge 把挑战与设备名打包存库(finish 时一并取回)。
type registerChallenge struct {
	Name string                `json:"name"`
	SD   *webauthn.SessionData `json:"sd"`
}

// BeginPasskeyRegistration 为已登录身份发起注册(排除已有凭据防重复注册同一密钥)。
func (s *Service) BeginPasskeyRegistration(ctx context.Context, role, userID, username, name string, secure bool, host string) (*BeginResult, error) {
	owner := OwnerKey(role, userID)
	rp, err := s.resolveRP(ctx, secure, host)
	if err != nil {
		return nil, err
	}
	wa, err := s.webauthnFor(rp)
	if err != nil {
		return nil, err
	}
	rows, err := s.st.listPasskeyRows(owner)
	if err != nil {
		return nil, err
	}
	creds := make([]webauthn.Credential, 0, len(rows))
	for _, r := range rows {
		creds = append(creds, r.toWebAuthnCredential())
	}
	user := &webauthnUser{key: owner, name: username, displayName: username, credentials: creds}

	var opts []webauthn.RegistrationOption
	if len(creds) > 0 {
		descriptors := make([]protocol.CredentialDescriptor, 0, len(creds))
		for i := range creds {
			descriptors = append(descriptors, creds[i].Descriptor())
		}
		opts = append(opts, webauthn.WithExclusions(descriptors))
	}
	creation, sd, err := wa.BeginRegistration(user, opts...)
	if err != nil {
		return nil, fmt.Errorf("mfa: begin passkey registration: %w", err)
	}
	payload, err := json.Marshal(registerChallenge{Name: normalizePasskeyName(name), SD: sd})
	if err != nil {
		return nil, fmt.Errorf("mfa: encode challenge: %w", err)
	}
	cid, err := s.st.saveChallenge(challengeRegister, payload, challengeTTL)
	if err != nil {
		return nil, err
	}
	return &BeginResult{ChallengeID: cid, Options: creation}, nil
}

// FinishPasskeyRegistration 校验注册回执并存入新凭据,返回对外视图。
func (s *Service) FinishPasskeyRegistration(ctx context.Context, role, userID, username, challengeID string, secure bool, host string, r *http.Request) (*PasskeyInfo, error) {
	owner := OwnerKey(role, userID)
	raw, err := s.st.popChallenge(challengeID, challengeRegister)
	if err != nil {
		return nil, err
	}
	var ch registerChallenge
	if err := json.Unmarshal(raw, &ch); err != nil || ch.SD == nil {
		return nil, ErrChallengeNotFound
	}
	rp, err := s.resolveRP(ctx, secure, host)
	if err != nil {
		return nil, err
	}
	wa, err := s.webauthnFor(rp)
	if err != nil {
		return nil, err
	}
	rows, err := s.st.listPasskeyRows(owner)
	if err != nil {
		return nil, err
	}
	creds := make([]webauthn.Credential, 0, len(rows))
	for _, row := range rows {
		creds = append(creds, row.toWebAuthnCredential())
	}
	user := &webauthnUser{key: owner, name: username, displayName: username, credentials: creds}
	cred, err := wa.FinishRegistration(user, *ch.SD, r)
	if err != nil {
		return nil, fmt.Errorf("mfa: finish passkey registration: %w", err)
	}
	row := fromWebAuthnCredential(owner, ch.Name, cred)
	id, err := s.st.insertPasskey(row)
	if err != nil {
		return nil, err
	}
	info := PasskeyInfo{ID: id, Name: row.Name, CreatedAt: row.CreatedAt.Format("2006-01-02 15:04")}
	return &info, nil
}

// DeletePasskey 删除本人名下指定凭据。
func (s *Service) DeletePasskey(role, userID string, id int64) error {
	return s.st.deletePasskey(OwnerKey(role, userID), id)
}

// ---- Passkey 登录(discoverable)---------------------------------------------------

// Identity 是 Passkey 登录解析出的身份(供 auth 建会话)。
type Identity struct {
	Role     string
	UserID   string
	Username string
}

// PasskeyLoginUsable 登录页是否展示「使用通行密钥登录」:
// 总开关开启 && 实例内至少注册了一把密钥。
func (s *Service) PasskeyLoginUsable(ctx context.Context) bool {
	if s.syscfg != nil {
		if c, err := s.syscfg.Get(ctx); err == nil && !c.PasskeyLoginEnabled {
			return false
		}
	}
	return s.st.anyPasskeyExists()
}

// BeginPasskeyLogin 发起 discoverable 登录(无需预知用户)。
func (s *Service) BeginPasskeyLogin(ctx context.Context, secure bool, host string) (*BeginResult, error) {
	if !s.PasskeyLoginUsable(ctx) {
		return nil, ErrPasskeyDisabled
	}
	rp, err := s.resolveRP(ctx, secure, host)
	if err != nil {
		return nil, err
	}
	wa, err := s.webauthnFor(rp)
	if err != nil {
		return nil, err
	}
	assertion, sd, err := wa.BeginDiscoverableLogin()
	if err != nil {
		return nil, fmt.Errorf("mfa: begin passkey login: %w", err)
	}
	payload, err := json.Marshal(sd)
	if err != nil {
		return nil, fmt.Errorf("mfa: encode challenge: %w", err)
	}
	cid, err := s.st.saveChallenge(challengeLogin, payload, challengeTTL)
	if err != nil {
		return nil, err
	}
	return &BeginResult{ChallengeID: cid, Options: assertion}, nil
}

// FinishPasskeyLogin 校验登录断言:按 credential ID 反查凭据 → 校验 user handle 与
// 身份状态(member 须 active)→ 回写签名计数。成功返回身份(由调用方建正式会话)。
func (s *Service) FinishPasskeyLogin(ctx context.Context, challengeID string, secure bool, host string, r *http.Request) (*Identity, error) {
	raw, err := s.st.popChallenge(challengeID, challengeLogin)
	if err != nil {
		return nil, err
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal(raw, &sd); err != nil {
		return nil, ErrChallengeNotFound
	}
	rp, err := s.resolveRP(ctx, secure, host)
	if err != nil {
		return nil, err
	}
	wa, err := s.webauthnFor(rp)
	if err != nil {
		return nil, err
	}

	var matched *passkeyRow
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		row, err := s.st.getPasskeyByCredentialID(rawID)
		if err != nil {
			return nil, err
		}
		// user handle 必须与凭据归属一致(防跨账户断言重放)。
		if len(userHandle) > 0 && string(userHandle) != row.OwnerKey {
			return nil, ErrPasskeyNotFound
		}
		ident, err := s.resolveOwner(row.OwnerKey)
		if err != nil {
			return nil, err
		}
		rows, err := s.st.listPasskeyRows(row.OwnerKey)
		if err != nil {
			return nil, err
		}
		creds := make([]webauthn.Credential, 0, len(rows))
		for _, pr := range rows {
			creds = append(creds, pr.toWebAuthnCredential())
		}
		matched = row
		return &webauthnUser{key: row.OwnerKey, name: ident.Username, displayName: ident.Username, credentials: creds}, nil
	}

	_, cred, err := wa.FinishPasskeyLogin(handler, sd, r)
	if err != nil {
		return nil, fmt.Errorf("mfa: finish passkey login: %w", err)
	}
	if matched == nil {
		return nil, ErrPasskeyNotFound
	}
	ident, err := s.resolveOwner(matched.OwnerKey)
	if err != nil {
		return nil, err
	}
	if err := s.st.updatePasskeyVerification(matched.ID, cred.Authenticator.SignCount, cred.Authenticator.CloneWarning); err != nil {
		return nil, err
	}
	return ident, nil
}

// resolveOwner 把 owner_key 还原为可建会话的身份;member 须 active,
// 管理员须存在。失败一律返回统一错误(不泄露具体账户状态)。
func (s *Service) resolveOwner(owner string) (*Identity, error) {
	if owner == "admin" {
		var username string
		err := s.db.QueryRow(`SELECT username FROM admin_user WHERE id = 1`).Scan(&username)
		if err != nil {
			return nil, ErrOwnerNotFound
		}
		return &Identity{Role: auth.RoleAdmin, Username: username}, nil
	}
	if !strings.HasPrefix(owner, "user:") {
		return nil, ErrOwnerNotFound
	}
	userID := strings.TrimPrefix(owner, "user:")
	var username string
	err := s.db.QueryRow(
		`SELECT username FROM users WHERE id = ? AND status = 'active'`, userID).Scan(&username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOwnerNotFound
		}
		return nil, fmt.Errorf("mfa: resolve owner: %w", err)
	}
	return &Identity{Role: auth.RoleMember, UserID: userID, Username: username}, nil
}

// ---- 管理员重置 -------------------------------------------------------------------

// AdminResetTOTP 清除成员的 TOTP 配置与备份码(成员被锁死时的恢复通道)。
func (s *Service) AdminResetTOTP(userID string) error {
	if err := s.ensureMember(userID); err != nil {
		return err
	}
	return s.st.deleteTOTP(OwnerKey(auth.RoleMember, userID))
}

// AdminResetPasskeys 清除成员的全部通行密钥,返回删除数。
func (s *Service) AdminResetPasskeys(userID string) (int64, error) {
	if err := s.ensureMember(userID); err != nil {
		return 0, err
	}
	return s.st.deletePasskeysByOwner(OwnerKey(auth.RoleMember, userID))
}

func (s *Service) ensureMember(userID string) error {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM users WHERE id = ?`, userID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrOwnerNotFound
		}
		return fmt.Errorf("mfa: check member: %w", err)
	}
	return nil
}

// PurgeExpiredChallenges 启动清理过期挑战行。
func (s *Service) PurgeExpiredChallenges() {
	_, _ = s.st.PurgeExpiredChallenges()
}

// normalizePasskeyName 设备名归一:去空白、限长。
func normalizePasskeyName(name string) string {
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 64 {
		name = string([]rune(name)[:64])
	}
	return name
}
