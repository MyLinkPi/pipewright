-- 0110 mesh(SQLite):凭据自动申请 API 令牌 ——
--   外部系统(CI/脚本)免会话调 POST /api/mesh/credentials 用;明文令牌 pwmc_ 前缀 +
--   32 字节随机,仅创建响应出现一次,库内只存 SHA-256 hex(token_hash)。
--   撤销 = 置 revoked_at(立即失效,last_used_at 供巡检)。
CREATE TABLE IF NOT EXISTS mesh_api_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL UNIQUE,
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   TEXT NOT NULL,
    last_used_at TEXT NOT NULL DEFAULT '',
    revoked_at   TEXT
);
