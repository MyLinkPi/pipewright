-- 0065 servers jumps(MySQL):SSH 跳板链(多跳登录),语义见 sqlite 同名文件。
-- MySQL TEXT 不支持字面 DEFAULT,空值以 COALESCE(jumps, '[]') 兜底(与 SELECT 一致)。
ALTER TABLE servers ADD COLUMN jumps TEXT NULL;
