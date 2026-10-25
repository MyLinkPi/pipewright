package dbadmin

import (
	"context"
	"database/sql"
	"fmt"
)

// 本文件是实例备份恢复(internal/dbbackup)的受控出口:连接借用与密码揭示。
// 两者都只供服务端备份链路使用,密码明文绝不进响应/日志/审计。

// ConnectBackup 为实例开一个备份/回放专用连接:multiStatements 开启(整段 SQL 回放)、
// 读超时放宽到导入档。database 语义与 openEngine 一致:MySQL 空串 = 服务器级连接
// (整实例 dump 用);PG 指定目标库(空回落维护库)。调用方负责 Close。
// 备份是读操作,不检查 readonly;回放的只读纪律由调用方(dbbackup)把关。
func (s *service) ConnectBackup(ctx context.Context, id, database string) (*sql.DB, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.connect(ctx, inst, database, connOpts{multiStatements: true})
}

// RevealPassword 解密返回实例连接密码,仅供服务端备份链路构造 SSH 执行环境
// (如容器内 pg_dumpall 的 PGPASSWORD)。空密文 → 明确错误。
func (s *service) RevealPassword(ctx context.Context, id string) (string, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return "", err
	}
	sealed := s.passwordSealed(ctx, id)
	if len(sealed) == 0 {
		return "", fmt.Errorf("dbadmin: instance has no sealed password")
	}
	plain, err := s.vault.OpenSecret(sealed)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}
	return string(plain), nil
}
