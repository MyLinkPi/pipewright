package mfa

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"sync"

	"golang.org/x/crypto/nacl/secretbox"
)

// Sealer 是 TOTP 密钥的加密封装(vault.Service 实现该接口;main 装配时注入)。
type Sealer interface {
	SealSecret(plaintext []byte) ([]byte, error)
	OpenSecret(sealed []byte) ([]byte, error)
}

// 密文前缀标识加密来源:先落 local、后配 vault master key(或反之)时,
// 旧密文仍能按前缀找到正确的解封路径。
const (
	sealPrefixVault = "vault:"
	sealPrefixLocal = "local:"
)

// localKeySize 与 vault keySize 一致(32B,secretbox 密钥长度)。
const localKeySize = 32

// secretKeeper 组合两种封存路径并按前缀分发:
//   - vault sealer 非 nil → 新密文用 vault 封("vault:" 前缀);
//   - 否则用 system_config.mfa_secret_key 兜底密钥封("local:" 前缀),首用自动生成。
//     兜底密钥存于同库,防的是密文列单独泄露(备份/导出/注入)而非全库泄露;
//     配置了 master key 的部署则获得与库隔离的完整保护。
type secretKeeper struct {
	db     *sql.DB
	vault  Sealer // 可为 nil
	mu     sync.Mutex
	local  *[localKeySize]byte // 兜底密钥(懒加载缓存)
	hasKey bool
}

// seal 加密 TOTP 密钥,返回带前缀的落库串。
func (k *secretKeeper) seal(plaintext []byte) (string, error) {
	if k.vault != nil {
		sealed, err := k.vault.SealSecret(plaintext)
		if err != nil {
			return "", fmt.Errorf("mfa: seal via vault: %w", err)
		}
		return sealPrefixVault + encodeBase64Std(sealed), nil
	}
	key, err := k.loadLocalKey()
	if err != nil {
		return "", err
	}
	sealed, err := sealSecretBox(key, plaintext)
	if err != nil {
		return "", err
	}
	return sealPrefixLocal + encodeBase64Std(sealed), nil
}

// open 按前缀解密 TOTP 密钥。
func (k *secretKeeper) open(stored string) ([]byte, error) {
	switch {
	case len(stored) > len(sealPrefixVault) && stored[:len(sealPrefixVault)] == sealPrefixVault:
		if k.vault == nil {
			return nil, ErrSecretUnreadable
		}
		raw, err := decodeBase64Std(stored[len(sealPrefixVault):])
		if err != nil {
			return nil, fmt.Errorf("mfa: decode vault-sealed secret: %w", err)
		}
		return k.vault.OpenSecret(raw)
	case len(stored) > len(sealPrefixLocal) && stored[:len(sealPrefixLocal)] == sealPrefixLocal:
		key, err := k.loadLocalKey()
		if err != nil {
			return nil, err
		}
		raw, err := decodeBase64Std(stored[len(sealPrefixLocal):])
		if err != nil {
			return nil, fmt.Errorf("mfa: decode local-sealed secret: %w", err)
		}
		return openSecretBox(key, raw)
	default:
		return nil, fmt.Errorf("mfa: unknown secret seal prefix")
	}
}

// loadLocalKey 懒加载/生成兜底密钥:读 system_config.mfa_secret_key;为空则生成
// 32B 随机密钥,条件更新落库(并发首用只有一个写入生效,输者回读)。
func (k *secretKeeper) loadLocalKey() (*[localKeySize]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.hasKey {
		return k.local, nil
	}
	var stored string
	if err := k.db.QueryRow(
		`SELECT COALESCE(mfa_secret_key, '') FROM system_config WHERE id = 1`).Scan(&stored); err != nil {
		return nil, fmt.Errorf("mfa: load local secret key: %w", err)
	}
	if stored != "" {
		raw, err := base64.StdEncoding.DecodeString(stored)
		if err != nil || len(raw) != localKeySize {
			return nil, fmt.Errorf("mfa: local secret key malformed")
		}
		var key [localKeySize]byte
		copy(key[:], raw)
		k.local, k.hasKey = &key, true
		return k.local, nil
	}
	// 生成新密钥并条件落库:仅当仍为空时写入,避免并发初始化互相覆盖。
	var key [localKeySize]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, fmt.Errorf("mfa: generate local secret key: %w", err)
	}
	enc := base64.StdEncoding.EncodeToString(key[:])
	res, err := k.db.Exec(
		`UPDATE system_config SET mfa_secret_key = ? WHERE id = 1 AND COALESCE(mfa_secret_key, '') = ''`, enc)
	if err != nil {
		return nil, fmt.Errorf("mfa: persist local secret key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 并发下他人先写入:回读权威值。
		if err := k.db.QueryRow(
			`SELECT mfa_secret_key FROM system_config WHERE id = 1`).Scan(&stored); err != nil {
			return nil, fmt.Errorf("mfa: reload local secret key: %w", err)
		}
		raw, derr := base64.StdEncoding.DecodeString(stored)
		if derr != nil || len(raw) != localKeySize {
			return nil, fmt.Errorf("mfa: local secret key malformed")
		}
		copy(key[:], raw)
	}
	k.local, k.hasKey = &key, true
	return k.local, nil
}

// sealSecretBox/openSecretBox:nonce(24B)前缀 + secretbox 密文(与 vault 封装格式同构)。
func sealSecretBox(key *[localKeySize]byte, plaintext []byte) ([]byte, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("mfa: seal nonce: %w", err)
	}
	return secretbox.Seal(nonce[:], plaintext, &nonce, key), nil
}

func openSecretBox(key *[localKeySize]byte, sealed []byte) ([]byte, error) {
	if len(sealed) < 24+secretbox.Overhead {
		return nil, fmt.Errorf("mfa: sealed secret too short")
	}
	var nonce [24]byte
	copy(nonce[:], sealed[:24])
	opened, ok := secretbox.Open(nil, sealed[24:], &nonce, key)
	if !ok {
		return nil, fmt.Errorf("mfa: open sealed secret failed")
	}
	return opened, nil
}
