-- 0110 mesh(MySQL):凭据自动申请 API 令牌 ——结构同 SQLite 版。
CREATE TABLE IF NOT EXISTS mesh_api_tokens (
    id           BIGINT AUTO_INCREMENT PRIMARY KEY,
    name         VARCHAR(64) NOT NULL UNIQUE,
    token_hash   VARCHAR(64) NOT NULL UNIQUE,
    created_at   VARCHAR(40) NOT NULL,
    last_used_at VARCHAR(40) NOT NULL DEFAULT '',
    revoked_at   VARCHAR(40)
);
