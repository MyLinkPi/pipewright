package mfa

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

// newAdminDB 打开隔离库并种入管理员行(Login/resolveOwner 依赖)。
func newAdminDB(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st := storetest.Open(t)
	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB.Exec(
		`INSERT INTO admin_user (id, username, password_hash, created_at, updated_at)
		 VALUES (1, 'admin', ?, ?, ?)`, hash, now, now); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return NewService(st.DB, nil, nil, nil), st
}

// codeNow 由密钥生成当前时刻的 6 位动态码。
func codeNow(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	return code
}

// TestTOTPFullFlow 覆盖:setup(密钥+备份码,明文一次性)→ 错码拒绝 → 正码启用 →
// 登录第二步(动态码/备份码)→ 备份码一次性 → 关闭(备份码)→ 状态清零。
func TestTOTPFullFlow(t *testing.T) {
	svc, st := newAdminDB(t)
	role, userID := auth.RoleAdmin, ""

	// 未配置时 EnableTOTP 报 ErrTOTPNotSetup。
	if err := svc.EnableTOTP(role, userID, "123456"); err != ErrTOTPNotSetup {
		t.Fatalf("EnableTOTP before setup = %v, want ErrTOTPNotSetup", err)
	}

	setup, err := svc.SetupTOTP(role, userID, "admin")
	if err != nil {
		t.Fatalf("SetupTOTP: %v", err)
	}
	if setup.Secret == "" || setup.OTPAuthURL == "" || len(setup.BackupCodes) != backupCodeCount {
		t.Fatalf("setup payload incomplete: %+v", setup)
	}
	if !strings.Contains(setup.OTPAuthURL, "otpauth://totp/") {
		t.Fatalf("otpauth url malformed: %s", setup.OTPAuthURL)
	}

	// 库内只存密文,且带 local: 前缀(vault 未注入 → 本地兜底密钥)。
	var secretEnc string
	if err := st.DB.QueryRow(
		`SELECT secret_enc FROM totp_credentials WHERE owner_key = 'admin'`).Scan(&secretEnc); err != nil {
		t.Fatalf("read secret_enc: %v", err)
	}
	if !strings.HasPrefix(secretEnc, sealPrefixLocal) || strings.Contains(secretEnc, setup.Secret) {
		t.Fatalf("secret must be sealed with %s prefix and no plaintext", sealPrefixLocal)
	}

	// 错误动态码:Enable 拒绝。
	if err := svc.EnableTOTP(role, userID, "000000"); err != ErrInvalidCode {
		t.Fatalf("EnableTOTP(bad) = %v, want ErrInvalidCode", err)
	}
	if svc.TOTPEnabled(role, userID) {
		t.Fatal("TOTPEnabled must be false before enable")
	}

	// 正确动态码启用。
	if err := svc.EnableTOTP(role, userID, codeNow(t, setup.Secret)); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}
	if !svc.TOTPEnabled(role, userID) {
		t.Fatal("TOTPEnabled must be true after enable")
	}
	if err := svc.EnableTOTP(role, userID, codeNow(t, setup.Secret)); err != ErrTOTPAlreadyOn {
		t.Fatalf("double enable = %v, want ErrTOTPAlreadyOn", err)
	}

	// 登录第二步:动态码通过。
	if err := svc.VerifyLoginCode(role, userID, codeNow(t, setup.Secret)); err != nil {
		t.Fatalf("VerifyLoginCode(totp): %v", err)
	}

	// 备份码可用且一次性;重放失败。
	backup := setup.BackupCodes[0]
	if err := svc.VerifyLoginCode(role, userID, backup); err != nil {
		t.Fatalf("VerifyLoginCode(backup): %v", err)
	}
	if err := svc.VerifyLoginCode(role, userID, backup); err != ErrInvalidCode {
		t.Fatalf("replayed backup = %v, want ErrInvalidCode", err)
	}
	if _, remaining := NormalizeBackupCode(backup); !remaining {
		t.Fatal("backup code normalize sanity")
	}

	// 状态:剩余备份码 = 总数-已用 1。
	info, err := svc.Status(role, userID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !info.TOTPEnabled || info.BackupCodesRemaining != backupCodeCount-1 {
		t.Fatalf("status = enabled=%v remaining=%d", info.TOTPEnabled, info.BackupCodesRemaining)
	}

	// 用另一枚备份码关闭二步验证。
	if err := svc.DisableTOTP(role, userID, setup.BackupCodes[1]); err != nil {
		t.Fatalf("DisableTOTP(backup): %v", err)
	}
	if svc.TOTPEnabled(role, userID) {
		t.Fatal("TOTPEnabled must be false after disable")
	}
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(1) FROM totp_credentials`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("totp rows after disable = %d err=%v", n, err)
	}
}

// TestRegenerateBackupCodes 旧备份码全量作废、新码可用。
func TestRegenerateBackupCodes(t *testing.T) {
	svc, _ := newAdminDB(t)
	role, userID := auth.RoleAdmin, ""

	setup, err := svc.SetupTOTP(role, userID, "admin")
	if err != nil {
		t.Fatalf("SetupTOTP: %v", err)
	}
	if err := svc.EnableTOTP(role, userID, codeNow(t, setup.Secret)); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}
	newCodes, err := svc.RegenerateBackupCodes(role, userID, codeNow(t, setup.Secret))
	if err != nil {
		t.Fatalf("RegenerateBackupCodes: %v", err)
	}
	if len(newCodes) != backupCodeCount {
		t.Fatalf("new codes = %d", len(newCodes))
	}
	// 旧码作废。
	if err := svc.VerifyLoginCode(role, userID, setup.BackupCodes[0]); err != ErrInvalidCode {
		t.Fatalf("old backup after regen = %v, want ErrInvalidCode", err)
	}
	// 新码可用。
	if err := svc.VerifyLoginCode(role, userID, newCodes[0]); err != nil {
		t.Fatalf("new backup: %v", err)
	}
}

// TestVerifyLoginCodeLockout 连续 5 次错码后锁定(15 分钟),正确码也不放行。
func TestVerifyLoginCodeLockout(t *testing.T) {
	svc, _ := newAdminDB(t)
	role, userID := auth.RoleAdmin, ""
	setup, err := svc.SetupTOTP(role, userID, "admin")
	if err != nil {
		t.Fatalf("SetupTOTP: %v", err)
	}
	if err := svc.EnableTOTP(role, userID, codeNow(t, setup.Secret)); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}
	for i := 0; i < auth.LockoutThreshold; i++ {
		if err := svc.VerifyLoginCode(role, userID, "000000"); err != ErrInvalidCode {
			t.Fatalf("attempt %d = %v, want ErrInvalidCode", i, err)
		}
	}
	err = svc.VerifyLoginCode(role, userID, codeNow(t, setup.Secret))
	var locked *ErrLockedOut
	if err == nil {
		t.Fatal("expected lockout after threshold failures")
	}
	if le, ok := err.(*ErrLockedOut); !ok || le.RetryAfter <= 0 {
		t.Fatalf("err = %v, want *ErrLockedOut with positive RetryAfter", err)
	}
	_ = locked
}

// TestSecretKeeperLocalKey 本地兜底密钥:自动生成、落 system_config、加解密回环。
func TestSecretKeeperLocalKey(t *testing.T) {
	st := storetest.Open(t)
	k := &secretKeeper{db: st.DB}
	sealed, err := k.seal([]byte("GEZDGNBVGY3TQOJQ"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !strings.HasPrefix(sealed, sealPrefixLocal) {
		t.Fatalf("sealed = %q, want local prefix", sealed)
	}
	opened, err := k.open(sealed)
	if err != nil || string(opened) != "GEZDGNBVGY3TQOJQ" {
		t.Fatalf("open = %q err=%v", opened, err)
	}
	// 密钥已落库且为 base64(32B)。
	var key string
	if err := st.DB.QueryRow(`SELECT mfa_secret_key FROM system_config WHERE id = 1`).Scan(&key); err != nil {
		t.Fatalf("read mfa_secret_key: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != localKeySize {
		t.Fatalf("mfa_secret_key malformed: len=%d err=%v", len(raw), err)
	}
	// 前缀识别:vault 前缀但 vault 未配置 → ErrSecretUnreadable。
	if _, err := k.open(sealPrefixVault + base64.StdEncoding.EncodeToString([]byte("x"))); err != ErrSecretUnreadable {
		t.Fatalf("vault-prefixed without vault = %v, want ErrSecretUnreadable", err)
	}
}

// TestChallengeOneTime 挑战一次性:第二次 pop 报 ErrChallengeNotFound;kind 不符同样拒绝。
func TestChallengeOneTime(t *testing.T) {
	st := storetest.Open(t)
	svc := NewService(st.DB, nil, nil, nil)
	s := svc.st
	id, err := s.saveChallenge(challengeLogin, []byte(`{}`), challengeTTL)
	if err != nil {
		t.Fatalf("saveChallenge: %v", err)
	}
	if _, err := s.popChallenge(id, challengeRegister); err != ErrChallengeNotFound {
		t.Fatalf("wrong kind pop = %v, want ErrChallengeNotFound", err)
	}
	if _, err := s.popChallenge(id, challengeLogin); err != nil {
		t.Fatalf("popChallenge: %v", err)
	}
	if _, err := s.popChallenge(id, challengeLogin); err != ErrChallengeNotFound {
		t.Fatalf("second pop = %v, want ErrChallengeNotFound", err)
	}
}

// TestAdminResetAndSummary 管理员重置成员 TOTP + 用户概要统计。
func TestAdminResetAndSummary(t *testing.T) {
	svc, st := newAdminDB(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB.Exec(
		`INSERT INTO users (id, username, password_hash, role, status, created_at, updated_at)
		 VALUES ('u1', 'alice', 'x', 'member', 'active', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	setup, err := svc.SetupTOTP(auth.RoleMember, "u1", "alice")
	if err != nil {
		t.Fatalf("SetupTOTP: %v", err)
	}
	if err := svc.EnableTOTP(auth.RoleMember, "u1", codeNow(t, setup.Secret)); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}
	if on, n := svc.UserMFASummary("u1"); !on || n != 0 {
		t.Fatalf("summary = totp=%v passkeys=%d", on, n)
	}
	if err := svc.AdminResetTOTP("u1"); err != nil {
		t.Fatalf("AdminResetTOTP: %v", err)
	}
	if on, _ := svc.UserMFASummary("u1"); on {
		t.Fatal("totp must be cleared after admin reset")
	}
	if err := svc.AdminResetTOTP("no-such-user"); err != ErrOwnerNotFound {
		t.Fatalf("reset unknown user = %v, want ErrOwnerNotFound", err)
	}
}

// TestPasskeyLoginUsable 任意注册一把凭据后即认为可用。
func TestPasskeyLoginUsable(t *testing.T) {
	svc, _ := newAdminDB(t)
	if svc.PasskeyLoginUsable(nil) {
		t.Fatal("no passkeys registered → not usable")
	}
	if _, err := svc.st.insertPasskey(&passkeyRow{
		CredentialID: "cred-1", OwnerKey: "admin", Name: "test",
		PublicKey: "pub", Transports: `["internal"]`,
	}); err != nil {
		t.Fatalf("insertPasskey: %v", err)
	}
	if !svc.PasskeyLoginUsable(nil) {
		t.Fatal("passkey registered → usable")
	}
}

// TestOwnerKey 身份键归一。
func TestOwnerKey(t *testing.T) {
	if got := OwnerKey(auth.RoleAdmin, ""); got != "admin" {
		t.Fatalf("admin owner = %q", got)
	}
	if got := OwnerKey(auth.RoleMember, "u1"); got != "user:u1" {
		t.Fatalf("member owner = %q", got)
	}
}
