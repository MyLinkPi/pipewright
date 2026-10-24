package vault

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ErrInvalidSSHKeyAlgorithm 表示密钥算法不在支持列表内。
var ErrInvalidSSHKeyAlgorithm = errors.New("vault: invalid ssh key algorithm")

// GenerateSSHKeyPair 按算法生成一对新的 SSH 密钥,返回 OpenSSH 格式私钥
// (-----BEGIN OPENSSH PRIVATE KEY-----)与 authorized_keys 单行公钥
// (如 "ssh-ed25519 AAAA…" 或 "rsa2048" → "ssh-rsa AAAA…")。
//
// 纯无状态加密运算:不触碰 DB 与 master key,与保险库配置状态无关。
// 私钥不加密 passphrase —— 克隆(gitauth)与部署 SSH(target)消费侧均以
// 空口令解析,带口令私钥会在使用时被拒。algorithm 空串视为默认 ed25519。
func GenerateSSHKeyPair(algorithm string) (privatePEM, publicAuthorized string, err error) {
	alg := strings.ToLower(strings.TrimSpace(algorithm))
	var priv crypto.PrivateKey
	switch alg {
	case "", "ed25519":
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return "", "", err
		}
		priv = key
	case "rsa2048", "rsa3072", "rsa4096":
		bits, err := strconv.Atoi(strings.TrimPrefix(alg, "rsa"))
		if err != nil {
			return "", "", err // 不可达:case 已限定数字后缀
		}
		key, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			return "", "", err
		}
		priv = key
	case "ecdsa256", "ecdsa384", "ecdsa521":
		var curve elliptic.Curve
		switch alg {
		case "ecdsa256":
			curve = elliptic.P256()
		case "ecdsa384":
			curve = elliptic.P384()
		default:
			curve = elliptic.P521()
		}
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			return "", "", err
		}
		priv = key
	default:
		return "", "", ErrInvalidSSHKeyAlgorithm
	}

	signer, ok := priv.(crypto.Signer)
	if !ok {
		return "", "", errors.New("vault: ssh key algorithm not a signer") // 不可达:三类密钥均实现 crypto.Signer
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", "", err
	}
	pub, err := ssh.NewPublicKey(signer.Public())
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(block)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), nil
}
