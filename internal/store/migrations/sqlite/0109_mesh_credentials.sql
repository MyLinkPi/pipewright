-- 0109 mesh(SQLite):临时凭据(EasyTier Secure Mode 签发)——
--   mesh_credentials 是签发登记簿:credential_id 对应 easytier-cli credential generate
--   返回的管理 ID(撤销锚点);**不存 credential_secret**(私钥只在签发响应出现一次,
--   明文落库零容忍,事实来源在对端 easytier 的 --credential-file)。
--   过期/撤销的实时状态以 easytier-cli credential list 为准,本表补备注/创建者/来源
--   (ListCredentials 按 credential_id 合并两侧)。
CREATE TABLE IF NOT EXISTS mesh_credentials (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    credential_id TEXT NOT NULL UNIQUE,
    remark        TEXT NOT NULL DEFAULT '',
    ttl_seconds   INTEGER NOT NULL,
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL,
    revoked_at    TEXT,
    created_by    TEXT NOT NULL DEFAULT '',
    source        TEXT NOT NULL DEFAULT 'admin'
);

CREATE INDEX IF NOT EXISTS idx_mesh_credentials_created ON mesh_credentials (created_at);
