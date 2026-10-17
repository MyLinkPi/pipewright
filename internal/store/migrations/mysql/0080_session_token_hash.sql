-- 0080 session_token_hash: sessions.token 由「原始会话 token」改为只存其 sha256 hex,
-- 防 DB 文件泄漏后库内值可直接当 cookie 冒充活跃会话。
--
-- 旧行存的是原始 token,与新格式不可比(哈希后无法反推),无法原地转换:
-- 一次性清空既有会话,升级后全员重新登录(安全迁移的标准做法,一次性代价)。
DELETE FROM sessions;
