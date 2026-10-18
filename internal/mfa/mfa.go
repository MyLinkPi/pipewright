// Package mfa 实现两步验证:TOTP 验证码(含一次性备份恢复码)与 Passkey(WebAuthn)。
//
// 身份统一为 owner_key:管理员 "admin"、普通用户 "user:<id>"(同时作为 WebAuthn
// user handle,长度远小于规范的 64 字节上限)。TOTP 密钥经 secretbox 加密落库:
// 优先 vault(master key 已配置),否则用 system_config.mfa_secret_key 自动生成的
// 兜底密钥;两种密文以 "vault:"/"local:" 前缀区分,后配 master key 也能解开旧密文。
//
// 登录流程与 auth 包协作:口令通过且 TOTP 已启用 → auth 建 10 分钟 pending 会话 →
// 本包 VerifyLoginCode 校验验证码 → auth.CompleteMFA 升级为 30 天正式会话。
// Passkey 登录本身即「持有 + 用户验证」双因素,直接建正式会话,不再叠加 TOTP。
package mfa

import (
	"errors"
	"fmt"
	"time"

	"github.com/huangchengsir/pipewright/internal/auth"
)

// 领域错误(httpapi 映射状态码)。
var (
	// ErrTOTPNotSetup 尚未发起 TOTP 配置(setup 先行)。
	ErrTOTPNotSetup = errors.New("mfa: totp not set up")
	// ErrTOTPAlreadyOn TOTP 已启用,不能重复开启。
	ErrTOTPAlreadyOn = errors.New("mfa: totp already enabled")
	// ErrTOTPNotEnabled TOTP 未启用(对需要已启用的操作)。
	ErrTOTPNotEnabled = errors.New("mfa: totp not enabled")
	// ErrInvalidCode 验证码/备份码错误。
	ErrInvalidCode = errors.New("mfa: invalid code")
	// ErrPasskeyNotFound 通行密钥不存在。
	ErrPasskeyNotFound = errors.New("mfa: passkey not found")
	// ErrChallengeNotFound WebAuthn 挑战不存在/已过期/已用(一次性)。
	ErrChallengeNotFound = errors.New("mfa: challenge not found or expired")
	// ErrPasskeyDisabled 管理员未启用 Passkey 登录。
	ErrPasskeyDisabled = errors.New("mfa: passkey login disabled")
	// ErrInsecureContext 非 HTTPS(且非 localhost)上下文,浏览器无法使用 WebAuthn。
	ErrInsecureContext = errors.New("mfa: webauthn requires https (or localhost)")
	// ErrSecretUnreadable 密文对应的加密密钥当前不可用(如 vault 密文但 master key 未配置)。
	ErrSecretUnreadable = errors.New("mfa: totp secret unreadable with current key")
)

// ErrLockedOut 尝试次数过多被锁定(携带重试间隔)。
type ErrLockedOut struct {
	RetryAfter time.Duration
}

func (e *ErrLockedOut) Error() string {
	mins := int(e.RetryAfter.Minutes()) + 1
	return fmt.Sprintf("mfa: locked out, retry after %d minutes", mins)
}

// OwnerKey 把 auth 会话身份(role/userID)归一为 owner_key;
// 管理员恒为 "admin"(单行 admin_user),普通用户为 "user:<id>"。
func OwnerKey(role, userID string) string {
	if role == auth.RoleAdmin {
		return "admin"
	}
	return "user:" + userID
}
