package redisadmin

import (
	"context"
	"fmt"
)

// RevealPassword 解密返回实例密码,仅供服务端备份链路构造 SSH 执行环境(容器内
// redis-cli 认证)。无密码实例(内网免认证合法)返回空串;绝无明文进响应/日志/审计。
func (s *service) RevealPassword(ctx context.Context, id string) (string, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return "", err
	}
	sealed := s.passwordSealedOf(ctx, id)
	if len(sealed) == 0 {
		return "", nil
	}
	plain, err := s.vault.OpenSecret(sealed)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}
	return string(plain), nil
}
