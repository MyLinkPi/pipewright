-- 0058 servers sudo credential:服务器可选绑定一条 sudo_password 凭据。
-- 非 root 登录且无免密 sudo 时,平台 HTTPS 等提权场景用该密码经 `sudo -S` 从 stdin 喂入
-- (密码绝不进 argv/日志)。空 = 不使用密码 sudo(仅 root / 免密 sudo)。
-- 无外键(SQLite 无法 ALTER 补约束),删除凭据由 vault.Delete 显式占用检查守卫。
ALTER TABLE servers ADD COLUMN sudo_credential_id TEXT NOT NULL DEFAULT '';
