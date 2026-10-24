package vault

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// 每种算法生成后:私钥必须是可被 ssh.ParsePrivateKey 解析的 OpenSSH PEM,
// 公钥必须是 authorized_keys 单行、前缀与算法一致、且与私钥配对。
func TestGenerateSSHKeyPairEachAlgorithm(t *testing.T) {
	cases := map[string]string{
		"":         "ssh-ed25519", // 空串默认 ed25519
		"ed25519":  "ssh-ed25519",
		"ED25519":  "ssh-ed25519", // 大小写不敏感
		"rsa2048":  "ssh-rsa",
		"ecdsa256": "ecdsa-sha2-nistp256",
		"ecdsa384": "ecdsa-sha2-nistp384",
		"ecdsa521": "ecdsa-sha2-nistp521",
	}
	for algo, prefix := range cases {
		privatePEM, publicAuthorized, err := GenerateSSHKeyPair(algo)
		if err != nil {
			t.Fatalf("%q: generate: %v", algo, err)
		}
		if !strings.HasPrefix(privatePEM, "-----BEGIN OPENSSH PRIVATE KEY-----") {
			t.Fatalf("%q: private key is not OpenSSH PEM: %.40s", algo, privatePEM)
		}
		signer, err := ssh.ParsePrivateKey([]byte(privatePEM))
		if err != nil {
			t.Fatalf("%q: parse private key: %v", algo, err)
		}
		if !strings.HasPrefix(publicAuthorized, prefix) {
			t.Fatalf("%q: public key prefix = %q, want %q", algo, publicAuthorized, prefix)
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicAuthorized))
		if err != nil {
			t.Fatalf("%q: parse authorized key: %v", algo, err)
		}
		if string(pub.Marshal()) != string(signer.PublicKey().Marshal()) {
			t.Fatalf("%q: public key does not match private key", algo)
		}
	}
}

func TestGenerateSSHKeyPairRejectsUnknownAlgorithm(t *testing.T) {
	for _, algo := range []string{"dsa", "rsa1024", "foo", "ed25519\nx"} {
		if _, _, err := GenerateSSHKeyPair(algo); !errors.Is(err, ErrInvalidSSHKeyAlgorithm) {
			t.Fatalf("%q: err = %v, want ErrInvalidSSHKeyAlgorithm", algo, err)
		}
	}
}
