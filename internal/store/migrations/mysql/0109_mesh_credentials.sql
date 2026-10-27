-- 0109 mesh(MySQL):临时凭据(EasyTier Secure Mode 签发)——结构同 SQLite 版。
CREATE TABLE IF NOT EXISTS mesh_credentials (
    id            BIGINT AUTO_INCREMENT PRIMARY KEY,
    credential_id VARCHAR(128) NOT NULL UNIQUE,
    remark        VARCHAR(200) NOT NULL DEFAULT '',
    ttl_seconds   BIGINT NOT NULL,
    created_at    VARCHAR(40) NOT NULL,
    expires_at    VARCHAR(40) NOT NULL,
    revoked_at    VARCHAR(40),
    created_by    VARCHAR(128) NOT NULL DEFAULT '',
    source        VARCHAR(16) NOT NULL DEFAULT 'admin'
);

CREATE INDEX idx_mesh_credentials_created ON mesh_credentials (created_at);
