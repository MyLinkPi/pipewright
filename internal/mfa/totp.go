package mfa

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TOTP 参数:与主流验证器 App(RFC 6238 默认)一致,保证扫码即用。
const (
	totpIssuer = "Pipewright"
	// totpSkew 允许 ±1 个周期的时钟偏移(验证器与服务器时间差 ≤30s 不影响)。
	totpSkew = 1
	// backupCodeCount 备份恢复码数量;格式 XXXX-XXXX(8 位 A-Z0-9)。
	backupCodeCount = 10
	backupCodeLen   = 8
)

// GenerateTOTPSecret 为 accountName(用户名)生成 TOTP 密钥;
// 返回 base32 密钥串与 otpauth:// URL(前端渲染二维码)。
func GenerateTOTPSecret(accountName string) (secret, otpauthURL string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: accountName,
		Period:      30,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", "", fmt.Errorf("mfa: generate totp secret: %w", err)
	}
	return key.Secret(), key.URL(), nil
}

// ValidateTOTPCode 校验 6 位验证码(去空格;±1 周期容忍)。
func ValidateTOTPCode(secret, code string) bool {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return false
	}
	ok, err := totp.ValidateCustom(code, secret, time.Now().UTC(), totp.ValidateOpts{
		Period:    30,
		Skew:      totpSkew,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	return err == nil && ok
}

// backupCharset 备份码字符集(去掉易混淆的 0/O/1/I)。
const backupCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// GenerateBackupCodes 生成 backupCodeCount 个一次性恢复码(明文仅此一次返回;
// 落库的是 argon2id 哈希)。随机数用拒绝采样避免模偏差。
func GenerateBackupCodes() ([]string, error) {
	max := big.NewInt(int64(len(backupCharset)))
	codes := make([]string, 0, backupCodeCount)
	seen := make(map[string]struct{}, backupCodeCount)
	for len(codes) < backupCodeCount {
		raw := make([]byte, backupCodeLen)
		for i := range raw {
			n, err := rand.Int(rand.Reader, max)
			if err != nil {
				return nil, fmt.Errorf("mfa: generate backup code: %w", err)
			}
			raw[i] = backupCharset[n.Int64()]
		}
		code := fmt.Sprintf("%s-%s", raw[:4], raw[4:])
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}
	return codes, nil
}

// NormalizeBackupCode 归一化备份码输入(去空白/连字符、转大写);
// 返回规范形态 XXXX-XXXX 与是否合法。
func NormalizeBackupCode(code string) (string, bool) {
	c := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(code), "-", ""), " ", ""))
	if len(c) != backupCodeLen {
		return "", false
	}
	for _, r := range c {
		if !strings.ContainsRune(backupCharset, r) {
			return "", false
		}
	}
	return c[:4] + "-" + c[4:], true
}

// looksLikeBackupCode 宽松判断输入是否可能是备份码形态(登录时不泄露是哪种因素失败,
// 但内部路由校验逻辑用)。
func looksLikeBackupCode(code string) bool {
	_, ok := NormalizeBackupCode(code)
	return ok
}
