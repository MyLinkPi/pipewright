-- 0065 servers jumps:SSH 跳板链(多跳登录)。
-- JSON 数组,按连接顺序(第一跳最先连,目标经最后一跳转发可达):
--   [{"host":"bastion","port":22,"user":"ops","credentialId":"<uuid>"}]
-- 每跳独立引用一条 ssh_key/ssh_password 凭据(仅存 ID,绝无明文);'[]' = 直连。
-- 无外键(JSON 列,SQLite 无法 ALTER 补约束),删除凭据由 vault.Delete 显式占用检查守卫
-- (对该列做 LIKE 子串匹配;凭据 ID 为定长 UUID,无子串歧义,等价精确匹配)。
ALTER TABLE servers ADD COLUMN jumps TEXT NOT NULL DEFAULT '[]';
